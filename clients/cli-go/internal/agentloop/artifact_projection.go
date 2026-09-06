package agentloop

import (
	"encoding/json"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
)

func artifactInfoValue(info localartifact.Info, minimal bool) map[string]any {
	value := map[string]any{"id": info.ID, "saved": info.Saved}
	if !minimal {
		value["kind"], value["bytes"], value["content_hash"] = info.Kind, info.Bytes, info.Hash
	}
	return value
}

func (r artifactToolResult) value(bodyLimit, itemLimit int, minimal, history bool) map[string]any {
	if r.Code != "" {
		return map[string]any{"error": r.Code, "availability": "unavailable"}
	}
	if p := r.Page; p != nil {
		v := artifactInfoValue(p.Info, minimal)
		n := min(bodyLimit, len(p.Data))
		if history {
			n = 0
		}
		v["offset"], v["next_offset"] = p.Offset, p.Offset+int64(n)
		v["more"] = p.Offset+int64(n) < p.Info.Bytes
		if n < len(p.Data) {
			v["projection_omitted"] = true
		}
		if n > 0 {
			v["data"], v["encoding"] = localOutputEncoding(p.Data[:n])
		}
		return v
	}
	if p := r.Search; p != nil {
		v := artifactInfoValue(p.Info, minimal)
		n := min(itemLimit, len(p.Offsets))
		next := p.NextOffset
		if history {
			n = 0
		}
		if n < len(p.Offsets) {
			next = p.Offset
			if n > 0 {
				next = p.Offsets[n-1] + 1
			}
			v["projection_omitted"] = true
		}
		v["matches"] = append([]int64{}, p.Offsets[:n]...)
		v["offset"], v["next_offset"], v["more"] = p.Offset, next, p.More || n < len(p.Offsets)
		if !minimal {
			v["scanned"] = p.Scanned
		}
		return v
	}
	n := min(itemLimit, len(r.Items))
	items := make([]any, 0, n)
	for _, item := range r.Items[:n] {
		items = append(items, artifactInfoValue(item, minimal))
	}
	v := map[string]any{"items": items, "next_offset": r.Offset + int64(n), "more": r.More || n < len(r.Items)}
	if n < len(r.Items) {
		v["projection_omitted"] = true
	}
	return v
}

func (r artifactToolResult) project(maxBytes int, accept func(string) bool, history bool) string {
	fits := func(value map[string]any) (string, bool) {
		data, err := json.Marshal(value)
		text := string(data)
		return text, err == nil && len(data) <= maxBytes && (accept == nil || accept(text))
	}
	for _, minimal := range []bool{false, true} {
		body, items := 65536, 100
		for {
			if text, ok := fits(r.value(body, items, minimal, history)); ok {
				return text
			}
			if body == 0 && items == 0 {
				break
			}
			body /= 2
			items /= 2
		}
	}
	// No payload is acknowledged when even its location cannot fit normally.
	v := map[string]any{"error": "artifact_projection", "projection_omitted": true}
	if r.Page != nil {
		v["id"], v["next_offset"] = r.Page.Info.ID, r.Page.Offset
	}
	if r.Search != nil {
		v["id"], v["next_offset"] = r.Search.Info.ID, r.Search.Offset
	}
	data, _ := json.Marshal(v)
	return string(data)
}

func (s *Session) appendArtifactToolResult(callID string, r artifactToolResult) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return ErrSessionClosed
	}
	allowed := min(max(32, s.currentToolResultBudget/max(1, s.currentToolResultShares)-30), max(32, s.currentToolResultBudget-s.currentToolResultTokens-6))
	live := r.project(maxToolOutputBytes, func(text string) bool { return s.estimator.EstimateText(text) <= allowed }, false)
	history := r.project(maxHistoryToolOutputBytes, nil, true)
	return s.appendLocalDataProjectionLocked(callID, live, history)
}
