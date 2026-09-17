package learningchange

import (
	"context"
	"encoding/json"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Service) references(ctx context.Context, tx pgx.Tx, c Change, snap Snapshot) ([]learning.KnowledgeReference, string, error) {
	scope := snap.Session.Context.KnowledgeRevisionID
	if c.Candidate.ContextID != "" {
		k, err := s.knowledge.ContextTx(ctx, tx, c.Candidate.ContextID)
		if err != nil {
			return nil, "", err
		}
		if k.Policy.GoalRevisionID != snap.Goal.ID {
			return nil, "", ErrConflict
		}
		scope = k.ScopeSnapshotID
	}
	refs := []learning.KnowledgeReference{}
	if scope == "" {
		return refs, scope, nil
	}
	resolver := learningknowledge.New(treeTx{s.knowledge, tx})
	for _, step := range c.Candidate.Steps {
		ref, err := resolver.Resolve(ctx, scope, step.NodeRevisionID)
		if err != nil {
			if learning.ErrorCode(err) == learning.CodeKnowledgeReferenceInvalid {
				return refs, scope, nil
			}
			return nil, scope, err
		}
		refs = append(refs, ref)
	}
	return refs, scope, nil
}

type prepared struct {
	steps []Step
	refs  []learning.KnowledgeReference
}

func (p prepared) Generate(_ context.Context, r learning.ProposalRequest) (json.RawMessage, error) {
	switch r.Type {
	case learning.ProposalRoute:
		route := []learning.RouteProposalStep{}
		for _, st := range p.steps {
			route = append(route, learning.RouteProposalStep{NodeRevisionID: st.NodeRevisionID, TeachingIntent: st.Name, CompletionCondition: st.Criterion})
		}
		return json.Marshal(map[string]any{"route": route})
	case learning.ProposalActivity:
		st := p.steps[0]
		return json.Marshal(map[string]any{"activity": learning.ActivityProposal{Prompt: st.Prompt, Type: learning.ActivityOpen, Difficulty: st.Difficulty, AllowedHelp: []learning.HelpLevel{learning.HelpNone, learning.HelpHint, learning.HelpScaffold, learning.HelpAnswerRevealed}, Rubric: learning.Rubric{Revision: "adaptive-v1", Items: []learning.RubricItem{{ID: "response", Criterion: st.Criterion}}}, References: []learning.KnowledgeReference{p.refs[0]}}})
	}
	return nil, ErrInvalid
}
func operation(session string, version int64) learning.OperationEnvelope {
	return learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: session, ExpectedVersion: version, Payload: json.RawMessage(`{}`)}
}

func (s *Service) apply(ctx context.Context, tx pgx.Tx, actor identity.Credential, c *Change, snap Snapshot, immediate bool) error {
	l := s.learning.WithinTx(tx)
	resolver := learningknowledge.New(treeTx{s.knowledge, tx})
	svc, err := learning.NewService(l, l, resolver, learning.ServiceOptions{})
	if err != nil {
		return err
	}
	switch c.Candidate.Kind {
	case "goal":
		op := operation(c.GoalID, snap.Goal.Revision)
		op.AggregateType = "goal"
		_, err = svc.CreateGoal(ctx, actor.Device.ID, learning.GoalCommand{Operation: op, GoalID: c.GoalID, Text: snap.Goal.Text, Source: "adaptive-confirmed", PreviousRevisionID: &snap.Goal.ID, Details: &c.Diff.AfterGoal})
		if err != nil {
			return err
		}
	case "explanation":
		if snap.Content == nil || snap.Base.ArtifactID != c.Base.ArtifactID || snap.Base.ArtifactVersion != c.Base.ArtifactVersion {
			return ErrConflict
		}
		_, err = s.content.CommitTx(ctx, tx, actor, c.Base.ArtifactID, learningcontent.Commit{ProtocolVersion: 1, OperationID: uuid.NewString(), ExpectedVersion: c.Base.ArtifactVersion, Status: "committed", Blocks: c.Diff.AfterContent, Interaction: snap.Content.Body.Interaction}, c.Generation)
		if err != nil {
			return err
		}
	case "route":
		refs, scope, e := s.references(ctx, tx, *c, snap)
		if e != nil {
			return e
		}
		if len(refs) != len(c.Candidate.Steps) || len(refs) == 0 {
			return ErrInvalid
		}
		if _, err = svc.PrepareAdaptiveBoundary(ctx, actor.Device.ID, operation(c.SessionID, snap.Base.SessionVersion), immediate, snap.Goal.ID, scope); err != nil {
			return err
		}
		a, e := l.LoadSessionAuthority(ctx, c.SessionID)
		if e != nil {
			return e
		}
		if a.Session.ActiveFrame != nil {
			c.FrameID = a.Session.ActiveFrame.ID
		}
		contextID := c.Base.ContextID
		if c.Candidate.ContextID != "" {
			contextID = c.Candidate.ContextID
		}
		if snap.Session.Context.GoalRevisionID != snap.Goal.ID {
			if contextID != "" {
				contextID, err = s.knowledge.RebindContextTx(ctx, tx, contextID, c.GoalID, snap.Goal.ID, c.ID)
				if err != nil {
					return err
				}
			}
		}
		var contextPointer *string
		if contextID != "" {
			contextPointer = &contextID
		}
		if err = l.ChangeSessionContextTx(ctx, tx, c.SessionID, snap.Goal.ID, scope, contextPointer); err != nil {
			return err
		}
		// 准备结果已固定；这里的模型接口只返回内存中的结构，不产生网络调用。
		svc, err = learning.NewService(l, l, resolver, learning.ServiceOptions{Model: prepared{c.Candidate.Steps, refs}, ModelID: "reviewed-learning-change", PromptRevision: "adaptive-v1"})
		if err != nil {
			return err
		}
		nodes := []string{}
		seen := map[string]bool{}
		for _, r := range refs {
			if !seen[r.NodeRevisionID] {
				nodes = append(nodes, r.NodeRevisionID)
				seen[r.NodeRevisionID] = true
			}
		}
		input, _ := json.Marshal(map[string]any{"change_id": c.ID, "revision": c.Revision, "hash": c.Hash})
		for _, st := range []struct {
			kind   learning.ProposalType
			action tutoring.Action
		}{{learning.ProposalRoute, tutoring.ActionApplyRoute}, {learning.ProposalActivity, tutoring.ActionIssueActivity}} {
			a, e = l.LoadSessionAuthority(ctx, c.SessionID)
			if e != nil {
				return e
			}
			p, e := svc.Propose(ctx, actor.Device.ID, learning.ProposalRequest{RequestID: uuid.NewString(), Type: st.kind, AggregateType: "session", AggregateID: c.SessionID, AggregateVersion: a.Session.AggregateVer, KnowledgeRevisionID: scope, NodeRevisionIDs: nodes, Input: input})
			if e != nil {
				return e
			}
			if _, err = svc.ApplyAction(ctx, actor.Device.ID, c.SessionID, learning.ActionCommand{Operation: operation(c.SessionID, a.Session.AggregateVer), Action: st.action, ProposalID: p.ID}); err != nil {
				return err
			}
			if st.kind == learning.ProposalRoute {
				var id *string
				if contextID != "" {
					id = &contextID
				}
				if err = l.ChangeSessionContextTx(ctx, tx, c.SessionID, snap.Goal.ID, scope, id); err != nil {
					return err
				}
			}
		}
		a, e = l.LoadSessionAuthority(ctx, c.SessionID)
		if e != nil {
			return e
		}
		if _, err = s.content.EnsureTx(ctx, tx, actor, c.SessionID, *a.Session.Context.ActivityID, c.Generation); err != nil {
			return err
		}
	default:
		return ErrInvalid
	}
	after, err := s.snapshot(ctx, tx, c.GoalID, c.SessionID, c.Generation)
	if err != nil {
		return err
	}
	c.Applied = &after.Base
	c.Status = "applied"
	c.Reason = "已生效；历史题目、答案和证据保持原版本。"
	return nil
}
func (s *Service) restore(ctx context.Context, tx pgx.Tx, actor identity.Credential, c *Change, snap Snapshot) error {
	l := s.learning.WithinTx(tx)
	svc, err := learning.NewService(l, l, learningknowledge.New(treeTx{s.knowledge, tx}), learning.ServiceOptions{})
	if err != nil {
		return err
	}
	if _, err = svc.ResumeAdaptiveFocus(ctx, actor.Device.ID, operation(c.SessionID, snap.Base.SessionVersion), c.FrameID); err != nil {
		return err
	}
	a, err := l.LoadSessionAuthority(ctx, c.SessionID)
	if err != nil {
		return err
	}
	var id *string
	if c.Base.ContextID != "" {
		id = &c.Base.ContextID
	}
	if err = l.ChangeSessionContextTx(ctx, tx, c.SessionID, a.Session.Context.GoalRevisionID, a.Session.Context.KnowledgeRevisionID, id); err != nil {
		return err
	}
	c.Restored = true
	c.Reason = "已返回原题，草稿可继续；新题和变更历史保留。"
	return nil
}
