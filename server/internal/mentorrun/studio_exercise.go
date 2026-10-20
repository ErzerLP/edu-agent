package mentorrun

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/jackc/pgx/v5"
)

// Studio owns its association; a study conversation identity is never invented.
type StudioExerciseView struct {
	PracticeView
	StudioArtifactID string `json:"studio_artifact_id"`
	StudioVersion    int64  `json:"studio_version"`
	Question         int    `json:"question"`
}
type StudioHelpRequest struct {
	OperationID string `json:"operation_id"`
}

func (s *Service) withStudioExercise(ctx context.Context, actor identity.Credential, id string, write bool, work func(pgx.Tx, learningcontent.Exercise, learningcontent.StudioRevision, int) error) error {
	if !s.CanStart() || !s.studio.Available() || !validID(id) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	g, err := gates(ctx, tx, write)
	if err != nil {
		return err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, write); err != nil {
		return err
	}
	var artifact string
	var version int64
	var question int
	err = tx.QueryRow(ctx, `SELECT q.artifact_id,q.version,q.question FROM learning_studio_quiz_questions q JOIN learning_exercises e ON e.content_artifact_id=q.content_artifact_id JOIN learning_studio_artifacts a ON a.id=q.artifact_id WHERE e.id=$1 AND a.space_id=$2 AND a.privacy_generation=$3`, id, learningspace.Scope(ctx), g).Scan(&artifact, &version, &question)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	r, mappings, err := s.studio.StudioQuizReadTx(ctx, tx, actor, artifact, version)
	if err != nil {
		return err
	}
	if r.Body.Quiz.Direction == nil {
		return ErrNotFound
	}
	e, err := s.starter.Content.ExerciseTx(ctx, tx, actor, id)
	if err != nil {
		return err
	}
	matched := false
	for _, m := range mappings {
		if m.Question == question && m.ExerciseID == id && m.SessionID == e.SessionID && m.ActivityID == e.ActivityID && m.ContentArtifactID == e.ContentArtifactID {
			matched = true
		}
	}
	if !matched {
		return ErrConflict
	}
	if err = work(tx, e, r, question); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) ReadStudioExercise(ctx context.Context, actor identity.Credential, id string, send func(StudioExerciseView) error) error {
	return s.withStudioExercise(ctx, actor, id, false, func(tx pgx.Tx, e learningcontent.Exercise, r learningcontent.StudioRevision, question int) error {
		v, err := s.exerciseViewTx(ctx, tx, actor, e, false)
		if err != nil {
			return err
		}
		return send(StudioExerciseView{PracticeView: v, StudioArtifactID: r.ArtifactID, StudioVersion: r.Version, Question: question})
	})
}

func (s *Service) PublishStudioHelp(ctx context.Context, actor identity.Credential, id string, c StudioHelpRequest) (learningcontent.ExerciseHelp, error) {
	var result learningcontent.ExerciseHelp
	if !validID(c.OperationID) {
		return result, ErrInvalid
	}
	err := s.withStudioExercise(ctx, actor, id, true, func(tx pgx.Tx, _ learningcontent.Exercise, _ learningcontent.StudioRevision, _ int) error {
		old, err := s.starter.Content.HelpOperationTx(ctx, tx, actor, id, c.OperationID)
		if err == nil {
			result = old
			return nil
		}
		if !errors.Is(err, learningcontent.ErrNotFound) {
			return err
		}
		// The saved question's manifest is authoritative even after selection edits.
		result, err = s.starter.Content.PublishHelp(ctx, actor, id, learningcontent.ExerciseHelp{OperationID: c.OperationID, Level: learning.HelpHint, Text: "先找出题目中的关键概念，用自己的话解释，再用所选资料核对理由。"})
		return err
	})
	return result, err
}

func (s *Service) StudioHelpOperation(ctx context.Context, actor identity.Credential, id, operation string) (learningcontent.ExerciseHelp, error) {
	var result learningcontent.ExerciseHelp
	err := s.withStudioExercise(ctx, actor, id, false, func(tx pgx.Tx, _ learningcontent.Exercise, _ learningcontent.StudioRevision, _ int) error {
		var err error
		result, err = s.starter.Content.HelpOperationTx(ctx, tx, actor, id, operation)
		return err
	})
	return result, err
}

func (s *Service) ResumeStudioAssessment(ctx context.Context, actor identity.Credential, id string, c StudyAssessmentResume) (Receipt, error) {
	var result Receipt
	err := s.withStudioExercise(ctx, actor, id, true, func(tx pgx.Tx, e learningcontent.Exercise, _ learningcontent.StudioRevision, _ int) error {
		attempt, err := s.studyExerciseAttemptTx(ctx, tx, e)
		if err != nil {
			return err
		}
		if attempt == "" {
			return ErrConflict
		}
		result, err = s.ResumeAssessment(ctx, actor, attempt, AssessmentResume{OperationID: c.OperationID, ExpectedVersion: c.ExpectedVersion, Policy: learningcontent.AssessmentPolicy{Configuration: c.Configuration}})
		return err
	})
	return result, err
}
