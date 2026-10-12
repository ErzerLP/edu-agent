package learning

import "context"

type proposalRetryKey struct{}

// ProposalRetryReason is internal model feedback; it never changes the frozen
// request, its hash, the public API, or the validator's inputs.
func ProposalRetryReason(ctx context.Context) string {
	reason, _ := ctx.Value(proposalRetryKey{}).(string)
	return reason
}

func retryableProposalCategory(kind ProposalType, category string) bool {
	switch category {
	case "schema_mismatch", "malformed_json", "reference_hash_mismatch", "invalid_answer_quote", "invalid_knowledge_quote",
		"knowledge_reference_mismatch", "unknown_risk_flag", "unknown_or_duplicate_rubric_item", "misconception_without_failure", "invalid_text":
		return true
	case "activity_target_reference_missing":
		return kind == ProposalActivity
	}
	return false
}
