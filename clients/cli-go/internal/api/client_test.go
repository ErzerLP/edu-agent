package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const validDeviceList = `{"devices":[{"id":"10000000-0000-4000-8000-000000000001","display_name":"Laptop","scopes":["devices:read"],"created_at":"2026-08-24T00:00:00Z"}]}`
const validRevision = `{"revision_id":"20000000-0000-4000-8000-000000000001","revision_no":1,"parent_revision_id":null,"manifest_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source":"go-cli-m1","created_by_device_id":"10000000-0000-4000-8000-000000000001","created_at":"2026-08-24T00:00:00Z","canonicalizer_version":"edu-markdown-v1","parser_version":"goldmark-v1.8.5-commonmark-0.31.2-gfm","indexer_version":"knowledge-indexer-v1","identity_policy_version":"identity-policy-v1"}`

func TestClientRefusesRedirectWithoutForwardingAuthorization(t *testing.T) {
	t.Parallel()
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("redirect received Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, validDeviceList)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer source.Close()
	client := NewClient(source.URL, "token-secret", time.Second, nil)
	_, err := client.Devices(t.Context())
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Category != "redirect_refused" {
		t.Fatalf("Devices error = %v", err)
	}
	if redirected.Load() != 0 {
		t.Fatalf("redirect target was called %d times", redirected.Load())
	}
}

func TestClientClosedRetryMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		firstCode int
		firstBody string
		header    string
		wantCalls int32
	}{
		{name: "502", firstCode: http.StatusBadGateway, firstBody: "proxy", wantCalls: 2},
		{name: "transient 503", firstCode: http.StatusServiceUnavailable, firstBody: errorJSON("unavailable", "request-1"), wantCalls: 2},
		{name: "redacted 503", firstCode: http.StatusServiceUnavailable, firstBody: errorJSON("content_redacted", "request-2"), wantCalls: 1},
		{name: "projection 503", firstCode: http.StatusServiceUnavailable, firstBody: errorJSON("projection_unavailable", "request-3"), wantCalls: 1},
		{name: "429 with Retry-After", firstCode: http.StatusTooManyRequests, firstBody: errorJSON("rate_limited", "request-4"), header: "0", wantCalls: 2},
		{name: "429 without Retry-After", firstCode: http.StatusTooManyRequests, firstBody: errorJSON("rate_limited", "request-5"), wantCalls: 1},
		{name: "429 overflowing Retry-After", firstCode: http.StatusTooManyRequests, firstBody: errorJSON("rate_limited", "request-6"), header: "9223372036854775807", wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				call := calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if call == 1 {
					if test.header != "" {
						w.Header().Set("Retry-After", test.header)
					}
					w.WriteHeader(test.firstCode)
					_, _ = io.WriteString(w, test.firstBody)
					return
				}
				_, _ = io.WriteString(w, validDeviceList)
			}))
			defer server.Close()
			client := NewClient(server.URL, "token", time.Second, nil)
			client.sleep = func(context.Context, time.Duration) error { return nil }
			_, _ = client.Devices(t.Context())
			if calls.Load() != test.wantCalls {
				t.Fatalf("calls = %d, want %d", calls.Load(), test.wantCalls)
			}
		})
	}
}

func TestClientRetriesConnectionEOFAtMostTwice(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, io.EOF
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(validDeviceList)),
		}, nil
	})
	client := NewClient("http://127.0.0.1:8080", "token", time.Second, &http.Client{Transport: transport})
	if _, err := client.Devices(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestPairAndLogoutNeverRetry(t *testing.T) {
	t.Parallel()
	var pairCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/pairings/exchange":
			pairCalls.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "proxy")
		case "/v1/devices/device":
			pairCalls.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "proxy")
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, "token", time.Second, nil)
	_, _ = client.Pair(t.Context(), "pair-code", "Laptop")
	if pairCalls.Load() != 1 {
		t.Fatalf("pair calls = %d", pairCalls.Load())
	}
	_ = client.RevokeDevice(t.Context(), "device")
	if pairCalls.Load() != 2 {
		t.Fatalf("total calls = %d", pairCalls.Load())
	}
}

func TestImportRetryReusesIdenticalBody(t *testing.T) {
	t.Parallel()
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, append([]byte(nil), data...))
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = io.WriteString(w, "proxy")
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"revision":`+validRevision+`,"unchanged":false}`)
	}))
	defer server.Close()
	client := NewClient(server.URL, "token", time.Second, nil)
	request := ImportRequest{OperationID: "30000000-0000-4000-8000-000000000001", Source: "go-cli-m1", Documents: []ImportDocument{{Path: "note.md", Markdown: "# Note"}}}
	if _, err := client.ImportKnowledge(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || string(bodies[0]) != string(bodies[1]) {
		t.Fatalf("retry bodies differ: %q %q", bodies[0], bodies[1])
	}
}

func TestClientRejectsMalformedBoundariesAndRedactsServerBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		contentType string
		body        string
		maxBody     int64
		want        string
	}{
		{name: "content type", contentType: "text/plain", body: validDeviceList, want: "unexpected_content_type"},
		{name: "unknown field", contentType: "application/json", body: `{"devices":[],"unknown":true}`, want: "malformed_success_response"},
		{name: "required array is null", contentType: "application/json", body: `{"devices":null}`, want: "malformed_success_response"},
		{name: "multiple values", contentType: "application/json", body: validDeviceList + `{}`, want: "malformed_success_response"},
		{name: "body limit", contentType: "application/json", body: validDeviceList, maxBody: 10, want: "response_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := NewClient(server.URL, "token", time.Second, nil)
			if test.maxBody != 0 {
				client.maxBody = test.maxBody
			}
			_, err := client.Devices(t.Context())
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) || protocolErr.Category != test.want {
				t.Fatalf("error = %v, want %s", err, test.want)
			}
		})
	}

	secret := "server-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"authentication_failed","message":"`+secret+`","request_id":"request-secret"}}`)
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "client-secret-token", time.Second, nil).Devices(t.Context())
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "client-secret-token") {
		t.Fatalf("unsafe error = %v", err)
	}
}

func TestClientRejectsOmittedRequiredZeroValueFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		call func(*Client) error
	}{
		{
			name: "boolean compatible",
			body: `{"profile":"openai-chat-completions-v1","context_window":0,"minimum_context_window":0,"system_user_assistant_messages":false,"non_streaming":false,"structured_json":false,"native_json_schema":false,"streaming":false,"tool_calls":false,"incompatibility_reasons":[]}`,
			call: func(client *Client) error { _, err := client.ModelCapabilities(t.Context()); return err },
		},
		{
			name: "boolean unchanged",
			body: `{"revision":` + validRevision + `}`,
			call: func(client *Client) error {
				_, err := client.ImportKnowledge(t.Context(), ImportRequest{OperationID: "30000000-0000-4000-8000-000000000001", Source: "go-cli-m1", Documents: []ImportDocument{{Path: "note.md", Markdown: "# Note"}}})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			err := test.call(NewClient(server.URL, "token", time.Second, nil))
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) || protocolErr.Category != "malformed_success_response" {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRedactRemovesAllProvidedSecrets(t *testing.T) {
	t.Parallel()
	got := Redact("code=alpha token=beta markdown=gamma", "alpha", "beta", "gamma")
	for _, secret := range []string{"alpha", "beta", "gamma"} {
		if strings.Contains(got, secret) {
			t.Fatalf("Redact retained %q in %q", secret, got)
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func errorJSON(code, requestID string) string {
	return `{"error":{"code":"` + code + `","message":"stable","request_id":"` + requestID + `"}}`
}
