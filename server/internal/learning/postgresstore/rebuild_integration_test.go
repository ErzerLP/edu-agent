package postgresstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningpostgres "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxpostgres "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	learningReplaySecondEvidenceID         = "87000000-0000-4000-8000-000000000010"
	learningReplaySecondDecisionID         = "87000000-0000-4000-8000-000000000011"
	learningReplaySecondAssessmentID       = "87000000-0000-4000-8000-000000000012"
	learningReplaySecondAttemptID          = "87000000-0000-4000-8000-000000000013"
	learningReplaySecondActivityID         = "87000000-0000-4000-8000-000000000014"
	learningReplayInvalidatedEvidenceID    = "87000000-0000-4000-8000-000000000015"
	learningReplayInvalidatedDecisionID    = "87000000-0000-4000-8000-000000000016"
	learningReplayInvalidatedAssessmentID  = "87000000-0000-4000-8000-000000000017"
	learningReplayOverrideAssessmentID     = "87000000-0000-4000-8000-000000000018"
	learningReplayVoidedAssessmentID       = "87000000-0000-4000-8000-000000000019"
	learningReplayOfflineAssessmentID      = "87000000-0000-4000-8000-000000000020"
	learningReplayOfflineAggregateID       = "87000000-0000-4000-8000-000000000021"
	learningReplayCarryoverProposalID      = "87000000-0000-4000-8000-000000000030"
	learningReplayStaleProposalID          = "87000000-0000-4000-8000-000000000031"
	learningReplayCarryoverDecisionID      = "87000000-0000-4000-8000-000000000032"
	learningReplayCarryoverLinkID          = "87000000-0000-4000-8000-000000000033"
	learningReplayTargetKnowledgeID        = "87000000-0000-4000-8000-000000000034"
	learningReplayTargetNodeID             = "87000000-0000-4000-8000-000000000035"
	learningReplayTargetNodeRevisionID     = "87000000-0000-4000-8000-000000000036"
	learningReplayTargetDocumentRevision   = "87000000-0000-4000-8000-000000000037"
	learningReplayLegacyRedactionEventID   = "87100000-0000-4000-8000-000000000080"
	learningReplayLegacyRedactionPayloadID = "87200000-0000-4000-8000-000000000080"
	learningReplayLegacyRedactionOpID      = "87300000-0000-4000-8000-000000000080"
	learningReplayLegacyErasureID          = "87400000-0000-4000-8000-000000000080"
	learningReplayEventNamespace           = "7d812fdc-fc90-4c6b-8f8f-9badf3281f70"
)

type learningReplayEventInput struct {
	ID                  string
	PayloadID           string
	OperationID         string
	Type                learning.EventType
	SchemaVersion       int
	Payload             any
	ReceivedAt          time.Time
	Source              string
	ParentSessionID     string
	OfflineAggregateID  string
	ArchiveDisposition  string
	EvidenceDisposition string
}

type canonicalLearningMetadata struct {
	AsOfEventSequence     int64    `json:"as_of_event_seq"`
	ProjectionVersion     string   `json:"projection_version"`
	MasteryReducerVersion string   `json:"mastery_reducer_version"`
	AssessmentPolicy      string   `json:"assessment_policy_version"`
	ReviewPolicy          string   `json:"review_policy_version"`
	KnowledgeRevisionID   string   `json:"knowledge_revision_id"`
	Incomplete            bool     `json:"incomplete"`
	Degraded              bool     `json:"degraded"`
	ReasonCodes           []string `json:"reason_codes"`
}

type canonicalLearningSession struct {
	SessionID string                     `json:"session_id"`
	Item      learning.SessionProjection `json:"item"`
}

type canonicalLearningStat struct {
	SessionID string                      `json:"session_id"`
	Item      learning.ActiveTimeEstimate `json:"item"`
}

type canonicalLearningNode struct {
	NodeRevisionID string                 `json:"node_revision_id"`
	Item           learning.NodeReduction `json:"item"`
}

type canonicalLearningReview struct {
	NodeRevisionID string                  `json:"node_revision_id"`
	StableID       string                  `json:"stable_id"`
	Item           learning.ReviewSchedule `json:"item"`
}

type canonicalLearningMisconception struct {
	NodeRevisionID string                           `json:"node_revision_id"`
	Item           learning.MisconceptionHypothesis `json:"item"`
}

type canonicalLearningSnapshot struct {
	Metadata              canonicalLearningMetadata               `json:"metadata"`
	HighWater             int64                                   `json:"high_water"`
	ProductionFingerprint string                                  `json:"production_fingerprint"`
	Timeline              []learning.TimelineItem                 `json:"timeline"`
	Routes                []learning.RouteProjection              `json:"routes"`
	Sessions              []canonicalLearningSession              `json:"sessions"`
	Stats                 []canonicalLearningStat                 `json:"stats"`
	Nodes                 []canonicalLearningNode                 `json:"nodes"`
	Evidence              []learning.AcceptedEvidence             `json:"evidence"`
	Reviews               []canonicalLearningReview               `json:"reviews"`
	Misconceptions        []canonicalLearningMisconception        `json:"misconceptions"`
	Carryovers            []learning.ProvisionalEvidenceCarryover `json:"carryovers"`
}

func TestPostgreSQLLearningAuthoritativeWriteGroupResponseLossRetryAndWorkerRestart(t *testing.T) {
	pool, initialStore, _ := newOfflineIngestFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	activityID := seedOfflineActivity(t, pool, "open", now.Add(-time.Hour), now.Add(time.Hour), now.Add(24*time.Hour))
	operation := seedOfflineSubmission(t, pool, learningDeviceOne, 90, activityID, now.Add(time.Hour), now.Add(24*time.Hour), learning.OfflineAttemptCompleted)
	operation.Attempt.Answer = "topic"
	operation.Attempt.AnswerSHA256 = learning.SHA256([]byte(operation.Attempt.Answer))

	// The caller loses the first response after the production transaction commits.
	if _, err := initialStore.IngestOffline(ctx, learning.OfflineIngestRequest{Operation: operation}); err != nil {
		t.Fatalf("first offline authoritative write group: %v", err)
	}
	restartedStore := learningpostgres.New(pool, tutoringpostgres.New(pool), knowledgedb.New(pool))
	replayed, err := restartedStore.IngestOffline(ctx, learning.OfflineIngestRequest{Operation: operation})
	if err != nil || !replayed.Replayed || replayed.AssessmentStatus != learning.OfflineAssessmentQueued {
		t.Fatalf("response-loss retry after store restart result=%+v err=%v", replayed, err)
	}
	assertLearningAuthorityFactCounts(t, pool, operation.SubmissionID, operation.OperationID, learningAuthorityFactCounts{
		Attempts: 1, Outbox: 1, Inbox: 1,
	})

	service, err := learning.NewService(restartedStore, restartedStore, offlineEvaluationIntegrationResolver{}, learning.ServiceOptions{
		Model: offlineEvaluationIntegrationModel{}, ModelID: "replay-idempotency-model",
		ModelParameters: map[string]any{"temperature": 0}, PromptRevision: "replay-idempotency-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := learning.NewOfflineEvaluationConsumer(service, restartedStore)
	if err != nil {
		t.Fatal(err)
	}
	recordingConsumer := &offlineEvaluationRecordingConsumer{inner: consumer}
	newWorker := func() *outbox.Worker {
		worker, workerErr := outbox.NewWorker(outboxpostgres.New(pool), map[string]outbox.Consumer{
			"learning.offline-evaluation": recordingConsumer,
		}, outbox.WorkerOptions{
			BatchSize: 1, Lease: time.Minute, BaseBackoff: time.Second, MaxBackoff: time.Minute,
			Now: time.Now, Jitter: func(time.Duration) time.Duration { return 0 },
		})
		if workerErr != nil {
			t.Fatal(workerErr)
		}
		return worker
	}
	if processed, err := newWorker().RunOnce(ctx); err != nil || processed != 1 {
		t.Fatalf("first evaluation worker processed=%d err=%v", processed, err)
	}
	if recordingConsumer.err != nil {
		t.Fatalf("offline evaluation consumer: %v", recordingConsumer.err)
	}
	if processed, err := newWorker().RunOnce(ctx); err != nil || processed != 0 {
		t.Fatalf("restarted evaluation worker processed=%d err=%v", processed, err)
	}
	if replayed, err := restartedStore.IngestOffline(ctx, learning.OfflineIngestRequest{Operation: operation}); err != nil || !replayed.Replayed {
		t.Fatalf("post-worker operation retry result=%+v err=%v", replayed, err)
	}
	assertLearningAuthorityFactCounts(t, pool, operation.SubmissionID, operation.OperationID, learningAuthorityFactCounts{
		Attempts: 1, Assessments: 1, Decisions: 1, Evidence: 1, Outbox: 1, Inbox: 1,
	})
}

type learningAuthorityFactCounts struct {
	Attempts, Assessments, Decisions, Evidence, Outbox, Inbox int64
}

func assertLearningAuthorityFactCounts(
	t *testing.T,
	pool *pgxpool.Pool,
	submissionID, operationID string,
	want learningAuthorityFactCounts,
) {
	t.Helper()
	var got learningAuthorityFactCounts
	if err := pool.QueryRow(context.Background(), `
		SELECT
		  (SELECT count(*) FROM learning_attempts WHERE id=$1),
		  (SELECT count(*) FROM learning_assessments WHERE attempt_id=$1),
		  (SELECT count(*) FROM learning_assessment_decisions decision
		     JOIN learning_assessments assessment ON assessment.id=decision.assessment_id
		    WHERE assessment.attempt_id=$1),
		  (SELECT count(*) FROM learning_evidence WHERE attempt_id=$1),
		  (SELECT count(*) FROM outbox_messages WHERE idempotency_key='learning.offline-evaluation:'||$1),
		  (SELECT count(*) FROM learning_inbox WHERE operation_id=$2)`, submissionID, operationID).Scan(
		&got.Attempts, &got.Assessments, &got.Decisions, &got.Evidence, &got.Outbox, &got.Inbox,
	); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("authoritative fact counts got=%+v want=%+v", got, want)
	}
}

func TestPostgreSQLLearningFullProjectionReplayParityAndFailClosedCorpus(t *testing.T) {
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'learning-replay-oracle',clock_timestamp())`, learningDeviceOne); err != nil {
		t.Fatal(err)
	}
	insertLearningKnowledgeFixture(t, pool)
	store := learningpostgres.New(pool, tutoringpostgres.New(pool))
	goal := goalCommit(t, learningDeviceOne, "20000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000001", 0, 1, 1)
	if _, err := store.Commit(ctx, goal); err != nil {
		t.Fatal(err)
	}

	baseTime := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	goalStatus, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	legacyRedactionSequence := appendLegacyLearningRedactionEvent(t, pool, goalStatus.HighWater, baseTime.Add(-time.Hour))
	sessionID, initialVersion := commitLearningAuthorityFixture(t, store, true)
	initialStatus, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertLegacyLearningRedactionFixture(t, pool)

	secondEvidence := learning.AcceptedEvidence{
		ID: learningReplaySecondEvidenceID, DispositionDecisionID: learningReplaySecondDecisionID,
		AssessmentID: learningReplaySecondAssessmentID, AttemptID: learningReplaySecondAttemptID,
		ActivityID: learningReplaySecondActivityID, ActivityRevision: 1,
		GoalRevisionID:      "30000000-0000-4000-8000-000000000001",
		RouteRevisionID:     "50000000-0000-4000-8000-000000000002",
		KnowledgeRevisionID: learningKnowledgeRevision, NodeRevisionID: learningNodeRevisionID,
		RubricRevision: "r2", Kind: learning.EvidenceReviewRecall,
		ActivityType: learning.ActivityObjective, Outcome: learning.OutcomePass, Help: learning.HelpHint,
		ReceivedAt: baseTime.Add(24 * time.Hour), AcceptancePolicyVersion: learning.AssessmentPolicyVersion,
		ReducerPolicyVersion: learning.MasteryReducerVersion, ReviewPolicyVersion: learning.ReviewPolicyVersion,
	}
	secondSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000001", PayloadID: "87200000-0000-4000-8000-000000000001",
		OperationID: "87300000-0000-4000-8000-000000000001", Type: learning.EventEvidenceAccepted,
		SchemaVersion: learning.EventSchemaVersion, Payload: secondEvidence, ReceivedAt: secondEvidence.ReceivedAt,
	})
	secondEvidence.AcceptedEventSequence = secondSequence
	invalidatedEvidence := secondEvidence
	invalidatedEvidence.ID = learningReplayInvalidatedEvidenceID
	invalidatedEvidence.DispositionDecisionID = learningReplayInvalidatedDecisionID
	invalidatedEvidence.AssessmentID = learningReplayInvalidatedAssessmentID
	invalidatedEvidence.AttemptID = "87000000-0000-4000-8000-000000000041"
	invalidatedEvidence.ActivityID = "87000000-0000-4000-8000-000000000042"
	invalidatedEvidence.ReceivedAt = baseTime.Add(24*time.Hour + time.Minute)
	invalidatedSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000010", PayloadID: "87200000-0000-4000-8000-000000000010",
		OperationID: "87300000-0000-4000-8000-000000000010", Type: learning.EventEvidenceAccepted,
		SchemaVersion: learning.EventSchemaVersion, Payload: invalidatedEvidence, ReceivedAt: invalidatedEvidence.ReceivedAt,
	})
	invalidatedEvidence.AcceptedEventSequence = invalidatedSequence
	invalidationSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000011", PayloadID: "87200000-0000-4000-8000-000000000011",
		OperationID: "87300000-0000-4000-8000-000000000011", Type: learning.EventEvidenceInvalidated,
		SchemaVersion: learning.EventSchemaVersion, Payload: map[string]any{"evidence_id": invalidatedEvidence.ID},
		ReceivedAt: baseTime.Add(24*time.Hour + 2*time.Minute),
	})
	overridePendingSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000012", PayloadID: "87200000-0000-4000-8000-000000000012",
		OperationID: "87300000-0000-4000-8000-000000000012", Type: learning.EventAssessmentMarkedProvisional,
		SchemaVersion: learning.EventSchemaVersion, Payload: learning.AssessmentProjectionEvent{
			AssessmentID: learningReplayOverrideAssessmentID, NodeRevisionID: learningNodeRevisionID, Reasons: []string{"manual_review"},
		}, ReceivedAt: baseTime.Add(24*time.Hour + 3*time.Minute),
	})
	overriddenSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000013", PayloadID: "87200000-0000-4000-8000-000000000013",
		OperationID: "87300000-0000-4000-8000-000000000013", Type: learning.EventAssessmentOverridden,
		SchemaVersion: learning.EventSchemaVersion, Payload: learning.AssessmentProjectionEvent{
			AssessmentID: learningReplayOverrideAssessmentID, NodeRevisionID: learningNodeRevisionID,
		}, ReceivedAt: baseTime.Add(24*time.Hour + 4*time.Minute),
	})
	voidPendingSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000014", PayloadID: "87200000-0000-4000-8000-000000000014",
		OperationID: "87300000-0000-4000-8000-000000000014", Type: learning.EventAssessmentMarkedProvisional,
		SchemaVersion: learning.EventSchemaVersion, Payload: learning.AssessmentProjectionEvent{
			AssessmentID: learningReplayVoidedAssessmentID, NodeRevisionID: learningNodeRevisionID, Reasons: []string{"unsafe_artifact"},
		}, ReceivedAt: baseTime.Add(24*time.Hour + 5*time.Minute),
	})
	voidedSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000015", PayloadID: "87200000-0000-4000-8000-000000000015",
		OperationID: "87300000-0000-4000-8000-000000000015", Type: learning.EventAssessmentVoided,
		SchemaVersion: learning.EventSchemaVersion, Payload: learning.AssessmentProjectionEvent{
			AssessmentID: learningReplayVoidedAssessmentID, NodeRevisionID: learningNodeRevisionID,
		}, ReceivedAt: baseTime.Add(24*time.Hour + 6*time.Minute),
	})
	queuedSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000002", PayloadID: "87200000-0000-4000-8000-000000000002",
		OperationID: "87300000-0000-4000-8000-000000000002", Type: learning.EventOfflineAssessmentQueued,
		SchemaVersion: learning.EventSchemaVersion, Payload: map[string]any{
			"assessment_id": learningReplayOfflineAssessmentID, "node_revision_id": learningNodeRevisionID,
			"reasons": []string{"offline_pending"},
		}, ReceivedAt: baseTime.Add(25 * time.Hour), Source: "offline", ParentSessionID: sessionID,
		OfflineAggregateID: learningReplayOfflineAggregateID, EvidenceDisposition: string(learning.OfflineEvidencePendingEvaluation),
	})
	acceptedSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000003", PayloadID: "87200000-0000-4000-8000-000000000003",
		OperationID: "87300000-0000-4000-8000-000000000003", Type: learning.EventAssessmentAccepted,
		SchemaVersion: learning.EventSchemaVersion, Payload: learning.AssessmentProjectionEvent{
			AssessmentID: learningReplayOfflineAssessmentID, NodeRevisionID: learningNodeRevisionID,
		}, ReceivedAt: baseTime.Add(26 * time.Hour), Source: "offline", ParentSessionID: sessionID,
		OfflineAggregateID: learningReplayOfflineAggregateID, EvidenceDisposition: string(learning.OfflineEvidenceAccepted),
	})
	provisional := learning.ProvisionalEvidenceCarryover{
		ProposalID: learningReplayCarryoverProposalID, KnowledgeProposalID: "87000000-0000-4000-8000-000000000038",
		SourceEvidenceID:          "50000000-0000-4000-8000-000000000010",
		SourceKnowledgeRevisionID: learningKnowledgeRevision, SourceNodeRevisionID: learningNodeRevisionID,
		TargetKnowledgeRevisionID: learningReplayTargetKnowledgeID,
		Links: []learning.EvidenceCarryoverLink{{
			ID: learningReplayCarryoverLinkID, ProposalID: learningReplayCarryoverProposalID,
			SourceEvidenceID:          "50000000-0000-4000-8000-000000000010",
			TargetKnowledgeRevisionID: learningReplayTargetKnowledgeID, TargetNodeID: learningReplayTargetNodeID,
			TargetNodeRevisionID:     learningReplayTargetNodeRevisionID,
			TargetDocumentRevisionID: learningReplayTargetDocumentRevision,
			DecisionID:               learningReplayCarryoverDecisionID, EventID: "87100000-0000-4000-8000-000000000004",
			CreatedAt: baseTime.Add(27 * time.Hour),
		}},
		BasisFingerprint: strings.Repeat("a", 64), PolicyVersion: learning.EvidenceCarryoverPolicyVersion,
	}
	carryoverSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000004", PayloadID: "87200000-0000-4000-8000-000000000004",
		OperationID: "87300000-0000-4000-8000-000000000004", Type: learning.EventEvidenceCarryoverApproved,
		SchemaVersion: learning.EventSchemaVersion, Payload: learning.EvidenceCarryoverEvent{
			ProposalID: learningReplayCarryoverProposalID, DecisionID: learningReplayCarryoverDecisionID,
			RequestedDecision: "approve", Outcome: string(learning.EvidenceCarryoverApproved),
			Reason: "deterministic carryover", Provisional: &provisional,
		}, ReceivedAt: baseTime.Add(27 * time.Hour),
	})
	provisional.ApprovedEventSequence = carryoverSequence
	staleSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000005", PayloadID: "87200000-0000-4000-8000-000000000005",
		OperationID: "87300000-0000-4000-8000-000000000005", Type: learning.EventEvidenceCarryoverStaled,
		SchemaVersion: learning.EventSchemaVersion, Payload: learning.EvidenceCarryoverEvent{
			ProposalID: learningReplayStaleProposalID, DecisionID: "87000000-0000-4000-8000-000000000039",
			RequestedDecision: "approve", Outcome: string(learning.EvidenceCarryoverStale), Reason: "stale basis",
		}, ReceivedAt: baseTime.Add(28 * time.Hour),
	})
	redactedSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000006", PayloadID: "87200000-0000-4000-8000-000000000006",
		OperationID: "87300000-0000-4000-8000-000000000006", Type: learning.EventRedacted,
		SchemaVersion: learning.EventSchemaVersion, Payload: map[string]any{
			"event_id": "87100000-0000-4000-8000-000000000002", "evidence_id": "",
		}, ReceivedAt: baseTime.Add(29 * time.Hour),
	})
	offlineAttemptAt := baseTime.Add(30 * time.Hour)
	offlineSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000007", PayloadID: "87200000-0000-4000-8000-000000000007",
		OperationID: "87300000-0000-4000-8000-000000000007", Type: learning.EventOfflineAttemptSubmitted,
		SchemaVersion: learning.EventSchemaVersion, Payload: map[string]any{"attempt_id": "87000000-0000-4000-8000-000000000040"},
		ReceivedAt: offlineAttemptAt, Source: "offline", ParentSessionID: sessionID,
		OfflineAggregateID: learningReplayOfflineAggregateID, EvidenceDisposition: string(learning.OfflineEvidenceNotApplicable),
	})
	offlineRejectedSequence, currentVersion := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000008", PayloadID: "87200000-0000-4000-8000-000000000008",
		OperationID: "87300000-0000-4000-8000-000000000008", Type: learning.EventOfflineOperationRejected,
		SchemaVersion: learning.EventSchemaVersion, Payload: map[string]any{"reason_codes": []string{"archive_expired"}},
		ReceivedAt: baseTime.Add(31 * time.Hour), Source: "offline", ParentSessionID: sessionID,
		OfflineAggregateID: learningReplayOfflineAggregateID, ArchiveDisposition: "rejected",
		EvidenceDisposition: string(learning.OfflineEvidenceUnchanged),
	})
	if initialVersion+10 != currentVersion {
		t.Fatalf("direct session corpus version=%d want=%d", currentVersion, initialVersion+10)
	}

	tickRequest := learning.CommitRequest{
		DeviceID: learningDeviceOne,
		Operation: learning.OperationEnvelope{
			OperationID: "87300000-0000-4000-8000-000000000090", PayloadSchemaVersion: 1,
			AggregateType: "session", AggregateID: sessionID, ExpectedVersion: currentVersion,
			Payload: json.RawMessage(`{"command":"projection_tick"}`),
		},
		RequestHash:  learning.SHA256([]byte("learning projection parity tick")),
		Expectations: []learning.AggregateExpectation{{Type: "session", ID: sessionID, ExpectedVersion: currentVersion}},
		Batch: learning.CommandBatch{Events: []learning.EventDraft{{
			Type: learning.EventActivityPresented, AggregateType: "session", AggregateID: sessionID,
			Payload: json.RawMessage(`{"kind":"projection_tick"}`),
		}}},
		ReceivedAt: baseTime.Add(32 * time.Hour),
	}
	tick, err := store.Commit(ctx, tickRequest)
	if err != nil {
		t.Fatal(err)
	}
	var eventsBeforeReplay, inboxBeforeReplay int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_events`).Scan(&eventsBeforeReplay); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_inbox`).Scan(&inboxBeforeReplay); err != nil {
		t.Fatal(err)
	}
	for replayIndex := 0; replayIndex < 2; replayIndex++ {
		replay, replayErr := store.Commit(ctx, tickRequest)
		if replayErr != nil || !replay.Replayed || replay.FirstEventSequence != tick.FirstEventSequence || replay.LastEventSequence != tick.LastEventSequence {
			t.Fatalf("learning operation replay %d result=%+v err=%v", replayIndex+1, replay, replayErr)
		}
	}
	assertLearningCount(t, pool, `SELECT count(*) FROM learning_events`, eventsBeforeReplay)
	assertLearningCount(t, pool, `SELECT count(*) FROM learning_inbox`, inboxBeforeReplay)

	incremental := loadCanonicalLearningSnapshot(t, pool, store)
	assertIndependentLearningOracle(t, incremental, sessionID, tick.AggregateVersion, initialStatus.HighWater,
		secondEvidence, invalidatedEvidence, provisional, offlineAttemptAt, legacyRedactionSequence,
		[]int64{
			legacyRedactionSequence, invalidatedSequence, invalidationSequence, overridePendingSequence,
			overriddenSequence, voidPendingSequence, voidedSequence, queuedSequence, acceptedSequence,
			carryoverSequence, staleSequence, redactedSequence, offlineSequence, offlineRejectedSequence,
		})
	incrementalSemantic := canonicalLearningFingerprint(t, incremental)
	firstGeneration := incrementalGeneration(t, store)

	firstRebuild, err := store.Rebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	firstReplay := loadCanonicalLearningSnapshot(t, pool, store)
	assertLearningSnapshotEqual(t, firstReplay, incremental, "first shadow rebuild", "incremental projection")
	if firstRebuild.ActiveGenerationID == firstGeneration || canonicalLearningFingerprint(t, firstReplay) != incrementalSemantic {
		t.Fatalf("first replay did not switch to semantically identical generation: before=%s after=%s", firstGeneration, firstRebuild.ActiveGenerationID)
	}
	secondRebuild, err := store.Rebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secondReplay := loadCanonicalLearningSnapshot(t, pool, store)
	assertLearningSnapshotEqual(t, secondReplay, incremental, "second shadow rebuild", "incremental projection")
	if secondRebuild.ActiveGenerationID == firstRebuild.ActiveGenerationID || canonicalLearningFingerprint(t, secondReplay) != incrementalSemantic {
		t.Fatalf("second replay did not remain idempotent: first=%s second=%s", firstRebuild.ActiveGenerationID, secondRebuild.ActiveGenerationID)
	}
	assertLearningCount(t, pool, `SELECT count(*) FROM learning_projection_evidence WHERE generation_id=(SELECT active_generation_id FROM learning_projection_head WHERE singleton_id=1)`, 3)
	assertLearningCount(t, pool, `SELECT count(*) FROM learning_projection_carryovers WHERE generation_id=(SELECT active_generation_id FROM learning_projection_head WHERE singleton_id=1)`, 1)

	stableStatus, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	duplicateErr := attemptRejectedLearningEventInsert(t, pool, sessionID, stableStatus.HighWater+1,
		"87100000-0000-4000-8000-000000000001", "87200000-0000-4000-8000-000000000091", "87300000-0000-4000-8000-000000000091")
	assertLearningUniqueViolation(t, duplicateErr, "duplicate event identity")
	assertProjectionStatusEqual(t, store, stableStatus, "duplicate event identity")
	outOfOrderErr := attemptRejectedLearningEventInsert(t, pool, sessionID, stableStatus.HighWater-1,
		"87100000-0000-4000-8000-000000000092", "87200000-0000-4000-8000-000000000092", "87300000-0000-4000-8000-000000000092")
	assertLearningUniqueViolation(t, outOfOrderErr, "out-of-order event sequence")
	assertProjectionStatusEqual(t, store, stableStatus, "out-of-order event sequence")

	badSequence, _ := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{
		ID: "87100000-0000-4000-8000-000000000099", PayloadID: "87200000-0000-4000-8000-000000000099",
		OperationID: "87300000-0000-4000-8000-000000000099", Type: learning.EventExposureRecorded,
		SchemaVersion: 99, Payload: map[string]any{"kind": "future"}, ReceivedAt: baseTime.Add(33 * time.Hour),
	})
	if badSequence != stableStatus.HighWater+1 {
		t.Fatalf("bad event sequence=%d want=%d", badSequence, stableStatus.HighWater+1)
	}
	if _, err := store.Rebuild(ctx); learning.ErrorCode(err) != learning.CodeUnsupportedEventSchema {
		t.Fatalf("bad event rebuild error=%v code=%q", err, learning.ErrorCode(err))
	}
	afterFailure, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.ActiveGenerationID != stableStatus.ActiveGenerationID || afterFailure.RebuildingGenerationID != nil ||
		!afterFailure.Metadata.Incomplete || !afterFailure.Metadata.Degraded ||
		!contains(afterFailure.Metadata.ReasonCodes, learning.CodeUnsupportedEventSchema) {
		t.Fatalf("bad event did not fail closed around active generation: before=%+v after=%+v", stableStatus, afterFailure)
	}
	postFailureNode, err := store.Node(ctx, learningNodeRevisionID)
	if err != nil || postFailureNode.Node.Mastery.State != learning.MasteryRetained || postFailureNode.Node.Mastery.ValidEvidenceCount != 2 {
		t.Fatalf("old mastery read model unavailable after failed rebuild: node=%+v err=%v", postFailureNode, err)
	}
	postFailureReviews, err := store.Reviews(ctx, learning.ReviewQuery{})
	if err != nil || len(postFailureReviews.Items) != 1 || postFailureReviews.Items[0].NodeRevisionID != learningNodeRevisionID {
		t.Fatalf("old review read model unavailable after failed rebuild: reviews=%+v err=%v", postFailureReviews, err)
	}
	assertLearningCount(t, pool, `SELECT count(*) FROM learning_projection_generations WHERE status='failed' AND $1=ANY(reason_codes)`, 1, learning.CodeUnsupportedEventSchema)
}

func appendLegacyLearningRedactionEvent(t *testing.T, pool *pgxpool.Pool, redactedThrough int64, receivedAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var high int64
	if err := tx.QueryRow(ctx, `SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1 FOR UPDATE`).Scan(&high); err != nil {
		t.Fatal(err)
	}
	sequence := high + 1
	payload := canonicalLearningJSON(t, map[string]any{
		"erasure_id": learningReplayLegacyErasureID, "generation": 2,
		"redacted_through": redactedThrough, "policy_version": "privacy-erasure-v1", "reason_code": "learner_request",
	})
	digest := sha256.Sum256(payload)
	if _, err := tx.Exec(ctx, `
		INSERT INTO learning_aggregate_heads(aggregate_type,aggregate_id,aggregate_version,last_event_seq,updated_at)
		VALUES('privacy',$1,1,$2,$3)`, learningReplayLegacyErasureID, sequence, receivedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO learning_event_payloads(id,payload,payload_hash,created_at) VALUES($1,$2,$3,$4)`,
		learningReplayLegacyRedactionPayloadID, payload, digest[:], receivedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO learning_events(
		  event_seq,id,event_type,event_schema_version,aggregate_type,aggregate_id,aggregate_version,
		  device_id,operation_id,operation_ordinal,received_at,payload_id,payload_hash,event_source)
		VALUES($1,$2,$3,$4,'privacy',$5,1,$6,$7,0,$8,$9,$10,'online')`,
		sequence, learningReplayLegacyRedactionEventID, learning.EventRedacted, learning.EventSchemaVersion,
		learningReplayLegacyErasureID, learningDeviceOne, learningReplayLegacyRedactionOpID, receivedAt,
		learningReplayLegacyRedactionPayloadID, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE learning_event_clock SET current_event_seq=$1,updated_at=$2 WHERE singleton_id=1`, sequence, receivedAt); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return sequence
}

func assertLegacyLearningRedactionFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var schemaVersion int
	var payload string
	if err := pool.QueryRow(context.Background(), `
		SELECT event.event_schema_version,payload.payload::text
		FROM learning_events event
		JOIN learning_event_payloads payload ON payload.id=event.payload_id
		WHERE event.id=$1`, learningReplayLegacyRedactionEventID).Scan(&schemaVersion, &payload); err != nil {
		t.Fatal(err)
	}
	if schemaVersion != learning.EventSchemaVersion || !strings.Contains(payload, `"redacted_through"`) || strings.Contains(payload, `"redacted_through_event_seq"`) {
		t.Fatalf("legacy redaction fixture schema=%d payload=%s", schemaVersion, payload)
	}
}

func appendLearningReplayEvent(t *testing.T, pool *pgxpool.Pool, sessionID string, input learningReplayEventInput) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var high int64
	if err := tx.QueryRow(ctx, `SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1 FOR UPDATE`).Scan(&high); err != nil {
		t.Fatal(err)
	}
	var eventVersion int64
	aggregateType := "session"
	aggregateID := sessionID
	if input.Source == "offline" {
		if input.OfflineAggregateID == "" || input.ParentSessionID == "" || input.EvidenceDisposition == "" {
			t.Fatal("offline replay event requires aggregate, parent session, and evidence disposition")
		}
		aggregateType = "offline_attempt"
		aggregateID = input.OfflineAggregateID
		if _, err := tx.Exec(ctx, `
			INSERT INTO learning_aggregate_heads(aggregate_type,aggregate_id,aggregate_version,last_event_seq,updated_at)
			VALUES('offline_attempt',$1,0,0,$2) ON CONFLICT DO NOTHING`, aggregateID, input.ReceivedAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.QueryRow(ctx, `
		SELECT aggregate_version FROM learning_aggregate_heads
		WHERE aggregate_type=$1 AND aggregate_id=$2 FOR UPDATE`, aggregateType, aggregateID).Scan(&eventVersion); err != nil {
		t.Fatal(err)
	}
	sequence := high + 1
	switch evidence := input.Payload.(type) {
	case learning.AcceptedEvidence:
		if input.Type == learning.EventEvidenceAccepted {
			evidence.AcceptedEventSequence = sequence
			input.Payload = evidence
		}
	case *learning.AcceptedEvidence:
		if input.Type == learning.EventEvidenceAccepted && evidence != nil {
			copy := *evidence
			copy.AcceptedEventSequence = sequence
			input.Payload = copy
		}
	}
	eventVersion++
	payload := canonicalLearningJSON(t, input.Payload)
	digest := sha256.Sum256(payload)
	schemaVersion := input.SchemaVersion
	if schemaVersion == 0 {
		schemaVersion = learning.EventSchemaVersion
	}
	source := input.Source
	if source == "" {
		source = "online"
	}
	var parent any
	if input.ParentSessionID != "" {
		parent = input.ParentSessionID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO learning_event_payloads(id,payload,payload_hash,created_at) VALUES($1,$2,$3,$4)`, input.PayloadID, payload, digest[:], input.ReceivedAt); err != nil {
		t.Fatal(err)
	}
	var archiveDisposition, evidenceDisposition, goalRevisionID, routeRevisionID, knowledgeRevisionID, activityID, activityRevision any
	if source == "offline" {
		archiveDisposition = input.ArchiveDisposition
		if archiveDisposition == "" {
			archiveDisposition = "succeeded"
		}
		evidenceDisposition = input.EvidenceDisposition
		goalRevisionID = "30000000-0000-4000-8000-000000000001"
		routeRevisionID = "50000000-0000-4000-8000-000000000002"
		knowledgeRevisionID = learningKnowledgeRevision
		activityID = "50000000-0000-4000-8000-000000000005"
		activityRevision = int64(1)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO learning_events(
		  event_seq,id,event_type,event_schema_version,aggregate_type,aggregate_id,aggregate_version,
		  device_id,operation_id,operation_ordinal,received_at,occurred_at,payload_id,payload_hash,
		  parent_session_id,event_source,archive_disposition,evidence_disposition,goal_revision_id,
		  route_revision_id,knowledge_revision_id,activity_id,activity_revision)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
		sequence, input.ID, input.Type, schemaVersion, aggregateType, aggregateID, eventVersion, learningDeviceOne,
		input.OperationID, eventVersion-1, input.ReceivedAt, input.PayloadID, digest[:], parent, source,
		archiveDisposition, evidenceDisposition, goalRevisionID, routeRevisionID, knowledgeRevisionID, activityID, activityRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE learning_aggregate_heads SET aggregate_version=$3,last_event_seq=$4,updated_at=$5
		WHERE aggregate_type=$1 AND aggregate_id=$2`, aggregateType, aggregateID, eventVersion, sequence, input.ReceivedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE learning_event_clock SET current_event_seq=$1,updated_at=$2 WHERE singleton_id=1`, sequence, input.ReceivedAt); err != nil {
		t.Fatal(err)
	}
	var sessionVersion int64
	if err := tx.QueryRow(ctx, `SELECT aggregate_version FROM learning_aggregate_heads WHERE aggregate_type='session' AND aggregate_id=$1`, sessionID).Scan(&sessionVersion); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return sequence, sessionVersion
}

func canonicalLearningJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func loadCanonicalLearningSnapshot(t *testing.T, pool *pgxpool.Pool, store *learningpostgres.Store) canonicalLearningSnapshot {
	t.Helper()
	ctx := context.Background()
	status, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result := canonicalLearningSnapshot{
		Metadata: canonicalLearningMetadata{
			AsOfEventSequence:     status.Metadata.AsOfEventSequence,
			ProjectionVersion:     status.Metadata.ProjectionVersion,
			MasteryReducerVersion: status.Metadata.MasteryReducerVersion,
			AssessmentPolicy:      status.Metadata.AssessmentPolicy, ReviewPolicy: status.Metadata.ReviewPolicy,
			KnowledgeRevisionID: status.Metadata.KnowledgeRevisionID,
			Incomplete:          status.Metadata.Incomplete, Degraded: status.Metadata.Degraded,
			ReasonCodes: append([]string{}, status.Metadata.ReasonCodes...),
		},
		HighWater: status.HighWater, ProductionFingerprint: status.Fingerprint,
	}
	generation := status.ActiveGenerationID
	rows, err := pool.Query(ctx, `SELECT item FROM learning_projection_timeline WHERE generation_id=$1 ORDER BY event_seq`, generation)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		var item learning.TimelineItem
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		result.Timeline = append(result.Timeline, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()

	rows, err = pool.Query(ctx, `SELECT item FROM learning_projection_routes WHERE generation_id=$1 ORDER BY event_seq,route_revision_id`, generation)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		var item learning.RouteProjection
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		result.Routes = append(result.Routes, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()

	rows, err = pool.Query(ctx, `SELECT session_id::text,item FROM learning_projection_sessions WHERE generation_id=$1 ORDER BY session_id`, generation)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		var item canonicalLearningSession
		if err := rows.Scan(&item.SessionID, &raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &item.Item); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		result.Sessions = append(result.Sessions, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()

	rows, err = pool.Query(ctx, `SELECT session_id::text,item FROM learning_projection_stats WHERE generation_id=$1 ORDER BY session_id`, generation)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		var item canonicalLearningStat
		if err := rows.Scan(&item.SessionID, &raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &item.Item); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		result.Stats = append(result.Stats, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()

	rows, err = pool.Query(ctx, `SELECT node_revision_id::text,item FROM learning_projection_nodes WHERE generation_id=$1 ORDER BY node_revision_id`, generation)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		var item canonicalLearningNode
		if err := rows.Scan(&item.NodeRevisionID, &raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &item.Item); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		result.Nodes = append(result.Nodes, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()

	rows, err = pool.Query(ctx, `SELECT item FROM learning_projection_evidence WHERE generation_id=$1 ORDER BY accepted_event_seq,evidence_id`, generation)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		var item learning.AcceptedEvidence
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		result.Evidence = append(result.Evidence, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()

	rows, err = pool.Query(ctx, `SELECT node_revision_id::text,stable_id::text,item FROM learning_projection_reviews WHERE generation_id=$1 ORDER BY due_at,node_revision_id,stable_id`, generation)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		var item canonicalLearningReview
		if err := rows.Scan(&item.NodeRevisionID, &item.StableID, &raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &item.Item); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		result.Reviews = append(result.Reviews, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()

	rows, err = pool.Query(ctx, `SELECT node_revision_id::text,item FROM learning_projection_misconceptions WHERE generation_id=$1 ORDER BY misconception_id`, generation)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		var item canonicalLearningMisconception
		if err := rows.Scan(&item.NodeRevisionID, &raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &item.Item); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		result.Misconceptions = append(result.Misconceptions, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()

	rows, err = pool.Query(ctx, `SELECT item FROM learning_projection_carryovers WHERE generation_id=$1 ORDER BY proposal_id`, generation)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		var item learning.ProvisionalEvidenceCarryover
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		result.Carryovers = append(result.Carryovers, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	return result
}

func assertIndependentLearningOracle(
	t *testing.T,
	snapshot canonicalLearningSnapshot,
	sessionID string,
	finalVersion int64,
	firstEvidenceSequence int64,
	secondEvidence learning.AcceptedEvidence,
	invalidatedEvidence learning.AcceptedEvidence,
	carryover learning.ProvisionalEvidenceCarryover,
	offlineAttemptAt time.Time,
	legacyRedactionSequence int64,
	directSequences []int64,
) {
	t.Helper()
	if snapshot.Metadata.AsOfEventSequence != snapshot.HighWater || snapshot.Metadata.Incomplete || snapshot.Metadata.Degraded {
		t.Fatalf("incremental projection is not complete: %+v", snapshot.Metadata)
	}
	if snapshot.Metadata.KnowledgeRevisionID != learningKnowledgeRevision {
		t.Fatalf("knowledge revision=%s want=%s", snapshot.Metadata.KnowledgeRevisionID, learningKnowledgeRevision)
	}
	firstEvidence := learning.AcceptedEvidence{
		ID:                    "50000000-0000-4000-8000-000000000010",
		DispositionDecisionID: "50000000-0000-4000-8000-000000000009",
		AssessmentID:          "50000000-0000-4000-8000-000000000007",
		AttemptID:             "50000000-0000-4000-8000-000000000006",
		ActivityID:            "50000000-0000-4000-8000-000000000005", ActivityRevision: 1,
		GoalRevisionID:      "30000000-0000-4000-8000-000000000001",
		RouteRevisionID:     "50000000-0000-4000-8000-000000000002",
		KnowledgeRevisionID: learningKnowledgeRevision, NodeRevisionID: learningNodeRevisionID,
		RubricRevision: "r1", Kind: learning.EvidencePracticeRecall,
		ActivityType: learning.ActivityObjective, Outcome: learning.OutcomePass, Help: learning.HelpNone,
		ReceivedAt:              time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC),
		AcceptedEventSequence:   firstEvidenceSequence,
		AcceptancePolicyVersion: learning.AssessmentPolicyVersion,
		ReducerPolicyVersion:    learning.MasteryReducerVersion, ReviewPolicyVersion: learning.ReviewPolicyVersion,
	}
	wantEvidence := []learning.AcceptedEvidence{firstEvidence, secondEvidence, invalidatedEvidence}
	if !reflect.DeepEqual(snapshot.Evidence, wantEvidence) {
		t.Fatalf("independent evidence oracle mismatch\ngot=%+v\nwant=%+v", snapshot.Evidence, wantEvidence)
	}
	if len(snapshot.Nodes) != 1 || snapshot.Nodes[0].NodeRevisionID != learningNodeRevisionID {
		t.Fatalf("node projection=%+v", snapshot.Nodes)
	}
	lastEvidenceAt := secondEvidence.ReceivedAt
	wantMastery := learning.MasteryProjection{
		NodeRevisionID: learningNodeRevisionID, State: learning.MasteryRetained,
		BaselineState: learning.MasteryRetained, ValidEvidenceCount: 2,
		Kinds:          map[learning.EvidenceKind]int{learning.EvidencePracticeRecall: 1, learning.EvidenceReviewRecall: 1},
		Outcomes:       map[learning.Outcome]int{learning.OutcomePass: 2},
		Help:           map[learning.HelpLevel]int{learning.HelpNone: 1, learning.HelpHint: 1},
		LastEvidenceAt: &lastEvidenceAt, PendingAssessments: 0,
		ReducerVersion: learning.MasteryReducerVersion,
	}
	if !reflect.DeepEqual(snapshot.Nodes[0].Item.Mastery, wantMastery) {
		t.Fatalf("independent mastery oracle mismatch\ngot=%+v\nwant=%+v", snapshot.Nodes[0].Item.Mastery, wantMastery)
	}
	wantReview := learning.ReviewSchedule{
		NodeRevisionID: learningNodeRevisionID, Step: 1,
		DueAt:         secondEvidence.ReceivedAt.Add(3 * 24 * time.Hour),
		Intervals:     []time.Duration{24 * time.Hour, 3 * 24 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour, 30 * 24 * time.Hour},
		PolicyVersion: learning.ReviewPolicyVersion,
	}
	if snapshot.Nodes[0].Item.Review == nil || !reflect.DeepEqual(*snapshot.Nodes[0].Item.Review, wantReview) || len(snapshot.Nodes[0].Item.Misconceptions) != 0 {
		t.Fatalf("independent node reduction oracle mismatch node=%+v want_review=%+v", snapshot.Nodes[0].Item, wantReview)
	}
	stableReviewID := uuid.NewSHA1(uuid.MustParse(learningReplayEventNamespace), []byte("review\n"+learningNodeRevisionID)).String()
	wantReviews := []canonicalLearningReview{{NodeRevisionID: learningNodeRevisionID, StableID: stableReviewID, Item: wantReview}}
	if !reflect.DeepEqual(snapshot.Reviews, wantReviews) {
		t.Fatalf("independent review table oracle mismatch\ngot=%+v\nwant=%+v", snapshot.Reviews, wantReviews)
	}
	if len(snapshot.Carryovers) != 1 || !reflect.DeepEqual(snapshot.Carryovers[0], carryover) || snapshot.Carryovers[0].ProposalID == learningReplayStaleProposalID {
		t.Fatalf("independent carryover oracle mismatch got=%+v want=%+v", snapshot.Carryovers, carryover)
	}
	staleEvents := 0
	for _, item := range snapshot.Timeline {
		if item.Type == learning.EventEvidenceCarryoverStaled {
			staleEvents++
		}
	}
	if staleEvents != 1 {
		t.Fatalf("stale carryover timeline events=%d want=1", staleEvents)
	}
	var session *canonicalLearningSession
	for index := range snapshot.Sessions {
		if snapshot.Sessions[index].SessionID == sessionID {
			session = &snapshot.Sessions[index]
			break
		}
	}
	if session == nil || session.Item.Session.State != tutoring.StateFeedback || session.Item.Session.AggregateVer != finalVersion || session.Item.UpdatedEventSequence != snapshot.HighWater {
		t.Fatalf("session oracle mismatch session=%+v final_version=%d high=%d", session, finalVersion, snapshot.HighWater)
	}
	var stat *canonicalLearningStat
	for index := range snapshot.Stats {
		if snapshot.Stats[index].SessionID == sessionID {
			stat = &snapshot.Stats[index]
			break
		}
	}
	firstAt := time.Date(2026, 8, 20, 14, 0, 6, 0, time.UTC)
	wantStat := learning.ActiveTimeEstimate{
		DurationSeconds: 300, Estimated: true, AlgorithmVersion: learning.ActiveTimePolicyVersion,
		SampleCount: 2, FirstReceivedAt: &firstAt, LastReceivedAt: &offlineAttemptAt,
	}
	if stat == nil || !reflect.DeepEqual(stat.Item, wantStat) {
		t.Fatalf("active-time oracle mismatch got=%+v want=%+v", stat, wantStat)
	}
	if len(snapshot.Routes) != 1 || !snapshot.Routes[0].Current || snapshot.Routes[0].Route.KnowledgeRevisionID != learningKnowledgeRevision {
		t.Fatalf("route projection oracle mismatch: %+v", snapshot.Routes)
	}
	if len(snapshot.Timeline) == 0 || snapshot.Timeline[0].EventSequence != legacyRedactionSequence || snapshot.Timeline[0].Type != learning.EventRedacted {
		t.Fatalf("legacy redaction was not upcast into the active projection timeline: %+v", snapshot.Timeline)
	}
	requiredTypes := []learning.EventType{
		learning.EventAssessmentOverridden, learning.EventAssessmentVoided, learning.EventEvidenceInvalidated,
		learning.EventOfflineOperationRejected,
	}
	for _, required := range requiredTypes {
		found := false
		for _, item := range snapshot.Timeline {
			if item.Type == required {
				found = true
				if required == learning.EventOfflineOperationRejected && (item.Source != "offline" || item.ArchiveDisposition != "rejected" || item.EvidenceDisposition != string(learning.OfflineEvidenceUnchanged)) {
					t.Fatalf("offline terminal metadata=%+v", item)
				}
				break
			}
		}
		if !found {
			t.Fatalf("required compensation/terminal event %s missing from timeline", required)
		}
	}
	for _, sequence := range directSequences {
		found := false
		for _, item := range snapshot.Timeline {
			if item.EventSequence == sequence {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("direct corpus event sequence %d missing from timeline", sequence)
		}
	}
}

func canonicalLearningFingerprint(t *testing.T, snapshot canonicalLearningSnapshot) string {
	t.Helper()
	copy := snapshot
	copy.ProductionFingerprint = ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func assertLearningSnapshotEqual(t *testing.T, got, want canonicalLearningSnapshot, gotName, wantName string) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	gotJSON, _ := json.MarshalIndent(got, "", "  ")
	wantJSON, _ := json.MarshalIndent(want, "", "  ")
	t.Fatalf("learning snapshots differ: %s != %s\n%s:\n%s\n%s:\n%s", gotName, wantName, gotName, gotJSON, wantName, wantJSON)
}

func incrementalGeneration(t *testing.T, store *learningpostgres.Store) string {
	t.Helper()
	status, err := store.ProjectionStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return status.ActiveGenerationID
}

func attemptRejectedLearningEventInsert(t *testing.T, pool *pgxpool.Pool, sessionID string, sequence int64, eventID, payloadID, operationID string) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var version int64
	if err := tx.QueryRow(ctx, `SELECT aggregate_version+1 FROM learning_aggregate_heads WHERE aggregate_type='session' AND aggregate_id=$1 FOR UPDATE`, sessionID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	payload := canonicalLearningJSON(t, map[string]any{"kind": "rejected"})
	digest := sha256.Sum256(payload)
	if _, err := tx.Exec(ctx, `INSERT INTO learning_event_payloads(id,payload,payload_hash,created_at) VALUES($1,$2,$3,clock_timestamp())`, payloadID, payload, digest[:]); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO learning_events(
		  event_seq,id,event_type,event_schema_version,aggregate_type,aggregate_id,aggregate_version,
		  device_id,operation_id,operation_ordinal,received_at,payload_id,payload_hash,event_source)
		VALUES($1,$2,'ActivityPresented',1,'session',$3,$4,$5,$6,0,clock_timestamp(),$7,$8,'online')`,
		sequence, eventID, sessionID, version, learningDeviceOne, operationID, payloadID, digest[:])
	return err
}

func assertLearningUniqueViolation(t *testing.T, err error, name string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("%s error=%v pg=%+v; want unique violation", name, err, pgErr)
	}
}

func assertProjectionStatusEqual(t *testing.T, store *learningpostgres.Store, want learning.ProjectionStatus, operation string) {
	t.Helper()
	got, err := store.ProjectionStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s changed projection status\ngot=%+v\nwant=%+v", operation, got, want)
	}
}

func assertLearningCount(t *testing.T, pool *pgxpool.Pool, query string, want int64, args ...any) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("query %q=%d want=%d", query, got, want)
	}
}
