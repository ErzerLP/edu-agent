package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

func conversationStudioFixture(t *testing.T) (*runtimeFixture, context.Context, string, StudioJobCreate) {
	t.Helper()
	f := topicFixture(t)
	content, err := learningcontent.New(f.pool, nil, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	content.ConfigureStudioSources(knowledgepg.New(f.pool))
	f.service.ConfigureStudio(content)
	ctx, _ := learningspace.WithScope(context.Background(), learningspace.DefaultID)
	id := newHistory(t, f, true, "")
	selection := knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{selectedDocument(t, f, "TCP 与子网资料")}}
	req := historyRequest(t, f, id, "RAW_CHAT_NEVER_READ")
	req.SourceSelection = &selection
	if _, err = f.service.Create(ctx, f.actor, learningspace.DefaultID, runtimeGoal(""), req); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	if _, err = f.service.RequestTopicIndex(ctx, f.actor, learningspace.DefaultID, id, TopicIndexRequest{OperationID: uuid.NewString(), Generation: 1, SourceSelection: &selection}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	page := readTopics(t, f, id)
	var topic string
	for _, v := range page.Items {
		if v.Title == "TCP" {
			topic = v.ID
		}
	}
	var through string
	if err = f.pool.QueryRow(ctx, `SELECT message_id::text FROM learning_conversation_messages WHERE conversation_id=$1 ORDER BY ordinal DESC LIMIT 1`, id).Scan(&through); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"operation_id": uuid.NewString(), "privacy_generation": 1, "kind": "note", "source_selection": selection, "direction_version": 0, "conversation_scope": map[string]any{"conversation_id": id, "topic_ids": []string{topic}, "index_revision": page.Revision, "through_message": through}})
	var c StudioJobCreate
	if err = json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return f, ctx, id, c
}

// Dropping the topic snapshot, accepting a foreign topic ID, or reading a turn
// body must break this test. Corrupt ciphertext makes even an unused raw read fail.
func TestPostgreSQLStudioConversationSelectedSnapshotNoRawMessages(t *testing.T) {
	f, ctx, id, c := conversationStudioFixture(t)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = gates(ctx, tx, true); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_tutor_turns SET ciphertext='unreadable'::bytea WHERE conversation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var selectedID string
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &request)
		var input struct {
			Topics []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"conversation_topics"`
		}
		_ = json.Unmarshal([]byte(request.Messages[1].Content), &input)
		if len(input.Topics) != 1 || input.Topics[0].Title != "TCP" {
			t.Errorf("selected scope missing or widened: %+v", input)
		} else {
			selectedID = input.Topics[0].ID
		}
		if strings.Contains(string(raw), "RAW_CHAT_NEVER_READ") {
			t.Error("raw conversation sent")
		}
		result, _ := json.Marshal(map[string]any{"title": "TCP 笔记", "coverage": "partial", "uncovered": []string{}, "explanation": "按所选话题规划。", "sections": []any{map[string]any{"id": "tcp", "topic": "TCP", "objective": "回顾连接", "chapters": []int{1}, "topic_ids": []string{selectedID}}}})
		studioReply(w, string(result), "stop")
	}
	j, err := f.service.CreateStudioJob(ctx, f.actor, c)
	if err != nil {
		t.Fatal(err)
	}
	// A newer index must not expand an accepted job or prevent its execution.
	tx, err = f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = gates(ctx, tx, true); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_topic_index_heads SET revision=revision+1 WHERE conversation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	j, err = f.service.StudioJob(ctx, f.actor, j.ID)
	if err != nil || j.Status != "awaiting_confirmation" {
		t.Fatalf("plan: %+v %v", j, err)
	}
	c.OperationID = uuid.NewString()
	if _, err = f.service.CreateStudioJob(ctx, f.actor, c); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale visible selection accepted: %v", err)
	}
	// A model cannot use plan edits to introduce an unselected topic. Failed
	// candidates preserve the confirmed selection and the last valid plan.
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		studioReply(w, string(mustJSON(t, map[string]any{"title": "越界规划", "coverage": "partial", "uncovered": []string{}, "explanation": "新增话题", "sections": []any{map[string]any{"id": "foreign", "topic": "子网", "objective": "未选内容", "chapters": []int{1}, "topic_ids": []string{uuid.NewString()}}}})), "stop")
	}
	if _, err = f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "message", StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: j.Version, PlanRevision: j.ActivePlanRevision, Text: "继续整理所选话题"}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	j, err = f.service.StudioJob(ctx, f.actor, j.ID)
	if err != nil || j.Status != "failed" || j.Plan == nil || len(j.Plan.Sections) != 1 || j.Plan.Sections[0].Topic != "TCP" {
		t.Fatal("unselected model topic entered plan", j, err)
	}
	var roots, calls int
	if err = f.pool.QueryRow(ctx, `SELECT count(DISTINCT t.id),count(*) FROM learning_model_tasks t JOIN learning_model_calls c ON c.task_id=t.id WHERE t.kind='studio' AND t.origin_id=$1`, j.ID).Scan(&roots, &calls); err != nil || roots != 1 || calls != 3 {
		t.Fatal("planning and repair usage split across jobs", roots, calls, err)
	}
}

func TestPostgreSQLStudioConversationTailRequiresExplicitChoice(t *testing.T) {
	f, ctx, id, c := conversationStudioFixture(t)
	req := historyRequest(t, f, id, "最新尾部")
	req.SourceSelection = &c.SourceSelection
	if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, runtimeGoal(""), req); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	if _, err := f.service.CreateStudioJob(ctx, f.actor, c); !errors.Is(err, ErrConflict) {
		t.Fatalf("uncovered tail silently accepted: %v", err)
	}
	previous := readTopics(t, f, id)
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", http.StatusBadRequest) }
	if _, err := f.service.RequestTopicIndex(ctx, f.actor, learningspace.DefaultID, id, TopicIndexRequest{OperationID: uuid.NewString(), Generation: 1, ExpectedRevision: previous.Revision, SourceSelection: &c.SourceSelection}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	f.modelOverride = nil
	failed := readTopics(t, f, id)
	if failed.Status != "failed" || failed.Revision != previous.Revision || len(failed.Items) != len(previous.Items) {
		t.Fatal("failed index discarded previous topics", failed)
	}
	raw, _ := json.Marshal(c)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	scope, ok := fields["conversation_scope"].(map[string]any)
	if !ok {
		t.Fatal("conversation selection was discarded")
	}
	scope["accept_partial"] = true
	_ = json.Unmarshal(mustJSON(t, fields), &c)
	j, err := f.service.CreateStudioJob(ctx, f.actor, c)
	if err != nil {
		t.Fatal("explicit old scope", err)
	}
	if j.ID == "" {
		t.Fatal("no persisted job")
	}
	// Alternatively, organize the new tail and bind a fresh visible revision.
	p := readTopics(t, f, id)
	if _, err = f.service.RequestTopicIndex(ctx, f.actor, learningspace.DefaultID, id, TopicIndexRequest{OperationID: uuid.NewString(), Generation: 1, ExpectedRevision: p.Revision, SourceSelection: &c.SourceSelection}); err != nil {
		t.Fatal(err)
	}
	f.work(t) // the queued Studio plan may execute first
	f.work(t)
	p = readTopics(t, f, id)
	if p.CoveredMessages != p.TotalMessages {
		t.Fatalf("tail not indexed: %+v", p)
	}
	var through string
	if err = f.pool.QueryRow(ctx, `SELECT message_id::text FROM learning_conversation_messages WHERE conversation_id=$1 ORDER BY ordinal DESC LIMIT 1`, id).Scan(&through); err != nil {
		t.Fatal(err)
	}
	scope["index_revision"], scope["through_message"], scope["accept_partial"] = p.Revision, through, false
	fields["operation_id"] = uuid.NewString()
	_ = json.Unmarshal(mustJSON(t, fields), &c)
	if _, err = f.service.CreateStudioJob(ctx, f.actor, c); err != nil {
		t.Fatal("fresh indexed scope", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPostgreSQLStudioConversationOptionsAndNoSources(t *testing.T) {
	f, ctx, id, c := conversationStudioFixture(t)
	read := func(selection knowledge.SourceSelection) StudioTopicOptions {
		t.Helper()
		var result StudioTopicOptions
		if err := f.service.StudioConversationTopics(ctx, f.actor, id, StudioTopicOptionsRequest{SourceSelection: selection}, func(p StudioTopicOptions) error { result = p; return nil }); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if p := read(c.SourceSelection); len(p.Items) != 2 || p.Partial || p.ThroughMessage == "" {
		t.Fatal("readable topics missing", p)
	}
	other := knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{selectedDocument(t, f, "其他来源")}}
	if p := read(other); len(p.Items) != 0 {
		t.Fatal("foreign source scope offered", p)
	}
	c.SourceSelection = knowledge.SourceSelection{Mode: "none", Items: []knowledge.SourceIdentity{}}
	if p := read(c.SourceSelection); len(p.Items) != 0 {
		t.Fatal("zero sources offered old topics", p)
	}
	if _, err := f.service.CreateStudioJob(ctx, f.actor, c); !errors.Is(err, ErrInvalid) {
		t.Fatal("source-less job accepted", err)
	}
}

func TestPostgreSQLStudioConversationLateResultsCannotRevive(t *testing.T) {
	for _, erase := range []bool{false, true} {
		name := "delete"
		if erase {
			name = "privacy_clear"
		}
		t.Run(name, func(t *testing.T) {
			f, ctx, id, c := conversationStudioFixture(t)
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			started, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			f.service.Lease = time.Second
			f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
				}
				studioReply(w, string(mustJSON(t, map[string]any{"title": "迟到笔记", "coverage": "partial", "uncovered": []string{}, "explanation": "已整理所选话题", "sections": []any{map[string]any{"id": "tcp", "topic": "TCP", "objective": "回顾连接", "chapters": []int{1}, "topic_ids": c.ConversationScope.TopicIDs}}})), "stop")
			}
			j, err := f.service.CreateStudioJob(ctx, f.actor, c)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := f.service.RunOnce(ctx); done <- err }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if erase {
				permits := privacy.NewReadPermitManager()
				tutoring := tutoringdb.New(f.pool)
				store := privacydb.New(f.pool, privacydb.WithReadPermits(permits), privacydb.WithLocalOwner(identitydb.New(f.pool)), privacydb.WithLocalOwner(knowledgepg.New(f.pool)), privacydb.WithLocalOwner(learningdb.New(f.pool, tutoring)), privacydb.WithLocalOwner(tutoring), privacydb.WithLocalOwner(memorydb.New(f.pool)), privacydb.WithLocalOwner(outboxdb.New(f.pool)))
				grants, e := privacy.NewErasureGrantService(privacydb.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
				if e != nil {
					t.Fatal(e)
				}
				grant, e := grants.Issue(ctx, f.actor.Device.ID, "对话整理清除验收")
				if e != nil {
					t.Fatal(e)
				}
				now := time.Now().UTC()
				barrier, e := store.CommitBarrierAuthorized(ctx, privacy.ErasureRequest{DeviceID: f.actor.Device.ID, OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}, privacy.NewErasureGrantAuthorization(f.actor.Device.ID, grant.Token))
				if e != nil {
					t.Fatal(e)
				}
				if _, e = store.RunLocalScrub(ctx, barrier.ErasureID); e != nil {
					t.Fatal(e)
				}
			} else {
				var version int64
				if err = f.pool.QueryRow(ctx, `SELECT version FROM learning_tutor_conversations WHERE id=$1`, id).Scan(&version); err != nil {
					t.Fatal(err)
				}
				if err = f.service.ChangeConversation(ctx, f.actor, learningspace.DefaultID, id, version, "", true); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("late job remained active")
			}
			if _, err = f.service.StudioJob(ctx, f.actor, j.ID); err == nil {
				t.Fatal("late job readable")
			}
			var count int
			if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_studio_jobs)+(SELECT count(*) FROM learning_studio_plans)+(SELECT count(*) FROM learning_studio_units)+(SELECT count(*) FROM learning_studio_artifacts)`).Scan(&count); err != nil || count != 0 {
				t.Fatal("late job revived", count, err)
			}
		})
	}
}
