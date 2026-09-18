package postgresstore

import (
	"context"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/jackc/pgx/v5"
)

func (s *Store) persistReviewSession(ctx context.Context, tx pgx.Tx, request learning.CommitRequest) error {
	source, session := request.Batch.ReviewSource, request.Batch.Session
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "review-task:"+source.TaskID); err != nil {
		return err
	}
	carriers, err := s.activeReviewCarriers(ctx, tx, []string{source.TaskID})
	if err != nil {
		return err
	}
	for _, carrier := range carriers {
		if carrier.evidence == source.EvidenceID {
			return &learning.Error{Code: learning.CodeInvalidTransition, Reason: "review_carrier_exists"}
		}
	}
	var goalID string
	var contextID *string
	if err := tx.QueryRow(ctx, `SELECT g.goal_id,a.knowledge_context_revision_id FROM learning_evidence e JOIN learning_activities a ON a.id=e.activity_id JOIN learning_goal_revisions g ON g.id=e.goal_revision_id WHERE e.id=$1 AND e.attempt_id=$2 AND e.goal_revision_id=$3 AND e.route_revision_id=$4 AND e.node_revision_id=$5 AND NOT EXISTS(SELECT 1 FROM learning_evidence_invalidations i WHERE i.evidence_id=e.id)`, source.EvidenceID, source.AttemptID, session.Context.GoalRevisionID, session.Context.RouteRevisionID, session.Context.FocusNodeRevisionID).Scan(&goalID, &contextID); err != nil {
		if err == pgx.ErrNoRows {
			return &learning.Error{Code: learning.CodeInvalidTransition, Reason: "review_evidence_unavailable"}
		}
		return err
	}
	values, err := loadValidEvidenceWith(ctx, tx, session.Context.FocusNodeRevisionID)
	if err != nil {
		return err
	}
	var evidence []learning.AcceptedEvidence
	for _, e := range values {
		var owner string
		if err = tx.QueryRow(ctx, `SELECT goal_id FROM learning_goal_revisions WHERE id=$1`, e.GoalRevisionID).Scan(&owner); err != nil {
			return err
		}
		if owner == goalID {
			evidence = append(evidence, e)
		}
	}
	review := learning.ReduceNode(session.Context.FocusNodeRevisionID, evidence, nil, nil).Review
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if review == nil || review.DueAt.After(now) || len(evidence) == 0 || evidence[len(evidence)-1].ID != source.EvidenceID {
		return &learning.Error{Code: learning.CodeInvalidTransition, Reason: "review_schedule_changed"}
	}
	if err = s.ChangeSessionContextTx(ctx, tx, session.ID, session.Context.GoalRevisionID, session.Context.KnowledgeRevisionID, contextID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_review_sessions(session_id,task_id,source_evidence_id,source_attempt_id,created_at) VALUES($1,$2,$3,$4,$5)`, session.ID, source.TaskID, source.EvidenceID, source.AttemptID, request.ReceivedAt)
	return err
}

// 只查询本页任务的承载关系，原来源字段始终保持不变。
func (s *Store) reviewCarriers(ctx context.Context, tx pgx.Tx, reviews []*learning.ReviewSchedule) error {
	if len(reviews) == 0 {
		return nil
	}
	ids := make([]string, 0, len(reviews))
	for _, r := range reviews {
		ids = append(ids, r.TaskID)
	}
	carriers, err := s.activeReviewCarriers(ctx, tx, ids)
	if err != nil {
		return err
	}
	for _, carrier := range carriers {
		for _, r := range reviews {
			if r.TaskID == carrier.task && r.EvidenceID == carrier.evidence {
				r.CarrierSessionID = carrier.session
			}
		}
	}
	return nil
}

type reviewCarrier struct {
	task, evidence, session, goal, route, node string
}

func (s *Store) activeReviewCarriers(ctx context.Context, tx pgx.Tx, tasks []string) ([]reviewCarrier, error) {
	rows, err := tx.Query(ctx, `SELECT r.task_id,r.source_evidence_id,r.session_id,e.goal_revision_id,e.route_revision_id,e.node_revision_id FROM learning_review_sessions r JOIN learning_evidence e ON e.id=r.source_evidence_id WHERE r.task_id=ANY($1::uuid[]) ORDER BY r.created_at,r.session_id`, tasks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []reviewCarrier
	for rows.Next() {
		var candidate reviewCarrier
		if err = rows.Scan(&candidate.task, &candidate.evidence, &candidate.session, &candidate.goal, &candidate.route, &candidate.node); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	var active []reviewCarrier
	for _, candidate := range candidates {
		// 会话事实与隐私屏障由 tutoring owner 读取，不跨表绕过其契约。
		session, err := s.tutoring.LoadSessionWith(ctx, tx, candidate.session)
		if err != nil {
			return nil, mapTutoringLoadError(err)
		}
		if session.State != tutoring.StateCompleted && session.Context.GoalRevisionID == candidate.goal && session.Context.RouteRevisionID == candidate.route && session.Context.FocusNodeRevisionID == candidate.node {
			active = append(active, candidate)
		}
	}
	return active, nil
}
