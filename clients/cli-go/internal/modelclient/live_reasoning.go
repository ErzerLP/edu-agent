package modelclient

import (
	"encoding/json"
	"io"
	"time"
)

// readableReasoning deliberately does not flatten arbitrary JSON: opaque data,
// encrypted reasoning, signatures and unknown objects must never reach the UI.
// Providers can send aliases together; select one representation per frame.
func readableReasoning(delta streamDelta) string {
	for _, raw := range []json.RawMessage{delta.ReasoningContent, delta.Reasoning, delta.Thinking} {
		var text string
		if json.Unmarshal(raw, &text) == nil && text != "" {
			return text
		}
	}
	var details []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Summary string `json:"summary"`
	}
	if json.Unmarshal(delta.ReasoningDetails, &details) != nil {
		return ""
	}
	var text string
	for _, detail := range details {
		switch detail.Type {
		case "reasoning.text":
			text += detail.Text
		case "reasoning.summary":
			text += detail.Summary
		}
	}
	return text
}

// streamProgressReader reports transport activity, not validated model content.
// It shares the synchronous read path and does not create a goroutine or timer.
// Throttling bounds UI traffic, while the inner activityReader touches the
// inactivity deadline on every read, including heartbeats and partial frames.
type streamProgressReader struct {
	reader  io.Reader
	observe func(StreamEvent) error
	last    time.Time
}

func (r *streamProgressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		now := time.Now()
		if r.last.IsZero() || now.Sub(r.last) >= 250*time.Millisecond {
			r.last = now
			if reportErr := r.observe(StreamEvent{Kind: StreamEventResponseActivity, ReceivedAt: now}); reportErr != nil {
				return 0, reportErr
			}
		}
	}
	return n, err
}
