package agentcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type journalExecutor struct {
	workspace.Executor
	commits int
	after   func(int, workspace.Result)
}

func (w *journalExecutor) CommitMutation(ctx context.Context, p *workspace.PreparedMutation) workspace.Result {
	r := w.Executor.CommitMutation(ctx, p)
	w.commits++
	if w.after != nil {
		w.after(w.commits, r)
	}
	return r
}

func journalCall(id, tool string, args map[string]any) modelclient.ToolCall {
	b, _ := json.Marshal(args)
	return modelclient.ToolCall{ID: id, Type: "function", Function: modelclient.ToolFunction{Name: tool, Arguments: string(b)}}
}

func journalMkdir(id, path string) modelclient.ToolCall {
	return journalCall(id, "mkdir", map[string]any{"path": path, "parents": true})
}

func journalModel(calls ...modelclient.ToolCall) *scriptedControllerModel {
	return &scriptedControllerModel{responses: []modelclient.Response{
		{Message: modelclient.Message{Role: "assistant", ToolCalls: calls}},
		{Message: modelclient.Message{Role: "assistant", Content: "操作已完成"}},
	}}
}

func newJournalController(t *testing.T, model agentloop.Model, limits agentsession.Limits) (*Controller, *journalExecutor, string, func() *Controller) {
	t.Helper()
	root, storeRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	executor, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &journalExecutor{Executor: executor}
	deps := controllerDependencies(controllerStoreWithLimits(t, storeRoot, secrets, limits), model, server, root, provider)
	deps.LoopOptions.Workspace = wrapped
	c, err := Start(t.Context(), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.abort)
	if err = c.loop.SetFileAuthorizationMode(agentloop.FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	id := c.SessionID()
	resume := func() *Controller {
		c.abort()
		r, err := Resume(t.Context(), controllerDependencies(controllerStoreWithLimits(t, storeRoot, secrets, limits), &controllerModel{}, server, root, provider), ResumeOptions{SessionID: id, CurrentWorkspace: root})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.Close)
		return r
	}
	return c, wrapped, root, resume
}

func journalReceipts(t *testing.T, c *Controller, outcomes ...string) []agentsession.FileReceipt {
	t.Helper()
	c.mu.Lock()
	receipts := append([]agentsession.FileReceipt(nil), c.record.FileReceipts...)
	c.mu.Unlock()
	if len(receipts) != len(outcomes) {
		t.Fatalf("receipts=%+v expected=%v", receipts, outcomes)
	}
	for i, outcome := range outcomes {
		if receipts[i].Outcome != outcome {
			t.Fatalf("receipt %d=%+v want %s", i, receipts[i], outcome)
		}
	}
	return receipts
}

func TestControllerJournalFileAndPreferenceMarkersCoexistOnCrash(t *testing.T) {
	c, executor, _, resume := newJournalController(t, &controllerModel{}, agentsession.Limits{})
	if err := c.BeginTurn(t.Context(), agentloop.DirtyIntent{TurnSequence: 1, OperationClass: "agent-turn"}); err != nil {
		t.Fatal(err)
	}
	for i, path := range []string{"alpha/" + strings.Repeat("x", 256) + "/last", "beta"} {
		p, failed := executor.PrepareMutation(t.Context(), "mkdir", `{"parents":true,"path":"`+path+`"}`)
		if p == nil {
			t.Fatal(failed)
		}
		id := []string{"alpha", "beta"}[i]
		if err := c.BeforeFilePublication(t.Context(), agentloop.FileWriteAhead{ToolCallID: id, Effect: p.FileEffect()}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			r := executor.CommitMutation(t.Context(), p)
			if r.Publication != workspace.PublicationUnknown || r.Effect.Directories.Created != 1 {
				t.Fatal(r)
			}
			if err := c.AfterFilePublication(t.Context(), id, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	pref := agentloop.PreferenceWriteAhead{
		ToolCallID: "pref", CreateOperationID: "10000000-0000-4000-8000-000000000001", AdmitOperationID: "10000000-0000-4000-8000-000000000002", RejectOperationID: "10000000-0000-4000-8000-000000000003",
		Stage: agentloop.PreferenceStageCreate, StableCode: "preference_outcome_unknown",
		Payload: agentloop.PreferencePayload{Content: "偏好图示", Reason: "用户明确要求", Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable", ValidUntil: time.Now().UTC().Add(time.Hour)},
	}
	if err := c.BeforePreferenceWrite(t.Context(), pref); err != nil {
		t.Fatal(err)
	}
	if len(c.server.(*controllerServer).createRequests) != 0 {
		t.Fatal("marker triggered a server write")
	}
	r := resume()
	receipts := journalReceipts(t, r, "unknown", "unknown")
	if receipts[0].Effect.Directories.Created != 1 || receipts[1].Effect.Directories.Created != 0 {
		t.Fatal(receipts)
	}
	if len(r.record.PreferenceReceipts) != 1 || r.record.PreferenceReceipts[0].ToolCallID != "pref" {
		t.Fatal(r.record.PreferenceReceipts)
	}
}

func TestControllerJournalConsecutiveResultsSurviveNormalFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"normal", "model-failure", "late-cancel", "partial-mkdir"} {
		t.Run(mode, func(t *testing.T) {
			second := "beta/child"
			if mode == "partial-mkdir" {
				second = "beta/" + strings.Repeat("x", 256) + "/last"
			}
			model := journalModel(journalMkdir("alpha-call", "alpha"), journalMkdir("beta-call", second))
			if mode == "model-failure" {
				model.responses = model.responses[:1]
			}
			c, executor, root, resume := newJournalController(t, model, agentsession.Limits{})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "late-cancel" {
				executor.after = func(n int, _ workspace.Result) {
					if n == 2 {
						cancel()
					}
				}
			}
			_, err := c.Send(ctx, "创建两个目录")
			if mode != "late-cancel" && err != nil {
				t.Fatal(err)
			}
			if executor.commits != 2 {
				t.Fatal("commits", executor.commits)
			}
			expected := agentsession.NoticeOutcomeCompleted
			if mode == "partial-mkdir" {
				expected = agentsession.NoticeOutcomeUnknown
			}
			r := resume()
			receipts := journalReceipts(t, r, agentsession.NoticeOutcomeCompleted, expected)
			if receipts[0].ToolCallID != "alpha-call" || receipts[1].ToolCallID != "beta-call" || receipts[0].Effect.Directories.Created != 1 {
				t.Fatal(receipts)
			}
			created := 2
			if mode == "partial-mkdir" {
				created = 1
			}
			if receipts[1].Effect.Directories.Created != created {
				t.Fatal(receipts)
			}
			for _, path := range []string{"alpha", "beta"} {
				if _, err = os.Stat(filepath.Join(root, path)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestControllerJournalMkdirThenCopyCompleted(t *testing.T) {
	model := journalModel()
	c, executor, root, resume := newJournalController(t, model, agentsession.Limits{})
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("actual bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	stat := executor.Execute(t.Context(), "stat", `{"path":"source"}`)
	version := stat.Value.(map[string]any)["entry_version"].(string)
	model.responses[0].Message.ToolCalls = []modelclient.ToolCall{journalMkdir("mkdir-call", "folder"), journalCall("copy-call", "copy", map[string]any{"source": "source", "destination": "folder/copy", "expected_version": version})}
	if _, err := c.Send(t.Context(), "创建目录并复制"); err != nil {
		t.Fatal(err)
	}
	receipts := journalReceipts(t, resume(), "completed", "completed")
	if receipts[1].Effect.Source.Version != version || receipts[1].Effect.Target.Version == "" {
		t.Fatal(receipts)
	}
	data, err := os.ReadFile(filepath.Join(root, "folder", "copy"))
	if err != nil || string(data) != "actual bytes" {
		t.Fatal(string(data), err)
	}
}

func TestControllerJournalSurvivesFileThenPreferenceWithoutExtraAuthorization(t *testing.T) {
	calls := []modelclient.ToolCall{journalMkdir("alpha-call", "alpha"), journalMkdir("beta-call", "beta"), journalCall("pref-call", "remember_preference", map[string]any{"content": "偏好图示", "reason": "用户明确要求", "category": "interaction_preference", "sensitivity": "non_sensitive", "stability": "stable"})}
	for _, approve := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending-crash", true: "authorized"}[approve], func(t *testing.T) {
			c, _, _, resume := newJournalController(t, journalModel(calls...), agentsession.Limits{})
			pending, err := c.Send(t.Context(), "创建目录并长期记住偏好")
			server := c.server.(*controllerServer)
			if err != nil || pending.Pending == nil || len(server.createRequests) != 0 {
				t.Fatal(pending, err, server.createRequests)
			}
			if approve {
				if _, err := c.ResolvePreference(t.Context(), agentloop.PreferenceSave); err != nil {
					t.Fatal(err)
				}
				if len(server.createRequests) != 1 {
					t.Fatal("extra preference write", server.createRequests)
				}
			}
			journalReceipts(t, resume(), "completed", "completed")
		})
	}
}

func TestControllerJournalSecondWALCrashBeforeAndAfterCommit(t *testing.T) {
	for _, commitSecond := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "after"}[commitSecond], func(t *testing.T) {
			c, executor, root, resume := newJournalController(t, &controllerModel{}, agentsession.Limits{})
			if err := c.BeginTurn(t.Context(), agentloop.DirtyIntent{TurnSequence: 1, OperationClass: "agent-turn"}); err != nil {
				t.Fatal(err)
			}
			for i, path := range []string{"alpha", "beta"} {
				p, failed := executor.PrepareMutation(t.Context(), "mkdir", `{"path":"`+path+`"}`)
				if p == nil {
					t.Fatal(failed)
				}
				if err := c.BeforeFilePublication(t.Context(), agentloop.FileWriteAhead{ToolCallID: path, Effect: p.FileEffect()}); err != nil {
					t.Fatal(err)
				}
				if i == 0 || commitSecond {
					r := executor.CommitMutation(t.Context(), p)
					if r.Publication != workspace.PublicationCompleted {
						t.Fatal(r)
					}
					if i == 0 {
						if err := c.AfterFilePublication(t.Context(), path, r); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			receipts := journalReceipts(t, resume(), "completed", "unknown")
			if receipts[1].Effect.Directories.Created != 0 {
				t.Fatal("WAL fabricated completion", receipts)
			}
			_, err := os.Stat(filepath.Join(root, "beta"))
			if commitSecond && err != nil || !commitSecond && !os.IsNotExist(err) {
				t.Fatal("recovery replay or deletion", err)
			}
		})
	}
}

func TestControllerJournalCapacityAndSettlementFailureStopNextSideEffect(t *testing.T) {
	for _, mode := range []string{"count", "bytes", "settlement-io"} {
		t.Run(mode, func(t *testing.T) {
			limits := agentsession.DefaultLimits()
			if mode == "count" {
				limits.ReceiptCount = 2
			}
			if mode == "bytes" {
				limits.DirtyMarkerBytes = 1600
			}
			c, executor, root, resume := newJournalController(t, journalModel(journalMkdir("alpha", "alpha"), journalMkdir("beta", "beta"), journalMkdir("gamma", "gamma")), limits)
			if mode == "settlement-io" {
				executor.after = func(n int, _ workspace.Result) {
					if n == 2 {
						if err := c.handle.Close(); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if _, err := c.Send(t.Context(), "创建三个目录"); err == nil {
				t.Fatal("durability failure not reported")
			}
			if executor.commits < 1 || executor.commits > 2 {
				t.Fatal("unexpected commit count", executor.commits)
			}
			if _, err := os.Stat(filepath.Join(root, "gamma")); !os.IsNotExist(err) {
				t.Fatal("next side effect executed", err)
			}
			// Neither another WAL nor normal shutdown may consume uncertain evidence.
			before := *c.dirty
			if err := c.BeforeFilePublication(t.Context(), agentloop.FileWriteAhead{ToolCallID: "next"}); err == nil {
				t.Fatal("failure latch ignored")
			}
			if !reflect.DeepEqual(before, *c.dirty) {
				t.Fatal("old in-memory evidence changed")
			}
			if err := c.Shutdown(t.Context()); err == nil {
				t.Fatal("shutdown swallowed dirty failure")
			}
			r := resume()
			if mode == "count" {
				journalReceipts(t, r, "completed", "completed")
			}
			if mode == "settlement-io" {
				journalReceipts(t, r, "completed", "unknown")
			}
			if mode == "bytes" {
				if len(r.record.FileReceipts) != executor.commits {
					t.Fatal("lost bounded evidence", r.record.FileReceipts, executor.commits)
				}
			}
		})
	}
}

func TestControllerJournalRejectsRepeatedCallAndConflictingSettlement(t *testing.T) {
	for _, mode := range []string{"same-wal", "plan-conflict", "missing-effect", "wrong-reference", "unchanged"} {
		t.Run(mode, func(t *testing.T) {
			c, executor, _, resume := newJournalController(t, &controllerModel{}, agentsession.Limits{})
			if err := c.BeginTurn(t.Context(), agentloop.DirtyIntent{TurnSequence: 1, OperationClass: "agent-turn"}); err != nil {
				t.Fatal(err)
			}
			p, _ := executor.PrepareMutation(t.Context(), "mkdir", `{"path":"alpha"}`)
			wal := agentloop.FileWriteAhead{ToolCallID: "alpha", Effect: p.FileEffect()}
			if err := c.BeforeFilePublication(t.Context(), wal); err != nil {
				t.Fatal(err)
			}
			var err error
			if mode == "same-wal" {
				err = c.BeforeFilePublication(t.Context(), wal)
			} else if mode == "unchanged" {
				err = c.AfterFilePublication(t.Context(), "alpha", workspace.Result{Publication: workspace.PublicationUnchanged})
				if err != nil {
					t.Fatal(err)
				}
				journalReceipts(t, resume())
				return
			} else {
				r := executor.CommitMutation(t.Context(), p)
				switch mode {
				case "plan-conflict":
					r.Effect.Target.Path = "different"
				case "missing-effect":
					r.Effect = nil
				case "wrong-reference":
					r.Reference.Path = "different"
				}
				err = c.AfterFilePublication(t.Context(), "alpha", r)
			}
			if err == nil {
				t.Fatal("conflicting evidence accepted")
			}
			if mode == "same-wal" && !errors.Is(err, agentsession.ErrCheckpointConflict) {
				t.Fatal(err)
			}
			journalReceipts(t, resume(), "unknown")
		})
	}
}
