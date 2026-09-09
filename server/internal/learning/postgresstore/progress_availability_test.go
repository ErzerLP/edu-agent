package postgresstore

import (
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
)

func TestReviewAvailabilityRequiresOriginalExecutableContext(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, state, node, status, reason string
		future                            bool
	}{
		{"原节点可开始", "RouteActive", "node", "active", "", false},
		{"离开原节点", "RouteActive", "other", "active", "original_session_not_at_review_node", false},
		{"已结束会话", "Completed", "node", "active", "original_session_not_at_review_node", false},
		{"暂停目标", "RouteActive", "node", "paused", "goal_or_space_not_active", false},
		{"尚未到期", "RouteActive", "node", "active", "not_due", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			review := learning.ReviewSchedule{SessionID: "session", GoalRevisionID: "goal", RouteRevisionID: "route", NodeRevisionID: "node", DueAt: now.Add(-time.Hour)}
			if tc.future {
				review.DueAt = now.Add(time.Hour)
			}
			sessions := []learning.SessionSummary{{SessionID: "session", GoalRevisionID: "goal", RouteRevisionID: "route", NodeRevisionID: tc.node, State: tc.state}}
			setReviewAvailability(&review, sessions, "active", tc.status, now)
			if review.Startable != (tc.reason == "") || review.UnavailableReason != tc.reason {
				t.Fatalf("复习入口与原上下文不一致：%+v", review)
			}
		})
	}
}
