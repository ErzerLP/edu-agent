package agentloop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestStatLoopProductionAndCheckpoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("private body not requested"), 0600); err != nil {
		t.Fatal(err)
	}
	executor, err := workspace.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var calls []modelclient.ToolCall
	for _, path := range []string{".", "data", "folder", "missing"} {
		raw, _ := json.Marshal(map[string]any{"path": path})
		calls = append(calls, modelclient.ToolCall{ID: "stat-" + path, Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolStat, Arguments: string(raw)}})
	}
	model := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", ToolCalls: calls}}, {Message: modelclient.Message{Role: "assistant", Content: "已检查入口"}}}}
	sink := &durabilitySink{}
	session := newDurableTestSession(t, model, &fakeServer{}, executor, sink)
	defer session.Close()
	var activities []Activity
	result, err := session.Send(WithActivityReporter(t.Context(), func(a Activity) { activities = append(activities, a) }), "只检查元数据")
	if err != nil || result.PendingFileMutation != nil || result.Text != "已检查入口" || len(sink.files) != 0 || len(model.requests) != 2 {
		t.Fatalf("stat required mutation authorization or failed: %+v %v %+v", result, err, sink)
	}
	registered := false
	for _, tool := range model.requests[0].Tools {
		if tool.Function.Name == workspace.ToolStat {
			registered = true
		}
	}
	if !registered {
		t.Fatal("production tool missing")
	}
	results := map[string]map[string]any{}
	for _, message := range model.requests[1].Messages {
		if message.Role != "tool" {
			continue
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(message.Content), &value); err != nil {
			t.Fatal(err)
		}
		results[message.ToolCallID] = value
		if strings.Contains(message.Content, "private body") || strings.Contains(message.Content, dir) || value["identity"] != nil {
			t.Fatalf("unsafe projection: %s", message.Content)
		}
	}
	for _, path := range []string{".", "data", "folder"} {
		value := results["stat-"+path]
		if value["exists"] != true || value["mtime"] == nil || value["entry_type"] == nil || value["entry_version"] == nil {
			t.Fatalf("metadata lost: %+v", value)
		}
	}
	if results["stat-missing"]["exists"] != false {
		t.Fatalf("absence lost: %+v", results)
	}
	foundActivity := false
	for _, activity := range activities {
		if activity.Event.Tool == workspace.ToolStat && activity.Event.Status == EventSucceeded && activity.File != nil && activity.File.Path == "data" && activity.File.EntryKind == "file" {
			foundActivity = true
		}
	}
	if !foundActivity {
		t.Fatalf("stat UI detail not integrated: %+v", activities)
	}
	if _, err := os.Stat(filepath.Join(dir, workspace.ArchiveDirectory)); !os.IsNotExist(err) {
		t.Fatalf("stat created archive: %v", err)
	}
	checkpoint, err := session.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	foundSource := false
	for _, source := range checkpoint.Context.Sources {
		if source.ModelMessage.ToolCallID != "stat-data" {
			continue
		}
		foundSource = true
		if source.Authority != AuthorityWorkspaceSnapshot || source.Freshness != FreshnessWorkspaceObserved || source.ServerReference != nil || source.WorkspaceReference == nil || source.WorkspaceReference.Kind != "entry_metadata" {
			t.Fatalf("metadata promoted or lost: %+v", source)
		}
	}
	if !foundSource {
		t.Fatal("metadata source absent")
	}
	encoded, err := EncodeSessionCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSessionCheckpoint(encoded)
	if err != nil {
		t.Fatal(err)
	}
	restoredExecutor, err := workspace.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	restored := newDurableTestSession(t, &fakeModel{}, &fakeServer{}, restoredExecutor, &durabilitySink{})
	defer restored.Close()
	if err := restored.RestoreCheckpoint(decoded); err != nil {
		t.Fatal(err)
	}
	roundTrip, err := restored.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(checkpoint, roundTrip) {
		t.Fatal("stat checkpoint roundtrip changed")
	}
	if err := restored.InvalidateWorkspaceEvidence(WorkspaceReference{Path: "data", Kind: "file", InvalidateObserved: true}); err != nil {
		t.Fatal(err)
	}
	expired, err := restored.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range expired.Context.Sources {
		if source.ModelMessage.ToolCallID == "stat-data" && (source.Freshness != FreshnessWorkspaceSuperseded || source.SourceAvailable || strings.Contains(source.ModelMessage.Content, "entry_version")) {
			t.Fatalf("unknown publication retained metadata: %+v", source)
		}
	}
}

func TestStatProjectionPreservesMetadataAndRawHash(t *testing.T) {
	value := map[string]any{"path": "data", "exists": true, "entry_type": "file", "size": int64(42), "mtime": "2026-10-01T00:00:00.123456789Z", "entry_version": "entry-v1:" + strings.Repeat("a", 64), "content_hash": "sha256:" + strings.Repeat("b", 64), "complete": true}
	result := workspace.Result{Value: value, Reference: &workspace.Reference{Path: "data", Kind: "entry_metadata", ContentHash: "sha256:" + strings.Repeat("c", 64)}}
	projection := projectWorkspaceToolResult(workspace.ToolStat, result)
	candidates := append(workspaceBudgetProjectionCandidates(workspace.ToolStat, value), projection.Live, projection.History, projection.Recall)
	for _, encoded := range candidates {
		var actual map[string]any
		if err := json.Unmarshal([]byte(encoded), &actual); err != nil {
			t.Fatal(err)
		}
		for key, want := range value {
			if key == "size" {
				if actual[key] != float64(42) {
					t.Fatal(actual)
				}
				continue
			}
			if actual[key] != want {
				t.Fatalf("projection lost %s: %s", key, encoded)
			}
		}
		if actual["payload_summary"] != nil || actual["preview_truncated"] != nil || actual["truncation_reason"] != nil {
			t.Fatalf("metadata treated as missing payload: %s", encoded)
		}
	}
	if projection.ServerReference != nil || projection.WorkspaceReference.Kind != "entry_metadata" {
		t.Fatalf("authority changed: %+v", projection)
	}
}
