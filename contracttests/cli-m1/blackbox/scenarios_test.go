package blackbox

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestBlackBoxAcceptedTeachingFlow(t *testing.T) {
	h := newHarness(t)
	h.pairBoth(h.serverURL)
	h.importFixture(h.primaryHome)
	sessionID := h.setGoal(h.primaryHome, "Understand the stable concept and its verification step")

	result := h.runCLI(h.primaryHome, standardTeachingInput().
		answer("accepted response").
		defaultHelp().
		acknowledgeFeedback().
		String(), "learn")
	requireExit(t, result, 0, "accepted teaching flow")
	for marker, label := range map[string]string{
		"Current: proposed route":           "route proposal",
		"Current: explanation (not scored)": "explanation exposure",
		"Question:":                         "activity",
		"disposition=accepted":              "accepted assessment",
		"completed":                         "completed session",
	} {
		requireContains(t, result.stdout, marker, label)
	}
	latestID, state := h.latestSession()
	if latestID != sessionID || state != "Completed" {
		t.Fatalf("accepted flow assertion failed: session state metadata")
	}
	assertSessionCount(t, h, "route revision", `SELECT count(*) FROM learning_route_revisions r JOIN learning_goal_revisions g ON g.id=r.goal_revision_id JOIN tutoring_sessions s ON s.goal_revision_id=g.id WHERE s.id=$1`, sessionID, 1)
	assertSessionCount(t, h, "explanation exposure", `SELECT count(*) FROM learning_exposures WHERE session_id=$1 AND exposure_kind='explanation'`, sessionID, 1)
	assertSessionCount(t, h, "activity", `SELECT count(*) FROM learning_activities WHERE session_id=$1`, sessionID, 1)
	assertSessionCount(t, h, "attempt", `SELECT count(*) FROM learning_attempts WHERE session_id=$1`, sessionID, 1)
	assertSessionCount(t, h, "assessment", `SELECT count(*) FROM learning_assessments WHERE session_id=$1`, sessionID, 1)
	assertSessionCount(t, h, "accepted evidence", `SELECT count(*) FROM learning_evidence WHERE session_id=$1`, sessionID, 1)

	counts := h.fakeAuditCounts()
	for _, kind := range []string{"route", "explanation", "activity", "assessment"} {
		if counts[kind] != 1 {
			t.Fatalf("strict fake assertion failed: kind=%s successful_calls=%d want=1", kind, counts[kind])
		}
	}
}

func TestBlackBoxMultilineAnswerAndNonDefaultHelp(t *testing.T) {
	h := newHarness(t)
	h.pairBoth(h.serverURL)
	h.importFixture(h.primaryHome)
	sessionID := h.setGoal(h.primaryHome, "Practice the stable concept with guided support")
	h.configureFake("activity", fakeScenario{Kind: "accepted", ActivityType: "open", AllowedHelp: []string{"hint", "scaffold"}})

	result := h.runCLI(h.primaryHome, standardTeachingInput().
		multilineAnswer("first private line", "second private line").
		selectHelp("hint").
		acknowledgeFeedback().
		String(), "learn")
	requireExit(t, result, 0, "multiline answer flow")
	requireContains(t, result.stdout, "Allowed help: hint,scaffold", "non-default help choices")
	requireContains(t, result.stderr, "a single . ends the block", "multiline terminator prompt")
	if !h.scalarBool("multiline answer and selected help", `
		SELECT count(*)=1
		FROM learning_attempts a
		JOIN learning_attempt_payloads p ON p.id=a.answer_payload_id
		WHERE a.session_id=$1 AND a.help_level='hint' AND position(chr(10) in p.answer_text)>0`, sessionID) {
		t.Fatalf("multiline assertion failed: answer shape or help metadata")
	}
	if state := h.scalarString("multiline session state", `SELECT state FROM tutoring_sessions WHERE id=$1`, sessionID); state != "Completed" {
		t.Fatalf("multiline assertion failed: session state=%s want=Completed", state)
	}
}

func TestBlackBoxProvisionalSurvivesExitAndSecondCLIConfirms(t *testing.T) {
	h := newHarness(t)
	h.pairBoth(h.serverURL)
	h.importFixture(h.primaryHome)
	sessionID := h.setGoal(h.primaryHome, "Confirm a provisional stable concept assessment")
	assessmentID := reachProvisionalFeedback(t, h, sessionID)

	result := h.runCLI(h.secondaryHome, "", "assessment", "confirm")
	requireExit(t, result, 0, "second CLI confirm")
	requireContains(t, result.stdout, "disposition=accepted", "confirmed assessment")
	assertSecondDeviceDecision(t, h, assessmentID, "accepted")
	assertSessionCount(t, h, "confirmed evidence", `SELECT count(*) FROM learning_evidence WHERE session_id=$1`, sessionID, 1)
}

func TestBlackBoxProvisionalSurvivesExitAndSecondCLIOverrides(t *testing.T) {
	h := newHarness(t)
	h.pairBoth(h.serverURL)
	h.importFixture(h.primaryHome)
	sessionID := h.setGoal(h.primaryHome, "Override a provisional stable concept assessment")
	assessmentID := reachProvisionalFeedback(t, h, sessionID)

	result := h.runCLI(h.secondaryHome, "second device correction\n\n\n", "assessment", "override")
	requireExit(t, result, 0, "second CLI override")
	requireContains(t, result.stdout, "disposition=overridden", "overridden assessment")
	assertSecondDeviceDecision(t, h, assessmentID, "overridden")
	assertSessionCount(t, h, "override evidence", `SELECT count(*) FROM learning_evidence WHERE session_id=$1`, sessionID, 1)
	if !h.scalarBool("override immutable source fields", `
		SELECT
			first.conclusions->0->>'answer_quote_sha256'=second.conclusions->0->>'answer_quote_sha256'
			AND first.conclusions->0->>'knowledge_quote_sha256'=second.conclusions->0->>'knowledge_quote_sha256'
			AND first.conclusions->0->>'knowledge_reference_id'=second.conclusions->0->>'knowledge_reference_id'
		FROM learning_assessment_decisions first
		JOIN learning_assessment_decisions second ON second.assessment_id=first.assessment_id AND second.version=2
		WHERE first.assessment_id=$1 AND first.version=1`, assessmentID) {
		t.Fatalf("override assertion failed: immutable source metadata changed")
	}
}

func TestBlackBoxFreeAnswerAttachedQuizFeedbackAndExplicitResume(t *testing.T) {
	h := newHarness(t)
	h.pairBoth(h.serverURL)
	h.importFixture(h.primaryHome)
	sessionID := h.setGoal(h.primaryHome, "Connect a free question to a scored quiz")

	paused := h.runCLI(h.primaryHome, newLearnInput().
		confirmRouteRetrieval().
		quit().
		String(), "learn")
	requireExit(t, paused, 0, "pause at route")
	if state := h.scalarString("route pause", `SELECT state FROM tutoring_sessions WHERE id=$1`, sessionID); state != "RouteActive" {
		t.Fatalf("focus assertion failed: initial state=%s want=RouteActive", state)
	}

	// RouteActive :ask -> FreeQuestion retrieval -> FreeAnswer :quiz -> attached ActivityIssued.
	quiz := h.runCLI(h.primaryHome, newLearnInput().
		ask("Why does verification follow the premise?").
		confirmFreeAnswerRetrieval().
		convertToQuiz().
		confirmAttachedQuizRetrieval().
		presentActivity().
		answer("attached quiz response").
		defaultHelp().
		acknowledgeFeedback().
		quit().
		String(), "learn")
	if quiz.exit != 0 {
		t.Fatalf("free answer attached quiz failed: exit=%d error_code=%s state=%s fake_calls=%v", quiz.exit, stableErrorCode(quiz.stderr), h.scalarString("free answer failure state", `SELECT state FROM tutoring_sessions WHERE id=$1`, sessionID), h.fakeAuditCounts())
	}
	requireContains(t, quiz.stdout, "Current: answer (not scored)", "free answer exposure")
	if state := h.scalarString("attached quiz feedback return", `SELECT state FROM tutoring_sessions WHERE id=$1`, sessionID); state != "FreeAnswer" {
		t.Fatalf("focus assertion failed: post-feedback state=%s want=FreeAnswer", state)
	}
	if !h.scalarBool("attached quiz exact question answer pair", `
		SELECT count(*)=1
		FROM learning_activities activity
		JOIN tutoring_free_questions question ON question.id=activity.attached_free_question_id
		JOIN tutoring_free_answers answer ON answer.id=activity.attached_free_answer_id
		WHERE activity.session_id=$1
		  AND answer.free_question_id=question.id
		  AND answer.focus_frame_id=question.focus_frame_id
		  AND activity.is_review=false`, sessionID) {
		t.Fatalf("focus assertion failed: attached quiz ownership metadata")
	}
	assertSessionCount(t, h, "attached quiz evidence", `SELECT count(*) FROM learning_evidence WHERE session_id=$1`, sessionID, 1)

	// Feedback acknowledgment returns to FreeAnswer; resume is a separate explicit action.
	resumed := h.runCLI(h.secondaryHome, newLearnInput().
		resumeFocus().
		quit().
		String(), "learn")
	requireExit(t, resumed, 0, "explicit resume")
	if state := h.scalarString("explicit resume state", `SELECT state FROM tutoring_sessions WHERE id=$1`, sessionID); state != "RouteActive" {
		t.Fatalf("focus assertion failed: resumed state=%s want=RouteActive", state)
	}
	assertSessionCount(t, h, "resumed focus frame", `SELECT count(*) FROM tutoring_focus_frames WHERE session_id=$1 AND resumed_at IS NOT NULL`, sessionID, 1)
	if got := h.scalarInt("second device focus resume event", `SELECT count(*) FROM learning_events WHERE aggregate_id=$1 AND event_type='FocusResumed' AND device_id=$2`, sessionID, h.secondDevice); got != 1 {
		t.Fatalf("focus assertion failed: second-device resume events=%d want=1", got)
	}
}

func TestBlackBoxDueReviewOnlyAcceptedEvidenceAdvances(t *testing.T) {
	h := newHarness(t)
	h.pairBoth(h.serverURL)
	h.importFixture(h.primaryHome)
	baselineSession := h.setGoal(h.primaryHome, "Create baseline evidence for the stable concept")
	baseline := h.runCLI(h.primaryHome, standardTeachingInput().
		answer("baseline response").
		defaultHelp().
		acknowledgeFeedback().
		String(), "learn")
	requireExit(t, baseline, 0, "baseline evidence")
	if state := h.scalarString("baseline completion", `SELECT state FROM tutoring_sessions WHERE id=$1`, baselineSession); state != "Completed" {
		t.Fatalf("review assertion failed: baseline state=%s", state)
	}
	nodeID, _, _ := h.ageCurrentReview()
	h.configureFake("assessment",
		fakeScenario{Kind: "provisional"},
		fakeScenario{Kind: "accepted"},
	)

	reviewSession := h.setGoal(h.primaryHome, "Review the stable concept when it is due")
	// Diagnostic route -> RouteActive due-review confirmation -> review ActivityIssued.
	presented := h.runCLI(h.primaryHome, dueReviewInput().
		quit().
		String(), "learn")
	requireExit(t, presented, 0, "present due review")
	stepAfterPresent, dueAfterPresent := reviewMetadata(t, h, nodeID)
	if stepAfterPresent != 0 || !dueAfterPresent.Before(time.Now().UTC()) {
		t.Fatalf("review assertion failed: presentation advanced schedule metadata")
	}
	assertSessionCount(t, h, "review presented event", `SELECT count(*) FROM learning_events WHERE aggregate_id=$1 AND event_type='ReviewPresented'`, reviewSession, 1)
	assertSessionCount(t, h, "presented review activity", `SELECT count(*) FROM learning_activities WHERE session_id=$1 AND is_review=true`, reviewSession, 1)
	assertGlobalCount(t, h, "evidence after presentation", `SELECT count(*) FROM learning_evidence`, 1)

	// Resume at review ActivityIssued, present it, then stop from provisional Feedback.
	provisional := h.runCLI(h.primaryHome, newLearnInput().
		presentActivity().
		answer("provisional review response").
		defaultHelp().
		quit().
		String(), "learn")
	requireExit(t, provisional, 0, "provisional review assessment")
	stepAfterProvisional, dueAfterProvisional := reviewMetadata(t, h, nodeID)
	if stepAfterProvisional != stepAfterPresent || !dueAfterProvisional.Equal(dueAfterPresent) {
		t.Fatalf("review assertion failed: provisional assessment advanced schedule")
	}
	if disposition := h.scalarString("review provisional disposition", `
		SELECT decision.disposition
		FROM learning_assessment_decisions decision
		JOIN learning_assessments assessment ON assessment.id=decision.assessment_id
		WHERE assessment.session_id=$1
		ORDER BY decision.version DESC LIMIT 1`, reviewSession); disposition != "provisional" {
		t.Fatalf("review assertion failed: disposition=%s want=provisional", disposition)
	}
	assertGlobalCount(t, h, "evidence after provisional", `SELECT count(*) FROM learning_evidence`, 1)

	voided := h.runCLI(h.secondaryHome, "review result excluded\n", "assessment", "void")
	requireExit(t, voided, 0, "void provisional review")
	stepAfterVoid, dueAfterVoid := reviewMetadata(t, h, nodeID)
	if stepAfterVoid != stepAfterPresent || !dueAfterVoid.Equal(dueAfterPresent) {
		t.Fatalf("review assertion failed: void decision advanced schedule")
	}
	assertGlobalCount(t, h, "evidence after void", `SELECT count(*) FROM learning_evidence`, 1)

	acknowledged := h.runCLI(h.secondaryHome, newLearnInput().
		acknowledgeFeedback().
		String(), "learn")
	requireExit(t, acknowledged, 0, "acknowledge voided review")
	acceptedSession := h.setGoal(h.secondaryHome, "Complete an accepted stable concept due review")
	// A due review skips explanation generation but still confirms review-activity retrieval.
	accepted := h.runCLI(h.secondaryHome, dueReviewInput().
		presentActivity().
		answer("accepted review response").
		defaultHelp().
		acknowledgeFeedback().
		String(), "learn")
	requireExit(t, accepted, 0, "accepted due review")
	acceptedStep, acceptedDue := reviewMetadata(t, h, nodeID)
	if acceptedStep != 1 || !acceptedDue.After(time.Now().UTC().Add(48*time.Hour)) {
		t.Fatalf("review assertion failed: accepted evidence did not advance schedule metadata")
	}
	assertSessionCount(t, h, "accepted review evidence", `SELECT count(*) FROM learning_evidence WHERE session_id=$1 AND evidence_kind='review_recall'`, acceptedSession, 1)
	assertGlobalCount(t, h, "total evidence after accepted review", `SELECT count(*) FROM learning_evidence`, 2)
}

func TestBlackBoxModelFailurePreservesAuthoritativeStateAndCanRetry(t *testing.T) {
	h := newHarness(t)
	h.pairBoth(h.serverURL)
	h.importFixture(h.primaryHome)
	sessionID := h.setGoal(h.primaryHome, "Retry a model-backed route without changing authority")
	h.configureFake("route", fakeScenario{Kind: "http_error", StatusCode: http.StatusServiceUnavailable, RouteStepLimit: 1})

	failed := h.runCLI(h.primaryHome, newLearnInput().
		confirmRouteRetrieval().
		String(), "learn")
	code := requireNonZero(t, failed, "route model failure")
	if code != "model_unavailable" && code != "dependency_unavailable" {
		t.Fatalf("model failure assertion failed: error_code=%s", code)
	}
	if state := h.scalarString("state after route model failure", `SELECT state FROM tutoring_sessions WHERE id=$1`, sessionID); state != "Diagnostic" {
		t.Fatalf("model failure assertion failed: state=%s want=Diagnostic", state)
	}
	assertSessionCount(t, h, "route revisions after model failure", `
		SELECT count(*) FROM learning_route_revisions route
		JOIN tutoring_sessions session ON session.goal_revision_id=route.goal_revision_id
		WHERE session.id=$1`, sessionID, 0)
	assertSessionCount(t, h, "activities after model failure", `SELECT count(*) FROM learning_activities WHERE session_id=$1`, sessionID, 0)

	h.configureFake("route", fakeScenario{Kind: "accepted", RouteStepLimit: 1})
	retried := h.runCLI(h.primaryHome, newLearnInput().
		confirmRouteRetrieval().
		quit().
		String(), "learn")
	requireExit(t, retried, 0, "route retry")
	if state := h.scalarString("state after route retry", `SELECT state FROM tutoring_sessions WHERE id=$1`, sessionID); state != "RouteActive" {
		t.Fatalf("model retry assertion failed: state=%s want=RouteActive", state)
	}
	assertSessionCount(t, h, "route revision after retry", `
		SELECT count(*) FROM learning_route_revisions route
		JOIN tutoring_sessions session ON session.goal_revision_id=route.goal_revision_id
		WHERE session.id=$1`, sessionID, 1)
}

func TestBlackBoxResponseLossReplaysSameBodyWithoutDuplicateAuthority(t *testing.T) {
	h := newHarness(t)
	proxyURL := h.startProxy()
	h.pairBoth(proxyURL)
	h.importFixture(h.primaryHome)
	sessionID := h.setGoal(h.primaryHome, "Replay an accepted stable concept assessment after response loss")
	h.configureFake("assessment", fakeScenario{Kind: "http_error", StatusCode: http.StatusServiceUnavailable})

	failed := h.runCLI(h.primaryHome, standardTeachingInput().
		answer("response loss candidate").
		defaultHelp().
		String(), "learn")
	failureCode := requireNonZero(t, failed, "prepare evaluating state")
	if state := h.scalarString("evaluating state before response loss", `SELECT state FROM tutoring_sessions WHERE id=$1`, sessionID); state != "Evaluating" {
		t.Fatalf("response-loss setup failed: state=%s want=Evaluating error_code=%s fake_calls=%v", state, failureCode, h.fakeAuditCounts())
	}
	assertSessionCount(t, h, "assessment before response loss", `SELECT count(*) FROM learning_assessments WHERE session_id=$1`, sessionID, 0)
	assertSessionCount(t, h, "evidence before response loss", `SELECT count(*) FROM learning_evidence WHERE session_id=$1`, sessionID, 0)

	h.configureFake("assessment", fakeScenario{Kind: "accepted"})
	actionPath := "/v1/tutoring/sessions/" + sessionID + "/actions"
	h.armCapture(actionPath)
	replayed := h.runCLI(h.primaryHome, newLearnInput().
		quit().
		String(), "learn")
	requireExit(t, replayed, 0, "assessment response-loss replay")
	requireContains(t, replayed.stdout, "disposition=accepted", "accepted replay result")

	rules := h.proxyRules()
	if len(rules) != 1 {
		t.Fatalf("response-loss assertion failed: bound rule count=%d want=1", len(rules))
	}
	rule := rules[0]
	if rule.Rule.Path != actionPath || rule.Rule.Method != http.MethodPost || rule.Calls != 2 || rule.UpstreamCalls != 2 || rule.Drops != 1 || rule.Rejections != 0 {
		t.Fatalf("response-loss assertion failed: rule metadata calls=%d upstream=%d drops=%d rejections=%d", rule.Calls, rule.UpstreamCalls, rule.Drops, rule.Rejections)
	}
	matchingAudit := make([]proxyAuditEntry, 0, 2)
	for _, entry := range h.proxyAudit() {
		if entry.OperationID == rule.Rule.OperationID && entry.Path == actionPath {
			matchingAudit = append(matchingAudit, entry)
		}
	}
	if len(matchingAudit) != 2 || !matchingAudit[0].Dropped || matchingAudit[1].Dropped || matchingAudit[0].RequestSHA256 == "" || matchingAudit[0].RequestSHA256 != matchingAudit[1].RequestSHA256 {
		t.Fatalf("response-loss assertion failed: replay audit metadata")
	}
	if got := h.scalarInt("single inbox result", `SELECT count(*) FROM learning_inbox WHERE operation_id=$1 AND terminal_status='succeeded'`, rule.Rule.OperationID); got != 1 {
		t.Fatalf("response-loss assertion failed: inbox results=%d want=1", got)
	}
	if !h.scalarBool("record assessment event batch", `
		SELECT count(*)=4
		   AND array_agg(event_type ORDER BY operation_ordinal)=ARRAY[
			'AssessmentRecorded','AssessmentAccepted','EvidenceAccepted','TutoringStateChanged'
		   ]::text[]
		   AND array_agg(operation_ordinal ORDER BY operation_ordinal)=ARRAY[0,1,2,3]::integer[]
		FROM learning_events WHERE operation_id=$1`, rule.Rule.OperationID) {
		t.Fatalf("response-loss assertion failed: record_assessment event batch metadata")
	}
	if !h.scalarBool("single evidence identity", `
		SELECT count(*)=1
		   AND count(*)=count(DISTINCT id)
		   AND count(*)=count(DISTINCT decision_id)
		   AND count(*)=count(DISTINCT assessment_id)
		FROM learning_evidence WHERE session_id=$1`, sessionID) {
		t.Fatalf("response-loss assertion failed: evidence identity metadata")
	}

	if status := h.sendDifferentReplayBody(actionPath, rule.Rule.OperationID); status != http.StatusConflict {
		t.Fatalf("response-loss assertion failed: different body status=%d want=409", status)
	}
	after := h.proxyRules()
	if len(after) != 1 || after[0].UpstreamCalls != 2 || after[0].Rejections != 1 || after[0].Calls != 3 {
		t.Fatalf("response-loss assertion failed: different body reached upstream metadata")
	}
	if got := h.scalarInt("inbox after different body", `SELECT count(*) FROM learning_inbox WHERE operation_id=$1`, rule.Rule.OperationID); got != 1 {
		t.Fatalf("response-loss assertion failed: inbox changed after rejected body")
	}
	assertSessionCount(t, h, "evidence after different body", `SELECT count(*) FROM learning_evidence WHERE session_id=$1`, sessionID, 1)
}

func reachProvisionalFeedback(t *testing.T, h *harness, sessionID string) string {
	t.Helper()
	h.configureFake("assessment", fakeScenario{Kind: "provisional"})
	result := h.runCLI(h.primaryHome, standardTeachingInput().
		answer("provisional response").
		defaultHelp().
		quit().
		String(), "learn")
	requireExit(t, result, 0, "provisional feedback and process exit")
	requireContains(t, result.stdout, "disposition=provisional", "provisional disposition")
	if state := h.scalarString("provisional feedback state", `SELECT state FROM tutoring_sessions WHERE id=$1`, sessionID); state != "Feedback" {
		t.Fatalf("provisional assertion failed: state=%s want=Feedback", state)
	}
	assertSessionCount(t, h, "evidence before provisional resolution", `SELECT count(*) FROM learning_evidence WHERE session_id=$1`, sessionID, 0)
	return h.scalarString("provisional assessment id", `SELECT id FROM learning_assessments WHERE session_id=$1`, sessionID)
}

func assertSecondDeviceDecision(t *testing.T, h *harness, assessmentID, disposition string) {
	t.Helper()
	if !h.scalarBool("second device assessment decision", `
		SELECT count(*)=1
		FROM learning_assessment_decisions
		WHERE assessment_id=$1 AND version=2 AND disposition=$2 AND actor_device_id=$3`, assessmentID, disposition, h.secondDevice) {
		t.Fatalf("provisional resolution assertion failed: second device decision metadata")
	}
}

func reviewMetadata(t *testing.T, h *harness, nodeID string) (int, time.Time) {
	t.Helper()
	fields := h.queryFields("review schedule metadata", `SELECT (item->>'step')::int,extract(epoch from due_at) FROM learning_projection_reviews WHERE node_revision_id=$1`, 2, nodeID)
	step, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("review assertion failed: step metadata")
	}
	return step, parseUnixTime(t, fields[1], "review due metadata")
}

func assertSessionCount(t *testing.T, h *harness, label, query, sessionID string, want int64) {
	t.Helper()
	if got := h.scalarInt(label, query, sessionID); got != want {
		t.Fatalf("database assertion failed: %s count=%d want=%d", label, got, want)
	}
}

func assertGlobalCount(t *testing.T, h *harness, label, query string, want int64) {
	t.Helper()
	if got := h.scalarInt(label, query); got != want {
		t.Fatalf("database assertion failed: %s count=%d want=%d", label, got, want)
	}
}
