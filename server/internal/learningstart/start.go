// Package learningstart 将有出处的研究成果接入原教学应用服务，不自行写教学事件。
package learningstart

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepostgres "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningpostgres "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Request struct {
	NewSession   bool `json:"new_session"`
	ModelConsent bool `json:"model_consent"`
}

// Prepared 只准备当前小活动；模型不能修改目标、掌握度或执行正式命令。
type Prepared struct {
	ConceptKey string              `json:"concept_key"`
	Name       string              `json:"name"`
	Prompt     string              `json:"prompt"`
	Criterion  string              `json:"criterion"`
	Citations  []research.Citation `json:"citations"`
}

func (p Prepared) Validate(sources []research.Source) error {
	for _, field := range []struct {
		value string
		limit int
	}{{p.ConceptKey, 200}, {p.Name, 200}, {p.Prompt, 2000}, {p.Criterion, 1000}} {
		if !utf8.ValidString(field.value) || strings.TrimSpace(field.value) == "" || len(field.value) > field.limit || strings.ContainsRune(field.value, 0) {
			return research.ErrPolicy
		}
	}
	return (research.Synthesis{Points: []research.Point{{Text: p.Name, Citations: p.Citations}}}).Validate(sources)
}

type Result struct {
	SessionID       string                             `json:"session_id"`
	ActivityID      string                             `json:"activity_id"`
	ArtifactID      string                             `json:"artifact_id"`
	ArtifactVersion int64                              `json:"artifact_version"`
	Context         knowledge.KnowledgeContextRevision `json:"knowledge_context"`
}

type State struct {
	Request  Request   `json:"request"`
	ModelID  string    `json:"model_id,omitempty"`
	Prepared *Prepared `json:"prepared,omitempty"`
	Result   *Result   `json:"result,omitempty"`
}

type Service struct {
	Learning  *learningpostgres.Store
	Knowledge *knowledgepostgres.Store
	Content   *learningcontent.Store
}

func (s *Service) Available() bool {
	return s != nil && s.Learning != nil && s.Knowledge != nil && s.Content.Available()
}

type treeTx struct {
	owner *knowledgepostgres.Store
	tx    pgx.Tx
}

func (t treeTx) Tree(ctx context.Context, id string) (knowledge.TreeResult, error) {
	return t.owner.TreeTx(ctx, t.tx, id)
}

type preparedModel struct {
	plan Prepared
	refs []learning.KnowledgeReference
}

func (m preparedModel) Generate(_ context.Context, r learning.ProposalRequest) (json.RawMessage, error) {
	if r.Type == learning.ProposalRoute {
		return json.Marshal(map[string]any{"route": []learning.RouteProposalStep{{NodeRevisionID: m.refs[0].NodeRevisionID, TeachingIntent: m.plan.Name, CompletionCondition: m.plan.Criterion}}})
	}
	if r.Type == learning.ProposalActivity {
		return json.Marshal(map[string]any{"activity": learning.ActivityProposal{Prompt: m.plan.Prompt, Type: learning.ActivityOpen, Difficulty: 1, AllowedHelp: []learning.HelpLevel{learning.HelpNone, learning.HelpHint, learning.HelpScaffold, learning.HelpAnswerRevealed}, Rubric: learning.Rubric{Revision: "start-v1", Items: []learning.RubricItem{{ID: "reflection", Criterion: m.plan.Criterion}}}, References: m.refs}})
	}
	return nil, errors.New("开学仅允许当前路线和活动")
}

// PublishTx 不调用外部模型。调用方必须已锁定有效运行、目标、设备及隐私代次。
func (s *Service) PublishTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, generation int64, run, goal string, goalVersion int64, request research.Request, plan Prepared, modelID string) (Result, error) {
	var result Result
	if !s.Available() {
		return result, learningcontent.ErrUnavailable
	}
	store := s.Learning.WithinTx(tx)
	resolver := learningknowledge.New(treeTx{s.Knowledge, tx})
	svc, err := learning.NewService(store, store, resolver, learning.ServiceOptions{})
	if err != nil {
		return result, err
	}
	g, err := svc.GetGoal(ctx, goal)
	if err != nil {
		return result, err
	}
	if g.Revision != goalVersion {
		return result, &learning.Error{Code: learning.CodeVersionConflict}
	}
	sources, err := s.Knowledge.ResearchSourcesTx(ctx, tx, run, goal)
	if err != nil {
		return result, err
	}
	if err = plan.Validate(sources); err != nil {
		return result, err
	}
	result.Context, err = s.Knowledge.PublishContextTx(ctx, tx, run, goal, g.ID, actor.Device.ID, request, knowledge.ConceptRevision{SemanticKey: plan.ConceptKey, Name: plan.Name, Support: plan.Citations})
	if err != nil {
		return result, err
	}
	tree, err := s.Knowledge.TreeTx(ctx, tx, result.Context.ScopeSnapshotID)
	if err != nil {
		return result, err
	}
	cited := map[string]bool{}
	for _, c := range plan.Citations {
		cited[c.SourceID] = true
	}
	refs := []learning.KnowledgeReference{}
	for _, doc := range tree.Revision.Documents {
		if !cited[doc.CollectionID] {
			continue
		}
		for _, node := range doc.Revision.Nodes {
			if node.SectionRange.End <= node.SectionRange.Start {
				continue
			}
			ref, e := resolver.Resolve(ctx, result.Context.ScopeSnapshotID, node.ID)
			if e != nil {
				return result, e
			}
			refs = append(refs, ref)
			break
		}
	}
	if len(refs) == 0 {
		return result, &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid}
	}
	svc, err = learning.NewService(store, store, resolver, learning.ServiceOptions{Model: preparedModel{plan, refs}, ModelID: modelID, PromptRevision: "start-learning-v1"})
	if err != nil {
		return result, err
	}
	result.SessionID = uuid.NewString()
	op := func(version int64) learning.OperationEnvelope {
		return learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: result.SessionID, ExpectedVersion: version, Payload: json.RawMessage(`{}`)}
	}
	if _, err = svc.CreateSession(ctx, actor.Device.ID, learning.SessionCommand{Operation: op(0), GoalRevisionID: g.ID}); err != nil {
		return result, err
	}
	if err = store.BindStartContextTx(ctx, tx, result.SessionID, g.ID, result.Context.ID, result.Context.ScopeSnapshotID); err != nil {
		return result, err
	}
	input, err := json.Marshal(map[string]any{"source": "authorized_research", "run_id": run, "prepared": plan})
	if err != nil {
		return result, err
	}
	for _, step := range []struct {
		kind   learning.ProposalType
		action tutoring.Action
	}{{learning.ProposalRoute, tutoring.ActionApplyRoute}, {learning.ProposalActivity, tutoring.ActionIssueActivity}} {
		a, e := store.LoadSessionAuthority(ctx, result.SessionID)
		if e != nil {
			return result, e
		}
		nodes := []string{}
		for _, ref := range refs {
			nodes = append(nodes, ref.NodeRevisionID)
		}
		p, e := svc.Propose(ctx, actor.Device.ID, learning.ProposalRequest{RequestID: uuid.NewString(), Type: step.kind, AggregateType: "session", AggregateID: result.SessionID, AggregateVersion: a.Session.AggregateVer, KnowledgeRevisionID: result.Context.ScopeSnapshotID, NodeRevisionIDs: nodes, Input: input})
		if e != nil {
			return result, e
		}
		if _, e = svc.ApplyAction(ctx, actor.Device.ID, result.SessionID, learning.ActionCommand{Operation: op(a.Session.AggregateVer), Action: step.action, ProposalID: p.ID}); e != nil {
			return result, e
		}
	}
	a, err := store.LoadSessionAuthority(ctx, result.SessionID)
	if err != nil {
		return result, err
	}
	result.ActivityID = *a.Session.Context.ActivityID
	content, err := s.Content.EnsureTx(ctx, tx, actor, result.SessionID, result.ActivityID, generation)
	if err != nil {
		return result, err
	}
	result.ArtifactID = content.ArtifactID
	result.ArtifactVersion = content.Version
	return result, nil
}
