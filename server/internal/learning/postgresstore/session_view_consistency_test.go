package postgresstore

import (
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

func TestConsistentSessionProjectionAcceptsPersistedInvalidationMarker(t *testing.T) {
	contextValue := tutoring.FocusContext{GoalRevisionID: "goal"}
	frame := &tutoring.FocusFrame{
		ID: "frame", SessionID: "session", SavedState: tutoring.StateRouteActive,
		Context: contextValue, SavedAggregateVersion: 3, CreatedEventSequence: 7,
		Invalidated: true, InvalidationReason: "switch_goal",
	}
	projected := tutoring.Session{
		ID: "session", State: tutoring.StateGoalReady, AggregateVer: 4,
		Context: contextValue, ActiveFrame: frame,
	}
	authority := projected
	authority.ActiveFrame = nil
	authority.FocusFrameInvalidated = true

	if !consistentSessionProjection(projected, authority) {
		t.Fatal("persisted invalidation marker should be equivalent to the projected invalidated frame")
	}
	for _, mutate := range []func(*tutoring.Session){
		func(value *tutoring.Session) { value.ActiveFrame.InvalidationReason = "" },
		func(value *tutoring.Session) { value.ActiveFrame.SessionID = "other" },
		func(value *tutoring.Session) { value.ActiveFrame.Invalidated = false },
		func(value *tutoring.Session) { value.AggregateVer++ },
	} {
		corrupt := projected
		frameCopy := *projected.ActiveFrame
		corrupt.ActiveFrame = &frameCopy
		mutate(&corrupt)
		if consistentSessionProjection(corrupt, authority) {
			t.Fatalf("corrupt projected invalidation was accepted: %+v", corrupt)
		}
	}
}
