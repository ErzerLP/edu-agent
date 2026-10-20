package modelgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

type callTransport struct {
	partialUsage     bool
	model            *Model
	requestID, stage string
	sent             bool
	err              error
}

func (t *callTransport) RoundTrip(original *http.Request) (*http.Response, error) {
	// A logical request gets exactly one HTTP attempt. Protocol fallback belongs
	// to the server, and must never cause a second CLI POST.
	if t.sent {
		t.err = ErrorFor("request_already_accepted")
		return nil, t.err
	}
	request := original.Clone(original.Context())
	t.model.headers(request.Header, t.requestID, t.stage)
	t.sent = true
	response, err := t.model.client.http.Transport.RoundTrip(request)
	if err != nil {
		t.err = ErrorFor("gateway_unavailable")
		if original.Context().Err() != nil {
			t.err = original.Context().Err()
		}
		return nil, t.err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		if readErr != nil {
			t.err = ErrorFor("gateway_unavailable")
		} else {
			t.err = responseError(response.StatusCode, response.Header, data)
		}
		response.StatusCode = 502
		response.Header.Set("Content-Type", "application/json")
		response.Body = io.NopCloser(strings.NewReader(`{"error":{"code":"gateway_error"}}`))
		return response, nil
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/html") {
		t.err = ErrorFor("gateway_connection_invalid")
		response.Body.Close()
		return nil, t.err
	}
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		response.Body = &inspectedStream{ReadCloser: response.Body, call: t}
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		response.Body = &inspectedJSON{ReadCloser: response.Body, call: t}
	}
	return response, nil
}

// Inspect each bounded SSE line while leaving successful Chat frames unchanged.
// The shared decoder still owns assembly, tool safety and completion validation.
type inspectedStream struct {
	io.ReadCloser
	line []byte
	call *callTransport
}

func (s *inspectedStream) inspectLine() {
	if !bytes.HasPrefix(s.line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(s.line, []byte("data:")))
	s.call.inspectUsage(data)
	var envelope struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Error != nil {
		s.call.err = ErrorFor(envelope.Error.Code)
	}
}
func (s *inspectedStream) Read(p []byte) (int, error) {
	// Forward each network fragment immediately so the shared client's idle
	// timer observes progress even before a long SSE line is complete.
	n, err := s.ReadCloser.Read(p)
	fragment := p[:n]
	for len(fragment) > 0 {
		end := bytes.IndexByte(fragment, '\n')
		count := len(fragment)
		if end >= 0 {
			count = end + 1
		}
		if len(s.line)+count > 512<<10 {
			s.call.err = ErrorFor("gateway_connection_invalid")
			return 0, s.call.err
		}
		s.line = append(s.line, fragment[:count]...)
		fragment = fragment[count:]
		if end >= 0 {
			s.inspectLine()
			s.line = s.line[:0]
		}
	}
	if err == io.EOF && len(s.line) > 0 {
		s.inspectLine()
		s.line = nil
	}
	return n, err
}

// A partial gateway usage must not become a zero-valued "complete" measurement
// in the shared client's integer-only usage type.
func (t *callTransport) inspectUsage(data []byte) {
	var envelope struct {
		Usage *struct {
			Prompt     *int64 `json:"prompt_tokens"`
			Completion *int64 `json:"completion_tokens"`
			Total      *int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Usage != nil {
		u := envelope.Usage
		t.partialUsage = u.Prompt == nil || u.Completion == nil || u.Total == nil
	}
}

type inspectedJSON struct {
	io.ReadCloser
	data []byte
	call *callTransport
}

func (b *inspectedJSON) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if len(b.data)+n <= 7<<20 {
		b.data = append(b.data, p[:n]...)
	}
	return n, err
}
func (b *inspectedJSON) Close() error {
	b.call.inspectUsage(b.data)
	b.data = nil
	return b.ReadCloser.Close()
}
