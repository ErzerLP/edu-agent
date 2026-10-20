// Package modelgateway is the CLI-only, device-authenticated model gateway.
package modelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/id"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

const PrivacyVersion = 1
const PrivacyNotice = "问题、相关对话和工具返回的本地内容会经过此服务端，再发送给你选择的模型服务。服务端不保存这些正文，只记录用量和设备归属。"
const DestinationNotice = "所选模型服务已更改，继续会把相关对话发送给更改后的服务"

type Connection struct {
	Target                string `json:"target"`
	DisplayName           string `json:"display_name"`
	ModelName             string `json:"model_name"`
	ConfigurationRevision string `json:"configuration_revision"`
	DestinationRevision   string `json:"destination_revision"`
	ContextTokens         int    `json:"context_tokens"`
	OutputTokens          int    `json:"output_tokens"`
}
type Configuration struct {
	ProtocolVersion   int          `json:"protocol_version"`
	InvokeAllowed     bool         `json:"invoke_allowed"`
	Available         bool         `json:"available"`
	UnavailableReason string       `json:"unavailable_reason"`
	PrivacyGeneration int64        `json:"privacy_generation"`
	Models            []Connection `json:"models"`
	Limits            struct {
		IdleTimeoutSeconds    int `json:"idle_timeout_seconds"`
		RequestTimeoutSeconds int `json:"request_timeout_seconds"`
	} `json:"limits"`
}

func (c Configuration) Find(target string) (Connection, bool) {
	for _, m := range c.Models {
		if m.Target == target {
			return m, true
		}
	}
	return Connection{}, false
}
func (c Configuration) Ready() error {
	if !c.InvokeAllowed {
		return ErrorFor("model_scope_required")
	}
	if !c.Available {
		if c.UnavailableReason == "gateway_capacity_disabled" {
			return ErrorFor(c.UnavailableReason)
		}
		return ErrorFor("no_selectable_model")
	}
	return nil
}

type Options struct {
	ServerURL, DeviceID, Token string
	AllowInsecureHTTP          bool
	Transport                  http.RoundTripper
	Select                     func(context.Context, Configuration) (Connection, error)
}
type Client struct {
	generation atomic.Int64
	options    Options
	http       *http.Client
	gate       chan struct{}
}
type Identity struct{ SessionID, TurnID, SpaceID string }
type Model struct {
	lifetime      context.Context
	stop          context.CancelFunc
	client        *Client
	Configuration Configuration
	Selected      Connection
	Identity      Identity
}

var schedulers struct {
	sync.Mutex
	gates map[string]chan struct{}
}

func New(options Options) (*Client, error) {
	server, err := config.ValidateServerURL(options.ServerURL, options.AllowInsecureHTTP)
	if err != nil {
		return nil, ErrorFor("gateway_connection_invalid")
	}
	if options.DeviceID == "" || strings.TrimSpace(options.Token) == "" {
		return nil, ErrorFor("device_unauthorized")
	}
	options.ServerURL = server
	transport := options.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	schedulers.Lock()
	defer schedulers.Unlock()
	if schedulers.gates == nil {
		schedulers.gates = make(map[string]chan struct{})
	}
	key := server + "\x00" + options.DeviceID
	gate := schedulers.gates[key]
	if gate == nil {
		gate = make(chan struct{}, 1)
		schedulers.gates[key] = gate
	}
	return &Client{options: options, gate: gate, http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Config(ctx context.Context) (Configuration, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.options.ServerURL+"/v1/model-gateway/config", nil)
	if err != nil {
		return Configuration{}, ErrorFor("gateway_connection_invalid")
	}
	req.Header.Set("Authorization", "Bearer "+c.options.Token)
	req.Header.Set("X-Model-Gateway-Version", "1")
	if generation := c.generation.Load(); generation > 0 {
		req.Header.Set("X-Learning-Generation", strconv.FormatInt(generation, 10))
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Configuration{}, ctx.Err()
		}
		return Configuration{}, ErrorFor("gateway_unavailable")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Configuration{}, ErrorFor("gateway_unavailable")
	}
	if response.StatusCode != 200 || isHTML(response.Header, data) {
		err := responseError(response.StatusCode, response.Header, data)
		var gateway *Error
		if errors.As(err, &gateway) && gateway.Code == "privacy_generation_changed" {
			c.generation.Store(0)
		}
		return Configuration{}, err
	}
	var value Configuration
	if json.Unmarshal(data, &value) != nil {
		return value, ErrorFor("gateway_connection_invalid")
	}
	if value.ProtocolVersion != 1 {
		return value, ErrorFor("gateway_version_unsupported")
	}
	if value.PrivacyGeneration <= 0 || value.Limits.IdleTimeoutSeconds <= 0 || value.Limits.IdleTimeoutSeconds > 600 || value.Limits.RequestTimeoutSeconds <= 0 || value.Limits.RequestTimeoutSeconds > int((1<<63-1)/int64(time.Second)) {
		return value, ErrorFor("gateway_connection_invalid")
	}
	seen := make(map[string]bool)
	for _, m := range value.Models {
		if (m.Target != "teaching" && m.Target != "mentor") || seen[m.Target] || !safeHeader(m.ConfigurationRevision) || !safeHeader(m.DestinationRevision) || m.ModelName == "" || m.ContextTokens < 4096 || m.OutputTokens <= 0 {
			return value, ErrorFor("gateway_connection_invalid")
		}
		seen[m.Target] = true
	}
	c.generation.Store(value.PrivacyGeneration)
	return value, nil
}
func safeHeader(s string) bool {
	return s != "" && len(s) <= 512 && !strings.ContainsAny(s, "\r\n\x00")
}
func (c *Client) Bind(ctx context.Context, identity Identity) (*Model, error) {
	cfg, err := c.Config(ctx)
	if err != nil {
		return nil, err
	}
	if err = cfg.Ready(); err != nil {
		return nil, err
	}
	if c.options.Select == nil {
		return nil, ErrorFor("model_selection_required")
	}
	selected, err := c.options.Select(ctx, cfg)
	if err != nil {
		return nil, err
	}
	actual, ok := cfg.Find(selected.Target)
	if !ok || actual != selected {
		return nil, ErrorFor("model_unavailable")
	}
	return c.Model(cfg, selected, identity), nil
}
func (m *Model) Prepare(ctx context.Context, identity Identity) (*Model, error) {
	m.stop()
	return m.client.Bind(ctx, identity)
}
func (m *Model) ServerURL() string { return m.client.options.ServerURL }
func (m *Model) DeviceID() string  { return m.client.options.DeviceID }

type stageKey struct{}

func WithStage(ctx context.Context, stage string) context.Context {
	return context.WithValue(ctx, stageKey{}, stage)
}
func stageOf(ctx context.Context) (string, error) {
	s, _ := ctx.Value(stageKey{}).(string)
	if s == "" {
		s = "foreground"
	}
	switch s {
	case "foreground", "context_observer", "context_reflector", "tool_limit_summary", "session_title", "probe":
		return s, nil
	}
	return "", ErrorFor("invalid_request")
}
func (m *Model) Complete(ctx context.Context, request modelclient.Request) (modelclient.Response, error) {
	return m.invoke(ctx, request, nil)
}
func (m *Model) Stream(ctx context.Context, request modelclient.Request, observe func(modelclient.StreamEvent) error) (modelclient.Response, error) {
	return m.invoke(ctx, request, observe)
}
func (m *Model) invoke(ctx context.Context, request modelclient.Request, observe func(modelclient.StreamEvent) error) (modelclient.Response, error) {
	operationCtx, cancelOperation := context.WithCancel(ctx)
	stopWatch := context.AfterFunc(m.lifetime, cancelOperation)
	defer stopWatch()
	defer cancelOperation()
	if m.lifetime.Err() != nil {
		return modelclient.Response{}, context.Canceled
	}
	ctx = operationCtx
	stage, err := stageOf(ctx)
	if err != nil {
		return modelclient.Response{}, err
	}
	if m.Identity.SessionID == "" || m.Identity.TurnID == "" {
		return modelclient.Response{}, ErrorFor("invalid_request")
	}
	select {
	case m.client.gate <- struct{}{}:
	case <-ctx.Done():
		return modelclient.Response{}, ctx.Err()
	}
	defer func() { <-m.client.gate }()
	if err := ctx.Err(); err != nil {
		return modelclient.Response{}, err
	}
	requestID, err := id.NewUUID()
	if err != nil {
		return modelclient.Response{}, err
	}
	if request.MaxTokens == 0 || stage != "foreground" && request.MaxTokens > m.Selected.OutputTokens {
		request.MaxTokens = m.Selected.OutputTokens
	}
	if request.MaxTokens < 0 || request.MaxTokens > m.Selected.OutputTokens {
		return modelclient.Response{}, ErrorFor("output_limit_exceeded")
	}
	overall := time.Duration(m.Configuration.Limits.RequestTimeoutSeconds) * time.Second
	requestCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()
	call := &callTransport{model: m, requestID: requestID, stage: stage}
	timeout := overall
	if observe != nil {
		timeout = time.Duration(m.Configuration.Limits.IdleTimeoutSeconds+5) * time.Second
	}
	core, err := modelclient.New(m.client.options.ServerURL+"/v1/model-gateway", m.Selected.Target, "", timeout, &http.Client{Transport: call})
	if err != nil {
		return modelclient.Response{}, err
	}
	var response modelclient.Response
	if observe == nil {
		response, err = core.Complete(requestCtx, request)
	} else {
		response, err = core.Stream(requestCtx, request, observe)
	}
	if call.partialUsage {
		response.Usage = nil
	}
	if call.err != nil {
		err = call.err
	}
	if err != nil && call.sent && (ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		m.cancel(requestID)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if requestCtx.Err() != nil {
			err = ErrorFor("model_request_timeout")
		} else {
			err = ErrorFor("model_idle_timeout")
		}
	}
	if err != nil {
		if call.sent {
			return modelclient.Response{}, &RequestError{RequestID: requestID, Err: err}
		}
		return modelclient.Response{}, err
	}
	return response, nil
}
func (m *Model) headers(h http.Header, requestID, stage string) {
	h.Set("Authorization", "Bearer "+m.client.options.Token)
	h.Set("X-Model-Gateway-Version", "1")
	h.Set("X-Model-Configuration", m.Selected.ConfigurationRevision)
	h.Set("X-Learning-Generation", strconv.FormatInt(m.Configuration.PrivacyGeneration, 10))
	h.Set("X-CLI-Session-ID", m.Identity.SessionID)
	h.Set("X-CLI-Turn-ID", m.Identity.TurnID)
	h.Set("X-CLI-Request-ID", requestID)
	h.Set("X-CLI-Model-Stage", stage)
	if m.Identity.SpaceID != "" {
		h.Set("X-Learning-Space-ID", m.Identity.SpaceID)
	}
}
func (m *Model) cancel(requestID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.client.options.ServerURL+"/v1/model-gateway/requests/"+requestID+"/cancel", strings.NewReader("{}"))
	if err != nil {
		return
	}
	m.headers(req.Header, requestID, "foreground")
	req.Header.Set("Content-Type", "application/json")
	response, err := m.client.http.Do(req)
	if err == nil {
		response.Body.Close()
	}
}

func (c *Client) Selection(ctx context.Context, cfg Configuration) (Connection, error) {
	return c.options.Select(ctx, cfg)
}
func (c *Client) Model(cfg Configuration, selected Connection, identity Identity) *Model {
	life, stop := context.WithCancel(context.Background())
	return &Model{client: c, Configuration: cfg, Selected: selected, Identity: identity, lifetime: life, stop: stop}
}
