package agentloop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestGitignoreProductionBothToolsRegistrationProjectionAndCheckpoint(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "complete"
		if partial {
			name = "partial"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{
				".gitignore":  "# private-ignore-config\nignored/\n*.secret\n",
				"visible.txt": "needle body-secret", "ignored/hidden.txt": "needle body-secret",
				"child/.gitignore": "*.txt\n!keep.txt\n", "child/keep.txt": "needle body-secret", "child/drop.txt": "needle body-secret",
			}
			if partial {
				files["broken/.gitignore"], files["broken/hidden.txt"] = "[\n", "needle body-secret"
			}
			for path, text := range files {
				full := filepath.Join(root, filepath.FromSlash(path))
				if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			executor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			message := toolMessage("ignore-find", workspace.ToolFind, `{"pattern":"*.txt","type":"file","respect_gitignore":true}`)
			message.ToolCalls = append(message.ToolCalls, toolMessage("ignore-search", workspace.ToolSearch, `{"query":"needle","output":"count","glob":"*.txt","respect_gitignore":true}`).ToolCalls...)
			model := &fakeModel{responses: []modelclient.Response{
				{Message: message}, {Message: modelclient.Message{Role: "assistant", Content: "已完成筛选"}},
			}}
			sink := &durabilitySink{}
			session := newDurableTestSession(t, model, &fakeServer{}, executor, sink)
			defer session.Close()
			result, err := session.Send(t.Context(), "按项目忽略规则查找和统计")
			if err != nil || result.PendingFileMutation != nil || len(sink.files) != 0 || len(model.requests) != 2 {
				t.Fatalf("not read-only: %+v err=%v", result, err)
			}
			registered := map[string]bool{}
			for _, tool := range model.requests[0].Tools {
				if tool.Function.Name == workspace.ToolFind || tool.Function.Name == workspace.ToolSearch {
					registered[tool.Function.Name] = strings.Contains(string(tool.Function.Parameters), `"respect_gitignore":{"type":"boolean","default":false}`)
				}
			}
			if !registered[workspace.ToolFind] || !registered[workspace.ToolSearch] {
				t.Fatalf("missing production schema %+v", registered)
			}
			seen := map[string]bool{}
			for _, msg := range model.requests[1].Messages {
				if msg.Role != "tool" {
					continue
				}
				seen[msg.ToolCallID] = true
				value := searchProjectionObject(t, msg.Content)
				if value["respect_gitignore"] != true || value["complete"] != !partial || value["ignored_entries"] != float64(2) {
					t.Fatalf("live lost filtering: %s", msg.Content)
				}
				if strings.Contains(msg.Content, root) || strings.Contains(msg.Content, "body-secret") || strings.Contains(msg.Content, "private-ignore-config") || strings.Contains(msg.Content, "hidden.txt") || strings.Contains(msg.Content, "drop.txt") {
					t.Fatalf("projection leak %s", msg.Content)
				}
				if msg.ToolCallID == "ignore-search" && (value["matched_lines"] != float64(2) || value["matched_files"] != float64(2) || value["counts_partial"] != partial) {
					t.Fatalf("dishonest count: %s", msg.Content)
				}
				if partial && value["truncation_reason"] != "gitignore_pattern_unsupported" {
					t.Fatalf("lost ignore failure %s", msg.Content)
				}
			}
			if !seen["ignore-find"] || !seen["ignore-search"] {
				t.Fatalf("missing production execution %+v", seen)
			}
			for _, call := range []string{"ignore-find", "ignore-search"} {
				if session.toolReferences[call] != nil || session.workspaceReferences[call] == nil {
					t.Fatalf("wrong authority for %s", call)
				}
				history := searchProjectionObject(t, session.toolHistory[call])
				if history["respect_gitignore"] != true || partial && history["complete"] != false {
					t.Fatalf("history lost filtering %+v", history)
				}
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
			if strings.Contains(string(encoded), "private-ignore-config") || strings.Contains(string(encoded), "body-secret") || strings.Contains(string(encoded), root) {
				t.Fatal("checkpoint leaked file body or root")
			}
			for _, source := range checkpoint.Context.Sources {
				if source.WorkspaceReference != nil && (source.WorkspaceReference.Kind == "find_result" || source.WorkspaceReference.Kind == "search_result") && source.Authority != AuthorityWorkspaceSnapshot {
					t.Fatal("ignore result promoted to server authority")
				}
			}
		})
	}
}

func TestGitignorePartialProjectionPreservesCauseAndCount(t *testing.T) {
	value := map[string]any{
		"path": ".", "output": "files", "files": []any{"a.txt", "b.txt"}, "returned": 2,
		"matched_files": 2, "counts_partial": true, "complete": false, "truncation_reason": "gitignore_unavailable",
		"respect_gitignore": true, "ignore_files": 1, "ignore_bytes": 200, "ignored_entries": 3,
	}
	for _, raw := range workspaceBudgetProjectionCandidates(workspace.ToolSearch, value) {
		got := searchProjectionObject(t, raw)
		if got["respect_gitignore"] != true || got["complete"] != false || got["counts_partial"] != true || got["matched_files"] != float64(2) {
			t.Fatalf("dishonest fallback %s", raw)
		}
	}
	got := compactSearchProjection(normalizedProjectionObject(value), 1, 20, "current_turn_budget")
	if got["source_truncation_reason"] != "gitignore_unavailable" || got["ignore_files"].(interface{ String() string }).String() != "1" {
		t.Fatalf("lost ignore cause/stats %+v", got)
	}
}
