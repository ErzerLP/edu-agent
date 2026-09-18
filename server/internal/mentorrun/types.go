// Package mentorrun 提供目标内导师的持久运行，不拥有教学状态或本机执行能力。
package mentorrun

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningstart"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
)

var (
	ErrInvalid   = errors.New("invalid_run_request")
	ErrConflict  = errors.New("version_conflict")
	ErrOperation = errors.New("idempotency_conflict")
	ErrNotFound  = errors.New("run_not_found")
	ErrForbidden = errors.New("run_forbidden")
	ErrInactive  = errors.New("run_context_changed")
	ErrResync    = errors.New("resync_required")
	ErrStorage   = errors.New("run_storage_unavailable")
	ErrLimit     = errors.New("run_storage_limit")
	ErrBudget    = errors.New("run_budget_exhausted")
	ErrLease     = errors.New("run_lease_lost")
	ErrModel     = errors.New("mentor_not_configured")
)

const MaxBody = 256 << 10
const MaxOutput = 64 << 10
const EventWindow = 128

type Create struct {
	TeachingSessionID string                       `json:"teaching_session_id,omitempty"`
	ContentEdit       *learningcontent.EditRequest `json:"content_edit,omitempty"`
	StartLearning     *learningstart.Request       `json:"start_learning,omitempty"`
	Research          *research.Request            `json:"research,omitempty"`
	OperationID       string                       `json:"operation_id"`
	SessionID         string                       `json:"session_id"`
	ExpectedVersion   int64                        `json:"expected_version"`
	Prompt            string                       `json:"prompt"`
	Save              bool                         `json:"save"`
	RequestBudget     int                          `json:"request_budget"`
	TokenBudget       int                          `json:"token_budget"`
}

type Command struct {
	OperationID     string `json:"operation_id"`
	ExpectedVersion int64  `json:"expected_version"`
	Kind            string `json:"kind"`
	InteractionID   string `json:"interaction_id,omitempty"`
	Answer          string `json:"answer,omitempty"`
	RequestBudget   int    `json:"request_budget,omitempty"`
	TokenBudget     int    `json:"token_budget,omitempty"`
}

type Receipt struct {
	OperationID string `json:"operation_id"`
	RunID       string `json:"run_id"`
	SessionID   string `json:"session_id"`
	Version     int64  `json:"version"`
}

type Meta struct {
	TeachingSessionID string    `json:"teaching_session_id,omitempty"`
	Kind              string    `json:"kind,omitempty"`
	RunID             string    `json:"run_id"`
	SessionID         string    `json:"session_id"`
	SpaceID           string    `json:"space_id"`
	GoalID            string    `json:"goal_id"`
	GoalVersion       int64     `json:"goal_version"`
	Generation        int64     `json:"privacy_generation"`
	Version           int64     `json:"version"`
	Watermark         int64     `json:"watermark"`
	Status            string    `json:"status"`
	Stage             string    `json:"stage"`
	Reason            string    `json:"reason"`
	Saved             bool      `json:"saved"`
	BodyAvailable     bool      `json:"body_available"`
	RequestsLeft      int       `json:"requests_left"`
	TokensLeft        int       `json:"tokens_left"`
	RequestsUsed      int       `json:"requests_used"`
	ResultUnknown     bool      `json:"result_unknown"`
	CostUnknown       bool      `json:"cost_unknown"`
	UpdatedAt         time.Time `json:"updated_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	Configuration     string    `json:"configuration"`
}

type Interaction struct {
	ReferenceSelection bool     `json:"reference_selection,omitempty"`
	ID                 string   `json:"id"`
	Question           string   `json:"question"`
	Choices            []string `json:"choices"`
	Approval           bool     `json:"approval"`
	CallID             string   `json:"call_id"`
}

type Body struct {
	// PDF 原件仅进入加密运行正文，不属于快照、模型输入或工具输出。
	SourceFiles   map[string][]byte      `json:"source_files,omitempty"`
	ChangeBase    *learningchange.Base   `json:"change_base,omitempty"`
	ContentEdit   *ContentEditState      `json:"content_edit,omitempty"`
	StartLearning *learningstart.State   `json:"start_learning,omitempty"`
	Research      *research.State        `json:"research,omitempty"`
	Messages      []modelclient.Message  `json:"messages"`
	Pending       []modelclient.ToolCall `json:"pending"`
	Output        string                 `json:"output"`
	Interaction   *Interaction           `json:"interaction,omitempty"`
}

type Snapshot struct {
	ContentEdit *ContentEditState `json:"content_edit,omitempty"`
	Meta
	StartLearning *learningstart.State `json:"start_learning,omitempty"`
	Research      *research.State      `json:"research,omitempty"`
	Output        string               `json:"output"`
	Interaction   *Interaction         `json:"interaction,omitempty"`
}

type Event struct {
	RunID      string `json:"run_id"`
	SessionID  string `json:"session_id"`
	SpaceID    string `json:"space_id"`
	GoalID     string `json:"goal_id"`
	Generation int64  `json:"privacy_generation"`
	Version    int64  `json:"version"`
	Seq        int64  `json:"seq"`
	Type       string `json:"type"`
}

func validID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}
func validText(s string, limit int) bool {
	return utf8.ValidString(s) && len(s) <= limit && strings.TrimSpace(s) != "" && !strings.ContainsRune(s, 0)
}
func terminal(s string) bool {
	return s == "succeeded" || s == "partial" || s == "failed" || s == "cancelled"
}
func bounded(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
