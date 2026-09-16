package postgresstore

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/jackc/pgx/v5"
)

// 在调用方持有的隐私事务内提供正规内容，不暴露 learning 的私有表给正文 owner。
func (s *Store) LearningContentSource(ctx context.Context, tx pgx.Tx, sessionID, activityID string) (learningcontent.Source, error) {
	var source learningcontent.Source
	a, err := loadActivityForView(ctx, tx, activityID)
	if errors.Is(err, pgx.ErrNoRows) || learning.ErrorCode(err) == learning.CodeNotFound {
		return source, learningcontent.ErrNotFound
	}
	if err != nil {
		return source, err
	}
	if a.SessionID != sessionID {
		return source, learningcontent.ErrNotFound
	}
	goal, err := scanGoal(tx.QueryRow(ctx, "SELECT "+goalColumns+" FROM learning_goal_revisions WHERE id=$1 AND space_id=$2", a.GoalRevisionID, learningspace.Scope(ctx)))
	if errors.Is(err, pgx.ErrNoRows) || learning.ErrorCode(err) == learning.CodeNotFound {
		return source, learningcontent.ErrNotFound
	}
	if err != nil {
		return source, err
	}
	if goal.Source == "privacy_erasure" {
		return source, learningcontent.ErrNotFound
	}
	source = learningcontent.Source{SpaceID: goal.LearningSpaceID(), GoalID: goal.GoalID, Activity: a}
	if a.SourceProposalID != "" {
		p, err := loadProposalTx(ctx, tx, a.SourceProposalID)
		if err != nil {
			return source, err
		}
		source.ModelID = p.ModelID
		source.InputFingerprint = p.InputHash
	}
	return source, nil
}

// 操作核对只返回回执元数据，始终绑定原设备、区和会话，不回落 current。
func (s *Store) SessionOperation(ctx context.Context, device, session, operation string) (learning.SessionOperationReceipt, error) {
	var receipt learning.SessionOperationReceipt
	if err := s.ValidateSessionScope(ctx, session); err != nil {
		return receipt, err
	}
	err := s.pool.QueryRow(ctx, `SELECT terminal_status,COALESCE((result->>'aggregate_version')::bigint,0),COALESCE(result->>'code','') FROM learning_inbox WHERE device_id=$1 AND operation_id=$2 AND aggregate_type='session' AND aggregate_id=$3`, device, operation, session).Scan(&receipt.Status, &receipt.AggregateVersion, &receipt.Code)
	if errors.Is(err, pgx.ErrNoRows) {
		return receipt, &learning.Error{Code: learning.CodeNotFound}
	}
	receipt.OperationID = operation
	receipt.SessionID = session
	return receipt, err
}
