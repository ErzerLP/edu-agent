package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type restoreDurabilitySink struct {
	durabilitySink
	settlements []workspace.Result
	settleErr   error
}

func (s *restoreDurabilitySink) AfterFilePublication(_ context.Context, _ string, result workspace.Result) error {
	s.settlements = append(s.settlements, result)
	return s.settleErr
}

func TestArchiveRestoreModelAuthorizationSettlementAndHistory(t *testing.T) {
	for _, mode := range []string{"approve", "yolo", "decline", "cancel_pending", "wal_failure", "recorded_identity", "conflict", "changed", "late_cancel", "unknown", "settlement_failure", "model_failure"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			source := ".edu-agent-archive/legacy-container/nested/item"
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, source)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, source), []byte("private-restore-bytes\x00\xff"), 0600); err != nil {
				t.Fatal(err)
			}
			executor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			stat := executor.Execute(t.Context(), workspace.ToolStat, `{"path":"`+source+`"}`)
			version := stat.Value.(map[string]any)["entry_version"].(string)
			raw, _ := json.Marshal(map[string]any{"source": source, "destination": "restored", "expected_version": version})
			model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("observed", "stat", `{"path":"`+source+`"}`)}, {Message: toolMessage("restore-call", "restore_archive", string(raw))}, {Message: modelclient.Message{Role: "assistant", Content: "结束"}}}}
			if mode == "model_failure" {
				model.responses = model.responses[:2]
			}
			sink := &restoreDurabilitySink{}
			if mode == "wal_failure" {
				sink.fileErr = errors.New("store failed")
			}
			if mode == "recorded_identity" {
				sink.fileErr = ErrLocalCallRecorded
			}
			if mode == "settlement_failure" {
				sink.settleErr = errors.New("settlement failed")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wrapped := &copyOutcomeExecutor{Executor: executor, unknown: mode == "unknown"}
			if mode == "late_cancel" {
				wrapped.cancel = cancel
			}
			session := newDurableTestSession(t, model, &fakeServer{}, wrapped, sink)
			defer session.Close()
			if mode == "yolo" {
				if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
					t.Fatal(err)
				}
			}
			result, err := session.Send(ctx, "恢复明确归档到指定目标")
			if err != nil {
				t.Fatal(err)
			}
			registered := false
			for _, tool := range model.requests[0].Tools {
				if tool.Function.Name == "restore_archive" {
					registered = true
				}
			}
			if !registered {
				t.Fatal("restore tool not registered")
			}
			if mode != "yolo" {
				p := result.PendingFileMutation
				if p == nil || p.Path != source || p.DestinationPath != "restored" || p.BaseVersion != version || p.Operation != "restore_archive" || !strings.Contains(p.Preview, version) {
					t.Fatal("invalid pending", result)
				}
				if _, err := os.Stat(filepath.Join(root, "restored")); !os.IsNotExist(err) {
					t.Fatal("prepare published target")
				}
				if mode == "conflict" {
					if err := os.WriteFile(filepath.Join(root, "restored"), []byte("keep"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "changed" {
					if err := os.WriteFile(filepath.Join(root, source), []byte("changed size"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "cancel_pending" {
					_, err = session.CancelPendingFileMutation("restore-call")
				} else {
					resolution := FileMutationApprove
					if mode == "decline" {
						resolution = FileMutationDecline
					}
					result, err = session.ResolveFileMutation(ctx, "restore-call", resolution)
				}
				if (err != nil) != (mode == "wal_failure") {
					t.Fatalf("mode=%s err=%v", mode, err)
				}
			}
			published := mode == "approve" || mode == "yolo" || mode == "late_cancel" || mode == "unknown" || mode == "settlement_failure" || mode == "model_failure"
			if !published {
				if _, err := os.Stat(filepath.Join(root, source)); err != nil {
					t.Fatal("source changed", err)
				}
				if mode == "recorded_identity" {
					var value map[string]any
					if json.Unmarshal([]byte(session.toolHistory["restore-call"]), &value) != nil || value["code"] != "file_call_recorded" || value["replay"] != false || value["destination"] != "restored" {
						t.Fatal("replay projection", value)
					}
				}
				if mode != "conflict" && mode != "changed" && wrapped.commits != 0 {
					t.Fatal("unauthorized commit", wrapped.commits)
				}
				return
			}
			if len(sink.files) != 1 || len(sink.settlements) != 1 || wrapped.commits != 1 || sink.files[0].Effect.Source.Version != version || !sink.files[0].Effect.IsArchiveRestore() {
				t.Fatal("WAL/settlement", sink.files, sink.settlements)
			}
			if _, err := os.Stat(filepath.Join(root, source)); !os.IsNotExist(err) {
				t.Fatal("source remains", err)
			}
			data, err := os.ReadFile(filepath.Join(root, "restored"))
			if err != nil || string(data) != "private-restore-bytes\x00\xff" {
				t.Fatal(data, err)
			}
			var fact struct {
				Effect struct {
					Source struct{ Path, Version string }
					Target struct{ Path, Version string }
				} `json:"file_effect"`
				Destination string `json:"destination"`
			}
			if err := json.Unmarshal([]byte(session.toolHistory["restore-call"]), &fact); err != nil || fact.Effect.Source.Path != source || fact.Effect.Target.Path != "restored" || fact.Effect.Source.Version != version || fact.Effect.Target.Version != "" || fact.Destination != "restored" {
				t.Fatal("lost history", fact, err)
			}
			cp, err := session.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := EncodeSessionCheckpoint(cp)
			if err != nil || strings.Contains(string(encoded), "private-restore-bytes") {
				t.Fatal("invalid checkpoint/body leakage", err)
			}
			if mode == "late_cancel" || mode == "unknown" || mode == "model_failure" || mode == "settlement_failure" {
				if !strings.Contains(result.Text, "归档恢复") && !strings.Contains(result.Text, "从归档恢复") || !strings.Contains(result.Text, source) || !strings.Contains(result.Text, "restored") || strings.Contains(result.Text, "未修改源") {
					t.Fatal("dishonest fallback", result.Text)
				}
			}
		})
	}
}
