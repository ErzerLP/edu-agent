package agentloop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestFindProductionProjectionAndCheckpoint(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "example.go"), []byte("do-not-read-this-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("find-call", workspace.ToolFind, `{"pattern":"*.go","type":"file"}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "已定位文件"}},
	}}
	sink := &durabilitySink{}
	session := newDurableTestSession(t, model, &fakeServer{}, executor, sink)
	defer session.Close()
	result, err := session.Send(t.Context(), "查找Go文件")
	if err != nil || result.PendingFileMutation != nil || len(sink.files) != 0 {
		t.Fatalf("read-only find result=%+v err=%v", result, err)
	}
	if len(model.requests) != 2 {
		t.Fatalf("requests=%d", len(model.requests))
	}
	last := model.requests[1].Messages[len(model.requests[1].Messages)-1]
	for _, required := range []string{"example.go", "entries", "visited_entries", "scanned_directories"} {
		if !strings.Contains(last.Content, required) {
			t.Fatalf("lost %s: %s", required, last.Content)
		}
	}
	if strings.Contains(last.Content, "do-not-read-this-body") || session.toolReferences["find-call"] != nil {
		t.Fatal("find body/server authority leak")
	}
	if ref := session.workspaceReferences["find-call"]; ref == nil || ref.Kind != "find_result" {
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
	for _, source := range checkpoint.Context.Sources {
		if source.WorkspaceReference != nil && source.WorkspaceReference.Kind == "find_result" && source.Authority != AuthorityWorkspaceSnapshot {
			t.Fatal("find promoted to wrong authority")
		}
	}
}
