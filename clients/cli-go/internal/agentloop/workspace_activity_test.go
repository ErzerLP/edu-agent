package agentloop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestWorkspaceSearchProgressPublishesSafeActivityOnOriginalCall(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	for index := range 40 {
		content := "ordinary content\n"
		if index%10 == 0 {
			content = "needle content\n"
		}
		if err := os.WriteFile(filepath.Join(root, "src", fmt.Sprintf("file-%02d.txt", index)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	executor, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = executor.Close() })
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("search-call", workspace.ToolSearch, `{"query":"needle","path":"src"}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "搜索完成。"}},
	}}
	session := newWorkspaceTestSession(t, model, executor)
	var activities []Activity
	ctx := WithActivityReporter(t.Context(), func(activity Activity) {
		activities = append(activities, activity)
	})
	result, err := session.Send(ctx, "搜索工作区")
	if err != nil || result.Text != "搜索完成。" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var runningCount int
	var final *Activity
	maxScanned := 0
	for index := range activities {
		activity := &activities[index]
		if activity.Kind != ActivityTool || activity.Event.ID != "search-call" {
			continue
		}
		if activity.Event.Status == EventRunning {
			runningCount++
			if activity.File != nil && activity.File.ScannedFiles > maxScanned {
				maxScanned = activity.File.ScannedFiles
			}
		} else {
			final = activity
		}
		encoded := fmt.Sprintf("%+v", activity.File)
		for _, forbidden := range []string{root, "expected_hash", `{"query"`, "provider reasoning"} {
			if strings.Contains(encoded, forbidden) {
				t.Fatalf("activity leaked %q: %s", forbidden, encoded)
			}
		}
	}
	if runningCount < 2 || maxScanned == 0 || final == nil || final.Event.Status != EventSucceeded || final.File == nil ||
		final.File.Path != "src" || !final.File.HasScanned || final.File.ScannedFiles != 40 || !final.File.HasMatches || final.File.Matches != 4 {
		t.Fatalf("running=%d maxScanned=%d final=%+v activities=%+v", runningCount, maxScanned, final, activities)
	}
}

func TestWorkspaceYOLOMutationPublishesBoundedFinalDiffActivity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.md")
	if err := os.WriteFile(path, []byte("old line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = executor.Close() })
	readValue := executor.Execute(t.Context(), workspace.ToolRead, `{"path":"notes.md"}`).Value.(map[string]any)
	arguments, err := json.Marshal(map[string]any{
		"path": "notes.md", "mode": "replace", "content": "new line\n", "expected_hash": readValue["content_hash"],
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("write-call", workspace.ToolWrite, string(arguments))},
		{Message: modelclient.Message{Role: "assistant", Content: "写入完成。"}},
	}}
	session := newWorkspaceTestSession(t, model, executor)
	if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	var activities []Activity
	ctx := WithActivityReporter(t.Context(), func(activity Activity) {
		activities = append(activities, activity)
	})
	result, err := session.Send(ctx, "更新笔记")
	if err != nil || result.Text != "写入完成。" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var final *Activity
	for index := range activities {
		activity := &activities[index]
		if activity.Kind == ActivityTool && activity.Event.ID == "write-call" && activity.Event.Status == EventSucceeded {
			final = activity
		}
	}
	if final == nil || final.File == nil || final.File.Path != "notes.md" || final.File.Operation != "write_replace" ||
		final.File.PreviewKind != "diff" || final.File.PublicationOutcome != string(workspace.PublicationCompleted) ||
		final.File.FirstChangedLine != 1 || !strings.Contains(final.File.Preview, "-old line") || !strings.Contains(final.File.Preview, "+new line") ||
		len(final.File.Preview) > maxFileActivityPreviewBytes {
		t.Fatalf("final activity=%+v", final)
	}
	encoded := fmt.Sprintf("%+v", final.File)
	for _, forbidden := range []string{root, readValue["content_hash"].(string), "expected_hash", string(arguments)} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("final activity leaked %q: %s", forbidden, encoded)
		}
	}
}
