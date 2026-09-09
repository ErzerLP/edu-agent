package postgresstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/jackc/pgx/v5"
)

const goalColumns = "id,goal_id,revision,goal_text,source,actor_device_id,created_at,previous_revision_id,space_id,management"

func scanGoal(row pgx.Row) (learning.GoalRevision, error) {
	var g learning.GoalRevision
	var raw []byte
	err := row.Scan(&g.ID, &g.GoalID, &g.Revision, &g.Text, &g.Source, &g.ActorDeviceID, &g.CreatedAt, &g.PreviousRevisionID, &g.SpaceID, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return g, err
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &g.Management); err != nil {
			return g, err
		}
	}
	// 旧 typed record 保持原 JSON 形状；默认归属由领域方法统一解释。
	if g.Management == nil && g.SpaceID == learningspace.DefaultID {
		g.SpaceID = ""
	}
	return g, nil
}

func (s *Store) GetGoal(ctx context.Context, id string) (learning.GoalRevision, error) {
	if !learningspace.ValidID(id) {
		return learning.GoalRevision{}, &learning.Error{Code: learning.CodeInvalidRequest}
	}
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (learning.GoalRevision, error) {
		return scanGoal(db.QueryRow(ctx, "SELECT "+goalColumns+" FROM learning_goal_revisions WHERE goal_id=$1 AND space_id=$2 ORDER BY revision DESC LIMIT 1", id, learningspace.Scope(ctx)))
	})
}

type goalCursor struct {
	Space, Search, Status, Goal, After string
	Revision                           int64
}

func (s *Store) ListGoals(ctx context.Context, q learning.GoalQuery) (learning.GoalPage, error) {
	return s.queryGoals(ctx, "", q)
}
func (s *Store) GoalHistory(ctx context.Context, id string, q learning.GoalQuery) (learning.GoalPage, error) {
	if !learningspace.ValidID(id) {
		return learning.GoalPage{}, &learning.Error{Code: learning.CodeInvalidRequest}
	}
	return s.queryGoals(ctx, id, q)
}
func (s *Store) queryGoals(ctx context.Context, id string, q learning.GoalQuery) (learning.GoalPage, error) {
	invalid := func() (learning.GoalPage, error) {
		return learning.GoalPage{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_goal_query"}
	}
	if q.Limit < 1 || q.Limit > 100 || !utf8.ValidString(q.Search) || utf8.RuneCountInString(q.Search) > 120 || (q.Status != "" && !learning.ValidGoalStatus(q.Status)) || (id != "" && (q.Search != "" || q.Status != "")) {
		return invalid()
	}
	c := goalCursor{Space: learningspace.Scope(ctx), Search: q.Search, Status: q.Status, Goal: id, After: "00000000-0000-0000-0000-000000000000"}
	if q.Cursor != "" {
		if len(q.Cursor) > 2048 {
			return invalid()
		}
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		var old goalCursor
		if err != nil || json.Unmarshal(raw, &old) != nil || old.Space != c.Space || old.Search != c.Search || old.Status != c.Status || old.Goal != id || !learningspace.ValidID(old.After) || old.Revision < 0 {
			return invalid()
		}
		c = old
	}
	return withLearningLoaderRead(ctx, s, func(db learningLoaderDB) (learning.GoalPage, error) {
		page := learning.GoalPage{Items: []learning.GoalRevision{}}
		var rows pgx.Rows
		var err error
		if id != "" {
			var exists bool
			if err = db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM learning_goal_revisions WHERE goal_id=$1 AND space_id=$2)", id, c.Space).Scan(&exists); err != nil {
				return page, err
			}
			if !exists {
				return page, &learning.Error{Code: learning.CodeNotFound}
			}
			rows, err = db.Query(ctx, "SELECT "+goalColumns+" FROM learning_goal_revisions WHERE goal_id=$1 AND space_id=$2 AND revision>$3 ORDER BY revision LIMIT $4", id, c.Space, c.Revision, q.Limit+1)
		} else {
			rows, err = db.Query(ctx, "SELECT "+goalColumns+` FROM (SELECT DISTINCT ON (goal_id) * FROM learning_goal_revisions WHERE space_id=$1 ORDER BY goal_id,revision DESC) g WHERE goal_id>$2 AND ($3='' OR COALESCE(management->>'status','active')=$3) AND strpos(lower(COALESCE(management->'details'->>'name',goal_text)||' '||goal_text),lower($4))>0 ORDER BY goal_id LIMIT $5`, c.Space, c.After, c.Status, c.Search, q.Limit+1)
		}
		if err != nil {
			return page, err
		}
		defer rows.Close()
		for rows.Next() {
			g, err := scanGoal(rows)
			if err != nil {
				return page, err
			}
			page.Items = append(page.Items, g)
		}
		if err = rows.Err(); err != nil {
			return page, err
		}
		if len(page.Items) > q.Limit {
			page.Items = page.Items[:q.Limit]
			last := page.Items[q.Limit-1]
			c.After = last.GoalID
			c.Revision = last.Revision
			raw, _ := json.Marshal(c)
			page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		}
		return page, nil
	})
}

func lockGoalSpace(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT privacy_lock_owner_gate('learning','write',NULL)`); err != nil {
		return err
	}
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM learning_spaces WHERE id=$1 FOR SHARE`, learningspace.Scope(ctx)).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return &learningspace.Error{Code: "learning_space_not_found"}
	}
	if err != nil {
		return err
	}
	if status != "active" {
		return &learningspace.Error{Code: "learning_space_archived"}
	}
	_, err = tx.Exec(ctx, `SELECT set_config('edu_agent.goal_space',$1,true)`, learningspace.Scope(ctx))
	return err
}

type goalScopeValidator interface {
	ValidateGoalScopeWith(context.Context, pgx.Tx, string) error
}

type teachingScopeWriter interface {
	EnsureTeachingScopeWith(context.Context, pgx.Tx, string, string) error
}

func (s *Store) validateGoalWrite(ctx context.Context, tx pgx.Tx, g *learning.GoalRevision) error {
	if g.LearningSpaceID() != learningspace.Scope(ctx) {
		return &learning.Error{Code: learning.CodeNotFound}
	}
	if g.Management != nil && g.Management.Details.ScopeSnapshotID != "" {
		owner, ok := s.knowledge.(goalScopeValidator)
		if !ok {
			return &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid}
		}
		if err := owner.ValidateGoalScopeWith(ctx, tx, g.Management.Details.ScopeSnapshotID); err != nil {
			return &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid, Cause: err}
		}
	}
	return lockGoalSpace(ctx, tx)
}

// 在目标锁内读取最新状态，使暂停与新教学创建具有确定的事务顺序。
func (s *Store) checkGoalStartWith(ctx context.Context, tx pgx.Tx, revisionID string) error {
	var goalID string
	if err := tx.QueryRow(ctx, `SELECT goal_id FROM learning_goal_revisions WHERE id=$1 AND space_id=$2`, revisionID, learningspace.Scope(ctx)).Scan(&goalID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &learning.Error{Code: learning.CodeNotFound}
		}
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learning-aggregate:goal:"+goalID); err != nil {
		return err
	}
	g, err := scanGoal(tx.QueryRow(ctx, "SELECT "+goalColumns+" FROM learning_goal_revisions WHERE goal_id=$1 AND space_id=$2 ORDER BY revision DESC LIMIT 1", goalID, learningspace.Scope(ctx)))
	if err != nil {
		return err
	}
	status := g.GoalManagement().Status
	if status != "draft" && status != "active" {
		return &learning.Error{Code: learning.CodeInvalidTransition, Reason: "goal_does_not_allow_new_learning"}
	}
	return nil
}
