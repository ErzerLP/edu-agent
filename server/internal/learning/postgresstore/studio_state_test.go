package postgresstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A first-page-only reader loses the older help request; an unbounded reader
// exposes all 27 concepts. Neither behavior satisfies the planning contract.
func TestPostgreSQLStudioLearningStateBoundsFormalRecordsAndKeepsPriority(t *testing.T) {
	pool, _, _ := newOfflineIngestFixture(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var helpNode string
	for i := 0; i < 26; i++ {
		node, revision, step := uuid.NewString(), uuid.NewString(), uuid.NewString()
		if _, err = tx.Exec(ctx, `INSERT INTO knowledge_nodes(id,document_id,created_at) VALUES($1,$2,now())`, node, learningDocumentID); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO knowledge_node_revisions(id,node_id,document_id,document_revision_id,parent_node_revision_id,sibling_index,heading_level,title,ancestor_titles,heading_start,heading_end,heading_start_line,heading_end_line,local_body_start,local_body_end,local_body_start_line,local_body_end_line,section_start,section_end,section_start_line,section_end_line,semantic_local_body_hash,indexer_version) VALUES($1,$2,$3,$4,$5,$6,1,$7,'[]',0,0,1,1,0,5,1,1,0,5,1,1,decode(repeat('44',32),'hex'),'i1')`, revision, node, learningDocumentID, learningDocumentRevisionID, learningNodeRevisionID, i, fmt.Sprintf("知识点 %d", i)); err != nil {
			t.Fatal(err)
		}
		studioCloneRecord(t, tx, "learning_route_steps", "50000000-0000-4000-8000-000000000004", map[string]any{"id": step, "ordinal": i + 1, "node_id": node, "node_revision_id": revision})
		activity := uuid.NewString()
		studioCloneRecord(t, tx, "learning_activities", "50000000-0000-4000-8000-000000000005", map[string]any{"id": activity, "route_step_id": step, "target_node_id": node, "target_node_revision_id": revision, "prompt": "题干全文不得外发"})
		help := "none"
		if i == 25 {
			help, helpNode = "scaffold", revision
		}
		studioCloneRecord(t, tx, "learning_attempts", "50000000-0000-4000-8000-000000000006", map[string]any{"id": uuid.NewString(), "activity_id": activity, "help_level": help, "received_at": time.Date(2026, 9, 30-i, 0, 0, 0, 0, time.UTC)})
	}
	state, err := learningdb.ReadStudioLearningStateTx(ctx, tx, "")
	if err != nil {
		t.Fatal(err)
	}
	if state.Coverage != "partial" || state.TotalConcepts != 27 || len(state.RecentConcepts) != 24 || len(state.MasteredEvidence) != 0 {
		t.Fatalf("不应把截取或未评估记录当完整掌握：%+v", state)
	}
	if len(state.NeedsReview) != 1 || state.NeedsReview[0].NodeRevisionID != helpNode || len(state.HelpUsed) != 1 || len(state.SuggestedFocus) == 0 || len(state.SuggestedFocus) > 4 {
		t.Fatalf("较早的需帮助记录应优先纳入：%+v", state)
	}
	if state.NeedsReview[0].Label != "知识点 25" {
		t.Fatalf("可读来源应由资料模块提供短标签：%+v", state.NeedsReview[0])
	}
	if len(state.PendingAssessments) == 0 || state.AsOfEventSeq == 0 {
		t.Fatalf("未评估状态和正式记录水位缺失：%+v", state)
	}
	raw, err := json.Marshal(state)
	if err != nil || strings.Contains(string(raw), "题干全文不得外发") || strings.Contains(string(raw), `"answer"`) || strings.Contains(string(raw), `"prompt"`) || strings.Contains(string(raw), `"messages"`) {
		t.Fatalf("学习状态混入正文：%s %v", raw, err)
	}
	other, _ := learningspace.WithScope(ctx, uuid.NewString())
	empty, err := learningdb.ReadStudioLearningStateTx(other, tx, "")
	if err != nil || empty.TotalConcepts != 0 || empty.Coverage != "empty" || len(empty.SuggestedFocus) != 0 {
		t.Fatalf("学习状态越过学习区：%+v %v", empty, err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM knowledge_collection_links WHERE space_id=$1`, learningspace.Scope(ctx)); err != nil {
		t.Fatal(err)
	}
	unlinked, err := learningdb.ReadStudioLearningStateTx(ctx, tx, "")
	if err != nil || unlinked.NeedsReview[0].Label != "知识点" || strings.Contains(strings.Join(unlinked.SuggestedFocus, ","), "知识点 25") {
		t.Fatalf("解除资料关联后不得外发目录标签：%+v %v", unlinked, err)
	}
}

func TestPostgreSQLStudioLearningStateMasteryRequiresCurrentValidEvidence(t *testing.T) {
	pool, _, _ := newOfflineIngestFixture(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	activity, attempt, assessment, decision, evidence := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	session := uuid.NewString()
	at := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	studioCloneRecord(t, tx, "tutoring_sessions", "50000000-0000-4000-8000-000000000001", map[string]any{"id": session, "activity_id": nil, "attempt_id": nil})
	studioCloneRecord(t, tx, "learning_activities", "50000000-0000-4000-8000-000000000005", map[string]any{"id": activity, "session_id": session})
	studioCloneRecord(t, tx, "learning_attempts", "50000000-0000-4000-8000-000000000006", map[string]any{"id": attempt, "session_id": session, "activity_id": activity, "received_at": at})
	studioCloneRecord(t, tx, "learning_assessments", "50000000-0000-4000-8000-000000000007", map[string]any{"id": assessment, "session_id": session, "attempt_id": attempt, "activity_id": activity, "created_at": at})
	studioCloneRecord(t, tx, "learning_assessment_decisions", "50000000-0000-4000-8000-000000000009", map[string]any{"id": decision, "assessment_id": assessment, "version": 1, "replaces_decision_id": nil, "created_at": at})
	if _, err = tx.Exec(ctx, `INSERT INTO learning_activity_evidence_claims(activity_id,activity_revision,winning_attempt_id,claim_source,claimed_at) VALUES($1,1,$2,'online',$3)`, activity, attempt, at); err != nil {
		t.Fatal(err)
	}
	studioCloneRecord(t, tx, "learning_evidence", "50000000-0000-4000-8000-000000000010", map[string]any{"id": evidence, "session_id": session, "decision_id": decision, "assessment_id": assessment, "attempt_id": attempt, "activity_id": activity, "received_at": at, "accepted_event_seq": 1000, "misconception_candidates": []any{}, "rubric_outcomes": []any{}})
	state, err := learningdb.ReadStudioLearningStateTx(ctx, tx, "")
	if err != nil || len(state.MasteredEvidence) != 1 || state.MasteredEvidence[0].NodeRevisionID != learningNodeRevisionID {
		t.Fatalf("两个不同练习的有效证据应支持掌握：%+v %v", state, err)
	}
	old, err := learningdb.ReadStudioLearningStateTx(ctx, tx, uuid.NewString())
	if err != nil || len(old.MasteredEvidence) != 0 || len(old.RecentConcepts) == 0 || old.RecentConcepts[0].CurrentDirection {
		t.Fatalf("旧方向记录应保留为历史：%+v %v", old, err)
	}
	if _, err = tx.Exec(ctx, `SAVEPOINT studio_pending`); err != nil {
		t.Fatal(err)
	}
	pendingActivity := uuid.NewString()
	studioCloneRecord(t, tx, "learning_activities", "50000000-0000-4000-8000-000000000005", map[string]any{"id": pendingActivity})
	studioCloneRecord(t, tx, "learning_attempts", "50000000-0000-4000-8000-000000000006", map[string]any{"id": uuid.NewString(), "activity_id": pendingActivity, "received_at": at.Add(time.Hour)})
	pending, err := learningdb.ReadStudioLearningStateTx(ctx, tx, "")
	if err != nil || len(pending.MasteredEvidence) != 0 || len(pending.PendingAssessments) != 1 {
		t.Fatalf("尚未评估的作答不应作为当前掌握：%+v %v", pending, err)
	}
	if _, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT studio_pending`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_evidence_invalidations(id,evidence_id,reason,event_seq,created_at) VALUES($1,$2,'需重新核实',1001,now())`, uuid.NewString(), evidence); err != nil {
		t.Fatal(err)
	}
	invalid, err := learningdb.ReadStudioLearningStateTx(ctx, tx, "")
	if err != nil || len(invalid.MasteredEvidence) != 0 {
		t.Fatalf("失效证据不应支持掌握：%+v %v", invalid, err)
	}
}

func studioCloneRecord(t *testing.T, tx pgx.Tx, table, id string, changes map[string]any) {
	t.Helper()
	raw, err := json.Marshal(changes)
	if err != nil {
		t.Fatal(err)
	}
	// The table names are fixed test fixture literals, never external input.
	query := fmt.Sprintf(`INSERT INTO %s SELECT (jsonb_populate_record(NULL::%s,to_jsonb(source)||$2::jsonb)).* FROM %s source WHERE id=$1`, table, table, table)
	if _, err = tx.Exec(context.Background(), query, id, raw); err != nil {
		t.Fatalf("clone %s: %v", table, err)
	}
}
