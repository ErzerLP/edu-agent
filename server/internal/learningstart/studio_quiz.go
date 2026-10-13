package learningstart

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type StudioQuizQuestionState struct {
	learningcontent.StudioQuizMapping
	State     tutoring.State `json:"state"`
	Completed bool           `json:"completed"`
	AttemptID string         `json:"attempt_id,omitempty"`
}
type StudioQuizState struct {
	ArtifactID  string                    `json:"artifact_id"`
	Version     int64                     `json:"version"`
	GoalVersion int64                     `json:"goal_version"`
	Questions   []StudioQuizQuestionState `json:"questions"`
}

func (s *Service) StudioQuizOperation(ctx context.Context, actor identity.Credential, operation string) (StudioQuizState, error) {
	var result StudioQuizState
	if !s.Available() {
		return result, learningcontent.ErrUnavailable
	}
	err := s.Content.StudioQuizTransaction(ctx, actor, false, func(tx pgx.Tx) error {
		id, version, err := s.Content.StudioQuizOperationTx(ctx, tx, actor, operation)
		if err != nil {
			return err
		}
		result, err = s.studioQuizTx(ctx, tx, actor, id, version)
		return err
	})
	return result, err
}

func (s *Service) StudioQuiz(ctx context.Context, actor identity.Credential, id string, version int64) (StudioQuizState, error) {
	var result StudioQuizState
	if !s.Available() {
		return result, learningcontent.ErrUnavailable
	}
	err := s.Content.StudioQuizTransaction(ctx, actor, false, func(tx pgx.Tx) error {
		var err error
		result, err = s.studioQuizTx(ctx, tx, actor, id, version)
		return err
	})
	return result, err
}
func (s *Service) studioQuizTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, id string, version int64) (StudioQuizState, error) {
	result := StudioQuizState{ArtifactID: id, Version: version, Questions: []StudioQuizQuestionState{}}
	r, mappings, err := s.Content.StudioQuizReadTx(ctx, tx, actor, id, version)
	if err != nil {
		return result, err
	}
	store := s.Learning.WithinTx(tx)
	result.GoalVersion = r.Body.Quiz.GoalVersion
	for _, m := range mappings {
		a, e := store.Session(ctx, m.SessionID)
		if e != nil {
			return result, e
		}
		g, e := store.LoadGoalRevision(ctx, a.Session.Context.GoalRevisionID)
		if e != nil {
			return result, e
		}
		result.GoalVersion = g.Revision
		q := StudioQuizQuestionState{StudioQuizMapping: m, State: a.Session.State, Completed: a.Session.State == tutoring.StateCompleted && a.Session.CompletedRoute}
		if a.Session.Context.AttemptID != nil {
			q.AttemptID = *a.Session.Context.AttemptID
		} else if a.Session.State == tutoring.StateCompleted {
			feedback, e := store.ListFeedback(ctx, learning.FeedbackQuery{Status: "all", SessionID: m.SessionID, Page: learning.CursorPageRequest{Limit: 100}})
			if e != nil {
				return result, e
			}
			for _, f := range feedback.Items {
				if f.ActivityID == m.ActivityID {
					q.AttemptID = f.AttemptID
					break
				}
			}
		}
		result.Questions = append(result.Questions, q)
	}
	return result, nil
}
func (s *Service) StartStudioQuiz(ctx context.Context, actor identity.Credential, id string, c learningcontent.StudioQuizStart) (StudioQuizState, error) {
	var result StudioQuizState
	if !s.Available() {
		return result, learningcontent.ErrUnavailable
	}
	if c.Validate() != nil {
		return result, learningcontent.ErrInvalid
	}
	err := s.Content.StudioQuizTransaction(ctx, actor, true, func(tx pgx.Tx) error {
		if err := s.Content.StartStudioQuizTx(ctx, tx, actor, id, c, func(r learningcontent.StudioRevision, previous []learningcontent.StudioQuizMapping) (learningcontent.StudioQuizMapping, error) {
			return s.publishStudioQuestionTx(ctx, tx, actor, r, c, previous)
		}); err != nil {
			return err
		}
		var err error
		result, err = s.studioQuizTx(ctx, tx, actor, id, c.Version)
		return err
	})
	return result, err
}

// Creates one dedicated session using only owner ports and the normal proposal
// validation/command pipeline. It never writes learning or tutoring tables.
func (s *Service) publishStudioQuestionTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, r learningcontent.StudioRevision, c learningcontent.StudioQuizStart, previous []learningcontent.StudioQuizMapping) (learningcontent.StudioQuizMapping, error) {
	result := learningcontent.StudioQuizMapping{Question: c.Question}
	store := s.Learning.WithinTx(tx)
	resolver := learningknowledge.New(treeTx{s.Knowledge, tx})
	svc, err := learning.NewService(store, store, resolver, learning.ServiceOptions{})
	if err != nil {
		return result, err
	}
	var goal learning.GoalRevision
	if r.Body.Quiz.Direction == nil {
		goal, err = svc.GetGoal(ctx, *r.GoalID)
		if err != nil {
			return result, err
		}
	}
	if len(previous) > 0 {
		last, e := store.Session(ctx, previous[len(previous)-1].SessionID)
		if e != nil {
			return result, e
		}
		if last.Session.State != tutoring.StateCompleted || !last.Session.CompletedRoute {
			return result, &learning.Error{Code: learning.CodeInvalidTransition, Reason: "quiz_previous_question_incomplete"}
		}
		goal, err = store.LoadGoalRevision(ctx, last.Session.Context.GoalRevisionID)
		if err != nil {
			return result, err
		}
	}
	if r.Body.Quiz.Direction != nil {
		goal, err = store.LoadGoalRevision(ctx, r.Body.Quiz.Direction.GoalRevisionID)
	}
	if err != nil {
		return result, err
	}
	if goal.GoalID != *r.GoalID {
		return result, learningcontent.ErrConflict
	}
	if goal.Revision != c.GoalVersion {
		return result, &learning.Error{Code: learning.CodeVersionConflict}
	}
	question := SingleQuestion{OperationID: c.OperationID, GoalRevisionID: goal.ID, Generation: r.Generation, Label: fmt.Sprintf("%s · 第 %d 题", r.Body.Title, c.Question), Model: r.Body.Model, Manifest: *r.Body.SourceManifest, Question: r.Body.Quiz.Questions[c.Question-1], Citations: r.Body.Citations}
	if r.Body.Quiz.Direction != nil {
		if r.Body.Quiz.Assessment == nil {
			return result, learningcontent.ErrInvalid
		}
		e, err := s.PublishExerciseTx(ctx, tx, actor, ExerciseRequest{Question: question, Assessment: *r.Body.Quiz.Assessment})
		if err != nil {
			return result, err
		}
		a, err := store.LoadSessionAuthority(ctx, e.SessionID)
		if err != nil {
			return result, err
		}
		if _, err = svc.ApplyAction(ctx, actor.Device.ID, e.SessionID, learning.ActionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: e.SessionID, ExpectedVersion: a.Session.AggregateVer, Payload: json.RawMessage(`{}`)}, Action: tutoring.ActionPresentActivity}); err != nil {
			return result, err
		}
		result.SessionID, result.ActivityID, result.ContentArtifactID, result.ExerciseID = e.SessionID, e.ActivityID, e.ContentArtifactID, e.ID
		return result, nil
	}
	published, err := s.PublishQuestionTx(ctx, tx, actor, question)
	if err != nil {
		return result, err
	}
	result.SessionID = published.SessionID
	result.ActivityID = published.ActivityID
	result.ContentArtifactID = published.ArtifactID
	return result, nil
}
