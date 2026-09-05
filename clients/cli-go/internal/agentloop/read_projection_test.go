package agentloop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestLargeFileReadProjectionKeepsExactPrefixAndCursor(t *testing.T) {
	content := strings.Repeat("界\t<&>\nnext\r\n尾巴", 1000)
	hash := "sha256:" + strings.Repeat("a", 64)
	value := map[string]any{"path": strings.Repeat("dir/", 800) + "file", "content": content, "content_hash": hash, "offset": 23, "byte_offset": 7, "start_line": 23, "complete": true, "read_byte_limit": workspace.DefaultReadFileBytes}
	projection := projectWorkspaceToolResult(workspace.ToolRead, workspace.Result{Value: value, Reference: &workspace.Reference{Path: "file", ContentHash: hash, Kind: "file"}})
	if projection.ServerReference != nil || projection.WorkspaceReference.ContentHash != hash {
		t.Fatal("read authority changed")
	}
	pages := []string{projection.Live, projection.History, projection.Recall, boundedProjectionJSON(workspace.ToolRead, value, 700, "test_budget")}
	pages = append(pages, workspaceBudgetProjectionCandidates(workspace.ToolRead, value)...)
	for _, raw := range pages {
		var page map[string]any
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		got, _ := page["content"].(string)
		if !utf8.ValidString(got) || !strings.HasPrefix(content, got) || page["content_hash"] != hash {
			t.Fatalf("not a precise prefix: %+v", page)
		}
		if path, exists := page["path"]; exists && path != value["path"] {
			t.Fatal("invented shortened path")
		}
		if len(got) < len(content) {
			line, column := int64(23), int64(7)
			for _, b := range []byte(got) {
				if b == '\n' {
					line++
					column = 0
				} else {
					column++
				}
			}
			if projectionInt64(page["next_offset"]) != line || projectionInt64(page["next_byte_offset"]) != column || page["complete"] != false {
				t.Fatalf("skipped data: %+v", page)
			}
		}
	}
	if len(projection.History) > maxHistoryToolOutputBytes || len(projection.Live) > maxToolOutputBytes || len(projection.Recall) > maxRecallToolOutputBytes || len(pages[3]) > 700 {
		t.Fatal("projection exceeded JSON budget")
	}
	empty := compactReadProjection(map[string]any{"content": "", "complete": true, "offset": 1, "byte_offset": 0}, 0, "test")
	if empty["complete"] != true {
		t.Fatal("empty EOF became incomplete")
	}
}

func TestLargeFileReadLineEndOriginSurvivesProjection(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("a\n"+strings.Repeat("b", 100)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	result := w.Execute(t.Context(), workspace.ToolRead, `{"path":"file","offset":1,"byte_offset":2,"limit":2}`)
	page := compactReadProjection(normalizedProjectionObject(result.Value), 7, "test")
	if projectionInt64(page["next_offset"]) != 2 || projectionInt64(page["next_byte_offset"]) != 7 {
		t.Fatalf("line-end origin lost: %+v", page)
	}
}

type largeFileReadModel struct {
	t          *testing.T
	pages      []string
	tail, hash string
}

func (m *largeFileReadModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	last := request.Messages[len(request.Messages)-1]
	args := map[string]any{"path": "large.txt", "offset": 2, "limit": 1}
	if last.Role == "tool" {
		var page map[string]any
		if err := json.Unmarshal([]byte(last.Content), &page); err != nil {
			m.t.Fatal(err)
		}
		if page["content_hash"] != m.hash {
			m.t.Fatal("model did not receive full-file hash")
		}
		content, _ := page["content"].(string)
		if content == "" {
			m.t.Fatal("empty production read page")
		}
		m.pages = append(m.pages, content)
		if !strings.HasPrefix(m.tail, strings.Join(m.pages, "")) {
			m.t.Fatal("production pagination skipped/duplicated bytes")
		}
		if len(m.pages) == 2 {
			return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "范围读取完成"}}, nil
		}
		args["offset"], args["byte_offset"], args["expected_hash"] = page["next_offset"], page["next_byte_offset"], m.hash
	}
	raw, _ := json.Marshal(args)
	id := "first-read"
	if len(m.pages) > 0 {
		id = "next-read"
	}
	return modelclient.Response{Message: toolMessage(id, workspace.ToolRead, string(raw))}, nil
}

func TestLargeFileReadModelPaginationAndVisibleActivity(t *testing.T) {
	root := t.TempDir()
	tail := strings.Repeat("界\t<&>", 1800) + "tail\n"
	body := strings.Repeat("unreturned-head", 100000) + "\n" + tail
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	digest := sha256.Sum256([]byte(body))
	model := &largeFileReadModel{t: t, tail: tail, hash: "sha256:" + hex.EncodeToString(digest[:])}
	session := newWorkspaceTestSession(t, model, w)
	var activities []Activity
	result, err := session.Send(WithActivityReporter(t.Context(), func(a Activity) { activities = append(activities, a) }), "读取大文件后续范围")
	if err != nil || result.Text != "范围读取完成" || len(model.pages) != 2 {
		t.Fatalf("result=%+v err=%v pages=%d", result, err, len(model.pages))
	}
	visible := false
	for _, a := range activities {
		if a.Kind == ActivityTool && a.File != nil && a.File.Path == "large.txt" && a.Event.Status == EventSucceeded {
			visible = true
		}
	}
	if !visible {
		t.Fatal("no client-visible successful file activity")
	}
	checkpoint, err := session.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), strings.Repeat("unreturned-head", 100)) {
		t.Fatal("full source file entered checkpoint")
	}
}
