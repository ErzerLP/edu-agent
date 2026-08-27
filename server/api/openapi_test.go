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
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
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
		"/v1/knowledge/revisions/head":                {"knowledge:read", []string{"200", "401", "403", "404", "429", "500", "503"}},
		"/v1/knowledge/imports":                       {"knowledge:write", []string{"200", "201", "400", "401", "403", "409", "413", "422", "429", "500", "503"}},
		"/v1/knowledge/revisions/{revisionID}/tree":   {"knowledge:read", []string{"200", "400", "401", "403", "404", "429", "500", "503"}},
		"/v1/knowledge/revisions/{revisionID}/export": {"knowledge:read", []string{"200", "400", "401", "403", "404", "429", "500", "503"}},
		"/v1/knowledge/retrievals":                    {"knowledge:read", []string{"200", "400", "401", "403", "404", "413", "429", "500", "503"}},
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
		if route == "/v1/knowledge/imports" {
			encoded, _ := json.Marshal(operation["x-required-scopes"])
			if string(encoded) != `["knowledge:write","knowledge:approve"]` {
				t.Fatalf("route %s combined scopes = %s", route, encoded)
			}
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
		if route == "/v1/learning/assessments/{assessmentID}/decisions" {
			encoded, _ := json.Marshal(op["x-required-scopes"])
			if string(encoded) != `["learning:write","learning:approve"]` {
				t.Fatalf("route %s combined scopes = %s", route, encoded)
			}
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
		if route == "/v1/learning/routes" && !strings.Contains(string(encoded), "current_only") {
			t.Error("routes lacks current_only contract")
		}
	}
}

func TestOfflineOpenAPIContractsAreClosedScopedAndMatchWireResults(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	paths := raw["paths"].(map[string]any)
	for route, expected := range map[string]struct {
		method string
		scope  string
		limit  int
		codes  []string
	}{
		"/v1/learning/offline/packs":                                {"post", "learning:write", 1048576, []string{"200", "201", "400", "401", "403", "409", "413", "429", "500", "503"}},
		"/v1/learning/offline/sync":                                 {"post", "learning:write", 8388608, []string{"200", "400", "401", "403", "413", "429", "500", "503"}},
		"/v1/learning/offline/operations/{operationID}":             {"get", "learning:read", 0, []string{"200", "400", "401", "403", "404", "429", "500", "503"}},
		"/v1/learning/offline/assessments":                          {"get", "learning:read", 0, []string{"200", "400", "401", "403", "409", "429", "500", "503"}},
		"/v1/learning/offline/assessments/{assessmentID}":           {"get", "learning:read", 0, []string{"200", "400", "401", "403", "404", "429", "500", "503"}},
		"/v1/learning/offline/assessments/{assessmentID}/decisions": {"post", "learning:write", 1048576, []string{"200", "201", "400", "401", "403", "404", "409", "413", "429", "500", "503"}},
	} {
		operation := paths[route].(map[string]any)[expected.method].(map[string]any)
		if operation["x-required-scope"] != expected.scope {
			t.Fatalf("offline route %s scope=%v", route, operation["x-required-scope"])
		}
		if route == "/v1/learning/offline/assessments/{assessmentID}/decisions" {
			encoded, _ := json.Marshal(operation["x-required-scopes"])
			if string(encoded) != `["learning:write","learning:approve"]` {
				t.Fatalf("offline route %s combined scopes = %s", route, encoded)
			}
		}
		if expected.limit > 0 && operation["x-max-body-bytes"] != expected.limit {
			t.Fatalf("offline route %s body limit=%v", route, operation["x-max-body-bytes"])
		}
		responses := operation["responses"].(map[string]any)
		for _, code := range expected.codes {
			if responses[code] == nil {
				t.Errorf("offline route %s is missing response %s", route, code)
			}
		}
	}
	schemas := raw["components"].(map[string]any)["schemas"].(map[string]any)
	for _, name := range []string{
		"OfflinePrepareRequest", "OfflineSignerManifestPayload", "OfflineSignerManifestEnvelope",
		"OfflineAuthorizationPayload", "OfflineAuthorizationEnvelope", "OfflinePackItem",
		"OfflinePackPayload", "OfflinePackEnvelope", "OfflinePrepareResponseSignaturePayload",
		"OfflinePrepareResponseSignatureEnvelope", "OfflinePrepareResponse", "OfflineObservation",
		"OfflineAttemptPayload", "OfflineSkipPayload", "OfflineOperation", "OfflineSyncRequest",
		"OfflineIngestReceipt", "OfflineStatusTicket", "OfflineSyncItemResult", "OfflineSyncResponse",
		"OfflineOperationStatus", "OfflineAssessmentSummary", "OfflineAssessmentPage", "OfflineAssessmentView",
		"OfflineAssessmentConfirmRequest", "OfflineAssessmentOverrideItem", "OfflineAssessmentOverrideRequest",
		"OfflineAssessmentVoidRequest", "OfflineAssessmentDecisionReceipt",
	} {
		schema := schemas[name].(map[string]any)
		if schema["type"] == "object" && schema["additionalProperties"] != false {
			t.Fatalf("offline schema %s is not closed", name)
		}
	}

	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	decode := func(value any) any {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var result any
		if err := json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	operationID := "10000000-0000-4000-8000-000000000001"
	status := learning.OfflineOperationStatus{
		OperationID:      operationID,
		SubmissionID:     "20000000-0000-4000-8000-000000000001",
		ArchiveStatus:    learning.OfflineArchivedSucceeded,
		AssessmentStatus: learning.OfflineAssessmentCompleted,
		EvidenceStatus:   learning.OfflineEvidenceAccepted,
		ReasonCodes:      []string{},
		Receipt:          learning.OfflineIngestReceipt{ReceiptID: "30000000-0000-4000-8000-000000000001", ArchivedAt: now, AggregateVersion: "4", FirstEventSequence: "10", LastEventSequence: "13", ProjectionAsOf: "13", ArchiveStatus: learning.OfflineArchivedSucceeded},
		StatusTicket:     learning.OfflineStatusTicket{TicketID: "40000000-0000-4000-8000-000000000001", OperationID: operationID, Revision: "1", UpdatedAt: now},
	}
	statusSchema := document.Paths.Find("/v1/learning/offline/operations/{operationID}").Get.Responses.Value("200").Value.Content.Get("application/json").Schema.Value
	if err := statusSchema.VisitJSON(decode(status), openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("offline status response failed schema validation: %v", err)
	}
	sync := learning.OfflineSyncResponse{SyncRequestID: "50000000-0000-4000-8000-000000000001", Results: []learning.OfflineIngestResult{{
		ResultKind:     learning.OfflineResultConflict,
		OperationID:    operationID,
		DeviceSequence: "9007199254740992",
		SubmissionID:   status.SubmissionID,
		ArchiveStatus:  learning.OfflineIdempotencyConflict,
		ReasonCodes:    []string{learning.OfflineReasonIdempotencyConflict},
	}}}
	syncSchema := document.Paths.Find("/v1/learning/offline/sync").Post.Responses.Value("200").Value.Content.Get("application/json").Schema.Value
	if err := syncSchema.VisitJSON(decode(sync), openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("offline sync response failed schema validation: %v", err)
	}
	invalidPrepare := map[string]any{
		"operation_id": operationID, "payload_schema_version": 1, "expected_session_version": "7",
		"trusted_manifest_revision": "0", "trusted_manifest_digest": learning.OfflineZeroDigest,
		"unknown": true,
	}
	if err := document.Components.Schemas["OfflinePrepareRequest"].Value.VisitJSON(invalidPrepare, openapi3.EnableJSONSchema2020()); err == nil {
		t.Fatal("offline prepare schema accepted an unknown field")
	}
	decisionRequest := map[string]any{
		"operation_id": operationID, "payload_schema_version": 1,
		"attempt_id": status.SubmissionID, "expected_version": "2", "kind": "void",
		"expected_disposition_version": "1", "reason": "invalid assessment",
	}
	if err := document.Components.Schemas["OfflineAssessmentDecisionRequest"].Value.VisitJSON(decode(decisionRequest), openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("offline assessment decision request failed schema validation: %v", err)
	}
	decisionRequest["unknown"] = true
	if err := document.Components.Schemas["OfflineAssessmentDecisionRequest"].Value.VisitJSON(decode(decisionRequest), openapi3.EnableJSONSchema2020()); err == nil {
		t.Fatal("offline assessment decision schema accepted an unknown field")
	}
	decisionSchema := document.Components.Schemas["OfflineAssessmentDecisionRequest"].Value
	overrideRequest := func(reason, rubricItemID, misconception string) map[string]any {
		return map[string]any{
			"operation_id": operationID, "payload_schema_version": 1,
			"attempt_id": status.SubmissionID, "expected_version": "2", "kind": "override",
			"expected_disposition_version": "1", "reason": reason,
			"items": []any{map[string]any{
				"rubric_item_id": rubricItemID, "conclusion": "partial", "misconception_candidate": misconception,
			}},
		}
	}
	boundaryOverride := overrideRequest(
		strings.Repeat("由", learning.MaxOfflineAssessmentDecisionReasonRunes),
		strings.Repeat("项", learning.MaxOfflineAssessmentRubricItemIDRunes),
		strings.Repeat("误", learning.MaxOfflineAssessmentMisconceptionRunes),
	)
	if err := decisionSchema.VisitJSON(decode(boundaryOverride), openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("offline assessment Unicode boundary failed schema validation: %v", err)
	}
	for name, request := range map[string]map[string]any{
		"reason":                  overrideRequest(strings.Repeat("由", learning.MaxOfflineAssessmentDecisionReasonRunes+1), "item-1", ""),
		"rubric_item_id":          overrideRequest("valid", strings.Repeat("项", learning.MaxOfflineAssessmentRubricItemIDRunes+1), ""),
		"misconception_candidate": overrideRequest("valid", "item-1", strings.Repeat("误", learning.MaxOfflineAssessmentMisconceptionRunes+1)),
	} {
		if err := decisionSchema.VisitJSON(decode(request), openapi3.EnableJSONSchema2020()); err == nil {
			t.Fatalf("offline assessment schema accepted overlong %s", name)
		}
	}
	metadata := learning.ProjectionMetadata{
		AsOfEventSequence: 20, ProjectionVersion: learning.ProjectionVersion,
		MasteryReducerVersion: learning.MasteryReducerVersion, AssessmentPolicy: learning.AssessmentPolicyVersion,
		ReviewPolicy: learning.ReviewPolicyVersion, KnowledgeRevisionID: "60000000-0000-4000-8000-000000000001",
		GenerationID: "70000000-0000-4000-8000-000000000001", ReasonCodes: []string{},
	}
	page := learning.OfflineAssessmentPage{Metadata: metadata, Items: []learning.OfflineAssessmentSummary{{
		AssessmentID: "80000000-0000-4000-8000-000000000001", AttemptID: status.SubmissionID,
		ActivityID: "90000000-0000-4000-8000-000000000001", ActivityRevision: "1",
		SubmissionID: status.SubmissionID, AggregateVersion: "2", DispositionVersion: "1",
		Disposition: learning.DispositionProvisional, Confidence: 849, Confirmable: true,
		AllowedDecisions: []string{"confirm", "override", "void"}, AttemptReceivedAt: now, AssessmentCreatedAt: now,
	}}}
	pageSchema := document.Paths.Find("/v1/learning/offline/assessments").Get.Responses.Value("200").Value.Content.Get("application/json").Schema.Value
	if err := pageSchema.VisitJSON(decode(page), openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("offline assessment page failed schema validation: %v", err)
	}
	conflictSchema := document.Paths.Find("/v1/learning/offline/assessments/{assessmentID}/decisions").Post.Responses.Value("409").Value.Content.Get("application/json").Schema.Value
	versionConflict := map[string]any{
		"error": map[string]any{"code": "version_conflict", "message": "conflict", "request_id": "request-1"},
		"conflict": map[string]any{
			"aggregate_type": "offline_attempt", "aggregate_id": status.SubmissionID,
			"expected_version": 2, "current_version": 3, "as_of_event_seq": 20,
		},
	}
	if err := conflictSchema.VisitJSON(decode(versionConflict), openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("offline assessment version conflict failed schema validation: %v", err)
	}
	dispositionConflict := map[string]any{
		"error":               map[string]any{"code": "assessment_disposition_conflict", "message": "conflict", "request_id": "request-2"},
		"current_disposition": "provisional",
	}
	if err := conflictSchema.VisitJSON(decode(dispositionConflict), openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("offline assessment disposition conflict failed schema validation: %v", err)
	}
}

func TestGoCLIM1OpenAPIReadContractsAreClosedAndScoped(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	paths := raw["paths"].(map[string]any)
	for path, expectedScope := range map[string]string{
		"/v1/devices":            "devices:read",
		"/v1/devices/{deviceID}": "devices:manage",
		"/v1/model/capabilities": "model:probe",
	} {
		item := paths[path].(map[string]any)
		method := "get"
		if path == "/v1/devices/{deviceID}" {
			method = "delete"
		}
		operation := item[method].(map[string]any)
		if operation["x-required-scope"] != expectedScope {
			t.Fatalf("%s scope=%v want=%s", path, operation["x-required-scope"], expectedScope)
		}
	}
	for _, path := range []string{"/v1/tutoring/sessions/current", "/v1/tutoring/sessions/{sessionID}"} {
		responses := paths[path].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)
		if responses["503"] == nil {
			t.Fatalf("%s lacks read-gate 503", path)
		}
	}
	routeParameters, _ := json.Marshal(paths["/v1/learning/routes"].(map[string]any)["get"].(map[string]any)["parameters"])
	if !strings.Contains(string(routeParameters), `"name":"current_only"`) || !strings.Contains(string(routeParameters), `"type":"boolean"`) {
		t.Fatalf("routes current_only is not a strict boolean query: %s", routeParameters)
	}

	schemas := raw["components"].(map[string]any)["schemas"].(map[string]any)
	for _, name := range []string{"SessionView", "SessionWorkItem", "Attempt", "FreeQuestion", "FreeAnswer", "FrozenReference", "GoalRevision", "RouteRevision", "Activity", "AssessmentArtifact", "AssessmentDecision"} {
		schema := schemas[name].(map[string]any)
		if schema["type"] == "object" && schema["additionalProperties"] != false {
			t.Fatalf("schema %s is not closed", name)
		}
	}
	sessionView := schemas["SessionView"].(map[string]any)
	required, _ := json.Marshal(sessionView["required"])
	if !strings.Contains(string(required), "work_item") {
		t.Fatalf("SessionView.work_item is not required: %s", required)
	}
	workItem := sessionView["properties"].(map[string]any)["work_item"].(map[string]any)
	oneOf, _ := json.Marshal(workItem["oneOf"])
	if !strings.Contains(string(oneOf), "SessionWorkItem") || !strings.Contains(string(oneOf), `"null"`) {
		t.Fatalf("SessionView.work_item is not object-or-null: %s", oneOf)
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
	attachedAssessment := `{"request_id":"10000000-0000-4000-8000-000000000001","proposal_type":"assessment","aggregate_type":"session","aggregate_id":"20000000-0000-4000-8000-000000000001","aggregate_version":8,"goal_revision_id":"30000000-0000-4000-8000-000000000001","route_revision_id":"30000000-0000-4000-8000-000000000002","route_step_id":"30000000-0000-4000-8000-000000000003","focus_node_revision_id":"30000000-0000-4000-8000-000000000004","activity_id":"30000000-0000-4000-8000-000000000005","attempt_id":"30000000-0000-4000-8000-000000000006","focus_frame_id":"30000000-0000-4000-8000-000000000007","tutoring_state":"Evaluating","knowledge_revision_id":"30000000-0000-4000-8000-000000000008","node_revision_ids":["40000000-0000-4000-8000-000000000001"],"input":{}}`
	validate("TutoringProposalRequest", attachedAssessment, false)
	validate("TutoringProposalRequest", strings.TrimSuffix(attachedAssessment, "}")+`,"free_question_id":"30000000-0000-4000-8000-000000000009","free_answer_id":"30000000-0000-4000-8000-000000000010"}`, true)

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

func TestLearningOpenAPIConflictSchemaFreezesEndpointAndCodeCoupling(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	const aggregateID = "10000000-0000-4000-8000-000000000001"
	errorOnly := func(code string) map[string]any {
		return map[string]any{"error": map[string]any{"code": code, "message": "Learning request conflicts with current state", "request_id": "request-1"}}
	}
	versionConflict := func(aggregateType string) map[string]any {
		value := errorOnly("version_conflict")
		value["conflict"] = map[string]any{
			"aggregate_type": aggregateType, "aggregate_id": aggregateID,
			"expected_version": 0, "current_version": 0, "as_of_event_seq": 0,
		}
		return value
	}
	dispositionConflict := func() map[string]any {
		value := errorOnly("assessment_disposition_conflict")
		value["current_disposition"] = "provisional"
		return value
	}
	schemaFor := func(path, method string) *openapi3.Schema {
		t.Helper()
		item := document.Paths.Find(path)
		var response *openapi3.ResponseRef
		if method == "get" {
			response = item.Get.Responses.Value("409")
		} else {
			response = item.Post.Responses.Value("409")
		}
		if response == nil || response.Value == nil {
			t.Fatalf("%s %s learning 409 response is missing", method, path)
		}
		media := response.Value.Content.Get("application/json")
		if media == nil || media.Schema == nil || media.Schema.Value == nil {
			t.Fatalf("%s %s learning 409 schema is missing", method, path)
		}
		return media.Schema.Value
	}
	validate := func(name, path, method string, value map[string]any, wantValid bool) {
		t.Helper()
		err := schemaFor(path, method).VisitJSON(value, openapi3.EnableJSONSchema2020())
		if wantValid && err != nil {
			t.Fatalf("%s rejected handler-compatible payload: %v", name, err)
		}
		if !wantValid && err == nil {
			t.Fatalf("%s accepted invalid payload: %#v", name, value)
		}
	}

	const (
		goalPath       = "/v1/learning/goals"
		sessionPath    = "/v1/tutoring/sessions"
		proposalPath   = "/v1/tutoring/proposals"
		actionPath     = "/v1/tutoring/sessions/{sessionID}/actions"
		assessmentPath = "/v1/learning/assessments/{assessmentID}/decisions"
		cursorPath     = "/v1/learning/timeline"
	)
	validate("goal version", goalPath, "post", versionConflict("goal"), true)
	validate("goal rejects session tuple", goalPath, "post", versionConflict("session"), false)
	validate("goal idempotency", goalPath, "post", errorOnly("idempotency_conflict"), true)
	validate("session version", sessionPath, "post", versionConflict("session"), true)
	validate("session rejects goal tuple", sessionPath, "post", versionConflict("goal"), false)
	validate("proposal stale", proposalPath, "post", errorOnly("stale_proposal"), true)
	validate("proposal rejects version", proposalPath, "post", versionConflict("session"), false)
	validate("action version", actionPath, "post", versionConflict("session"), true)
	validate("action state", actionPath, "post", errorOnly("invalid_transition"), true)
	validate("action disposition", actionPath, "post", dispositionConflict(), true)
	validate("action disposition without current value", actionPath, "post", errorOnly("assessment_disposition_conflict"), true)
	validate("assessment version", assessmentPath, "post", versionConflict("session"), true)
	validate("assessment state", assessmentPath, "post", errorOnly("activity_state_conflict"), true)
	validate("assessment disposition", assessmentPath, "post", dispositionConflict(), true)
	validate("cursor stale", cursorPath, "get", errorOnly("stale_cursor"), true)

	missingTuple := errorOnly("version_conflict")
	validate("version requires tuple", actionPath, "post", missingTuple, false)
	missingDisposition := errorOnly("assessment_disposition_conflict")
	validate("assessment conflict requires disposition", assessmentPath, "post", missingDisposition, false)
	invalidActionDisposition := errorOnly("assessment_disposition_conflict")
	invalidActionDisposition["current_disposition"] = "unknown"
	validate("action conflict rejects invalid disposition", actionPath, "post", invalidActionDisposition, false)
	withDisposition := versionConflict("session")
	withDisposition["current_disposition"] = "provisional"
	validate("version rejects disposition", assessmentPath, "post", withDisposition, false)
	withTuple := errorOnly("activity_state_conflict")
	withTuple["conflict"] = versionConflict("session")["conflict"]
	validate("state conflict rejects tuple", actionPath, "post", withTuple, false)
	withUnknown := errorOnly("unknown_conflict")
	validate("unknown code", actionPath, "post", withUnknown, false)
	validate("cursor rejects mutation code", cursorPath, "get", errorOnly("idempotency_conflict"), false)
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
	goalResult := operationResult("goal", id2, 1, goal)
	sessionResult := operationResult("session", id2, 2, session)
	actionResult := operationResult("session", id2, 4, session)
	decisionResult := operationResult("session", id2, 5, decision)
	validateResponse("/v1/learning/goals", "post", "201", goalResult)
	validateResponse("/v1/tutoring/sessions", "post", "201", sessionResult)
	validateResponse("/v1/tutoring/sessions/{sessionID}/actions", "post", "201", actionResult)
	validateResponse("/v1/learning/assessments/{assessmentID}/decisions", "post", "201", decisionResult)
	for name, candidate := range map[string]struct {
		path  string
		value learning.OperationResult
	}{
		"goal endpoint with session aggregate":    {"/v1/learning/goals", sessionResult},
		"session endpoint with goal aggregate":    {"/v1/tutoring/sessions", goalResult},
		"assessment endpoint with session result": {"/v1/learning/assessments/{assessmentID}/decisions", actionResult},
	} {
		schema := document.Paths.Find(candidate.path).Post.Responses.Value("201").Value.Content.Get("application/json").Schema.Value
		if err := schema.VisitJSON(decodeDTO(candidate.value), openapi3.EnableJSONSchema2020()); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}

	frozen := learning.ProposalRequest{RequestID: id1, Type: learning.ProposalRoute, AggregateType: "goal", AggregateID: id2, AggregateVersion: 1, GoalRevisionID: id3, KnowledgeRevisionID: id4, NodeRevisionIDs: []string{id7}, Input: json.RawMessage(`{"topic":"fractions"}`)}
	proposal := learning.ProposalArtifact{ID: id5, SchemaVersion: learning.ProposalSchemaVersion, InputHash: strings.Repeat("a", 64), Type: learning.ProposalRoute, AggregateType: "goal", AggregateID: id2, AggregateVersion: 1, GoalRevisionID: id3, KnowledgeRevisionID: id4, FrozenRequest: frozen, Route: []learning.RouteProposalStep{{NodeRevisionID: id7, TeachingIntent: "Explain", CompletionCondition: "Recall"}}, ModelID: "model", ModelParameters: map[string]any{"temperature": 0.0}, PromptRevision: "prompt-v1", AttemptCategories: []string{"success"}, CreatedAt: now}
	validateResponse("/v1/tutoring/proposals", "post", "201", proposal)

	reference := learning.KnowledgeReference{KnowledgeRevisionID: id4, NodeID: id6, NodeRevisionID: id7, DocumentRevisionID: id5, Range: learning.SourceRange{Start: 0, End: 9}, Slice: "fractions", SliceSHA256: strings.Repeat("b", 64)}
	_ = reference
	evidence := learning.AcceptedEvidence{ID: id8, DispositionDecisionID: id7, AssessmentID: id6, AttemptID: id5, ActivityID: id4, ActivityRevision: 1, GoalRevisionID: id3, RouteRevisionID: id2, KnowledgeRevisionID: id1, NodeRevisionID: id7, RubricRevision: "rubric-v1", Kind: learning.EvidencePracticeRecall, ActivityType: learning.ActivityObjective, Outcome: learning.OutcomePass, Help: learning.HelpNone, ReceivedAt: now, AcceptancePolicyVersion: learning.AssessmentPolicyVersion, ReducerPolicyVersion: learning.MasteryReducerVersion, ReviewPolicyVersion: learning.ReviewPolicyVersion, Misconceptions: []learning.MisconceptionCandidate{}, RubricOutcomes: []learning.RubricOutcome{{RubricItemID: "item-1", Conclusion: learning.ConclusionPass}}}
	review := learning.ReviewSchedule{NodeRevisionID: id7, Step: 1, DueAt: now.Add(24 * time.Hour), Intervals: []time.Duration{24 * time.Hour, 72 * time.Hour}, PolicyVersion: learning.ReviewPolicyVersion}
	mastery := learning.MasteryProjection{NodeRevisionID: id7, State: learning.MasteryLearning, BaselineState: learning.MasteryLearning, ValidEvidenceCount: 1, Kinds: map[learning.EvidenceKind]int{learning.EvidencePracticeRecall: 1}, Outcomes: map[learning.Outcome]int{learning.OutcomePass: 1}, Help: map[learning.HelpLevel]int{learning.HelpNone: 1}, LastEvidenceAt: &now, PendingAssessments: 0, UncertaintyReasons: []string{}, ReducerVersion: learning.MasteryReducerVersion}
	misconception := learning.MisconceptionHypothesis{ID: id6, Revision: 1, NodeRevisionID: id7, RubricItemID: "item-1", CandidateHash: strings.Repeat("c", 64), Candidate: "denominator confusion", Status: learning.MisconceptionProposed, SourceEvidenceIDs: []string{id8}, CounterEvidenceIDs: []string{}, CausedByEvidenceID: id8}

	routeRevision := learning.RouteRevision{ID: id5, RouteID: id4, Revision: 1, GoalRevisionID: id3, KnowledgeRevisionID: id2, PolicyVersion: learning.RoutePolicyVersion, SourceProposalID: id1, Steps: []learning.RouteStep{{ID: id6, Ordinal: 0, NodeID: id7, NodeRevisionID: id8, TeachingIntent: "Explain", CompletionCondition: "Recall"}}, CreatedAt: now}
	sessionView := learning.SessionView{Metadata: metadata, Session: session, Estimate: learning.ActiveTimeEstimate{DurationSeconds: 30, Estimated: true, AlgorithmVersion: learning.ActiveTimePolicyVersion, SampleCount: 2, FirstReceivedAt: &now, LastReceivedAt: &now}, WorkItem: &learning.SessionWorkItem{AllowedActions: []tutoring.Action{tutoring.ActionIssueActivity}, AllowedAssessmentDecisions: []string{}, GoalRevision: &goal, RouteRevision: &routeRevision}}
	timeline := learning.TimelinePage{Metadata: metadata, Items: []learning.TimelineItem{{EventSequence: 17, EventID: id8, Type: learning.EventTutoringStateChanged, AggregateID: id2, Source: "online", ActorDeviceID: id1, ReceivedAt: now, OccurredAt: &now, OccurredAtTrusted: false}}, NextCursor: "opaque"}
	routes := learning.RoutesPage{Metadata: metadata, Items: []learning.RouteProjection{{Route: routeRevision, EventSequence: 12, Current: true}}, NextCursor: "opaque"}
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

func TestOpenAPIDeclaresMemoryPrivacyRoutesAndContracts(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	paths := raw["paths"].(map[string]any)
	type contract struct {
		path, method, scope string
		write               bool
		codes               []string
	}
	contracts := []contract{
		{"/v1/memory/candidates", "post", "memory:write", true, []string{"200", "201", "400", "401", "403", "409", "413", "422", "429", "500", "503"}},
		{"/v1/memory/candidates", "get", "memory:read", false, []string{"200", "400", "401", "403", "409", "429", "500", "503"}},
		{"/v1/memory/candidates/{candidateID}", "get", "memory:read", false, []string{"200", "400", "401", "403", "404", "429", "500", "503"}},
		{"/v1/memory/candidates/{candidateID}/decisions", "post", "memory:write", true, []string{"200", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503"}},
		{"/v1/memory/records", "get", "memory:read", false, []string{"200", "400", "401", "403", "409", "429", "500", "503"}},
		{"/v1/memory/records/{memoryID}", "get", "memory:read", false, []string{"200", "400", "401", "403", "404", "429", "500", "503"}},
		{"/v1/memory/records/{memoryID}/candidates", "post", "memory:write", true, []string{"200", "201", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503"}},
		{"/v1/memory/records/{memoryID}", "delete", "memory:write", true, []string{"200", "202", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503"}},
		{"/v1/memory/export", "get", "memory:read", false, []string{"200", "400", "401", "403", "409", "429", "500", "503"}},
		{"/v1/memory/deliveries/{deliveryID}/replays", "post", "memory:write", true, []string{"200", "202", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503"}},
		{"/v1/privacy/erasures", "post", "privacy:erase", true, []string{"202", "400", "401", "403", "409", "413", "422", "429", "500", "503"}},
		{"/v1/privacy/erasures/{erasureID}", "get", "privacy:read", false, []string{"200", "400", "401", "403", "404", "409", "429", "500", "503"}},
		{"/v1/privacy/erasures/{erasureID}/offline-device-purge", "get", "privacy:device", false, []string{"200", "204", "400", "401", "403", "404", "409", "429", "500", "503"}},
		{"/v1/privacy/erasures/{erasureID}/offline-device-purge/ack", "post", "privacy:device", true, []string{"200", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503"}},
	}
	for _, expected := range contracts {
		item, ok := paths[expected.path].(map[string]any)
		if !ok {
			t.Fatalf("path %s is missing", expected.path)
		}
		operation, ok := item[expected.method].(map[string]any)
		if !ok || operation["x-required-scope"] != expected.scope {
			t.Fatalf("%s %s scope/method mismatch: %#v", expected.method, expected.path, operation)
		}
		if expected.write && (operation["x-max-body-bytes"] != 1048576 || operation["requestBody"] == nil) {
			t.Errorf("%s %s lacks frozen 1MiB request contract", expected.method, expected.path)
		}
		responses := operation["responses"].(map[string]any)
		for _, code := range expected.codes {
			if responses[code] == nil {
				t.Errorf("%s %s missing response %s", expected.method, expected.path, code)
			}
		}
	}
	for _, path := range []string{"/v1/memory/candidates", "/v1/memory/records", "/v1/memory/export"} {
		encoded, _ := json.Marshal(paths[path].(map[string]any)["get"].(map[string]any)["parameters"])
		if !strings.Contains(string(encoded), "#/components/parameters/Cursor") || !strings.Contains(string(encoded), "#/components/parameters/PageLimit") {
			t.Errorf("%s lacks cursor and limit", path)
		}
	}
	security, _ := json.Marshal(paths["/v1/privacy/erasures"].(map[string]any)["post"].(map[string]any)["security"])
	if !strings.Contains(string(security), "deviceBearer") || !strings.Contains(string(security), "privacyErasureGrant") {
		t.Fatalf("privacy erasure security does not require bearer and grant: %s", security)
	}
	erasureOperation := paths["/v1/privacy/erasures"].(map[string]any)["post"].(map[string]any)
	if erasureOperation["x-required-erasure-grant"] != true || erasureOperation["x-effective-scope-source"] != "privacyErasureGrant" ||
		!strings.Contains(erasureOperation["description"].(string), "effective privacy:erase") ||
		!strings.Contains(erasureOperation["description"].(string), "already committed operation may omit the grant") {
		t.Fatalf("privacy effective scope/grant semantics drifted: %#v", erasureOperation)
	}
	schemes := raw["components"].(map[string]any)["securitySchemes"].(map[string]any)
	deviceBearer := schemes["deviceBearer"].(map[string]any)
	grant := schemes["privacyErasureGrant"].(map[string]any)
	if grant["in"] != "header" || grant["name"] != "X-Privacy-Erasure-Grant" ||
		!strings.Contains(grant["description"].(string), "same serializable transaction") ||
		!strings.Contains(deviceBearer["description"].(string), "do not include privacy:erase") {
		t.Fatalf("privacy erasure security description drifted: bearer=%#v grant=%#v", deviceBearer, grant)
	}
	device := raw["components"].(map[string]any)["schemas"].(map[string]any)["Device"].(map[string]any)
	scopeDescription := device["properties"].(map[string]any)["scopes"].(map[string]any)["description"].(string)
	if !strings.Contains(scopeDescription, "never privacy:erase") {
		t.Fatalf("default credential scope description drifted: %q", scopeDescription)
	}
}

func TestMemoryPrivacyOpenAPISchemasAreClosedAndRejectClientOwnedFields(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	schemas := raw["components"].(map[string]any)["schemas"].(map[string]any)
	var assertClosed func(string, any)
	assertClosed = func(path string, value any) {
		switch typed := value.(type) {
		case map[string]any:
			if typed["type"] == "object" && typed["additionalProperties"] != false {
				t.Errorf("%s is not additionalProperties:false", path)
			}
			for key, child := range typed {
				assertClosed(path+"."+key, child)
			}
		case []any:
			for _, child := range typed {
				assertClosed(path+"[]", child)
			}
		}
	}
	for name, schema := range schemas {
		if strings.HasPrefix(name, "Memory") || strings.HasPrefix(name, "Privacy") || strings.HasPrefix(name, "OfflinePurge") {
			assertClosed(name, schema)
		}
	}

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
	candidate := `{"operation_id":"10000000-0000-4000-8000-000000000001","payload_schema_version":1,"content":"concise answers","reason":"explicit preference","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable","valid_until":"2030-01-01T00:00:00Z"}`
	candidateSchema := document.Components.Schemas["MemoryCandidateRequest"].Value
	if err := candidateSchema.VisitJSON(decode(candidate), openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("valid memory request failed schema: %v", err)
	}
	offsetCandidate := strings.Replace(candidate, "2030-01-01T00:00:00Z", "2030-01-01T00:00:00+00:00", 1)
	if err := candidateSchema.VisitJSON(decode(offsetCandidate), openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("RFC3339 offset memory request failed schema: %v", err)
	}
	for _, forbidden := range []string{"source_kind", "source_event_id", "source_operation_id", "model_id", "proposer_id", "device_id", "namespace", "domain"} {
		invalid := strings.TrimSuffix(candidate, "}") + `,"` + forbidden + `":"client-owned"}`
		if err := candidateSchema.VisitJSON(decode(invalid), openapi3.EnableJSONSchema2020()); err == nil {
			t.Fatalf("memory request accepted client-owned field %s", forbidden)
		}
	}
	erasureSchema := document.Components.Schemas["PrivacyErasureRequest"].Value
	erasure := `{"operation_id":"10000000-0000-4000-8000-000000000001","payload_schema_version":1,"expected_current_learner_generation":1,"reason_code":"learner_request","explicit_confirmation":true}`
	if err := erasureSchema.VisitJSON(decode(erasure), openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("valid erasure request failed schema: %v", err)
	}
	for _, invalid := range []string{
		strings.Replace(erasure, `"explicit_confirmation":true`, `"explicit_confirmation":false`, 1),
		strings.TrimSuffix(erasure, "}") + `,"device_id":"90000000-0000-4000-8000-000000000001"}`,
		strings.TrimSuffix(erasure, "}") + `,"requested_at":"2026-08-20T14:00:00Z"}`,
	} {
		if err := erasureSchema.VisitJSON(decode(invalid), openapi3.EnableJSONSchema2020()); err == nil {
			t.Fatalf("erasure schema accepted server-owned or unconfirmed input: %s", invalid)
		}
	}
	ackSchema := document.Components.Schemas["OfflinePurgeAckRequest"].Value
	challenge := strings.Repeat("A", 43)
	for _, valid := range []string{
		`{"challenge_revision":1,"challenge":"` + challenge + `","outcome":"succeeded","managed_objects_absent":true}`,
		`{"challenge_revision":1,"challenge":"` + challenge + `","outcome":"failed","failure_code":"profile_busy"}`,
	} {
		if err := ackSchema.VisitJSON(decode(valid), openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatalf("valid offline purge acknowledgment failed schema: %v", err)
		}
	}
	for _, invalid := range []string{
		`{"challenge_revision":1,"challenge":"` + challenge + `","outcome":"succeeded"}`,
		`{"challenge_revision":1,"challenge":"` + challenge + `","outcome":"succeeded","managed_objects_absent":true,"failure_code":"profile_busy"}`,
		`{"challenge_revision":1,"challenge":"` + challenge + `","outcome":"failed","managed_objects_absent":false,"failure_code":"profile_busy"}`,
	} {
		if err := ackSchema.VisitJSON(decode(invalid), openapi3.EnableJSONSchema2020()); err == nil {
			t.Fatalf("offline purge acknowledgment accepted invalid shape: %s", invalid)
		}
	}
}

func TestMemoryPrivacyOpenAPIValidatesRepresentativeResponses(t *testing.T) {
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
	validate := func(path, method, code string, value any) {
		t.Helper()
		item := document.Paths.Find(path)
		var response *openapi3.ResponseRef
		switch method {
		case "get":
			response = item.Get.Responses.Value(code)
		case "post":
			response = item.Post.Responses.Value(code)
		case "delete":
			response = item.Delete.Responses.Value(code)
		}
		schema := response.Value.Content.Get("application/json").Schema.Value
		if err := schema.VisitJSON(decodeDTO(value), openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatalf("%s %s %s failed schema: %v", method, path, code, err)
		}
	}
	now := time.Date(2026, time.August, 20, 14, 0, 0, 0, time.UTC)
	candidate := memory.Candidate{
		ID: "20000000-0000-4000-8000-000000000001", URI: "candidate://20000000-0000-4000-8000-000000000001",
		PayloadID: "30000000-0000-4000-8000-000000000001", ContentHash: strings.Repeat("a", 64),
		Source: memory.SourceUserStatement, SourceReference: memory.SourceReference{}, ProposerID: "90000000-0000-4000-8000-000000000001",
		Reason: "explicit preference", Category: memory.CategoryInteractionPreference,
		Sensitivity: memory.SensitivityNonSensitive, Stability: memory.StabilityStable,
		ValidUntil: now.Add(time.Hour), PolicyVersion: memory.AdmissionPolicyVersion,
		Status: memory.CandidatePending, Revision: 1, CreatedAt: now,
	}
	candidateView := memory.CandidateView{Candidate: candidate, ContentStatus: "available", ProposedContent: "concise answers", ReadGeneration: memory.GenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	validate("/v1/memory/candidates/{candidateID}", "get", "200", candidateView)

	record := memory.Record{
		LogicalMemoryID: "40000000-0000-4000-8000-000000000001", ID: "50000000-0000-4000-8000-000000000001",
		Revision: 1, RecordGeneration: 1, LearnerGeneration: 1, CandidateID: candidate.ID,
		ExternalURI:       "nocturne://core/edu-agent/40000000-0000-4000-8000-000000000001",
		ExternalURIDigest: strings.Repeat("b", 64), ContentHash: strings.Repeat("c", 64), Status: memory.RecordApplied,
		DeliveryID: "60000000-0000-4000-8000-000000000001", ReceiptID: "70000000-0000-4000-8000-000000000001", CreatedAt: now,
	}
	delivery := memory.Delivery{
		ID: record.DeliveryID, Kind: memory.DeliveryAdmit, LogicalMemoryID: record.LogicalMemoryID,
		RecordRevisionID: record.ID, RecordRevision: 1, LearnerGeneration: 1, RecordGeneration: 1,
		PayloadID: "80000000-0000-4000-8000-000000000001", PayloadHash: record.ContentHash,
		ExternalURI: record.ExternalURI, AttemptState: memory.AttemptConfirmed, Status: memory.DeliveryStatusApplied,
		PublicStatus: memory.DeliveryApplied, ValidUntil: now.Add(time.Hour), ReceiptID: record.ReceiptID, CreatedAt: now, UpdatedAt: now,
	}
	receipt := memory.Receipt{ID: record.ReceiptID, DeliveryID: delivery.ID, Version: 1, Status: memory.ReceiptSucceeded, Reason: "hash_verified", VerificationMethod: "remote_readback", CreatedAt: now}
	recordDetail := memory.RecordDetail{
		Record: record, Delivery: delivery, Receipt: receipt,
		ReadGeneration: memory.GenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1},
		ContentStatus:  memory.ExportContentAvailable, Content: "concise answers",
	}
	validate("/v1/memory/records/{memoryID}", "get", "200", recordDetail)
	export := memory.ExportPage{Items: []memory.ExportItem{{Record: record, DeliveryStatus: memory.DeliveryApplied, Receipt: receipt, ContentStatus: memory.ExportContentAvailable, Content: "concise answers"}}, ReadGeneration: recordDetail.ReadGeneration, ReasonCodes: []string{}}
	validate("/v1/memory/export", "get", "200", export)

	privacyReceipt := privacy.ErasureReceipt{
		ErasureID: "90000000-0000-4000-8000-000000000002", Status: privacy.StatusPartial,
		SummaryVersion: 2, LearnerGeneration: 2, PolicyVersion: privacy.PolicyVersion,
		ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, UpdatedAt: now,
		Steps: []privacy.StepReceipt{{ID: "90000000-0000-4000-8000-000000000003", Store: privacy.StoreExternalProvider, Version: 1, Status: privacy.StepUnsupported, StableReason: "provider_controlled", VerificationMethod: "unsupported_by_local_core", StartedAt: now, CompletedAt: &now}},
	}
	validate("/v1/privacy/erasures", "post", "202", privacyReceipt)
	validate("/v1/privacy/erasures/{erasureID}", "get", "200", privacyReceipt)
	purgeTask := privacy.OfflinePurgeChallenge{
		ErasureID: privacyReceipt.ErasureID, DeviceID: "90000000-0000-4000-8000-000000000004",
		OldGeneration: 1, CurrentGeneration: 2, ChallengeRevision: 1,
		Challenge: strings.Repeat("A", 43), IssuedAt: now, Status: privacy.OfflineDeviceChildPending,
	}
	validate("/v1/privacy/erasures/{erasureID}/offline-device-purge", "get", "200", purgeTask)
	purgeAck := privacy.OfflineDeviceChildReceipt{
		ErasureID: privacyReceipt.ErasureID, DeviceID: purgeTask.DeviceID,
		SourceGeneration: 1, CurrentGeneration: 2, ChallengeRevision: 1,
		Status: privacy.OfflineDeviceChildSucceeded, UpdatedAt: now, StableReason: "device_acknowledged",
	}
	validate("/v1/privacy/erasures/{erasureID}/offline-device-purge/ack", "post", "200", purgeAck)
}

func TestNotesyncOpenAPIContractsAreClosedScopedAndMatchDomain(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	paths, ok := raw["paths"].(map[string]any)
	if !ok {
		t.Fatalf("OpenAPI paths are missing: %#v", raw["paths"])
	}
	contracts := map[string]struct {
		method string
		scope  string
		codes  []string
	}{
		"/v1/knowledge/notesync/status":                         {"get", "knowledge:read", []string{"200", "400", "401", "403", "429", "500", "503"}},
		"/v1/knowledge/notesync/previews":                       {"post", "knowledge:read", []string{"200", "400", "401", "403", "404", "409", "413", "429", "500", "503"}},
		"/v1/knowledge/notesync/reviews":                        {"get", "knowledge:read", []string{"200", "400", "401", "403", "409", "429", "500", "503"}},
		"/v1/knowledge/notesync/reviews/{reviewID}":             {"get", "knowledge:read", []string{"200", "400", "401", "403", "404", "409", "429", "500", "503"}},
		"/v1/knowledge/notesync/reviews/{reviewID}/resolutions": {"post", "knowledge:write", []string{"200", "201", "400", "401", "403", "404", "409", "413", "429", "500", "503"}},
	}
	for route, contract := range contracts {
		item, ok := paths[route].(map[string]any)
		if !ok {
			t.Fatalf("NoteSync route %s is missing", route)
		}
		operation, ok := item[contract.method].(map[string]any)
		if !ok {
			t.Fatalf("NoteSync route %s %s operation is missing", contract.method, route)
		}
		if operation["x-required-scope"] != contract.scope {
			t.Fatalf("NoteSync route %s scope=%v want=%s", route, operation["x-required-scope"], contract.scope)
		}
		if operation["security"] == nil {
			t.Fatalf("NoteSync route %s has no device authentication contract", route)
		}
		responses, ok := operation["responses"].(map[string]any)
		if !ok {
			t.Fatalf("NoteSync route %s responses are missing", route)
		}
		for _, code := range contract.codes {
			if responses[code] == nil {
				t.Errorf("NoteSync route %s is missing response %s", route, code)
			}
		}
		if route == "/v1/knowledge/notesync/previews" || route == "/v1/knowledge/notesync/reviews/{reviewID}/resolutions" {
			if operation["x-max-body-bytes"] != 16777216 {
				t.Errorf("NoteSync route %s body limit=%v want=16777216", route, operation["x-max-body-bytes"])
			}
			requestBody, ok := operation["requestBody"].(map[string]any)
			if !ok || requestBody["required"] != true {
				t.Errorf("NoteSync route %s does not require a JSON request body", route)
			}
		}
	}

	components, ok := raw["components"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI components are missing")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI schemas are missing")
	}
	for name, value := range schemas {
		if !strings.HasPrefix(name, "Notesync") {
			continue
		}
		schema, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("NoteSync schema %s is not an object", name)
		}
		if schema["type"] == "object" && schema["additionalProperties"] != false {
			t.Errorf("NoteSync schema %s is not closed", name)
		}
	}

	propertyEnums := func(schemaName, propertyName string) map[string]bool {
		t.Helper()
		schema, ok := schemas[schemaName].(map[string]any)
		if !ok {
			t.Fatalf("schema %s is missing", schemaName)
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("schema %s properties are missing", schemaName)
		}
		property, ok := properties[propertyName].(map[string]any)
		if !ok {
			t.Fatalf("schema %s property %s is missing", schemaName, propertyName)
		}
		values, ok := property["enum"].([]any)
		if !ok {
			t.Fatalf("schema %s property %s enum is missing", schemaName, propertyName)
		}
		result := make(map[string]bool, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				t.Fatalf("schema %s property %s has non-string enum value %#v", schemaName, propertyName, value)
			}
			result[text] = true
		}
		return result
	}
	assertExactEnum := func(schemaName, propertyName string, expected []string) {
		t.Helper()
		actual := propertyEnums(schemaName, propertyName)
		want := make(map[string]bool, len(expected))
		for _, value := range expected {
			want[value] = true
		}
		if len(actual) != len(want) {
			t.Fatalf("schema %s property %s enum=%v want=%v", schemaName, propertyName, actual, want)
		}
		for value := range want {
			if !actual[value] {
				t.Fatalf("schema %s property %s is missing enum %q: got=%v", schemaName, propertyName, value, actual)
			}
		}
	}
	assertExactEnum("NotesyncPreviewItem", "category", []string{"in_sync", "remote_unchanged", "local_changed", "remote_changed", "both_changed", "remote_missing", "remote_moved", "unbased_remote", "path_occupied", "invalid_remote_markdown"})
	assertExactEnum("NotesyncPreviewItem", "reason_code", []string{"in_sync", "local_revision_changed", "both_sides_changed", "remote_identity_moved", "unmanaged_remote_note", "remote_markdown_invalid", "remote_content_changed", "remote_note_missing", "remote_path_occupied", "publication_preflight_changed", "publication_readback_changed"})
	assertExactEnum("NotesyncResolutionRequest", "kind", []string{"accept_remote", "keep_canonical", "merged"})
	assertExactEnum("NotesyncReview", "resolution_kind", []string{"accept_remote", "keep_canonical", "merged", "superseded", "privacy_redaction"})
	assertExactEnum("NotesyncReviewSummary", "resolution_kind", []string{"accept_remote", "keep_canonical", "merged", "superseded", "privacy_redaction"})
	assertExactEnum("NotesyncReview", "status", []string{"open", "resolved", "closed"})

	resolutionSchema := schemas["NotesyncResolutionRequest"].(map[string]any)
	resolutionProperties := resolutionSchema["properties"].(map[string]any)
	for _, name := range []string{
		"basis_hash", "operation_id", "kind", "merged_markdown",
		"identity_review_basis_hash", "identity_review_operation_id", "identity_review_receipt",
		"document_resolutions", "node_resolutions",
	} {
		if resolutionProperties[name] == nil {
			t.Errorf("resolution request is missing property %s", name)
		}
	}
	if resolutionProperties["device_id"] != nil {
		t.Fatal("resolution request exposes server-owned device_id")
	}
	required, ok := resolutionSchema["required"].([]any)
	if !ok {
		t.Fatal("resolution request required fields are missing")
	}
	requiredSet := make(map[string]bool, len(required))
	for _, value := range required {
		if name, ok := value.(string); ok {
			requiredSet[name] = true
		}
	}
	for _, name := range []string{"basis_hash", "operation_id", "kind"} {
		if !requiredSet[name] {
			t.Errorf("resolution request does not require %s", name)
		}
	}

	summarySchema := schemas["NotesyncReviewSummary"].(map[string]any)
	summaryProperties := summarySchema["properties"].(map[string]any)
	if _, exists := summaryProperties["diff"]; exists || summaryProperties["markdown"] != nil {
		t.Fatal("review summary exposes diff or Markdown content")
	}
	summarySnapshotSchema := schemas["NotesyncReviewSnapshotSummary"].(map[string]any)
	if summarySnapshotSchema["properties"].(map[string]any)["markdown"] != nil {
		t.Fatal("review summary snapshot exposes Markdown content")
	}
	detailSchema := schemas["NotesyncReview"].(map[string]any)
	if _, exists := detailSchema["properties"].(map[string]any)["diff"]; !exists {
		t.Fatal("review detail does not expose diff")
	}
	snapshotSchema := schemas["NotesyncReviewSnapshot"].(map[string]any)
	snapshotProperties := snapshotSchema["properties"].(map[string]any)
	if _, exists := snapshotProperties["markdown"]; !exists {
		t.Fatal("review detail snapshot does not expose Markdown")
	}
	if !strings.Contains(string(mustJSON(snapshotSchema["required"])), "markdown") {
		t.Fatal("review detail snapshot does not require present Markdown field")
	}

	errorSchema := schemas["NotesyncErrorEnvelope"].(map[string]any)
	errorProperties := errorSchema["properties"].(map[string]any)
	errorObject := errorProperties["error"].(map[string]any)
	errorCodes := make(map[string]bool)
	for _, value := range errorObject["properties"].(map[string]any)["code"].(map[string]any)["enum"].([]any) {
		if code, ok := value.(string); ok {
			errorCodes[code] = true
		}
	}
	for _, code := range []string{"notesync_not_configured", "invalid_request", "not_found", "stale_notesync_review", "idempotency_conflict", "content_redacted", "privacy_clear_in_progress", "notesync_unavailable", "payload_too_large", "path_occupied", "identity_review_required", "stale_identity_review", "revision_conflict"} {
		if !errorCodes[code] {
			t.Errorf("NoteSync error enum is missing %q", code)
		}
	}
	if errorCodes["stale_review"] {
		t.Fatal("NoteSync error enum uses non-domain stale_review code")
	}

	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	decode := func(value any) any {
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
	snapshot := document.Components.Schemas["NotesyncReviewSnapshot"].Value
	for _, missing := range []bool{true, false} {
		value := map[string]any{"missing": missing, "markdown": "", "remote_version": 0, "remote_last_time": 0}
		if err := snapshot.VisitJSON(decode(value), openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatalf("present-empty snapshot missing=%t failed schema validation: %v", missing, err)
		}
	}
	previewResult := document.Components.Schemas["NotesyncPreviewResult"].Value
	if err := previewResult.VisitJSON(decode(map[string]any{"items": []any{}, "page": 1, "page_size": 25, "total_rows": 0}), openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("empty preview result failed schema validation: %v", err)
	}

	responses := components["responses"].(map[string]any)
	for _, responseName := range []string{"NotesyncBadRequest", "NotesyncUnauthorized", "NotesyncForbidden", "NotesyncNotFound", "NotesyncPayloadTooLarge", "NotesyncRateLimited", "NotesyncInternalError", "NotesyncUnavailable"} {
		response := responses[responseName].(map[string]any)
		content := response["content"].(map[string]any)["application/json"].(map[string]any)
		if content["schema"].(map[string]any)["$ref"] != "#/components/schemas/NotesyncErrorEnvelope" {
			t.Errorf("response %s does not use NotesyncErrorEnvelope", responseName)
		}
	}
}

func mustJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}
