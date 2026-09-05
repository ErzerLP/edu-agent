package agentcontroller

import (
	"bytes"
	"errors"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
)

func TestLocalRecoveryMarkerFailureKeepsWAL(t *testing.T) {
	f := outputFixture(t, false)
	c := f.controller
	marker, err := c.handle.MarkDirty(t.Context(), c.record.RecordRevision, 1, "agent", false)
	if err != nil {
		t.Fatal(err)
	}
	c.dirty = &marker
	for _, id := range []string{"marker-first", "marker-second"} {
		if err := c.BeforeLocalExecution(t.Context(), agentloop.LocalExecutionIntent{ToolCallID: id, Operation: "shell"}); err != nil {
			t.Fatal(err)
		}
	}
	second, expected := localCallMarker("marker-second")
	if err := c.handle.WriteArtifact(t.Context(), second, []byte("invalid-identity")); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	err = c.saveCheckpointLocked(t.Context(), true)
	c.mu.Unlock()
	if !errors.Is(err, agentsession.ErrCorrupt) || c.dirty == nil {
		t.Fatalf("failed identity publication consumed WAL: %v", err)
	}
	loaded, err := c.handle.Load()
	if err != nil || loaded.Interrupted == nil || len(loaded.Interrupted.LocalEffects) != 2 {
		t.Fatalf("disk WAL lost: %+v err=%v", loaded.Interrupted, err)
	}
	first, firstExpected := localCallMarker("marker-first")
	data, err := c.handle.ReadArtifact(t.Context(), first)
	if err != nil || !bytes.Equal(data, firstExpected) {
		t.Fatal("confirmed identity prefix lost")
	}
	// Explicitly repair only the fixture's corrupted marker. Production refuses
	// to overwrite it; retrying the checkpoint never retries an OS operation.
	if err := c.handle.WriteArtifact(t.Context(), second, expected); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	err = c.saveCheckpointLocked(t.Context(), true)
	c.mu.Unlock()
	if err != nil || c.dirty != nil {
		t.Fatalf("repaired identity settlement failed: %v", err)
	}
	if err := c.BeforeLocalExecution(t.Context(), agentloop.LocalExecutionIntent{ToolCallID: "marker-first", Operation: "shell"}); !errors.Is(err, agentloop.ErrLocalCallRecorded) {
		t.Fatalf("identity reusable: %v", err)
	}
}

func TestLocalRecoveryMarkerContainsOnlyVersionAndDigest(t *testing.T) {
	name, expected := localCallMarker("private-provider-call-id")
	if bytes.Contains(expected, []byte("private-provider-call-id")) || len(name) != len(localCallPrefix)+64 {
		t.Fatal("identity marker stored raw identifier")
	}
	if err := validateLocalCallMarker(expected, expected); err != nil {
		t.Fatal(err)
	}
	if err := validateLocalCallMarker([]byte("local-call-v2\nfuture"), expected); !errors.Is(err, agentsession.ErrVersionUnsupported) {
		t.Fatalf("future marker=%v", err)
	}
	if err := validateLocalCallMarker([]byte("local-call-v1\nbad"), expected); !errors.Is(err, agentsession.ErrCorrupt) {
		t.Fatalf("corrupt marker=%v", err)
	}
}
