package agentcontroller

import (
	"context"
	"errors"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

var ErrPreferenceReceiptNotFound = errors.New("没有可重试核对的长期偏好回执")

// RetryPreferenceReceipt explicitly reconciles one persisted unknown preference
// outcome. It reuses the exact typed payload, operation IDs, candidate identity,
// revision, and stage from encrypted storage. It never restores the old selector
// or permits changing the decision to session-only or decline.
func (c *Controller) RetryPreferenceReceipt(ctx context.Context, receiptID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return agentloop.ErrSessionClosed
	}
	if c.providerBlocked {
		return ErrProviderConfirmationRequired
	}
	if !c.persistent || c.handle == nil || c.server == nil || c.dirty != nil {
		return ErrPreferenceReceiptNotFound
	}
	receiptID = strings.TrimSpace(receiptID)
	index := -1
	for current := range c.record.PreferenceReceipts {
		receipt := c.record.PreferenceReceipts[current]
		if receipt.CreateOperationID == receiptID && receipt.Outcome == agentsession.NoticeOutcomeUnknown {
			index = current
			break
		}
	}
	if index < 0 {
		return ErrPreferenceReceiptNotFound
	}
	receipt := c.record.PreferenceReceipts[index]
	switch receipt.Stage {
	case agentsession.PreferenceStageCreate:
		return c.retryPreferenceCreateLocked(ctx, index, receipt)
	case agentsession.PreferenceStageAdmit:
		return c.retryPreferenceAdmitLocked(ctx, index, receipt)
	case agentsession.PreferenceStageReject:
		return c.retryPreferenceRejectLocked(ctx, index, receipt)
	default:
		return agentsession.ErrCorrupt
	}
}

func (c *Controller) retryPreferenceCreateLocked(ctx context.Context, index int, receipt agentsession.PreferenceReceipt) error {
	payload := receipt.Payload
	result, err := c.server.CreateMemoryCandidate(ctx, api.MemoryCandidateRequest{
		OperationID: receipt.CreateOperationID, PayloadSchemaVersion: 1,
		Content: payload.Content, Reason: payload.Reason, Category: payload.Category,
		Sensitivity: payload.Sensitivity, Stability: payload.Stability, ValidUntil: payload.ValidUntil,
	})
	if err != nil {
		if code, deterministic := preferenceAPIErrorCode(err); deterministic {
			receipt.StableCode = code
			receipt.Outcome = agentsession.NoticeOutcomeRejected
			return c.persistPreferenceReceiptLocked(ctx, index, receipt)
		}
		return agentloop.ErrPreferenceOutcomeUnknown
	}
	candidate, ok := preferenceCandidate(result, "")
	if !ok {
		return agentloop.ErrPreferenceOutcomeUnknown
	}
	receipt.CandidateID = candidate.ID
	receipt.CandidateRevision = candidate.Revision
	switch candidate.Status {
	case "admitted":
		receipt.StableCode = "preference_saved"
		receipt.Outcome = agentsession.NoticeOutcomeCompleted
		return c.persistPreferenceReceiptLocked(ctx, index, receipt)
	case "rejected", "expired":
		receipt.StableCode = "candidate_" + candidate.Status
		receipt.Outcome = agentsession.NoticeOutcomeRejected
		return c.persistPreferenceReceiptLocked(ctx, index, receipt)
	case "pending_review":
		receipt.Stage = agentsession.PreferenceStageAdmit
		receipt.StableCode = "preference_admit_pending"
		if err := c.persistPreferenceReceiptLocked(ctx, index, receipt); err != nil {
			return err
		}
		return c.retryPreferenceAdmitLocked(ctx, index, c.record.PreferenceReceipts[index])
	default:
		return agentloop.ErrPreferenceOutcomeUnknown
	}
}

func (c *Controller) retryPreferenceAdmitLocked(ctx context.Context, index int, receipt agentsession.PreferenceReceipt) error {
	view, err := c.server.MemoryCandidate(ctx, receipt.CandidateID)
	if err != nil {
		return agentloop.ErrPreferenceOutcomeUnknown
	}
	candidate := view.Candidate
	if candidate.ID != receipt.CandidateID || candidate.Revision < 1 {
		return agentloop.ErrPreferenceOutcomeUnknown
	}
	switch candidate.Status {
	case "admitted":
		receipt.CandidateRevision = candidate.Revision
		receipt.StableCode = "preference_saved"
		receipt.Outcome = agentsession.NoticeOutcomeCompleted
		return c.persistPreferenceReceiptLocked(ctx, index, receipt)
	case "rejected", "expired":
		receipt.CandidateRevision = candidate.Revision
		receipt.StableCode = "candidate_" + candidate.Status
		receipt.Outcome = agentsession.NoticeOutcomeRejected
		return c.persistPreferenceReceiptLocked(ctx, index, receipt)
	case "pending_review":
		if candidate.Revision != receipt.CandidateRevision {
			receipt.CandidateRevision = candidate.Revision
			if err := c.persistPreferenceReceiptLocked(ctx, index, receipt); err != nil {
				return err
			}
		}
	default:
		return agentloop.ErrPreferenceOutcomeUnknown
	}
	result, err := c.server.DecideMemoryCandidate(ctx, receipt.CandidateID, api.MemoryCandidateDecisionRequest{
		OperationID: receipt.AdmitOperationID, PayloadSchemaVersion: 1,
		ExpectedRevision: receipt.CandidateRevision, Decision: "admit", Reason: "user_confirmed_preference_save",
	})
	if err != nil {
		if code, deterministic := preferenceAPIErrorCode(err); deterministic {
			receipt.Stage = agentsession.PreferenceStageReject
			receipt.StableCode = code
			if err := c.persistPreferenceReceiptLocked(ctx, index, receipt); err != nil {
				return err
			}
			return c.retryPreferenceRejectLocked(ctx, index, c.record.PreferenceReceipts[index])
		}
		return agentloop.ErrPreferenceOutcomeUnknown
	}
	resolved, ok := preferenceCandidate(result, receipt.CandidateID)
	if !ok || resolved.Status != "admitted" || resolved.Revision <= receipt.CandidateRevision {
		return agentloop.ErrPreferenceOutcomeUnknown
	}
	receipt.CandidateRevision = resolved.Revision
	receipt.StableCode = "preference_saved"
	receipt.Outcome = agentsession.NoticeOutcomeCompleted
	return c.persistPreferenceReceiptLocked(ctx, index, receipt)
}

func (c *Controller) retryPreferenceRejectLocked(ctx context.Context, index int, receipt agentsession.PreferenceReceipt) error {
	view, err := c.server.MemoryCandidate(ctx, receipt.CandidateID)
	if err != nil {
		return agentloop.ErrPreferenceOutcomeUnknown
	}
	candidate := view.Candidate
	if candidate.ID != receipt.CandidateID || candidate.Revision < 1 {
		return agentloop.ErrPreferenceOutcomeUnknown
	}
	if candidate.Status == "rejected" || candidate.Status == "expired" {
		receipt.CandidateRevision = candidate.Revision
		if receipt.StableCode == "" {
			receipt.StableCode = "candidate_" + candidate.Status
		}
		receipt.Outcome = agentsession.NoticeOutcomeRejected
		return c.persistPreferenceReceiptLocked(ctx, index, receipt)
	}
	if candidate.Status == "admitted" {
		return agentloop.ErrPreferenceOutcomeUnknown
	}
	if candidate.Status != "pending_review" {
		return agentloop.ErrPreferenceOutcomeUnknown
	}
	if candidate.Revision != receipt.CandidateRevision {
		receipt.CandidateRevision = candidate.Revision
		if err := c.persistPreferenceReceiptLocked(ctx, index, receipt); err != nil {
			return err
		}
	}
	result, err := c.server.DecideMemoryCandidate(ctx, receipt.CandidateID, api.MemoryCandidateDecisionRequest{
		OperationID: receipt.RejectOperationID, PayloadSchemaVersion: 1,
		ExpectedRevision: receipt.CandidateRevision, Decision: "reject", Reason: "compensate_failed_preference_admission",
	})
	if err != nil {
		return agentloop.ErrPreferenceOutcomeUnknown
	}
	resolved, ok := preferenceCandidate(result, receipt.CandidateID)
	if !ok || resolved.Status != "rejected" || resolved.Revision <= receipt.CandidateRevision {
		return agentloop.ErrPreferenceOutcomeUnknown
	}
	receipt.CandidateRevision = resolved.Revision
	if receipt.StableCode == "" {
		receipt.StableCode = "preference_rejected"
	}
	receipt.Outcome = agentsession.NoticeOutcomeRejected
	return c.persistPreferenceReceiptLocked(ctx, index, receipt)
}

func (c *Controller) persistPreferenceReceiptLocked(ctx context.Context, index int, receipt agentsession.PreferenceReceipt) error {
	if index < 0 || index >= len(c.record.PreferenceReceipts) {
		return ErrPreferenceReceiptNotFound
	}
	previous := c.record.PreferenceReceipts[index]
	c.record.PreferenceReceipts[index] = receipt
	if err := c.saveRecordLocked(ctx, false); err != nil {
		c.record.PreferenceReceipts[index] = previous
		persistenceErr := checkpointPersistenceError(err)
		c.saveFailed = persistenceErr
		return persistenceErr
	}
	return nil
}

func preferenceCandidate(result api.MemoryOperationResponse, expectedID string) (api.MemoryCandidate, bool) {
	if result.Candidate == nil {
		return api.MemoryCandidate{}, false
	}
	candidate := result.Candidate.Candidate
	if candidate.ID == "" || candidate.Revision < 1 || expectedID != "" && candidate.ID != expectedID {
		return api.MemoryCandidate{}, false
	}
	return candidate, true
}

func preferenceAPIErrorCode(err error) (string, bool) {
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) || apiErr.Status < 400 || apiErr.Status >= 500 {
		return "", false
	}
	code := stablePreferenceRetryCode(apiErr.Code, "preference_request_rejected")
	return code, true
}

func stablePreferenceRetryCode(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fallback
	}
	for _, current := range value {
		if current != '_' && current != '-' && (current < 'a' || current > 'z') && (current < '0' || current > '9') {
			return fallback
		}
	}
	return value
}
