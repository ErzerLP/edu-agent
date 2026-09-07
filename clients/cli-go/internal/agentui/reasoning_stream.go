package agentui

import (
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

// Coalesce only already queued, same-request reasoning deltas. This reduces
// full transcript redraws without a timer, an unbounded queue, or reordering
// start/terminal activities. One nonmatching lookahead is retained exactly.
func (s *turnStream) popActivity() (agentloop.Activity, bool) {
	s.deltaMu.Lock()
	defer s.deltaMu.Unlock()
	if s.activityPending != nil {
		activity := *s.activityPending
		s.activityPending = nil
		return s.coalesceActivityLocked(activity), true
	}
	select {
	case activity := <-s.activities:
		return s.coalesceActivityLocked(activity), true
	default:
		return agentloop.Activity{}, false
	}
}

func (s *turnStream) coalesceActivity(activity agentloop.Activity) agentloop.Activity {
	s.deltaMu.Lock()
	defer s.deltaMu.Unlock()
	return s.coalesceActivityLocked(activity)
}

func (s *turnStream) coalesceActivityLocked(activity agentloop.Activity) agentloop.Activity {
	if activity.Kind != agentloop.ActivityReasoningDelta {
		return activity
	}
	var text strings.Builder
	text.WriteString(activity.Delta)
	for text.Len() < 64<<10 {
		select {
		case next := <-s.activities:
			if next.Kind != activity.Kind || next.Event.ID != activity.Event.ID || text.Len()+len(next.Delta) > 64<<10 {
				s.activityPending = &next
				activity.Delta = text.String()
				return activity
			}
			text.WriteString(next.Delta)
		default:
			activity.Delta = text.String()
			return activity
		}
	}
	activity.Delta = text.String()
	return activity
}
