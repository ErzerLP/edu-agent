package mentorrun

import (
	"context"
	"encoding/json"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/learningstart"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type StudyExerciseState struct {
	ID     string                           `json:"id,omitempty"`
	Policy learningcontent.AssessmentPolicy `json:"policy"`
}

const pendingStudyExerciseSQL = `SELECT EXISTS(SELECT 1 FROM learning_tutor_turns t JOIN learning_exercises e ON e.id=t.exercise_id WHERE t.conversation_id=$1 AND NOT t.exercise_deferred AND NOT EXISTS(SELECT 1 FROM learning_attempts a WHERE a.activity_id=e.activity_id))`

func (s *Service) pendingStudyExercise(ctx context.Context, conversation string) (bool, error) {
	var pending bool
	err := s.pool.QueryRow(ctx, pendingStudyExerciseSQL, conversation).Scan(&pending)
	return pending, err
}

func (s *Service) publishStudyExerciseTx(ctx context.Context, tx pgx.Tx, item *row, candidate learningcontent.StudioBody) error {
	if !s.CanStart() || item.body.Study == nil || item.body.StudyExercise == nil || candidate.Quiz == nil || len(candidate.Quiz.Questions) != 1 {
		return ErrInvalid
	}
	ctx, err := learningspace.WithScope(ctx, item.SpaceID)
	if err != nil {
		return err
	}
	// This lock also serializes defer/resume. No conversation row lock is taken
	// after the run lock, preserving the existing worker/delete lock order.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "study-exercise:"+item.ConversationID); err != nil {
		return err
	}
	var pending bool
	if err = tx.QueryRow(ctx, pendingStudyExerciseSQL, item.ConversationID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return ErrConflict
	}
	actor := identity.Credential{Device: identity.Device{ID: item.device}, TokenID: item.token}
	exercise, err := s.starter.PublishExerciseTx(ctx, tx, actor, learningstart.ExerciseRequest{
		Question:   learningstart.SingleQuestion{OperationID: item.RunID, GoalRevisionID: item.body.Study.GoalRevisionID, Generation: item.Generation, Label: "练习", Model: s.settings.View().EffectiveMentor.Model, Manifest: *item.body.SourceManifest, Question: candidate.Quiz.Questions[0], Citations: candidate.Citations},
		Assessment: item.body.StudyExercise.Policy,
	})
	if err != nil {
		return err
	}
	// The ordinary present action enters AwaitingResponse in the same transaction.
	store := s.starter.Learning.WithinTx(tx)
	a, err := store.LoadSessionAuthority(ctx, exercise.SessionID)
	if err != nil {
		return err
	}
	service, err := learning.NewService(store, store, learningknowledge.New(assessmentTreeTx{s.starter.Knowledge, tx}), learning.ServiceOptions{})
	if err != nil {
		return err
	}
	_, err = service.ApplyAction(ctx, item.device, exercise.SessionID, learning.ActionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: exercise.SessionID, ExpectedVersion: a.Session.AggregateVer, Payload: json.RawMessage(`{}`)}, Action: tutoring.ActionPresentActivity})
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_tutor_turns SET exercise_id=$2 WHERE run_id=$1`, item.RunID, exercise.ID); err != nil {
		return err
	}
	item.body.StudyExercise.ID = exercise.ID
	return nil
}
