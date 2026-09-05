package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestLocalExecutionIntentIsAppendOnlyAndSharesCallNamespace(t *testing.T) {
	s, h, record, marker := journalStore(t)
	marker.LocalEffects = []LocalEffectIntent{{ToolCallID: "shell-call", Operation: "shell"}}
	updated, err := h.UpdateDirty(t.Context(), marker)
	if err != nil {
		t.Fatal(err)
	}
	before := readSessionArtifactForTest(t, s, dirtyName(record.StorageID))
	for _, test := range []struct {
		name    string
		intents []LocalEffectIntent
	}{
		{"drop", nil},
		{"replace", []LocalEffectIntent{{ToolCallID: "another-call", Operation: "shell"}}},
		{"duplicate", []LocalEffectIntent{{ToolCallID: "shell-call", Operation: "shell"}, {ToolCallID: "shell-call", Operation: "shell"}}},
		{"file-collision", []LocalEffectIntent{{ToolCallID: "shell-call", Operation: "shell"}, {ToolCallID: "alpha", Operation: "shell"}}},
		{"bad-operation", []LocalEffectIntent{{ToolCallID: "shell-call", Operation: "shell"}, {ToolCallID: "input", Operation: "exec_raw"}}},
		{"missing-task", []LocalEffectIntent{{ToolCallID: "shell-call", Operation: "shell"}, {ToolCallID: "input", Operation: "task_input"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := updated
			candidate.LocalEffects = test.intents
			if _, err := h.UpdateDirty(t.Context(), candidate); err == nil {
				t.Fatal("invalid local effect transition accepted")
			}
			if !bytes.Equal(before, readSessionArtifactForTest(t, s, dirtyName(record.StorageID))) {
				t.Fatal("rejected transition changed durable evidence")
			}
		})
	}
	candidate := updated
	candidate.LocalEffects = append(append([]LocalEffectIntent(nil), updated.LocalEffects...), LocalEffectIntent{
		ToolCallID: "input-call", Operation: "task_input", TaskID: "task_abc123",
	})
	got, err := h.UpdateDirty(t.Context(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := h.Load()
	if err != nil || loaded.Interrupted == nil || !reflect.DeepEqual(got, *loaded.Interrupted) {
		t.Fatalf("load=%+v err=%v", loaded, err)
	}
}

func TestLocalExecutionIntentQuotaPreservesEvidence(t *testing.T) {
	s, h, record, marker := journalStore(t)
	s.limits.ReceiptCount = 1
	marker.LocalEffects = []LocalEffectIntent{{ToolCallID: "shell-call", Operation: "shell"}}
	if _, err := h.UpdateDirty(t.Context(), marker); err != nil {
		t.Fatal(err)
	}
	before := readSessionArtifactForTest(t, s, dirtyName(record.StorageID))
	marker.LocalEffects = append(marker.LocalEffects, LocalEffectIntent{ToolCallID: "second", Operation: "shell"})
	if _, err := h.UpdateDirty(t.Context(), marker); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("quota error=%v", err)
	}
	if !bytes.Equal(before, readSessionArtifactForTest(t, s, dirtyName(record.StorageID))) {
		t.Fatal("quota rejection changed evidence")
	}
}

func TestLocalExecutionDirtyV6MigrationKeepsJournalAndRejectsNewFields(t *testing.T) {
	_, _, _, marker := journalStore(t)
	marker = journalSettled(marker)
	marker.SchemaVersion = 6
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeDirtyPayload(data, DefaultLimits().DirtyMarkerBytes)
	if err != nil {
		t.Fatal(err)
	}
	marker.SchemaVersion = dirtySchemaVersion
	if !reflect.DeepEqual(got, marker) {
		t.Fatalf("migration changed journal: got=%+v want=%+v", got, marker)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	object["local_effects"] = []any{map[string]any{"tool_call_id": "injected", "operation": "shell"}}
	smuggled, _ := json.Marshal(object)
	if _, err := decodeDirtyPayload(smuggled, DefaultLimits().DirtyMarkerBytes); err == nil {
		t.Fatal("v6 accepted a v7 execution intent")
	}
}

func TestLocalExecutionIntentRejectsExecutablePayload(t *testing.T) {
	_, _, _, marker := journalStore(t)
	marker.LocalEffects = []LocalEffectIntent{{ToolCallID: "shell-call", Operation: "shell"}}
	data, _ := json.Marshal(marker)
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	object["local_effects"].([]any)[0].(map[string]any)["command"] = "should never persist"
	data, _ = json.Marshal(object)
	if _, err := decodeDirtyPayload(data, DefaultLimits().DirtyMarkerBytes); err == nil {
		t.Fatal("execution intent accepted command text")
	}
}
