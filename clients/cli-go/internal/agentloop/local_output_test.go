package agentloop

import (
	"bytes"
	"context"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func TestLocalOutputSearchSchemaRejectsInvalidActions(t *testing.T) {
	for _, raw := range []string{
		`{"action":"search","task_id":"task_a","stream":"stdout"}`,
		`{"action":"search","task_id":"task_a","stream":"stdout","needle":""}`,
		`{"action":"search","task_id":"task_a","stream":"stdout","needle":"x","limit":101}`,
		`{"action":"search","task_id":"task_a","stream":"stdout","needle":"x","wait_ms":1}`,
		`{"action":"status","task_id":"task_a","needle":"x"}`,
		`{"action":"search","task_id":"task_a","stream":"stdout","needle":"x","needle":"y"}`,
	} {
		if _, err := decodeLocalTask(raw); err == nil {
			t.Errorf("accepted invalid search: %s", raw)
		}
	}
	args, err := decodeLocalTask(`{"action":"search","task_id":"task_a","stream":"stdout","needle":"a","limit":2}`)
	if err != nil || args.Limit != 2 || args.Needle != "a" {
		t.Fatalf("args=%+v err=%v", args, err)
	}
}

func TestLocalOutputSearchProjectionPreservesCursor(t *testing.T) {
	result := localToolResult{Action: "search", TaskID: "task_a", Search: &localexec.SearchPage{Offsets: []int64{4, 19, 30}, Offset: 1, NextOffset: 100, Scanned: 99, Received: 100, Retained: 100}}
	for _, itemLimit := range []int{0, 1, 2, 3} {
		value := result.value(0, itemLimit, false, false)["search"].(map[string]any)
		want := int64(100)
		if itemLimit < 3 {
			want = 1
			if itemLimit > 0 {
				want = result.Search.Offsets[itemLimit-1] + 1
			}
		}
		if value["next_offset"] != want || len(value["matches"].([]int64)) != itemLimit {
			t.Fatalf("limit=%d value=%+v", itemLimit, value)
		}
		if itemLimit < 3 && (value["more"] != true || value["projection_omitted"] != true) {
			t.Fatalf("omission hidden: %+v", value)
		}
	}
	page := localPageValue(localexec.OutputPage{Data: []byte("raw body"), Offset: 30, NextOffset: 38, Received: 40, Retained: 40, Saved: 40, Availability: "saved", Historical: true}, 2, true)
	if page["next_offset"] != int64(32) || page["saved"] != int64(40) || page["historical"] != true {
		t.Fatalf("saved locator omitted: %+v", page)
	}
}

func TestLocalOutputModelSearchReadKeepsRawDataOutOfHistory(t *testing.T) {
	step := 0
	taskID := ""
	s := newLocalExecutionSession(t, localExecutionModel(func(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
		step++
		switch step {
		case 1:
			return localResponse(localCall("search-shell", "shell", map[string]any{"command": "printf 'aaa-raw-marker-aaa'", "wait_ms": 1000})), nil
		case 2:
			taskID, _ = localLastResult(t, request)["task_id"].(string)
			return localResponse(localCall("search-call", "task", map[string]any{"action": "search", "task_id": taskID, "stream": "stdout", "needle": "aaa", "limit": 1})), nil
		case 3:
			result := localLastResult(t, request)
			search, ok := result["search"].(map[string]any)
			if !ok || search["next_offset"] != float64(1) {
				t.Fatalf("search result=%+v", result)
			}
			return localResponse(localCall("search-read", "task", map[string]any{"action": "read", "task_id": taskID, "stream": "stdout", "offset": 0, "limit": 3})), nil
		case 4:
			result := localLastResult(t, request)
			if page, ok := result["stdout"].(map[string]any); !ok || page["data"] != "aaa" {
				t.Fatalf("read result=%+v", result)
			}
		}
		return localFinal(), nil
	}), nil)
	result, err := s.Send(t.Context(), "find the text")
	if err != nil || result.Text != "done" || step != 4 {
		t.Fatalf("result=%+v step=%d err=%v", result, step, err)
	}
	encoded := localCheckpoint(t, s)
	for _, forbidden := range []string{"aaa-raw-marker-aaa", `\"needle\"`, `\"data\":\"aaa\"`} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("raw body/arguments persisted: %s", forbidden)
		}
	}
}
