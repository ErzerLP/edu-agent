package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/integrations/nocturne"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
)

func memoryFixture(t *testing.T, content string, final func(string)) *runtimeFixture {
	t.Helper()
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			args, _ := json.Marshal(map[string]string{"content": content, "reason": "用户提出稳定交互偏好", "category": "interaction_preference", "sensitivity": "non_sensitive", "stability": "stable"})
			stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "save-preference", Type: "function", Function: modelclient.ToolFunction{Name: "request_memory", Arguments: string(args)}}}})
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if final != nil {
			final(string(raw))
		}
		stream(w, modelclient.Message{Role: "assistant", Content: "已核对原记忆服务回执"})
	})
	if _, err := f.pool.Exec(context.Background(), `UPDATE device_tokens SET scopes=scopes||ARRAY['memory:web','memory:read','memory:write'] WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	return f
}

// 确定性远端模拟原交付已写入、回读哈希相同的对账路径，不直接改 PG 状态。
type contextMemoryRemote struct {
	memory.NocturneRemote
	content string
	node    string
}

func (r contextMemoryRemote) Capabilities(context.Context) (memory.NocturneCapabilities, error) {
	return memory.NocturneCapabilities{UpstreamCommit: memory.NocturneUpstreamCommit, CompatRevision: memory.NocturneCompatRevision, BootEpoch: "记忆上下文夹具"}, nil
}
func (r contextMemoryRemote) EnsureParent(context.Context) error { return nil }
func (r contextMemoryRemote) GetNode(_ context.Context, path string) (memory.RemoteNode, error) {
	return memory.RemoteNode{NodeID: r.node, Path: path, URI: "core://" + path, Content: r.content}, nil
}
func (r contextMemoryRemote) References(context.Context, string) (memory.RemoteReferences, error) {
	return memory.RemoteReferences{NodeID: r.node, Complete: true, ActiveMemoryID: 1}, nil
}

type memoryExportFunc func(context.Context, memory.PageRequest) (memory.ExportPage, error)

func (f memoryExportFunc) Export(ctx context.Context, p memory.PageRequest) (memory.ExportPage, error) {
	return f(ctx, p)
}

func TestPostgreSQLMentorMemoryContextSourceAndSendFence(t *testing.T) {
	for _, mode := range []string{"available", "deleted_before_send", "scope_revoked_before_send"} {
		t.Run(mode, func(t *testing.T) {
			content := "我长期偏好用几何图形解释概率"
			f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				raw, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(raw), content) || !strings.Contains(string(raw), "全局长期") {
					t.Error("模型没有收到获准内容及范围", string(raw))
				}
				stream(w, modelclient.Message{Role: "assistant", Content: "已基于获准偏好解释"})
			})
			ctx := context.Background()
			if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=scopes||ARRAY['memory:read'] WHERE id=$1`, f.actor.TokenID); err != nil {
				t.Fatal(err)
			}
			store := memorydb.New(f.pool)
			service, err := memory.NewService(store, memory.ServiceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			saved, err := service.CreateCandidate(ctx, memory.DevicePrincipal{DeviceID: f.actor.Device.ID}, memory.CreateCandidateCommand{OperationID: uuid.NewString(), Content: content, Reason: "用户明确陈述", Category: memory.CategoryInteractionPreference, Sensitivity: memory.SensitivityNonSensitive, Stability: memory.StabilityStable, ValidUntil: time.Now().UTC().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			if saved.Record == nil {
				saved, err = service.DecideCandidate(ctx, memory.DevicePrincipal{DeviceID: f.actor.Device.ID}, memory.DecideCandidateCommand{OperationID: uuid.NewString(), CandidateID: saved.Candidate.Candidate.ID, ExpectedRevision: saved.Candidate.Candidate.Revision, Decision: memory.DecisionAdmit, Reason: "用户明确批准夹具偏好"})
			}
			if err != nil || saved.Record == nil {
				t.Fatal("未建立原服务准入记录", err)
			}
			remote := contextMemoryRemote{content: content, node: uuid.NewString()}
			consumer, err := nocturne.NewConsumer(store, remote, nocturne.ConsumerOptions{Lease: time.Minute, Namespace: "edu-agent", Domain: "core", ParentPath: "edu-agent"})
			if err != nil {
				t.Fatal(err)
			}
			worker, err := outbox.NewWorker(outboxdb.New(f.pool), map[string]outbox.Consumer{"memory.delivery": consumer}, outbox.WorkerOptions{BatchSize: 10, Lease: time.Minute, BaseBackoff: time.Millisecond, MaxBackoff: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			view, err := service.Record(ctx, saved.Record.LogicalMemoryID)
			if err != nil || view.Record.Status != memory.RecordApplied {
				t.Fatal("真实交付回执未推进记录", view, err)
			}
			permits := privacy.NewReadPermitManager()
			exporter, err := nocturne.NewMemoryExporter(nocturne.MemoryExporterOptions{Service: service, Remote: remote, ReadPermits: permits, ParentPath: "edu-agent"})
			if err != nil {
				t.Fatal(err)
			}
			f.service.ConfigureMemory(memoryExportFunc(func(ctx context.Context, p memory.PageRequest) (memory.ExportPage, error) {
				page, err := exporter.Export(ctx, p)
				if err != nil {
					return page, err
				}
				if mode == "deleted_before_send" {
					_, err = service.DeleteRecord(ctx, memory.DevicePrincipal{DeviceID: f.actor.Device.ID}, memory.DeleteRecordCommand{OperationID: uuid.NewString(), LogicalMemoryID: view.Record.LogicalMemoryID, ExpectedRevision: view.Record.Revision, ExpectedRecordGeneration: view.Record.RecordGeneration})
				} else if mode == "scope_revoked_before_send" {
					_, err = f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,'memory:read') WHERE id=$1`, f.actor.TokenID)
				}
				return page, err
			}), permits)
			run := f.accept(t)
			f.work(t)
			if mode != "available" {
				if f.calls.Load() != 0 {
					t.Fatal("读取后删除或撤权仍把旧记忆发给模型")
				}
				return
			}
			snapshot := f.snapshot(t, run.RunID)
			if snapshot.Status != "succeeded" || len(snapshot.MemorySources) != 1 || snapshot.MemorySources[0].MemoryID != saved.Record.LogicalMemoryID {
				t.Fatal("未显示真实来源", snapshot)
			}
			item, err := scan(f.pool.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1`, run.RunID))
			if err != nil {
				t.Fatal(err)
			}
			if err = f.service.decode(&item); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(item.body)
			if strings.Contains(string(raw), content) {
				t.Fatal("远端记忆正文被复制到 checkpoint")
			}
		})
	}
}

func TestPostgreSQLMentorMemoryExplicitApprovalAndReceipt(t *testing.T) {
	f := memoryFixture(t, "我偏好简洁的分步解释", func(raw string) {
		if !strings.Contains(raw, `\"public_status\":\"queued\"`) || !strings.Contains(raw, `\"status\":\"admitted\"`) {
			t.Error("继续没有拿到原服务真实排队回执", raw)
		}
	})
	ctx := context.Background()
	run := f.accept(t)
	f.work(t)
	snap := f.snapshot(t, run.RunID)
	if snap.Status != "waiting_approval" || snap.Interaction == nil || snap.Interaction.MemoryCandidateID == "" {
		t.Fatalf("未等待具体审批：%+v", snap)
	}
	service, err := memory.NewService(memorydb.New(f.pool), memory.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := service.Candidate(ctx, snap.Interaction.MemoryCandidateID)
	if err != nil || candidate.Candidate.Status != memory.CandidatePending || candidate.Candidate.Source != memory.SourceModelInference {
		t.Fatal("模型提议被当成用户批准", candidate, err)
	}
	command := Command{OperationID: uuid.NewString(), ExpectedVersion: snap.Version, Kind: "respond", InteractionID: snap.Interaction.ID, Answer: "好"}
	if _, err = f.service.Command(ctx, f.actor, learningspace.DefaultID, run.RunID, command); !errors.Is(err, ErrInvalid) {
		t.Fatal("普通回复意外批准", err)
	}
	command.Answer = memoryChecked
	if _, err = f.service.Command(ctx, f.actor, learningspace.DefaultID, run.RunID, command); !errors.Is(err, ErrConflict) {
		t.Fatal("未审批就继续", err)
	}
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM memory_record_revisions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("未明确批准已产生记录", count, err)
	}
	decision := memory.DecideCandidateCommand{OperationID: uuid.NewString(), CandidateID: candidate.Candidate.ID, ExpectedRevision: candidate.Candidate.Revision, Decision: memory.DecisionAdmit, Reason: "用户阅读具体内容后批准"}
	result, err := service.DecideCandidate(ctx, memory.DevicePrincipal{DeviceID: f.actor.Device.ID}, decision)
	if err != nil || result.Record == nil || result.Delivery.PublicStatus != memory.DeliveryQueued {
		t.Fatal("批准未创建真实记录", result, err)
	}
	if replay, err := service.DecideCandidate(ctx, memory.DevicePrincipal{DeviceID: f.actor.Device.ID}, decision); err != nil || !replay.Replayed || replay.Record.ID != result.Record.ID {
		t.Fatal("批准重试创建了重复记录", err)
	}
	if _, err = f.service.Command(ctx, f.actor, learningspace.DefaultID, run.RunID, command); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	if f.snapshot(t, run.RunID).Status != "succeeded" {
		t.Fatal("批准后未完成运行")
	}
}

func TestPostgreSQLMentorMemoryTemporaryCancelAndRevocation(t *testing.T) {
	for _, mode := range []string{"temporary", "cancel", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			content := "我偏好简洁的分步解释"
			if mode == "temporary" {
				content = "今天只有20分钟，请简洁解释"
			}
			f := memoryFixture(t, content, nil)
			ctx := context.Background()
			if mode == "revoked" {
				f.modelHook = func(int) {
					if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,'memory:web') WHERE id=$1`, f.actor.TokenID); err != nil {
						t.Error(err)
					}
				}
			}
			run := f.accept(t)
			f.work(t)
			snap := f.snapshot(t, run.RunID)
			if mode == "cancel" {
				if snap.Interaction == nil {
					t.Fatal("缺少候选交互")
				}
				_, err := f.service.Command(ctx, f.actor, learningspace.DefaultID, run.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: snap.Version, Kind: "respond", InteractionID: snap.Interaction.ID, Answer: memoryCancelled})
				if err != nil {
					t.Fatal(err)
				}
				var status string
				if err = f.pool.QueryRow(ctx, `SELECT status FROM memory_candidate_heads WHERE candidate_id=$1`, snap.Interaction.MemoryCandidateID).Scan(&status); err != nil || status != "rejected" {
					t.Fatal("取消未拒绝候选", status, err)
				}
			}
			var records, payloads int
			if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM memory_record_revisions),(SELECT count(*) FROM memory_candidate_payloads)`).Scan(&records, &payloads); err != nil || records != 0 || payloads != 0 {
				t.Fatal("临时/取消/撤权遗留长期记录或候选正文", records, payloads, err)
			}
		})
	}
}

func TestPostgreSQLMentorMemoryOuterRollbackRemovesCandidate(t *testing.T) {
	f := fixture(t, func(http.ResponseWriter, *http.Request, int) { t.Error("回滚检查不应调用模型") })
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	service, err := memory.NewService(memorydb.InTransaction(tx), memory.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CreateCandidate(ctx, memory.DevicePrincipal{DeviceID: f.actor.Device.ID}, memory.CreateCandidateCommand{OperationID: uuid.NewString(), RequireReview: true, Content: "我偏好简洁解释", Reason: "待用户确认", Category: memory.CategoryInteractionPreference, Sensitivity: memory.SensitivityNonSensitive, Stability: memory.StabilityStable, ValidUntil: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var candidates, operations int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM memory_candidate_heads),(SELECT count(*) FROM memory_operation_inbox)`).Scan(&candidates, &operations); err != nil || candidates != 0 || operations != 0 {
		t.Fatal("运行事务回滚后记忆操作仍被提交", candidates, operations, err)
	}
}
