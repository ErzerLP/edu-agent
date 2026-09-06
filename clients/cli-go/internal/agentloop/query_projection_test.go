package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestQueryPaginationProjectionNeverSkipsOrShortensPaths(t *testing.T) {
	cursor := "q1_ABCDEFGHIJKLMNOPQRSTUVWXYZ.7"
	for _, tool := range []string{workspace.ToolList, workspace.ToolFind, workspace.ToolSearch} {
		value := map[string]any{"path": strings.Repeat("folder/", 300), "cursor": cursor, "offset": 7, "returned": 40, "more": false, "complete": true, "scan_complete": true, "scan_finished": true}
		key := "entries"
		if tool == workspace.ToolSearch {
			key = "matches"
			value["output"] = "content"
		}
		items := make([]map[string]any, 40)
		for i := range items {
			items[i] = map[string]any{"path": fmt.Sprintf("long-file-%02d", i), "type": "file", "line": i + 1, "column": 1, "preview": strings.Repeat("界\t<&>", 1000)}
		}
		value[key] = items
		projection := projectWorkspaceToolResult(tool, workspace.Result{Value: value, Reference: &workspace.Reference{Path: ".", Kind: "find_result", ContentHash: "query-hash"}})
		if projection.ServerReference != nil {
			t.Fatal("query gained server authority")
		}
		variants := []string{projection.Live, projection.History, projection.Recall, boundedProjectionJSON(tool, value, 700, "test")}
		variants = append(variants, workspaceBudgetProjectionCandidates(tool, value)...)
		for _, raw := range variants {
			var page map[string]any
			if err := json.Unmarshal([]byte(raw), &page); err != nil {
				t.Fatal(err)
			}
			if page["error"] != nil {
				t.Fatalf("lost query continuation: %s", raw)
			}
			rows, _ := page[key].([]any)
			for i, row := range rows {
				if row.(map[string]any)["path"] != items[i]["path"] {
					t.Fatal("invented abbreviated path")
				}
			}
			if path, ok := page["path"]; ok && path != value["path"] {
				t.Fatal("invented abbreviated scope")
			}
			if len(rows) < len(items) {
				want, _ := workspace.QueryCursorAt(cursor, 7+len(rows))
				if page["next_cursor"] != want || page["more"] != true || page["complete"] != false || page["scan_complete"] != true {
					t.Fatalf("cursor crossed an omitted result: %s", raw)
				}
			}
		}
	}
}

type queryPaginationModel struct {
	t     *testing.T
	tool  string
	args  map[string]any
	seen  []string
	pages int
}

func (m *queryPaginationModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	last := request.Messages[len(request.Messages)-1]
	if last.Role == "tool" {
		m.pages++
		if m.pages > 7 {
			m.t.Fatal("model query did not make bounded progress")
		}
		var page map[string]any
		if err := json.Unmarshal([]byte(last.Content), &page); err != nil {
			m.t.Fatal(err)
		}
		if page["error"] != nil {
			m.t.Fatalf("query failed: %s", last.Content)
		}
		key := "entries"
		if m.tool == workspace.ToolSearch {
			key = "matches"
		}
		if rows, ok := page[key].([]any); ok {
			for _, row := range rows {
				m.seen = append(m.seen, row.(map[string]any)["path"].(string))
			}
		}
		if page["more"] == false {
			if page["scan_complete"] != true {
				m.t.Fatal("model mistook an incomplete scope for completion")
			}
			return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "查询完整完成"}}, nil
		}
		if page["next_cursor"] == nil {
			m.t.Fatal("model lost continuation")
		}
		m.args["cursor"] = page["next_cursor"]
	}
	raw, _ := json.Marshal(m.args)
	return modelclient.Response{Message: toolMessage(fmt.Sprintf("query-%d", m.pages), m.tool, string(raw))}, nil
}

func TestQueryPaginationModelAndClientActivity(t *testing.T) {
	for _, tool := range []string{workspace.ToolList, workspace.ToolFind, workspace.ToolSearch} {
		t.Run(tool, func(t *testing.T) {
			root := t.TempDir()
			want := make([]string, 7)
			for i := range want {
				want[i] = fmt.Sprintf("f%d.txt", i)
				if err := os.WriteFile(filepath.Join(root, want[i]), []byte("needle\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			limits := workspace.DefaultLimits()
			limits.ListEntries, limits.DirectoryScanEntries, limits.SearchMatches = 3, 3, 3
			w, err := workspace.OpenWithLimits(root, limits)
			if err != nil {
				t.Fatal(err)
			}
			model := &queryPaginationModel{t: t, tool: tool, args: map[string]any{}}
			if tool == workspace.ToolFind {
				model.args["pattern"] = "*.txt"
			}
			if tool == workspace.ToolSearch {
				model.args["query"] = "needle"
			}
			session := newWorkspaceTestSession(t, model, w)
			defer session.Close()
			var activities []Activity
			result, err := session.Send(WithActivityReporter(t.Context(), func(a Activity) { activities = append(activities, a) }), "完整查询所有文件")
			if err != nil || result.Text != "查询完整完成" || !slices.Equal(model.seen, want) {
				t.Fatalf("result=%+v err=%v seen=%v", result, err, model.seen)
			}
			visible := false
			for _, a := range activities {
				if a.Event.Tool == tool && a.Event.Status == EventSucceeded {
					visible = true
				}
			}
			if !visible {
				t.Fatal("no client-visible completed query activity")
			}
			checkpoint, err := session.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(checkpoint)
			if strings.Contains(string(encoded), "inotify") || strings.Contains(string(encoded), "querySearchFile") {
				t.Fatal("live query implementation entered checkpoint")
			}
		})
	}
}
