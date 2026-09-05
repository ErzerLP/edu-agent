package agentloop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestSearchOutputProductionRegistrationProjectionAndNoMutation(t *testing.T) {
	for _, output := range []string{"content", "files", "count"} {
		t.Run(output, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("before body-secret\nneedle needle body-secret\nafter body-secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			executor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			contextLines := 0
			if output == "content" {
				contextLines = 1
			}
			model := &fakeModel{responses: []modelclient.Response{
				{Message: toolMessage("search-call", workspace.ToolSearch, fmt.Sprintf(`{"query":"needle","output":%q,"context":%d}`, output, contextLines))},
				{Message: modelclient.Message{Role: "assistant", Content: "搜索完成"}},
			}}
			sink := &durabilitySink{}
			session := newDurableTestSession(t, model, &fakeServer{}, executor, sink)
			defer session.Close()
			result, err := session.Send(t.Context(), "搜索文件")
			if err != nil || result.PendingFileMutation != nil || len(sink.files) != 0 || len(model.requests) != 2 {
				t.Fatalf("search required write authorization or failed: result=%+v err=%v requests=%d", result, err, len(model.requests))
			}
			registered := false
			for _, tool := range model.requests[0].Tools {
				if tool.Function.Name == workspace.ToolSearch {
					registered = strings.Contains(string(tool.Function.Parameters), `"output"`) && strings.Contains(string(tool.Function.Parameters), `"context"`)
				}
			}
			if !registered || workspace.IsMutationTool(workspace.ToolSearch) || !workspace.IsReadTool(workspace.ToolSearch) {
				t.Fatal("production search schema or read-only classification missing")
			}
			last := model.requests[1].Messages[len(model.requests[1].Messages)-1]
			if last.Role != "tool" {
				t.Fatalf("last=%+v", last)
			}
			for label, raw := range map[string]string{"live": last.Content, "history": session.toolHistory["search-call"]} {
				value := searchProjectionObject(t, raw)
				if value["output"] != output || value["complete"] != true {
					t.Fatalf("%s lost mode/completeness: %s", label, raw)
				}
				switch output {
				case "content":
					if len(value["matches"].([]any)) != 2 || len(value["context_lines"].([]any)) != 3 {
						t.Fatalf("%s lost content/context: %s", label, raw)
					}
				case "files":
					if len(value["files"].([]any)) != 1 || value["files"].([]any)[0] != "notes.txt" || value["matched_files"] != float64(1) || value["counts_partial"] != false {
						t.Fatalf("%s lost files: %s", label, raw)
					}
				case "count":
					if value["matched_lines"] != float64(1) || value["matched_files"] != float64(1) || value["counts_partial"] != false {
						t.Fatalf("%s lost counts: %s", label, raw)
					}
				}
				if output != "content" && (strings.Contains(raw, "body-secret") || value["matches"] != nil || value["context_lines"] != nil) {
					t.Fatalf("%s leaked file body: %s", label, raw)
				}
			}
			if session.toolReferences["search-call"] != nil {
				t.Fatal("search promoted to server authority")
			}
			if ref := session.workspaceReferences["search-call"]; ref == nil || ref.Kind != "search_result" {
				t.Fatalf("reference=%+v", ref)
			}
			checkpoint, err := session.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := EncodeSessionCheckpoint(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeSessionCheckpoint(encoded); err != nil {
				t.Fatal(err)
			}
			if output != "content" && strings.Contains(string(encoded), "body-secret") {
				t.Fatal("checkpoint/recall leaked file body")
			}
		})
	}
}

func TestSearchOutputProjectionClippingIsHonest(t *testing.T) {
	matches, contextLines, files := []any{}, []any{}, []any{}
	for i := range 12 {
		path := fmt.Sprintf("file-%02d.txt", i)
		matches = append(matches, map[string]any{"path": path, "line": 2, "column": 1, "preview": strings.Repeat("界", 100)})
		contextLines = append(contextLines, map[string]any{"path": path, "line": 1, "content": "before"})
		files = append(files, path)
	}
	for _, output := range []string{"content", "files", "count"} {
		t.Run(output, func(t *testing.T) {
			value := map[string]any{"path": ".", "output": output, "returned": 12, "complete": true, "scanned_files": 12, "scanned_bytes": 3600}
			switch output {
			case "content":
				value["matches"], value["context"], value["context_lines"] = matches, 1, contextLines
			case "files":
				value["files"], value["matched_files"], value["counts_partial"] = files, 12, false
			case "count":
				value["matched_lines"], value["matched_files"], value["counts_partial"] = 18, 12, false
				value["returned"] = 18
			}
			projection := projectWorkspaceToolResult(workspace.ToolSearch, workspace.Result{Value: value})
			live := searchProjectionObject(t, projection.Live)
			if live["complete"] != true || live["output"] != output {
				t.Fatalf("small live projection should be intact: %s", projection.Live)
			}
			history := searchProjectionObject(t, projection.History)
			if output != "count" && (history["complete"] != false || history["truncated"] != true) {
				t.Fatalf("history claimed complete after dropping payload: %s", projection.History)
			}
			candidates := workspaceBudgetProjectionCandidates(workspace.ToolSearch, value)
			for _, raw := range append(candidates, projection.History, projection.Recall) {
				got := searchProjectionObject(t, raw)
				if got["output"] != output || !utf8.ValidString(raw) {
					t.Fatalf("lost mode/UTF8: %s", raw)
				}
				if output == "content" {
					if got["returned"] != float64(len(got["matches"].([]any))) {
						t.Fatalf("returned falsely describes omitted matches: %s", raw)
					}
					if got["complete"] != true && got["truncation_reason"] == nil {
						t.Fatalf("missing clipping reason: %s", raw)
					}
				} else {
					if got["matched_files"] != float64(12) || (got["complete"] == false && got["counts_partial"] != true) {
						t.Fatalf("lost counts/partial flag: %s", raw)
					}
					if output == "count" && got["matched_lines"] != float64(18) {
						t.Fatalf("lost matching lines: %s", raw)
					}
					if output == "files" {
						for _, file := range got["files"].([]any) {
							if !strings.HasSuffix(file.(string), ".txt") {
								t.Fatalf("fabricated shortened file path: %s", raw)
							}
						}
					}
					if got["matches"] != nil || got["context_lines"] != nil || strings.Contains(raw, "before") || strings.Contains(raw, "界") {
						t.Fatalf("non-content mode leaked body: %s", raw)
					}
				}
			}
		})
	}
}

func TestSearchCountProjectionPreservesPartialCountsAndMinimumFallback(t *testing.T) {
	value := map[string]any{
		"output": "count", "path": strings.Repeat("long/", 900), "matched_lines": 23, "matched_files": 7,
		"returned": 23, "counts_partial": true, "complete": false, "truncation_reason": "match_limit",
	}
	projection := projectWorkspaceToolResult(workspace.ToolSearch, workspace.Result{Value: value})
	candidates := append(workspaceBudgetProjectionCandidates(workspace.ToolSearch, value), projection.Live, projection.History, projection.Recall,
		boundedProjectionJSON(workspace.ToolSearch, value, 256, "test_budget"))
	for _, raw := range candidates {
		got := searchProjectionObject(t, raw)
		if got["output"] != "count" || got["matched_lines"] != float64(23) || got["matched_files"] != float64(7) || got["counts_partial"] != true || got["complete"] != false {
			t.Fatalf("partial count promoted/dropped: %s", raw)
		}
	}
	if len(candidates[len(candidates)-1]) > 256 {
		t.Fatal("minimal fallback exceeds byte budget")
	}
}

func TestSearchOutputProductionMinimumContextWindow(t *testing.T) {
	for _, output := range []string{"files", "count"} {
		t.Run(output, func(t *testing.T) {
			root := t.TempDir()
			for i := range 20 {
				if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%02d.txt", i)), []byte("needle secret-body needle\nneedle\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			executor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			model := &fakeModel{responses: []modelclient.Response{
				{Message: toolMessage("search", workspace.ToolSearch, fmt.Sprintf(`{"query":"needle","output":%q}`, output))},
				{Message: modelclient.Message{Role: "assistant", Content: "完成"}},
			}}
			uuidCalls := 0
			session, err := New(model, &fakeServer{}, Options{
				ContextWindow: 4096, MaxToolRounds: 8, Workspace: executor,
				NewUUID: func() (string, error) {
					uuidCalls++
					return fmt.Sprintf("65000000-0000-4000-8000-%012d", uuidCalls), nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if _, err := session.Send(t.Context(), "搜索文件"); err != nil {
				t.Fatal(err)
			}
			if len(model.requests) != 2 || model.requests[1].MaxTokens != 512 {
				t.Fatalf("minimum-window requests=%d", len(model.requests))
			}
			last := model.requests[1].Messages[len(model.requests[1].Messages)-1]
			value := searchProjectionObject(t, last.Content)
			if value["output"] != output || value["matched_files"] != float64(20) || strings.Contains(last.Content, "secret-body") {
				t.Fatalf("minimum projection lost outcome/leaked body: %s", last.Content)
			}
			if output == "count" && value["matched_lines"] != float64(40) {
				t.Fatalf("minimum projection lost line count: %s", last.Content)
			}
			if value["complete"] == false && value["counts_partial"] != true {
				t.Fatalf("minimum projection claims all counts: %s", last.Content)
			}
		})
	}
}

func TestSearchOutputActivityUsesMatchingLineCount(t *testing.T) {
	for _, test := range []struct {
		output string
		value  map[string]any
		want   int
	}{
		{"count", map[string]any{"path": ".", "matched_lines": 3, "matched_files": 2, "returned": 3, "scanned_files": 10}, 3},
		{"files", map[string]any{"path": ".", "matched_files": 2, "returned": 2, "scanned_files": 10}, 2},
		{"content", map[string]any{"path": ".", "returned": 4, "scanned_files": 10}, 4},
	} {
		test.value["output"] = test.output
		detail := fileActivityDetailFromResult(workspace.ToolSearch, workspace.Result{Value: test.value})
		if detail == nil || !detail.HasMatches || detail.Matches != test.want || detail.ScannedFiles != 10 || detail.Preview != "" {
			t.Fatalf("%s detail=%+v", test.output, detail)
		}
	}
}

func searchProjectionObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("projection is not JSON: %s: %v", raw, err)
	}
	return value
}
