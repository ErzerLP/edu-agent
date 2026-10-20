package modelusage

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/google/uuid"
	"time"
)

type SearchCall struct {
	ID            string     `json:"id"`
	Stage         string     `json:"stage"`
	AttemptNo     int64      `json:"attempt_no"`
	State         string     `json:"state"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	Provider      string     `json:"provider"`
	Configuration string     `json:"configuration"`
}

func (s *Store) SearchCalls(ctx context.Context, actor identity.Credential, q Query, task string) (Page[SearchCall], error) {
	p := Page[SearchCall]{Items: []SearchCall{}}
	if err := q.Validate(); err != nil {
		return p, err
	}
	if _, err := uuid.Parse(task); err != nil {
		return p, ErrInvalid
	}
	tx, g, err := s.read(ctx, actor)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(context.Background())
	p.Generation = g
	var exists bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_model_tasks WHERE id=$1 AND privacy_generation=$2 AND ($3='' OR space_id::text=$3))`, task, g, q.SpaceID).Scan(&exists)
	if err != nil {
		return p, err
	}
	if !exists {
		return p, ErrNotFound
	}
	bind := binding(actor, g, q, task+":search")
	after, err := readPosition(q, bind)
	if err != nil {
		return p, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id::text,c.stage,c.attempt_no,c.dispatch_state,c.started_at,c.finished_at,c.provider,c.configuration`+selected+` AND c.is_search AND c.task_id=$9 AND (c.started_at,c.id)<($10,$11::uuid) ORDER BY c.started_at DESC,c.id DESC LIMIT $12`, append(args(g, q), task, after.At, after.ID, q.Limit+1)...)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var c SearchCall
		if err = rows.Scan(&c.ID, &c.Stage, &c.AttemptNo, &c.State, &c.StartedAt, &c.FinishedAt, &c.Provider, &c.Configuration); err != nil {
			return p, err
		}
		p.Items = append(p.Items, c)
	}
	if err = rows.Err(); err != nil {
		return p, err
	}
	if len(p.Items) > q.Limit {
		p.Items = p.Items[:q.Limit]
		last := p.Items[len(p.Items)-1]
		p.NextCursor = nextPosition(last.StartedAt, last.ID, bind)
	}
	return p, nil
}
