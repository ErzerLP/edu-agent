package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

// Storage and worker gates are real; only the remote model is replaced.
func topicFixture(t *testing.T) *runtimeFixture {
	return fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		var req struct {
			Messages []modelclient.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var batch struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
			Candidates []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"candidates"`
		}
		if len(req.Messages) > 1 && json.Unmarshal([]byte(req.Messages[1].Content), &batch) == nil && len(batch.Messages) > 0 {
			ids := []string{}
			for _, m := range batch.Messages {
				ids = append(ids, m.ID)
			}
			topics := []any{}
			for _, title := range []string{"TCP", "子网"} {
				id := ""
				for _, c := range batch.Candidates {
					if c.Title == title {
						id = c.ID
					}
				}
				topics = append(topics, map[string]any{"id": id, "title": title, "anchor_ids": ids, "points": []any{map[string]any{"text": "比较连接与地址", "anchor_ids": ids}}, "observations": []any{map[string]any{"state": "question_open", "text": "仍在讨论", "evidence": "tutor_explanation", "anchor_ids": ids}}})
			}
			raw, _ := json.Marshal(map[string]any{"topics": topics})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"role": "assistant", "content": string(raw)}}}, "usage": map[string]int{"prompt_tokens": 30, "completion_tokens": 20, "total_tokens": 50}})
			return
		}
		content := "TCP 和子网的比较"
		if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "来源问答导师") {
			content = `{"answer":"TCP 和子网的比较","citations":[]}`
		}
		stream(w, modelclient.Message{Role: "assistant", Content: content})
	})
}
func readTopics(t *testing.T, f *runtimeFixture, id string) TopicPage {
	t.Helper()
	var p TopicPage
	if err := f.service.ReadTopics(context.Background(), f.actor, learningspace.DefaultID, id, "", 20, func(page TopicPage) error { p = page; return nil }); err != nil {
		t.Fatal(err)
	}
	return p
}
func requestTopics(t *testing.T, f *runtimeFixture, id string) Receipt {
	t.Helper()
	p := readTopics(t, f, id)
	r, err := f.service.RequestTopicIndex(context.Background(), f.actor, learningspace.DefaultID, id, TopicIndexRequest{OperationID: uuid.NewString(), ExpectedRevision: p.Revision, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestPostgreSQLTopicsIncrementalManyToManyAndLocation(t *testing.T) {
	f := topicFixture(t)
	id := newHistory(t, f, true, "")
	for _, p := range []string{"TCP", "子网", "回到 TCP"} {
		sendHistory(t, f, id, p)
		f.work(t)
	}
	before := f.calls.Load()
	p := readTopics(t, f, id)
	if p.Status != "not_started" || len(p.Items) != 0 || f.calls.Load() != before {
		t.Fatal("read started indexing", p)
	}
	requestTopics(t, f, id)
	f.work(t)
	p = readTopics(t, f, id)
	if p.Status != "ready" || len(p.Items) != 2 || p.CoveredMessages != 6 || p.TotalMessages != 6 {
		t.Fatalf("index: %+v", p)
	}
	var anchors TopicAnchorPage
	err := f.service.ReadTopicAnchors(context.Background(), f.actor, learningspace.DefaultID, id, p.Items[0].ID, 0, false, 2, func(p TopicAnchorPage) error { anchors = p; return nil })
	if err != nil || len(anchors.Items) != 2 || anchors.NextCursor == 0 {
		t.Fatal("anchor pagination", anchors, err)
	}
	var next TopicAnchorPage
	err = f.service.ReadTopicAnchors(context.Background(), f.actor, learningspace.DefaultID, id, p.Items[0].ID, anchors.NextCursor, false, 2, func(p TopicAnchorPage) error { next = p; return nil })
	if err != nil || len(next.Items) != 2 || next.Items[0].Ordinal <= anchors.Items[1].Ordinal {
		t.Fatal("anchor page overlaps", err)
	}
	target := next.Items[0]
	err = f.service.ReadMessage(context.Background(), f.actor, learningspace.DefaultID, id, target.MessageID, func(p TurnPage) error {
		if len(p.Items) != 1 || p.Items[0].RunID != target.RunID || p.Items[0].Messages[0].Content != "子网" {
			t.Errorf("wrong target: %+v", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_topic_anchors WHERE message_id=$1`, target.MessageID).Scan(&count); err != nil || count != 2 {
		t.Fatal("message must belong to two topics", count, err)
	}
	sendHistory(t, f, id, "TCP 再问")
	f.work(t)
	requestTopics(t, f, id)
	f.work(t)
	p = readTopics(t, f, id)
	if len(p.Items) != 2 || p.CoveredMessages != 8 {
		t.Fatal("incremental update", p)
	}
	var calls int
	if err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_model_calls c JOIN learning_model_tasks t ON t.id=c.task_id WHERE t.kind='topic_index' AND t.origin_id=$1`, id).Scan(&calls); err != nil || calls != 2 {
		t.Fatal("topic usage attribution", calls, err)
	}
}
func TestPostgreSQLTopicsFailureDoesNotAdvanceOrRetry(t *testing.T) {
	f := topicFixture(t)
	id := newHistory(t, f, true, "")
	sendHistory(t, f, id, "TCP")
	f.work(t)
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) { http.Error(w, "failed", http.StatusBadRequest) }
	requestTopics(t, f, id)
	f.work(t)
	p := readTopics(t, f, id)
	if p.Status != "failed" || p.CoveredThrough != 0 || p.Revision != 0 || len(p.Items) != 0 {
		t.Fatal("failure advanced index", p)
	}
	calls := f.calls.Load()
	f.work(t)
	if f.calls.Load() != calls {
		t.Fatal("automatic infinite retry")
	}
	f.modelOverride = nil
	requestTopics(t, f, id)
	f.work(t)
	if p = readTopics(t, f, id); p.CoveredMessages != 2 || p.Status != "ready" {
		t.Fatal("retry failed", p)
	}
}
func TestPostgreSQLTopicsConcurrentCASAndTemporary(t *testing.T) {
	f := topicFixture(t)
	id := newHistory(t, f, true, "")
	sendHistory(t, f, id, "TCP")
	f.work(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.service.RequestTopicIndex(context.Background(), f.actor, learningspace.DefaultID, id, TopicIndexRequest{OperationID: uuid.NewString(), ExpectedRevision: 0, Generation: 1})
			results <- err
		}()
	}
	wg.Wait()
	a, b := <-results, <-results
	if !(a == nil && errors.Is(b, ErrConflict) || b == nil && errors.Is(a, ErrConflict)) {
		t.Fatal("concurrent jobs accepted", a, b)
	}
	item, err := f.service.claim(context.Background())
	if err != nil || item == nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(context.Background(), `UPDATE learning_topic_index_heads SET revision=revision+1 WHERE conversation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	host := executionHost{service: f.service, owned: *item, body: item.body}
	if err = host.commitTopicBatch(context.Background(), TopicBatch{}, nil); !errors.Is(err, ErrConflict) {
		t.Fatal("stale CAS accepted", err)
	}
	temp := newHistory(t, f, false, "")
	sendHistory(t, f, temp, "临时 TCP")
	f.work(t)
	_, err = f.service.RequestTopicIndex(context.Background(), f.actor, learningspace.DefaultID, temp, TopicIndexRequest{OperationID: uuid.NewString(), Generation: 1})
	if !errors.Is(err, ErrTemporary) {
		t.Fatal(err)
	}
	var count int
	if err = f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM learning_conversation_messages WHERE conversation_id=$1)+(SELECT count(*) FROM learning_tutor_conversations WHERE id=$1 AND topic_index_scope IS NOT NULL)`, temp).Scan(&count); err != nil || count != 0 {
		t.Fatal("temporary anchors persisted", err)
	}
}

func TestPostgreSQLTopicsScopeChangeRejectsLateBatch(t *testing.T) {
	f := topicFixture(t)
	id := newHistory(t, f, true, "")
	sendHistory(t, f, id, "旧范围")
	f.work(t)
	requestTopics(t, f, id)
	owned, err := f.service.claim(context.Background())
	if err != nil || owned == nil {
		t.Fatal(err)
	}
	request := historyRequest(t, f, id, "新范围")
	request.SourceSelection = &knowledge.SourceSelection{Mode: "none", Items: []knowledge.SourceIdentity{}}
	if _, err = f.service.Create(context.Background(), f.actor, learningspace.DefaultID, uuid.Nil.String(), request); err != nil {
		t.Fatal("index blocked conversation", err)
	}
	h := executionHost{service: f.service, owned: *owned, body: owned.body}
	if err = h.commitTopicBatch(context.Background(), TopicBatch{}, nil); !errors.Is(err, ErrInactive) {
		t.Fatal("superseded scope accepted", err)
	}
	if p := readTopics(t, f, id); p.CoveredThrough != 0 || p.Revision != 0 {
		t.Fatal("late result advanced", p)
	}
}
func TestPostgreSQLTopicsLegacyReadIsLazy(t *testing.T) {
	f := topicFixture(t)
	id := newHistory(t, f, true, "")
	sendHistory(t, f, id, "旧 TCP")
	f.work(t)
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM learning_conversation_messages WHERE conversation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	calls := f.calls.Load()
	historyPage(t, f, id, 0, 1)
	p := readTopics(t, f, id)
	if p.Status != "not_started" || f.calls.Load() != calls {
		t.Fatal("opening old history dispatched")
	}
	var count int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_conversation_messages WHERE conversation_id=$1`, id).Scan(&count)
	if count != 0 {
		t.Fatal("GET backfilled")
	}
	requestTopics(t, f, id)
	f.work(t)
	if p = readTopics(t, f, id); p.CoveredMessages != 2 {
		t.Fatal("lazy backfill failed", p)
	}
}

func TestPostgreSQLTopicsScopeGapsAndAutomaticTrigger(t *testing.T) {
	f := topicFixture(t)
	id := newHistory(t, f, true, "")
	for i := 0; i < 4; i++ {
		sendHistory(t, f, id, "TCP 新消息")
		f.work(t)
	}
	if p := readTopics(t, f, id); p.Status != "queued" || p.CoveredMessages != 0 {
		t.Fatal("periodic trigger missing", p)
	}
	f.work(t)
	if p := readTopics(t, f, id); p.CoveredMessages != 8 {
		t.Fatal("automatic index", p)
	}
	// Explicit zero-source mode must not reuse legacy context or its summaries.
	request := historyRequest(t, f, id, "零来源新消息")
	request.SourceSelection = &knowledge.SourceSelection{Mode: "none", Items: []knowledge.SourceIdentity{}}
	if _, err := f.service.Create(context.Background(), f.actor, learningspace.DefaultID, uuid.Nil.String(), request); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	p := readTopics(t, f, id)
	_, err := f.service.RequestTopicIndex(context.Background(), f.actor, learningspace.DefaultID, id, TopicIndexRequest{OperationID: uuid.NewString(), ExpectedRevision: p.Revision, Generation: 1, SourceSelection: request.SourceSelection})
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	p = readTopics(t, f, id)
	if p.CoveredMessages != 10 || len(p.Items) != 4 {
		t.Fatal("source projections mixed", p)
	}
}
