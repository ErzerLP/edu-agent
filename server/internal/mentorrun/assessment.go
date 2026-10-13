package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/integrations/tutormodel"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Dispatch is transactional with the learning-owned task binding. No external
// request occurs here; a crash cannot orphan a task or create a second run.
func (s *Service) enqueueAssessment(ctx context.Context) error {
	if !s.CanStart() {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, true)
	if err != nil {
		return err
	}
	task, err := s.starter.Learning.PendingAssessmentTx(ctx, tx)
	if err != nil || task == nil {
		return err
	}
	ctx, err = learningspace.WithScope(ctx, task.SpaceID)
	if err != nil {
		return err
	}
	actor := identity.Credential{Device: identity.Device{ID: task.DeviceID}, TokenID: task.TokenID}
	exercise, accessErr := s.starter.Content.ExerciseTx(ctx, tx, actor, task.ExerciseID)
	if accessErr == nil {
		accessErr = actorGate(ctx, tx, task.DeviceID, task.TokenID, true)
	}
	if accessErr != nil {
		if assessmentUnavailable(accessErr) {
			if err = s.starter.Learning.RejectAssessmentTaskTx(ctx, tx, task.AttemptID); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		return accessErr
	}
	store := s.starter.Learning.WithinTx(tx)
	view, err := store.LoadSessionAuthority(ctx, task.SessionID)
	if err != nil {
		return err
	}
	goal, err := store.LoadGoalRevision(ctx, view.Session.Context.GoalRevisionID)
	if err != nil {
		return err
	}
	policy := exercise.Body.Assessment
	// Retain the originally authorized budget. Claim/dispatch checks its bound
	// configuration before spending, including changes to configured limits.
	if err = s.quota(ctx, tx, 512<<10); err != nil {
		return err
	}
	var session string
	err = tx.QueryRow(ctx, `INSERT INTO learning_mentor_sessions(id,device_id,space_id,goal_id,privacy_generation,kind) VALUES($1,$2,$3,$4,$5,'assessment') ON CONFLICT(device_id,space_id,goal_id,privacy_generation,kind) WHERE NOT history DO UPDATE SET id=learning_mentor_sessions.id RETURNING id::text`, uuid.NewString(), task.DeviceID, task.SpaceID, goal.GoalID, generation).Scan(&session)
	if err != nil {
		return err
	}
	run := uuid.NewString()
	task.RunID = run
	item := row{Meta: Meta{RunID: run, SessionID: session, SpaceID: task.SpaceID, GoalID: goal.GoalID, GoalVersion: goal.Revision, Generation: generation, Kind: "assessment", Status: "queued", Stage: "queued", Saved: true, BodyAvailable: true, RequestsLeft: policy.Requests, TokensLeft: policy.Tokens, Configuration: policy.Configuration, ExpiresAt: time.Now().Add(7 * 24 * time.Hour)}, device: task.DeviceID, token: task.TokenID, process: s.process}
	item.body = Body{Assessment: task, SourceManifest: &exercise.Body.Manifest}
	raw, _ := json.Marshal(item.Meta)
	if _, err = tx.Exec(ctx, `INSERT INTO learning_mentor_runs(id,session_id,device_id,token_id,space_id,goal_id,goal_version,privacy_generation,state,process_id,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, run, session, item.device, item.token, item.SpaceID, item.GoalID, item.GoalVersion, generation, raw, s.process, item.ExpiresAt); err != nil {
		return err
	}
	if err = s.starter.Learning.BindAssessmentRunTx(ctx, tx, task.AttemptID, run); err != nil {
		return err
	}
	if err = s.save(ctx, tx, &item, "accepted"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func assessmentUnavailable(err error) bool {
	return sourceUnavailable(err) || errors.Is(err, ErrInactive) || errors.Is(err, ErrNotFound) || errors.Is(err, learningcontent.ErrForbidden) || errors.Is(err, learningcontent.ErrNotFound)
}

// Reconcile before applying the generic worker's unknown-call rule. A ready
// proposal needs no model call; a live proposal lease must be allowed to settle.
func (s *Service) recoverAssessmentTx(ctx context.Context, tx pgx.Tx, item *row) (bool, error) {
	if err := actorGate(ctx, tx, item.device, item.token, true); err != nil {
		if !assessmentUnavailable(err) {
			return false, err
		}
		item.Status = "failed"
		item.Reason = "context_changed"
		return false, nil
	}
	if err := s.assessmentContextTx(ctx, tx, *item); err != nil {
		if !assessmentUnavailable(err) {
			return false, err
		}
		item.Status = "failed"
		item.Reason = "context_changed"
		return false, nil
	}
	recovery, err := s.starter.Learning.InspectAssessmentTx(ctx, tx, *item.body.Assessment)
	if err != nil {
		return false, err
	}
	switch {
	case recovery.OperationStatus == "succeeded":
		item.Status = "succeeded"
		item.Reason = ""
		item.callStarted = false
		item.ResultUnknown = false
	case recovery.OperationStatus != "" || recovery.ProposalStatus == "failed":
		item.Status = "failed"
		item.Reason = "assessment_rejected"
		item.ResultUnknown = false
	case recovery.ProposalStatus == "ready":
		item.callStarted = false
		item.ResultUnknown = false
	case recovery.ProposalStatus == "processing" && recovery.ProposalLeaseUntil != nil && recovery.ProposalLeaseUntil.After(time.Now()):
		item.leaseUntil = recovery.ProposalLeaseUntil
		return true, nil
	}
	return false, nil
}

func (s *Service) assessmentContextTx(ctx context.Context, tx pgx.Tx, item row) error {
	if item.body.Assessment == nil || !s.CanStart() {
		return ErrInactive
	}
	task := item.body.Assessment
	ctx, err := learningspace.WithScope(ctx, item.SpaceID)
	if err != nil {
		return err
	}
	current, err := s.starter.Learning.AssessmentTaskTx(ctx, tx, task.AttemptID)
	if err != nil {
		return err
	}
	if current != *task || task.DeviceID != item.device || task.TokenID != item.token || task.RunID != item.RunID {
		return ErrInactive
	}
	e, err := s.starter.Content.ExerciseTx(ctx, tx, identity.Credential{Device: identity.Device{ID: item.device}, TokenID: item.token}, task.ExerciseID)
	if err != nil {
		return err
	}
	if e.Generation != item.Generation || e.SessionID != task.SessionID || item.body.SourceManifest == nil || e.Body.Manifest.SHA256 != item.body.SourceManifest.SHA256 {
		return ErrInactive
	}
	// Settlement is tied to the original goal revision. A later direction or
	// goal revision cannot replace the submitted question's context.
	return nil
}

type assessmentChat struct {
	host   *executionHost
	client *llm.Client
}

func (c assessmentChat) Chat(ctx context.Context, request llm.ChatRequest) (llm.ChatResult, error) {
	h := c.host
	estimate := modelclient.Request{MaxTokens: h.limits.OutputTokens}
	for _, m := range request.Messages {
		estimate.Messages = append(estimate.Messages, modelclient.Message{Role: string(m.Role), Content: m.Content})
	}
	h.reserve = agentcore.NewTokenEstimator().EstimateRequest(estimate) + len(request.Schema) + h.limits.OutputTokens + 256
	if h.reserve > h.limits.ContextTokens {
		return llm.ChatResult{}, ErrLimit
	}
	h.transportErr = nil
	result, err := c.client.Chat(ctx, request)
	if h.transportErr != nil {
		return result, &llm.Error{Category: llm.ErrorInvalidRequest, Cause: h.transportErr}
	}
	// Keep call_started until the proposal artifact is durable. Receiving a
	// response alone is insufficient to safely repeat this evaluation on restart.
	if err == nil {
		if e := h.service.mutate(ctx, &h.owned, "assessment_received", func(item *row) error {
			if result.Usage.TotalTokens > 0 && result.Usage.TotalTokens <= h.reserve {
				item.TokensLeft += h.reserve - result.Usage.TotalTokens
				item.CostUnknown = false
			}
			return nil
		}); e != nil {
			return result, e
		}
	}
	return result, err
}

func assessmentRequest(task learning.AssessmentTask, activity learning.Activity, attempt learning.Attempt) learning.ProposalRequest {
	input, _ := json.Marshal(map[string]any{"work_item": map[string]any{"activity": activity, "attempt": attempt}})
	nodes := []string{}
	for _, r := range activity.References {
		nodes = append(nodes, r.NodeRevisionID)
	}
	return learning.ProposalRequest{RequestID: task.RequestID, Type: learning.ProposalAssessment, AggregateType: "session", AggregateID: task.SessionID, AggregateVersion: task.SessionVersion, KnowledgeRevisionID: activity.KnowledgeRevisionID, NodeRevisionIDs: nodes, Input: input}
}
func assessmentCommand(task learning.AssessmentTask, proposal string) learning.ActionCommand {
	return learning.ActionCommand{Operation: learning.OperationEnvelope{OperationID: task.OperationID, PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: task.SessionID, ExpectedVersion: task.SessionVersion, Payload: json.RawMessage(`{}`)}, Action: tutoring.ActionRecordAssessment, ProposalID: proposal}
}

func (h *executionHost) assess(ctx context.Context) error {
	ctx, err := learningspace.WithScope(ctx, h.owned.SpaceID)
	if err != nil {
		return err
	}
	task := *h.body.Assessment
	var recovery learning.AssessmentRecovery
	err = h.service.mutateTx(ctx, &h.owned, "assessment_checked", func(tx pgx.Tx, item *row) error {
		var e error
		recovery, e = h.service.starter.Learning.InspectAssessmentTx(ctx, tx, task)
		return e
	})
	if err != nil {
		return err
	}
	if recovery.OperationStatus == "succeeded" {
		return h.completeAssessment(ctx, "")
	}
	if recovery.OperationStatus != "" || recovery.ProposalStatus == "failed" {
		return ErrConflict
	}
	view, err := h.service.starter.Learning.Session(ctx, task.SessionID)
	if err != nil {
		return err
	}
	if view.WorkItem.Activity == nil || view.WorkItem.Attempt == nil || view.WorkItem.Attempt.ID != task.AttemptID {
		return ErrConflict
	}
	ks, err := knowledge.NewService(h.service.starter.Knowledge, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		return err
	}
	proposal := recovery.ProposalID
	if proposal == "" && view.WorkItem.Activity.Type != learning.ActivityObjective {
		client, model, _, e := h.service.settings.StudioClient(func(next http.RoundTripper) http.RoundTripper { return budgetTransport{host: h, next: next} })
		if e != nil {
			return e
		}
		svc, e := learning.NewService(h.service.starter.Learning, h.service.starter.Learning, learningknowledge.New(ks), learning.ServiceOptions{Model: tutormodel.NewWithClient(assessmentChat{h, client}), ModelID: model, PromptRevision: "exercise-assessment-v1"})
		if e != nil {
			return e
		}
		artifact, e := svc.Propose(ctx, task.DeviceID, assessmentRequest(task, *view.WorkItem.Activity, *view.WorkItem.Attempt))
		if e != nil {
			if h.transportErr != nil {
				return h.transportErr
			}
			if settleErr := h.service.mutateTx(ctx, &h.owned, "assessment_rejected", func(tx pgx.Tx, item *row) error {
				state, err := h.service.starter.Learning.InspectAssessmentTx(ctx, tx, task)
				if err != nil {
					return err
				}
				if state.ProposalStatus == "failed" {
					item.callStarted = false
				}
				return nil
			}); settleErr != nil {
				return settleErr
			}
			return e
		}
		proposal = artifact.ID
	}
	return h.completeAssessment(ctx, proposal)
}

type assessmentTreeTx struct {
	store *knowledgepg.Store
	tx    pgx.Tx
}

func (t assessmentTreeTx) Tree(ctx context.Context, id string) (knowledge.TreeResult, error) {
	return t.store.TreeTx(ctx, t.tx, id)
}

func (h *executionHost) completeAssessment(ctx context.Context, proposal string) error {
	return h.service.mutateTx(ctx, &h.owned, "completed", func(tx pgx.Tx, item *row) error {
		task := *item.body.Assessment
		store := h.service.starter.Learning.WithinTx(tx)
		state, err := store.InspectAssessmentTx(ctx, tx, task)
		if err != nil {
			return err
		}
		if state.OperationStatus != "succeeded" {
			svc, e := learning.NewService(store, store, learningknowledge.New(assessmentTreeTx{h.service.starter.Knowledge, tx}), learning.ServiceOptions{})
			if e != nil {
				return e
			}
			if _, e = svc.ApplyAction(ctx, task.DeviceID, task.SessionID, assessmentCommand(task, proposal)); e != nil {
				return e
			}
		}
		item.Status = "succeeded"
		item.Stage = "assessment_committed"
		item.Reason = ""
		item.callStarted = false
		item.ResultUnknown = false
		return nil
	})
}
