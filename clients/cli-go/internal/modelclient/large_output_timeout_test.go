package modelclient

import "testing"

func TestLargeOutputStillEnforcesInactivityTimeout(t *testing.T) {
	TestStreamConfiguredInactivityTimeoutRequiresResponseBody(t)
}
