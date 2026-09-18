package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	knowledgepostgres "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/jackc/pgx/v5"
)

type ListQuery struct {
	Kind, Status, Cursor string
	Limit                int
}

type Page struct {
	Items      []Snapshot `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// ReadList 直接读取原 owner 的元数据；不解密正文，也不建立第二份任务状态。
func (s *Service) ReadList(ctx context.Context, actor identity.Credential, space string, q ListQuery, send func(Page) error) error {
	if !validID(space) || q.Cursor != "" && !validID(q.Cursor) || q.Limit < 1 || q.Limit > 100 {
		return ErrInvalid
	}
	switch q.Kind {
	case "", "mentor", "research", "start_learning", "content_edit":
	default:
		return ErrInvalid
	}
	switch q.Status {
	case "", "queued", "running", "waiting_input", "waiting_approval", "paused_budget", "succeeded", "partial", "failed", "cancelling", "cancelled":
	default:
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, false)
	if err != nil {
		return err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, false); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT state FROM learning_mentor_runs
		WHERE device_id=$1 AND space_id=$2 AND privacy_generation=$3
		AND ($4='' OR COALESCE(state->>'kind','mentor')=$4) AND ($5='' OR state->>'status'=$5)
		AND ($6='' OR id>NULLIF($6,'')::uuid) ORDER BY id LIMIT $7`,
		actor.Device.ID, space, generation, q.Kind, q.Status, q.Cursor, q.Limit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	page := Page{Items: []Snapshot{}}
	for rows.Next() {
		var raw []byte
		var meta Meta
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		if json.Unmarshal(raw, &meta) != nil {
			return ErrStorage
		}
		page.Items = append(page.Items, Snapshot{Meta: meta})
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(page.Items) > q.Limit {
		page.Items = page.Items[:q.Limit]
		page.NextCursor = page.Items[q.Limit-1].RunID
	}
	return send(page)
}

// ReadSources 在运行副本到期或清除后仍能显示 knowledge 中获准保留的历史来源。
func (s *Service) ReadSources(ctx context.Context, actor identity.Credential, space, id string, send func([]research.Source) error) error {
	return s.read(ctx, actor, space, id, func(tx pgx.Tx, item row) error {
		if item.Kind != "research" && item.Kind != "start_learning" {
			return ErrNotFound
		}
		if item.BodyAvailable && time.Now().Before(item.ExpiresAt) {
			if err := s.decode(&item); err != nil {
				return err
			}
			if item.body.Research != nil {
				return send(item.body.Research.Sources)
			}
		}
		ctx, err := learningspace.WithScope(ctx, space)
		if err != nil {
			return err
		}
		sources, err := knowledgepostgres.New(s.pool).ResearchSourcesTx(ctx, tx, id, item.GoalID)
		if err != nil {
			return err
		}
		return send(sources)
	})
}

func (s *Service) read(ctx context.Context, actor identity.Credential, space, id string, fn func(pgx.Tx, row) error) error {
	if !validID(space) || !validID(id) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, false)
	if err != nil {
		return err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, false); err != nil {
		return err
	}
	item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1 AND device_id=$2 AND space_id=$3 AND privacy_generation=$4 FOR SHARE`, id, actor.Device.ID, space, generation))
	if err != nil {
		return err
	}
	if err = fn(tx, item); err != nil {
		return err
	}
	return nil
}

// ReadSnapshot 在短事务内发送有界响应，撤销/清除必须等待本次发送完成。
func (s *Service) ReadSnapshot(ctx context.Context, actor identity.Credential, space, id string, send func(Snapshot) error) error {
	return s.read(ctx, actor, space, id, func(tx pgx.Tx, item row) error {
		if time.Now().After(item.ExpiresAt) {
			item.BodyAvailable = false
			item.Reason = "expired"
		}
		if err := s.decode(&item); err != nil {
			item.BodyAvailable = false
			if !item.Saved {
				item.Reason = "temporary_unavailable"
			} else {
				return err
			}
		}
		if item.body.ContentEdit != nil {
			if err := s.contentReadable(ctx, tx, item); err != nil {
				return err
			}
		}
		return send(Snapshot{Meta: item.Meta, Output: item.body.Output, Interaction: item.body.Interaction, Research: item.body.Research, StartLearning: item.body.StartLearning, ContentEdit: item.body.ContentEdit})
	})
}

func (s *Service) Current(ctx context.Context, actor identity.Credential, space, goal string, kinds ...string) (string, error) {
	kind := "mentor"
	if len(kinds) > 0 {
		kind = kinds[0]
	}
	if !validID(space) || !validID(goal) {
		return "", ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, false)
	if err != nil {
		return "", err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, false); err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT current_run_id::text FROM learning_mentor_sessions WHERE device_id=$1 AND space_id=$2 AND goal_id=$3 AND privacy_generation=$4 AND kind=$5 AND NOT history AND current_run_id IS NOT NULL`, actor.Device.ID, space, goal, generation, kind).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s *Service) Operation(ctx context.Context, actor identity.Credential, space, id string) (Receipt, error) {
	if !validID(id) || !validID(space) {
		return Receipt{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, false)
	if err != nil {
		return Receipt{}, err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, false); err != nil {
		return Receipt{}, err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT o.receipt FROM learning_mentor_operations o JOIN learning_mentor_runs r ON r.id=o.run_id WHERE o.device_id=$1 AND o.operation_id=$2 AND r.space_id=$3 AND r.privacy_generation=$4`, actor.Device.ID, id, space, generation).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, ErrNotFound
	}
	var result Receipt
	if err != nil {
		return result, err
	}
	if json.Unmarshal(raw, &result) != nil {
		return result, ErrStorage
	}
	return result, nil
}

func (s *Service) Events(ctx context.Context, actor identity.Credential, space, id string, after int64, send func([]Event) error) error {
	if after < 0 {
		return ErrInvalid
	}
	return s.read(ctx, actor, space, id, func(tx pgx.Tx, item row) error {
		if after > item.Watermark || after < item.Watermark-EventWindow {
			return ErrResync
		}
		rows, err := tx.Query(ctx, `SELECT event FROM learning_mentor_events WHERE run_id=$1 AND seq>$2 ORDER BY seq`, id, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		events := []Event{}
		for rows.Next() {
			var raw []byte
			var event Event
			if err = rows.Scan(&raw); err != nil {
				return err
			}
			if json.Unmarshal(raw, &event) != nil {
				return ErrStorage
			}
			events = append(events, event)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if len(events) > 0 && events[0].Seq != after+1 {
			return ErrResync
		}
		return send(events)
	})
}
