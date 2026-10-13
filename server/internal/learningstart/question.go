package learningstart

import (
	"context"
	"encoding/json"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type SingleQuestion struct {
	OperationID    string
	GoalRevisionID string
	Generation     int64
	Label          string
	Model          string
	Manifest       knowledge.SourceManifest
	Question       learningcontent.StudioQuestion
	Citations      []learningcontent.StudioCitation
}

type ExerciseRequest struct {
	Question   SingleQuestion
	Assessment learningcontent.AssessmentPolicy
}

// PublishExerciseTx explicitly adds automatic assessment and help tracking.
// Direction quizzes and study use it; legacy Studio retains PublishQuestionTx.
func (s *Service) PublishExerciseTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, r ExerciseRequest) (learningcontent.Exercise, error) {
	hash, err := learning.HashJSON(r)
	if err != nil {
		return learningcontent.Exercise{}, err
	}
	return s.Content.PublishExerciseTx(ctx, tx, actor, r.Question.OperationID, r.Question.Generation, hash, learningcontent.ExerciseBody{Manifest: r.Question.Manifest, Assessment: r.Assessment}, func() (learningcontent.Revision, error) {
		result, err := s.PublishQuestionTx(ctx, tx, actor, r.Question)
		if err != nil {
			return learningcontent.Revision{}, err
		}
		return s.Content.ReadTx(ctx, tx, actor, result.ArtifactID, result.ArtifactVersion)
	})
}

// PublishQuestionTx uses an exact immutable goal revision and exact source
// manifest. The caller owns the transaction; it never calls an external model.
func (s *Service) PublishQuestionTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, q SingleQuestion) (Result, error) {
	result := Result{SessionID: uuid.NewString()}
	if !s.Available() {
		return result, learningcontent.ErrUnavailable
	}
	if uuid.Validate(q.OperationID) != nil || uuid.Validate(q.GoalRevisionID) != nil || q.Label == "" {
		return result, learningcontent.ErrInvalid
	}
	if err := s.Content.ValidateQuestionTx(ctx, tx, actor, q.Generation, q.Manifest, q.Question, q.Citations); err != nil {
		return result, err
	}
	store := s.Learning.WithinTx(tx)
	resolver := learningknowledge.New(treeTx{s.Knowledge, tx})
	goal, err := store.LoadGoalRevision(ctx, q.GoalRevisionID)
	if err != nil {
		return result, err
	}
	svc, err := learning.NewService(store, store, resolver, learning.ServiceOptions{})
	if err != nil {
		return result, err
	}
	op := func(v int64) learning.OperationEnvelope {
		return learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: result.SessionID, ExpectedVersion: v, Payload: json.RawMessage(`{}`)}
	}
	if _, err = svc.CreateSession(ctx, actor.Device.ID, learning.SessionCommand{Operation: op(0), GoalRevisionID: goal.ID}); err != nil {
		return result, err
	}
	selection := knowledge.ReferenceSelection{SessionID: result.SessionID, Entries: []knowledge.ReferenceEntry{}}
	for _, entry := range q.Manifest.Entries {
		selection.Entries = append(selection.Entries, knowledge.ReferenceEntry{ScopeEntry: knowledge.ScopeEntry{CollectionID: entry.CollectionID, RevisionID: entry.RevisionID, DocumentID: entry.DocumentID}, Role: "restrict"})
	}
	refs, err := s.Knowledge.PublishReferencesTx(ctx, tx, goal.GoalID, goal.ID, actor.Device.ID, q.OperationID, "", "", knowledge.ReferenceState{}, selection)
	if err != nil {
		return result, err
	}
	if err = store.BindStartContextTx(ctx, tx, result.SessionID, goal.ID, refs.ContextID, refs.ScopeSnapshotID); err != nil {
		return result, err
	}
	question := q.Question
	references := []learning.KnowledgeReference{}
	seen := map[string]bool{}
	for _, id := range question.Citations {
		for _, citation := range q.Citations {
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
	model := singleQuestionModel{q.Label, question, references}
	svc, err = learning.NewService(store, store, resolver, learning.ServiceOptions{Model: model, ModelID: q.Model, PromptRevision: "studio-quiz-v1"})
	if err != nil {
		return result, err
	}
	nodes := []string{}
	for _, ref := range references {
		nodes = append(nodes, ref.NodeRevisionID)
	}
	input, _ := json.Marshal(map[string]any{"single_question_operation": q.OperationID})
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
	content, err := s.Content.EnsureTx(ctx, tx, actor, result.SessionID, result.ActivityID, q.Generation)
	if err != nil {
		return result, err
	}
	result.ArtifactID = content.ArtifactID
	result.ArtifactVersion = content.Version
	return result, nil
}

type singleQuestionModel struct {
	label    string
	question learningcontent.StudioQuestion
	refs     []learning.KnowledgeReference
}

func (m singleQuestionModel) Generate(_ context.Context, r learning.ProposalRequest) (json.RawMessage, error) {
	switch r.Type {
	case learning.ProposalRoute:
		return json.Marshal(map[string]any{"route": []learning.RouteProposalStep{{NodeRevisionID: m.refs[0].NodeRevisionID, TeachingIntent: m.label, CompletionCondition: "完成本题作答并查看反馈"}}})
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
