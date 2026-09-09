package postgresstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/jackc/pgx/v5"
)

func replaceProgress(ctx context.Context, tx pgx.Tx, generation string, p learning.Projection, high int64) error {
	if _, err := tx.Exec(ctx, "DELETE FROM learning_projection_progress WHERE generation_id=$1", generation); err != nil {
		return err
	}
	revisions := map[string]learning.GoalRevision{}
	latest := map[string]learning.GoalRevision{}
	rows, err := tx.Query(ctx, "SELECT "+goalColumns+` FROM learning_goal_revisions g WHERE EXISTS (SELECT 1 FROM learning_events e WHERE e.aggregate_type='goal' AND e.aggregate_id=g.goal_id AND e.aggregate_version=g.revision AND e.event_type='GoalRevisionCreated' AND e.event_seq<=$1)`, high)
	if err != nil {
		return err
	}
	for rows.Next() {
		g, e := scanGoal(rows)
		if e != nil {
			rows.Close()
			return e
		}
		revisions[g.ID] = g
		if g.Revision > latest[g.GoalID].Revision {
			latest[g.GoalID] = g
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	activities := map[string]learning.Activity{}
	rows, err = tx.Query(ctx, `SELECT id,session_id FROM learning_activities UNION ALL SELECT id,parent_session_id FROM offline_activities`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a learning.Activity
		if err = rows.Scan(&a.ID, &a.SessionID); err != nil {
			rows.Close()
			return err
		}
		activities[a.ID] = a
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	pendingGoals := map[string]string{}
	rows, err = tx.Query(ctx, `SELECT p.payload->>'assessment_id',COALESCE(ac.goal_revision_id::text,e.goal_revision_id::text,''),COALESCE(e.parent_session_id::text,e.aggregate_id::text) FROM learning_events e JOIN learning_event_payloads p ON p.id=e.payload_id LEFT JOIN learning_assessments a ON a.id::text=p.payload->>'assessment_id' LEFT JOIN learning_activities ac ON ac.id=a.activity_id WHERE e.event_seq<=$1 AND e.event_type IN ('AssessmentMarkedProvisional','OfflineAssessmentQueued') AND p.redacted_at IS NULL`, high)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, g, session string
		if err = rows.Scan(&id, &g, &session); err != nil {
			rows.Close()
			return err
		}
		if g == "" {
			g = p.Sessions[session].Session.Context.GoalRevisionID
		}
		pendingGoals[id] = g
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	completed := map[string]map[string]bool{}
	rows, err = tx.Query(ctx, `SELECT e.event_type,p.payload FROM learning_events e JOIN learning_event_payloads p ON p.id=e.payload_id WHERE e.event_seq<=$1 AND e.event_type IN ('RouteAdvanced','LearningCompleted') AND p.redacted_at IS NULL ORDER BY e.event_seq`, high)
	if err != nil {
		return err
	}
	for rows.Next() {
		var kind string
		var raw []byte
		if err = rows.Scan(&kind, &raw); err != nil {
			rows.Close()
			return err
		}
		var s learning.SessionProjection
		if err = json.Unmarshal(raw, &s); err != nil {
			rows.Close()
			return err
		}
		for _, r := range p.Routes {
			if r.Route.ID != s.Session.Context.RouteRevisionID {
				continue
			}
			for i, step := range r.Route.Steps {
				if step.ID != s.Session.Context.RouteStepID {
					continue
				}
				id := ""
				if kind == "RouteAdvanced" && i > 0 {
					id = r.Route.Steps[i-1].ID
				}
				if kind == "LearningCompleted" && s.Session.CompletedRoute {
					id = step.ID
				}
				if id != "" {
					if completed[r.Route.ID] == nil {
						completed[r.Route.ID] = map[string]bool{}
					}
					completed[r.Route.ID][id] = true
				}
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, goal := range latest {
		if goal.Source == "privacy_erasure" {
			continue
		}
		item := learning.BuildGoalProgress(goal, revisions, p, activities, pendingGoals, completed)
		raw, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO learning_projection_progress(generation_id,goal_id,space_id,item) VALUES($1,$2,$3,$4)`, generation, goal.GoalID, goal.LearningSpaceID(), raw); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE learning_projection_generations SET progress_version=1 WHERE id=$1`, generation)
	return err
}

type progressCursor struct {
	Scope, Goal, Status, Order, Kind, Generation, Due string
	Lifecycle                                         string
	HighWater                                         int64
	Seq                                               int64
	Offset                                            int
}

func progressPosition(ctx context.Context, tx pgx.Tx, q learning.ProgressQuery, kind, due string, m learning.ProjectionMetadata) (progressCursor, error) {
	c := progressCursor{Scope: learningspace.Scope(ctx), Goal: q.GoalID, Status: q.Status, Order: q.Order, Kind: kind, Generation: m.GenerationID, Seq: m.AsOfEventSequence, Due: due}
	if q.Global {
		c.Scope = ""
	}
	if c.Status == "" {
		c.Status = "active"
	}
	if c.Order == "" {
		c.Order = "priority"
	}
	invalid := &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_progress_query"}
	if q.Limit < 1 || q.Limit > 200 || (q.GoalID != "" && !learningspace.ValidID(q.GoalID)) || (c.Status != "all" && !learning.ValidGoalStatus(c.Status)) || (c.Order != "priority" && c.Order != "recent") {
		return c, invalid
	}
	if err := tx.QueryRow(ctx, `SELECT md5(COALESCE(string_agg(id::text||':'||version::text,',' ORDER BY id),'')),(SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1) FROM learning_spaces`).Scan(&c.Lifecycle, &c.HighWater); err != nil {
		return c, err
	}
	if c.Goal != "" {
		var source string
		err := tx.QueryRow(ctx, `SELECT source FROM learning_goal_revisions WHERE goal_id=$1 AND ($2='' OR space_id::text=$2) ORDER BY revision DESC LIMIT 1`, c.Goal, c.Scope).Scan(&source)
		if err == pgx.ErrNoRows {
			return c, &learning.Error{Code: learning.CodeNotFound}
		}
		if err != nil {
			return c, err
		}
		if source == "privacy_erasure" {
			return c, &learning.Error{Code: learning.CodeContentRedacted}
		}
	}
	if q.Cursor != "" {
		var old progressCursor
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if len(q.Cursor) > 4096 || err != nil || json.Unmarshal(raw, &old) != nil || old.Offset < 0 {
			return c, invalid
		}
		offset := old.Offset
		old.Offset = 0
		if old != c {
			return c, &learning.Error{Code: learning.CodeStaleCursor}
		}
		c.Offset = offset
	}
	return c, nil
}

func progressCursorValue(c progressCursor, count int) string {
	c.Offset += count
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func progressReady(ctx context.Context, tx pgx.Tx, m learning.ProjectionMetadata) (time.Time, int64, error) {
	var version int
	var updated time.Time
	var high int64
	err := tx.QueryRow(ctx, `SELECT g.progress_version,COALESCE(g.completed_at,g.created_at),c.current_event_seq FROM learning_projection_generations g CROSS JOIN learning_event_clock c WHERE g.id=$1`, m.GenerationID).Scan(&version, &updated, &high)
	if err == nil && version != 1 {
		err = &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "progress_rebuild_required"}
	}
	return updated, high, err
}

// 所有过滤、计数和分页在同一个投影快照中执行，生命周期读取同事务的最新权威记录。
const progressCTE = `WITH scoped AS (
 SELECT p.goal_id,p.space_id,p.item,sp.status AS space_status,sp.name AS space_name,
 COALESCE(g.management->>'status','active') AS status,
 COALESCE(g.management->'details'->>'priority','normal') AS priority,
 COALESCE((g.management->'details'->>'deadline')::timestamptz,'infinity') AS deadline
 FROM learning_projection_progress p JOIN learning_spaces sp ON sp.id=p.space_id
 JOIN LATERAL (SELECT management FROM learning_goal_revisions WHERE goal_id=p.goal_id ORDER BY revision DESC LIMIT 1) g ON true
 WHERE p.generation_id=$1 AND ($2='' OR p.space_id::text=$2) AND ($3='' OR p.goal_id::text=$3)
 AND ($4='all' OR COALESCE(g.management->>'status','active')=$4)
 AND ($4<>'active' OR sp.status='active')
) `

func (s *Store) Progress(ctx context.Context, q learning.ProgressQuery) (learning.ProgressPage, error) {
	return withProjectionRead(ctx, s, func(tx pgx.Tx, m learning.ProjectionMetadata) (learning.ProgressPage, error) {
		result := learning.ProgressPage{Metadata: m, Items: []learning.GoalProgress{}}
		c, err := progressPosition(ctx, tx, q, "progress", "", m)
		if err != nil {
			return result, err
		}
		result.UpdatedAt, result.HighWater, err = progressReady(ctx, tx, m)
		if err != nil {
			return result, err
		}
		var now time.Time
		if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
			return result, err
		}
		args := []any{m.GenerationID, c.Scope, c.Goal, c.Status}
		if err = tx.QueryRow(ctx, progressCTE+`SELECT count(*) FROM scoped`, args...).Scan(&result.Total); err != nil {
			return result, err
		}
		order := `CASE priority WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END,deadline,goal_id`
		if c.Order == "recent" {
			order = `COALESCE((item->'sessions'->0->>'last_event_seq')::bigint,0) DESC,goal_id`
		}
		rows, err := tx.Query(ctx, progressCTE+`SELECT item,space_status,space_name,status FROM scoped ORDER BY `+order+` LIMIT $5 OFFSET $6`, append(args, q.Limit, c.Offset)...)
		if err != nil {
			return result, err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			var status, spaceName, goalStatus string
			if err = rows.Scan(&raw, &status, &spaceName, &goalStatus); err != nil {
				return result, err
			}
			var item learning.GoalProgress
			if err = json.Unmarshal(raw, &item); err != nil {
				return result, err
			}
			item.SpaceName = spaceName
			for i := range item.Sessions {
				item.Sessions[i].GoalStatus = goalStatus
				item.Sessions[i].Resumable = status == "active" && (goalStatus == "active" || goalStatus == "draft") && item.Sessions[i].State != "Completed"
			}
			for i := range item.Reviews {
				item.Reviews[i].SpaceName = spaceName
				item.Reviews[i].GoalName = item.Goal.GoalManagement().Details.Name
				setReviewAvailability(&item.Reviews[i], item.Sessions, status, goalStatus, now)
			}
			result.Items = append(result.Items, item)
		}
		if err = rows.Err(); err != nil {
			return result, err
		}
		if c.Offset+len(result.Items) < result.Total {
			result.NextCursor = progressCursorValue(c, len(result.Items))
		}
		return result, nil
	})
}

func (s *Store) scopedReviews(ctx context.Context, q learning.ReviewQuery) (learning.ReviewsPage, error) {
	return withProjectionRead(ctx, s, func(tx pgx.Tx, m learning.ProjectionMetadata) (learning.ReviewsPage, error) {
		result := learning.ReviewsPage{Metadata: m, Items: []learning.ReviewSchedule{}}
		var cutoff time.Time
		if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&cutoff); err != nil {
			return result, err
		}
		due := cutoff.UTC().Format(time.RFC3339Nano)
		if q.DueBefore == nil && q.Page.Cursor != "" {
			var previous progressCursor
			raw, err := base64.RawURLEncoding.DecodeString(q.Page.Cursor)
			if err != nil || json.Unmarshal(raw, &previous) != nil {
				return result, &learning.Error{Code: learning.CodeStaleCursor}
			}
			due = previous.Due
		}
		if q.DueBefore != nil {
			due = q.DueBefore.UTC().Format(time.RFC3339Nano)
		}
		var parseErr error
		result.DueBefore, parseErr = time.Parse(time.RFC3339Nano, due)
		if parseErr != nil {
			return result, &learning.Error{Code: learning.CodeStaleCursor}
		}
		c, err := progressPosition(ctx, tx, learning.ProgressQuery{Global: q.Global, GoalID: q.GoalID, Status: q.Status, Cursor: q.Page.Cursor, Limit: q.Page.Limit}, "reviews", due, m)
		if err != nil {
			return result, err
		}
		result.UpdatedAt, _, err = progressReady(ctx, tx, m)
		if err != nil {
			return result, err
		}
		cte := progressCTE + `, tasks AS (SELECT s.*,r.value AS review FROM scoped s CROSS JOIN LATERAL jsonb_array_elements(s.item->'reviews') r WHERE (r.value->>'due_at')::timestamptz<=$5::timestamptz) `
		args := []any{m.GenerationID, c.Scope, c.Goal, c.Status, due}
		if err = tx.QueryRow(ctx, cte+`SELECT count(*) FROM tasks`, args...).Scan(&result.Total); err != nil {
			return result, err
		}
		rows, err := tx.Query(ctx, cte+`SELECT review,space_status,status,item->'sessions',space_name,COALESCE(item->'goal'->'management'->'details'->>'name',item->'goal'->>'text') FROM tasks ORDER BY (review->>'due_at')::timestamptz,review->>'task_id' LIMIT $6 OFFSET $7`, append(args, q.Page.Limit, c.Offset)...)
		if err != nil {
			return result, err
		}
		defer rows.Close()
		for rows.Next() {
			var raw, sessionRaw []byte
			var spaceStatus, status, spaceName, goalName string
			if err = rows.Scan(&raw, &spaceStatus, &status, &sessionRaw, &spaceName, &goalName); err != nil {
				return result, err
			}
			var review learning.ReviewSchedule
			var sessions []learning.SessionSummary
			if err = json.Unmarshal(raw, &review); err != nil {
				return result, err
			}
			review.SpaceName = spaceName
			review.GoalName = goalName
			if err = json.Unmarshal(sessionRaw, &sessions); err != nil {
				return result, err
			}
			setReviewAvailability(&review, sessions, spaceStatus, status, cutoff)
			result.Items = append(result.Items, review)
		}
		if err = rows.Err(); err != nil {
			return result, err
		}
		if c.Offset+len(result.Items) < result.Total {
			result.NextCursor = progressCursorValue(c, len(result.Items))
		}
		return result, nil
	})
}

var _ learning.ProgressStore = (*Store)(nil)

// 目标详情与统一队列共用可开始条件，实际执行仍由原会话动作再次校验。
func setReviewAvailability(review *learning.ReviewSchedule, sessions []learning.SessionSummary, spaceStatus, goalStatus string, now time.Time) {
	review.Startable = false
	review.UnavailableReason = "original_session_not_at_review_node"
	for _, session := range sessions {
		if session.SessionID == review.SessionID && session.GoalRevisionID == review.GoalRevisionID && session.RouteRevisionID == review.RouteRevisionID && session.NodeRevisionID == review.NodeRevisionID && session.State == string(tutoring.StateRouteActive) {
			review.Startable = true
			review.UnavailableReason = ""
		}
	}
	if spaceStatus != "active" || goalStatus != "active" {
		review.Startable = false
		review.UnavailableReason = "goal_or_space_not_active"
	}
	if review.DueAt.After(now) {
		review.Startable = false
		review.UnavailableReason = "not_due"
	}
}

// EnsureProgressProjection 只在升级缺少读模型时使用既有重放租约；不写学习事实。
func (s *Store) EnsureProgressProjection(ctx context.Context) (int, error) {
	var ready bool
	if err := s.pool.QueryRow(ctx, `SELECT g.progress_version=1 FROM learning_projection_head h JOIN learning_projection_generations g ON g.id=h.active_generation_id WHERE h.singleton_id=1`).Scan(&ready); err != nil {
		return 0, err
	}
	if ready {
		return 0, nil
	}
	if _, err := s.Rebuild(ctx); err != nil {
		return 0, err
	}
	return 1, nil
}
