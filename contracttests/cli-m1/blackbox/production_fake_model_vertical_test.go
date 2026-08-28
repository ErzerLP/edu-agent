package blackbox

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	productionModelProtocolProfile = "openai-chat-completions-v1"
	productionProposalSchema       = 1
	productionPromptRevision       = "tutor-proposal-v1"
)

type productionModelProfile struct {
	label   string
	modelID string
}

type productionModelCorpusSummary struct {
	ProtocolProfile          string
	ProposalSchemaVersion    int
	PromptRevision           string
	SchemaFailureCategory    string
	SchemaFailureAttempts    string
	LowConfidenceDisposition string
	LowConfidenceAttempts    string
	TimeoutAttempts          string
	RateLimitAttempts        string
	ExhaustedCategory        string
	ExhaustedAttempts        string
	LaterSuccessAttempts     string
}

type productionModelAuthoritySnapshot struct {
	AggregateVersion string
	State            string
	Attempts         string
	Assessments      string
	Decisions        string
	Evidence         string
	EventClock       string
}

type productionModelMasterySnapshot struct {
	ValidEvidence      int64
	PendingAssessments int64
}

type productionModelAssessmentObservation struct {
	ModelID           string
	ContextWindow     string
	PromptRevision    string
	Attempts          int
	AttemptCategories string
	ProposalInputHash string
}

type productionFakeAttempt struct {
	Scenario string
	Status   int
}

type fakeAuditEntry struct {
	Sequence        uint64       `json:"sequence"`
	Method          string       `json:"method"`
	Path            string       `json:"path"`
	ProtocolProfile string       `json:"protocol_profile,omitempty"`
	Model           string       `json:"model,omitempty"`
	RequestKind     string       `json:"request_kind,omitempty"`
	RequestID       string       `json:"request_id,omitempty"`
	ResponseFormat  string       `json:"response_format,omitempty"`
	Scenario        fakeScenario `json:"scenario"`
	Status          int          `json:"status"`
	RequestBytes    int          `json:"request_bytes"`
	RequestSHA256   string       `json:"request_sha256,omitempty"`
}

type productionFakeAuditObservation struct {
	ProtocolProfile string
	RequestID       string
	RequestSHA256   string
}

func TestBlackBoxProductionFakeModelVerticalPostgreSQL(t *testing.T) {
	if strings.TrimSpace(os.Getenv("TEST_DATABASE_URL")) == "" {
		t.Skip("TEST_DATABASE_URL is not set; production fake-model vertical requires real PostgreSQL and is not pass evidence")
	}

	profiles := []productionModelProfile{
		{label: "baseline", modelID: "operations-baseline-v1"},
		{label: "candidate", modelID: "operations-candidate-v2"},
	}
	if profiles[0].modelID == profiles[1].modelID {
		t.Fatal("production model profiles must have distinct versioned model IDs")
	}

	summaries := make([]productionModelCorpusSummary, len(profiles))
	for index, profile := range profiles {
		index, profile := index, profile
		t.Run(profile.label, func(t *testing.T) {
			h := newHarnessWithOptions(t, harnessOptions{
				offlineSigner: false,
				modelName:     profile.modelID,
				modelTimeout:  2 * time.Second,
			})
			h.pairBoth(h.serverURL)
			h.importFixture(h.primaryHome)
			summaries[index] = runProductionModelCorpus(t, h, profile)
		})
	}
	if summaries[0] != summaries[1] {
		t.Fatalf("baseline/candidate production model invariants differ: baseline=%+v candidate=%+v", summaries[0], summaries[1])
	}
}

func runProductionModelCorpus(t *testing.T, h *harness, profile productionModelProfile) productionModelCorpusSummary {
	t.Helper()
	h.configureFake("route", fakeScenario{Kind: "accepted", RouteStepLimit: 1})
	h.configureFake("explanation", fakeScenario{Kind: "accepted"})
	h.configureFake("activity", fakeScenario{Kind: "accepted", ActivityType: "open"})

	schemaProtocol, schemaFailure, exhaustionFailure, laterSuccess := runSchemaExhaustionAndLaterSuccess(t, h, profile)
	lowProtocol, lowAssessment := runLowConfidenceProvisional(t, h, profile)
	timeoutProtocol, timeoutAssessment := runTransientAssessmentRetry(t, h, profile,
		"timeout", fakeScenario{Kind: "timeout", DelayMillis: 3000}, 0)
	rateProtocol, rateAssessment := runTransientAssessmentRetry(t, h, profile,
		"rate_limit", fakeScenario{Kind: "rate_limited", RetryAfter: "0"}, http.StatusTooManyRequests)

	for label, protocol := range map[string]string{
		"schema/exhaustion": schemaProtocol,
		"low confidence":    lowProtocol,
		"timeout":           timeoutProtocol,
		"rate limit":        rateProtocol,
	} {
		if protocol != productionModelProtocolProfile {
			t.Fatalf("%s protocol_profile=%s want=%s", label, protocol, productionModelProtocolProfile)
		}
	}
	for label, observation := range map[string]productionModelAssessmentObservation{
		"later success":  laterSuccess,
		"low confidence": lowAssessment,
		"timeout":        timeoutAssessment,
		"rate limit":     rateAssessment,
	} {
		if observation.PromptRevision != productionPromptRevision {
			t.Fatalf("%s prompt_revision=%s want=%s", label, observation.PromptRevision, productionPromptRevision)
		}
	}

	return productionModelCorpusSummary{
		ProtocolProfile:       schemaProtocol,
		ProposalSchemaVersion: productionProposalSchema,
		PromptRevision:        laterSuccess.PromptRevision,
		SchemaFailureCategory: schemaFailure[0],
		SchemaFailureAttempts: schemaFailure[1],
		LowConfidenceDisposition: h.scalarString("low-confidence disposition summary", `
			SELECT decision.disposition
			FROM learning_assessment_decisions decision
			JOIN learning_assessments assessment ON assessment.id=decision.assessment_id
			WHERE assessment.session_id=(
				SELECT session_id FROM learning_assessments
				WHERE trusted_model_id=$1 AND confidence=800
				ORDER BY created_at DESC LIMIT 1)
			ORDER BY decision.version ASC LIMIT 1`, profile.modelID),
		LowConfidenceAttempts: lowAssessment.AttemptCategories,
		TimeoutAttempts:       timeoutAssessment.AttemptCategories,
		RateLimitAttempts:     rateAssessment.AttemptCategories,
		ExhaustedCategory:     exhaustionFailure[0],
		ExhaustedAttempts:     exhaustionFailure[1],
		LaterSuccessAttempts:  laterSuccess.AttemptCategories,
	}
}

func runSchemaExhaustionAndLaterSuccess(t *testing.T, h *harness, profile productionModelProfile) (string, [2]string, [2]string, productionModelAssessmentObservation) {
	t.Helper()
	h.configureFake("assessment", fakeScenario{Kind: "schema_mismatch"})
	sessionID, nodeID, masteryBefore := materializeProductionModelAssessment(t, h, "schema")

	cursor := h.fakeAuditCursor()
	failed := h.runCLI(h.primaryHome, newLearnInput().
		answer("fixed production model corpus response").
		defaultHelp().
		String(), "learn")
	if code := requireNonZero(t, failed, "schema mismatch assessment"); code != "proposal_rejected" {
		t.Fatalf("schema mismatch error_code=%s want=proposal_rejected", code)
	}
	schemaAudit := assertProductionFakeAttempts(t, h, cursor, profile, true,
		productionFakeAttempt{Scenario: "schema_mismatch", Status: http.StatusOK})
	assertSessionModelCounts(t, h, sessionID, "Evaluating", 1, 0, 0, 0)
	if masteryAfter := productionModelMastery(t, h, nodeID); masteryAfter != masteryBefore {
		t.Fatalf("schema mismatch changed mastery: before=%+v after=%+v", masteryBefore, masteryAfter)
	}
	schemaFailure := failedAssessmentProposal(t, h, sessionID, "schema_mismatch", "schema_mismatch")

	authorityBeforeExhaustion := productionModelAuthority(t, h, sessionID)
	h.configureFake("assessment",
		fakeScenario{Kind: "rate_limited", RetryAfter: "0"},
		fakeScenario{Kind: "rate_limited", RetryAfter: "0"},
		fakeScenario{Kind: "accepted"},
	)
	cursor = h.fakeAuditCursor()
	exhausted := h.runCLI(h.primaryHome, "", "learn")
	code := requireNonZero(t, exhausted, "rate-limit retry exhaustion")
	if code != "model_unavailable" && code != "dependency_unavailable" {
		t.Fatalf("rate-limit exhaustion error_code=%s", code)
	}
	exhaustedAudit := assertProductionFakeAttempts(t, h, cursor, profile, true,
		productionFakeAttempt{Scenario: "rate_limited", Status: http.StatusTooManyRequests},
		productionFakeAttempt{Scenario: "rate_limited", Status: http.StatusTooManyRequests})
	if authorityAfter := productionModelAuthority(t, h, sessionID); authorityAfter != authorityBeforeExhaustion {
		t.Fatalf("retry exhaustion changed authoritative learning state: before=%+v after=%+v", authorityBeforeExhaustion, authorityAfter)
	}
	if masteryAfter := productionModelMastery(t, h, nodeID); masteryAfter != masteryBefore {
		t.Fatalf("retry exhaustion changed mastery: before=%+v after=%+v", masteryBefore, masteryAfter)
	}
	exhaustionFailure := failedAssessmentProposal(t, h, sessionID, "rate_limited", "rate_limited,rate_limited")

	cursor = h.fakeAuditCursor()
	accepted := h.runCLI(h.primaryHome, newLearnInput().acknowledgeFeedback().String(), "learn")
	requireModelSuccess(t, accepted, "later accepted assessment")
	laterAudit := assertProductionFakeAttempts(t, h, cursor, profile, true,
		productionFakeAttempt{Scenario: "accepted", Status: http.StatusOK})
	if laterAudit.RequestID == exhaustedAudit.RequestID || laterAudit.RequestID == schemaAudit.RequestID {
		t.Fatal("later successful application attempt reused a failed proposal request ID")
	}
	assertSessionModelCounts(t, h, sessionID, "Completed", 1, 1, 1, 1)
	if masteryAfter := productionModelMastery(t, h, nodeID); masteryAfter.ValidEvidence != masteryBefore.ValidEvidence+1 || masteryAfter.PendingAssessments != masteryBefore.PendingAssessments {
		t.Fatalf("later success mastery=%+v before=%+v", masteryAfter, masteryBefore)
	}
	observation := assertProductionAssessmentPersistence(t, h, sessionID, profile, 1, "success")
	if got := h.scalarInt("single ready assessment proposal", `
		SELECT count(*) FROM tutoring_proposal_requests
		WHERE aggregate_id=$1 AND proposal_type='assessment' AND status='ready'`, sessionID); got != 1 {
		t.Fatalf("later success ready assessment proposals=%d want=1", got)
	}
	return schemaAudit.ProtocolProfile, schemaFailure, exhaustionFailure, observation
}

func runLowConfidenceProvisional(t *testing.T, h *harness, profile productionModelProfile) (string, productionModelAssessmentObservation) {
	t.Helper()
	h.configureFake("assessment", fakeScenario{Kind: "provisional"})
	sessionID, nodeID, masteryBefore := materializeProductionModelAssessment(t, h, "low_confidence")
	cursor := h.fakeAuditCursor()
	result := h.runCLI(h.primaryHome, newLearnInput().
		answer("fixed production model corpus response").
		defaultHelp().
		quit().
		String(), "learn")
	requireModelSuccess(t, result, "low-confidence assessment")
	audit := assertProductionFakeAttempts(t, h, cursor, profile, true,
		productionFakeAttempt{Scenario: "provisional", Status: http.StatusOK})
	assertSessionModelCounts(t, h, sessionID, "Feedback", 1, 1, 1, 0)
	if confidence := h.scalarInt("low-confidence persisted value", `SELECT confidence FROM learning_assessments WHERE session_id=$1`, sessionID); confidence != 800 {
		t.Fatalf("low-confidence assessment confidence=%d want=800", confidence)
	}
	if disposition := h.scalarString("low-confidence provisional decision", `
		SELECT decision.disposition
		FROM learning_assessment_decisions decision
		JOIN learning_assessments assessment ON assessment.id=decision.assessment_id
		WHERE assessment.session_id=$1 AND decision.version=1`, sessionID); disposition != "provisional" {
		t.Fatalf("low-confidence disposition=%s want=provisional", disposition)
	}
	masteryProvisional := productionModelMastery(t, h, nodeID)
	if masteryProvisional.ValidEvidence != masteryBefore.ValidEvidence || masteryProvisional.PendingAssessments != masteryBefore.PendingAssessments+1 {
		t.Fatalf("low-confidence mastery=%+v before=%+v", masteryProvisional, masteryBefore)
	}
	observation := assertProductionAssessmentPersistence(t, h, sessionID, profile, 1, "success")

	confirmed := h.runCLI(h.primaryHome, "", "assessment", "confirm")
	requireModelSuccess(t, confirmed, "explicit provisional confirmation")
	acknowledged := h.runCLI(h.primaryHome, newLearnInput().acknowledgeFeedback().String(), "learn")
	requireModelSuccess(t, acknowledged, "acknowledge confirmed assessment")
	assertSessionModelCounts(t, h, sessionID, "Completed", 1, 1, 2, 1)
	masteryConfirmed := productionModelMastery(t, h, nodeID)
	if masteryConfirmed.ValidEvidence != masteryBefore.ValidEvidence+1 || masteryConfirmed.PendingAssessments != masteryBefore.PendingAssessments {
		t.Fatalf("confirmed low-confidence mastery=%+v before=%+v", masteryConfirmed, masteryBefore)
	}
	return audit.ProtocolProfile, observation
}

func runTransientAssessmentRetry(t *testing.T, h *harness, profile productionModelProfile, label string, first fakeScenario, firstStatus int) (string, productionModelAssessmentObservation) {
	t.Helper()
	h.configureFake("assessment", first, fakeScenario{Kind: "accepted"})
	sessionID, nodeID, masteryBefore := materializeProductionModelAssessment(t, h, label)
	cursor := h.fakeAuditCursor()
	result := h.runCLI(h.primaryHome, newLearnInput().
		answer("fixed production model corpus response").
		defaultHelp().
		acknowledgeFeedback().
		String(), "learn")
	requireModelSuccess(t, result, label+" transient retry")
	orderedAudit := first.Kind != "timeout"
	audit := assertProductionFakeAttempts(t, h, cursor, profile, orderedAudit,
		productionFakeAttempt{Scenario: first.Kind, Status: firstStatus},
		productionFakeAttempt{Scenario: "accepted", Status: http.StatusOK})
	assertSessionModelCounts(t, h, sessionID, "Completed", 1, 1, 1, 1)
	masteryAfter := productionModelMastery(t, h, nodeID)
	if masteryAfter.ValidEvidence != masteryBefore.ValidEvidence+1 || masteryAfter.PendingAssessments != masteryBefore.PendingAssessments {
		t.Fatalf("%s mastery=%+v before=%+v", label, masteryAfter, masteryBefore)
	}
	observation := assertProductionAssessmentPersistence(t, h, sessionID, profile, 2, first.Kind+",success")
	return audit.ProtocolProfile, observation
}

func materializeProductionModelAssessment(t *testing.T, h *harness, label string) (string, string, productionModelMasterySnapshot) {
	t.Helper()
	sessionID := h.setGoal(h.primaryHome, "Verify the stable concept with production model vertical "+label)
	result := h.runCLI(h.primaryHome, standardTeachingInput().String(), "learn")
	if result.exit == 0 {
		t.Fatalf("%s setup unexpectedly completed before answer submission", label)
	}
	if state := h.scalarString(label+" setup session state", `SELECT state FROM tutoring_sessions WHERE id=$1`, sessionID); state != "AwaitingResponse" {
		code := stableErrorCode(result.stderr)
		if code == "" {
			code = "none"
		}
		t.Fatalf("%s setup state=%s want=AwaitingResponse exit=%d error_code=%s stdout=%q stderr=%q", label, state, result.exit, code, result.stdout, result.stderr)
	}
	nodeID := h.scalarString(label+" target node", `SELECT target_node_revision_id::text FROM learning_activities WHERE session_id=$1`, sessionID)
	return sessionID, nodeID, productionModelMastery(t, h, nodeID)
}

func (h *harness) fakeAudit() []fakeAuditEntry {
	h.t.Helper()
	var response struct {
		Audit []fakeAuditEntry `json:"audit"`
	}
	h.controlJSON(http.MethodGet, h.fakeURL+"/__fixture/audit", fakeControlKey, nil, http.StatusOK, &response)
	return response.Audit
}

func (h *harness) fakeAuditCursor() uint64 {
	h.t.Helper()
	audit := h.fakeAudit()
	if len(audit) == 0 {
		return 0
	}
	return audit[len(audit)-1].Sequence
}

func assertProductionFakeAttempts(t *testing.T, h *harness, after uint64, profile productionModelProfile, ordered bool, expected ...productionFakeAttempt) productionFakeAuditObservation {
	t.Helper()
	entries := make([]fakeAuditEntry, 0, len(expected))
	for _, entry := range h.fakeAudit() {
		if entry.Sequence > after && entry.RequestKind == "assessment" {
			entries = append(entries, entry)
		}
	}
	if len(entries) != len(expected) {
		t.Fatalf("fake assessment attempts=%d want=%d", len(entries), len(expected))
	}
	observation := productionFakeAuditObservation{}
	actualCounts := map[productionFakeAttempt]int{}
	expectedCounts := map[productionFakeAttempt]int{}
	for _, value := range expected {
		expectedCounts[value]++
	}
	for index, entry := range entries {
		if entry.Method != http.MethodPost || entry.Path != "/v1/chat/completions" ||
			entry.ProtocolProfile != productionModelProtocolProfile || entry.ResponseFormat != "json_schema" ||
			entry.Model != profile.modelID || entry.RequestID == "" || entry.RequestSHA256 == "" || entry.RequestBytes <= 0 {
			t.Fatalf("fake assessment audit metadata invalid: sequence=%d kind=%s status=%d", entry.Sequence, entry.Scenario.Kind, entry.Status)
		}
		if index == 0 {
			observation = productionFakeAuditObservation{ProtocolProfile: entry.ProtocolProfile, RequestID: entry.RequestID, RequestSHA256: entry.RequestSHA256}
		} else if entry.RequestID != observation.RequestID || entry.RequestSHA256 != observation.RequestSHA256 {
			t.Fatal("production retry changed request identity or canonical request hash")
		}
		actual := productionFakeAttempt{Scenario: entry.Scenario.Kind, Status: entry.Status}
		actualCounts[actual]++
		if ordered && actual != expected[index] {
			t.Fatalf("fake attempt %d=%+v want=%+v", index+1, actual, expected[index])
		}
	}
	if !ordered {
		for key, count := range expectedCounts {
			if actualCounts[key] != count {
				t.Fatalf("fake attempt category/status count for %+v=%d want=%d", key, actualCounts[key], count)
			}
		}
	}
	return observation
}

func failedAssessmentProposal(t *testing.T, h *harness, sessionID, category, categories string) [2]string {
	t.Helper()
	if got := h.scalarInt("failed assessment proposal count", `
		SELECT count(*) FROM tutoring_proposal_requests
		WHERE aggregate_id=$1 AND proposal_type='assessment' AND status='failed'
		  AND error_category=$2 AND array_to_string(attempt_categories,',')=$3`, sessionID, category, categories); got != 1 {
		t.Fatalf("failed assessment proposal category=%s count=%d want=1", category, got)
	}
	fields := h.queryFields("failed assessment proposal metadata", `
		SELECT error_category,array_to_string(attempt_categories,',')
		FROM tutoring_proposal_requests
		WHERE aggregate_id=$1 AND proposal_type='assessment' AND status='failed' AND error_category=$2`, 2, sessionID, category)
	return [2]string{fields[0], fields[1]}
}

func assertProductionAssessmentPersistence(t *testing.T, h *harness, sessionID string, profile productionModelProfile, attempts int, categories string) productionModelAssessmentObservation {
	t.Helper()
	fields := h.queryFields("production assessment provenance", `
		SELECT trusted_model_id,model_parameters->>'context_window',prompt_revision,
		       model_attempts::text,array_to_string(attempt_categories,','),encode(proposal_input_hash,'hex')
		FROM learning_assessments WHERE session_id=$1`, 6, sessionID)
	parsedAttempts, err := strconv.Atoi(fields[3])
	if err != nil {
		t.Fatal("production assessment attempts metadata is not an integer")
	}
	observation := productionModelAssessmentObservation{
		ModelID: fields[0], ContextWindow: fields[1], PromptRevision: fields[2],
		Attempts: parsedAttempts, AttemptCategories: fields[4], ProposalInputHash: fields[5],
	}
	if observation.ModelID != profile.modelID || observation.ContextWindow != "8192" ||
		observation.PromptRevision != productionPromptRevision || observation.Attempts != attempts ||
		observation.AttemptCategories != categories || len(observation.ProposalInputHash) != 64 {
		t.Fatalf("production assessment provenance metadata invalid: model=%s attempts=%d categories=%s", observation.ModelID, observation.Attempts, observation.AttemptCategories)
	}
	if !h.scalarBool("single frozen assessment proposal artifact", `
		SELECT count(*)=1 AND COALESCE(bool_and(
			schema_version=$2
			AND trusted_model_id=$3
			AND model_parameters->>'context_window'='8192'
			AND prompt_revision=$4
			AND array_to_string(attempt_categories,',')=$5
			AND artifact->>'schema_version'=$2::text
			AND artifact->>'model_id'=$3
			AND artifact->'model_parameters'->>'context_window'='8192'
			AND artifact->>'prompt_revision'=$4
			AND artifact->>'input_hash'=encode(input_hash,'hex')
			AND artifact->'assessment'->>'model_id'=$3
			AND artifact->'assessment'->'model_parameters'->>'context_window'='8192'
			AND artifact->'assessment'->>'prompt_revision'=$4
			AND artifact->'assessment'->>'proposal_input_hash'=encode(input_hash,'hex')
			AND (artifact->'assessment'->>'attempts')::int=$6
			AND artifact->'assessment'->'attempt_categories'=to_jsonb(attempt_categories)
		),FALSE)
		FROM tutoring_proposal_artifacts
		WHERE aggregate_id=$1 AND proposal_type='assessment'`, sessionID, productionProposalSchema, profile.modelID, productionPromptRevision, categories, attempts) {
		t.Fatal("assessment proposal artifact was not frozen exactly once with production provenance")
	}
	return observation
}

func assertSessionModelCounts(t *testing.T, h *harness, sessionID, state string, attempts, assessments, decisions, evidence int64) {
	t.Helper()
	if got := h.scalarString("production model session state", `SELECT state FROM tutoring_sessions WHERE id=$1`, sessionID); got != state {
		t.Fatalf("production model session state=%s want=%s", got, state)
	}
	assertSessionCount(t, h, "production model attempts", `SELECT count(*) FROM learning_attempts WHERE session_id=$1`, sessionID, attempts)
	assertSessionCount(t, h, "production model assessments", `SELECT count(*) FROM learning_assessments WHERE session_id=$1`, sessionID, assessments)
	assertSessionCount(t, h, "production model decisions", `
		SELECT count(*) FROM learning_assessment_decisions decision
		JOIN learning_assessments assessment ON assessment.id=decision.assessment_id
		WHERE assessment.session_id=$1`, sessionID, decisions)
	assertSessionCount(t, h, "production model evidence", `SELECT count(*) FROM learning_evidence WHERE session_id=$1`, sessionID, evidence)
}

func productionModelAuthority(t *testing.T, h *harness, sessionID string) productionModelAuthoritySnapshot {
	t.Helper()
	fields := h.queryFields("authoritative production model snapshot", `
		SELECT session.aggregate_version::text,session.state,
		       (SELECT count(*)::text FROM learning_attempts WHERE session_id=session.id),
		       (SELECT count(*)::text FROM learning_assessments WHERE session_id=session.id),
		       (SELECT count(*)::text FROM learning_assessment_decisions decision JOIN learning_assessments assessment ON assessment.id=decision.assessment_id WHERE assessment.session_id=session.id),
		       (SELECT count(*)::text FROM learning_evidence WHERE session_id=session.id),
		       (SELECT current_event_seq::text FROM learning_event_clock WHERE singleton_id=1)
		FROM tutoring_sessions session WHERE session.id=$1`, 7, sessionID)
	return productionModelAuthoritySnapshot{
		AggregateVersion: fields[0], State: fields[1], Attempts: fields[2], Assessments: fields[3],
		Decisions: fields[4], Evidence: fields[5], EventClock: fields[6],
	}
}

func productionModelMastery(t *testing.T, h *harness, nodeID string) productionModelMasterySnapshot {
	t.Helper()
	fields := h.queryFields("production model mastery snapshot", `
		SELECT COALESCE((node.item->'mastery'->>'valid_evidence_count')::bigint,0)::text,
		       COALESCE((node.item->'mastery'->>'pending_assessments')::bigint,0)::text
		FROM learning_projection_head head
		LEFT JOIN learning_projection_nodes node
		  ON node.generation_id=head.active_generation_id AND node.node_revision_id=$1
		WHERE head.singleton_id=1`, 2, nodeID)
	validEvidence, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		t.Fatal("production model valid evidence metadata is not an integer")
	}
	pending, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatal("production model pending assessment metadata is not an integer")
	}
	return productionModelMasterySnapshot{ValidEvidence: validEvidence, PendingAssessments: pending}
}

func requireModelSuccess(t *testing.T, result commandResult, label string) {
	t.Helper()
	if result.exit != 0 {
		code := stableErrorCode(result.stderr)
		if code == "" {
			code = "none"
		}
		t.Fatalf("%s failed: exit=%d error_code=%s", label, result.exit, code)
	}
}
