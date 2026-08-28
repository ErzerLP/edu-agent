package fixture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/integrations/tutormodel"
	"github.com/edu-agent/edu-agent/server/internal/learning"
)

const (
	testRequestID           = "10000000-0000-4000-8000-000000000001"
	testSessionID           = "10000000-0000-4000-8000-000000000002"
	testGoalRevisionID      = "10000000-0000-4000-8000-000000000003"
	testRouteRevisionID     = "10000000-0000-4000-8000-000000000004"
	testRouteStepID         = "10000000-0000-4000-8000-000000000005"
	testKnowledgeRevisionID = "10000000-0000-4000-8000-000000000006"
	testNodeID              = "10000000-0000-4000-8000-000000000007"
	testNodeRevisionID      = "10000000-0000-4000-8000-000000000008"
	testDocumentRevisionID  = "10000000-0000-4000-8000-000000000009"
	testActivityID          = "10000000-0000-4000-8000-000000000010"
	testAttemptID           = "10000000-0000-4000-8000-000000000011"
	testFreeQuestionID      = "10000000-0000-4000-8000-000000000012"
	testFocusFrameID        = "10000000-0000-4000-8000-000000000013"
)

func TestRealTutorModelAdapterDecodesEveryProposalSchema(t *testing.T) {
	controller := NewController()
	server := httptest.NewServer(NewHandler(HandlerOptions{APIKey: "test-key", Controller: controller}))
	defer server.Close()
	adapter := tutormodel.New(newLLMClient(t, server.URL, time.Second))
	for _, kind := range []learning.ProposalType{
		learning.ProposalRoute,
		learning.ProposalActivity,
		learning.ProposalAssessment,
		learning.ProposalFreeAnswer,
		learning.ProposalExplanation,
	} {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			raw, err := adapter.Generate(context.Background(), adapterProposalRequest(t, kind))
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case learning.ProposalRoute:
				var value struct {
					Route []learning.RouteProposalStep `json:"route"`
				}
				decodeOutput(t, raw, &value)
				if len(value.Route) != 1 || value.Route[0].NodeRevisionID != testNodeRevisionID {
					t.Fatalf("route=%+v", value.Route)
				}
			case learning.ProposalActivity:
				var value struct {
					Activity learning.ActivityProposal `json:"activity"`
				}
				decodeOutput(t, raw, &value)
				if value.Activity.Type != learning.ActivityOpen || len(value.Activity.References) != 1 {
					t.Fatalf("activity=%+v", value.Activity)
				}
			case learning.ProposalAssessment:
				assessment := decodeAssessment(t, raw)
				if assessment.Confidence != 950 || len(assessment.Items) != 1 || assessment.Items[0].RubricItemID != "rubric-item-1" {
					t.Fatalf("assessment=%+v", assessment)
				}
			case learning.ProposalFreeAnswer, learning.ProposalExplanation:
				var value struct {
					Text learning.TextProposal `json:"text"`
				}
				decodeOutput(t, raw, &value)
				if strings.TrimSpace(value.Text.Text) == "" || len(value.Text.References) != 1 {
					t.Fatalf("text=%+v", value.Text)
				}
			}
		})
	}
}

func TestCapabilityProbeRemainsCompatibleWithoutNativeSchema(t *testing.T) {
	controller := NewController()
	if err := controller.Configure(KindCapabilityProbe, Scenario{Kind: ScenarioNoNativeSchema}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewHandler(HandlerOptions{APIKey: "test-key", Controller: controller}))
	defer server.Close()
	capabilities := newLLMClient(t, server.URL, time.Second).Probe(context.Background())
	if !capabilities.Compatible || capabilities.NativeJSONSchema || !capabilities.StructuredJSON || !capabilities.SystemUserAssistant {
		t.Fatalf("capabilities=%+v", capabilities)
	}
	audit := controller.Audit()
	if len(audit) != 2 || audit[0].Status != http.StatusBadRequest || audit[1].Status != http.StatusOK {
		t.Fatalf("audit=%+v", audit)
	}
}

func TestControlAPIProgramsAuditsAndResetsFixture(t *testing.T) {
	controller := NewController()
	server := httptest.NewServer(NewHandler(HandlerOptions{APIKey: "test-key", ControlKey: "control", Controller: controller}))
	defer server.Close()
	program := []byte(`{"sequence":[{"kind":"accepted","activity_type":"objective","allowed_help":["hint"]}]}`)
	request, err := http.NewRequest(http.MethodPut, server.URL+ControlPrefix+"/scenarios/activity", bytes.NewReader(program))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Fixture-Control-Key", "control")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("program status=%d", response.StatusCode)
	}
	raw, err := tutormodel.New(newLLMClient(t, server.URL, time.Second)).Generate(context.Background(), adapterProposalRequest(t, learning.ProposalActivity))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Activity learning.ActivityProposal `json:"activity"`
	}
	decodeOutput(t, raw, &value)
	if value.Activity.Type != learning.ActivityObjective || len(value.Activity.AllowedHelp) != 1 || value.Activity.AllowedHelp[0] != learning.HelpHint {
		t.Fatalf("activity=%+v", value.Activity)
	}
	request, err = http.NewRequest(http.MethodGet, server.URL+ControlPrefix+"/audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Fixture-Control-Key", "control")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var auditResponse struct {
		Audit []AuditEntry `json:"audit"`
	}
	if err := json.NewDecoder(response.Body).Decode(&auditResponse); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if len(auditResponse.Audit) != 1 || auditResponse.Audit[0].RequestKind != KindActivity ||
		auditResponse.Audit[0].ProtocolProfile != OpenAIChatCompletionsProfileV1 ||
		auditResponse.Audit[0].ResponseFormat != "json_schema" || auditResponse.Audit[0].Model != "strict-fake" {
		t.Fatalf("audit metadata=%+v", auditResponse.Audit)
	}
	request, err = http.NewRequest(http.MethodPost, server.URL+ControlPrefix+"/reset", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Fixture-Control-Key", "control")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(controller.Audit()) != 0 || len(controller.Programs()) != 0 {
		t.Fatalf("reset status=%d audit=%v programs=%v", response.StatusCode, controller.Audit(), controller.Programs())
	}
}

func TestAssessmentAcceptedProvisionalAndEveryRiskScenario(t *testing.T) {
	controller := NewController()
	server := httptest.NewServer(NewHandler(HandlerOptions{APIKey: "test-key", Controller: controller}))
	defer server.Close()
	adapter := tutormodel.New(newLLMClient(t, server.URL, time.Second))
	tests := []struct {
		name           string
		scenario       Scenario
		wantConfidence int
		wantRisk       string
	}{
		{name: "accepted", scenario: Scenario{Kind: ScenarioAccepted}, wantConfidence: 950},
		{name: "provisional", scenario: Scenario{Kind: ScenarioProvisional}, wantConfidence: 800},
	}
	for _, risk := range RiskFlags() {
		tests = append(tests, struct {
			name           string
			scenario       Scenario
			wantConfidence int
			wantRisk       string
		}{name: "risk " + risk, scenario: Scenario{Kind: ScenarioRisk, RiskFlag: risk}, wantConfidence: 950, wantRisk: risk})
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			if err := controller.Configure(KindAssessment, test.scenario); err != nil {
				t.Fatal(err)
			}
			raw, err := adapter.Generate(context.Background(), adapterProposalRequest(t, learning.ProposalAssessment))
			if err != nil {
				t.Fatal(err)
			}
			assessment := decodeAssessment(t, raw)
			if assessment.Confidence != test.wantConfidence {
				t.Fatalf("confidence=%d want=%d", assessment.Confidence, test.wantConfidence)
			}
			if test.wantRisk == "" && len(assessment.RiskFlags) != 0 {
				t.Fatalf("risk_flags=%v", assessment.RiskFlags)
			}
			if test.wantRisk != "" && (len(assessment.RiskFlags) != 1 || assessment.RiskFlags[0] != test.wantRisk) {
				t.Fatalf("risk_flags=%v want=%q", assessment.RiskFlags, test.wantRisk)
			}
		})
	}
}

func TestRealAdapterFailureCategoriesFromProgrammableScenarios(t *testing.T) {
	tests := []struct {
		name     string
		scenario Scenario
		timeout  time.Duration
		want     string
	}{
		{name: "malformed", scenario: Scenario{Kind: ScenarioMalformed}, timeout: time.Second, want: "malformed_json"},
		{name: "schema mismatch", scenario: Scenario{Kind: ScenarioSchemaMismatch}, timeout: time.Second, want: "schema_mismatch"},
		{name: "rate limited", scenario: Scenario{Kind: ScenarioRateLimited}, timeout: time.Second, want: "rate_limited"},
		{name: "5xx", scenario: Scenario{Kind: ScenarioHTTPError, StatusCode: http.StatusServiceUnavailable}, timeout: time.Second, want: "upstream_error"},
		{name: "timeout", scenario: Scenario{Kind: ScenarioTimeout}, timeout: 25 * time.Millisecond, want: "timeout"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			controller := NewController()
			if err := controller.Configure(KindRoute, test.scenario); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(NewHandler(HandlerOptions{APIKey: "test-key", Controller: controller, TimeoutDelay: 250 * time.Millisecond}))
			defer server.Close()
			_, err := tutormodel.New(newLLMClient(t, server.URL, test.timeout)).Generate(context.Background(), adapterProposalRequest(t, learning.ProposalRoute))
			failure, ok := err.(interface{ ModelCategory() string })
			if !ok || failure.ModelCategory() != test.want {
				t.Fatalf("error=%T %v category=%q want=%q", err, err, modelCategory(err), test.want)
			}
		})
	}
}

func TestScenarioSequenceConsumesThenSticksOnFinalResult(t *testing.T) {
	controller := NewController()
	if err := controller.Configure(
		KindRoute,
		Scenario{Kind: ScenarioHTTPError, StatusCode: http.StatusServiceUnavailable},
		Scenario{Kind: ScenarioAccepted},
	); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewHandler(HandlerOptions{APIKey: "test-key", Controller: controller}))
	defer server.Close()
	adapter := tutormodel.New(newLLMClient(t, server.URL, time.Second))
	if _, err := adapter.Generate(context.Background(), adapterProposalRequest(t, learning.ProposalRoute)); modelCategory(err) != "upstream_error" {
		t.Fatalf("first category=%q error=%v", modelCategory(err), err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := adapter.Generate(context.Background(), adapterProposalRequest(t, learning.ProposalRoute)); err != nil {
			t.Fatalf("accepted attempt %d: %v", attempt+1, err)
		}
	}
	audit := controller.Audit()
	if len(audit) != 3 || audit[0].Scenario.Kind != ScenarioHTTPError || audit[1].Scenario.Kind != ScenarioAccepted || audit[2].Scenario.Kind != ScenarioAccepted {
		t.Fatalf("audit=%+v", audit)
	}
}

func TestStrictChatRequestParsingAndMetadataOnlyAudit(t *testing.T) {
	controller := NewController()
	server := httptest.NewServer(NewHandler(HandlerOptions{APIKey: "test-key", Controller: controller}))
	defer server.Close()
	proposal := adapterProposalRequest(t, learning.ProposalRoute)
	proposal.Input = json.RawMessage(`{"sensitive_answer":"do not audit this body"}`)
	userContent, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"model": "strict-fake", "stream": false, "unexpected": true,
		"messages": []map[string]string{
			{"role": "system", "content": "system"},
			{"role": "assistant", "content": "assistant"},
			{"role": "user", "content": string(userContent)},
		},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "tutoring_proposal", "strict": true, "schema": map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+ChatCompletionsPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer test-key")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Private-Header", "private-header-secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", response.StatusCode)
	}
	auditJSON, err := json.Marshal(controller.Audit())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"do not audit this body", "test-key", "private-header-secret", "Authorization", "X-Private-Header", "messages", "sensitive_answer"} {
		if bytes.Contains(auditJSON, []byte(secret)) {
			t.Fatalf("audit recorded request content, token, or header %q: %s", secret, auditJSON)
		}
	}
	if len(controller.Audit()) != 1 || controller.Audit()[0].Status != http.StatusBadRequest {
		t.Fatalf("audit=%s", auditJSON)
	}
}

func TestFixtureRejectsEveryFrozenSchemaDriftWithoutProductionValidator(t *testing.T) {
	tests := []struct {
		kind   RequestKind
		mutate func(*testing.T, map[string]any)
	}{
		{kind: KindRoute, mutate: func(t *testing.T, schema map[string]any) {
			delete(contractObject(t, schema, "properties", "route", "items"), "required")
		}},
		{kind: KindActivity, mutate: func(t *testing.T, schema map[string]any) {
			contractObject(t, schema, "properties", "activity", "properties", "knowledge_references")["minItems"] = float64(2)
		}},
		{kind: KindAssessment, mutate: func(t *testing.T, schema map[string]any) {
			contractObject(t, schema, "properties", "assessment", "properties", "items", "items", "properties", "conclusion")["enum"] = []any{"pass"}
		}},
		{kind: KindFreeAnswer, mutate: func(t *testing.T, schema map[string]any) {
			contractObject(t, schema, "properties", "text")["additionalProperties"] = true
		}},
		{kind: KindExplanation, mutate: func(t *testing.T, schema map[string]any) {
			delete(contractObject(t, schema, "properties", "text", "properties", "knowledge_references", "items"), "required")
		}},
	}
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			expected, err := proposalSchemaContract(test.kind)
			if err != nil {
				t.Fatal(err)
			}
			schema := decodeContractSchema(t, expected)
			test.mutate(t, schema)
			body := proposalContractRequestBody(t, test.kind, schema, proposalSystemPrompt, proposalAssistantPrompt, "system")
			if _, _, _, err := decodeChatRequest("application/json", body); err == nil {
				t.Fatal("fixture accepted drifted proposal schema")
			}
		})
	}

	capability := decodeContractSchema(t, capabilitySchemaContract)
	contractObject(t, capability, "properties", "capability_probe")["type"] = "string"
	body := capabilityContractRequestBody(t, capability, capabilitySystemPrompt, capabilityAssistantPrompt, capabilityUserPrompt, "system")
	if _, _, _, err := decodeChatRequest("application/json", body); err == nil {
		t.Fatal("fixture accepted drifted capability schema")
	}
}

func TestFixtureRejectsPromptAndRoleRevisionDrift(t *testing.T) {
	proposalSchema := decodeContractSchema(t, routeSchemaContract)
	proposalTests := []struct {
		name       string
		system     string
		assistant  string
		systemRole string
	}{
		{name: "system prompt", system: proposalSystemPrompt + " changed", assistant: proposalAssistantPrompt, systemRole: "system"},
		{name: "assistant prompt", system: proposalSystemPrompt, assistant: proposalAssistantPrompt + " changed", systemRole: "system"},
		{name: "system role", system: proposalSystemPrompt, assistant: proposalAssistantPrompt, systemRole: "user"},
	}
	for _, test := range proposalTests {
		t.Run("proposal "+test.name, func(t *testing.T) {
			body := proposalContractRequestBody(t, KindRoute, proposalSchema, test.system, test.assistant, test.systemRole)
			if _, _, _, err := decodeChatRequest("application/json", body); err == nil {
				t.Fatal("fixture accepted proposal prompt or role drift")
			}
		})
	}

	capabilitySchema := decodeContractSchema(t, capabilitySchemaContract)
	capabilityTests := []struct {
		name       string
		user       string
		systemRole string
	}{
		{name: "user prompt", user: capabilityUserPrompt + " changed", systemRole: "system"},
		{name: "system role", user: capabilityUserPrompt, systemRole: "user"},
	}
	for _, test := range capabilityTests {
		t.Run("capability "+test.name, func(t *testing.T) {
			body := capabilityContractRequestBody(t, capabilitySchema, capabilitySystemPrompt, capabilityAssistantPrompt, test.user, test.systemRole)
			if _, _, _, err := decodeChatRequest("application/json", body); err == nil {
				t.Fatal("fixture accepted capability prompt or role drift")
			}
		})
	}
}

func TestControllerIsConcurrentAndResettable(t *testing.T) {
	controller := NewController()
	server := httptest.NewServer(NewHandler(HandlerOptions{APIKey: "test-key", Controller: controller}))
	defer server.Close()
	adapter := tutormodel.New(newLLMClient(t, server.URL, time.Second))
	const calls = 24
	var wait sync.WaitGroup
	errorsFound := make(chan error, calls)
	for index := 0; index < calls; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := adapter.Generate(context.Background(), adapterProposalRequest(t, learning.ProposalRoute))
			errorsFound <- err
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	if audit := controller.Audit(); len(audit) != calls {
		t.Fatalf("audit calls=%d want=%d", len(audit), calls)
	}
	controller.Reset()
	if len(controller.Audit()) != 0 || len(controller.Programs()) != 0 {
		t.Fatalf("reset audit=%v programs=%v", controller.Audit(), controller.Programs())
	}
}

func TestResetExcludesTimedOutInflightRequestFromNewGeneration(t *testing.T) {
	controller := NewController()
	if err := controller.Configure(KindRoute, Scenario{Kind: ScenarioTimeout, DelayMillis: 30_000}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(HandlerOptions{APIKey: "test-key", Controller: controller})
	selected := make(chan struct{})
	var selectedOnce sync.Once
	handler.scenarioSelected = func(kind RequestKind, scenario Scenario) {
		if kind == KindRoute && scenario.Kind == ScenarioTimeout {
			selectedOnce.Do(func() { close(selected) })
		}
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	adapter := tutormodel.New(newLLMClient(t, server.URL, 35*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := adapter.Generate(ctx, adapterProposalRequest(t, learning.ProposalRoute))
		result <- err
	}()
	select {
	case <-selected:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for timeout scenario to start")
	}

	controller.Reset()
	if err := controller.Configure(
		KindRoute,
		Scenario{Kind: ScenarioHTTPError, StatusCode: http.StatusServiceUnavailable},
		Scenario{Kind: ScenarioAccepted},
	); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled timeout request unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for canceled request")
	}
	if audit := controller.Audit(); len(audit) != 0 {
		t.Fatalf("old generation repopulated audit: %+v", audit)
	}

	if _, err := adapter.Generate(context.Background(), adapterProposalRequest(t, learning.ProposalRoute)); modelCategory(err) != "upstream_error" {
		t.Fatalf("first new-generation scenario was consumed by old request: category=%q error=%v", modelCategory(err), err)
	}
	if _, err := adapter.Generate(context.Background(), adapterProposalRequest(t, learning.ProposalRoute)); err != nil {
		t.Fatalf("second new-generation scenario failed: %v", err)
	}
	audit := controller.Audit()
	if len(audit) != 2 || audit[0].Sequence != 1 || audit[0].Scenario.Kind != ScenarioHTTPError || audit[1].Scenario.Kind != ScenarioAccepted {
		t.Fatalf("new-generation audit=%+v", audit)
	}
}

func decodeContractSchema(t *testing.T, raw string) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func contractObject(t *testing.T, root map[string]any, path ...string) map[string]any {
	t.Helper()
	current := root
	for _, name := range path {
		next, ok := current[name].(map[string]any)
		if !ok {
			t.Fatalf("contract path %q is %T", strings.Join(path, "."), current[name])
		}
		current = next
	}
	return current
}

func proposalContractRequestBody(t *testing.T, kind RequestKind, schema map[string]any, system, assistant, systemRole string) []byte {
	t.Helper()
	proposal := adapterProposalRequest(t, learning.ProposalType(kind))
	user, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"model":  "strict-fake",
		"stream": false,
		"messages": []map[string]string{
			{"role": systemRole, "content": system},
			{"role": "assistant", "content": assistant},
			{"role": "user", "content": string(user)},
		},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "tutoring_proposal", "strict": true, "schema": schema},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func capabilityContractRequestBody(t *testing.T, schema map[string]any, system, assistant, user, systemRole string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":  "strict-fake",
		"stream": false,
		"messages": []map[string]string{
			{"role": systemRole, "content": system},
			{"role": "assistant", "content": assistant},
			{"role": "user", "content": user},
		},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "capability_probe", "strict": true, "schema": schema},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

type assessmentOutput struct {
	Items          []learning.AssessmentItem `json:"items"`
	RubricComplete bool                      `json:"rubric_complete"`
	Confidence     int                       `json:"confidence"`
	RiskFlags      []string                  `json:"risk_flags"`
}

func decodeAssessment(t *testing.T, raw json.RawMessage) assessmentOutput {
	t.Helper()
	var value struct {
		Assessment assessmentOutput `json:"assessment"`
	}
	decodeOutput(t, raw, &value)
	return value.Assessment
}

func decodeOutput(t *testing.T, raw json.RawMessage, target any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatal(err)
	}
}

func adapterProposalRequest(t *testing.T, kind learning.ProposalType) learning.ProposalRequest {
	t.Helper()
	reference := map[string]any{
		"knowledge_revision_id": testKnowledgeRevisionID,
		"document_revision_id":  testDocumentRevisionID,
		"node_id":               testNodeID,
		"node_revision_id":      testNodeRevisionID,
		"range":                 map[string]int{"start": 0, "end": len("canonical knowledge")},
		"slice":                 "canonical knowledge",
		"slice_sha256":          hashText("canonical knowledge"),
	}
	activity := map[string]any{
		"activity_id":             testActivityID,
		"session_id":              testSessionID,
		"goal_revision_id":        testGoalRevisionID,
		"route_revision_id":       testRouteRevisionID,
		"route_step_id":           testRouteStepID,
		"knowledge_revision_id":   testKnowledgeRevisionID,
		"target_node_id":          testNodeID,
		"target_node_revision_id": testNodeRevisionID,
		"rubric": map[string]any{
			"rubric_revision": "rubric-v1",
			"items": []map[string]any{{
				"rubric_item_id": "rubric-item-1", "criterion": "supported answer",
				"required_reference_ids": []string{testNodeRevisionID},
			}},
		},
		"knowledge_references": []any{reference},
	}
	workItem := map[string]any{
		"allowed_actions":              []string{},
		"allowed_assessment_decisions": []string{},
		"goal_revision": map[string]any{
			"goal_revision_id": testGoalRevisionID,
		},
		"route_revision": map[string]any{
			"route_revision_id":     testRouteRevisionID,
			"goal_revision_id":      testGoalRevisionID,
			"knowledge_revision_id": testKnowledgeRevisionID,
			"steps": []map[string]any{{
				"route_step_id":    testRouteStepID,
				"node_id":          testNodeID,
				"node_revision_id": testNodeRevisionID,
			}},
		},
		"activity": activity,
		"attempt": map[string]any{
			"attempt_id":  testAttemptID,
			"session_id":  testSessionID,
			"activity_id": testActivityID,
			"answer":      "candidate answer",
		},
	}
	if kind == learning.ProposalFreeAnswer {
		workItem["free_question"] = map[string]any{
			"free_question_id":      testFreeQuestionID,
			"session_id":            testSessionID,
			"focus_frame_id":        testFocusFrameID,
			"knowledge_revision_id": testKnowledgeRevisionID,
		}
	}
	contextValue := map[string]any{
		"schema_version": "go-cli-context-v1",
		"work_item":      workItem,
		"retrieval": map[string]any{
			"knowledge_revision_id": testKnowledgeRevisionID,
			"hits":                  []any{reference},
		},
	}
	input, err := json.Marshal(contextValue)
	if err != nil {
		t.Fatal(err)
	}
	request := learning.ProposalRequest{
		RequestID: testRequestID, Type: kind, AggregateType: "session", AggregateID: testSessionID,
		AggregateVersion: 7, GoalRevisionID: testGoalRevisionID, RouteRevisionID: testRouteRevisionID,
		RouteStepID: testRouteStepID, FocusNodeRevisionID: testNodeRevisionID,
		KnowledgeRevisionID: testKnowledgeRevisionID, NodeRevisionIDs: []string{testNodeRevisionID}, Input: input,
	}
	switch kind {
	case learning.ProposalRoute:
		request.TutoringState = "Diagnostic"
	case learning.ProposalActivity:
		request.TutoringState = "RouteActive"
	case learning.ProposalAssessment:
		request.TutoringState = "Evaluating"
		request.ActivityID = testActivityID
		request.AttemptID = testAttemptID
	case learning.ProposalFreeAnswer:
		request.TutoringState = "FreeQuestion"
		request.FreeQuestionID = testFreeQuestionID
		request.FocusFrameID = testFocusFrameID
	case learning.ProposalExplanation:
		request.TutoringState = "RouteActive"
	}
	return request
}

func newLLMClient(t *testing.T, rawURL string, timeout time.Duration) *llm.Client {
	t.Helper()
	base, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := llm.New(llm.Options{
		BaseURL: base, Model: "strict-fake", APIKey: "test-key",
		ContextWindow: 8192, MinimumContext: 4096, Timeout: timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func hashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func modelCategory(err error) string {
	if failure, ok := err.(interface{ ModelCategory() string }); ok {
		return failure.ModelCategory()
	}
	return fmt.Sprintf("%T", err)
}
