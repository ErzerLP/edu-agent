package postgresstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	faultMatrixGoalID             = "70000000-0000-4000-8000-000000000001"
	faultMatrixGoalRevisionID     = "70000000-0000-4000-8000-000000000002"
	faultMatrixRouteID            = "70000000-0000-4000-8000-000000000003"
	faultMatrixRouteRevisionID    = "70000000-0000-4000-8000-000000000004"
	faultMatrixRouteStepID        = "70000000-0000-4000-8000-000000000005"
	faultMatrixSessionID          = "70000000-0000-4000-8000-000000000006"
	faultMatrixFocusFrameID       = "70000000-0000-4000-8000-000000000007"
	faultMatrixFreeQuestionID     = "70000000-0000-4000-8000-000000000008"
	faultMatrixFreeAnswerID       = "70000000-0000-4000-8000-000000000009"
	faultMatrixActivityID         = "70000000-0000-4000-8000-000000000010"
	faultMatrixAttemptPayloadID   = "70000000-0000-4000-8000-000000000011"
	faultMatrixAttemptID          = "70000000-0000-4000-8000-000000000012"
	faultMatrixAssessmentID       = "70000000-0000-4000-8000-000000000013"
	faultMatrixDecisionID         = "70000000-0000-4000-8000-000000000014"
	faultMatrixEvidenceID         = "70000000-0000-4000-8000-000000000015"
	faultMatrixInvalidationID     = "70000000-0000-4000-8000-000000000016"
	faultMatrixExposureID         = "70000000-0000-4000-8000-000000000017"
	faultMatrixMisconceptionID    = "70000000-0000-4000-8000-000000000018"
	faultMatrixOperationID        = "70000000-0000-4000-8000-000000000019"
	faultMatrixSeedSessionID      = "70000000-0000-4000-8000-000000000101"
	faultMatrixSeedFrameID        = "70000000-0000-4000-8000-000000000102"
	faultMatrixSeedOperationID    = "70000000-0000-4000-8000-000000000103"
	faultMatrixUpdateOperationID  = "70000000-0000-4000-8000-000000000104"
	faultMatrixInjectedMarkerBase = "injected typed-record write fault"
)

type faultMatrixFixture int

const (
	faultMatrixMaximal faultMatrixFixture = iota
	faultMatrixSessionUpsert
	faultMatrixFrameInvalidate
	faultMatrixFrameResume
)

type typedRecordFaultCase struct {
	writePoint       string
	table            string
	operation        string
	keyColumn        string
	keyValue         string
	fixture          faultMatrixFixture
	wantTargetBefore int64
}

func TestPostgreSQLTypedRecordWriteFaultMatrixRollsBack(t *testing.T) {
	cases := typedRecordFaultCases()
	assertTypedRecordFaultCoverage(t, cases)
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL typed-record fault matrix not run")
	}

	for _, test := range cases {
		t.Run(strings.ReplaceAll(test.writePoint, "/", "_"), func(t *testing.T) {
			ctx := context.Background()
			pool := learningIntegrationPool(t)
			if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'fault-matrix',now())`, learningDeviceOne); err != nil {
				t.Fatal(err)
			}
			insertLearningKnowledgeFixture(t, pool)
			store := postgresstore.New(pool, tutoringpostgres.New(pool))
			request := faultMatrixRequest(t, store, test.fixture)

			beforeTarget := faultMatrixTargetCount(t, pool, test.table, test.keyColumn, test.keyValue)
			if beforeTarget != test.wantTargetBefore {
				t.Fatalf("%s target rows before fault=%d want=%d", test.writePoint, beforeTarget, test.wantTargetBefore)
			}
			before := captureFaultMatrixSnapshot(t, pool)
			marker := faultMatrixInjectedMarkerBase + ": " + test.writePoint
			installTypedRecordFault(t, pool, test, marker)

			if _, err := store.Commit(ctx, request); err == nil || !strings.Contains(err.Error(), marker) {
				t.Fatalf("%s commit error=%v, want injected marker %q", test.writePoint, err, marker)
			}
			after := captureFaultMatrixSnapshot(t, pool)
			assertFaultMatrixSnapshotEqual(t, before, after)
			if got := faultMatrixTargetCount(t, pool, test.table, test.keyColumn, test.keyValue); got != beforeTarget {
				t.Fatalf("%s target rows after rollback=%d want=%d", test.writePoint, got, beforeTarget)
			}
			assertFaultMatrixOperationAbsent(t, store, pool, request)

			removeTypedRecordFault(t, pool, test.table)
			result, err := store.Commit(ctx, request)
			if err != nil {
				t.Fatalf("%s valid retry after removing trigger: %v", test.writePoint, err)
			}
			if result.Status != "succeeded" || !result.Archived {
				t.Fatalf("%s valid retry result=%+v", test.writePoint, result)
			}
			wantTargetAfter := beforeTarget
			if test.operation == "INSERT" {
				wantTargetAfter++
			}
			if got := faultMatrixTargetCount(t, pool, test.table, test.keyColumn, test.keyValue); got != wantTargetAfter {
				t.Fatalf("%s target rows after valid retry=%d want=%d", test.writePoint, got, wantTargetAfter)
			}
		})
	}
}

func typedRecordFaultCases() []typedRecordFaultCase {
	return []typedRecordFaultCase{
		{writePoint: "learning_goal_revisions/insert", table: "learning_goal_revisions", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixGoalRevisionID, fixture: faultMatrixMaximal},
		{writePoint: "learning_route_revisions/insert", table: "learning_route_revisions", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixRouteRevisionID, fixture: faultMatrixMaximal},
		{writePoint: "learning_route_steps/insert", table: "learning_route_steps", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixRouteStepID, fixture: faultMatrixMaximal},
		{writePoint: "tutoring_sessions/insert", table: "tutoring_sessions", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixSessionID, fixture: faultMatrixMaximal},
		{writePoint: "tutoring_sessions/upsert", table: "tutoring_sessions", operation: "UPDATE", keyColumn: "id", keyValue: faultMatrixSeedSessionID, fixture: faultMatrixSessionUpsert, wantTargetBefore: 1},
		{writePoint: "tutoring_focus_frames/insert", table: "tutoring_focus_frames", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixFocusFrameID, fixture: faultMatrixMaximal},
		{writePoint: "tutoring_focus_frames/invalidate", table: "tutoring_focus_frames", operation: "UPDATE", keyColumn: "session_id", keyValue: faultMatrixSeedSessionID, fixture: faultMatrixFrameInvalidate, wantTargetBefore: 1},
		{writePoint: "tutoring_focus_frames/resume", table: "tutoring_focus_frames", operation: "UPDATE", keyColumn: "session_id", keyValue: faultMatrixSeedSessionID, fixture: faultMatrixFrameResume, wantTargetBefore: 1},
		{writePoint: "tutoring_free_questions/insert", table: "tutoring_free_questions", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixFreeQuestionID, fixture: faultMatrixMaximal},
		{writePoint: "tutoring_free_answers/insert", table: "tutoring_free_answers", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixFreeAnswerID, fixture: faultMatrixMaximal},
		{writePoint: "learning_activities/insert", table: "learning_activities", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixActivityID, fixture: faultMatrixMaximal},
		{writePoint: "learning_activity_references/insert", table: "learning_activity_references", operation: "INSERT", keyColumn: "activity_id", keyValue: faultMatrixActivityID, fixture: faultMatrixMaximal},
		{writePoint: "learning_attempt_payloads/insert", table: "learning_attempt_payloads", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixAttemptPayloadID, fixture: faultMatrixMaximal},
		{writePoint: "learning_attempts/insert", table: "learning_attempts", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixAttemptID, fixture: faultMatrixMaximal},
		{writePoint: "learning_assessments/insert", table: "learning_assessments", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixAssessmentID, fixture: faultMatrixMaximal},
		{writePoint: "learning_assessment_items/insert", table: "learning_assessment_items", operation: "INSERT", keyColumn: "assessment_id", keyValue: faultMatrixAssessmentID, fixture: faultMatrixMaximal},
		{writePoint: "learning_assessment_decisions/insert", table: "learning_assessment_decisions", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixDecisionID, fixture: faultMatrixMaximal},
		{writePoint: "learning_evidence/insert", table: "learning_evidence", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixEvidenceID, fixture: faultMatrixMaximal},
		{writePoint: "learning_evidence_invalidations/insert", table: "learning_evidence_invalidations", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixInvalidationID, fixture: faultMatrixMaximal},
		{writePoint: "learning_exposures/insert", table: "learning_exposures", operation: "INSERT", keyColumn: "id", keyValue: faultMatrixExposureID, fixture: faultMatrixMaximal},
		{writePoint: "learning_misconception_revisions/insert", table: "learning_misconception_revisions", operation: "INSERT", keyColumn: "misconception_id", keyValue: faultMatrixMisconceptionID, fixture: faultMatrixMaximal},
	}
}

func assertTypedRecordFaultCoverage(t *testing.T, cases []typedRecordFaultCase) {
	t.Helper()
	type expectedWritePoint struct {
		table     string
		operation string
	}
	expected := map[string]expectedWritePoint{
		"learning_goal_revisions/insert":         {table: "learning_goal_revisions", operation: "INSERT"},
		"learning_route_revisions/insert":        {table: "learning_route_revisions", operation: "INSERT"},
		"learning_route_steps/insert":            {table: "learning_route_steps", operation: "INSERT"},
		"tutoring_sessions/insert":               {table: "tutoring_sessions", operation: "INSERT"},
		"tutoring_sessions/upsert":               {table: "tutoring_sessions", operation: "UPDATE"},
		"tutoring_focus_frames/insert":           {table: "tutoring_focus_frames", operation: "INSERT"},
		"tutoring_focus_frames/invalidate":       {table: "tutoring_focus_frames", operation: "UPDATE"},
		"tutoring_focus_frames/resume":           {table: "tutoring_focus_frames", operation: "UPDATE"},
		"tutoring_free_questions/insert":         {table: "tutoring_free_questions", operation: "INSERT"},
		"tutoring_free_answers/insert":           {table: "tutoring_free_answers", operation: "INSERT"},
		"learning_activities/insert":             {table: "learning_activities", operation: "INSERT"},
		"learning_activity_references/insert":    {table: "learning_activity_references", operation: "INSERT"},
		"learning_attempt_payloads/insert":       {table: "learning_attempt_payloads", operation: "INSERT"},
		"learning_attempts/insert":               {table: "learning_attempts", operation: "INSERT"},
		"learning_assessments/insert":            {table: "learning_assessments", operation: "INSERT"},
		"learning_assessment_items/insert":       {table: "learning_assessment_items", operation: "INSERT"},
		"learning_assessment_decisions/insert":   {table: "learning_assessment_decisions", operation: "INSERT"},
		"learning_evidence/insert":               {table: "learning_evidence", operation: "INSERT"},
		"learning_evidence_invalidations/insert": {table: "learning_evidence_invalidations", operation: "INSERT"},
		"learning_exposures/insert":              {table: "learning_exposures", operation: "INSERT"},
		"learning_misconception_revisions/insert": {
			table: "learning_misconception_revisions", operation: "INSERT",
		},
	}
	seen := make(map[string]bool, len(cases))
	for _, test := range cases {
		want, ok := expected[test.writePoint]
		if !ok {
			t.Errorf("unexpected typed-record fault case %q", test.writePoint)
			continue
		}
		if seen[test.writePoint] {
			t.Errorf("duplicate typed-record fault case %q", test.writePoint)
		}
		seen[test.writePoint] = true
		if test.table != want.table || test.operation != want.operation {
			t.Errorf("%s trigger=%s %s want=%s %s", test.writePoint, test.operation, test.table, want.operation, want.table)
		}
	}
	for writePoint := range expected {
		if !seen[writePoint] {
			t.Errorf("missing typed-record fault case %q", writePoint)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

func faultMatrixRequest(t *testing.T, store *postgresstore.Store, fixture faultMatrixFixture) learning.CommitRequest {
	t.Helper()
	switch fixture {
	case faultMatrixMaximal:
		return maximalFaultMatrixRequest(t)
	case faultMatrixSessionUpsert:
		seedFaultMatrixSession(t, store, false)
		return updateFaultMatrixSessionRequest(t, false, false)
	case faultMatrixFrameInvalidate:
		seedFaultMatrixSession(t, store, true)
		return updateFaultMatrixSessionRequest(t, true, false)
	case faultMatrixFrameResume:
		seedFaultMatrixSession(t, store, true)
		return updateFaultMatrixSessionRequest(t, false, true)
	default:
		t.Fatalf("unknown fault matrix fixture %d", fixture)
		return learning.CommitRequest{}
	}
}

func maximalFaultMatrixRequest(t *testing.T) learning.CommitRequest {
	t.Helper()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	activityID := faultMatrixActivityID
	attemptID := faultMatrixAttemptID
	contextValue := tutoring.FocusContext{
		GoalRevisionID:      faultMatrixGoalRevisionID,
		RouteRevisionID:     faultMatrixRouteRevisionID,
		RouteStepID:         faultMatrixRouteStepID,
		KnowledgeRevisionID: learningKnowledgeRevision,
		FocusNodeRevisionID: learningNodeRevisionID,
		ActivityID:          &activityID,
		AttemptID:           &attemptID,
	}
	frame := tutoring.FocusFrame{
		ID: faultMatrixFocusFrameID, SessionID: faultMatrixSessionID,
		SavedState: tutoring.StateRouteActive, Context: contextValue,
		SavedAggregateVersion: 1, CreatedEventSequence: 1,
	}
	session := tutoring.Session{
		ID: faultMatrixSessionID, State: tutoring.StateFreeAnswer, AggregateVer: 1,
		Context: contextValue, ActiveFrame: &frame,
	}
	goal := learning.GoalRevision{
		ID: faultMatrixGoalRevisionID, GoalID: faultMatrixGoalID, Revision: 1,
		Text: "fault matrix goal", Source: "integration", ActorDeviceID: learningDeviceOne, CreatedAt: now,
	}
	route := learning.RouteRevision{
		ID: faultMatrixRouteRevisionID, RouteID: faultMatrixRouteID, Revision: 1,
		GoalRevisionID: faultMatrixGoalRevisionID, KnowledgeRevisionID: learningKnowledgeRevision,
		PolicyVersion: learning.RoutePolicyVersion, CreatedAt: now,
		Steps: []learning.RouteStep{{
			ID: faultMatrixRouteStepID, Ordinal: 0, NodeID: learningNodeID,
			NodeRevisionID: learningNodeRevisionID, TeachingIntent: "teach", CompletionCondition: "pass",
		}},
	}
	frozenReference := tutoring.FrozenReference{
		KnowledgeRevisionID: learningKnowledgeRevision, NodeID: learningNodeID,
		NodeRevisionID: learningNodeRevisionID, DocumentRevisionID: learningDocumentRevisionID,
		Start: 0, End: 5, Slice: "topic", SliceSHA256: learning.SHA256([]byte("topic")),
	}
	freeQuestion := tutoring.FreeQuestion{
		ID: faultMatrixFreeQuestionID, SessionID: faultMatrixSessionID, FocusFrameID: faultMatrixFocusFrameID,
		Text: "What is topic?", KnowledgeRevisionID: learningKnowledgeRevision,
		References: []tutoring.FrozenReference{frozenReference}, ActorDeviceID: learningDeviceOne,
		OccurredAt: &now, ReceivedAt: now,
	}
	freeAnswer := tutoring.FreeAnswer{
		ID: faultMatrixFreeAnswerID, SessionID: faultMatrixSessionID, FocusFrameID: faultMatrixFocusFrameID,
		FreeQuestionID: faultMatrixFreeQuestionID, Text: "Topic is the answer.",
		KnowledgeRevisionID: learningKnowledgeRevision,
		References:          []tutoring.FrozenReference{frozenReference}, ReceivedAt: now,
	}
	knowledgeReference := learning.KnowledgeReference{
		KnowledgeRevisionID: learningKnowledgeRevision, NodeID: learningNodeID,
		NodeRevisionID: learningNodeRevisionID, DocumentRevisionID: learningDocumentRevisionID,
		Range: learning.SourceRange{Start: 0, End: 5}, Slice: "topic", SliceSHA256: learning.SHA256([]byte("topic")),
	}
	activity := learning.Activity{
		ID: faultMatrixActivityID, Revision: 1, SessionID: faultMatrixSessionID,
		GoalRevisionID: faultMatrixGoalRevisionID, RouteRevisionID: faultMatrixRouteRevisionID,
		RouteStepID: faultMatrixRouteStepID, KnowledgeRevisionID: learningKnowledgeRevision,
		TargetNodeID: learningNodeID, TargetNodeRevisionID: learningNodeRevisionID,
		References: []learning.KnowledgeReference{knowledgeReference}, Prompt: "topic?", Type: learning.ActivityObjective,
		Rubric: learning.Rubric{
			Revision: "fault-rubric-v1", Items: []learning.RubricItem{{ID: "item-1", Criterion: "correct"}},
			ObjectiveRule: &learning.ObjectiveRule{AcceptedAnswers: []string{"ok"}, TrimSpace: true},
		},
		Difficulty: 1, AllowedHelp: []learning.HelpLevel{learning.HelpNone},
		ActivityPolicyVersion: learning.ActivityPolicyVersion, AssessmentPolicyVersion: learning.AssessmentPolicyVersion,
		ReviewPolicyVersion: learning.ReviewPolicyVersion, AttachedFreeQuestionID: faultMatrixFreeQuestionID,
		AttachedFreeAnswerID: faultMatrixFreeAnswerID, CreatedAt: now,
	}
	attempt := learning.Attempt{
		ID: faultMatrixAttemptID, SessionID: faultMatrixSessionID, ActivityID: faultMatrixActivityID,
		ActivityRevision: 1, AnswerPayloadID: faultMatrixAttemptPayloadID, Answer: "ok",
		AnswerSHA256: learning.SHA256([]byte("ok")), Help: learning.HelpNone,
		ActorDeviceID: learningDeviceOne, OccurredAt: &now, ReceivedAt: now,
	}
	assessmentItem := learning.AssessmentItem{
		RubricItemID: "item-1", Conclusion: learning.ConclusionPass,
		AnswerQuote: "ok", AnswerRange: learning.SourceRange{Start: 0, End: 2},
		AnswerQuoteSHA256: learning.SHA256([]byte("ok")), KnowledgeReferenceID: learningNodeRevisionID,
		KnowledgeQuote: "topic", KnowledgeRange: learning.SourceRange{Start: 0, End: 5},
		KnowledgeQuoteSHA256: learning.SHA256([]byte("topic")), MisconceptionCandidate: "confuses topic",
	}
	assessment := learning.AssessmentArtifact{
		ID: faultMatrixAssessmentID, SessionID: faultMatrixSessionID, AttemptID: faultMatrixAttemptID,
		ActivityID: faultMatrixActivityID, ActivityRevision: 1, Items: []learning.AssessmentItem{assessmentItem},
		RubricComplete: true, Confidence: 800, RiskFlags: []learning.RiskFlag{learning.RiskSchemaRepaired},
		ModelID: "fault-matrix", ModelParameters: map[string]any{"temperature": 0}, PromptRevision: "fault-prompt-v1",
		ProposalInputHash: learning.SHA256([]byte("fault proposal")), Attempts: 1,
		AttemptCategories: []string{"success"}, CreatedAt: now,
	}
	decision := learning.AssessmentDecision{
		ID: faultMatrixDecisionID, AssessmentID: faultMatrixAssessmentID, Version: 1,
		Disposition: learning.DispositionAccepted, Items: []learning.AssessmentItem{assessmentItem},
		ActorDeviceID: learningDeviceOne, CreatedAt: now,
	}
	evidence := learning.AcceptedEvidence{
		ID: faultMatrixEvidenceID, DispositionDecisionID: faultMatrixDecisionID,
		AssessmentID: faultMatrixAssessmentID, AttemptID: faultMatrixAttemptID,
		ActivityID: faultMatrixActivityID, ActivityRevision: 1,
		GoalRevisionID: faultMatrixGoalRevisionID, RouteRevisionID: faultMatrixRouteRevisionID,
		KnowledgeRevisionID: learningKnowledgeRevision, NodeRevisionID: learningNodeRevisionID,
		RubricRevision: "fault-rubric-v1", Kind: learning.EvidencePracticeRecall,
		ActivityType: learning.ActivityObjective, Outcome: learning.OutcomePass, Help: learning.HelpNone,
		ReceivedAt: now, AcceptancePolicyVersion: learning.AssessmentPolicyVersion,
		ReducerPolicyVersion: learning.MasteryReducerVersion, ReviewPolicyVersion: learning.ReviewPolicyVersion,
		Misconceptions: []learning.MisconceptionCandidate{{RubricItemID: "item-1", Text: "confuses topic"}},
		RubricOutcomes: []learning.RubricOutcome{{RubricItemID: "item-1", Conclusion: learning.ConclusionPass}},
	}
	decisionID := faultMatrixDecisionID
	invalidation := learning.EvidenceInvalidation{
		ID: faultMatrixInvalidationID, EvidenceID: faultMatrixEvidenceID, DecisionID: &decisionID,
		Reason: "fault matrix", EventSeq: 1, CreatedAt: now,
	}
	exposure := learning.Exposure{
		ID: faultMatrixExposureID, SessionID: faultMatrixSessionID, Kind: "reading",
		Text: "topic", References: []learning.KnowledgeReference{knowledgeReference}, ReceivedAt: now,
	}
	misconception := learning.MisconceptionHypothesis{
		ID: faultMatrixMisconceptionID, Revision: 1, NodeRevisionID: learningNodeRevisionID,
		RubricItemID: "item-1", CandidateHash: learning.SHA256([]byte("confuses topic")),
		Candidate: "confuses topic", Status: learning.MisconceptionProposed,
		SourceEvidenceIDs: []string{faultMatrixEvidenceID}, CounterEvidenceIDs: []string{},
		CausedByEvidenceID: faultMatrixEvidenceID,
	}
	message, err := outbox.NewMessage(outbox.NewMessageInput{
		BusinessType: "learning.fault-matrix", AggregateID: faultMatrixSessionID,
		IdempotencyKey: "learning-typed-record-fault-matrix", Revision: 1,
		Payload: json.RawMessage(`{"kind":"fault-matrix"}`), AuditMetadata: json.RawMessage(`{}`), MaxAttempts: 3,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	goalPayload, err := json.Marshal(goal)
	if err != nil {
		t.Fatal(err)
	}
	return learning.CommitRequest{
		DeviceID: learningDeviceOne,
		Operation: learning.OperationEnvelope{
			OperationID: faultMatrixOperationID, PayloadSchemaVersion: 1,
			AggregateType: "goal", AggregateID: faultMatrixGoalID, ExpectedVersion: 0,
			Payload: json.RawMessage(`{"command":"typed_record_fault_matrix"}`),
		},
		RequestHash:  learning.SHA256([]byte(faultMatrixOperationID)),
		Expectations: []learning.AggregateExpectation{{Type: "goal", ID: faultMatrixGoalID, ExpectedVersion: 0}},
		Batch: learning.CommandBatch{
			GoalRevision: &goal, RouteRevision: &route, Session: &session, FocusFrame: &frame,
			FreeQuestion: &freeQuestion, FreeAnswer: &freeAnswer, Activity: &activity, Attempt: &attempt,
			Assessment: &assessment, Decisions: []learning.AssessmentDecision{decision},
			Evidence: []learning.AcceptedEvidence{evidence}, Invalidations: []learning.EvidenceInvalidation{invalidation},
			Exposures: []learning.Exposure{exposure}, Misconceptions: []learning.MisconceptionHypothesis{misconception},
			Outbox: []outbox.Message{message},
			Events: []learning.EventDraft{{
				Type: learning.EventGoalRevisionCreated, AggregateType: "goal", AggregateID: faultMatrixGoalID, Payload: goalPayload,
			}},
			Authority: learning.AuthorityProvenance{
				RouteSteps: map[string]learning.KnowledgeOwner{faultMatrixRouteStepID: {
					KnowledgeRevisionID: learningKnowledgeRevision, NodeID: learningNodeID,
					NodeRevisionID: learningNodeRevisionID, DocumentRevisionID: learningDocumentRevisionID,
				}},
				AssessmentItems: []learning.KnowledgeOwner{{
					KnowledgeRevisionID: learningKnowledgeRevision, NodeID: learningNodeID,
					NodeRevisionID: learningNodeRevisionID, DocumentRevisionID: learningDocumentRevisionID,
				}},
				Evidence: map[string]learning.EvidenceOwner{faultMatrixEvidenceID: {
					SessionID: faultMatrixSessionID,
					KnowledgeOwner: learning.KnowledgeOwner{
						KnowledgeRevisionID: learningKnowledgeRevision, NodeID: learningNodeID,
						NodeRevisionID: learningNodeRevisionID, DocumentRevisionID: learningDocumentRevisionID,
					},
				}},
			},
		},
		ReceivedAt: now,
	}
}

func seedFaultMatrixSession(t *testing.T, store *postgresstore.Store, withFrame bool) {
	t.Helper()
	now := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	session := tutoring.Session{ID: faultMatrixSeedSessionID, State: tutoring.StateGoalReady}
	batch := learning.CommandBatch{Session: &session, TutoringState: string(session.State)}
	eventType := learning.EventLearningSessionStarted
	if withFrame {
		frame := &tutoring.FocusFrame{
			ID: faultMatrixSeedFrameID, SessionID: faultMatrixSeedSessionID,
			SavedState: tutoring.StateRouteActive, SavedAggregateVersion: 1,
		}
		session.State = tutoring.StateFreeQuestion
		session.ActiveFrame = frame
		batch.Session = &session
		batch.FocusFrame = frame
		batch.TutoringState = string(session.State)
		eventType = learning.EventFocusSuspended
	}
	payload, err := json.Marshal(learning.SessionProjection{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	batch.Events = []learning.EventDraft{{
		Type: eventType, AggregateType: "session", AggregateID: faultMatrixSeedSessionID, Payload: payload,
	}}
	request := learning.CommitRequest{
		DeviceID: learningDeviceOne,
		Operation: learning.OperationEnvelope{
			OperationID: faultMatrixSeedOperationID, PayloadSchemaVersion: 1,
			AggregateType: "session", AggregateID: faultMatrixSeedSessionID, ExpectedVersion: 0,
			Payload: json.RawMessage(`{"command":"seed_fault_matrix"}`),
		},
		RequestHash: learning.SHA256([]byte(faultMatrixSeedOperationID)),
		Expectations: []learning.AggregateExpectation{{
			Type: "session", ID: faultMatrixSeedSessionID, ExpectedVersion: 0,
		}},
		Batch: batch, ReceivedAt: now,
	}
	if _, err := store.Commit(context.Background(), request); err != nil {
		t.Fatalf("seed fault matrix session: %v", err)
	}
}

func updateFaultMatrixSessionRequest(t *testing.T, invalidate, resume bool) learning.CommitRequest {
	t.Helper()
	now := time.Date(2026, 9, 1, 13, 1, 0, 0, time.UTC)
	session := tutoring.Session{ID: faultMatrixSeedSessionID, State: tutoring.StateDiagnostic}
	eventType := learning.EventTutoringStateChanged
	if invalidate {
		session.State = tutoring.StateCompleted
	}
	if resume {
		session.State = tutoring.StateFocusResumed
		eventType = learning.EventFocusResumed
	}
	payload, err := json.Marshal(learning.SessionProjection{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	return learning.CommitRequest{
		DeviceID: learningDeviceOne,
		Operation: learning.OperationEnvelope{
			OperationID: faultMatrixUpdateOperationID, PayloadSchemaVersion: 1,
			AggregateType: "session", AggregateID: faultMatrixSeedSessionID, ExpectedVersion: 1,
			Payload: json.RawMessage(`{"command":"update_fault_matrix"}`),
		},
		RequestHash: learning.SHA256([]byte(faultMatrixUpdateOperationID)),
		Expectations: []learning.AggregateExpectation{{
			Type: "session", ID: faultMatrixSeedSessionID, ExpectedVersion: 1,
		}},
		Batch: learning.CommandBatch{
			Session: &session, InvalidateFrame: invalidate, ResumeFrame: resume,
			TutoringState: string(session.State),
			Events: []learning.EventDraft{{
				Type: eventType, AggregateType: "session", AggregateID: faultMatrixSeedSessionID, Payload: payload,
			}},
		},
		ReceivedAt: now,
	}
}

type faultMatrixSnapshot map[string][]byte

func captureFaultMatrixSnapshot(t *testing.T, pool *pgxpool.Pool) faultMatrixSnapshot {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT tablename
		FROM pg_catalog.pg_tables
		WHERE schemaname=current_schema()
		  AND (starts_with(tablename,'learning_') OR starts_with(tablename,'tutoring_') OR tablename='outbox_messages')
		ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	assertFaultSnapshotTables(t, tables)

	snapshot := make(faultMatrixSnapshot, len(tables))
	for _, table := range tables {
		query := fmt.Sprintf(`
			SELECT COALESCE(jsonb_agg(row_value ORDER BY row_value::text),'[]'::jsonb)::text
			FROM (SELECT to_jsonb(source_row) AS row_value FROM %s AS source_row) AS snapshot_rows`,
			pgx.Identifier{table}.Sanitize())
		var value []byte
		if err := pool.QueryRow(ctx, query).Scan(&value); err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		snapshot[table] = append([]byte(nil), value...)
	}
	return snapshot
}

func assertFaultSnapshotTables(t *testing.T, actual []string) {
	t.Helper()
	required := []string{
		"outbox_messages",
		"learning_event_clock", "learning_aggregate_heads", "learning_inbox", "learning_event_payloads", "learning_events",
		"learning_goal_revisions", "learning_route_revisions", "learning_route_steps",
		"tutoring_sessions", "tutoring_focus_frames", "tutoring_free_questions", "tutoring_proposal_requests", "tutoring_proposal_artifacts", "tutoring_free_answers",
		"learning_activities", "learning_activity_references", "learning_attempt_payloads", "learning_attempts",
		"learning_assessments", "learning_assessment_items", "learning_assessment_decisions", "learning_evidence",
		"learning_evidence_invalidations", "learning_exposures", "learning_misconception_revisions",
		"learning_projection_generations", "learning_projection_head", "learning_projection_checkpoints",
		"learning_projection_timeline", "learning_projection_routes", "learning_projection_sessions",
		"learning_projection_nodes", "learning_projection_evidence", "learning_projection_reviews",
		"learning_projection_misconceptions", "learning_projection_stats",
	}
	seen := make(map[string]bool, len(actual))
	for _, table := range actual {
		seen[table] = true
	}
	for _, table := range required {
		if !seen[table] {
			t.Fatalf("fault matrix snapshot is missing required table %s; actual=%v", table, actual)
		}
	}
}

func assertFaultMatrixSnapshotEqual(t *testing.T, before, after faultMatrixSnapshot) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("fault rollback snapshot table count changed: before=%d after=%d", len(before), len(after))
	}
	tables := make([]string, 0, len(before))
	for table := range before {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		afterValue, ok := after[table]
		if !ok {
			t.Fatalf("fault rollback snapshot lost table %s", table)
		}
		if !bytes.Equal(before[table], afterValue) {
			t.Fatalf("fault rollback changed %s:\nbefore=%s\nafter=%s", table, before[table], afterValue)
		}
	}
}

func installTypedRecordFault(t *testing.T, pool *pgxpool.Pool, test typedRecordFaultCase, marker string) {
	t.Helper()
	if test.operation != "INSERT" && test.operation != "UPDATE" {
		t.Fatalf("unsupported fault trigger operation %q", test.operation)
	}
	functionName := pgx.Identifier{"fault_matrix_reject_write"}.Sanitize()
	triggerName := pgx.Identifier{"fault_matrix_reject_write"}.Sanitize()
	tableName := pgx.Identifier{test.table}.Sanitize()
	statement := fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $fault_matrix$
		BEGIN
			IF to_jsonb(NEW)->>%s = %s THEN
				RAISE EXCEPTION USING MESSAGE = %s;
			END IF;
			RETURN NEW;
		END
		$fault_matrix$;
		CREATE TRIGGER %s BEFORE %s ON %s
		FOR EACH ROW EXECUTE FUNCTION %s()`,
		functionName, quoteFaultMatrixLiteral(test.keyColumn), quoteFaultMatrixLiteral(test.keyValue),
		quoteFaultMatrixLiteral(marker), triggerName, test.operation, tableName, functionName)
	if _, err := pool.Exec(context.Background(), statement); err != nil {
		t.Fatalf("install %s fault: %v", test.writePoint, err)
	}
}

func removeTypedRecordFault(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	statement := fmt.Sprintf(`DROP TRIGGER fault_matrix_reject_write ON %s; DROP FUNCTION fault_matrix_reject_write()`, pgx.Identifier{table}.Sanitize())
	if _, err := pool.Exec(context.Background(), statement); err != nil {
		t.Fatalf("remove typed-record fault from %s: %v", table, err)
	}
}

func quoteFaultMatrixLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func faultMatrixTargetCount(t *testing.T, pool *pgxpool.Pool, table, keyColumn, keyValue string) int64 {
	t.Helper()
	query := fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s::text=$1`, pgx.Identifier{table}.Sanitize(), pgx.Identifier{keyColumn}.Sanitize())
	var count int64
	if err := pool.QueryRow(context.Background(), query, keyValue).Scan(&count); err != nil {
		t.Fatalf("count %s.%s=%s: %v", table, keyColumn, keyValue, err)
	}
	return count
}

func assertFaultMatrixOperationAbsent(t *testing.T, store *postgresstore.Store, pool *pgxpool.Pool, request learning.CommitRequest) {
	t.Helper()
	lookup := learning.OperationLookup{
		DeviceID: request.DeviceID, OperationID: request.Operation.OperationID, RequestHash: request.RequestHash,
	}
	if result, err, found := store.LookupOperation(context.Background(), lookup); err != nil || found {
		t.Fatalf("failed operation remained archived: found=%v result=%+v err=%v", found, result, err)
	}
	var eventCount, inboxCount int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_events WHERE device_id=$1 AND operation_id=$2`, request.DeviceID, request.Operation.OperationID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_inbox WHERE device_id=$1 AND operation_id=$2`, request.DeviceID, request.Operation.OperationID).Scan(&inboxCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 || inboxCount != 0 {
		t.Fatalf("failed operation residue: events=%d inbox=%d", eventCount, inboxCount)
	}
}
