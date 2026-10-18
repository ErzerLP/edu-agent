package settings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/modelusage"
)

type Options struct {
	ModelUsage     *modelusage.Store
	Path           string
	ModelEndpoints []string
	Teaching       Connection
	TeachingKey    string
	Limits         *Limits
	Now            func() time.Time
}

type Service struct {
	usage          *modelusage.Store
	mu             sync.Mutex
	path           string
	modelEndpoints []string
	state          stored
	activeTeaching secretSlot
	activeLimits   Limits
	now            func() time.Time
	probing        bool
}

func Open(o Options) (*Service, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Service{usage: o.ModelUsage, path: o.Path, modelEndpoints: append([]string{}, o.ModelEndpoints...), now: o.Now}
	for _, endpoint := range s.modelEndpoints {
		if _, err := endpointURL(endpoint); err != nil {
			return nil, ErrInvalid
		}
	}
	initial := secretSlot{Connection: o.Teaching, Key: o.TeachingKey, Source: "environment"}
	if initial.Provider == "" {
		initial.Provider = "openai_compatible"
		initial.AuthMode = "bearer"
	}
	if initial.Endpoint != "" && !slices.Contains(s.modelEndpoints, initial.Endpoint) {
		s.modelEndpoints = append(s.modelEndpoints, initial.Endpoint)
	}
	s.state = stored{Version: 1, Teaching: initial, Mentor: secretSlot{Connection: Connection{Provider: "openai_compatible", AuthMode: "bearer"}, Source: "server"}, Search: secretSlot{Connection: Connection{Provider: "brave", AuthMode: "bearer"}, Source: "server"}, Limits: DefaultLimits()}
	if o.Limits != nil {
		s.state.Limits = *o.Limits
	}
	value, exists, err := loadFile(o.Path)
	if err != nil {
		return nil, err
	}
	if exists {
		s.state = value
		if s.state.Teaching.Source == "environment" {
			s.state.Teaching = initial
		}
	}
	if s.state.Limits.Validate() != nil {
		return nil, ErrInvalid
	}
	for _, target := range []Target{Teaching, Mentor, Search} {
		if s.state.slot(target).APIStyle == "" {
			s.state.slot(target).APIStyle = string(llm.ChatCompletions)
		}
		if err := s.validate(target, *s.state.slot(target)); err != nil {
			return nil, err
		}
	}
	s.activeTeaching = s.state.Teaching
	s.activeLimits = s.state.Limits
	return s, nil
}

func (s *Service) validate(target Target, slot secretSlot) error {
	if slot.APIStyle != string(llm.ChatCompletions) && slot.APIStyle != string(llm.Responses) {
		return ErrInvalid
	}
	// Responses is a teaching-client option; mentor execution still uses agentcore Chat Completions.
	if target != Teaching && slot.APIStyle != string(llm.ChatCompletions) {
		return ErrInvalid
	}
	if !validKey(slot.Key) || len(slot.Model) > 200 || strings.TrimSpace(slot.Model) != slot.Model || strings.ContainsAny(slot.Model, "\r\n\t") || (slot.AuthMode != "bearer" && slot.AuthMode != "none") {
		return ErrInvalid
	}
	if target == Search {
		if slot.Provider != "brave" || slot.Model != "" || slot.AuthMode != "bearer" {
			return ErrInvalid
		}
	} else if slot.Provider != "openai_compatible" {
		return ErrInvalid
	}
	if slot.AuthMode == "none" && slot.Key != "" {
		return ErrInvalid
	}
	if slot.Key != "" && (strings.Contains(slot.Endpoint, slot.Key) || strings.Contains(slot.Model, slot.Key)) {
		return ErrInvalid
	}
	if slot.Endpoint == "" {
		return nil
	}
	u, err := endpointURL(slot.Endpoint)
	if err != nil {
		return err
	}
	if target == Search {
		if u.Scheme != "https" || !publicHost(u.Hostname()) {
			return ErrInvalid
		}
	} else if !slices.Contains(s.modelEndpoints, slot.Endpoint) {
		return ErrInvalid
	}
	return nil
}

func (s *Service) View() View { s.mu.Lock(); defer s.mu.Unlock(); return s.view() }
func (s *Service) view() View {
	now := s.now().UTC()
	mentor := s.state.Mentor
	if s.state.MentorUsesTeaching {
		mentor = s.state.Teaching
		mentor.APIStyle = string(llm.ChatCompletions)
		mentor.Probe = s.state.Mentor.Probe
	}
	return View{Revision: s.state.Revision, Writable: s.path != "", Teaching: connectionView(s.state.Teaching, now), Mentor: connectionView(s.state.Mentor, now), EffectiveMentor: connectionView(mentor, now), Search: connectionView(s.state.Search, now), MentorUsesTeaching: s.state.MentorUsesTeaching, Limits: s.state.Limits, Policy: s.state.Policy, TeachingRestartRequired: !sameConnection(s.activeTeaching, s.state.Teaching) || s.activeLimits != s.state.Limits, ModelEndpoints: append([]string{}, s.modelEndpoints...)}
}
func sameConnection(a, b secretSlot) bool { return a.Connection == b.Connection && a.Key == b.Key }

func (s *Service) Update(u Update) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return View{}, ErrStorage
	}
	if u.ExpectedRevision != s.state.Revision {
		return View{}, ErrConflict
	}
	if u.NewKey != nil && u.ClearKey {
		return View{}, ErrInvalid
	}
	if u.Target != "" && !u.Target.valid() {
		return View{}, ErrInvalid
	}
	if u.Target == "" && (u.Connection != nil || u.NewKey != nil || u.ClearKey) {
		return View{}, ErrInvalid
	}
	if u.Connection == nil && u.NewKey == nil && !u.ClearKey && u.MentorUsesTeaching == nil && u.Limits == nil && u.Policy == nil {
		return View{}, ErrInvalid
	}
	next := s.state
	if u.Target.valid() {
		slot := next.slot(u.Target)
		if u.Connection != nil {
			connection := *u.Connection
			if connection.APIStyle == "" {
				connection.APIStyle = string(llm.ChatCompletions)
			}
			if slot.Provider != connection.Provider || slot.Endpoint != connection.Endpoint || slot.AuthMode != connection.AuthMode || slot.APIStyle != connection.APIStyle {
				slot.Key = ""
			}
			slot.Connection = connection
		}
		if u.NewKey != nil {
			if *u.NewKey == "" {
				return View{}, ErrInvalid
			}
			slot.Key = *u.NewKey
		}
		if u.ClearKey {
			slot.Key = ""
		}
		slot.Source = "server"
		slot.Probe = nil
		if err := s.validate(u.Target, *slot); err != nil {
			return View{}, err
		}
		if u.Target == Teaching && next.MentorUsesTeaching {
			next.Mentor.Probe = nil
		}
	}
	if u.MentorUsesTeaching != nil {
		next.MentorUsesTeaching = *u.MentorUsesTeaching
		next.Mentor.Probe = nil
	}
	if u.Limits != nil {
		if u.Limits.Validate() != nil {
			return View{}, ErrInvalid
		}
		next.Limits = *u.Limits
		next.Teaching.Probe = nil
		next.Mentor.Probe = nil
		next.Search.Probe = nil
	}
	if u.Policy != nil {
		next.Policy = *u.Policy
	}
	// 任何槽位的新公开字段都不能携带已保存的 Key。
	for _, key := range []string{s.state.Teaching.Key, s.state.Mentor.Key, s.state.Search.Key, next.Teaching.Key, next.Mentor.Key, next.Search.Key} {
		if key == "" {
			continue
		}
		for _, target := range []Target{Teaching, Mentor, Search} {
			v := next.slot(target)
			if strings.Contains(v.Endpoint, key) || strings.Contains(v.Model, key) {
				return View{}, ErrInvalid
			}
		}
		for _, endpoint := range s.modelEndpoints {
			if strings.Contains(endpoint, key) {
				return View{}, ErrInvalid
			}
		}
	}
	next.Revision++
	if err := saveFile(s.path, next); err != nil {
		return View{}, err
	}
	s.state = next
	return s.view(), nil
}

func (s *Service) Capabilities() Capabilities {
	v := s.View()
	search := Capability{Available: v.Search.Status == "ready", Reason: v.Search.Reason}
	if v.Search.Status == "probe_failed" {
		search.Reason = "probe_failed:" + v.Search.Reason
	}
	model := v.Teaching
	schema := Capability{Reason: model.Reason}
	if model.Status == "ready" && model.Probe != nil {
		schema = Capability{Available: model.Probe.StructuredJSON}
	}
	return Capabilities{SchemaVersion: 1, CheckedAt: s.now().UTC(), Schema: schema, Rendering: Capability{Available: true}, Answering: Capability{Reason: "not_implemented"}, Search: search, Persistence: Capability{Available: true}, Research: Capability{Reason: "not_implemented"}, WebMentor: Capability{Reason: "not_implemented"}}
}

// 探测只接受目标和确认，不接受学习正文。网络请求期间不占用配置锁。
func (s *Service) Probe(ctx context.Context, target Target, revision int64, consent bool) (View, error) {
	if !consent {
		return View{}, ErrConsent
	}
	if !target.valid() {
		return View{}, ErrInvalid
	}
	s.mu.Lock()
	if s.path == "" {
		s.mu.Unlock()
		return View{}, ErrStorage
	}
	if revision != s.state.Revision {
		s.mu.Unlock()
		return View{}, ErrConflict
	}
	if s.probing {
		s.mu.Unlock()
		return View{}, ErrBusy
	}
	slot := *s.state.slot(target)
	if target == Mentor && s.state.MentorUsesTeaching {
		slot = s.state.Teaching
		slot.APIStyle = string(llm.ChatCompletions)
	}
	limits := s.state.Limits
	s.probing = true
	s.mu.Unlock()
	probe := Probe{Status: "failed", CostEstimate: "unknown"}
	switch {
	case !configured(slot):
		probe.Reason = "not_configured"
	case !slot.Enabled:
		probe.Reason = "not_enabled"
	default:
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		probe = s.probeConnection(bounded, target, slot, limits)
		cancel()
	}
	probe.CheckedAt = s.now().UTC()
	probe.ValidUntil = probe.CheckedAt.Add(15 * time.Minute)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probing = false
	if s.state.Revision != revision {
		return View{}, ErrConflict
	}
	next := s.state
	next.slot(target).Probe = &probe
	if err := saveFile(s.path, next); err != nil {
		return View{}, err
	}
	s.state = next
	return s.view(), nil
}

// TeachingClient 是重启时冻结的执行配置；保存不会隐式切换正在运行的教学。
func (s *Service) TeachingClient() (*llm.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.activeTeaching.Enabled || !configured(s.activeTeaching) {
		return nil, nil
	}
	return modelClient(s.activeTeaching, s.activeLimits, false, s.usageTransport(s.activeTeaching, s.activeLimits, modelTransport()))
}

func (s *Service) usageTransport(slot secretSlot, limits Limits, next http.RoundTripper) http.RoundTripper {
	if s.usage == nil {
		return next
	}
	destination := sha256.Sum256([]byte(slot.Provider + "\n" + slot.Endpoint))
	raw, _ := json.Marshal(struct {
		Connection Connection
		Key        string
		Limits     Limits
	}{slot.Connection, slot.Key, limits})
	configuration := sha256.Sum256(raw)
	return s.usage.Transport(next, modelusage.Model{Provider: slot.Provider, Destination: hex.EncodeToString(destination[:]), Model: slot.Model, APIStyle: slot.APIStyle}, hex.EncodeToString(configuration[:]))
}

// MentorClient 每次承领和付费调用前重新读取配置；返回的摘要绑定端点及预算，不暴露秘密。
func (s *Service) MentorClient(wrappers ...func(http.RoundTripper) http.RoundTripper) (*modelclient.Client, Limits, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mentorClient(nil, wrappers...)
}

// MentorUsageClient freezes the accounting identity with the actual client.
// The shared agentcore protocol is unchanged.
func (s *Service) MentorUsageClient(wrap func(Connection, http.RoundTripper) http.RoundTripper) (*modelclient.Client, Limits, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mentorClient(wrap)
}

func (s *Service) mentorClient(observe func(Connection, http.RoundTripper) http.RoundTripper, wrappers ...func(http.RoundTripper) http.RoundTripper) (*modelclient.Client, Limits, string, error) {
	slot := s.state.Mentor
	if s.state.MentorUsesTeaching {
		slot = s.state.Teaching
	}
	limits := s.state.Limits
	if !slot.Enabled || !configured(slot) {
		return nil, limits, "", nil
	}
	connection := slot.Connection
	// Preserve pre-api_style fingerprints for unchanged Chat Completions connections.
	if connection.APIStyle == string(llm.ChatCompletions) {
		connection.APIStyle = ""
	}
	raw, err := json.Marshal(struct {
		Connection Connection
		Key        string
		Limits     Limits
	}{connection, slot.Key, limits})
	if err != nil {
		return nil, limits, "", ErrInvalid
	}
	digest := sha256.Sum256(raw)
	var transport http.RoundTripper = modelTransport()
	for _, wrap := range wrappers {
		transport = wrap(transport)
	}
	if observe != nil {
		transport = observe(slot.Connection, transport)
	}
	client, err := modelclient.New(slot.Endpoint, slot.Model, slot.Key, time.Duration(limits.IdleTimeoutSeconds)*time.Second, &http.Client{Transport: transport, CheckRedirect: rejectRedirect})
	return client, limits, hex.EncodeToString(digest[:]), err
}

func modelClient(slot secretSlot, limits Limits, probe bool, transport http.RoundTripper) (*llm.Client, error) {
	u, _ := url.Parse(slot.Endpoint)
	output := limits.OutputTokens
	if probe && slot.APIStyle != string(llm.Responses) {
		output = 64
	}
	return llm.New(llm.Options{BaseURL: u, WireAPI: llm.WireAPI(slot.APIStyle), Model: slot.Model, APIKey: slot.Key, AllowNoKey: slot.AuthMode == "none", ContextWindow: limits.ContextTokens, MinimumContext: 4096, Timeout: time.Duration(limits.IdleTimeoutSeconds) * time.Second, MaxOutputTokens: output, HTTPClient: &http.Client{Transport: transport, CheckRedirect: rejectRedirect}})
}

// 保存的探测记录只供状态查询，readiness 不再触发外部请求。
func (s *Service) TeachingHealth(context.Context) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !sameConnection(s.activeTeaching, s.state.Teaching) || s.activeLimits != s.state.Limits {
		return false, "restart_required"
	}
	v := connectionView(s.state.Teaching, s.now())
	return v.Status == "ready", v.Reason
}
