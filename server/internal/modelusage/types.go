// Package modelusage owns actual model HTTP attempts under learning privacy.
package modelusage

import (
	"errors"
	"math"
	"time"
)

var ErrInactive = errors.New("usage_generation_changed")
var ErrInvalid = errors.New("invalid_usage_query")
var ErrNotFound = errors.New("usage_task_not_found")

type Usage struct {
	Input         *int64 `json:"input_tokens"`
	Output        *int64 `json:"output_tokens"`
	Reasoning     *int64 `json:"reasoning_tokens"`
	ProviderTotal *int64 `json:"provider_total_tokens"`
	Inconsistent  bool   `json:"usage_inconsistent"`
}

func (u Usage) Total() *int64 {
	if u.Inconsistent || u.Input == nil || u.Output == nil || *u.Input > math.MaxInt64-*u.Output {
		return nil
	}
	n := *u.Input + *u.Output
	return &n
}

func (u *Usage) normalize() {
	if u.Reasoning != nil && u.Output != nil && *u.Reasoning > *u.Output {
		u.Inconsistent = true
	}
	if u.Input != nil && u.Output != nil {
		if *u.Input > math.MaxInt64-*u.Output || u.ProviderTotal != nil && *u.ProviderTotal != *u.Input+*u.Output {
			u.Inconsistent = true
		}
	}
}

type BeginRequest struct {
	Generation                                            int64
	SpaceID, Source, ActorDeviceID, Kind, OriginID        string
	Stage, AttemptKey, LeaseID                            string
	LeaseUntil                                            *time.Time
	Provider, Destination, Model, APIStyle, Configuration string
}
type CallRef struct {
	ID         string
	Generation int64
	APIStyle   string
	LeaseID    string
	LeaseUntil time.Time
}
type Metric struct {
	KnownSum     int64 `json:"known_sum"`
	UnknownCalls int64 `json:"unknown_calls"`
}
type Totals struct {
	Calls               int64  `json:"calls"`
	PendingCalls        int64  `json:"pending_calls"`
	OutcomeUnknownCalls int64  `json:"outcome_unknown_calls"`
	Input               Metric `json:"input"`
	Output              Metric `json:"output"`
	Reasoning           Metric `json:"reasoning"`
	Total               Metric `json:"total"`
}
type Bucket struct {
	Start time.Time `json:"start"`
	Totals
}
type Model struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	APIStyle    string `json:"api_style"`
	Destination string `json:"destination"`
}
type Summary struct {
	Generation        int64      `json:"privacy_generation"`
	CoverageSince     time.Time  `json:"coverage_since"`
	FullCoverageSince *time.Time `json:"full_coverage_since"`
	Coverage          string     `json:"coverage"`
	From              time.Time  `json:"from"`
	To                time.Time  `json:"to"`
	Timezone          string     `json:"timezone"`
	Grain             string     `json:"grain"`
	Totals            Totals     `json:"totals"`
	Buckets           []Bucket   `json:"buckets"`
	Models            []Model    `json:"models"`
}
type Task struct {
	ActorDeviceID  *string   `json:"actor_device_id"`
	CLISessionID   *string   `json:"cli_session_id"`
	CLITurnID      *string   `json:"cli_turn_id"`
	ConversationID *string   `json:"conversation_id"`
	ID             string    `json:"id"`
	SpaceID        *string   `json:"space_id"`
	Kind           string    `json:"kind"`
	Source         string    `json:"source"`
	StartedAt      time.Time `json:"started_at"`
	RunID          *string   `json:"run_id"`
	Totals         Totals    `json:"totals"`
}
type Call struct {
	ID         string     `json:"id"`
	Stage      string     `json:"stage"`
	AttemptNo  int64      `json:"attempt_no"`
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Model
	Usage
	Total *int64 `json:"total_tokens"`
}
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	Generation int64  `json:"privacy_generation"`
}
