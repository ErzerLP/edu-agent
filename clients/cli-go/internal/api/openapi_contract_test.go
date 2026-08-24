package api

import (
	"os"
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
	assertResponses(t, paths, "/readyz", "get", "200", "503")
	assertResponses(t, paths, "/v1/pairings/exchange", "post", "201", "400", "401", "429", "500")
	assertResponses(t, paths, "/v1/devices", "get", "200", "401", "403", "429", "500")
	assertResponses(t, paths, "/v1/devices/{deviceID}", "delete", "204", "400", "401", "403", "404", "429", "500")
	assertResponses(t, paths, "/v1/model/capabilities", "get", "200", "401", "403", "429", "500")
	assertResponses(t, paths, "/v1/knowledge/revisions/head", "get", "200", "401", "403", "404", "429", "500", "503")
	assertResponses(t, paths, "/v1/knowledge/imports", "post", "200", "201", "400", "401", "403", "409", "413", "422", "429", "500", "503")
	assertRequestSchemaRef(t, paths, "/v1/pairings/exchange", "post", "#/components/schemas/PairingExchangeRequest")
	assertResponseSchemaRef(t, paths, "/v1/pairings/exchange", "post", "201", "#/components/schemas/IssuedCredential")
	assertResponseSchemaRef(t, paths, "/v1/model/capabilities", "get", "200", "#/components/schemas/ModelCapabilities")
	assertRequestSchemaRef(t, paths, "/v1/knowledge/imports", "post", "#/components/schemas/KnowledgeImportRequest")
	assertResponseSchemaRef(t, paths, "/v1/knowledge/imports", "post", "200", "#/components/schemas/KnowledgeImportResult")
	assertResponseSchemaRef(t, paths, "/v1/knowledge/imports", "post", "201", "#/components/schemas/KnowledgeImportResult")
	assertResponseSchemaRef(t, paths, "/v1/knowledge/imports", "post", "409", "#/components/schemas/KnowledgeConflict")
	assertInlineResponseRequired(t, paths, "/v1/devices", "get", "200", "devices")
	assertInlineResponseRequired(t, paths, "/v1/knowledge/revisions/head", "get", "200", "revision")

	components := childMap(t, document, "components")
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
	assertEnum(t, childMap(t, childMap(t, schemas, "DocumentIdentityResolution"), "properties"), "action", "preserve", "new")
	assertEnum(t, childMap(t, childMap(t, schemas, "NodeIdentityResolution"), "properties"), "action", "preserve", "new", "rewrite", "split", "merge")
	assertDiscriminator(t, childMap(t, schemas, "TutoringActionRequest"), "action")
	assertDiscriminator(t, childMap(t, schemas, "AssessmentDecisionRequest"), "kind")
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
	for _, status := range statuses {
		if _, ok := responses[status]; !ok {
			t.Errorf("%s %s is missing response %s", method, path, status)
		}
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
	assertRequired(t, schema, fields...)
}

func assertRequired(t *testing.T, schema map[string]any, fields ...string) {
	t.Helper()
	values, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("schema required is %T", schema["required"])
	}
	required := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			required = append(required, text)
		}
	}
	for _, field := range fields {
		if !slices.Contains(required, field) {
			t.Errorf("required fields %v do not contain %s", required, field)
		}
	}
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
	for _, value := range values {
		if !slices.Contains(actual, value) {
			t.Errorf("%s enum %v does not contain %s", field, actual, value)
		}
	}
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
