package learning

import (
	"sort"
	"time"
)

type InteractionSample struct {
	EventSequence int64     `json:"event_seq"`
	SessionID     string    `json:"session_id"`
	ReceivedAt    time.Time `json:"received_at"`
	UserInitiated bool      `json:"user_initiated"`
}

type ActiveTimeEstimate struct {
	DurationSeconds  int64      `json:"duration_seconds"`
	Estimated        bool       `json:"estimated"`
	AlgorithmVersion string     `json:"algorithm_version"`
	SampleCount      int        `json:"sample_count"`
	FirstReceivedAt  *time.Time `json:"first_received_at,omitempty"`
	LastReceivedAt   *time.Time `json:"last_received_at,omitempty"`
}

func EstimateActiveTime(sessionID string, samples []InteractionSample) ActiveTimeEstimate {
	filtered := make([]InteractionSample, 0, len(samples))
	for _, sample := range samples {
		if sample.SessionID == sessionID && sample.UserInitiated {
			filtered = append(filtered, sample)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].ReceivedAt.Equal(filtered[j].ReceivedAt) {
			return filtered[i].EventSequence < filtered[j].EventSequence
		}
		return filtered[i].ReceivedAt.Before(filtered[j].ReceivedAt)
	})
	result := ActiveTimeEstimate{Estimated: true, AlgorithmVersion: ActiveTimePolicyVersion, SampleCount: len(filtered)}
	if len(filtered) == 0 {
		return result
	}
	first, last := filtered[0].ReceivedAt, filtered[len(filtered)-1].ReceivedAt
	result.FirstReceivedAt, result.LastReceivedAt = &first, &last
	for i := 1; i < len(filtered); i++ {
		gap := filtered[i].ReceivedAt.Sub(filtered[i-1].ReceivedAt)
		if gap <= 0 {
			continue
		}
		if gap > 5*time.Minute {
			gap = 5 * time.Minute
		}
		result.DurationSeconds += int64(gap / time.Second)
	}
	return result
}

func isActiveTimeEvent(eventType EventType) bool {
	switch eventType {
	case EventAttemptSubmitted, EventFreeQuestionAsked, EventReviewPresented:
		return true
	default:
		return false
	}
}

func estimateActiveTimeFromTimeline(sessionID string, timeline []TimelineItem) ActiveTimeEstimate {
	samples := make([]InteractionSample, 0)
	for _, item := range timeline {
		if item.AggregateID == sessionID && isActiveTimeEvent(item.Type) {
			samples = append(samples, InteractionSample{
				EventSequence: item.EventSequence,
				SessionID:     sessionID,
				ReceivedAt:    item.ReceivedAt,
				UserInitiated: true,
			})
		}
	}
	return EstimateActiveTime(sessionID, samples)
}
