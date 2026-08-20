package api_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/getkin/kin-openapi/openapi3"
	"go.yaml.in/yaml/v3"
)

func TestOpenAPIParsesAndDeclaresKnowledgeRoutes(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatalf("OpenAPI paths are missing: %#v", document["paths"])
	}
	for route, expected := range map[string]struct {
		scope string
		codes []string
	}{
		"/v1/knowledge/revisions/head":                {"knowledge:read", []string{"200", "401", "403", "404", "429", "500"}},
		"/v1/knowledge/imports":                       {"knowledge:write", []string{"200", "201", "400", "401", "403", "409", "413", "422", "429", "500"}},
		"/v1/knowledge/revisions/{revisionID}/tree":   {"knowledge:read", []string{"200", "400", "401", "403", "404", "429", "500"}},
		"/v1/knowledge/revisions/{revisionID}/export": {"knowledge:read", []string{"200", "400", "401", "403", "404", "429", "500"}},
		"/v1/knowledge/retrievals":                    {"knowledge:read", []string{"200", "400", "401", "403", "404", "413", "429", "500"}},
	} {
		pathItem := paths[route].(map[string]any)
		var operation map[string]any
		for _, method := range []string{"get", "post"} {
			if candidate, exists := pathItem[method].(map[string]any); exists {
				operation = candidate
				break
			}
		}
		if operation == nil || operation["x-required-scope"] != expected.scope {
			t.Fatalf("route %s scope = %#v, want %s", route, operation["x-required-scope"], expected.scope)
		}
		responses, ok := operation["responses"].(map[string]any)
		if !ok {
			t.Fatalf("route %s responses are missing", route)
		}
		for _, code := range expected.codes {
			if _, exists := responses[code]; !exists {
				t.Errorf("route %s is missing response %s", route, code)
			}
		}
	}
}

func TestKnowledgeReviewAndRetrievalSchemasValidateDomainPayloads(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	decodeDomain := func(value any) map[string]any {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	const (
		revisionID = "10000000-0000-4000-8000-000000000001"
		documentID = "20000000-0000-4000-8000-000000000001"
		nodeID     = "30000000-0000-4000-8000-000000000001"
		nodeRevID  = "40000000-0000-4000-8000-000000000001"
	)
	review := knowledge.IdentityReview{
		BasisHash: strings.Repeat("a", 64), OperationID: revisionID, Receipt: strings.Repeat("b", 64),
		Documents: []knowledge.DocumentIdentityReview{{
			Path: "notes/topic.md", Locator: strings.Repeat("c", 64), ReasonCode: "document_match_ambiguous",
			Candidates: []knowledge.IdentityCandidate{{StableID: documentID, RevisionID: revisionID, ReasonCode: "same_path", Evidence: map[string]any{"path": "notes/topic.md"}}},
		}},
		Nodes: []knowledge.NodeIdentityReview{{
			Path: "notes/topic.md", Locator: strings.Repeat("d", 64), Preorder: 0, ReasonCode: "node_match_ambiguous",
			Candidates: []knowledge.IdentityCandidate{{StableID: nodeID, RevisionID: nodeRevID, ReasonCode: "node_similarity", Score: 500000, Evidence: map[string]any{"semantic_similarity": 500000}}},
		}},
	}
	conflict := map[string]any{
		"error":           map[string]any{"code": "identity_review_required", "message": "Knowledge import could not be committed", "request_id": "request-1"},
		"identity_review": decodeDomain(review),
	}
	conflictSchema := document.Paths.Find("/v1/knowledge/imports").Post.Responses.Value("409").Value.Content.Get("application/json").Schema.Value
	if err := conflictSchema.VisitJSON(conflict, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("real identity review response failed schema validation: %v", err)
	}

	sourceRange := knowledge.SourceRange{Start: 0, End: 7, StartLine: 1, EndLine: 1}
	candidate := knowledge.Candidate{Ordinal: 0, NodeRevisionID: nodeRevID, Score: 1000000, Title: "Topic", TitleSHA256: strings.Repeat("e", 64), HasChildren: false, LocalBodyScore: 1000000}
	retrieval := knowledge.RetrievalResult{
		KnowledgeRevisionID: revisionID, RetrieverVersion: knowledge.RetrieverVersion, SelectorVersion: knowledge.SelectorVersion,
		QueryContextVersion: knowledge.QueryContextVersion, SummarySnapshot: []string{}, DocumentShortlist: []string{"notes/topic.md"},
		Trace: []knowledge.RetrievalTrace{{
			Index: 0, Depth: 0, ParentNodeRevisionID: "50000000-0000-4000-8000-000000000001", Candidates: []knowledge.Candidate{candidate},
			Decisions: []knowledge.Decision{{NodeRevisionID: nodeRevID, Action: "select"}}, CandidateSetHash: strings.Repeat("f", 64), ReasonCode: "selector_not_configured", Degraded: true,
		}},
		Hits: []knowledge.RetrievalHit{{
			DocumentID: documentID, DocumentRevisionID: revisionID, NodeID: nodeID, NodeRevisionID: nodeRevID, Path: "notes/topic.md",
			HeadingRange: sourceRange, LocalBodyRange: sourceRange, SectionRange: sourceRange, CanonicalSlice: "# Topic", SliceSHA256: strings.Repeat("a", 64), Provenance: "canonical_markdown",
		}},
		Degraded: true,
	}
	retrievalSchema := document.Paths.Find("/v1/knowledge/retrievals").Post.Responses.Value("200").Value.Content.Get("application/json").Schema.Value
	if err := retrievalSchema.VisitJSON(decodeDomain(retrieval), openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("real retrieval response failed schema validation: %v", err)
	}
}

func TestKnowledgeConflictResponseSchemaValidatesRealPayload(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := document.Paths.Find("/v1/knowledge/imports").Post.Responses.Value("409")
	if response == nil || response.Value == nil {
		t.Fatal("knowledge import 409 response is missing")
	}
	media := response.Value.Content.Get("application/json")
	if media == nil || media.Schema == nil || media.Schema.Value == nil {
		t.Fatal("knowledge import 409 response schema is missing")
	}
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(`{
		"error":{"code":"revision_conflict","message":"Knowledge operation failed","request_id":"request-1"},
		"current_revision_id":"10000000-0000-4000-8000-000000000001"
	}`), &payload); err != nil {
		t.Fatal(err)
	}
	if err := media.Schema.Value.VisitJSON(payload, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("real conflict response failed schema validation: %v", err)
	}
	payload["current_revision_id"] = nil
	if err := media.Schema.Value.VisitJSON(payload, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("empty-head conflict response failed schema validation: %v", err)
	}
	payload["unexpected"] = true
	if err := media.Schema.Value.VisitJSON(payload, openapi3.EnableJSONSchema2020()); err == nil {
		t.Fatal("conflict schema accepted an undeclared top-level response field")
	}
}

func TestOpenAPIDeclaresLearningRoutesAndContracts(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	paths := raw["paths"].(map[string]any)
	contracts := map[string]struct {
		method string
		scope  string
		write  bool
		codes  []string
	}{
		"/v1/learning/goals":                                {"post", "learning:write", true, []string{"200", "201", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503"}},
		"/v1/tutoring/sessions":                             {"post", "learning:write", true, []string{"200", "201", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503"}},
		"/v1/tutoring/proposals":                            {"post", "learning:write", true, []string{"201", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503"}},
		"/v1/tutoring/sessions/{sessionID}/actions":         {"post", "learning:write", true, []string{"200", "201", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503"}},
		"/v1/learning/assessments/{assessmentID}/decisions": {"post", "learning:write", true, []string{"200", "201", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503"}},
		"/v1/tutoring/sessions/current":                     {"get", "learning:read", false, []string{"200", "400", "401", "403", "404", "429", "500", "503"}},
		"/v1/tutoring/sessions/{sessionID}":                 {"get", "learning:read", false, []string{"200", "400", "401", "403", "404", "429", "500", "503"}},
		"/v1/learning/timeline":                             {"get", "learning:read", false, []string{"200", "400", "401", "403", "409", "429", "500", "503"}},
		"/v1/learning/routes":                               {"get", "learning:read", false, []string{"200", "400", "401", "403", "409", "429", "500", "503"}},
		"/v1/learning/nodes/{nodeRevisionID}":               {"get", "learning:read", false, []string{"200", "400", "401", "403", "404", "429", "500", "503"}},
		"/v1/learning/evidence":                             {"get", "learning:read", false, []string{"200", "400", "401", "403", "409", "429", "500", "503"}},
		"/v1/learning/reviews":                              {"get", "learning:read", false, []string{"200", "400", "401", "403", "409", "429", "500", "503"}},
		"/v1/learning/projections/status":                   {"get", "learning:read", false, []string{"200", "400", "401", "403", "429", "500", "503"}},
	}
	for route, contract := range contracts {
		item, ok := paths[route].(map[string]any)
		if !ok {
			t.Fatalf("learning route %s is missing", route)
		}
		op, ok := item[contract.method].(map[string]any)
		if !ok || op["x-required-scope"] != contract.scope {
			t.Fatalf("route %s scope/method mismatch: %#v", route, op)
		}
		if contract.write && (op["x-max-body-bytes"] != 1048576 || op["requestBody"] == nil) {
			t.Errorf("write route %s lacks frozen 1MiB body contract", route)
		}
		responses := op["responses"].(map[string]any)
		for _, code := range contract.codes {
			if responses[code] == nil {
				t.Errorf("route %s is missing response %s", route, code)
			}
		}
	}
	for _, route := range []string{"/v1/learning/timeline", "/v1/learning/routes", "/v1/learning/evidence", "/v1/learning/reviews"} {
		op := paths[route].(map[string]any)["get"].(map[string]any)
		encoded, _ := json.Marshal(op["parameters"])
		if !strings.Contains(string(encoded), "#/components/parameters/Cursor") || !strings.Contains(string(encoded), "#/components/parameters/PageLimit") {
			t.Errorf("route %s lacks cursor and limit contracts", route)
		}
	}
}

func TestLearningOpenAPIWriteSchemasAreClosedAndDiscriminated(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	decode := func(raw string) any {
		t.Helper()
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	validate := func(schemaName, raw string, valid bool) {
		t.Helper()
		schema := document.Components.Schemas[schemaName].Value
		err := schema.VisitJSON(decode(raw), openapi3.EnableJSONSchema2020())
		if valid && err != nil {
			t.Fatalf("%s rejected valid payload: %v", schemaName, err)
		}
		if !valid && err == nil {
			t.Fatalf("%s accepted invalid payload: %s", schemaName, raw)
		}
	}
	base := `"operation_id":"10000000-0000-4000-8000-000000000001","payload_schema_version":1,"aggregate_type":"session","aggregate_id":"20000000-0000-4000-8000-000000000001","expected_version":0`
	validate("TutoringActionRequest", `{`+base+`,"action":"start_diagnostic"}`, true)
	validate("TutoringActionRequest", `{`+base+`,"action":"apply_route","proposal_id":"30000000-0000-4000-8000-000000000001"}`, true)
	validate("TutoringActionRequest", `{`+base+`,"action":"record_exposure","exposure_kind":"explanation","exposure_text":"explanation","knowledge_references":[]}`, true)
	validate("TutoringActionRequest", `{`+base+`,"action":"record_exposure","proposal_id":"30000000-0000-4000-8000-000000000001"}`, true)
	validate("TutoringActionRequest", `{`+base+`,"action":"record_exposure","proposal_id":"30000000-0000-4000-8000-000000000001","exposure_kind":"reading"}`, true)
	validate("TutoringActionRequest", `{`+base+`,"action":"record_exposure","exposure_text":"explanation","knowledge_references":[]}`, false)
	validate("TutoringActionRequest", `{`+base+`,"action":"record_exposure","exposure_kind":"","exposure_text":"explanation"}`, false)
	validate("TutoringActionRequest", `{`+base+`,"action":"record_exposure","exposure_kind":"video","exposure_text":"explanation"}`, false)
	validate("TutoringActionRequest", `{`+base+`,"action":"record_exposure","proposal_id":"30000000-0000-4000-8000-000000000001","exposure_kind":""}`, false)
	validate("TutoringActionRequest", `{`+base+`,"action":"record_exposure","proposal_id":"30000000-0000-4000-8000-000000000001","exposure_kind":"video"}`, false)
	validate("TutoringActionRequest", `{`+base+`,"action":"start_diagnostic","proposal_id":"30000000-0000-4000-8000-000000000001"}`, false)
	validate("TutoringActionRequest", `{`+base+`,"action":"start_diagnostic","unknown":true}`, false)
	validate("TutoringActionRequest", `{"operation_id":"10000000-0000-4000-8000-000000000001","payload_schema_version":1,"aggregate_type":"session","aggregate_id":"20000000-0000-4000-8000-000000000001","action":"start_diagnostic"}`, false)
	validate("AssessmentDecisionRequest", `{`+base+`,"kind":"confirm","expected_disposition_version":1}`, true)
	validate("AssessmentDecisionRequest", `{`+base+`,"kind":"confirm","expected_disposition_version":1,"reason":"unrelated"}`, false)
	validate("LearningGoalRequest", `{"operation_id":"10000000-0000-4000-8000-000000000001","payload_schema_version":1,"aggregate_type":"goal","aggregate_id":"20000000-0000-4000-8000-000000000001","expected_version":0,"text":"Learn","source":"device"}`, true)
	validate("LearningGoalRequest", `{"operation_id":"10000000-0000-4000-8000-000000000001","payload_schema_version":1,"aggregate_type":"goal","aggregate_id":"20000000-0000-4000-8000-000000000001","text":"Learn","source":"device"}`, false)
	validate("TutoringProposalRequest", `{"request_id":"10000000-0000-4000-8000-000000000001","proposal_type":"route","aggregate_type":"goal","aggregate_id":"20000000-0000-4000-8000-000000000001","aggregate_version":1,"knowledge_revision_id":"30000000-0000-4000-8000-000000000001","node_revision_ids":["40000000-0000-4000-8000-000000000001"],"input":{},"unknown":true}`, false)

	actionMapping := document.Components.Schemas["TutoringActionRequest"].Value.Discriminator.Mapping
	if len(actionMapping) != 16 {
		t.Fatalf("action discriminator mapping has %d entries, want 16", len(actionMapping))
	}
	decisionMapping := document.Components.Schemas["AssessmentDecisionRequest"].Value.Discriminator.Mapping
	if len(decisionMapping) != 3 {
		t.Fatalf("decision discriminator mapping has %d entries, want 3", len(decisionMapping))
	}
}

func TestLearningOpenAPICanonicalUUIDSHACollectionAndActivityMatrix(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	validate := func(schemaName string, value any, valid bool) {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var wireValue any
		if err := json.Unmarshal(encoded, &wireValue); err != nil {
			t.Fatal(err)
		}
		schema := document.Components.Schemas[schemaName].Value
		err = schema.VisitJSON(wireValue, openapi3.EnableJSONSchema2020())
		if valid && err != nil {
			t.Fatalf("%s rejected valid value: %v", schemaName, err)
		}
		if !valid && err == nil {
			t.Fatalf("%s accepted invalid value: %#v", schemaName, value)
		}
	}
	canonical := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	variants := []string{
		"aaaaaaaaaaaa4aaa8aaaaaaaaaaaaaaa",
		"{aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa}",
		"urn:uuid:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA",
	}
	validate("LearningUUID", canonical, true)
	for _, variant := range variants {
		validate("LearningUUID", variant, false)
	}

	goal := map[string]any{
		"operation_id": canonical, "payload_schema_version": 1, "aggregate_type": "goal",
		"aggregate_id": canonical, "expected_version": 0, "text": "Learn", "source": "device",
	}
	validate("LearningGoalRequest", goal, true)
	for _, variant := range variants {
		invalid := mapsClone(goal)
		invalid["operation_id"] = variant
		validate("LearningGoalRequest", invalid, false)
	}

	for _, parameterName := range []string{"SessionID", "AssessmentID", "NodeRevisionID"} {
		schema := document.Components.Parameters[parameterName].Value.Schema.Value
		if err := schema.VisitJSON(canonical, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatalf("%s rejected canonical UUID: %v", parameterName, err)
		}
		for _, variant := range variants {
			if err := schema.VisitJSON(variant, openapi3.EnableJSONSchema2020()); err == nil {
				t.Fatalf("%s accepted non-canonical UUID %q", parameterName, variant)
			}
		}
	}
	for _, route := range []string{"/v1/learning/timeline", "/v1/learning/evidence"} {
		var schema *openapi3.Schema
		for _, parameter := range document.Paths.Find(route).Get.Parameters {
			if parameter.Value.Name == "session_id" || parameter.Value.Name == "node_revision_id" {
				schema = parameter.Value.Schema.Value
				break
			}
		}
		if schema == nil {
			t.Fatalf("%s UUID query parameter is missing", route)
		}
		for _, variant := range variants {
			if err := schema.VisitJSON(variant, openapi3.EnableJSONSchema2020()); err == nil {
				t.Fatalf("%s accepted non-canonical query UUID %q", route, variant)
			}
		}
	}

	validSHA := strings.Repeat("a", 64)
	referenceInput := map[string]any{"node_revision_id": canonical}
	validate("KnowledgeReferenceInput", referenceInput, true)
	withSHA := mapsClone(referenceInput)
	withSHA["slice_sha256"] = validSHA
	validate("KnowledgeReferenceInput", withSHA, true)
	for _, invalidSHA := range []string{"", strings.ToUpper(validSHA), strings.Repeat("a", 63), strings.Repeat("g", 64)} {
		invalid := mapsClone(referenceInput)
		invalid["slice_sha256"] = invalidSHA
		validate("KnowledgeReferenceInput", invalid, false)
	}
	assessmentItem := map[string]any{
		"rubric_item_id": "item-1", "conclusion": "pass", "answer_quote": "answer",
		"answer_range": map[string]any{"start": 0, "end": 1}, "answer_quote_sha256": validSHA,
		"knowledge_reference_id": "", "knowledge_quote": "knowledge",
		"knowledge_range": map[string]any{"start": 0, "end": 1}, "knowledge_quote_sha256": validSHA,
	}
	validate("AssessmentItemInput", assessmentItem, true)
	for _, field := range []string{"answer_quote_sha256", "knowledge_quote_sha256"} {
		invalid := mapsClone(assessmentItem)
		invalid[field] = ""
		validate("AssessmentItemInput", invalid, false)
	}

	override := map[string]any{
		"operation_id": canonical, "payload_schema_version": 1, "aggregate_type": "session",
		"aggregate_id": canonical, "expected_version": 0, "kind": "override",
		"expected_disposition_version": 1, "reason": "review",
	}
	for count, valid := range map[int]bool{64: true, 65: false} {
		items := make([]any, count)
		for index := range items {
			items[index] = assessmentItem
		}
		candidate := mapsClone(override)
		candidate["items"] = items
		validate("AssessmentOverrideRequest", candidate, valid)
	}
	directExposure := map[string]any{
		"operation_id": canonical, "payload_schema_version": 1, "aggregate_type": "session",
		"aggregate_id": canonical, "expected_version": 0, "action": "record_exposure", "exposure_kind": "explanation", "exposure_text": "text",
	}
	for count, valid := range map[int]bool{100: true, 101: false} {
		references := make([]any, count)
		for index := range references {
			references[index] = referenceInput
		}
		candidate := mapsClone(directExposure)
		candidate["knowledge_references"] = references
		validate("ActionDirectExposureRequest", candidate, valid)
	}

	now := time.Date(2026, time.August, 20, 14, 0, 0, 0, time.UTC)
	reference := learning.KnowledgeReference{
		KnowledgeRevisionID: canonical, NodeID: canonical, NodeRevisionID: canonical,
		Range: learning.SourceRange{Start: 0, End: 4}, Slice: "text", SliceSHA256: validSHA,
	}
	activity := learning.Activity{
		ID: canonical, Revision: 1, SessionID: canonical, GoalRevisionID: canonical,
		RouteRevisionID: canonical, RouteStepID: canonical, KnowledgeRevisionID: canonical,
		TargetNodeID: canonical, TargetNodeRevisionID: canonical, References: []learning.KnowledgeReference{reference},
		Prompt: "Question", Type: learning.ActivityOpen,
		Rubric:     learning.Rubric{Revision: "rubric-v1", Items: []learning.RubricItem{{ID: "item-1", Criterion: "Correct"}}},
		Difficulty: 1, AllowedHelp: []learning.HelpLevel{learning.HelpNone},
		ActivityPolicyVersion: learning.ActivityPolicyVersion, AssessmentPolicyVersion: learning.AssessmentPolicyVersion,
		ReviewPolicyVersion: learning.ReviewPolicyVersion, Review: false, CreatedAt: now,
	}
	encoded, err := json.Marshal(activity)
	if err != nil {
		t.Fatal(err)
	}
	var activityPayload map[string]any
	if err := json.Unmarshal(encoded, &activityPayload); err != nil {
		t.Fatal(err)
	}
	validate("Activity", activityPayload, true)
	for _, required := range []string{"target_node_id", "target_node_revision_id"} {
		invalid := mapsClone(activityPayload)
		delete(invalid, required)
		validate("Activity", invalid, false)
	}
	withUnknown := mapsClone(activityPayload)
	withUnknown["unknown"] = true
	validate("Activity", withUnknown, false)

	routeStep := map[string]any{
		"route_step_id": canonical, "ordinal": 0, "node_id": canonical,
		"node_revision_id": canonical, "teaching_intent": "Explain", "completion_condition": "Recall",
	}
	validate("RouteStep", routeStep, true)
	for _, required := range []string{"node_id", "node_revision_id"} {
		invalid := mapsClone(routeStep)
		delete(invalid, required)
		validate("RouteStep", invalid, false)
	}
}

func mapsClone(value map[string]any) map[string]any {
	clone := make(map[string]any, len(value))
	for key, item := range value {
		clone[key] = item
	}
	return clone
}

func TestLearningOpenAPIValidatesWireShapes(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	decodeDTO := func(value any) any {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	validateResponse := func(path, method, code string, value any) {
		t.Helper()
		operation := document.Paths.Find(path)
		var response *openapi3.ResponseRef
		if method == "get" {
			response = operation.Get.Responses.Value(code)
		} else {
			response = operation.Post.Responses.Value(code)
		}
		if response == nil || response.Value == nil || response.Value.Content.Get("application/json") == nil {
			t.Fatalf("%s %s response %s is not declared", method, path, code)
		}
		schema := response.Value.Content.Get("application/json").Schema.Value
		if err := schema.VisitJSON(decodeDTO(value), openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatalf("%s %s response %s failed schema: %v", method, path, code, err)
		}
	}

	const (
		id1 = "10000000-0000-4000-8000-000000000001"
		id2 = "20000000-0000-4000-8000-000000000001"
		id3 = "30000000-0000-4000-8000-000000000001"
		id4 = "40000000-0000-4000-8000-000000000001"
		id5 = "50000000-0000-4000-8000-000000000001"
		id6 = "60000000-0000-4000-8000-000000000001"
		id7 = "70000000-0000-4000-8000-000000000001"
		id8 = "80000000-0000-4000-8000-000000000001"
	)
	now := time.Date(2026, time.August, 20, 14, 0, 0, 0, time.UTC)
	metadata := learning.ProjectionMetadata{
		AsOfEventSequence: 17, ProjectionVersion: learning.ProjectionVersion,
		MasteryReducerVersion: learning.MasteryReducerVersion, AssessmentPolicy: learning.AssessmentPolicyVersion,
		ReviewPolicy: learning.ReviewPolicyVersion, KnowledgeRevisionID: id4, GenerationID: id8,
		Rebuilding: true, Degraded: true, Incomplete: true, ReasonCodes: []string{"checkpoint_lag"},
	}
	session := tutoring.Session{ID: id2, State: tutoring.StateRouteActive, AggregateVer: 4, Context: tutoring.FocusContext{GoalRevisionID: id3, RouteRevisionID: id5, RouteStepID: id6, KnowledgeRevisionID: id4, FocusNodeRevisionID: id7}, AttachedQuiz: false, CompletedRoute: false}
	goal := learning.GoalRevision{ID: id3, GoalID: id2, Revision: 1, Text: "Learn fractions", Source: "device", ActorDeviceID: id1, CreatedAt: now}
	decision := learning.AssessmentDecision{ID: id7, AssessmentID: id6, Version: 1, Disposition: learning.DispositionAccepted, Items: []learning.AssessmentItem{}, ActorDeviceID: id1, CreatedAt: now}
	operationResult := func(aggregateType, aggregateID string, version int64, result any) learning.OperationResult {
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return learning.OperationResult{Status: "succeeded", AggregateType: aggregateType, AggregateID: aggregateID, AggregateVersion: version, FirstEventSequence: 10, LastEventSequence: 11, ProjectionAsOf: 11, Result: encoded}
	}
	validateResponse("/v1/learning/goals", "post", "201", operationResult("goal", id2, 1, goal))
	validateResponse("/v1/tutoring/sessions", "post", "201", operationResult("session", id2, 2, session))
	validateResponse("/v1/tutoring/sessions/{sessionID}/actions", "post", "201", operationResult("session", id2, 4, session))
	validateResponse("/v1/learning/assessments/{assessmentID}/decisions", "post", "201", operationResult("session", id2, 5, decision))

	frozen := learning.ProposalRequest{RequestID: id1, Type: learning.ProposalRoute, AggregateType: "goal", AggregateID: id2, AggregateVersion: 1, GoalRevisionID: id3, KnowledgeRevisionID: id4, NodeRevisionIDs: []string{id7}, Input: json.RawMessage(`{"topic":"fractions"}`)}
	proposal := learning.ProposalArtifact{ID: id5, SchemaVersion: learning.ProposalSchemaVersion, InputHash: strings.Repeat("a", 64), Type: learning.ProposalRoute, AggregateType: "goal", AggregateID: id2, AggregateVersion: 1, GoalRevisionID: id3, KnowledgeRevisionID: id4, FrozenRequest: frozen, Route: []learning.RouteProposalStep{{NodeRevisionID: id7, TeachingIntent: "Explain", CompletionCondition: "Recall"}}, ModelID: "model", ModelParameters: map[string]any{"temperature": 0.0}, PromptRevision: "prompt-v1", AttemptCategories: []string{"success"}, CreatedAt: now}
	validateResponse("/v1/tutoring/proposals", "post", "201", proposal)

	reference := learning.KnowledgeReference{KnowledgeRevisionID: id4, NodeID: id6, NodeRevisionID: id7, DocumentRevisionID: id5, Range: learning.SourceRange{Start: 0, End: 9}, Slice: "fractions", SliceSHA256: strings.Repeat("b", 64)}
	_ = reference
	evidence := learning.AcceptedEvidence{ID: id8, DispositionDecisionID: id7, AssessmentID: id6, AttemptID: id5, ActivityID: id4, ActivityRevision: 1, GoalRevisionID: id3, RouteRevisionID: id2, KnowledgeRevisionID: id1, NodeRevisionID: id7, RubricRevision: "rubric-v1", Kind: learning.EvidencePracticeRecall, ActivityType: learning.ActivityObjective, Outcome: learning.OutcomePass, Help: learning.HelpNone, ReceivedAt: now, AcceptancePolicyVersion: learning.AssessmentPolicyVersion, ReducerPolicyVersion: learning.MasteryReducerVersion, ReviewPolicyVersion: learning.ReviewPolicyVersion, Misconceptions: []learning.MisconceptionCandidate{}, RubricOutcomes: []learning.RubricOutcome{{RubricItemID: "item-1", Conclusion: learning.ConclusionPass}}}
	review := learning.ReviewSchedule{NodeRevisionID: id7, Step: 1, DueAt: now.Add(24 * time.Hour), Intervals: []time.Duration{24 * time.Hour, 72 * time.Hour}, PolicyVersion: learning.ReviewPolicyVersion}
	mastery := learning.MasteryProjection{NodeRevisionID: id7, State: learning.MasteryLearning, BaselineState: learning.MasteryLearning, ValidEvidenceCount: 1, Kinds: map[learning.EvidenceKind]int{learning.EvidencePracticeRecall: 1}, Outcomes: map[learning.Outcome]int{learning.OutcomePass: 1}, Help: map[learning.HelpLevel]int{learning.HelpNone: 1}, LastEvidenceAt: &now, PendingAssessments: 0, UncertaintyReasons: []string{}, ReducerVersion: learning.MasteryReducerVersion}
	misconception := learning.MisconceptionHypothesis{ID: id6, Revision: 1, NodeRevisionID: id7, RubricItemID: "item-1", CandidateHash: strings.Repeat("c", 64), Candidate: "denominator confusion", Status: learning.MisconceptionProposed, SourceEvidenceIDs: []string{id8}, CounterEvidenceIDs: []string{}, CausedByEvidenceID: id8}

	sessionView := learning.SessionView{Metadata: metadata, Session: session, Estimate: learning.ActiveTimeEstimate{DurationSeconds: 30, Estimated: true, AlgorithmVersion: learning.ActiveTimePolicyVersion, SampleCount: 2, FirstReceivedAt: &now, LastReceivedAt: &now}}
	timeline := learning.TimelinePage{Metadata: metadata, Items: []learning.TimelineItem{{EventSequence: 17, EventID: id8, Type: learning.EventTutoringStateChanged, AggregateID: id2, ReceivedAt: now, OccurredAt: &now, OccurredAtTrusted: false}}, NextCursor: "opaque"}
	routes := learning.RoutesPage{Metadata: metadata, Items: []learning.RouteProjection{{Route: learning.RouteRevision{ID: id5, RouteID: id4, Revision: 1, GoalRevisionID: id3, KnowledgeRevisionID: id2, PolicyVersion: learning.RoutePolicyVersion, SourceProposalID: id1, Steps: []learning.RouteStep{{ID: id6, Ordinal: 0, NodeID: id7, NodeRevisionID: id8, TeachingIntent: "Explain", CompletionCondition: "Recall"}}, CreatedAt: now}, EventSequence: 12, Current: true}}, NextCursor: "opaque"}
	node := learning.NodeView{Metadata: metadata, Node: learning.NodeReduction{Mastery: mastery, Review: &review, Misconceptions: []learning.MisconceptionHypothesis{misconception}}, Evidence: []learning.AcceptedEvidence{evidence}}
	evidencePage := learning.EvidencePage{Metadata: metadata, Items: []learning.AcceptedEvidence{evidence}, NextCursor: "opaque"}
	reviews := learning.ReviewsPage{Metadata: metadata, Items: []learning.ReviewSchedule{review}, NextCursor: "opaque"}
	status := learning.ProjectionStatus{Metadata: metadata, HighWater: 18, Fingerprint: strings.Repeat("d", 64), ActiveGenerationID: id8}

	validateResponse("/v1/tutoring/sessions/current", "get", "200", sessionView)
	validateResponse("/v1/tutoring/sessions/{sessionID}", "get", "200", sessionView)
	validateResponse("/v1/learning/timeline", "get", "200", timeline)
	validateResponse("/v1/learning/routes", "get", "200", routes)
	validateResponse("/v1/learning/nodes/{nodeRevisionID}", "get", "200", node)
	validateResponse("/v1/learning/evidence", "get", "200", evidencePage)
	validateResponse("/v1/learning/reviews", "get", "200", reviews)
	validateResponse("/v1/learning/projections/status", "get", "200", status)
}
