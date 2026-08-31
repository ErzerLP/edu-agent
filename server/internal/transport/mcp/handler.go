package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/transport/access"
	"github.com/edu-agent/edu-agent/server/internal/transport/mcpadmin"
	"github.com/edu-agent/edu-agent/server/internal/transport/problem"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const DefaultMaxRequestBodyBytes = int64(1 << 20)

type IdentityService interface {
	Authenticate(context.Context, string, string) (identity.Credential, error)
}

type KnowledgeService interface {
	Head(context.Context) (*knowledge.KnowledgeRevision, error)
	Tree(context.Context, string) (knowledge.TreeResult, error)
	Export(context.Context, string) (knowledge.ExportResult, error)
	Retrieve(context.Context, knowledge.RetrievalCommand) (knowledge.RetrievalResult, error)
	Create(context.Context, knowledge.CreateProposalCommand) (knowledge.Proposal, error)
	List(context.Context, knowledge.ProposalListCommand) (knowledge.ProposalPage, error)
	Get(context.Context, string) (knowledge.Proposal, error)
}

type LearningService interface {
	CreateGoal(context.Context, string, learning.GoalCommand) (learning.OperationResult, error)
	CreateSession(context.Context, string, learning.SessionCommand) (learning.OperationResult, error)
	Propose(context.Context, string, learning.ProposalRequest) (learning.ProposalArtifact, error)
	ApplyAction(context.Context, string, string, learning.ActionCommand) (learning.OperationResult, error)
	CurrentSession(context.Context) (learning.SessionView, error)
	Session(context.Context, string) (learning.SessionView, error)
	Timeline(context.Context, learning.TimelineQuery) (learning.TimelinePage, error)
	Routes(context.Context, learning.CursorPageRequest) (learning.RoutesPage, error)
	Node(context.Context, string) (learning.NodeView, error)
	Evidence(context.Context, learning.EvidenceQuery) (learning.EvidencePage, error)
	Reviews(context.Context, learning.ReviewQuery) (learning.ReviewsPage, error)
	ProjectionStatus(context.Context) (learning.ProjectionStatus, error)
	ListEvidenceCarryovers(context.Context, learning.EvidenceCarryoverListCommand) (learning.EvidenceCarryoverPage, error)
	GetEvidenceCarryover(context.Context, string) (learning.EvidenceCarryoverProposal, error)
}

type MemoryService interface {
	ListRecords(context.Context, memory.PageRequest) (memory.RecordPage, error)
}

type MemoryExporter interface {
	Detail(context.Context, string) (memory.RecordDetail, error)
	Export(context.Context, memory.PageRequest) (memory.ExportPage, error)
}

type Options struct {
	Identity       IdentityService
	Knowledge      KnowledgeService
	Learning       LearningService
	Memory         MemoryService
	MemoryExporter MemoryExporter
	ReadPermits    *privacy.ReadPermitManager
	Logger         *slog.Logger
	AuthLimiter    access.Limiter
	DeviceLimiter  access.Limiter
	MaxRequestBody int64

	beforeResponseWrite func(context.Context)
}

type Handler struct {
	identity            IdentityService
	readPermits         *privacy.ReadPermitManager
	logger              *slog.Logger
	authLimiter         access.Limiter
	deviceLimiter       access.Limiter
	maxRequestBody      int64
	originProtection    *http.CrossOriginProtection
	sdk                 http.Handler
	beforeResponseWrite func(context.Context)
	recentMu            sync.Mutex
	recentInvocations   []mcpadmin.Invocation
}

type invocationContextKey struct{}

type invocationContext struct {
	Credential identity.Credential
	Descriptor Descriptor
	RequestID  string
	State      *invocationState
}

type invocationState struct {
	mu        sync.Mutex
	result    string
	errorCode string
}

func (s *invocationState) finish(result, errorCode string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.result, s.errorCode = result, errorCode
	s.mu.Unlock()
}

func (s *invocationState) values() (string, string) {
	if s == nil {
		return "", ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.errorCode
}

func invocationFromContext(ctx context.Context) (invocationContext, bool) {
	value, ok := ctx.Value(invocationContextKey{}).(invocationContext)
	return value, ok
}

func New(options Options) (*Handler, error) {
	if options.Identity == nil || options.Knowledge == nil || options.Learning == nil || options.Memory == nil || options.MemoryExporter == nil || options.Logger == nil || options.AuthLimiter == nil || options.DeviceLimiter == nil {
		return nil, errors.New("MCP transport dependencies are required")
	}
	if options.ReadPermits == nil {
		options.ReadPermits = privacy.DefaultReadPermits
	}
	if options.MaxRequestBody <= 0 {
		options.MaxRequestBody = DefaultMaxRequestBodyBytes
	}
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: implementationName, Version: implementationVersion}, &sdkmcp.ServerOptions{
		Capabilities: &sdkmcp.ServerCapabilities{
			Resources: &sdkmcp.ResourceCapabilities{}, Tools: &sdkmcp.ToolCapabilities{},
		},
	})
	runtime := callbackRuntime{
		knowledge: options.Knowledge, learning: options.Learning,
		memory: options.Memory, memoryExporter: options.MemoryExporter,
	}
	for _, descriptor := range descriptorCatalog {
		descriptor := descriptor
		switch descriptor.Kind {
		case DescriptorResource:
			server.AddResource(resourceDefinition(descriptor), func(ctx context.Context, request *sdkmcp.ReadResourceRequest) (*sdkmcp.ReadResourceResult, error) {
				return runtime.readResource(ctx, request, descriptor)
			})
		case DescriptorResourceTemplate:
			server.AddResourceTemplate(resourceTemplateDefinition(descriptor), func(ctx context.Context, request *sdkmcp.ReadResourceRequest) (*sdkmcp.ReadResourceResult, error) {
				return runtime.readResource(ctx, request, descriptor)
			})
		case DescriptorTool:
			server.AddTool(toolDefinition(descriptor), func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
				return runtime.callTool(ctx, request, descriptor)
			})
		default:
			return nil, fmt.Errorf("unsupported MCP descriptor kind %q", descriptor.Kind)
		}
	}
	sdkHandler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, PropagateRequestCancellation: true,
		MaxRequestBodyBytes: options.MaxRequestBody,
	})
	return &Handler{
		identity: options.Identity, readPermits: options.ReadPermits, logger: options.Logger,
		authLimiter: options.AuthLimiter, deviceLimiter: options.DeviceLimiter,
		maxRequestBody: options.MaxRequestBody, originProtection: http.NewCrossOriginProtection(),
		sdk: sdkHandler, beforeResponseWrite: options.beforeResponseWrite,
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := middleware.GetReqID(r.Context())
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Request-Id", requestID)
	descriptorName := ""
	deviceID := ""
	result := "rejected"
	errorCode := ""
	defer func() {
		duration := time.Since(started)
		peer := access.ClientIP(r)
		h.recordInvocation(mcpadmin.Invocation{
			CompletedAt: time.Now().UTC(), RequestID: requestID, Descriptor: descriptorName,
			DeviceID: deviceID, Result: result, ErrorCode: errorCode,
			DurationMS: duration.Milliseconds(), Peer: peer,
		})
		h.logger.InfoContext(r.Context(), "mcp_invocation",
			"request_id", requestID, "transport", "mcp", "descriptor", descriptorName,
			"device_id", deviceID, "result", result, "error_code", errorCode,
			"duration_ms", duration.Milliseconds(), "peer", peer)
	}()

	if !validRequestHost(r) {
		errorCode = "forbidden"
		writeGatewayProblem(w, requestID, problem.Problem{Status: http.StatusForbidden, Code: "forbidden", Message: "MCP Host header is not allowed"})
		return
	}
	if err := h.originProtection.Check(r); err != nil {
		errorCode = "forbidden"
		writeGatewayProblem(w, requestID, problem.Problem{Status: http.StatusForbidden, Code: "forbidden", Message: "Cross-origin MCP requests are not allowed"})
		return
	}

	var body []byte
	var inspected inspectedRequest
	if r.Method == http.MethodPost {
		var err error
		body, err = readBoundedBody(r.Body, h.maxRequestBody)
		if err != nil {
			mapped := problem.InvalidRequest("MCP request body is invalid")
			if errors.Is(err, errBodyTooLarge) {
				mapped = problem.PayloadTooLarge("MCP request body exceeds the configured limit")
			}
			errorCode = mapped.Code
			writeGatewayProblem(w, requestID, mapped)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		inspected, err = inspectRequest(body)
		if err != nil {
			mapped := problem.InvalidRequest("MCP request is invalid")
			if errors.Is(err, errBodyTooLarge) {
				mapped = problem.PayloadTooLarge("MCP descriptor arguments exceed the configured limit")
			} else if errors.Is(err, errDescriptorNotFound) {
				mapped = problem.DescriptorNotFound()
			}
			errorCode = mapped.Code
			writeGatewayProblem(w, requestID, mapped)
			return
		}
		descriptorName = inspected.auditDescriptor()
	} else {
		descriptorName = "http." + strings.ToLower(r.Method)
	}

	failureKey := "auth:" + access.ClientIP(r)
	if h.authLimiter.Limited(failureKey) {
		mapped := problem.RateLimited("Too many authentication failures")
		errorCode = mapped.Code
		writeGatewayProblem(w, requestID, mapped)
		return
	}
	token, ok := access.BearerToken(r.Header.Get("Authorization"))
	if !ok {
		mapped := h.authenticationFailed(failureKey)
		errorCode = mapped.Code
		writeGatewayProblem(w, requestID, mapped)
		return
	}
	credential, err := h.identity.Authenticate(r.Context(), token, "")
	if err != nil {
		if errors.Is(err, identity.ErrUnauthenticated) {
			mapped := h.authenticationFailed(failureKey)
			errorCode = mapped.Code
			writeGatewayProblem(w, requestID, mapped)
			return
		}
		h.logger.ErrorContext(r.Context(), "MCP device authentication failed", "request_id", requestID, "error_category", "internal")
		mapped := problem.Internal()
		errorCode = mapped.Code
		writeGatewayProblem(w, requestID, mapped)
		return
	}
	deviceID = credential.Device.ID
	if !h.deviceLimiter.Allow("device:" + deviceID) {
		mapped := problem.RateLimited("Device request rate exceeded")
		errorCode = mapped.Code
		writeGatewayProblem(w, requestID, mapped)
		return
	}

	if inspected.Descriptor != nil && !access.ContainsScope(credential.Scopes, inspected.Descriptor.RequiredScope) {
		mapped := problem.Forbidden()
		errorCode = mapped.Code
		writeGatewayProblem(w, requestID, mapped)
		return
	}

	state := &invocationState{}
	requestContext := r.Context()
	var permit *privacy.ReadPermit
	if inspected.Descriptor != nil {
		permit, err = h.readPermits.Acquire(requestContext, inspected.Descriptor.PrivacyOwners...)
		if err != nil {
			mapped := problem.Privacy(err)
			if privacy.ErrorCode(err) == privacy.CodeContentRedacted {
				mapped = problem.ContentRedacted()
			}
			errorCode = mapped.Code
			writeGatewayProblem(w, requestID, mapped)
			return
		}
		defer permit.Release()
		requestContext = permit.Context()
	}
	if inspected.Descriptor != nil {
		requestContext = context.WithValue(requestContext, invocationContextKey{}, invocationContext{
			Credential: credential, Descriptor: *inspected.Descriptor, RequestID: requestID, State: state,
		})
	}
	r = r.WithContext(access.WithCredential(requestContext, credential))
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
	}

	buffered := newBufferedResponse()
	h.sdk.ServeHTTP(buffered, r)
	if h.beforeResponseWrite != nil {
		h.beforeResponseWrite(requestContext)
	}
	if permit != nil {
		if err := permit.CommitResponse(func() { buffered.flush(w) }); err != nil {
			if privacy.ErrorCode(err) == privacy.CodeContentRedacted {
				mapped := problem.ContentRedacted()
				result, errorCode = "error", mapped.Code
				writeGatewayProblem(w, requestID, mapped)
			} else {
				result, errorCode = "error", "request_cancelled"
			}
			return
		}
	} else {
		buffered.flush(w)
	}
	stateResult, stateCode := state.values()
	if stateResult != "" {
		result, errorCode = stateResult, stateCode
	} else if buffered.statusCode() >= 200 && buffered.statusCode() < 400 {
		result = "success"
	} else {
		result, errorCode = "error", "protocol_error"
	}
}

func (h *Handler) authenticationFailed(key string) problem.Problem {
	if !h.authLimiter.Allow(key) {
		return problem.RateLimited("Too many authentication failures")
	}
	return problem.AuthenticationFailed()
}

var (
	errBodyTooLarge       = errors.New("MCP request body too large")
	errDescriptorNotFound = errors.New("MCP descriptor not found")
)

func readBoundedBody(body io.ReadCloser, limit int64) ([]byte, error) {
	if body == nil {
		return nil, errors.New("MCP request body is required")
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errBodyTooLarge
	}
	return data, nil
}

type inspectedRequest struct {
	Method     string
	Descriptor *Descriptor
}

func (r inspectedRequest) auditDescriptor() string {
	if r.Descriptor != nil {
		return r.Descriptor.AuditName
	}
	return r.Method
}

func inspectRequest(body []byte) (inspectedRequest, error) {
	if len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] == '[' {
		return inspectedRequest{}, errors.New("MCP batch requests are not accepted")
	}
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || envelope.JSONRPC != "2.0" || envelope.Method == "" {
		return inspectedRequest{}, errors.New("invalid JSON-RPC request")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return inspectedRequest{}, errors.New("multiple JSON-RPC values")
	}
	result := inspectedRequest{Method: envelope.Method}
	switch envelope.Method {
	case "server/discover", "initialize", "notifications/initialized", "ping", "tools/list", "resources/list", "resources/templates/list":
		return result, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments,omitempty"`
		}
		if json.Unmarshal(envelope.Params, &params) != nil || params.Name == "" {
			return inspectedRequest{}, errors.New("invalid tool call")
		}
		descriptor, ok := descriptorByToolName(params.Name)
		if !ok {
			return inspectedRequest{}, errDescriptorNotFound
		}
		if descriptor.InputLimit > 0 && int64(len(params.Arguments)) > descriptor.InputLimit {
			return inspectedRequest{}, errBodyTooLarge
		}
		result.Descriptor = &descriptor
		return result, nil
	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		if json.Unmarshal(envelope.Params, &params) != nil || params.URI == "" {
			return inspectedRequest{}, errors.New("invalid resource read")
		}
		descriptor, ok := descriptorByResourceURI(params.URI)
		if !ok {
			return inspectedRequest{}, errDescriptorNotFound
		}
		result.Descriptor = &descriptor
		return result, nil
	default:
		return inspectedRequest{}, errDescriptorNotFound
	}
}

func descriptorByToolName(name string) (Descriptor, bool) {
	for _, descriptor := range descriptorCatalog {
		if descriptor.Kind == DescriptorTool && descriptor.Name == name {
			return descriptor, true
		}
	}
	return Descriptor{}, false
}

func descriptorByResourceURI(uri string) (Descriptor, bool) {
	for _, descriptor := range descriptorCatalog {
		switch descriptor.Kind {
		case DescriptorResource:
			if descriptor.URI == uri {
				return descriptor, true
			}
		case DescriptorResourceTemplate:
			if _, ok := matchResourceTemplate(descriptor.URITemplate, uri); ok {
				return descriptor, true
			}
		}
	}
	return Descriptor{}, false
}

func matchResourceTemplate(template, rawURI string) (map[string]string, bool) {
	parsed, err := url.Parse(rawURI)
	if err != nil || parsed.Scheme != "edu-agent" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, false
	}
	templateParts := strings.Split(template, "/")
	uriParts := strings.Split(rawURI, "/")
	if len(templateParts) != len(uriParts) {
		return nil, false
	}
	values := map[string]string{}
	for index, expected := range templateParts {
		actual := uriParts[index]
		if strings.HasPrefix(expected, "{") && strings.HasSuffix(expected, "}") {
			name := strings.TrimSuffix(strings.TrimPrefix(expected, "{"), "}")
			if !canonicalUUID(actual) {
				return nil, false
			}
			values[name] = actual
			continue
		}
		if expected != actual {
			return nil, false
		}
	}
	return values, true
}

func validRequestHost(r *http.Request) bool {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || local == nil || !loopbackHost(local.String()) {
		return true
	}
	return loopbackHost(r.Host)
}

func loopbackHost(value string) bool {
	host := value
	if parsedHost, _, err := net.SplitHostPort(value); err == nil {
		host = parsedHost
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferedResponse() *bufferedResponse    { return &bufferedResponse{header: make(http.Header)} }
func (w *bufferedResponse) Header() http.Header { return w.header }
func (w *bufferedResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *bufferedResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}
func (w *bufferedResponse) Flush() {}
func (w *bufferedResponse) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
func (w *bufferedResponse) flush(target http.ResponseWriter) {
	for key, values := range w.header {
		target.Header()[key] = append([]string(nil), values...)
	}
	target.WriteHeader(w.statusCode())
	_, _ = target.Write(w.body.Bytes())
}

func writeGatewayProblem(w http.ResponseWriter, requestID string, mapped problem.Problem) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(mapped.Status)
	_ = json.NewEncoder(w).Encode(mapped.Envelope(requestID))
}
