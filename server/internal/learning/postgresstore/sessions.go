package postgresstore

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/jackc/pgx/v5"
)

type sessionCursor struct{ Space, Goal, Status, After string }

func (s *Store) ValidateSessionScope(ctx context.Context, id string) error {
	_, err := s.LoadSessionAuthority(ctx, id)
	return err
}

func setTeachingWriteScope(ctx context.Context, tx pgx.Tx, space string, settlement bool) error {
	var status string
	if err := tx.QueryRow(ctx, "SELECT status FROM learning_spaces WHERE id=$1 FOR SHARE", space).Scan(&status); err != nil {
		return err
	}
	if !settlement && status != "active" {
		return &learningspace.Error{Code: "learning_space_archived"}
	}
	mode := "off"
	if settlement {
		mode = "on"
	}
	_, err := tx.Exec(ctx, `SELECT set_config('edu_agent.teaching_space',$1,true),set_config('edu_agent.teaching_settlement',$2,true)`, space, mode)
	return err
}

func setOfflineSettlementScope(ctx context.Context, tx pgx.Tx, submission string) error {
	var space string
	err := tx.QueryRow(ctx, `SELECT g.space_id FROM offline_submission_authorizations au JOIN offline_activities a ON a.id=au.offline_activity_id JOIN learning_goal_revisions g ON g.id=a.goal_revision_id WHERE au.submission_id=$1`, submission).Scan(&space)
	if err != nil {
		return err
	}
	return setTeachingWriteScope(ctx, tx, space, true)
}

func (s *Store) ListSessions(ctx context.Context, q learning.SessionQuery) (learning.SessionPage, error) {
	invalid := func() (learning.SessionPage, error) {
		return learning.SessionPage{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_session_query"}
	}
	if q.Limit < 1 || q.Limit > 100 || (q.GoalID != "" && !learningspace.ValidID(q.GoalID)) || (q.Status != "" && q.Status != "resumable" && q.Status != "completed") {
		return invalid()
	}
	c := sessionCursor{Space: learningspace.Scope(ctx), Goal: q.GoalID, Status: q.Status, After: "00000000-0000-0000-0000-000000000000"}
	if q.Cursor != "" {
		if len(q.Cursor) > 2048 {
			return invalid()
		}
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		var previous sessionCursor
		if err != nil || json.Unmarshal(raw, &previous) != nil || previous.Space != c.Space || previous.Goal != c.Goal || previous.Status != c.Status || !learningspace.ValidID(previous.After) {
			return invalid()
		}
		c = previous
	}
	return withProjectionRead(ctx, s, func(tx pgx.Tx, metadata learning.ProjectionMetadata) (learning.SessionPage, error) {
		page := learning.SessionPage{Items: []learning.SessionSummary{}}
		if c.Goal != "" {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_goal_revisions WHERE goal_id=$1 AND space_id=$2)`, c.Goal, c.Space).Scan(&exists); err != nil {
				return page, err
			}
			if !exists {
				return page, &learning.Error{Code: learning.CodeNotFound}
			}
		}
		rows, err := tx.Query(ctx, `SELECT p.session_id,p.item,p.updated_event_seq,g.goal_id,g.id,
 COALESCE(g.management->'details'->>'scope_snapshot_id',''),COALESCE(g.management->'details'->>'name',g.goal_text),
 COALESCE(latest.management->>'status','active'),sp.status,
 COALESCE('路线步骤 ' || step.ordinal::text || '：' || step.teaching_intent,'尚未生成路线')
 FROM learning_projection_sessions p
 JOIN learning_goal_revisions g ON g.id=(p.item->'session'->'focus'->>'goal_revision_id')::uuid
 JOIN learning_spaces sp ON sp.id=g.space_id
 LEFT JOIN learning_route_steps step ON step.id=NULLIF(p.item->'session'->'focus'->>'route_step_id','')::uuid
 JOIN LATERAL (SELECT management FROM learning_goal_revisions WHERE goal_id=g.goal_id ORDER BY revision DESC LIMIT 1) latest ON true
 WHERE p.generation_id=$1 AND g.space_id=$2 AND p.session_id>$3
 AND ($4='' OR g.goal_id::text=$4)
 AND ($5='' OR ($5='completed' AND p.item->'session'->>'state'='Completed')
 OR ($5='resumable' AND p.item->'session'->>'state'<>'Completed' AND sp.status='active' AND COALESCE(latest.management->>'status','active') IN ('draft','active')))
 ORDER BY p.session_id LIMIT $6`, metadata.GenerationID, c.Space, c.After, c.Goal, c.Status, q.Limit+1)
		if err != nil {
			return page, err
		}
		defer rows.Close()
		for rows.Next() {
			var item learning.SessionSummary
			var raw []byte
			var spaceStatus string
			if err := rows.Scan(&item.SessionID, &raw, &item.LastEventSequence, &item.GoalID, &item.GoalRevisionID, &item.ScopeSnapshotID, &item.Name, &item.GoalStatus, &spaceStatus, &item.Position); err != nil {
				return page, err
			}
			var projected learning.SessionProjection
			if err := json.Unmarshal(raw, &projected); err != nil {
				return page, err
			}
			item.LearningSpaceID = c.Space
			item.State = string(projected.Session.State)
			item.RouteStepID = projected.Session.Context.RouteStepID
			if projected.Session.Context.ActivityID != nil {
				item.ActivityID = *projected.Session.Context.ActivityID
			}
			item.Resumable = projected.Session.State != tutoring.StateCompleted && spaceStatus == "active" && (item.GoalStatus == "draft" || item.GoalStatus == "active")
			page.Items = append(page.Items, item)
		}
		if err := rows.Err(); err != nil {
			return page, err
		}
		if len(page.Items) > q.Limit {
			page.Items = page.Items[:q.Limit]
			c.After = page.Items[q.Limit-1].SessionID
			raw, _ := json.Marshal(c)
			page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		}
		return page, nil
	})
}
