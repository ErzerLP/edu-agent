package postgresstore

import (
	"context"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/jackc/pgx/v5"
)

// Read-only labels from the original formal route. No quiz progress is stored.
func studioQuizSessionNames(ctx context.Context, tx pgx.Tx, items []learning.SessionSummary) error {
	ids := []string{}
	for _, item := range items {
		ids = append(ids, item.SessionID)
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT q.session_id,step.teaching_intent FROM learning_studio_quiz_questions q JOIN learning_activities a ON a.id=q.activity_id JOIN learning_route_steps step ON step.id=a.route_step_id WHERE q.session_id=ANY($1::uuid[])`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			return err
		}
		for i := range items {
			if items[i].SessionID == id {
				items[i].Name = name
			}
		}
	}
	return rows.Err()
}
