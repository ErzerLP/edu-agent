package agentcontroller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

// A process can stop after the next WAL replaces the previous operation's
// marker, before any stable turn checkpoint. Earlier effects must remain known
// as possible effects, even when their completion was not yet checkpointed.
func TestControllerConsecutiveFileWALRetainsEveryPossibleEffectOnCrash(t *testing.T) {
	storeRoot, root := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	executor, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	c, err := Start(t.Context(), controllerDependencies(controllerStore(t, storeRoot, secrets), &controllerModel{}, server, root, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.abort()
	if err = c.BeginTurn(t.Context(), agentloop.DirtyIntent{TurnSequence: 1, OperationClass: "agent-turn"}); err != nil {
		t.Fatal(err)
	}
	for index, path := range []string{"alpha", "beta"} {
		prepared, result := executor.PrepareMutation(t.Context(), "mkdir", `{"path":"`+path+`"}`)
		if prepared == nil {
			t.Fatalf("prepare %s: %+v", path, result)
		}
		if err = c.BeforeFilePublication(t.Context(), agentloop.FileWriteAhead{ToolCallID: path + "-call", Effect: prepared.FileEffect()}); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			result = executor.CommitMutation(t.Context(), prepared)
			if result.Publication != workspace.PublicationCompleted {
				t.Fatalf("first effect: %+v", result)
			}
		}
	}
	id := c.SessionID()
	c.abort() // Crash-equivalent: no stable turn save, and no beta commit.
	resumed, err := Resume(t.Context(), controllerDependencies(controllerStore(t, storeRoot, secrets), &controllerModel{}, server, root, provider), ResumeOptions{SessionID: id, CurrentWorkspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	unknown := resumed.UnknownOutcomes()
	for _, path := range []string{"alpha", "beta"} {
		found := false
		for _, item := range unknown {
			found = found || strings.Contains(item.Label, path)
		}
		if !found {
			t.Errorf("lost %s effect after next WAL: %+v", path, unknown)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "alpha")); err != nil {
		t.Fatal("completed directory was removed", err)
	}
	if _, err := os.Stat(filepath.Join(root, "beta")); !os.IsNotExist(err) {
		t.Fatal("resume replayed uncommitted operation", err)
	}
}
