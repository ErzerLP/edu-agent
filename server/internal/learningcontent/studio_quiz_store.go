package learningcontent

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type StudioQuizStart struct {
	OperationID string `json:"operation_id"`
	Version     int64  `json:"version"`
	Generation  int64  `json:"privacy_generation"`
	GoalVersion int64  `json:"goal_version"`
	Question    int    `json:"question"`
}

func (c StudioQuizStart) Validate() error {
	if uuid.Validate(c.OperationID) != nil || c.Version < 1 || c.Generation < 1 || c.GoalVersion < 1 || c.Question < 1 || c.Question > 8 {
		return ErrInvalid
	}
	return nil
}

type StudioQuizMapping struct {
	Question          int    `json:"question"`
	SessionID         string `json:"session_id"`
	ActivityID        string `json:"activity_id"`
	ContentArtifactID string `json:"content_artifact_id"`
}

func (s *Store) StudioQuizOperationTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, operation string) (string, int64, error) {
	if uuid.Validate(operation) != nil {
		return "", 0, ErrInvalid
	}
	if _, err := gates(ctx, tx, actor, false); err != nil {
		return "", 0, err
	}
	var id string
	var version int64
	err := tx.QueryRow(ctx, `SELECT o.artifact_id,o.version FROM learning_studio_quiz_operations o JOIN learning_studio_artifacts a ON a.id=o.artifact_id WHERE o.device_id=$1 AND o.operation_id=$2 AND a.space_id=$3`, actor.Device.ID, operation, learningspace.Scope(ctx)).Scan(&id, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return id, version, err
}

// StudioQuizReadTx verifies the immutable quiz and its original sources before
// exposing any carrier. The caller owns the transaction and reads actual results.
func (s *Store) StudioQuizReadTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, id string, version int64) (StudioRevision, []StudioQuizMapping, error) {
	if uuid.Validate(id) != nil || version < 1 {
		return StudioRevision{}, nil, ErrInvalid
	}
	g, err := gates(ctx, tx, actor, false)
	if err != nil {
		return StudioRevision{}, nil, err
	}
	r, err := s.studioRead(ctx, tx, id, version, g, actor.TokenID)
	if err != nil {
		return r, nil, err
	}
	if r.Kind != "quiz" || r.GoalID == nil || r.Body.Quiz == nil || r.Body.Quiz.GoalVersion < 1 {
		return r, nil, ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT question,session_id,activity_id,content_artifact_id FROM learning_studio_quiz_questions WHERE artifact_id=$1 AND version=$2 ORDER BY question`, id, version)
	if err != nil {
		return r, nil, err
	}
	defer rows.Close()
	items := []StudioQuizMapping{}
	for rows.Next() {
		var m StudioQuizMapping
		if err = rows.Scan(&m.Question, &m.SessionID, &m.ActivityID, &m.ContentArtifactID); err != nil {
			return r, nil, err
		}
		items = append(items, m)
	}
	return r, items, rows.Err()
}

// StudioQuizTransaction supplies gates and atomicity to the application bridge.
func (s *Store) StudioQuizTransaction(ctx context.Context, actor identity.Credential, write bool, work func(pgx.Tx) error) error {
	tx, _, err := s.studioBegin(ctx, actor, write)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err = work(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) StartStudioQuizTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, id string, c StudioQuizStart, create func(StudioRevision, []StudioQuizMapping) (StudioQuizMapping, error)) error {
	if uuid.Validate(id) != nil || c.Validate() != nil {
		return ErrInvalid
	}
	g, err := gates(ctx, tx, actor, true)
	if err != nil {
		return err
	}
	if g != c.Generation {
		return &privacy.Error{Code: privacy.CodeContentRedacted}
	}
	// Lock operation first, then the quiz version, also serializing different devices.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "studio-quiz-operation:"+actor.Device.ID+":"+c.OperationID); err != nil {
		return err
	}
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM learning_studio_artifacts WHERE id=$1 AND space_id=$2 AND privacy_generation=$3 FOR UPDATE`, id, learningspace.Scope(ctx), g).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	r, mappings, err := s.StudioQuizReadTx(ctx, tx, actor, id, c.Version)
	if err != nil {
		return err
	}
	if c.Question > len(r.Body.Quiz.Questions) {
		return ErrInvalid
	}
	if c.GoalVersion != r.Body.Quiz.GoalVersion {
		return ErrConflict
	}
	hash := fingerprint(struct {
		ID      string
		Command StudioQuizStart
	}{id, c})
	var saved string
	err = tx.QueryRow(ctx, `SELECT encode(request_hash,'hex') FROM learning_studio_quiz_operations WHERE device_id=$1 AND operation_id=$2`, actor.Device.ID, c.OperationID).Scan(&saved)
	if err == nil {
		if saved != hash {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	exists := false
	for _, m := range mappings {
		exists = exists || m.Question == c.Question
	}
	if !exists {
		if c.Question != len(mappings)+1 {
			return ErrConflict
		}
		m, e := create(r, mappings)
		if e != nil {
			return e
		}
		if m.Question != c.Question {
			return ErrInvalid
		}
		if _, err = tx.Exec(ctx, `INSERT INTO learning_studio_quiz_questions(artifact_id,version,question,session_id,activity_id,content_artifact_id) VALUES($1,$2,$3,$4,$5,$6)`, id, c.Version, c.Question, m.SessionID, m.ActivityID, m.ContentArtifactID); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_studio_quiz_operations(device_id,operation_id,artifact_id,version,question,request_hash) VALUES($1,$2,$3,$4,$5,decode($6,'hex'))`, actor.Device.ID, c.OperationID, id, c.Version, c.Question, hash)
	return err
}
