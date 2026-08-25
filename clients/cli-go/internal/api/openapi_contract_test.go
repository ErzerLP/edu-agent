package api

import (
	"os"
	"reflect"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestOpenAPIContainsFoundationClientContract(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../../../server/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	paths := childMap(t, document, "paths")
	assertOperation(t, paths, "/readyz", "get", "getReadiness", "")
	assertOperation(t, paths, "/v1/pairings/exchange", "post", "exchangePairingCode", "")
	assertOperation(t, paths, "/v1/devices", "get", "listDevices", "devices:read")
	assertOperation(t, paths, "/v1/devices/{deviceID}", "delete", "revokeDevice", "devices:manage")
	assertOperation(t, paths, "/v1/model/capabilities", "get", "probeModelCapabilities", "model:probe")
	assertOperation(t, paths, "/v1/knowledge/revisions/head", "get", "getKnowledgeHead", "knowledge:read")
	assertOperation(t, paths, "/v1/knowledge/imports", "post", "importKnowledgeDocuments", "knowledge:write")
	assertOperation(t, paths, "/v1/knowledge/retrievals", "post", "retrieveKnowledge", "knowledge:read")
	assertOperation(t, paths, "/v1/learning/goals", "post", "createLearningGoal", "learning:write")
	assertOperation(t, paths, "/v1/tutoring/sessions", "post", "createTutoringSession", "learning:write")
	assertOperation(t, paths, "/v1/tutoring/proposals", "post", "proposeTutoringArtifact", "learning:write")
	assertOperation(t, paths, "/v1/tutoring/sessions/{sessionID}/actions", "post", "applyTutoringAction", "learning:write")
	assertOperation(t, paths, "/v1/learning/assessments/{assessmentID}/decisions", "post", "decideLearningAssessment", "learning:write")
	assertOperation(t, paths, "/v1/tutoring/sessions/current", "get", "getCurrentTutoringSession", "learning:read")
	assertOperation(t, paths, "/v1/tutoring/sessions/{sessionID}", "get", "getTutoringSession", "learning:read")
	assertOperation(t, paths, "/v1/learning/timeline", "get", "listLearningTimeline", "learning:read")
	assertOperation(t, paths, "/v1/learning/routes", "get", "listLearningRoutes", "learning:read")
	assertOperation(t, paths, "/v1/learning/nodes/{nodeRevisionID}", "get", "getLearningNode", "learning:read")
	assertOperation(t, paths, "/v1/learning/evidence", "get", "listLearningEvidence", "learning:read")
	assertOperation(t, paths, "/v1/learning/reviews", "get", "listLearningReviews", "learning:read")
	assertOperation(t, paths, "/v1/learning/projections/status", "get", "getLearningProjectionStatus", "learning:read")
	assertResponses(t, paths, "/readyz", "get", "200", "503")
	assertResponses(t, paths, "/v1/pairings/exchange", "post", "201", "400", "401", "429", "500")
	assertResponses(t, paths, "/v1/devices", "get", "200", "401", "403", "429", "500")
	assertResponses(t, paths, "/v1/devices/{deviceID}", "delete", "204", "400", "401", "403", "404", "429", "500")
	assertResponses(t, paths, "/v1/model/capabilities", "get", "200", "401", "403", "429", "500")
	assertResponses(t, paths, "/v1/knowledge/revisions/head", "get", "200", "401", "403", "404", "429", "500", "503")
	assertResponses(t, paths, "/v1/knowledge/imports", "post", "200", "201", "400", "401", "403", "409", "413", "422", "429", "500", "503")
	assertResponses(t, paths, "/v1/knowledge/retrievals", "post", "200", "400", "401", "403", "404", "413", "429", "500", "503")
	for _, endpoint := range []string{"/v1/learning/goals", "/v1/tutoring/sessions", "/v1/tutoring/sessions/{sessionID}/actions", "/v1/learning/assessments/{assessmentID}/decisions"} {
		assertResponses(t, paths, endpoint, "post", "200", "201", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503")
	}
	assertResponses(t, paths, "/v1/tutoring/proposals", "post", "201", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503")
	for _, endpoint := range []string{"/v1/tutoring/sessions/current", "/v1/tutoring/sessions/{sessionID}"} {
		assertResponses(t, paths, endpoint, "get", "200", "400", "401", "403", "404", "429", "500", "503")
	}
	for _, endpoint := range []string{"/v1/learning/timeline", "/v1/learning/routes", "/v1/learning/evidence", "/v1/learning/reviews"} {
		assertResponses(t, paths, endpoint, "get", "200", "400", "401", "403", "409", "429", "500", "503")
	}
	assertResponses(t, paths, "/v1/learning/nodes/{nodeRevisionID}", "get", "200", "400", "401", "403", "404", "429", "500", "503")
	assertResponses(t, paths, "/v1/learning/projections/status", "get", "200", "400", "401", "403", "429", "500", "503")
	assertRequestSchemaRef(t, paths, "/v1/pairings/exchange", "post", "#/components/schemas/PairingExchangeRequest")
	assertResponseSchemaRef(t, paths, "/v1/pairings/exchange", "post", "201", "#/components/schemas/IssuedCredential")
	assertResponseSchemaRef(t, paths, "/v1/model/capabilities", "get", "200", "#/components/schemas/ModelCapabilities")
	assertRequestSchemaRef(t, paths, "/v1/knowledge/imports", "post", "#/components/schemas/KnowledgeImportRequest")
	assertRequestSchemaRef(t, paths, "/v1/knowledge/retrievals", "post", "#/components/schemas/KnowledgeRetrievalRequest")
	assertRequestSchemaRef(t, paths, "/v1/learning/goals", "post", "#/components/schemas/LearningGoalRequest")
	assertRequestSchemaRef(t, paths, "/v1/tutoring/sessions", "post", "#/components/schemas/TutoringSessionRequest")
	assertRequestSchemaRef(t, paths, "/v1/tutoring/proposals", "post", "#/components/schemas/TutoringProposalRequest")
	assertRequestSchemaRef(t, paths, "/v1/tutoring/sessions/{sessionID}/actions", "post", "#/components/schemas/TutoringActionRequest")
	assertRequestSchemaRef(t, paths, "/v1/learning/assessments/{assessmentID}/decisions", "post", "#/components/schemas/AssessmentDecisionRequest")
	assertResponseSchemaRef(t, paths, "/v1/knowledge/imports", "post", "200", "#/components/schemas/KnowledgeImportResult")
	assertResponseSchemaRef(t, paths, "/v1/knowledge/imports", "post", "201", "#/components/schemas/KnowledgeImportResult")
	assertResponseSchemaRef(t, paths, "/v1/knowledge/imports", "post", "409", "#/components/schemas/KnowledgeConflict")
	assertResponseSchemaRef(t, paths, "/v1/knowledge/retrievals", "post", "200", "#/components/schemas/KnowledgeRetrievalResult")
	for endpoint, schema := range map[string]string{
		"/v1/learning/goals":                                "GoalOperationResult",
		"/v1/tutoring/sessions":                             "SessionOperationResult",
		"/v1/tutoring/sessions/{sessionID}/actions":         "SessionOperationResult",
		"/v1/learning/assessments/{assessmentID}/decisions": "AssessmentDecisionOperationResult",
	} {
		assertResponseSchemaRef(t, paths, endpoint, "post", "200", "#/components/schemas/"+schema)
		assertResponseSchemaRef(t, paths, endpoint, "post", "201", "#/components/schemas/"+schema)
	}
	assertResponseSchemaRef(t, paths, "/v1/tutoring/proposals", "post", "201", "#/components/schemas/TutoringProposal")
	assertResponseSchemaRef(t, paths, "/v1/tutoring/sessions/current", "get", "200", "#/components/schemas/SessionView")
	assertResponseSchemaRef(t, paths, "/v1/tutoring/sessions/{sessionID}", "get", "200", "#/components/schemas/SessionView")
	assertResponseSchemaRef(t, paths, "/v1/learning/timeline", "get", "200", "#/components/schemas/TimelinePage")
	assertResponseSchemaRef(t, paths, "/v1/learning/routes", "get", "200", "#/components/schemas/RoutesPage")
	assertResponseSchemaRef(t, paths, "/v1/learning/nodes/{nodeRevisionID}", "get", "200", "#/components/schemas/NodeView")
	assertResponseSchemaRef(t, paths, "/v1/learning/evidence", "get", "200", "#/components/schemas/EvidencePage")
	assertResponseSchemaRef(t, paths, "/v1/learning/reviews", "get", "200", "#/components/schemas/ReviewsPage")
	assertResponseSchemaRef(t, paths, "/v1/learning/projections/status", "get", "200", "#/components/schemas/ProjectionStatus")
	assertQueryParameter(t, paths, "/v1/learning/routes", "get", "current_only", "boolean")
	assertParameterList(t, paths, "/v1/tutoring/sessions/{sessionID}/actions", "post", "#/components/parameters/SessionID")
	assertParameterList(t, paths, "/v1/learning/assessments/{assessmentID}/decisions", "post", "#/components/parameters/AssessmentID")
	assertParameterList(t, paths, "/v1/tutoring/sessions/{sessionID}", "get", "#/components/parameters/SessionID")
	assertParameterList(t, paths, "/v1/learning/timeline", "get", "#/components/parameters/Cursor", "#/components/parameters/PageLimit", "session_id")
	assertParameterList(t, paths, "/v1/learning/routes", "get", "#/components/parameters/Cursor", "#/components/parameters/PageLimit", "current_only")
	assertParameterList(t, paths, "/v1/learning/nodes/{nodeRevisionID}", "get", "#/components/parameters/NodeRevisionID")
	assertParameterList(t, paths, "/v1/learning/evidence", "get", "#/components/parameters/Cursor", "#/components/parameters/PageLimit", "node_revision_id")
	assertParameterList(t, paths, "/v1/learning/reviews", "get", "#/components/parameters/Cursor", "#/components/parameters/PageLimit", "#/components/parameters/DueBefore")
	assertInlineResponseRequired(t, paths, "/v1/devices", "get", "200", "devices")
	assertInlineResponseRequired(t, paths, "/v1/knowledge/revisions/head", "get", "200", "revision")

	components := childMap(t, document, "components")
	parameters := childMap(t, components, "parameters")
	assertParameterDefinition(t, parameters, "Cursor", "cursor", "query", false, map[string]any{"type": "string"})
	assertParameterDefinition(t, parameters, "PageLimit", "limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "default": 50})
	assertParameterDefinition(t, parameters, "DueBefore", "due_before", "query", false, map[string]any{"type": "string", "format": "date-time"})
	assertParameterDefinition(t, parameters, "SessionID", "sessionID", "path", true, map[string]any{"$ref": "#/components/schemas/LearningUUID"})
	assertParameterDefinition(t, parameters, "AssessmentID", "assessmentID", "path", true, map[string]any{"$ref": "#/components/schemas/LearningUUID"})
	assertParameterDefinition(t, parameters, "NodeRevisionID", "nodeRevisionID", "path", true, map[string]any{"$ref": "#/components/schemas/LearningUUID"})
	assertInlineParameterSchema(t, paths, "/v1/learning/timeline", "get", "session_id", map[string]any{"$ref": "#/components/schemas/LearningUUID"})
	assertInlineParameterSchema(t, paths, "/v1/learning/routes", "get", "current_only", map[string]any{"type": "boolean", "default": false})
	assertInlineParameterSchema(t, paths, "/v1/learning/evidence", "get", "node_revision_id", map[string]any{"$ref": "#/components/schemas/LearningUUID"})
	schemas := childMap(t, components, "schemas")
	importRequest := childMap(t, schemas, "KnowledgeImportRequest")
	assertRequired(t, importRequest, "operation_id", "expected_parent_revision_id", "source", "documents")
	importProperties := childMap(t, importRequest, "properties")
	for _, field := range []string{"identity_review_basis_hash", "identity_review_operation_id", "identity_review_receipt", "document_resolutions", "node_resolutions"} {
		if _, ok := importProperties[field]; !ok {
			t.Fatalf("KnowledgeImportRequest is missing %s", field)
		}
	}
	assertClosedRequired(t, schemas, "Readiness", "status", "components")
	assertClosedRequired(t, schemas, "HealthComponent", "status")
	assertClosedRequired(t, schemas, "IssuedCredential", "device", "token")
	assertClosedRequired(t, schemas, "Device", "id", "display_name", "created_at")
	assertClosedRequired(t, schemas, "ModelCapabilities", "profile", "compatible", "context_window", "minimum_context_window", "system_user_assistant_messages", "non_streaming", "structured_json", "native_json_schema", "streaming", "tool_calls", "incompatibility_reasons")
	assertClosedRequired(t, schemas, "KnowledgeImportRequest", "operation_id", "expected_parent_revision_id", "source", "documents")
	assertClosedRequired(t, schemas, "DocumentIdentityResolution", "locator", "action", "reason")
	assertClosedRequired(t, schemas, "NodeIdentityResolution", "locator", "action", "reason")
	assertClosedRequired(t, schemas, "KnowledgeImportResult", "revision", "unchanged")
	assertClosedRequired(t, schemas, "KnowledgeRevision", "revision_id", "revision_no", "parent_revision_id", "manifest_hash", "source", "created_by_device_id", "created_at", "canonicalizer_version", "parser_version", "indexer_version", "identity_policy_version")
	assertClosedRequired(t, schemas, "IdentityCandidate", "stable_id", "revision_id", "reason_code")
	assertClosedRequired(t, schemas, "DocumentIdentityReview", "path", "locator", "reason_code", "candidates")
	assertClosedRequired(t, schemas, "NodeIdentityReview", "path", "locator", "preorder", "reason_code", "candidates")
	assertClosedRequired(t, schemas, "IdentityReview", "identity_review_basis_hash", "identity_review_operation_id", "identity_review_receipt", "document_reviews", "node_reviews")
	assertClosedRequired(t, schemas, "KnowledgeConflict", "error")
	assertClosedRequired(t, schemas, "LearningGoalRequest", "operation_id", "payload_schema_version", "aggregate_type", "aggregate_id", "expected_version", "text", "source")
	assertClosedRequired(t, schemas, "TutoringSessionRequest", "operation_id", "payload_schema_version", "aggregate_type", "aggregate_id", "expected_version", "goal_revision_id")
	assertClosedRequired(t, schemas, "TutoringProposalRequest", "request_id", "proposal_type", "aggregate_type", "aggregate_id", "aggregate_version", "knowledge_revision_id", "node_revision_ids", "input")
	for _, schema := range []string{"GoalOperationResult", "SessionOperationResult", "AssessmentDecisionOperationResult"} {
		assertClosedRequired(t, schemas, schema, "status", "replayed", "archived", "aggregate_type", "aggregate_id", "aggregate_version", "first_event_seq", "last_event_seq", "projection_as_of_event_seq", "result")
	}
	assertClosedRequired(t, schemas, "TutoringProposal", "proposal_id", "schema_version", "input_hash", "proposal_type", "aggregate_type", "aggregate_id", "aggregate_version", "knowledge_revision_id", "frozen_request", "model_id", "model_parameters", "prompt_revision", "attempt_categories", "created_at")
	assertClosedRequired(t, schemas, "ActionDirectExposureRequest", "operation_id", "payload_schema_version", "aggregate_type", "aggregate_id", "expected_version", "action", "exposure_kind", "exposure_text")
	assertClosedRequired(t, schemas, "ActionProposalExposureRequest", "operation_id", "payload_schema_version", "aggregate_type", "aggregate_id", "expected_version", "action", "proposal_id")
	for _, schema := range []string{"GoalVersionConflictEnvelope", "SessionVersionConflictEnvelope", "AssessmentDispositionConflictEnvelope", "TutoringActionAssessmentDispositionConflictEnvelope", "LearningIdempotencyConflictEnvelope", "TutoringProposalStateConflictEnvelope", "TutoringActionStateConflictEnvelope", "AssessmentDecisionStateConflictEnvelope", "LearningCursorConflictEnvelope"} {
		assertClosedRequired(t, schemas, schema)
	}
	assertClosedRequired(t, schemas, "SessionView", "metadata", "session", "estimated_active_time", "work_item")
	assertClosedRequired(t, schemas, "SessionWorkItem", "allowed_actions", "allowed_assessment_decisions")
	for _, schema := range []string{"GoalRevision", "RouteRevision", "RouteStep", "Activity", "Attempt", "AssessmentArtifact", "AssessmentDecision", "FreeQuestion", "FreeAnswer", "ProjectionMetadata", "EvidencePage", "ReviewsPage", "RoutesPage", "NodeView"} {
		assertClosedRequired(t, schemas, schema)
	}
	assertEnum(t, childMap(t, childMap(t, schemas, "DocumentIdentityResolution"), "properties"), "action", "preserve", "new")
	assertEnum(t, childMap(t, childMap(t, schemas, "NodeIdentityResolution"), "properties"), "action", "preserve", "new", "rewrite", "split", "merge")
	assertDiscriminator(t, childMap(t, schemas, "TutoringActionRequest"), "action")
	actionSchema := childMap(t, schemas, "TutoringActionRequest")
	assertDiscriminatorMappings(t, actionSchema, "start_diagnostic", "apply_route", "issue_activity", "present_activity", "submit_attempt", "record_assessment", "acknowledge_feedback", "present_review", "record_exposure", "ask_free_question", "record_free_answer", "convert_free_answer_to_quiz", "resume_focus", "end_activity", "switch_goal", "complete_session")
	assertDiscriminatorMappingRefs(t, actionSchema, map[string]string{
		"start_diagnostic": "#/components/schemas/ActionNoFieldsRequest", "apply_route": "#/components/schemas/ActionProposalRequest", "issue_activity": "#/components/schemas/ActionProposalRequest", "present_activity": "#/components/schemas/ActionNoFieldsRequest", "submit_attempt": "#/components/schemas/ActionAttemptRequest", "record_assessment": "#/components/schemas/ActionAssessmentRequest", "acknowledge_feedback": "#/components/schemas/ActionNoFieldsRequest", "present_review": "#/components/schemas/ActionProposalRequest", "record_exposure": "#/components/schemas/ActionExposureRequest", "ask_free_question": "#/components/schemas/ActionQuestionRequest", "record_free_answer": "#/components/schemas/ActionProposalRequest", "convert_free_answer_to_quiz": "#/components/schemas/ActionAttachedQuizRequest", "resume_focus": "#/components/schemas/ActionNoFieldsRequest", "end_activity": "#/components/schemas/ActionNoFieldsRequest", "switch_goal": "#/components/schemas/ActionSwitchGoalRequest", "complete_session": "#/components/schemas/ActionNoFieldsRequest",
	})
	decisionSchema := childMap(t, schemas, "AssessmentDecisionRequest")
	assertDiscriminator(t, decisionSchema, "kind")
	assertDiscriminatorMappings(t, decisionSchema, "confirm", "override", "void")
	assertDiscriminatorMappingRefs(t, decisionSchema, map[string]string{"confirm": "#/components/schemas/AssessmentConfirmRequest", "override": "#/components/schemas/AssessmentOverrideRequest", "void": "#/components/schemas/AssessmentVoidRequest"})
	assertOneOfRefs(t, childMap(t, schemas, "LearningGoalConflict"), "#/components/schemas/GoalVersionConflictEnvelope", "#/components/schemas/LearningIdempotencyConflictEnvelope")
	assertOneOfRefs(t, childMap(t, schemas, "TutoringSessionConflict"), "#/components/schemas/SessionVersionConflictEnvelope", "#/components/schemas/LearningIdempotencyConflictEnvelope")
	assertOneOfRefs(t, childMap(t, schemas, "TutoringProposalConflict"), "#/components/schemas/TutoringProposalStateConflictEnvelope")
	assertOneOfRefs(t, childMap(t, schemas, "TutoringActionConflict"), "#/components/schemas/SessionVersionConflictEnvelope", "#/components/schemas/TutoringActionAssessmentDispositionConflictEnvelope", "#/components/schemas/TutoringActionStateConflictEnvelope")
	assertOneOfRefs(t, childMap(t, schemas, "AssessmentDecisionConflict"), "#/components/schemas/SessionVersionConflictEnvelope", "#/components/schemas/AssessmentDispositionConflictEnvelope", "#/components/schemas/AssessmentDecisionStateConflictEnvelope")
	assertOneOfRefs(t, childMap(t, schemas, "LearningCursorConflict"), "#/components/schemas/LearningCursorConflictEnvelope")
	assertOneOfRefs(t, childMap(t, schemas, "ActionExposureRequest"), "#/components/schemas/ActionDirectExposureRequest", "#/components/schemas/ActionProposalExposureRequest")
	assertLearningResponseRefs(t, paths)
	knowledgeConflict := childMap(t, schemas, "KnowledgeConflict")
	properties := childMap(t, knowledgeConflict, "properties")
	for _, field := range []string{"error", "current_revision_id", "identity_review"} {
		if _, ok := properties[field]; !ok {
			t.Fatalf("KnowledgeConflict is missing %s", field)
		}
	}
}

func assertOperation(t *testing.T, paths map[string]any, path, method, operationID, scope string) {
	t.Helper()
	operation := childMap(t, childMap(t, paths, path), method)
	if operation["operationId"] != operationID {
		t.Errorf("%s %s operationId = %v, want %s", method, path, operation["operationId"], operationID)
	}
	if scope == "" {
		if _, ok := operation["x-required-scope"]; ok {
			t.Errorf("%s %s unexpectedly declares a required scope", method, path)
		}
		return
	}
	if operation["x-required-scope"] != scope {
		t.Errorf("%s %s scope = %v, want %s", method, path, operation["x-required-scope"], scope)
	}
}

func assertResponses(t *testing.T, paths map[string]any, path, method string, statuses ...string) {
	t.Helper()
	operation := childMap(t, childMap(t, paths, path), method)
	responses := childMap(t, operation, "responses")
	actual := make([]string, 0, len(responses))
	for status := range responses {
		actual = append(actual, status)
	}
	slices.Sort(actual)
	expected := append([]string(nil), statuses...)
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		t.Errorf("%s %s responses = %v, want %v", method, path, actual, expected)
	}
}

func assertRequestSchemaRef(t *testing.T, paths map[string]any, path, method, want string) {
	t.Helper()
	operation := childMap(t, childMap(t, paths, path), method)
	requestBody := childMap(t, operation, "requestBody")
	schema := jsonSchema(t, requestBody)
	if schema["$ref"] != want {
		t.Errorf("%s %s request schema = %v, want %s", method, path, schema["$ref"], want)
	}
}

func assertResponseSchemaRef(t *testing.T, paths map[string]any, path, method, status, want string) {
	t.Helper()
	operation := childMap(t, childMap(t, paths, path), method)
	response := childMap(t, childMap(t, operation, "responses"), status)
	schema := jsonSchema(t, response)
	if schema["$ref"] != want {
		t.Errorf("%s %s response %s schema = %v, want %s", method, path, status, schema["$ref"], want)
	}
}

func assertInlineResponseRequired(t *testing.T, paths map[string]any, path, method, status string, fields ...string) {
	t.Helper()
	operation := childMap(t, childMap(t, paths, path), method)
	response := childMap(t, childMap(t, operation, "responses"), status)
	schema := jsonSchema(t, response)
	if schema["additionalProperties"] != false {
		t.Errorf("%s %s response %s additionalProperties = %v, want false", method, path, status, schema["additionalProperties"])
	}
	assertRequired(t, schema, fields...)
}

func jsonSchema(t *testing.T, parent map[string]any) map[string]any {
	t.Helper()
	content := childMap(t, parent, "content")
	media := childMap(t, content, "application/json")
	return childMap(t, media, "schema")
}

func assertClosedRequired(t *testing.T, schemas map[string]any, name string, fields ...string) {
	t.Helper()
	schema := childMap(t, schemas, name)
	if schema["additionalProperties"] != false {
		t.Errorf("%s additionalProperties = %v, want false", name, schema["additionalProperties"])
	}
	if len(fields) > 0 {
		assertRequiredExact(t, schema, fields...)
	}
}

func assertRequired(t *testing.T, schema map[string]any, fields ...string) {
	t.Helper()
	required := requiredFields(t, schema)
	for _, field := range fields {
		if !slices.Contains(required, field) {
			t.Errorf("required fields %v do not contain %s", required, field)
		}
	}
}

func assertRequiredExact(t *testing.T, schema map[string]any, fields ...string) {
	t.Helper()
	actual := requiredFields(t, schema)
	expected := append([]string(nil), fields...)
	slices.Sort(actual)
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		t.Errorf("required fields = %v, want %v", actual, expected)
	}
}

func requiredFields(t *testing.T, schema map[string]any) []string {
	t.Helper()
	values, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("schema required is %T", schema["required"])
	}
	required := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("required value is %T", value)
		}
		required = append(required, text)
	}
	return required
}

func assertEnum(t *testing.T, properties map[string]any, field string, values ...string) {
	t.Helper()
	fieldSchema := childMap(t, properties, field)
	raw, ok := fieldSchema["enum"].([]any)
	if !ok {
		t.Fatalf("%s enum is %T", field, fieldSchema["enum"])
	}
	actual := make([]string, 0, len(raw))
	for _, value := range raw {
		if text, ok := value.(string); ok {
			actual = append(actual, text)
		}
	}
	expected := append([]string(nil), values...)
	slices.Sort(actual)
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		t.Errorf("%s enum = %v, want %v", field, actual, expected)
	}
}

func assertQueryParameter(t *testing.T, paths map[string]any, path, method, name, schemaType string) {
	t.Helper()
	operation := childMap(t, childMap(t, paths, path), method)
	parameters, ok := operation["parameters"].([]any)
	if !ok {
		t.Fatalf("%s %s parameters are %T", method, path, operation["parameters"])
	}
	for _, raw := range parameters {
		parameter, ok := raw.(map[string]any)
		if !ok || parameter["name"] != name {
			continue
		}
		schema := childMap(t, parameter, "schema")
		if schema["type"] != schemaType {
			t.Errorf("%s %s parameter %s type=%v, want %s", method, path, name, schema["type"], schemaType)
		}
		return
	}
	t.Errorf("%s %s is missing query parameter %s", method, path, name)
}

func assertDiscriminator(t *testing.T, schema map[string]any, property string) {
	t.Helper()
	discriminator := childMap(t, schema, "discriminator")
	if discriminator["propertyName"] != property {
		t.Errorf("discriminator propertyName = %v, want %s", discriminator["propertyName"], property)
	}
	mapping := childMap(t, discriminator, "mapping")
	if len(mapping) == 0 {
		t.Error("discriminator mapping is empty")
	}
}

func assertDiscriminatorMappings(t *testing.T, schema map[string]any, values ...string) {
	t.Helper()
	mapping := childMap(t, childMap(t, schema, "discriminator"), "mapping")
	actual := make([]string, 0, len(mapping))
	for value := range mapping {
		actual = append(actual, value)
	}
	expected := append([]string(nil), values...)
	slices.Sort(actual)
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		t.Errorf("discriminator mappings = %v, want %v", actual, expected)
	}
}

func assertDiscriminatorMappingRefs(t *testing.T, schema map[string]any, expected map[string]string) {
	t.Helper()
	mapping := childMap(t, childMap(t, schema, "discriminator"), "mapping")
	actual := make(map[string]string, len(mapping))
	for key, raw := range mapping {
		value, ok := raw.(string)
		if !ok {
			t.Fatalf("discriminator mapping %s is %T", key, raw)
		}
		actual[key] = value
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("discriminator mapping refs = %v, want %v", actual, expected)
	}
}

func assertParameterDefinition(t *testing.T, parameters map[string]any, key, name, location string, required bool, expectedSchema map[string]any) {
	t.Helper()
	parameter := childMap(t, parameters, key)
	if parameter["name"] != name || parameter["in"] != location {
		t.Errorf("parameter %s identity = %v/%v, want %s/%s", key, parameter["name"], parameter["in"], name, location)
	}
	actualRequired, present := parameter["required"]
	if required {
		if !present || actualRequired != true {
			t.Errorf("parameter %s required = %v, want true", key, actualRequired)
		}
	} else if present {
		t.Errorf("parameter %s unexpectedly declares required=%v", key, actualRequired)
	}
	if schema := childMap(t, parameter, "schema"); !reflect.DeepEqual(schema, expectedSchema) {
		t.Errorf("parameter %s schema = %v, want %v", key, schema, expectedSchema)
	}
}

func assertInlineParameterSchema(t *testing.T, paths map[string]any, path, method, name string, expected map[string]any) {
	t.Helper()
	operation := childMap(t, childMap(t, paths, path), method)
	parameters, ok := operation["parameters"].([]any)
	if !ok {
		t.Fatalf("%s %s parameters are %T", method, path, operation["parameters"])
	}
	for _, raw := range parameters {
		parameter, ok := raw.(map[string]any)
		if !ok || parameter["name"] != name {
			continue
		}
		if parameter["in"] != "query" {
			t.Errorf("%s %s parameter %s location=%v, want query", method, path, name, parameter["in"])
		}
		if schema := childMap(t, parameter, "schema"); !reflect.DeepEqual(schema, expected) {
			t.Errorf("%s %s parameter %s schema=%v, want %v", method, path, name, schema, expected)
		}
		return
	}
	t.Errorf("%s %s is missing inline parameter %s", method, path, name)
}

func assertParameterList(t *testing.T, paths map[string]any, path, method string, expected ...string) {
	t.Helper()
	operation := childMap(t, childMap(t, paths, path), method)
	parameters, ok := operation["parameters"].([]any)
	if !ok {
		t.Fatalf("%s %s parameters are %T", method, path, operation["parameters"])
	}
	actual := make([]string, 0, len(parameters))
	for _, raw := range parameters {
		parameter, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("%s %s parameter is %T", method, path, raw)
		}
		if ref, ok := parameter["$ref"].(string); ok {
			actual = append(actual, ref)
			continue
		}
		name, ok := parameter["name"].(string)
		if !ok {
			t.Fatalf("%s %s inline parameter has no name", method, path)
		}
		actual = append(actual, name)
	}
	if !slices.Equal(actual, expected) {
		t.Errorf("%s %s parameters = %v, want %v", method, path, actual, expected)
	}
}

func assertOneOfRefs(t *testing.T, schema map[string]any, expected ...string) {
	t.Helper()
	target := schema
	if _, ok := target["oneOf"]; !ok {
		properties := childMap(t, target, "properties")
		target = childMap(t, properties, "result")
	}
	values, ok := target["oneOf"].([]any)
	if !ok {
		t.Fatalf("oneOf is %T", target["oneOf"])
	}
	actual := make([]string, 0, len(values))
	for _, raw := range values {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("oneOf entry is %T", raw)
		}
		ref, ok := entry["$ref"].(string)
		if !ok {
			t.Fatalf("oneOf entry ref is %T", entry["$ref"])
		}
		actual = append(actual, ref)
	}
	if !slices.Equal(actual, expected) {
		t.Errorf("oneOf refs = %v, want %v", actual, expected)
	}
}

func assertLearningResponseRefs(t *testing.T, paths map[string]any) {
	t.Helper()
	mutationErrors := map[string]string{"400": "BadRequest", "401": "Unauthorized", "403": "Forbidden", "413": "PayloadTooLarge", "422": "UnprocessableEntity", "429": "RateLimited", "500": "InternalError", "503": "DependencyUnavailable"}
	conflicts := map[string]string{
		"/v1/learning/goals":                                "LearningGoalConflict",
		"/v1/tutoring/sessions":                             "TutoringSessionConflict",
		"/v1/tutoring/proposals":                            "TutoringProposalConflict",
		"/v1/tutoring/sessions/{sessionID}/actions":         "TutoringActionConflict",
		"/v1/learning/assessments/{assessmentID}/decisions": "AssessmentDecisionConflict",
	}
	for path, conflict := range conflicts {
		for status, name := range mutationErrors {
			assertResponseComponentRef(t, paths, path, "post", status, "#/components/responses/"+name)
		}
		assertResponseComponentRef(t, paths, path, "post", "409", "#/components/responses/"+conflict)
	}
	queryErrors := map[string]string{"400": "BadRequest", "401": "Unauthorized", "403": "Forbidden", "429": "RateLimited", "500": "InternalError", "503": "DependencyUnavailable"}
	for _, path := range []string{"/v1/learning/timeline", "/v1/learning/routes", "/v1/learning/evidence", "/v1/learning/reviews"} {
		for status, name := range queryErrors {
			assertResponseComponentRef(t, paths, path, "get", status, "#/components/responses/"+name)
		}
		assertResponseComponentRef(t, paths, path, "get", "409", "#/components/responses/LearningCursorConflict")
	}
}

func assertResponseComponentRef(t *testing.T, paths map[string]any, path, method, status, expected string) {
	t.Helper()
	operation := childMap(t, childMap(t, paths, path), method)
	response := childMap(t, childMap(t, operation, "responses"), status)
	if response["$ref"] != expected {
		t.Errorf("%s %s response %s ref = %v, want %s", method, path, status, response["$ref"], expected)
	}
}

func childMap(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key]
	if !ok {
		t.Fatalf("missing key %s", key)
	}
	mapping, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("key %s is %T", key, value)
	}
	return mapping
}
