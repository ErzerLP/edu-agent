package agentloop

import (
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestStatSupersessionDistinguishesMetadataFromRawHash(t *testing.T) {
	session := newWorkspaceTestSession(t, &fakeModel{}, &fakeWorkspaceExecutor{status: workspace.Status{Available: true, Label: "project"}})
	defer session.Close()
	if _, err := session.startTurn(); err != nil {
		t.Fatal(err)
	}
	before := workspace.Result{Value: map[string]any{"path": "file", "content": "old body", "complete": true}, Reference: &workspace.Reference{Path: "file", Kind: "file", ContentHash: "sha256:" + strings.Repeat("a", 64)}}
	if err := session.appendWorkspaceToolResult(workspace.ToolRead, "read", before); err != nil {
		t.Fatal(err)
	}
	after := workspace.Result{Value: map[string]any{"path": "file", "exists": false, "complete": true}, Reference: &workspace.Reference{Path: "file", Kind: "entry_metadata", ContentHash: "sha256:" + strings.Repeat("b", 64)}}
	if err := session.appendWorkspaceToolResult(workspace.ToolStat, "stat", after); err != nil {
		t.Fatal(err)
	}
	for _, message := range session.messages {
		if message.ToolCallID != "read" {
			continue
		}
		if strings.Contains(message.Content, "current_content_hash") || strings.Contains(message.Content, "old body") || !strings.Contains(message.Content, "current_metadata_hash") {
			t.Fatalf("metadata digest advertised as raw content: %s", message.Content)
		}
	}
}
