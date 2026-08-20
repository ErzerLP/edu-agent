package httpapi

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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type IdentityService interface {
	ExchangePairingCode(context.Context, string, string) (identity.IssuedCredential, error)
	Authenticate(context.Context, string, string) (identity.Credential, error)
	ListDevices(context.Context) ([]identity.Device, error)
	RevokeDevice(context.Context, string) error
}

type ModelProber interface {
	Probe(context.Context) llm.Capabilities
}

type Readiness interface {
	Ready(context.Context) health.Report
}

type KnowledgeService interface {
	Head(context.Context) (*knowledge.KnowledgeRevision, error)
	Import(context.Context, knowledge.ImportCommand) (knowledge.ImportResult, error)
	Tree(context.Context, string) (knowledge.TreeResult, error)
	Export(context.Context, string) (knowledge.ExportResult, error)
	Retrieve(context.Context, knowledge.RetrievalCommand) (knowledge.RetrievalResult, error)
}

type LearningService interface {
	CreateGoal(context.Context, string, learning.GoalCommand) (learning.OperationResult, error)
	CreateSession(context.Context, string, learning.SessionCommand) (learning.OperationResult, error)
	Propose(context.Context, string, learning.ProposalRequest) (learning.ProposalArtifact, error)
	ApplyAction(context.Context, string, string, learning.ActionCommand) (learning.OperationResult, error)
	Decide(context.Context, string, string, learning.AssessmentDecisionCommand) (learning.OperationResult, error)
	CurrentSession(context.Context) (learning.SessionView, error)
	Session(context.Context, string) (learning.SessionView, error)
	Timeline(context.Context, learning.TimelineQuery) (learning.TimelinePage, error)
	Routes(context.Context, learning.CursorPageRequest) (learning.RoutesPage, error)
	Node(context.Context, string) (learning.NodeView, error)
	Evidence(context.Context, learning.EvidenceQuery) (learning.EvidencePage, error)
	Reviews(context.Context, learning.ReviewQuery) (learning.ReviewsPage, error)
	ProjectionStatus(context.Context) (learning.ProjectionStatus, error)
}

type Options struct {
	Identity                IdentityService
	Model                   ModelProber
	Knowledge               KnowledgeService
	Learning                LearningService
	Readiness               Readiness
	Logger                  *slog.Logger
	PairLimiter             *FixedWindowLimiter
	AuthLimiter             *FixedWindowLimiter
	DeviceLimiter           *FixedWindowLimiter
	MaxRequestBody          int64
	MaxKnowledgeRequestBody int64
	MaxLearningRequestBody  int64
}

type API struct {
	identity                IdentityService
	model                   ModelProber
	knowledge               KnowledgeService
	learning                LearningService
	readiness               Readiness
	logger                  *slog.Logger
	pairLimiter             *FixedWindowLimiter
	authLimiter             *FixedWindowLimiter
	deviceLimiter           *FixedWindowLimiter
	maxRequestBody          int64
	maxKnowledgeRequestBody int64
	maxLearningRequestBody  int64
}

type credentialContextKey struct{}

func New(options Options) (http.Handler, error) {
	if options.Identity == nil || options.Readiness == nil || options.Logger == nil || options.PairLimiter == nil || options.AuthLimiter == nil || options.DeviceLimiter == nil {
		return nil, errors.New("HTTP API dependencies are required")
	}
	if options.MaxRequestBody <= 0 {
		options.MaxRequestBody = 64 << 10
	}
	if options.MaxKnowledgeRequestBody <= 0 {
		options.MaxKnowledgeRequestBody = knowledge.MaxImportBodyBytes
	}
	if options.MaxLearningRequestBody <= 0 {
		options.MaxLearningRequestBody = 1 << 20
	}
	api := &API{
		identity: options.Identity, model: options.Model, knowledge: options.Knowledge, learning: options.Learning,
		readiness: options.Readiness, logger: options.Logger,
		pairLimiter: options.PairLimiter, authLimiter: options.AuthLimiter,
		deviceLimiter: options.DeviceLimiter, maxRequestBody: options.MaxRequestBody,
		maxKnowledgeRequestBody: options.MaxKnowledgeRequestBody,
		maxLearningRequestBody:  options.MaxLearningRequestBody,
	}
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(api.recoverer)
	router.Use(api.audit)
	router.Get("/livez", api.livez)
	router.Get("/readyz", api.readyz)
	router.Post("/v1/pairings/exchange", api.exchangePairingCode)
	router.Group(func(protected chi.Router) {
		protected.Use(api.authenticate)
		protected.With(api.requireScope("devices:read")).Get("/v1/devices", api.listDevices)
		protected.With(api.requireScope("devices:manage")).Delete("/v1/devices/{deviceID}", api.revokeDevice)
		protected.With(api.requireScope("model:probe")).Get("/v1/model/capabilities", api.modelCapabilities)
		if api.knowledge != nil {
			protected.With(api.requireScope("knowledge:read")).Get("/v1/knowledge/revisions/head", api.knowledgeHead)
			protected.With(api.requireScope("knowledge:write")).Post("/v1/knowledge/imports", api.knowledgeImport)
			protected.With(api.requireScope("knowledge:read")).Get("/v1/knowledge/revisions/{revisionID}/tree", api.knowledgeTree)
			protected.With(api.requireScope("knowledge:read")).Get("/v1/knowledge/revisions/{revisionID}/export", api.knowledgeExport)
			protected.With(api.requireScope("knowledge:read")).Post("/v1/knowledge/retrievals", api.knowledgeRetrieval)
		}
		if api.learning != nil {
			protected.With(api.requireScope("learning:write")).Post("/v1/learning/goals", api.learningCreateGoal)
			protected.With(api.requireScope("learning:write")).Post("/v1/tutoring/sessions", api.learningCreateSession)
			protected.With(api.requireScope("learning:write")).Post("/v1/tutoring/proposals", api.learningProposal)
			protected.With(api.requireScope("learning:write")).Post("/v1/tutoring/sessions/{sessionID}/actions", api.learningAction)
			protected.With(api.requireScope("learning:write")).Post("/v1/learning/assessments/{assessmentID}/decisions", api.learningDecision)
			protected.With(api.requireScope("learning:read")).Get("/v1/tutoring/sessions/current", api.learningCurrentSession)
			protected.With(api.requireScope("learning:read")).Get("/v1/tutoring/sessions/{sessionID}", api.learningSession)
			protected.With(api.requireScope("learning:read")).Get("/v1/learning/timeline", api.learningTimeline)
			protected.With(api.requireScope("learning:read")).Get("/v1/learning/routes", api.learningRoutes)
			protected.With(api.requireScope("learning:read")).Get("/v1/learning/nodes/{nodeRevisionID}", api.learningNode)
			protected.With(api.requireScope("learning:read")).Get("/v1/learning/evidence", api.learningEvidence)
			protected.With(api.requireScope("learning:read")).Get("/v1/learning/reviews", api.learningReviews)
			protected.With(api.requireScope("learning:read")).Get("/v1/learning/projections/status", api.learningProjectionStatus)
		}
	})
	return router, nil
}

func (a *API) learningCreateGoal(w http.ResponseWriter, r *http.Request) {
	a.handleLearningCreateGoal(w, r)
}
func (a *API) learningCreateSession(w http.ResponseWriter, r *http.Request) {
	a.handleLearningCreateSession(w, r)
}
func (a *API) learningProposal(w http.ResponseWriter, r *http.Request) {
	a.handleLearningProposal(w, r)
}
func (a *API) learningAction(w http.ResponseWriter, r *http.Request) { a.handleLearningAction(w, r) }
func (a *API) learningDecision(w http.ResponseWriter, r *http.Request) {
	a.handleLearningDecision(w, r)
}
func (a *API) learningCurrentSession(w http.ResponseWriter, r *http.Request) {
	a.handleLearningCurrentSession(w, r)
}
func (a *API) learningSession(w http.ResponseWriter, r *http.Request) { a.handleLearningSession(w, r) }
func (a *API) learningTimeline(w http.ResponseWriter, r *http.Request) {
	a.handleLearningTimeline(w, r)
}
func (a *API) learningRoutes(w http.ResponseWriter, r *http.Request) { a.handleLearningRoutes(w, r) }
func (a *API) learningNode(w http.ResponseWriter, r *http.Request)   { a.handleLearningNode(w, r) }
func (a *API) learningEvidence(w http.ResponseWriter, r *http.Request) {
	a.handleLearningEvidence(w, r)
}
func (a *API) learningReviews(w http.ResponseWriter, r *http.Request) { a.handleLearningReviews(w, r) }
func (a *API) learningProjectionStatus(w http.ResponseWriter, r *http.Request) {
	a.handleLearningProjectionStatus(w, r)
}

func (a *API) livez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func (a *API) readyz(w http.ResponseWriter, r *http.Request) {
	report := a.readiness.Ready(r.Context())
	status := http.StatusOK
	if report.Status == health.StatusNotReady {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, report)
}

func (a *API) exchangePairingCode(w http.ResponseWriter, r *http.Request) {
	if !a.pairLimiter.Allow("pair:" + clientIP(r)) {
		writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many pairing attempts")
		return
	}
	var request struct {
		Code        string `json:"code"`
		DisplayName string `json:"display_name"`
	}
	if err := decodeJSON(w, r, a.maxRequestBody, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body is invalid")
		return
	}
	credential, err := a.identity.ExchangePairingCode(r.Context(), request.Code, request.DisplayName)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrInvalidInput):
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Device display name is invalid")
		case errors.Is(err, identity.ErrInvalidPairingCode):
			writeError(w, r, http.StatusUnauthorized, "pairing_failed", "Pairing code is invalid or expired")
		default:
			a.logger.ErrorContext(r.Context(), "pairing exchange failed", "request_id", middleware.GetReqID(r.Context()), "error", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Request could not be completed")
		}
		return
	}
	writeJSON(w, http.StatusCreated, credential)
}

func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failureKey := "auth:" + clientIP(r)
		if a.authLimiter.Limited(failureKey) {
			writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many authentication failures")
			return
		}
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			a.authenticationFailed(w, r, failureKey)
			return
		}
		credential, err := a.identity.Authenticate(r.Context(), token, "")
		if err != nil {
			if errors.Is(err, identity.ErrUnauthenticated) {
				a.authenticationFailed(w, r, failureKey)
				return
			}
			a.logger.ErrorContext(r.Context(), "device authentication failed", "request_id", middleware.GetReqID(r.Context()), "error", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Request could not be completed")
			return
		}
		if !a.deviceLimiter.Allow("device:" + credential.Device.ID) {
			writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Device request rate exceeded")
			return
		}
		ctx := context.WithValue(r.Context(), credentialContextKey{}, credential)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *API) authenticationFailed(w http.ResponseWriter, r *http.Request, failureKey string) {
	if !a.authLimiter.Allow(failureKey) {
		writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many authentication failures")
		return
	}
	writeError(w, r, http.StatusUnauthorized, "authentication_failed", "Device credentials are invalid")
}

func (a *API) requireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			credential, ok := credentialFromContext(r.Context())
			if !ok || !contains(credential.Scopes, scope) {
				writeError(w, r, http.StatusForbidden, "forbidden", "Device does not have the required scope")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (a *API) listDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := a.identity.ListDevices(r.Context())
	if err != nil {
		a.logger.ErrorContext(r.Context(), "list devices failed", "request_id", middleware.GetReqID(r.Context()), "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Request could not be completed")
		return
	}
	if devices == nil {
		devices = []identity.Device{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

func (a *API) revokeDevice(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimSpace(chi.URLParam(r, "deviceID"))
	if deviceID == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Device ID is invalid")
		return
	}
	if err := a.identity.RevokeDevice(r.Context(), deviceID); err != nil {
		if errors.Is(err, identity.ErrInvalidInput) {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Device ID is invalid")
			return
		}
		if errors.Is(err, identity.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "Device was not found")
			return
		}
		a.logger.ErrorContext(r.Context(), "revoke device failed", "request_id", middleware.GetReqID(r.Context()), "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Request could not be completed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) modelCapabilities(w http.ResponseWriter, r *http.Request) {
	if a.model == nil {
		writeJSON(w, http.StatusOK, llm.Capabilities{Profile: "openai-chat-completions-v1", Compatible: false, IncompatibilityReasons: []string{"not_configured"}})
		return
	}
	writeJSON(w, http.StatusOK, a.model.Probe(r.Context()))
}

func (a *API) knowledgeHead(w http.ResponseWriter, r *http.Request) {
	revision, err := a.knowledge.Head(r.Context())
	if err != nil {
		a.writeKnowledgeFailure(w, r, "read_head", err)
		return
	}
	if revision == nil {
		writeError(w, r, http.StatusNotFound, knowledge.CodeNotFound, "Knowledge revision was not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": revision})
}

func (a *API) knowledgeImport(w http.ResponseWriter, r *http.Request) {
	var command knowledge.ImportCommand
	if err := decodeJSON(w, r, a.maxKnowledgeRequestBody, &command); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeError(w, r, http.StatusRequestEntityTooLarge, knowledge.CodePayloadTooLarge, "Request body exceeds the knowledge import limit")
		} else {
			writeError(w, r, http.StatusBadRequest, knowledge.CodeInvalidRequest, "Request body is invalid")
		}
		return
	}
	if !command.ExpectedParentProvided {
		writeError(w, r, http.StatusBadRequest, knowledge.CodeInvalidRequest, "expected_parent_revision_id is required")
		return
	}
	credential, ok := credentialFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "authentication_failed", "Device credentials are invalid")
		return
	}
	command.ActorDeviceID = credential.Device.ID
	result, err := a.knowledge.Import(r.Context(), command)
	if err != nil {
		a.writeKnowledgeFailure(w, r, "import", err)
		return
	}
	status := http.StatusCreated
	if result.Unchanged || result.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

func (a *API) knowledgeTree(w http.ResponseWriter, r *http.Request) {
	result, err := a.knowledge.Tree(r.Context(), chi.URLParam(r, "revisionID"))
	if err != nil {
		a.writeKnowledgeFailure(w, r, "read_tree", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) knowledgeExport(w http.ResponseWriter, r *http.Request) {
	result, err := a.knowledge.Export(r.Context(), chi.URLParam(r, "revisionID"))
	if err != nil {
		a.writeKnowledgeFailure(w, r, "export", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) knowledgeRetrieval(w http.ResponseWriter, r *http.Request) {
	var command knowledge.RetrievalCommand
	if err := decodeJSON(w, r, a.maxKnowledgeRequestBody, &command); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeError(w, r, http.StatusRequestEntityTooLarge, knowledge.CodePayloadTooLarge, "Request body exceeds the knowledge request limit")
		} else {
			writeError(w, r, http.StatusBadRequest, knowledge.CodeInvalidRequest, "Request body is invalid")
		}
		return
	}
	result, err := a.knowledge.Retrieve(r.Context(), command)
	if err != nil {
		a.writeKnowledgeFailure(w, r, "retrieve", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) writeKnowledgeFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	code := knowledge.ErrorCode(err)
	status := http.StatusInternalServerError
	message := "Request could not be completed"
	switch code {
	case knowledge.CodePayloadTooLarge:
		status, message = http.StatusRequestEntityTooLarge, "Knowledge payload exceeds the configured limit"
	case knowledge.CodeInvalidRequest, knowledge.CodeInvalidPath:
		status, message = http.StatusBadRequest, "Knowledge request is invalid"
	case knowledge.CodeInvalidMarkdown, knowledge.CodeInvalidIdentityMarker:
		status, message = http.StatusUnprocessableEntity, "Markdown identity or syntax is invalid"
	case knowledge.CodeDuplicateDocumentIdentity, knowledge.CodePathOccupied,
		knowledge.CodeIdentityReviewRequired, knowledge.CodeStaleIdentityReview,
		knowledge.CodeRevisionConflict, knowledge.CodeIdempotencyConflict:
		status, message = http.StatusConflict, "Knowledge import could not be committed"
	case knowledge.CodeNotFound:
		status, message = http.StatusNotFound, "Knowledge revision was not found"
	case "":
		a.logger.ErrorContext(r.Context(), "knowledge request failed",
			"request_id", middleware.GetReqID(r.Context()), "operation", operation, "error_category", "internal")
	}
	if code == "" {
		code = "internal_error"
	}
	response := map[string]any{"error": map[string]string{
		"code": code, "message": message, "request_id": middleware.GetReqID(r.Context()),
	}}
	var domainErr *knowledge.Error
	if errors.As(err, &domainErr) {
		if domainErr.CurrentRevisionKnown {
			if domainErr.CurrentRevisionID == nil {
				response["current_revision_id"] = nil
			} else {
				response["current_revision_id"] = *domainErr.CurrentRevisionID
			}
		}
		if domainErr.Review != nil {
			response["identity_review"] = domainErr.Review
		}
	}
	writeJSON(w, status, response)
}

func (a *API) audit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		wrapped := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(wrapped, r)
		a.logger.InfoContext(r.Context(), "http_request",
			"request_id", middleware.GetReqID(r.Context()), "method", r.Method, "path", r.URL.Path,
			"status", wrapped.Status(), "duration_ms", time.Since(started).Milliseconds(), "remote_ip", clientIP(r))
	})
}

func (a *API) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				a.logger.ErrorContext(r.Context(), "http handler panic", "request_id", middleware.GetReqID(r.Context()), "panic_type", fmt.Sprintf("%T", recovered))
				writeError(w, r, http.StatusInternalServerError, "internal_error", "Request could not be completed")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func credentialFromContext(ctx context.Context) (identity.Credential, bool) {
	credential, ok := ctx.Value(credentialContextKey{}).(identity.Credential)
	return credential, ok
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, target any) error {
	data, err := readJSONBody(w, r, limit)
	if err != nil {
		return err
	}
	return decodeJSONData(data, target)
}

func readJSONBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, errors.New("request body must be valid UTF-8")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, err
	}
	return data, nil
}

func decodeJSONData(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] {
					return errors.New("request contains a duplicate object key")
				}
				seen[key] = true
				if err := value(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return errors.New("request contains an invalid object")
			}
		case '[':
			for decoder.More() {
				if err := value(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return errors.New("request contains an invalid array")
			}
		default:
			return errors.New("request contains an invalid JSON delimiter")
		}
		return nil
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"code": code, "message": message, "request_id": middleware.GetReqID(r.Context()),
	}})
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
