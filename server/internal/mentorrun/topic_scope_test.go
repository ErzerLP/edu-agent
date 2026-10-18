package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningdirection"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

func TestPostgreSQLReadTopicScopeBoundsAndIsolation(t *testing.T) {
	f := topicFixture(t)
	id := newHistory(t, f, true, "")
	sendHistory(t, f, id, "TCP 的连接")
	f.work(t)
	requestTopics(t, f, id)
	f.work(t)
	page := readTopics(t, f, id)
	turn := historyPage(t, f, id, 0, 1).Items[0]
	q := TopicScopeRequest{TopicIDs: []string{page.Items[0].ID}, IndexRevision: page.Revision, ThroughMessage: turn.MessageAnchors[len(turn.MessageAnchors)-1].MessageID, MaxTokens: 1000}
	var result TopicScopeProjection
	read := func() error {
		return f.service.ReadTopicScope(context.Background(), f.actor, learningspace.DefaultID, id, q, func(p TopicScopeProjection) error { result = p; return nil })
	}
	if err := read(); err != nil || len(result.Topics) != 1 || result.Topics[0].ID != page.Items[0].ID {
		t.Fatal(result, err)
	}
	none := &knowledge.SourceManifest{Schema: 1, SpaceID: learningspace.DefaultID, Generation: 1, Entries: []knowledge.SourceManifestEntry{}}
	none.SHA256 = none.Hash()
	q.CurrentManifest = none
	if err := read(); !errors.Is(err, ErrInvalid) {
		t.Fatal("零来源借用旧摘要", err)
	}
	q.CurrentManifest = nil
	q.DirectionRevision = "00000000-0000-4000-8000-000000000001"
	if err := read(); !errors.Is(err, ErrInvalid) {
		t.Fatal("换方向借用旧摘要", err)
	}
	q.DirectionRevision = ""
	q.IndexRevision++
	if err := read(); !errors.Is(err, ErrConflict) {
		t.Fatal("错误索引版本被接受", err)
	}
}

func TestPostgreSQLManyTopicsRelevantProjectionAndFailure(t *testing.T) {
	f := topicFixture(t)
	id := newHistory(t, f, true, "")
	receipt := sendHistory(t, f, id, "TCP")
	f.work(t)
	requestTopics(t, f, id)
	f.work(t)
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = gates(ctx, tx, true); err != nil {
		t.Fatal(err)
	}
	var original []byte
	var originalID string
	if err = tx.QueryRow(ctx, `SELECT id::text,ciphertext FROM learning_conversation_topics WHERE conversation_id=$1 LIMIT 1`, id).Scan(&originalID, &original); err != nil {
		t.Fatal(err)
	}
	var topic topicBody
	if err = openHistory(f.service.aead, historyAAD(id, 1, "topic:"+originalID), original, &topic); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 140; i++ {
		topic.ID = uuid.NewString()
		topic.Title = "无关话题"
		topic.Points[0].Text = "其他事项"
		if i == 139 {
			topic.Title = "TARGET_TCP"
			topic.Points[0].Text = "TARGET_TCP 的早期要点"
		}
		raw, e := sealHistory(f.service.aead, historyAAD(id, 1, "topic:"+topic.ID), topic)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, `INSERT INTO learning_conversation_topics(id,conversation_id,privacy_generation,scope_fingerprint,revision,ciphertext) VALUES($1,$2,1,$3,1,$4)`, topic.ID, id, topicScope(Body{}), raw); e != nil {
			t.Fatal(e)
		}
	}
	raw, err := sealHistory(f.service.aead, historyAAD(id, 1, receipt.RunID), Body{TopicProvenance: true, Messages: []modelclient.Message{{Role: "user", Content: strings.Repeat("很长的原文", 5000)}}, Output: "旧回答"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_tutor_turns SET ciphertext=$2 WHERE run_id=$1`, receipt.RunID, raw); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	h := executionHost{service: f.service, owned: row{Meta: Meta{ConversationID: id, SpaceID: learningspace.DefaultID, Generation: 1, Saved: true}, device: f.actor.Device.ID, token: f.actor.TokenID}, body: Body{Context: &ConversationContext{ThroughTurn: 1}, Messages: []modelclient.Message{{Role: "user", Content: "TARGET_TCP"}}}, limits: settings.Limits{ContextTokens: 8192, OutputTokens: 256}}
	assemble := func() string {
		t.Helper()
		plan, e := h.assembleContext(ctx, modelclient.Request{MaxTokens: 256, Messages: []modelclient.Message{{Role: "system", Content: "系统"}}}, nil, h.body.Messages)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(plan.Request)
		return string(raw)
	}
	text := assemble()
	if !strings.Contains(text, "早期要点") || strings.Count(text, "无关话题") >= 32 {
		t.Fatal("相关投影没有限额或没有匹配", text)
	}
	none := &knowledge.SourceManifest{Schema: 1, SpaceID: learningspace.DefaultID, Generation: 1, Entries: []knowledge.SourceManifestEntry{}}
	none.SHA256 = none.Hash()
	h.body.SourceManifest = none
	if strings.Contains(assemble(), "早期要点") {
		t.Fatal("零来源泄漏旧摘要")
	}
	h.body.SourceManifest = nil
	if _, err = f.pool.Exec(ctx, `UPDATE learning_topic_index_heads SET status='failed' WHERE conversation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(assemble(), "早期要点") || !strings.Contains(h.body.ContextNotice, "较早内容未纳入") {
		t.Fatal("摘要失败未降级")
	}
}

func TestPostgreSQLTopicProjectionDoesNotCrossSelectedSources(t *testing.T) {
	f := topicFixture(t)
	a, b := selectedDocument(t, f, "来源甲"), selectedDocument(t, f, "来源乙")
	id := newHistory(t, f, true, "")
	request := historyRequest(t, f, id, "TCP 的来源内问题")
	request.SourceSelection = &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{a}}
	receipt, err := f.service.Create(context.Background(), f.actor, learningspace.DefaultID, runtimeGoal(""), request)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	manifestA := f.snapshot(t, receipt.RunID).SourceManifest
	_, err = f.service.RequestTopicIndex(context.Background(), f.actor, learningspace.DefaultID, id, TopicIndexRequest{OperationID: uuid.NewString(), Generation: 1, SourceSelection: request.SourceSelection})
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	h := executionHost{service: f.service, owned: row{Meta: Meta{ConversationID: id, SpaceID: learningspace.DefaultID, Generation: 1, Saved: true}, device: f.actor.Device.ID, token: f.actor.TokenID}, body: Body{Context: &ConversationContext{ThroughTurn: 1}, SourceManifest: manifestA, Messages: []modelclient.Message{{Role: "user", Content: "TCP"}}}}
	messages, _, err := h.contextTopics(context.Background(), 2000)
	if err != nil || len(messages) == 0 {
		t.Fatal("同来源摘要不可读", err)
	}
	ctx, _ := learningspace.WithScope(context.Background(), learningspace.DefaultID)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	manifestB, err := knowledgepg.New(f.pool).FreezeSourceSelectionTx(ctx, tx, knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{b}})
	if err != nil {
		t.Fatal(err)
	}
	none, err := knowledgepg.New(f.pool).FreezeSourceSelectionTx(ctx, tx, knowledge.SourceSelection{Mode: "none", Items: []knowledge.SourceIdentity{}})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for _, manifest := range []*knowledge.SourceManifest{manifestB, none, nil} {
		h.body.SourceManifest = manifest
		messages, _, err = h.contextTopics(ctx, 2000)
		if err != nil || len(messages) != 0 {
			t.Fatal("来源改变泄漏旧话题", messages, err)
		}
	}
	h.body.SourceManifest = manifestA
	h.body.Study = &learningdirection.Revision{ID: uuid.NewString()}
	messages, _, err = h.contextTopics(ctx, 2000)
	if err != nil || len(messages) != 0 {
		t.Fatal("方向改变泄漏旧状态", messages, err)
	}
}
