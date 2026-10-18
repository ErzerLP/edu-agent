package modelusage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

func ParseChatUsage(data []byte) Usage {
	var envelope struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return Usage{}
	}
	return parseUsage(envelope.Usage)
}
func parseUsage(data []byte) Usage {
	return parseProtocolUsage(data, false)
}
func parseProtocolUsage(data []byte, responses bool) Usage {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return Usage{}
	}
	u := Usage{}
	read := func(raw json.RawMessage) *int64 {
		if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil
		}
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 0 {
			u.Inconsistent = true
			return nil
		}
		return &n
	}
	u.Input = read(raw["prompt_tokens"])
	u.Output = read(raw["completion_tokens"])
	u.ProviderTotal = read(raw["total_tokens"])
	if responses {
		u.Input = read(raw["input_tokens"])
		u.Output = read(raw["output_tokens"])
	}
	var details map[string]json.RawMessage
	detailKey := "completion_tokens_details"
	if responses {
		detailKey = "output_tokens_details"
	}
	if json.Unmarshal(raw[detailKey], &details) == nil {
		u.Reasoning = read(details["reasoning_tokens"])
	}
	u.normalize()
	return u
}

// usageJSON retains only a top-level usage object (up to 16 KiB). All other
// JSON strings, including arbitrarily long answers/reasoning, are skipped.
type usageJSON struct {
	responses                                            bool
	depth                                                int
	inString, escaped, key, wantUsage, capture, overflow bool
	name                                                 []byte
	raw                                                  []byte
	usage                                                Usage
	complete                                             bool
}

func (p *usageJSON) feed(data []byte) {
	for _, c := range data {
		if p.capture {
			if len(p.raw) < 16<<10 {
				p.raw = append(p.raw, c)
			} else {
				p.overflow = true
			}
		}
		if p.inString {
			if p.escaped {
				p.escaped = false
				if p.key && len(p.name) < 32 {
					p.name = append(p.name, c)
				}
				continue
			}
			if c == '\\' {
				p.escaped = true
				continue
			}
			if c == '"' {
				p.inString = false
				if p.key {
					p.wantUsage = string(p.name) == "usage"
					p.key = false
				}
				continue
			}
			if p.key && len(p.name) < 32 {
				p.name = append(p.name, c)
			}
			continue
		}
		switch c {
		case '"':
			p.inString = true
			if p.depth == 1 && !p.wantUsage {
				p.key = true
				p.name = p.name[:0]
			}
		case '{', '[':
			if p.depth == 1 && p.wantUsage && c == '{' {
				p.capture = true
				p.overflow = false
				p.raw = append(p.raw[:0], c)
			}
			p.depth++
		case '}', ']':
			p.depth--
			if p.depth == 0 {
				p.complete = true
			}
			if p.capture && p.depth == 1 {
				if !p.overflow {
					p.usage = parseProtocolUsage(p.raw, p.responses)
				}
				clear(p.raw)
				p.raw = p.raw[:0]
				p.capture = false
				p.wantUsage = false
			}
		case ',':
			if p.depth == 1 {
				p.wantUsage = false
			}
		}
	}
}

// Observe wraps the response body without pre-reading or changing read sizes.
// Finish uses a bounded detached context so cancellation cannot erase a call.
func (s *Store) Observe(ctx context.Context, ref CallRef, next http.RoundTripper, request *http.Request) (*http.Response, error) {
	response, err := next.RoundTrip(request)
	if err != nil {
		state := "outcome_unknown"
		if errors.Is(err, context.Canceled) && !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			state = "cancelled"
		}
		s.settle(ctx, ref, state, Usage{})
		return response, err
	}
	response.Body = &observedBody{ReadCloser: response.Body, store: s, ctx: ctx, ref: ref, sse: strings.Contains(response.Header.Get("Content-Type"), "text/event-stream"), status: response.StatusCode, parser: usageJSON{responses: ref.APIStyle == "responses"}}
	return response, nil
}
func (s *Store) settle(ctx context.Context, ref CallRef, state string, usage Usage) {
	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// Failure leaves dispatching for lease recovery. No response content is logged.
	if err := s.Finish(settle, ref, state, usage); err != nil && !errors.Is(err, ErrInactive) {
		slog.WarnContext(settle, "model_usage_settlement_unavailable", "call_id", ref.ID)
	}
}

type observedBody struct {
	io.ReadCloser
	store        *Store
	ctx          context.Context
	ref          CallRef
	sse          bool
	status       int
	parser       usageJSON
	line         []byte
	frame        []byte
	overflow     bool
	done, failed bool
	once         sync.Once
}

func (b *observedBody) event() {
	data := bytes.TrimSpace(b.frame)
	if bytes.Equal(data, []byte("[DONE]")) {
		b.done = true
	} else if len(data) > 0 {
		var u Usage
		if b.ref.APIStyle == "responses" {
			var event struct {
				Type     string `json:"type"`
				Response struct {
					Usage json.RawMessage `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal(data, &event) == nil {
				switch event.Type {
				case "response.completed", "response.incomplete", "response.failed":
					b.done = true
					b.failed = event.Type == "response.failed"
				}
				u = parseProtocolUsage(event.Response.Usage, true)
			}
		} else {
			u = ParseChatUsage(data)
		}
		if u.Input != nil || u.Output != nil || u.Reasoning != nil || u.ProviderTotal != nil || u.Inconsistent {
			b.parser.usage = u
		}
	}
	clear(b.frame)
	b.frame = b.frame[:0]
}
func (b *observedBody) feed(data []byte) {
	if !b.sse {
		b.parser.feed(data)
		return
	}
	for _, c := range data {
		if c == '\n' {
			line := bytes.TrimSuffix(b.line, []byte{'\r'})
			if len(line) == 0 {
				if !b.overflow {
					b.event()
				}
				b.overflow = false
				b.frame = b.frame[:0]
			} else if bytes.HasPrefix(line, []byte("data:")) && !b.overflow {
				part := bytes.TrimPrefix(line[5:], []byte{' '})
				if len(b.frame)+len(part)+1 > 64<<10 {
					b.overflow = true
				} else {
					b.frame = append(b.frame, part...)
					b.frame = append(b.frame, '\n')
				}
			}
			clear(b.line)
			b.line = b.line[:0]
		} else if len(b.line) < 64<<10 {
			b.line = append(b.line, c)
		} else {
			b.overflow = true
		}
	}
}
func (b *observedBody) finish(err error) {
	b.once.Do(func() {
		state := "outcome_unknown"
		if b.done || !b.sse && b.parser.complete {
			state = "received"
			if b.status >= 400 || b.failed {
				state = "failed"
			}
		} else if (errors.Is(err, context.Canceled) || errors.Is(context.Cause(b.ctx), context.Canceled)) && !errors.Is(context.Cause(b.ctx), context.DeadlineExceeded) {
			state = "cancelled"
		}
		b.store.settle(b.ctx, b.ref, state, b.parser.usage)
		clear(b.line)
		clear(b.frame)
		clear(b.parser.raw)
	})
}
func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.feed(p[:n])
	}
	// A decoder/stream consumer can stop at protocol completion without EOF.
	// Persist the receipt before it can finish the run and release its lease.
	if b.done || !b.sse && b.parser.complete || err != nil {
		b.finish(err)
	}
	return n, err
}
func (b *observedBody) Close() error { err := b.ReadCloser.Close(); b.finish(err); return err }
