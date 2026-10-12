package learningstart

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
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

type studioQuizModel struct {
	label    string
	question learningcontent.StudioQuestion
	refs     []learning.KnowledgeReference
}

func (m studioQuizModel) Generate(_ context.Context, r learning.ProposalRequest) (json.RawMessage, error) {
	switch r.Type {
	case learning.ProposalRoute:
		return json.Marshal(map[string]any{"route": []learning.RouteProposalStep{{NodeRevisionID: m.refs[0].NodeRevisionID, TeachingIntent: m.label, CompletionCondition: "完成本题正式作答并确认反馈"}}})
	case learning.ProposalActivity:
		rubric := learning.Rubric{Revision: "studio-quiz-v1", Items: []learning.RubricItem{}}
		for _, item := range m.question.Rubric {
			rubric.Items = append(rubric.Items, learning.RubricItem{ID: item.ID, Criterion: item.Criterion})
		}
		help := []learning.HelpLevel{}
		for _, h := range m.question.AllowedHelp {
			help = append(help, learning.HelpLevel(h))
		}
		return json.Marshal(map[string]any{"activity": learning.ActivityProposal{Prompt: m.question.Prompt, Type: learning.ActivityOpen, Difficulty: 1, AllowedHelp: help, Rubric: rubric, References: m.refs}})
	}
	return nil, learningcontent.ErrInvalid
}

// Creates one dedicated session using only owner ports and the normal proposal
// validation/command pipeline. It never writes learning or tutoring tables.
func (s *Service) publishStudioQuestionTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, r learningcontent.StudioRevision, c learningcontent.StudioQuizStart, previous []learningcontent.StudioQuizMapping) (learningcontent.StudioQuizMapping, error) {
	result := learningcontent.StudioQuizMapping{Question: c.Question, SessionID: uuid.NewString()}
	store := s.Learning.WithinTx(tx)
	resolver := learningknowledge.New(treeTx{s.Knowledge, tx})
	svc, err := learning.NewService(store, store, resolver, learning.ServiceOptions{})
	if err != nil {
		return result, err
	}
	goal, err := svc.GetGoal(ctx, *r.GoalID)
	if err != nil {
		return result, err
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
	if goal.Revision != c.GoalVersion {
		return result, &learning.Error{Code: learning.CodeVersionConflict}
	}
	op := func(v int64) learning.OperationEnvelope {
		return learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: result.SessionID, ExpectedVersion: v, Payload: json.RawMessage(`{}`)}
	}
	if _, err = svc.CreateSession(ctx, actor.Device.ID, learning.SessionCommand{Operation: op(0), GoalRevisionID: goal.ID}); err != nil {
		return result, err
	}
	selection := knowledge.ReferenceSelection{SessionID: result.SessionID, Entries: []knowledge.ReferenceEntry{}}
	for _, entry := range r.Body.SourceManifest.Entries {
		selection.Entries = append(selection.Entries, knowledge.ReferenceEntry{ScopeEntry: knowledge.ScopeEntry{CollectionID: entry.CollectionID, RevisionID: entry.RevisionID, DocumentID: entry.DocumentID}, Role: "restrict"})
	}
	refs, err := s.Knowledge.PublishReferencesTx(ctx, tx, *r.GoalID, goal.ID, actor.Device.ID, c.OperationID, "", "", knowledge.ReferenceState{}, selection)
	if err != nil {
		return result, err
	}
	if err = store.BindStartContextTx(ctx, tx, result.SessionID, goal.ID, refs.ContextID, refs.ScopeSnapshotID); err != nil {
		return result, err
	}
	question := r.Body.Quiz.Questions[c.Question-1]
	references := []learning.KnowledgeReference{}
	seen := map[string]bool{}
	for _, id := range question.Citations {
		for _, citation := range r.Body.Citations {
			if citation.ID == id && !seen[citation.Fragment.NodeRevisionID] {
				ref, e := resolver.Resolve(ctx, refs.ScopeSnapshotID, citation.Fragment.NodeRevisionID)
				if e != nil {
					return result, e
				}
				references = append(references, ref)
				seen[ref.NodeRevisionID] = true
			}
		}
	}
	if len(references) == 0 {
		return result, learningcontent.ErrStudioCitation
	}
	model := studioQuizModel{fmt.Sprintf("%s · 第 %d 题", r.Body.Title, c.Question), question, references}
	svc, err = learning.NewService(store, store, resolver, learning.ServiceOptions{Model: model, ModelID: r.Body.Model, PromptRevision: "studio-quiz-v1"})
	if err != nil {
		return result, err
	}
	nodes := []string{}
	for _, ref := range references {
		nodes = append(nodes, ref.NodeRevisionID)
	}
	input, _ := json.Marshal(map[string]any{"studio_artifact_id": r.ArtifactID, "version": r.Version, "question": c.Question})
	for _, step := range []struct {
		kind   learning.ProposalType
		action tutoring.Action
	}{{learning.ProposalRoute, tutoring.ActionApplyRoute}, {learning.ProposalActivity, tutoring.ActionIssueActivity}} {
		a, e := store.LoadSessionAuthority(ctx, result.SessionID)
		if e != nil {
			return result, e
		}
		proposal, e := svc.Propose(ctx, actor.Device.ID, learning.ProposalRequest{RequestID: uuid.NewString(), Type: step.kind, AggregateType: "session", AggregateID: result.SessionID, AggregateVersion: a.Session.AggregateVer, KnowledgeRevisionID: refs.ScopeSnapshotID, NodeRevisionIDs: nodes, Input: input})
		if e != nil {
			return result, e
		}
		if _, e = svc.ApplyAction(ctx, actor.Device.ID, result.SessionID, learning.ActionCommand{Operation: op(a.Session.AggregateVer), Action: step.action, ProposalID: proposal.ID}); e != nil {
			return result, e
		}
	}
	a, err := store.LoadSessionAuthority(ctx, result.SessionID)
	if err != nil {
		return result, err
	}
	result.ActivityID = *a.Session.Context.ActivityID
	content, err := s.Content.EnsureTx(ctx, tx, actor, result.SessionID, result.ActivityID, r.Generation)
	if err != nil {
		return result, err
	}
	result.ContentArtifactID = content.ArtifactID
	return result, nil
}
