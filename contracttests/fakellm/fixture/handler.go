package fixture

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
	"strings"
	"time"
)

const (
	ChatCompletionsPath = "/v1/chat/completions"
	ControlPrefix       = "/__fixture"
)

type Handler struct {
	apiKey           string
	controlKey       string
	controller       *Controller
	maxRequestBytes  int64
	timeoutDelay     time.Duration
	scenarioSelected func(RequestKind, Scenario)
}

func (h *Handler) Controller() *Controller { return h.controller }

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream         *bool `json:"stream"`
	ResponseFormat struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name   string         `json:"name"`
			Strict *bool          `json:"strict"`
			Schema map[string]any `json:"schema"`
		} `json:"json_schema,omitempty"`
	} `json:"response_format"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, ControlPrefix) {
		h.serveControl(w, r)
		return
	}
	generation := h.controller.beginRequest()
	if r.URL.Path != ChatCompletionsPath {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+h.apiKey {
		h.record(generation, r, nil, "", "", DefaultScenario(), http.StatusUnauthorized)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodHead {
		scenario := h.controller.nextScenario(generation, KindCapabilityProbe)
		h.notifyScenarioSelected(KindCapabilityProbe, scenario)
		status := h.executeAvailability(w, r, scenario)
		h.record(generation, r, nil, KindCapabilityProbe, "", scenario, status)
		return
	}
	if r.Method != http.MethodPost {
		h.record(generation, r, nil, "", "", DefaultScenario(), http.StatusMethodNotAllowed)
		w.Header().Set("Allow", http.MethodPost+", "+http.MethodHead)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := readBounded(r.Body, h.maxRequestBytes)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		h.record(generation, r, body, "", "", DefaultScenario(), status)
		http.Error(w, http.StatusText(status), status)
		return
	}
	request, kind, proposal, err := decodeChatRequest(r.Header.Get("Content-Type"), body)
	if err != nil {
		h.record(generation, r, body, "", "", DefaultScenario(), http.StatusBadRequest)
		http.Error(w, "invalid chat completions profile", http.StatusBadRequest)
		return
	}
	scenario := h.controller.nextScenario(generation, kind)
	h.notifyScenarioSelected(kind, scenario)
	requestID := ""
	if proposal != nil {
		requestID = proposal.RequestID
	}
	status := h.execute(w, r, request, kind, proposal, scenario)
	h.record(generation, r, body, kind, requestID, scenario, status)
}

func (h *Handler) notifyScenarioSelected(kind RequestKind, scenario Scenario) {
	if h.scenarioSelected != nil {
		h.scenarioSelected(kind, cloneScenario(scenario))
	}
}

func (h *Handler) executeAvailability(w http.ResponseWriter, r *http.Request, scenario Scenario) int {
	switch scenario.Kind {
	case ScenarioUnauthorized:
		w.WriteHeader(http.StatusUnauthorized)
		return http.StatusUnauthorized
	case ScenarioRateLimited:
		retryAfter := scenario.RetryAfter
		if retryAfter == "" {
			retryAfter = "1"
		}
		w.Header().Set("Retry-After", retryAfter)
		w.WriteHeader(http.StatusTooManyRequests)
		return http.StatusTooManyRequests
	case ScenarioHTTPError:
		w.WriteHeader(scenario.StatusCode)
		return scenario.StatusCode
	case ScenarioTimeout:
		delay := h.timeoutDelay
		if scenario.DelayMillis > 0 {
			delay = time.Duration(scenario.DelayMillis) * time.Millisecond
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return 0
		case <-timer.C:
			w.WriteHeader(http.StatusGatewayTimeout)
			return http.StatusGatewayTimeout
		}
	default:
		w.WriteHeader(http.StatusNoContent)
		return http.StatusNoContent
	}
}

func (h *Handler) execute(w http.ResponseWriter, r *http.Request, request chatRequest, kind RequestKind, proposal *proposalRequest, scenario Scenario) int {
	if scenario.Kind == ScenarioUnauthorized {
		w.WriteHeader(http.StatusUnauthorized)
		return http.StatusUnauthorized
	}
	if scenario.Kind == ScenarioNoNativeSchema && request.ResponseFormat.Type == "json_schema" {
		w.WriteHeader(http.StatusBadRequest)
		return http.StatusBadRequest
	}
	if scenario.Kind == ScenarioRateLimited {
		retryAfter := scenario.RetryAfter
		if retryAfter == "" {
			retryAfter = "1"
		}
		w.Header().Set("Retry-After", retryAfter)
		w.WriteHeader(http.StatusTooManyRequests)
		return http.StatusTooManyRequests
	}
	if scenario.Kind == ScenarioHTTPError {
		w.WriteHeader(scenario.StatusCode)
		return scenario.StatusCode
	}
	if scenario.Kind == ScenarioTimeout {
		delay := h.timeoutDelay
		if scenario.DelayMillis > 0 {
			delay = time.Duration(scenario.DelayMillis) * time.Millisecond
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return 0
		case <-timer.C:
			w.WriteHeader(http.StatusGatewayTimeout)
			return http.StatusGatewayTimeout
		}
	}
	if scenario.Kind == ScenarioMalformedEnvelope {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":`)
		return http.StatusOK
	}
	content := []byte(`{"capability_probe":true}`)
	if kind != KindCapabilityProbe {
		var err error
		content, err = renderArtifact(kind, *proposal, scenario)
		if err != nil {
			http.Error(w, "invalid proposal fixture context", http.StatusBadRequest)
			return http.StatusBadRequest
		}
	}
	if scenario.Kind == ScenarioMalformed {
		content = []byte(`{"incomplete":`)
	}
	if scenario.Kind == ScenarioSchemaMismatch {
		content = []byte(`{"unexpected":true}`)
	}
	writeChatResponse(w, content)
	return http.StatusOK
}

func decodeChatRequest(contentType string, body []byte) (chatRequest, RequestKind, *proposalRequest, error) {
	var request chatRequest
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return request, "", nil, fmt.Errorf("content type must be application/json")
	}
	if err := decodeStrict(body, &request); err != nil {
		return request, "", nil, err
	}
	if strings.TrimSpace(request.Model) == "" || request.Stream == nil || *request.Stream {
		return request, "", nil, fmt.Errorf("model and stream=false are required")
	}
	if len(request.Messages) != 3 || request.Messages[0].Role != "system" || request.Messages[1].Role != "assistant" || request.Messages[2].Role != "user" {
		return request, "", nil, fmt.Errorf("ordered system, assistant, and user messages are required")
	}
	for _, message := range request.Messages {
		if strings.TrimSpace(message.Content) == "" {
			return request, "", nil, fmt.Errorf("message content is required")
		}
	}
	if request.ResponseFormat.Type != "json_object" && request.ResponseFormat.Type != "json_schema" {
		return request, "", nil, fmt.Errorf("structured response format is required")
	}
	if request.ResponseFormat.Type == "json_schema" {
		schema := request.ResponseFormat.JSONSchema
		if schema == nil || strings.TrimSpace(schema.Name) == "" || schema.Strict == nil || !*schema.Strict || len(schema.Schema) == 0 {
			return request, "", nil, fmt.Errorf("strict native JSON schema is required")
		}
	} else if request.ResponseFormat.JSONSchema != nil {
		return request, "", nil, fmt.Errorf("json_object must not include json_schema")
	}
	if isCapabilityProbe(request) {
		if err := validateCapabilityContract(request); err != nil {
			return request, "", nil, err
		}
		return request, KindCapabilityProbe, nil, nil
	}
	var proposal proposalRequest
	if err := decodeStrict([]byte(request.Messages[2].Content), &proposal); err != nil {
		return request, "", nil, fmt.Errorf("decode proposal request: %w", err)
	}
	kind, err := ParseRequestKind(string(proposal.ProposalType))
	if err != nil || kind == KindCapabilityProbe || proposal.RequestID == "" || proposal.AggregateID == "" || proposal.KnowledgeRevisionID == "" || len(proposal.NodeRevisionIDs) == 0 || !isJSONObject(proposal.Input) {
		return request, "", nil, fmt.Errorf("invalid proposal request")
	}
	if err := validateProposalContract(request, kind); err != nil {
		return request, "", nil, err
	}
	return request, kind, &proposal, nil
}

func isCapabilityProbe(request chatRequest) bool {
	if request.ResponseFormat.Type == "json_schema" && request.ResponseFormat.JSONSchema != nil && request.ResponseFormat.JSONSchema.Name == "capability_probe" {
		return true
	}
	return request.ResponseFormat.Type == "json_object" &&
		request.Messages[0].Content == "Return only JSON matching the requested schema." &&
		request.Messages[1].Content == "I will return the requested JSON object." &&
		request.Messages[2].Content == "Confirm the core profile."
}

func isJSONObject(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &value) == nil && value != nil
}

func decodeStrict(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func writeChatResponse(w http.ResponseWriter, content []byte) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type choice struct {
		Message message `json:"message"`
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Choices []choice `json:"choices"`
	}{Choices: []choice{{Message: message{Role: "assistant", Content: string(content)}}}})
}

var errBodyTooLarge = errors.New("request body too large")

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return body[:limit], errBodyTooLarge
	}
	return body, nil
}

func (h *Handler) record(generation uint64, r *http.Request, body []byte, kind RequestKind, requestID string, scenario Scenario, status int) {
	entry := AuditEntry{
		Method: r.Method, Path: r.URL.Path, RequestKind: kind, RequestID: requestID,
		Scenario: scenario, Status: status, RequestBytes: len(body),
	}
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		entry.RequestSHA256 = hex.EncodeToString(sum[:])
	}
	var request chatRequest
	if json.Unmarshal(body, &request) == nil {
		entry.Model = request.Model
		entry.ResponseFormat = request.ResponseFormat.Type
	}
	h.controller.record(generation, entry)
}

func (h *Handler) serveControl(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Fixture-Control-Key") != h.controlKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == ControlPrefix+"/audit" && r.Method == http.MethodGet:
		writeControlJSON(w, http.StatusOK, map[string]any{"audit": h.controller.Audit()})
	case r.URL.Path == ControlPrefix+"/scenarios" && r.Method == http.MethodGet:
		writeControlJSON(w, http.StatusOK, map[string]any{"scenarios": h.controller.Programs()})
	case r.URL.Path == ControlPrefix+"/reset" && r.Method == http.MethodPost:
		h.controller.Reset()
		writeControlJSON(w, http.StatusOK, map[string]any{"status": "reset"})
	case strings.HasPrefix(r.URL.Path, ControlPrefix+"/scenarios/") && r.Method == http.MethodPut:
		kindName := strings.TrimPrefix(r.URL.Path, ControlPrefix+"/scenarios/")
		if kindName == "" || strings.Contains(kindName, "/") {
			http.NotFound(w, r)
			return
		}
		kind, err := ParseRequestKind(kindName)
		if err != nil {
			http.Error(w, "unknown request kind", http.StatusBadRequest)
			return
		}
		body, err := readBounded(r.Body, 64<<10)
		if err != nil {
			http.Error(w, "invalid scenario program", http.StatusBadRequest)
			return
		}
		var input struct {
			Sequence []Scenario `json:"sequence"`
		}
		if err := decodeStrict(body, &input); err != nil {
			http.Error(w, "invalid scenario program", http.StatusBadRequest)
			return
		}
		if err := h.controller.Configure(kind, input.Sequence...); err != nil {
			http.Error(w, "invalid scenario program", http.StatusBadRequest)
			return
		}
		writeControlJSON(w, http.StatusOK, map[string]any{"request_kind": kind, "sequence": input.Sequence})
	default:
		http.NotFound(w, r)
	}
}

func writeControlJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
