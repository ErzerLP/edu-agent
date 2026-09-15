// Package settings 管理服务端配置、秘密和显式能力探测，不拥有学习正文或 CLI 配置。
package settings

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

var (
	ErrInvalid  = errors.New("配置格式或范围无效")
	ErrConflict = errors.New("配置版本已变化，请重新读取")
	ErrStorage  = errors.New("受保护的配置存储不可用")
	ErrBusy     = errors.New("已有连接探测正在进行")
	ErrConsent  = errors.New("连接探测需要明确同意外部请求和可能费用")
)

type Target string

const (
	Teaching Target = "teaching"
	Mentor   Target = "mentor"
	Search   Target = "search"
)

type Connection struct {
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider"`
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
	AuthMode string `json:"auth_mode"`
}

type Limits struct {
	ResearchRequests   int `json:"research_requests"`
	ResearchTokens     int `json:"research_tokens"`
	OutputTokens       int `json:"output_tokens"`
	ContextTokens      int `json:"context_tokens"`
	IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
	Concurrency        int `json:"concurrency"`
	StorageMiB         int `json:"storage_mib"`
}

func DefaultLimits() Limits { return Limits{20, 100000, 2048, 32768, 60, 2, 512} }
func (l Limits) Validate() error {
	for _, v := range [][3]int{{l.ResearchRequests, 1, 1000}, {l.ResearchTokens, 1024, 10000000}, {l.OutputTokens, 64, 32768}, {l.ContextTokens, 4096, 2000000}, {l.IdleTimeoutSeconds, 5, 600}, {l.Concurrency, 1, 16}, {l.StorageMiB, 16, 1048576}} {
		if v[0] < v[1] || v[0] > v[2] {
			return ErrInvalid
		}
	}
	if l.OutputTokens >= l.ContextTokens || l.OutputTokens > l.ResearchTokens {
		return ErrInvalid
	}
	return nil
}

type Policy struct {
	PrivateQueries bool `json:"private_queries"`
}

type Probe struct {
	Status         string    `json:"status"`
	Reason         string    `json:"reason"`
	CheckedAt      time.Time `json:"checked_at"`
	ValidUntil     time.Time `json:"valid_until"`
	StructuredJSON bool      `json:"structured_json"`
	NativeSchema   bool      `json:"native_schema"`
	Requests       int       `json:"requests"`
	CostEstimate   string    `json:"cost_estimate"`
}

type ConnectionView struct {
	Connection
	HasKey     bool   `json:"has_key"`
	Configured bool   `json:"configured"`
	Source     string `json:"source"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	Probe      *Probe `json:"probe"`
}

type View struct {
	Revision                int64          `json:"revision"`
	Writable                bool           `json:"writable"`
	Teaching                ConnectionView `json:"teaching"`
	Mentor                  ConnectionView `json:"mentor"`
	EffectiveMentor         ConnectionView `json:"effective_mentor"`
	Search                  ConnectionView `json:"search"`
	MentorUsesTeaching      bool           `json:"mentor_uses_teaching"`
	Limits                  Limits         `json:"limits"`
	Policy                  Policy         `json:"policy"`
	TeachingRestartRequired bool           `json:"teaching_restart_required"`
	ModelEndpoints          []string       `json:"model_endpoints"`
}

type Update struct {
	ExpectedRevision   int64       `json:"expected_revision"`
	Target             Target      `json:"target"`
	Connection         *Connection `json:"connection,omitempty"`
	NewKey             *string     `json:"new_key,omitempty"`
	ClearKey           bool        `json:"clear_key"`
	MentorUsesTeaching *bool       `json:"mentor_uses_teaching,omitempty"`
	Limits             *Limits     `json:"limits,omitempty"`
	Policy             *Policy     `json:"policy,omitempty"`
}

// 更新结构也禁止通过格式化输出意外记录新 Key。
func (Update) String() string     { return "设置更新（秘密已隐藏）" }
func (u Update) GoString() string { return u.String() }

type Capability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}
type Capabilities struct {
	SchemaVersion int        `json:"schema_version"`
	CheckedAt     time.Time  `json:"checked_at"`
	Schema        Capability `json:"schema"`
	Rendering     Capability `json:"rendering"`
	Answering     Capability `json:"answering"`
	Search        Capability `json:"search"`
	Persistence   Capability `json:"persistence"`
	Research      Capability `json:"research"`
	WebMentor     Capability `json:"web_mentor"`
}

type secretSlot struct {
	Connection
	Key    string `json:"key"`
	Source string `json:"source"`
	Probe  *Probe `json:"probe"`
}

func (secretSlot) String() string     { return "服务端连接（秘密已隐藏）" }
func (s secretSlot) GoString() string { return s.String() }

type stored struct {
	Version            int        `json:"version"`
	Revision           int64      `json:"revision"`
	Teaching           secretSlot `json:"teaching"`
	Mentor             secretSlot `json:"mentor"`
	Search             secretSlot `json:"search"`
	MentorUsesTeaching bool       `json:"mentor_uses_teaching"`
	Limits             Limits     `json:"limits"`
	Policy             Policy     `json:"policy"`
}

func endpointURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u == nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(raw, "\\\r\n\t ") || u.Opaque != "" {
		return nil, ErrInvalid
	}
	if u.Host != strings.ToLower(u.Host) || strings.Contains(u.Path, "..") || strings.Contains(u.Host, "%") {
		return nil, ErrInvalid
	}
	return u, nil
}

func validKey(key string) bool {
	if len(key) > 4096 {
		return false
	}
	for _, r := range key {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}

func configured(s secretSlot) bool {
	return s.Endpoint != "" && (s.Provider == "brave" || s.Model != "") && (s.AuthMode == "none" || s.Key != "")
}

func connectionView(s secretSlot, now time.Time) ConnectionView {
	v := ConnectionView{Connection: s.Connection, HasKey: s.Key != "", Configured: configured(s), Source: s.Source, Status: "unavailable", Probe: s.Probe}
	switch {
	case !v.Configured:
		v.Reason = "not_configured"
	case !s.Enabled:
		v.Reason = "not_enabled"
	case s.Probe == nil:
		v.Reason = "not_probed"
	case s.Probe.Status != "ready":
		v.Reason = s.Probe.Reason
		v.Status = "probe_failed"
	case !now.Before(s.Probe.ValidUntil):
		v.Reason = "probe_expired"
	default:
		v.Status = "ready"
		v.Reason = ""
	}
	return v
}

func (t Target) valid() bool { return t == Teaching || t == Mentor || t == Search }
func (s *stored) slot(t Target) *secretSlot {
	switch t {
	case Teaching:
		return &s.Teaching
	case Mentor:
		return &s.Mentor
	case Search:
		return &s.Search
	}
	panic(fmt.Sprintf("内部配置槽位无效: %s", t))
}
