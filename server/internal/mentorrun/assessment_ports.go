package mentorrun

import (
	"context"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

type AssessmentStatus struct {
	Task learning.AssessmentTask
	Run  *Meta
}
type AssessmentResume struct {
	OperationID     string
	ExpectedVersion int64
	Policy          learningcontent.AssessmentPolicy
}

func assessmentCanResume(item row, recovery learning.AssessmentRecovery) bool {
	if !item.BodyAvailable || !time.Now().Before(item.ExpiresAt) || recovery.OperationStatus != "" || item.Status != "failed" {
		return false
	}
	// A durable proposal can be committed without another model call, including
	// older failures whose call_started/result_unknown flags were never settled.
	if recovery.ProposalStatus == "ready" {
		return true
	}
	if item.ResultUnknown || item.callStarted || recovery.ProposalStatus == "processing" && recovery.ProposalLeaseUntil != nil && recovery.ProposalLeaseUntil.After(time.Now()) {
		return false
	}
	return item.Reason == "context_changed" || recovery.ProposalStatus == "failed"
}

// Assessment authorization binds the model destination.
func automaticAssessmentPolicy(configuration string) learningcontent.AssessmentPolicy {
	return learningcontent.AssessmentPolicy{
		Configuration: configuration,
	}
}

// AssessmentPolicy binds explicit authorization to the current model
// destination/configuration. C3b must obtain this before publishing a candidate.
func (s *Service) AssessmentPolicy() (learningcontent.AssessmentPolicy, error) {
	model, _, configuration, err := s.settings.MentorClient()
	if err != nil || model == nil {
		return learningcontent.AssessmentPolicy{}, ErrModel
	}
	return learningcontent.AssessmentPolicy{Configuration: configuration}, nil
}

// AssessmentStatus is an internal, source-gated owner port. Existing public
// mentor readers exclude this task kind until C3b defines its HTTP contract.
func (s *Service) AssessmentStatus(ctx context.Context, actor identity.Credential, attempt string) (AssessmentStatus, error) {
	var result AssessmentStatus
	if !s.CanStart() || !validID(attempt) {
		return result, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	if _, err = gates(ctx, tx, false); err != nil {
		return result, err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, false); err != nil {
		return result, err
	}
	result.Task, err = s.starter.Learning.AssessmentTaskTx(ctx, tx, attempt)
	if err != nil {
		return result, err
	}
	if result.Task.DeviceID != actor.Device.ID {
		return AssessmentStatus{}, ErrNotFound
	}
	if _, err = s.starter.Content.ExerciseTx(ctx, tx, actor, result.Task.ExerciseID); err != nil {
		return AssessmentStatus{}, err
	}
	if result.Task.RunID != "" {
		item, e := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1 AND space_id=$2 AND device_id=$3`, result.Task.RunID, learningspace.Scope(ctx), actor.Device.ID))
		if e != nil {
			return result, e
		}
		result.Run = &item.Meta
	}
	return result, nil
}

// ResumeAssessment is explicit destination authorization, not an
// automatic retry. It never creates an Attempt or changes the task's IDs.
func (s *Service) ResumeAssessment(ctx context.Context, actor identity.Credential, attempt string, c AssessmentResume) (Receipt, error) {
	if !s.CanStart() || !validID(attempt) || !validID(c.OperationID) || c.ExpectedVersion < 1 {
		return Receipt{}, ErrInvalid
	}
	policy, err := s.AssessmentPolicy()
	if err != nil {
		return Receipt{}, err
	}
	if policy != c.Policy {
		return Receipt{}, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, true)
	if err != nil {
		return Receipt{}, err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, true); err != nil {
		return Receipt{}, err
	}
	task, err := s.starter.Learning.AssessmentTaskTx(ctx, tx, attempt)
	if err != nil {
		return Receipt{}, err
	}
	if task.DeviceID != actor.Device.ID || task.RunID == "" {
		return Receipt{}, ErrNotFound
	}
	hash := requestHash("resume_assessment", learningspace.Scope(ctx), attempt, c)
	if r, found, e := operation(ctx, tx, actor.Device.ID, c.OperationID, hash); e != nil || found {
		return r, e
	}
	item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1 FOR UPDATE`, task.RunID))
	if err != nil {
		return Receipt{}, err
	}
	if item.Generation != generation || item.Version != c.ExpectedVersion || !item.BodyAvailable || time.Now().After(item.ExpiresAt) {
		return Receipt{}, ErrConflict
	}
	if err = s.decode(&item); err != nil {
		return Receipt{}, err
	}
	if err = s.assessmentContextTx(ctx, tx, item); err != nil {
		return Receipt{}, err
	}
	recovery, err := s.starter.Learning.InspectAssessmentTx(ctx, tx, task)
	if err != nil {
		return Receipt{}, err
	}
	if !assessmentCanResume(item, recovery) {
		return Receipt{}, ErrConflict
	}
	if err = s.starter.Learning.ResumeAssessmentProposalTx(ctx, tx, task); err != nil {
		return Receipt{}, err
	}
	item.Configuration = policy.Configuration
	item.Status = "queued"
	item.Stage = "queued"
	item.Reason = ""
	item.callStarted = false
	item.ResultUnknown = false
	item.lease = nil
	item.leaseUntil = nil
	if err = s.save(ctx, tx, &item, "assessment_resumed"); err != nil {
		return Receipt{}, err
	}
	r, err := receipt(ctx, tx, item, c.OperationID, hash)
	if err != nil {
		return Receipt{}, err
	}
	return r, tx.Commit(ctx)
}
