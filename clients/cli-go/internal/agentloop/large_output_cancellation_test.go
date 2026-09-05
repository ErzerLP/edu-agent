package agentloop

import "testing"

// Existing cancellation/effect contracts remain part of the large-output gate.
func TestLargeOutputCancellationKeepsCompletedFileEffect(t *testing.T) {
	TestFileMutationCancellationAfterPublicationPreservesEffect(t)
}
func TestLargeOutputCancelledCheckpointKeepsEffect(t *testing.T) {
	TestCancelledFileEffectCheckpointRoundTripPreservesReceiptHistoryWithoutPending(t)
}
