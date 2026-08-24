package responseproxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const ControlPrefix = "/__fixture"

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Rule struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	OperationID string `json:"operation_id"`
}

type RuleStats struct {
	Rule          Rule `json:"rule"`
	Calls         int  `json:"calls"`
	UpstreamCalls int  `json:"upstream_calls"`
	Drops         int  `json:"drops"`
	Rejections    int  `json:"rejections"`
}

type AuditEntry struct {
	Sequence       uint64 `json:"sequence"`
	Method         string `json:"method"`
	Path           string `json:"path"`
	OperationID    string `json:"operation_id,omitempty"`
	RequestBytes   int    `json:"request_bytes"`
	RequestSHA256  string `json:"request_sha256,omitempty"`
	UpstreamStatus int    `json:"upstream_status,omitempty"`
	Dropped        bool   `json:"dropped"`
	Rejected       bool   `json:"rejected"`
	Error          string `json:"error,omitempty"`
}

type Options struct {
	ControlKey       string
	MaxRequestBytes  int64
	MaxResponseBytes int64
	HTTPClient       *http.Client
}

type ruleKey struct {
	method      string
	path        string
	operationID string
}

type methodPath struct {
	method string
	path   string
}

type ruleState struct {
	mu            sync.Mutex
	rule          Rule
	baselineBody  []byte
	calls         int
	upstreamCalls int
	drops         int
	rejections    int
}

type Proxy struct {
	upstream         *url.URL
	controlKey       string
	maxRequestBytes  int64
	maxResponseBytes int64
	client           *http.Client

	rulesMu    sync.RWMutex
	rules      map[ruleKey]*ruleState
	paths      map[methodPath]int
	generation uint64

	auditMu       sync.Mutex
	auditSequence uint64
	audit         []AuditEntry
}

func New(rawUpstream string, options Options) (*Proxy, error) {
	upstream, err := url.Parse(rawUpstream)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" || upstream.RawQuery != "" || upstream.Fragment != "" || upstream.User != nil {
		return nil, fmt.Errorf("upstream must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if options.ControlKey == "" {
		options.ControlKey = "test-control-key"
	}
	if options.MaxRequestBytes <= 0 {
		options.MaxRequestBytes = 32 << 20
	}
	if options.MaxResponseBytes <= 0 {
		options.MaxResponseBytes = 32 << 20
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Proxy{
		upstream: upstream, controlKey: options.ControlKey,
		maxRequestBytes: options.MaxRequestBytes, maxResponseBytes: options.MaxResponseBytes,
		client: &clone, rules: map[ruleKey]*ruleState{}, paths: map[methodPath]int{}, generation: 1,
	}, nil
}

func (p *Proxy) AddRule(rule Rule) error {
	normalized, err := normalizeRule(rule)
	if err != nil {
		return err
	}
	key := keyFor(normalized)
	pathKey := methodPath{method: normalized.Method, path: normalized.Path}
	p.rulesMu.Lock()
	defer p.rulesMu.Unlock()
	if _, exists := p.rules[key]; exists {
		return fmt.Errorf("response-loss rule already exists")
	}
	p.rules[key] = &ruleState{rule: normalized}
	p.paths[pathKey]++
	return nil
}

func (p *Proxy) Reset() {
	p.rulesMu.Lock()
	defer p.rulesMu.Unlock()
	p.generation++
	p.rules = map[ruleKey]*ruleState{}
	p.paths = map[methodPath]int{}
	p.auditMu.Lock()
	p.audit = nil
	p.auditSequence = 0
	p.auditMu.Unlock()
}

func (p *Proxy) beginRequest() uint64 {
	p.rulesMu.RLock()
	defer p.rulesMu.RUnlock()
	return p.generation
}

func (p *Proxy) Audit() []AuditEntry {
	p.auditMu.Lock()
	defer p.auditMu.Unlock()
	result := make([]AuditEntry, len(p.audit))
	copy(result, p.audit)
	return result
}

func (p *Proxy) Rules() []RuleStats {
	p.rulesMu.RLock()
	states := make([]*ruleState, 0, len(p.rules))
	for _, state := range p.rules {
		states = append(states, state)
	}
	p.rulesMu.RUnlock()
	result := make([]RuleStats, 0, len(states))
	for _, state := range states {
		state.mu.Lock()
		result = append(result, RuleStats{
			Rule: state.rule, Calls: state.calls, UpstreamCalls: state.upstreamCalls,
			Drops: state.drops, Rejections: state.rejections,
		})
		state.mu.Unlock()
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Rule.Method != result[right].Rule.Method {
			return result[left].Rule.Method < result[right].Rule.Method
		}
		if result[left].Rule.Path != result[right].Rule.Path {
			return result[left].Rule.Path < result[right].Rule.Path
		}
		return result[left].Rule.OperationID < result[right].Rule.OperationID
	})
	return result
}

func (p *Proxy) Stats(rule Rule) (RuleStats, bool) {
	normalized, err := normalizeRule(rule)
	if err != nil {
		return RuleStats{}, false
	}
	p.rulesMu.RLock()
	state := p.rules[keyFor(normalized)]
	p.rulesMu.RUnlock()
	if state == nil {
		return RuleStats{}, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return RuleStats{
		Rule: state.rule, Calls: state.calls, UpstreamCalls: state.upstreamCalls,
		Drops: state.drops, Rejections: state.rejections,
	}, true
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, ControlPrefix) {
		p.serveControl(w, r)
		return
	}
	generation := p.beginRequest()
	body, err := readBounded(r.Body, p.maxRequestBytes)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		p.record(generation, auditFor(r, body, "", statusText(status)))
		writeError(w, status, "response_loss_invalid_request")
		return
	}
	method := strings.ToUpper(r.Method)
	pathKey := methodPath{method: method, path: r.URL.Path}
	p.rulesMu.RLock()
	configuredPath := generation == p.generation && p.paths[pathKey] > 0
	p.rulesMu.RUnlock()
	operationID := ""
	if configuredPath {
		mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaErr != nil || mediaType != "application/json" {
			entry := auditFor(r, body, "", "invalid_content_type")
			entry.Rejected = true
			p.record(generation, entry)
			writeError(w, http.StatusBadRequest, "response_loss_invalid_content_type")
			return
		}
		operationID, err = extractOperationID(body)
		if err != nil {
			entry := auditFor(r, body, "", "invalid_operation_id")
			entry.Rejected = true
			p.record(generation, entry)
			writeError(w, http.StatusBadRequest, "response_loss_invalid_operation_id")
			return
		}
	}
	p.rulesMu.RLock()
	var state *ruleState
	if generation == p.generation {
		state = p.rules[ruleKey{method: method, path: r.URL.Path, operationID: operationID}]
	}
	p.rulesMu.RUnlock()
	if state == nil {
		p.forwardUnconfigured(generation, w, r, body, operationID)
		return
	}
	p.forwardConfigured(generation, w, r, body, state)
}

func (p *Proxy) forwardConfigured(generation uint64, w http.ResponseWriter, request *http.Request, body []byte, state *ruleState) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.calls++
	entry := auditFor(request, body, state.rule.OperationID, "")
	if state.baselineBody == nil {
		state.baselineBody = append([]byte(nil), body...)
	} else if !bytes.Equal(state.baselineBody, body) {
		state.rejections++
		entry.Rejected = true
		entry.Error = "body_mismatch"
		p.record(generation, entry)
		writeError(w, http.StatusConflict, "response_loss_replay_body_mismatch")
		return
	}
	response, err := p.doUpstream(request, body)
	state.upstreamCalls++
	if err != nil {
		entry.Error = "upstream_unavailable"
		p.record(generation, entry)
		writeError(w, http.StatusBadGateway, "response_loss_upstream_unavailable")
		return
	}
	entry.UpstreamStatus = response.status
	if state.drops == 0 && response.status >= 200 && response.status < 300 {
		state.drops++
		entry.Dropped = true
		p.record(generation, entry)
		abortConnection(w)
		return
	}
	p.record(generation, entry)
	writeBufferedResponse(w, response)
}

func (p *Proxy) forwardUnconfigured(generation uint64, w http.ResponseWriter, request *http.Request, body []byte, operationID string) {
	entry := auditFor(request, body, operationID, "")
	response, err := p.doUpstream(request, body)
	if err != nil {
		entry.Error = "upstream_unavailable"
		p.record(generation, entry)
		writeError(w, http.StatusBadGateway, "response_loss_upstream_unavailable")
		return
	}
	entry.UpstreamStatus = response.status
	p.record(generation, entry)
	writeBufferedResponse(w, response)
}

type bufferedResponse struct {
	status int
	header http.Header
	body   []byte
}

func (p *Proxy) doUpstream(original *http.Request, body []byte) (bufferedResponse, error) {
	target := *p.upstream
	target.Path = joinURLPath(p.upstream.Path, original.URL.Path)
	target.RawPath = ""
	target.RawQuery = original.URL.RawQuery
	request, err := http.NewRequestWithContext(original.Context(), original.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return bufferedResponse{}, err
	}
	request.Header = original.Header.Clone()
	removeHopByHop(request.Header)
	request.Host = target.Host
	response, err := p.client.Do(request)
	if err != nil {
		return bufferedResponse{}, err
	}
	defer response.Body.Close()
	responseBody, err := readBounded(response.Body, p.maxResponseBytes)
	if err != nil {
		return bufferedResponse{}, err
	}
	header := response.Header.Clone()
	removeHopByHop(header)
	header.Del("Content-Length")
	return bufferedResponse{status: response.StatusCode, header: header, body: responseBody}, nil
}

func writeBufferedResponse(w http.ResponseWriter, response bufferedResponse) {
	for name, values := range response.header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(response.status)
	_, _ = w.Write(response.body)
}

func abortConnection(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		panic(http.ErrAbortHandler)
	}
	connection, _, err := hijacker.Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = connection.Close()
}

func (p *Proxy) record(generation uint64, entry AuditEntry) {
	p.rulesMu.RLock()
	defer p.rulesMu.RUnlock()
	if generation != p.generation {
		return
	}
	p.auditMu.Lock()
	defer p.auditMu.Unlock()
	p.auditSequence++
	entry.Sequence = p.auditSequence
	p.audit = append(p.audit, entry)
}

func auditFor(request *http.Request, body []byte, operationID, category string) AuditEntry {
	entry := AuditEntry{
		Method: strings.ToUpper(request.Method), Path: request.URL.Path,
		OperationID: operationID, RequestBytes: len(body), Error: category,
	}
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		entry.RequestSHA256 = hex.EncodeToString(sum[:])
	}
	return entry
}

func normalizeRule(rule Rule) (Rule, error) {
	rule.Method = strings.ToUpper(strings.TrimSpace(rule.Method))
	rule.Path = strings.TrimSpace(rule.Path)
	rule.OperationID = strings.TrimSpace(rule.OperationID)
	if rule.Method == "" || !strings.HasPrefix(rule.Path, "/") || strings.ContainsAny(rule.Path, "?#") || !canonicalUUID.MatchString(rule.OperationID) {
		return Rule{}, fmt.Errorf("rule requires method, absolute path, and canonical lowercase operation_id")
	}
	return rule, nil
}

func keyFor(rule Rule) ruleKey {
	return ruleKey{method: rule.Method, path: rule.Path, operationID: rule.OperationID}
}

func extractOperationID(body []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return "", fmt.Errorf("request body must be a JSON object")
	}
	seen := map[string]bool{}
	operationID := ""
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return "", fmt.Errorf("duplicate or invalid top-level field")
		}
		seen[name] = true
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return "", err
		}
		if name == "operation_id" {
			if err := json.Unmarshal(raw, &operationID); err != nil || !canonicalUUID.MatchString(operationID) {
				return "", fmt.Errorf("operation_id must be a canonical lowercase UUID")
			}
		}
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return "", fmt.Errorf("invalid JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("multiple JSON values")
	}
	if operationID == "" {
		return "", fmt.Errorf("operation_id is required")
	}
	return operationID, nil
}

func joinURLPath(base, requestPath string) string {
	if base == "" || base == "/" {
		return requestPath
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(requestPath, "/")
}

func removeHopByHop(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
}

var errBodyTooLarge = errors.New("body too large")

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return body[:limit], errBodyTooLarge
	}
	return body, nil
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
}

func statusText(status int) string {
	return strings.ToLower(strings.ReplaceAll(http.StatusText(status), " ", "_"))
}

func (p *Proxy) serveControl(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Fixture-Control-Key") != p.controlKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == ControlPrefix+"/rules" && r.Method == http.MethodPost:
		body, err := readBounded(r.Body, 64<<10)
		if err != nil {
			writeError(w, http.StatusBadRequest, "response_loss_invalid_rule")
			return
		}
		var rule Rule
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&rule); err != nil || decoder.Decode(&struct{}{}) != io.EOF || p.AddRule(rule) != nil {
			writeError(w, http.StatusBadRequest, "response_loss_invalid_rule")
			return
		}
		writeControlJSON(w, http.StatusCreated, rule)
	case r.URL.Path == ControlPrefix+"/rules" && r.Method == http.MethodGet:
		writeControlJSON(w, http.StatusOK, map[string]any{"rules": p.Rules()})
	case r.URL.Path == ControlPrefix+"/audit" && r.Method == http.MethodGet:
		writeControlJSON(w, http.StatusOK, map[string]any{"audit": p.Audit()})
	case r.URL.Path == ControlPrefix+"/reset" && r.Method == http.MethodPost:
		p.Reset()
		writeControlJSON(w, http.StatusOK, map[string]string{"status": "reset"})
	default:
		http.NotFound(w, r)
	}
}

func writeControlJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

var _ http.Handler = (*Proxy)(nil)
