package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/getkin/kin-openapi/openapi3"
)

type fakeLearning struct {
	actor            string
	actors           []string
	method           string
	calls            int
	goal             learning.GoalCommand
	session          learning.SessionCommand
	proposal         learning.ProposalRequest
	action           learning.ActionCommand
	decision         learning.AssessmentDecisionCommand
	timeline         learning.TimelineQuery
	page             learning.CursorPageRequest
	evidence         learning.EvidenceQuery
	review           learning.ReviewQuery
	err              error
	operation        learning.OperationResult
	proposalResult   learning.ProposalArtifact
	sessionView      learning.SessionView
	timelinePage     learning.TimelinePage
	routesPage       learning.RoutesPage
	nodeView         learning.NodeView
	evidencePage     learning.EvidencePage
	reviewsPage      learning.ReviewsPage
	status           learning.ProjectionStatus
	currentSessionFn func(context.Context) (learning.SessionView, error)
}

func (f *fakeLearning) called(method, actor string) {
	f.method, f.actor = method, actor
	f.actors = append(f.actors, actor)
	f.calls++
}
func (f *fakeLearning) operationResult(aggregateType, aggregateID string) learning.OperationResult {
	if f.operation.Status != "" {
		return f.operation
	}
	return learning.OperationResult{Status: "succeeded", AggregateType: aggregateType, AggregateID: aggregateID, AggregateVersion: 1, FirstEventSequence: 1, LastEventSequence: 1, ProjectionAsOf: 1, Result: []byte(`{}`)}
}
func (f *fakeLearning) CreateGoal(_ context.Context, actor string, command learning.GoalCommand) (learning.OperationResult, error) {
	f.called("create_goal", actor)
	f.goal = command
	if f.err != nil {
		return learning.OperationResult{}, f.err
	}
	return f.operationResult("goal", command.Operation.AggregateID), nil
}
func (f *fakeLearning) CreateSession(_ context.Context, actor string, command learning.SessionCommand) (learning.OperationResult, error) {
	f.called("create_session", actor)
	f.session = command
	if f.err != nil {
		return learning.OperationResult{}, f.err
	}
	return f.operationResult("session", command.Operation.AggregateID), nil
}
func (f *fakeLearning) Propose(_ context.Context, actor string, request learning.ProposalRequest) (learning.ProposalArtifact, error) {
	f.called("proposal", actor)
	f.proposal = request
	return f.proposalResult, f.err
}
func (f *fakeLearning) ApplyAction(_ context.Context, actor, _ string, command learning.ActionCommand) (learning.OperationResult, error) {
	f.called("action", actor)
	f.action = command
	if f.err != nil {
		return learning.OperationResult{}, f.err
	}
	return f.operationResult("session", command.Operation.AggregateID), nil
}
func (f *fakeLearning) Decide(_ context.Context, actor, _ string, command learning.AssessmentDecisionCommand) (learning.OperationResult, error) {
	f.called("decision", actor)
	f.decision = command
	if f.err != nil {
		return learning.OperationResult{}, f.err
	}
	return f.operationResult("session", command.Operation.AggregateID), nil
}
func (f *fakeLearning) CurrentSession(ctx context.Context) (learning.SessionView, error) {
	f.called("current_session", "")
	if f.currentSessionFn != nil {
		return f.currentSessionFn(ctx)
	}
	return f.sessionView, f.err
}
func (f *fakeLearning) Session(context.Context, string) (learning.SessionView, error) {
	f.called("session", "")
	return f.sessionView, f.err
}
func (f *fakeLearning) Timeline(_ context.Context, query learning.TimelineQuery) (learning.TimelinePage, error) {
	f.called("timeline", "")
	f.timeline = query
	return f.timelinePage, f.err
}
func (f *fakeLearning) Routes(_ context.Context, page learning.CursorPageRequest) (learning.RoutesPage, error) {
	f.called("routes", "")
	f.page = page
	return f.routesPage, f.err
}
func (f *fakeLearning) Node(context.Context, string) (learning.NodeView, error) {
	f.called("node", "")
	return f.nodeView, f.err
}
func (f *fakeLearning) Evidence(_ context.Context, query learning.EvidenceQuery) (learning.EvidencePage, error) {
	f.called("evidence", "")
	f.evidence = query
	return f.evidencePage, f.err
}
func (f *fakeLearning) Reviews(_ context.Context, query learning.ReviewQuery) (learning.ReviewsPage, error) {
	f.called("reviews", "")
	f.review = query
	return f.reviewsPage, f.err
}
func (f *fakeLearning) ProjectionStatus(context.Context) (learning.ProjectionStatus, error) {
	f.called("projection_status", "")
	return f.status, f.err
}

func newLearningTestAPI(t *testing.T, scopes []string, service *fakeLearning, logs *bytes.Buffer) http.Handler {
	t.Helper()
	return newLearningTestAPIWithPermits(t, scopes, service, nil, logs)
}

func newLearningTestAPIWithPermits(t *testing.T, scopes []string, service *fakeLearning, permits *privacy.ReadPermitManager, logs *bytes.Buffer) http.Handler {
	t.Helper()
	id := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: "90000000-0000-4000-8000-000000000001"}, Scopes: scopes}}
	handler, err := New(Options{Identity: id, Learning: service, Readiness: fakeReadiness{}, ReadPermits: permits, Logger: slog.New(slog.NewJSONHandler(logs, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestLearningHTTPEnforcesScopeBodyLimitAndActor(t *testing.T) {
	valid := `{"operation_id":"10000000-0000-4000-8000-000000000001","payload_schema_version":1,"aggregate_type":"goal","aggregate_id":"20000000-0000-4000-8000-000000000001","expected_version":0,"text":"Learn fractions","source":"device"}`
	var logs bytes.Buffer
	service := &fakeLearning{}
	handler := newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
	request := httptest.NewRequest(http.MethodPost, "/v1/learning/goals", strings.NewReader(valid))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || service.actor != "" {
		t.Fatalf("scope failure = %d actor=%q", response.Code, service.actor)
	}

	handler = newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
	request = httptest.NewRequest(http.MethodPost, "/v1/learning/goals", strings.NewReader(valid))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || service.actor != "90000000-0000-4000-8000-000000000001" {
		t.Fatalf("goal = %d actor=%q body=%s", response.Code, service.actor, response.Body.String())
	}
	if service.goal.Operation.PayloadSchemaVersion != 1 || service.goal.Operation.ExpectedVersion != 0 {
		t.Fatalf("operation envelope not preserved: %#v", service.goal.Operation)
	}

	oversized := `{"padding":"` + strings.Repeat("x", (1<<20)+1) + `"}`
	for _, path := range []string{
		"/v1/learning/goals",
		"/v1/tutoring/sessions",
		"/v1/tutoring/proposals",
		"/v1/tutoring/sessions/" + testAggregateID + "/actions",
		"/v1/learning/assessments/" + testAssessment + "/decisions",
	} {
		service = &fakeLearning{}
		handler = newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
		response = learningRequest(t, handler, http.MethodPost, path, oversized)
		if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "payload_too_large") || service.calls != 0 {
			t.Fatalf("body limit %s = %d calls=%d %s", path, response.Code, service.calls, response.Body.String())
		}
	}
}

func TestLearningHTTPValidatesCursorAndRedactsFailures(t *testing.T) {
	var logs bytes.Buffer
	service := &fakeLearning{}
	handler := newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
	request := httptest.NewRequest(http.MethodGet, "/v1/learning/evidence?limit=201", nil)
	request.Header.Set("Authorization", "Bearer secret-learning-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid page = %d %s", response.Code, response.Body.String())
	}

	service.err = &learning.Error{Code: learning.CodeStaleCursor, Cause: errors.New("database-password=secret")}
	request = httptest.NewRequest(http.MethodGet, "/v1/learning/evidence?cursor=old", nil)
	request.Header.Set("Authorization", "Bearer secret-learning-token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), learning.CodeStaleCursor) {
		t.Fatalf("stale cursor = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "database-password") || strings.Contains(logs.String(), "secret-learning-token") || strings.Contains(logs.String(), "database-password") {
		t.Fatalf("learning failure leaked secret: response=%s logs=%s", response.Body.String(), logs.String())
	}
}

const (
	testOperationID = "10000000-0000-4000-8000-000000000001"
	testAggregateID = "20000000-0000-4000-8000-000000000001"
	testRelatedID   = "30000000-0000-4000-8000-000000000001"
	testNodeID      = "40000000-0000-4000-8000-000000000001"
	testAssessment  = "50000000-0000-4000-8000-000000000001"
	testActorID     = "90000000-0000-4000-8000-000000000001"
)

func learningRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func validAssessmentItemJSON() string {
	sha := strings.Repeat("a", 64)
	return `{"rubric_item_id":"item-1","conclusion":"pass","answer_quote":"answer","answer_range":{"start":0,"end":1},"answer_quote_sha256":"` + sha + `","knowledge_reference_id":"","knowledge_quote":"knowledge","knowledge_range":{"start":0,"end":1},"knowledge_quote_sha256":"` + sha + `"}`
}

func validLearningBodies() map[string]string {
	operation := `"operation_id":"` + testOperationID + `","payload_schema_version":1,"aggregate_type":"session","aggregate_id":"` + testAggregateID + `","expected_version":0`
	return map[string]string{
		"goal":     `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"aggregate_type":"goal","aggregate_id":"` + testAggregateID + `","expected_version":0,"text":"Learn fractions","source":"device"}`,
		"session":  `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"aggregate_type":"session","aggregate_id":"` + testAggregateID + `","expected_version":0,"goal_revision_id":"` + testRelatedID + `"}`,
		"proposal": `{"request_id":"` + testOperationID + `","proposal_type":"route","aggregate_type":"goal","aggregate_id":"` + testAggregateID + `","aggregate_version":1,"goal_revision_id":"` + testRelatedID + `","knowledge_revision_id":"` + testRelatedID + `","node_revision_ids":["` + testNodeID + `"],"input":{}}`,
		"action":   `{` + operation + `,"action":"start_diagnostic"}`,
		"decision": `{` + operation + `,"kind":"confirm","expected_disposition_version":1}`,
	}
}

func TestLearningHTTPRouteMethodScopeAndActorMatrix(t *testing.T) {
	bodies := validLearningBodies()
	contracts := []struct {
		name, method, path, scope, body string
		status                          int
	}{
		{"create_goal", http.MethodPost, "/v1/learning/goals", "learning:write", bodies["goal"], http.StatusCreated},
		{"create_session", http.MethodPost, "/v1/tutoring/sessions", "learning:write", bodies["session"], http.StatusCreated},
		{"proposal", http.MethodPost, "/v1/tutoring/proposals", "learning:write", bodies["proposal"], http.StatusCreated},
		{"action", http.MethodPost, "/v1/tutoring/sessions/" + testAggregateID + "/actions", "learning:write", bodies["action"], http.StatusCreated},
		{"decision", http.MethodPost, "/v1/learning/assessments/" + testAssessment + "/decisions", "learning:write", bodies["decision"], http.StatusCreated},
		{"current_session", http.MethodGet, "/v1/tutoring/sessions/current", "learning:read", "", http.StatusOK},
		{"session", http.MethodGet, "/v1/tutoring/sessions/" + testAggregateID, "learning:read", "", http.StatusOK},
		{"timeline", http.MethodGet, "/v1/learning/timeline", "learning:read", "", http.StatusOK},
		{"routes", http.MethodGet, "/v1/learning/routes", "learning:read", "", http.StatusOK},
		{"node", http.MethodGet, "/v1/learning/nodes/" + testNodeID, "learning:read", "", http.StatusOK},
		{"evidence", http.MethodGet, "/v1/learning/evidence", "learning:read", "", http.StatusOK},
		{"reviews", http.MethodGet, "/v1/learning/reviews", "learning:read", "", http.StatusOK},
		{"projection_status", http.MethodGet, "/v1/learning/projections/status", "learning:read", "", http.StatusOK},
	}
	for _, contract := range contracts {
		t.Run(contract.name, func(t *testing.T) {
			var logs bytes.Buffer
			service := &fakeLearning{}
			handler := newLearningTestAPI(t, []string{contract.scope}, service, &logs)
			response := learningRequest(t, handler, contract.method, contract.path, contract.body)
			if response.Code != contract.status || service.calls != 1 || service.method != contract.name {
				t.Fatalf("authorized route=%d calls=%d method=%q body=%s", response.Code, service.calls, service.method, response.Body.String())
			}
			if contract.scope == "learning:write" && service.actor != testActorID {
				t.Fatalf("write actor=%q, want auth actor %q", service.actor, testActorID)
			}

			service = &fakeLearning{}
			wrongScope := "learning:read"
			if contract.scope == wrongScope {
				wrongScope = "learning:write"
			}
			handler = newLearningTestAPI(t, []string{wrongScope}, service, &logs)
			response = learningRequest(t, handler, contract.method, contract.path, contract.body)
			if response.Code != http.StatusForbidden || service.calls != 0 {
				t.Fatalf("wrong scope=%d calls=%d", response.Code, service.calls)
			}

			service = &fakeLearning{}
			handler = newLearningTestAPI(t, []string{contract.scope}, service, &logs)
			wrongMethod := http.MethodGet
			if contract.method == http.MethodGet {
				wrongMethod = http.MethodDelete
			}
			response = learningRequest(t, handler, wrongMethod, contract.path, contract.body)
			if response.Code != http.StatusMethodNotAllowed || service.calls != 0 {
				t.Fatalf("wrong method=%d calls=%d", response.Code, service.calls)
			}
		})
	}
}

func TestLearningHTTPStrictActionAndWriteDecoding(t *testing.T) {
	bodies := validLearningBodies()
	validAction := bodies["action"]
	invalidUTF8 := []byte(validAction)
	invalidUTF8[len(invalidUTF8)-2] = 0xff
	cases := map[string]string{
		"unknown action field":     strings.TrimSuffix(validAction, "}") + `,"unknown":true}`,
		"unrelated action field":   strings.TrimSuffix(validAction, "}") + `,"proposal_id":"` + testRelatedID + `"}`,
		"missing discriminator":    strings.Replace(validAction, `,"action":"start_diagnostic"`, "", 1),
		"duplicate key":            strings.Replace(validAction, `"expected_version":0`, `"expected_version":0,"expected_version":0`, 1),
		"trailing json":            validAction + `{}`,
		"invalid utf8":             string(invalidUTF8),
		"missing expected version": strings.Replace(validAction, `,"expected_version":0`, "", 1),
		"missing schema version":   strings.Replace(validAction, `,"payload_schema_version":1`, "", 1),
		"null expected version":    strings.Replace(validAction, `"expected_version":0`, `"expected_version":null`, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			service := &fakeLearning{}
			handler := newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
			response := learningRequest(t, handler, http.MethodPost, "/v1/tutoring/sessions/"+testAggregateID+"/actions", body)
			if response.Code != http.StatusBadRequest || service.calls != 0 {
				t.Fatalf("strict decode=%d calls=%d body=%s", response.Code, service.calls, response.Body.String())
			}
		})
	}

	closedWrites := []struct{ path, body string }{
		{"/v1/learning/goals", strings.TrimSuffix(bodies["goal"], "}") + `,"actor_device_id":"` + testRelatedID + `"}`},
		{"/v1/tutoring/sessions", strings.Replace(bodies["session"], `,"expected_version":0`, "", 1)},
		{"/v1/tutoring/proposals", strings.TrimSuffix(bodies["proposal"], "}") + `,"unknown":true}`},
		{"/v1/learning/assessments/" + testAssessment + "/decisions", strings.TrimSuffix(bodies["decision"], "}") + `,"reason":"unrelated"}`},
	}
	for _, item := range closedWrites {
		var logs bytes.Buffer
		service := &fakeLearning{}
		handler := newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
		response := learningRequest(t, handler, http.MethodPost, item.path, item.body)
		if response.Code != http.StatusBadRequest || service.calls != 0 {
			t.Fatalf("closed write %s=%d calls=%d body=%s", item.path, response.Code, service.calls, response.Body.String())
		}
	}
}

func TestLearningHTTPRejectsOptionalNullsBeforeService(t *testing.T) {
	bodies := validLearningBodies()
	exposure := `{` + strings.TrimPrefix(bodies["action"], "{")
	exposure = strings.Replace(exposure, `"start_diagnostic"`, `"record_exposure"`, 1)
	exposure = strings.TrimSuffix(exposure, "}") + `,"exposure_kind":"explanation","exposure_text":"explanation"}`
	exposureWithReference := strings.TrimSuffix(exposure, "}") + `,"knowledge_references":[{"node_revision_id":"` + testNodeID + `"}]}`
	decisionOverride := `{` + strings.TrimPrefix(bodies["decision"], "{")
	decisionOverride = strings.Replace(decisionOverride, `"confirm"`, `"override"`, 1)
	decisionOverride = strings.TrimSuffix(decisionOverride, `,"expected_disposition_version":1}`) + `,"expected_disposition_version":1,"reason":"review","items":[` + validAssessmentItemJSON() + `]}`

	cases := []struct {
		name, path, absent, explicitNull string
	}{
		{"goal occurred_at", "/v1/learning/goals", bodies["goal"], strings.TrimSuffix(bodies["goal"], "}") + `,"occurred_at":null}`},
		{"goal goal_id", "/v1/learning/goals", bodies["goal"], strings.TrimSuffix(bodies["goal"], "}") + `,"goal_id":null}`},
		{"proposal context", "/v1/tutoring/proposals", bodies["proposal"], strings.TrimSuffix(bodies["proposal"], "}") + `,"route_revision_id":null}`},
		{"action occurred_at", "/v1/tutoring/sessions/" + testAggregateID + "/actions", bodies["action"], strings.TrimSuffix(bodies["action"], "}") + `,"occurred_at":null}`},
		{"record assessment proposal", "/v1/tutoring/sessions/" + testAggregateID + "/actions", strings.Replace(bodies["action"], `"start_diagnostic"`, `"record_assessment"`, 1), strings.Replace(bodies["action"], `"start_diagnostic"`, `"record_assessment","proposal_id":null`, 1)},
		{"record exposure optional field", "/v1/tutoring/sessions/" + testAggregateID + "/actions", exposure, strings.TrimSuffix(exposure, "}") + `,"exposure_kind":null}`},
		{"record exposure references", "/v1/tutoring/sessions/" + testAggregateID + "/actions", exposureWithReference, strings.TrimSuffix(exposureWithReference, "}") + `,"knowledge_references":null}`},
		{"reference optional field", "/v1/tutoring/sessions/" + testAggregateID + "/actions", exposureWithReference, strings.Replace(exposureWithReference, `"node_revision_id":"`+testNodeID+`"`, `"node_revision_id":"`+testNodeID+`","knowledge_revision_id":null`, 1)},
		{"decision occurred_at", "/v1/learning/assessments/" + testAssessment + "/decisions", bodies["decision"], strings.TrimSuffix(bodies["decision"], "}") + `,"occurred_at":null}`},
		{"assessment item optional field", "/v1/learning/assessments/" + testAssessment + "/decisions", decisionOverride, strings.Replace(decisionOverride, `"knowledge_quote_sha256":"`+strings.Repeat("a", 64)+`"}]`, `"knowledge_quote_sha256":"`+strings.Repeat("a", 64)+`","misconception_candidate":null}]`, 1)},
	}
	for _, item := range cases {
		t.Run(item.name+" absent", func(t *testing.T) {
			var logs bytes.Buffer
			service := &fakeLearning{}
			handler := newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
			response := learningRequest(t, handler, http.MethodPost, item.path, item.absent)
			if response.Code != http.StatusCreated || service.calls != 1 {
				t.Fatalf("absent optional field=%d calls=%d body=%s", response.Code, service.calls, response.Body.String())
			}
		})
		t.Run(item.name+" null", func(t *testing.T) {
			var logs bytes.Buffer
			service := &fakeLearning{}
			handler := newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
			response := learningRequest(t, handler, http.MethodPost, item.path, item.explicitNull)
			if response.Code != http.StatusBadRequest || service.calls != 0 {
				t.Fatalf("explicit null=%d calls=%d body=%s", response.Code, service.calls, response.Body.String())
			}
		})
	}
}

func TestLearningHTTPRecordExposureKindMatrix(t *testing.T) {
	bodies := validLearningBodies()
	base := strings.TrimSuffix(strings.Replace(bodies["action"], `"start_diagnostic"`, `"record_exposure"`, 1), "}")
	direct := func(kind string, includeKind bool) string {
		body := base
		if includeKind {
			body += `,"exposure_kind":"` + kind + `"`
		}
		return body + `,"exposure_text":"explanation"}`
	}
	proposal := func(kind string, includeKind bool) string {
		body := base + `,"proposal_id":"` + testRelatedID + `"`
		if includeKind {
			body += `,"exposure_kind":"` + kind + `"`
		}
		return body + `}`
	}
	valid := []struct {
		name, body, kind string
	}{
		{"direct reading", direct("reading", true), "reading"},
		{"direct explanation", direct("explanation", true), "explanation"},
		{"proposal omitted", proposal("", false), "explanation"},
		{"proposal reading", proposal("reading", true), "reading"},
		{"proposal explanation", proposal("explanation", true), "explanation"},
	}
	for _, item := range valid {
		t.Run(item.name, func(t *testing.T) {
			var logs bytes.Buffer
			service := &fakeLearning{}
			handler := newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
			response := learningRequest(t, handler, http.MethodPost, "/v1/tutoring/sessions/"+testAggregateID+"/actions", item.body)
			if response.Code != http.StatusCreated || service.calls != 1 || service.action.ExposureKind != item.kind {
				t.Fatalf("valid exposure kind=%q status=%d calls=%d command=%+v body=%s", item.kind, response.Code, service.calls, service.action, response.Body.String())
			}
		})
	}
	invalid := []struct {
		name, body string
	}{
		{"direct missing", direct("", false)},
		{"direct empty", direct("", true)},
		{"direct unknown", direct("video", true)},
		{"proposal empty", proposal("", true)},
		{"proposal unknown", proposal("video", true)},
	}
	for _, item := range invalid {
		t.Run(item.name, func(t *testing.T) {
			var logs bytes.Buffer
			service := &fakeLearning{}
			handler := newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
			response := learningRequest(t, handler, http.MethodPost, "/v1/tutoring/sessions/"+testAggregateID+"/actions", item.body)
			if response.Code != http.StatusBadRequest || service.calls != 0 {
				t.Fatalf("invalid exposure status=%d calls=%d body=%s", response.Code, service.calls, response.Body.String())
			}
		})
	}
}

func TestLearningHTTPRejectsIdentifiersSourceAndQueriesBeforeService(t *testing.T) {
	bodies := validLearningBodies()
	tooLongSource := strings.Repeat("é", 101)
	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/learning/goals", strings.Replace(bodies["goal"], testOperationID, "not-a-uuid", 1)},
		{http.MethodPost, "/v1/learning/goals", strings.Replace(bodies["goal"], `"source":"device"`, `"source":"`+tooLongSource+`"`, 1)},
		{http.MethodPost, "/v1/tutoring/sessions", strings.Replace(bodies["session"], testRelatedID, "not-a-uuid", 1)},
		{http.MethodPost, "/v1/tutoring/proposals", strings.Replace(bodies["proposal"], testNodeID, "not-a-uuid", 1)},
		{http.MethodPost, "/v1/tutoring/proposals", strings.Replace(bodies["proposal"], `"proposal_type":"route"`, `"proposal_type":"forged"`, 1)},
		{http.MethodPost, "/v1/tutoring/proposals", strings.Replace(bodies["proposal"], `"input":{}`, `"input":[]`, 1)},
		{http.MethodPost, "/v1/tutoring/sessions/not-a-uuid/actions", bodies["action"]},
		{http.MethodPost, "/v1/learning/assessments/not-a-uuid/decisions", bodies["decision"]},
		{http.MethodGet, "/v1/tutoring/sessions/not-a-uuid", ""},
		{http.MethodGet, "/v1/learning/nodes/not-a-uuid", ""},
		{http.MethodGet, "/v1/learning/timeline?session_id=not-a-uuid", ""},
		{http.MethodGet, "/v1/learning/evidence?node_revision_id=not-a-uuid", ""},
		{http.MethodGet, "/v1/learning/reviews?due_before=not-a-time", ""},
		{http.MethodGet, "/v1/learning/routes?limit=0", ""},
		{http.MethodGet, "/v1/learning/routes?limit=201", ""},
		{http.MethodGet, "/v1/learning/routes?limit=50&limit=51", ""},
		{http.MethodGet, "/v1/learning/routes?unknown=true", ""},
	}
	for _, item := range cases {
		var logs bytes.Buffer
		service := &fakeLearning{}
		handler := newLearningTestAPI(t, []string{"learning:read", "learning:write"}, service, &logs)
		response := learningRequest(t, handler, item.method, item.path, item.body)
		if response.Code != http.StatusBadRequest || service.calls != 0 {
			t.Fatalf("pre-service validation %s %s=%d calls=%d body=%s", item.method, item.path, response.Code, service.calls, response.Body.String())
		}
	}

	var logs bytes.Buffer
	service := &fakeLearning{}
	handler := newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
	boundary := strings.Replace(bodies["goal"], `"source":"device"`, `"source":"`+strings.Repeat("x", learning.MaxGoalSourceBytes)+`"`, 1)
	response := learningRequest(t, handler, http.MethodPost, "/v1/learning/goals", boundary)
	if response.Code != http.StatusCreated || service.calls != 1 {
		t.Fatalf("200-byte source=%d calls=%d body=%s", response.Code, service.calls, response.Body.String())
	}
}

func TestLearningHTTPRejectsNonCanonicalUUIDsBeforeService(t *testing.T) {
	canonical := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	variants := []string{
		"aaaaaaaaaaaa4aaa8aaaaaaaaaaaaaaa",
		"{aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa}",
		"urn:uuid:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA",
	}
	bodies := validLearningBodies()
	locations := []struct {
		name   string
		method string
		path   func(string) string
		body   func(string) string
	}{
		{
			name:   "body",
			method: http.MethodPost,
			path:   func(string) string { return "/v1/learning/goals" },
			body: func(value string) string {
				return strings.Replace(bodies["goal"], testOperationID, value, 1)
			},
		},
		{
			name:   "path",
			method: http.MethodGet,
			path:   func(value string) string { return "/v1/tutoring/sessions/" + value },
			body:   func(string) string { return "" },
		},
		{
			name:   "query",
			method: http.MethodGet,
			path:   func(value string) string { return "/v1/learning/timeline?session_id=" + value },
			body:   func(string) string { return "" },
		},
	}
	if !validLearningUUID(canonical) {
		t.Fatalf("canonical UUID rejected: %s", canonical)
	}
	if validLearningUUID(" " + canonical + " ") {
		t.Fatalf("UUID helper accepted surrounding whitespace")
	}
	for _, variant := range variants {
		if validLearningUUID(variant) {
			t.Errorf("non-canonical UUID accepted by helper: %s", variant)
		}
		for _, location := range locations {
			t.Run(location.name+"/"+variant, func(t *testing.T) {
				var logs bytes.Buffer
				service := &fakeLearning{}
				handler := newLearningTestAPI(t, []string{"learning:read", "learning:write"}, service, &logs)
				response := learningRequest(t, handler, location.method, location.path(variant), location.body(variant))
				if response.Code != http.StatusBadRequest || service.calls != 0 {
					t.Fatalf("UUID %q at %s = %d calls=%d body=%s", variant, location.name, response.Code, service.calls, response.Body.String())
				}
			})
		}
	}
}

func TestLearningHTTPRoutesStrictCurrentOnly(t *testing.T) {
	var logs bytes.Buffer
	for _, test := range []struct {
		query string
		want  bool
	}{
		{"", false},
		{"?current_only=false", false},
		{"?current_only=true", true},
	} {
		service := &fakeLearning{}
		handler := newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
		response := learningRequest(t, handler, http.MethodGet, "/v1/learning/routes"+test.query, "")
		if response.Code != http.StatusOK || service.calls != 1 || service.page.CurrentOnly != test.want {
			t.Fatalf("query=%q status=%d calls=%d page=%+v body=%s", test.query, response.Code, service.calls, service.page, response.Body.String())
		}
	}
	for _, query := range []string{"?current_only=TRUE", "?current_only=1", "?current_only=", "?current_only=true&current_only=false"} {
		service := &fakeLearning{}
		handler := newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
		response := learningRequest(t, handler, http.MethodGet, "/v1/learning/routes"+query, "")
		if response.Code != http.StatusBadRequest || service.calls != 0 {
			t.Fatalf("invalid query=%q status=%d calls=%d body=%s", query, response.Code, service.calls, response.Body.String())
		}
	}
}

func TestLearningHTTPStrictRawQueryMatrix(t *testing.T) {
	cases := []string{
		"/v1/learning/routes?cursor=%zz",
		"/v1/learning/routes?cursor=first;limit=50",
		"/v1/learning/routes?cursor=first&cursor=second",
		"/v1/learning/routes?unknown=value",
		"/v1/learning/routes?limit=",
		"/v1/learning/timeline?session_id=",
		"/v1/learning/timeline?session_id=%20" + testAggregateID + "%20",
		"/v1/learning/evidence?node_revision_id=",
		"/v1/learning/reviews?due_before=",
	}
	for _, path := range cases {
		var logs bytes.Buffer
		service := &fakeLearning{}
		handler := newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
		response := learningRequest(t, handler, http.MethodGet, path, "")
		if response.Code != http.StatusBadRequest || service.calls != 0 {
			t.Fatalf("strict query %s = %d calls=%d body=%s", path, response.Code, service.calls, response.Body.String())
		}
	}

	var logs bytes.Buffer
	service := &fakeLearning{}
	handler := newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
	response := learningRequest(t, handler, http.MethodGet, "/v1/learning/routes?", "")
	if response.Code != http.StatusOK || service.calls != 1 {
		t.Fatalf("empty query = %d calls=%d body=%s", response.Code, service.calls, response.Body.String())
	}
}

func TestLearningHTTPValidatesSHA256AndCollectionLimitsBeforeService(t *testing.T) {
	bodies := validLearningBodies()
	baseAction := strings.Replace(bodies["action"], `"start_diagnostic"`, `"record_exposure"`, 1)
	reference := `{"node_revision_id":"` + testNodeID + `"}`
	directExposure := func(referenceJSON string) string {
		return strings.TrimSuffix(baseAction, "}") + `,"exposure_kind":"explanation","exposure_text":"explanation","knowledge_references":[` + referenceJSON + `]}`
	}
	validSHA := strings.Repeat("a", 64)

	validReferences := []string{
		reference,
		strings.TrimSuffix(reference, "}") + `,"slice_sha256":"` + validSHA + `"}`,
	}
	for _, validReference := range validReferences {
		var logs bytes.Buffer
		service := &fakeLearning{}
		handler := newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
		response := learningRequest(t, handler, http.MethodPost, "/v1/tutoring/sessions/"+testAggregateID+"/actions", directExposure(validReference))
		if response.Code != http.StatusCreated || service.calls != 1 {
			t.Fatalf("valid optional SHA reference = %d calls=%d body=%s", response.Code, service.calls, response.Body.String())
		}
	}
	for _, invalidSHA := range []string{"", strings.ToUpper(validSHA), strings.Repeat("a", 63), strings.Repeat("g", 64)} {
		var logs bytes.Buffer
		service := &fakeLearning{}
		handler := newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
		invalidReference := strings.TrimSuffix(reference, "}") + `,"slice_sha256":"` + invalidSHA + `"}`
		response := learningRequest(t, handler, http.MethodPost, "/v1/tutoring/sessions/"+testAggregateID+"/actions", directExposure(invalidReference))
		if response.Code != http.StatusBadRequest || service.calls != 0 {
			t.Fatalf("invalid optional SHA %q = %d calls=%d body=%s", invalidSHA, response.Code, service.calls, response.Body.String())
		}
	}

	decisionBase := strings.Replace(bodies["decision"], `"confirm"`, `"override"`, 1)
	override := func(items string) string {
		return strings.TrimSuffix(decisionBase, `,"expected_disposition_version":1}`) + `,"expected_disposition_version":1,"reason":"review","items":[` + items + `]}`
	}
	for _, field := range []string{"answer_quote_sha256", "knowledge_quote_sha256"} {
		invalidItem := strings.Replace(validAssessmentItemJSON(), `"`+field+`":"`+validSHA+`"`, `"`+field+`":""`, 1)
		var logs bytes.Buffer
		service := &fakeLearning{}
		handler := newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
		response := learningRequest(t, handler, http.MethodPost, "/v1/learning/assessments/"+testAssessment+"/decisions", override(invalidItem))
		if response.Code != http.StatusBadRequest || service.calls != 0 {
			t.Fatalf("invalid %s = %d calls=%d body=%s", field, response.Code, service.calls, response.Body.String())
		}
	}

	for count, wantStatus := range map[int]int{learning.MaxRubricItems: http.StatusCreated, learning.MaxRubricItems + 1: http.StatusBadRequest} {
		items := strings.TrimSuffix(strings.Repeat(validAssessmentItemJSON()+",", count), ",")
		var logs bytes.Buffer
		service := &fakeLearning{}
		handler := newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
		response := learningRequest(t, handler, http.MethodPost, "/v1/learning/assessments/"+testAssessment+"/decisions", override(items))
		wantCalls := 1
		if wantStatus != http.StatusCreated {
			wantCalls = 0
		}
		if response.Code != wantStatus || service.calls != wantCalls {
			t.Fatalf("override items=%d = %d calls=%d body=%s", count, response.Code, service.calls, response.Body.String())
		}
	}

	for count, wantStatus := range map[int]int{100: http.StatusCreated, 101: http.StatusBadRequest} {
		references := strings.TrimSuffix(strings.Repeat(reference+",", count), ",")
		var logs bytes.Buffer
		service := &fakeLearning{}
		handler := newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
		response := learningRequest(t, handler, http.MethodPost, "/v1/tutoring/sessions/"+testAggregateID+"/actions", directExposure(references))
		wantCalls := 1
		if wantStatus != http.StatusCreated {
			wantCalls = 0
		}
		if response.Code != wantStatus || service.calls != wantCalls {
			t.Fatalf("knowledge references=%d = %d calls=%d body=%s", count, response.Code, service.calls, response.Body.String())
		}
	}
}

func TestLearningHTTPPaginationErrorsAndNilModelSemantics(t *testing.T) {
	var logs bytes.Buffer
	service := &fakeLearning{}
	handler := newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
	response := learningRequest(t, handler, http.MethodGet, "/v1/learning/timeline?cursor=opaque&session_id="+testAggregateID, "")
	if response.Code != http.StatusOK || service.timeline.Page.Limit != 50 || service.timeline.Page.Cursor != "opaque" || service.timeline.SessionID != testAggregateID {
		t.Fatalf("default pagination=%d query=%+v", response.Code, service.timeline)
	}

	bodies := validLearningBodies()
	service = &fakeLearning{err: &learning.Error{Code: learning.CodeModelUnavailable, Reason: "not_configured"}}
	handler = newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
	for attempt := 0; attempt < 2; attempt++ {
		response = learningRequest(t, handler, http.MethodPost, "/v1/tutoring/proposals", bodies["proposal"])
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), learning.CodeModelUnavailable) {
			t.Fatalf("nil-model proposal attempt %d=%d %s", attempt, response.Code, response.Body.String())
		}
	}

	service = &fakeLearning{}
	handler = newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
	objective := strings.Replace(bodies["action"], `"start_diagnostic"`, `"record_assessment"`, 1)
	response = learningRequest(t, handler, http.MethodPost, "/v1/tutoring/sessions/"+testAggregateID+"/actions", objective)
	if response.Code != http.StatusCreated || service.action.ProposalID != "" {
		t.Fatalf("objective no-proposal action=%d command=%+v body=%s", response.Code, service.action, response.Body.String())
	}
}

func TestLearningHTTPStableDomainErrorPayloads(t *testing.T) {
	var logs bytes.Buffer
	bodies := validLearningBodies()
	conflict := &learning.Error{Code: learning.CodeVersionConflict, AggregateType: "session", AggregateID: testAggregateID, ExpectedVersion: 2, CurrentVersion: 3, AsOfEventSequence: 9, Cause: errors.New("private model output")}
	service := &fakeLearning{err: conflict}
	handler := newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
	response := learningRequest(t, handler, http.MethodPost, "/v1/tutoring/sessions/"+testAggregateID+"/actions", bodies["action"])
	body := response.Body.String()
	if response.Code != http.StatusConflict || !strings.Contains(body, `"request_id":`) || !strings.Contains(body, `"current_version":3`) || strings.Contains(body, "private model output") || strings.Contains(logs.String(), "private model output") {
		t.Fatalf("conflict response=%d body=%s logs=%s", response.Code, body, logs.String())
	}

	for code, status := range map[string]int{
		learning.CodeNotFound:                  http.StatusNotFound,
		learning.CodeKnowledgeReferenceInvalid: http.StatusUnprocessableEntity,
		learning.CodeProjectionUnavailable:     http.StatusServiceUnavailable,
	} {
		service = &fakeLearning{err: &learning.Error{Code: code}}
		handler = newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
		response = learningRequest(t, handler, http.MethodGet, "/v1/tutoring/sessions/current", "")
		if response.Code != status || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("error %s=%d body=%s", code, response.Code, response.Body.String())
		}
	}
}

func TestA101LearningHTTPSessionNestedAndCompletedResponsesMatchOpenAPI(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	validate := func(path string, response *httptest.ResponseRecorder) {
		t.Helper()
		var payload any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode session response: %v; body=%s", err, response.Body.String())
		}
		schema := document.Paths.Find(path).Get.Responses.Value("200").Value.Content.Get("application/json").Schema.Value
		if err := schema.VisitJSON(payload, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatalf("session response failed schema: %v; body=%s", err, response.Body.String())
		}
	}
	view := a101HTTPSessionViewFixture()
	var logs bytes.Buffer
	service := &fakeLearning{sessionView: view}
	handler := newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
	response := learningRequest(t, handler, http.MethodGet, "/v1/tutoring/sessions/current", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"free_question"`) || !strings.Contains(response.Body.String(), `"free_answer"`) || !strings.Contains(response.Body.String(), `"session_aggregate_version":6`) {
		t.Fatalf("nested session response=%d body=%s", response.Code, response.Body.String())
	}
	validate("/v1/tutoring/sessions/current", response)

	completed := view
	completed.Session.State = tutoring.StateCompleted
	completed.Session.ActiveFrame = nil
	completed.WorkItem = nil
	service = &fakeLearning{sessionView: completed}
	handler = newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
	response = learningRequest(t, handler, http.MethodGet, "/v1/tutoring/sessions/"+testAggregateID, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"work_item":null`) {
		t.Fatalf("completed session response=%d body=%s", response.Code, response.Body.String())
	}
	validate("/v1/tutoring/sessions/{sessionID}", response)
}

func TestA101LearningHTTPBarrierAndRedactedResponsesDropWorkItemContent(t *testing.T) {
	secret := "private free-answer body must not escape"
	view := a101HTTPSessionViewFixture()
	view.WorkItem.FreeAnswer.Text = secret
	manager := privacy.NewReadPermitManager()
	started := make(chan struct{})
	service := &fakeLearning{currentSessionFn: func(ctx context.Context) (learning.SessionView, error) {
		close(started)
		<-ctx.Done()
		return view, nil
	}}
	var logs bytes.Buffer
	handler := newLearningTestAPIWithPermits(t, []string{"learning:read"}, service, manager, &logs)
	responseCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseCh <- learningRequest(t, handler, http.MethodGet, "/v1/tutoring/sessions/current", "")
	}()
	<-started
	drainCh := make(chan error, 1)
	go func() {
		drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		drainCh <- manager.CloseAndDrain(drainCtx, 2, privacy.OwnerLearning, privacy.OwnerTutoring)
	}()
	response := <-responseCh
	if err := <-drainCh; err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), learning.CodeContentRedacted) || strings.Contains(response.Body.String(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatalf("barrier response=%d body=%s logs=%s", response.Code, response.Body.String(), logs.String())
	}

	logs.Reset()
	service = &fakeLearning{err: &learning.Error{Code: learning.CodeContentRedacted, Cause: errors.New(secret)}}
	handler = newLearningTestAPIWithPermits(t, []string{"learning:read"}, service, privacy.NewReadPermitManager(), &logs)
	response = learningRequest(t, handler, http.MethodGet, "/v1/tutoring/sessions/current", "")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), learning.CodeContentRedacted) || strings.Contains(response.Body.String(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatalf("redacted response=%d body=%s logs=%s", response.Code, response.Body.String(), logs.String())
	}
}

func a101HTTPSessionViewFixture() learning.SessionView {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	const (
		goalRevisionID      = "31000000-0000-4000-8000-000000000001"
		goalID              = "31000000-0000-4000-8000-000000000002"
		routeRevisionID     = "32000000-0000-4000-8000-000000000001"
		routeID             = "32000000-0000-4000-8000-000000000002"
		routeStepID         = "32000000-0000-4000-8000-000000000003"
		knowledgeRevisionID = "33000000-0000-4000-8000-000000000001"
		nodeID              = "33000000-0000-4000-8000-000000000002"
		nodeRevisionID      = "33000000-0000-4000-8000-000000000003"
		frameID             = "34000000-0000-4000-8000-000000000001"
		questionID          = "35000000-0000-4000-8000-000000000001"
		answerID            = "35000000-0000-4000-8000-000000000002"
		proposalID          = "36000000-0000-4000-8000-000000000001"
	)
	focus := tutoring.FocusContext{GoalRevisionID: goalRevisionID, RouteRevisionID: routeRevisionID, RouteStepID: routeStepID, KnowledgeRevisionID: knowledgeRevisionID, FocusNodeRevisionID: nodeRevisionID}
	frame := &tutoring.FocusFrame{ID: frameID, SessionID: testAggregateID, SavedState: tutoring.StateRouteActive, Context: focus, SavedAggregateVersion: 4, CreatedEventSequence: 12}
	goal := learning.GoalRevision{ID: goalRevisionID, GoalID: goalID, Revision: 1, Text: "Learn a topic", Source: "go-cli-m1", ActorDeviceID: testActorID, CreatedAt: now}
	route := learning.RouteRevision{ID: routeRevisionID, RouteID: routeID, Revision: 1, GoalRevisionID: goalRevisionID, KnowledgeRevisionID: knowledgeRevisionID, PolicyVersion: learning.RoutePolicyVersion, SourceProposalID: proposalID, Steps: []learning.RouteStep{{ID: routeStepID, Ordinal: 0, NodeID: nodeID, NodeRevisionID: nodeRevisionID, TeachingIntent: "teach", CompletionCondition: "pass"}}, CreatedAt: now}
	question := tutoring.FreeQuestion{ID: questionID, SessionID: testAggregateID, FocusFrameID: frameID, SessionAggregateVer: 6, Text: "Why?", KnowledgeRevisionID: knowledgeRevisionID, References: []tutoring.FrozenReference{}, ActorDeviceID: testActorID, ReceivedAt: now}
	answer := tutoring.FreeAnswer{ID: answerID, SessionID: testAggregateID, FocusFrameID: frameID, FreeQuestionID: questionID, Text: "Because.", KnowledgeRevisionID: knowledgeRevisionID, References: []tutoring.FrozenReference{}, SourceProposalID: proposalID, ReceivedAt: now}
	return learning.SessionView{
		Metadata: learning.ProjectionMetadata{AsOfEventSequence: 20, ProjectionVersion: learning.ProjectionVersion, MasteryReducerVersion: learning.MasteryReducerVersion, AssessmentPolicy: learning.AssessmentPolicyVersion, ReviewPolicy: learning.ReviewPolicyVersion, KnowledgeRevisionID: knowledgeRevisionID, GenerationID: testRelatedID, ReasonCodes: []string{}},
		Session:  tutoring.Session{ID: testAggregateID, State: tutoring.StateFreeAnswer, AggregateVer: 7, Context: focus, ActiveFrame: frame},
		Estimate: learning.ActiveTimeEstimate{Estimated: true, AlgorithmVersion: learning.ActiveTimePolicyVersion, SampleCount: 1},
		WorkItem: &learning.SessionWorkItem{AllowedActions: []tutoring.Action{tutoring.ActionAskFreeQuestion, tutoring.ActionConvertFreeAnswerToQuiz, tutoring.ActionResumeFocus, tutoring.ActionSwitchGoal}, AllowedAssessmentDecisions: []string{}, GoalRevision: &goal, RouteRevision: &route, FreeQuestion: &question, FreeAnswer: &answer},
	}
}

func TestLearningHTTPActualResponsesValidateOpenAPI(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	validate := func(path, method, code string, response *httptest.ResponseRecorder) {
		t.Helper()
		var payload any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode actual response: %v; body=%s", err, response.Body.String())
		}
		item := document.Paths.Find(path)
		var schema *openapi3.Schema
		if method == http.MethodGet {
			schema = item.Get.Responses.Value(code).Value.Content.Get("application/json").Schema.Value
		} else {
			schema = item.Post.Responses.Value(code).Value.Content.Get("application/json").Schema.Value
		}
		if err := schema.VisitJSON(payload, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatalf("actual %s %s response %s failed schema: %v; body=%s", method, path, code, err, response.Body.String())
		}
	}
	metadata := learning.ProjectionMetadata{AsOfEventSequence: 1, ProjectionVersion: learning.ProjectionVersion, MasteryReducerVersion: learning.MasteryReducerVersion, AssessmentPolicy: learning.AssessmentPolicyVersion, ReviewPolicy: learning.ReviewPolicyVersion, GenerationID: testRelatedID}
	var logs bytes.Buffer
	service := &fakeLearning{evidencePage: learning.EvidencePage{Metadata: metadata}}
	handler := newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
	response := learningRequest(t, handler, http.MethodGet, "/v1/learning/evidence", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"items":[]`) || !strings.Contains(response.Body.String(), `"reason_codes":[]`) {
		t.Fatalf("normalized empty read=%d body=%s", response.Code, response.Body.String())
	}
	validate("/v1/learning/evidence", http.MethodGet, "200", response)

	response = learningRequest(t, handler, http.MethodGet, "/v1/learning/evidence?limit=0", "")
	validate("/v1/learning/evidence", http.MethodGet, "400", response)

	service = &fakeLearning{err: &learning.Error{Code: learning.CodeVersionConflict, AggregateType: "session", AggregateID: testAggregateID, ExpectedVersion: 2, CurrentVersion: 3, AsOfEventSequence: 9}}
	handler = newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
	response = learningRequest(t, handler, http.MethodGet, "/v1/learning/evidence", "")
	validate("/v1/learning/evidence", http.MethodGet, "409", response)

	service = &fakeLearning{err: &learning.Error{Code: learning.CodeNotFound}}
	handler = newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
	response = learningRequest(t, handler, http.MethodGet, "/v1/tutoring/sessions/current", "")
	validate("/v1/tutoring/sessions/current", http.MethodGet, "404", response)

	bodies := validLearningBodies()
	for code, domainErr := range map[string]error{
		"422": &learning.Error{Code: learning.CodeProposalRejected},
		"503": &learning.Error{Code: learning.CodeModelUnavailable},
	} {
		service = &fakeLearning{err: domainErr}
		handler = newLearningTestAPI(t, []string{"learning:write"}, service, &logs)
		response = learningRequest(t, handler, http.MethodPost, "/v1/tutoring/proposals", bodies["proposal"])
		validate("/v1/tutoring/proposals", http.MethodPost, code, response)
	}

	service = &fakeLearning{err: errors.New("answer=request-hash=secret")}
	handler = newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
	response = learningRequest(t, handler, http.MethodGet, "/v1/tutoring/sessions/current", "")
	validate("/v1/tutoring/sessions/current", http.MethodGet, "500", response)
	if strings.Contains(response.Body.String(), "request-hash") || strings.Contains(logs.String(), "request-hash") {
		t.Fatalf("internal learning failure leaked sensitive data: response=%s logs=%s", response.Body.String(), logs.String())
	}
}
