package postgresstore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type privacyMarkerFixture struct {
	marker string

	deviceID string
	tokenID  string

	knowledgeRevisionID string
	documentRevisionID  string
	nodeID              string
	nodeRevisionID      string
	lineageID           string
	artifactID          string

	goalID               string
	goalRevisionID       string
	routeID              string
	routeRevisionID      string
	routeStepID          string
	sessionID            string
	focusFrameID         string
	freeQuestionID       string
	freeAnswerID         string
	proposalRequestID    string
	proposalArtifactID   string
	activityID           string
	attemptPayloadID     string
	attemptID            string
	assessmentID         string
	assessmentDecisionID string
	evidenceID           string
	invalidationID       string
	exposureID           string
	misconceptionID      string
	learningEventID      string
	learningPayloadID    string
	learningOperationID  string

	oldActiveProjectionID  string
	oldRetiredProjectionID string

	pendingCandidateID  string
	admittedCandidateID string
	logicalMemoryID     string
	recordRevisionID    string
	deliveryID          string
	memoryAttemptID     string
	memoryReceiptID     string
	outboxID            string
}

type markerLocation struct {
	table  string
	column string
	min    int64
}

var privacyMarkerLocationsByStore = map[privacy.StoreKind][]markerLocation{
	privacy.StoreIdentityMetadata: {
		{table: "devices", column: "display_name", min: 1},
	},
	privacy.StoreKnowledgeContent: {
		{table: "knowledge_revisions", column: "source", min: 1},
		{table: "knowledge_document_payloads", column: "canonical_markdown", min: 1},
		{table: "knowledge_snapshot_documents", column: "canonical_path", min: 1},
		{table: "knowledge_snapshot_documents", column: "folded_path", min: 1},
		{table: "knowledge_lineages", column: "reason", min: 1},
	},
	privacy.StoreKnowledgeIndex: {
		{table: "knowledge_node_revisions", column: "title", min: 1},
		{table: "knowledge_node_revisions", column: "ancestor_titles", min: 1},
	},
	privacy.StoreKnowledgeArtifacts: {
		{table: "knowledge_node_artifacts", column: "content", min: 1},
	},
	privacy.StoreLearningEventPayload: {
		{table: "learning_event_payloads", column: "payload", min: 1},
	},
	privacy.StoreLearningTypedPayload: {
		{table: "learning_inbox", column: "result", min: 1},
		{table: "learning_goal_revisions", column: "goal_text", min: 1},
		{table: "learning_goal_revisions", column: "source", min: 1},
		{table: "learning_route_steps", column: "teaching_intent", min: 1},
		{table: "learning_route_steps", column: "completion_condition", min: 1},
		{table: "learning_activities", column: "prompt", min: 1},
		{table: "learning_activities", column: "rubric_revision", min: 1},
		{table: "learning_activities", column: "rubric", min: 1},
		{table: "learning_activity_references", column: "slice_text", min: 1},
		{table: "learning_attempt_payloads", column: "answer_text", min: 1},
		{table: "learning_assessments", column: "risk_flags", min: 1},
		{table: "learning_assessments", column: "trusted_model_id", min: 1},
		{table: "learning_assessments", column: "model_parameters", min: 1},
		{table: "learning_assessments", column: "prompt_revision", min: 1},
		{table: "learning_assessments", column: "attempt_categories", min: 1},
		{table: "learning_assessment_items", column: "rubric_item_id", min: 1},
		{table: "learning_assessment_items", column: "answer_quote", min: 1},
		{table: "learning_assessment_items", column: "knowledge_quote", min: 1},
		{table: "learning_assessment_items", column: "misconception_candidate", min: 1},
		{table: "learning_assessment_decisions", column: "conclusions", min: 1},
		{table: "learning_assessment_decisions", column: "reason", min: 1},
		{table: "learning_evidence", column: "rubric_revision", min: 1},
		{table: "learning_evidence", column: "misconception_candidates", min: 1},
		{table: "learning_evidence", column: "rubric_outcomes", min: 1},
		{table: "learning_evidence_invalidations", column: "reason", min: 1},
		{table: "learning_exposures", column: "content", min: 1},
		{table: "learning_exposures", column: "references_snapshot", min: 1},
		{table: "learning_misconception_revisions", column: "rubric_item_id", min: 1},
		{table: "learning_misconception_revisions", column: "candidate_text", min: 1},
	},
	privacy.StoreTutoringPayload: {
		{table: "tutoring_focus_frames", column: "invalidation_reason", min: 1},
		{table: "tutoring_free_questions", column: "question_text", min: 1},
		{table: "tutoring_free_questions", column: "references_snapshot", min: 1},
		{table: "tutoring_free_answers", column: "answer_text", min: 1},
		{table: "tutoring_free_answers", column: "references_snapshot", min: 1},
		{table: "tutoring_proposal_requests", column: "input", min: 1},
		{table: "tutoring_proposal_requests", column: "attempt_categories", min: 1},
		{table: "tutoring_proposal_artifacts", column: "artifact", min: 1},
		{table: "tutoring_proposal_artifacts", column: "trusted_model_id", min: 1},
		{table: "tutoring_proposal_artifacts", column: "model_parameters", min: 1},
		{table: "tutoring_proposal_artifacts", column: "prompt_revision", min: 1},
		{table: "tutoring_proposal_artifacts", column: "attempt_categories", min: 1},
	},
	privacy.StoreInboxOutbox: {
		{table: "outbox_messages", column: "payload", min: 1},
		{table: "outbox_messages", column: "audit_metadata", min: 1},
		{table: "outbox_messages", column: "last_error_category", min: 1},
	},
	privacy.StoreProjectionGenerations: {
		{table: "learning_projection_timeline", column: "item", min: 2},
		{table: "learning_projection_routes", column: "item", min: 2},
		{table: "learning_projection_sessions", column: "item", min: 2},
		{table: "learning_projection_nodes", column: "item", min: 2},
		{table: "learning_projection_evidence", column: "item", min: 2},
		{table: "learning_projection_reviews", column: "item", min: 2},
		{table: "learning_projection_misconceptions", column: "item", min: 2},
		{table: "learning_projection_stats", column: "item", min: 2},
	},
	privacy.StoreMemoryCandidateDelivery: {
		{table: "memory_candidates", column: "reason", min: 2},
		{table: "memory_candidate_decisions", column: "reason", min: 1},
		{table: "memory_candidate_payloads", column: "content", min: 1},
		{table: "memory_delivery_payloads", column: "content", min: 1},
		{table: "memory_delivery_receipts", column: "reason", min: 1},
	},
}

func TestPostgreSQLCompletePrivacyMarkerFixtureAndReplay(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	fixture := seedPrivacyMarkerFixture(t, pool)
	assertPrivacyMarkerFixturePresent(t, pool, fixture.marker)
	before := capturePrivacyPreservedState(t, pool, fixture)

	manager := privacy.NewReadPermitManager()
	store := newPrivacyFixtureStore(pool, manager, "", "")
	now := time.Now().UTC()
	barrier, err := store.CommitBarrier(ctx, privacy.ErasureRequest{
		DeviceID: fixture.deviceID, OperationID: uuid.NewString(), ActorDeviceID: fixture.deviceID,
		ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now,
		ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if barrier.Status != privacy.StatusBarrierCommitted || barrier.LearnerGeneration != 2 || barrier.RedactedThroughEventSeq != 1 {
		t.Fatalf("barrier=%+v", barrier)
	}
	assertPrivacyMarkerFixturePresent(t, pool, fixture.marker)
	assertPrivacyStatusConstraintsRejectInvalid(t, pool, barrier.ErasureID)

	complete, err := store.RunLocalScrub(ctx, barrier.ErasureID)
	if err != nil {
		t.Fatal(err)
	}
	if complete.Status != privacy.StatusLocalScrubbed {
		t.Fatalf("local scrub receipt=%+v", complete)
	}
	assertNoMarkerInSchema(t, pool, fixture.marker)
	after := capturePrivacyPreservedState(t, pool, fixture)
	assertPrivacyPreservedStateEqual(t, before, after)
	assertPrivacyFixtureTombstones(t, pool, fixture, barrier)
	assertPostErasureReplayConvergence(t, pool, fixture)
}

type privacyFaultPhase string

const (
	privacyFaultBeforeMutation privacyFaultPhase = "before_owner_mutation"
	privacyFaultBeforeVerify   privacyFaultPhase = "after_scrub_commit_before_verify_receipt"
)

func TestPostgreSQLLocalPrivacyScrubFaultMatrix(t *testing.T) {
	stores := []privacy.StoreKind{
		privacy.StoreIdentityMetadata,
		privacy.StoreKnowledgeContent,
		privacy.StoreKnowledgeIndex,
		privacy.StoreKnowledgeArtifacts,
		privacy.StoreLearningEventPayload,
		privacy.StoreLearningTypedPayload,
		privacy.StoreTutoringPayload,
		privacy.StoreInboxOutbox,
		privacy.StoreProjectionGenerations,
		privacy.StoreMemoryCandidateDelivery,
	}
	assertPrivacyFaultMatrixCoverage(t, stores)
	for _, storeKind := range stores {
		for _, phase := range []privacyFaultPhase{privacyFaultBeforeMutation, privacyFaultBeforeVerify} {
			t.Run(string(storeKind)+"/"+string(phase), func(t *testing.T) {
				ctx := context.Background()
				pool := privacyIntegrationPool(t)
				fixture := seedPrivacyMarkerFixture(t, pool)
				assertPrivacyMarkerFixturePresent(t, pool, fixture.marker)

				manager := privacy.NewReadPermitManager()
				store := newPrivacyFixtureStore(pool, manager, storeKind, phase)
				now := time.Now().UTC()
				barrier, err := store.CommitBarrier(ctx, privacy.ErasureRequest{
					DeviceID: fixture.deviceID, OperationID: uuid.NewString(), ActorDeviceID: fixture.deviceID,
					ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now,
					ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1,
				})
				if err != nil {
					t.Fatal(err)
				}
				failedReceipt, err := store.RunLocalScrub(ctx, barrier.ErasureID)
				if err == nil || failedReceipt.Status != privacy.StatusBarrierCommitted {
					t.Fatalf("fault did not stop scrub receipt=%+v err=%v", failedReceipt, err)
				}
				assertBarrierAndGenerationPersisted(t, pool, barrier.ErasureID, barrier.LearnerGeneration)
				assertCurrentStepStatus(t, pool, barrier.ErasureID, storeKind, privacy.StepPending, 1)
				markerCount := markerCountForStore(t, pool, fixture.marker, storeKind)
				if phase == privacyFaultBeforeMutation && markerCount == 0 {
					t.Fatalf("%s marker disappeared before owner mutation", storeKind)
				}
				if phase == privacyFaultBeforeVerify && markerCount != 0 {
					t.Fatalf("%s scrub did not commit before verification fault; marker_count=%d", storeKind, markerCount)
				}

				completedHistory := currentCompletedReceiptHistory(t, pool, barrier.ErasureID)
				converged, err := store.RunLocalScrub(ctx, barrier.ErasureID)
				if err != nil {
					t.Fatalf("second scrub did not converge: %v", err)
				}
				if converged.Status != privacy.StatusLocalScrubbed {
					t.Fatalf("second scrub receipt=%+v", converged)
				}
				assertCompletedReceiptHistoryUnchanged(t, pool, barrier.ErasureID, completedHistory)
				assertCurrentStepStatus(t, pool, barrier.ErasureID, storeKind, privacy.StepSucceeded, 2)
				assertNoMarkerInSchema(t, pool, fixture.marker)
				assertLocalGatesAfterConvergence(t, pool, barrier.ErasureID, barrier.LearnerGeneration)

				if storeKind == privacy.StoreInboxOutbox && phase == privacyFaultBeforeVerify {
					assertPostErasureReplayConvergence(t, pool, fixture)
				}
			})
		}
	}
}

type verifyFaultOwner struct {
	privacy.LocalOwnerPort
	target privacy.StoreKind
	failed bool
}

func (o *verifyFaultOwner) VerifyRedacted(ctx context.Context, request privacy.LocalRedactionRequest) (int64, error) {
	if request.Store == o.target && !o.failed {
		o.failed = true
		return 0, errors.New("injected failure after scrub commit before verification receipt")
	}
	return o.LocalOwnerPort.VerifyRedacted(ctx, request)
}

func (o *verifyFaultOwner) AppendEventRedactedTx(ctx context.Context, db privacy.DBTX, request privacy.RedactionEventAppendRequest) (privacy.RedactionEventAppendResult, error) {
	appender, ok := o.LocalOwnerPort.(privacy.RedactionEventAppender)
	if !ok {
		return privacy.RedactionEventAppendResult{}, errors.New("wrapped owner does not append redaction events")
	}
	return appender.AppendEventRedactedTx(ctx, db, request)
}

func newPrivacyFixtureStore(pool *pgxpool.Pool, manager *privacy.ReadPermitManager, target privacy.StoreKind, phase privacyFaultPhase) *privacydb.Store {
	tutoringStore := tutoringdb.New(pool)
	owners := []privacy.LocalOwnerPort{
		identitydb.New(pool),
		knowledgedb.New(pool),
		learningdb.New(pool, tutoringStore),
		tutoringStore,
		memorydb.New(pool),
		outboxdb.New(pool),
	}
	if phase == privacyFaultBeforeVerify {
		ownerKind, _ := privacy.OwnerForStore(target)
		for index, owner := range owners {
			if owner.Owner() == ownerKind {
				owners[index] = &verifyFaultOwner{LocalOwnerPort: owner, target: target}
			}
		}
	}
	options := []privacydb.Option{privacydb.WithReadPermits(manager)}
	for _, owner := range owners {
		options = append(options, privacydb.WithLocalOwner(owner))
	}
	if phase == privacyFaultBeforeMutation {
		failed := false
		options = append(options, privacydb.WithBeforeStep(func(store privacy.StoreKind) error {
			if store == target && !failed {
				failed = true
				return errors.New("injected failure before owner mutation")
			}
			return nil
		}))
	}
	return privacydb.New(pool, options...)
}

func seedPrivacyMarkerFixture(t *testing.T, pool *pgxpool.Pool) privacyMarkerFixture {
	t.Helper()
	ctx := context.Background()
	fixture := privacyMarkerFixture{
		marker:   "privacy-marker-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		deviceID: uuid.NewString(),
		tokenID:  uuid.NewString(),
	}
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,$2,clock_timestamp())`, fixture.deviceID, fixture.marker); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO device_tokens(id,device_id,token_hash,scopes,created_at,last_used_at)
		VALUES($1,$2,$3,ARRAY['learning:read','learning:write','memory:read','memory:write','privacy:read'],clock_timestamp(),clock_timestamp())`,
		fixture.tokenID, fixture.deviceID, hashBytes("token:"+fixture.marker)); err != nil {
		t.Fatal(err)
	}

	knowledgeStore := knowledgedb.New(pool)
	knowledgeService, err := knowledge.NewService(knowledgeStore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	knowledgeResult, err := knowledgeService.Import(ctx, knowledge.ImportCommand{
		OperationID: uuid.NewString(), ExpectedParentProvided: true,
		Source: fixture.marker,
		Documents: []knowledge.ImportDocument{{
			Path:     fixture.marker + ".md",
			Markdown: "# " + fixture.marker + "\n## child " + fixture.marker + "\nbody " + fixture.marker + "\n",
		}},
		ActorDeviceID: fixture.deviceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.knowledgeRevisionID = knowledgeResult.Revision.ID
	document := knowledgeResult.Revision.Documents[0].Revision
	fixture.documentRevisionID = document.ID
	for _, node := range document.Nodes {
		if strings.Contains(node.Title, "child "+fixture.marker) {
			fixture.nodeID = node.NodeID
			fixture.nodeRevisionID = node.ID
			break
		}
	}
	if fixture.nodeRevisionID == "" {
		t.Fatalf("knowledge fixture did not create marker child node: %+v", document.Nodes)
	}
	fixture.lineageID, fixture.artifactID = uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO knowledge_lineages(id,knowledge_revision_id,action,actor_device_id,reason,policy_version,created_at)
		VALUES($1,$2,'rewrite',$3,$4,'fixture-v1',clock_timestamp())`,
		fixture.lineageID, fixture.knowledgeRevisionID, fixture.deviceID, fixture.marker); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO knowledge_lineage_members(lineage_id,role,node_revision_id,ordinal)
		VALUES($1,'source',$2,0),($1,'target',$2,0)`, fixture.lineageID, fixture.nodeRevisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO knowledge_node_artifacts(
			id,node_revision_id,kind,producer_version,prompt_version,model_version,input_hash,content,status,created_at)
		VALUES($1,$2,'summary','producer-v1','prompt-v1','model-v1',$3,$4,'ready',clock_timestamp())`,
		fixture.artifactID, fixture.nodeRevisionID, hashBytes("artifact:"+fixture.marker), fixture.marker); err != nil {
		t.Fatal(err)
	}

	seedLearningTutoringMarkerFixture(t, pool, &fixture)
	seedProjectionMarkerFixture(t, pool, &fixture)
	fixture.outboxID = uuid.NewString()
	markerJSON := json.RawMessage(`{"marker":"` + fixture.marker + `"}`)
	if _, err := pool.Exec(ctx, `
		INSERT INTO outbox_messages(
			id,business_type,aggregate_id,idempotency_key,revision,generation,payload,audit_metadata,
			status,available_at,attempts,max_attempts,last_error_category,last_error_at,created_at,updated_at)
		VALUES($1,'privacy.fixture',$2,$3,1,1,$4,$4,'dead',clock_timestamp(),3,3,$5,clock_timestamp(),clock_timestamp(),clock_timestamp())`,
		fixture.outboxID, fixture.outboxID, "privacy-fixture:"+uuid.NewString(), markerJSON, fixture.marker); err != nil {
		t.Fatal(err)
	}
	seedMemoryMarkerFixture(t, pool, &fixture)
	return fixture
}

func seedLearningTutoringMarkerFixture(t *testing.T, pool *pgxpool.Pool, fixture *privacyMarkerFixture) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	fixture.goalID, fixture.goalRevisionID = uuid.NewString(), uuid.NewString()
	fixture.routeID, fixture.routeRevisionID, fixture.routeStepID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	fixture.sessionID, fixture.focusFrameID = uuid.NewString(), uuid.NewString()
	fixture.freeQuestionID, fixture.freeAnswerID = uuid.NewString(), uuid.NewString()
	fixture.proposalRequestID, fixture.proposalArtifactID = uuid.NewString(), uuid.NewString()
	fixture.activityID, fixture.attemptPayloadID, fixture.attemptID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	fixture.assessmentID, fixture.assessmentDecisionID = uuid.NewString(), uuid.NewString()
	fixture.evidenceID, fixture.invalidationID = uuid.NewString(), uuid.NewString()
	fixture.exposureID, fixture.misconceptionID = uuid.NewString(), uuid.NewString()
	fixture.learningEventID, fixture.learningPayloadID, fixture.learningOperationID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	markerJSON, err := json.Marshal(map[string]any{"marker": fixture.marker})
	if err != nil {
		t.Fatal(err)
	}
	markerHash := hashBytes(fixture.marker)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	mustExecPrivacyFixture(t, tx, `SET CONSTRAINTS ALL DEFERRED`)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_goal_revisions(id,goal_id,revision,goal_text,source,actor_device_id,created_at)
		VALUES($1,$2,1,$3,$3,$4,$5)`, fixture.goalRevisionID, fixture.goalID, fixture.marker, fixture.deviceID, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_route_revisions(id,route_id,revision,goal_revision_id,knowledge_revision_id,route_policy_version,created_at)
		VALUES($1,$2,1,$3,$4,$5,$6)`, fixture.routeRevisionID, fixture.routeID, fixture.goalRevisionID, fixture.knowledgeRevisionID, learning.RoutePolicyVersion, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_route_steps(
			id,route_revision_id,ordinal,knowledge_revision_id,node_id,node_revision_id,document_revision_id,
			teaching_intent,completion_condition)
		VALUES($1,$2,0,$3,$4,$5,$6,$7,$7)`, fixture.routeStepID, fixture.routeRevisionID,
		fixture.knowledgeRevisionID, fixture.nodeID, fixture.nodeRevisionID, fixture.documentRevisionID, fixture.marker)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO tutoring_sessions(
			id,aggregate_version,state,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,
			focus_node_revision_id,activity_id,attempt_id,attached_quiz,started_at,updated_at)
		VALUES($1,1,'FreeAnswer',$2,$3,$4,$5,$6,$7,$8,TRUE,$9,$9)`, fixture.sessionID,
		fixture.goalRevisionID, fixture.routeRevisionID, fixture.routeStepID, fixture.knowledgeRevisionID,
		fixture.nodeRevisionID, fixture.activityID, fixture.attemptID, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO tutoring_focus_frames(
			id,session_id,saved_state,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,
			focus_node_revision_id,activity_id,attempt_id,saved_aggregate_version,created_event_seq,invalidation_reason)
		VALUES($1,$2,'RouteActive',$3,$4,$5,$6,$7,$8,$9,1,1,$10)`, fixture.focusFrameID, fixture.sessionID,
		fixture.goalRevisionID, fixture.routeRevisionID, fixture.routeStepID, fixture.knowledgeRevisionID,
		fixture.nodeRevisionID, fixture.activityID, fixture.attemptID, fixture.marker)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO tutoring_free_questions(
			id,session_id,focus_frame_id,question_text,knowledge_revision_id,references_snapshot,actor_device_id,received_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, fixture.freeQuestionID, fixture.sessionID, fixture.focusFrameID,
		fixture.marker, fixture.knowledgeRevisionID, markerJSON, fixture.deviceID, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO tutoring_proposal_requests(
			device_id,request_id,request_hash,proposal_type,aggregate_type,aggregate_id,aggregate_version,input,
			status,attempt_categories,result_proposal_id,created_at,updated_at)
		VALUES($1,$2,$3,'free_answer','session',$4,1,$5,'ready',$6,$7,$8,$8)`, fixture.deviceID,
		fixture.proposalRequestID, markerHash, fixture.sessionID, markerJSON, []string{fixture.marker}, fixture.proposalArtifactID, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO tutoring_proposal_artifacts(
			id,device_id,request_id,schema_version,input_hash,proposal_type,aggregate_type,aggregate_id,aggregate_version,
			goal_revision_id,route_revision_id,activity_id,attempt_id,knowledge_revision_id,artifact,trusted_model_id,
			model_parameters,prompt_revision,attempt_categories,created_at)
		VALUES($1,$2,$3,1,$4,'free_answer','session',$5,1,$6,$7,$8,$9,$10,$11,$12,$11,$12,$13,$14)`,
		fixture.proposalArtifactID, fixture.deviceID, fixture.proposalRequestID, markerHash, fixture.sessionID,
		fixture.goalRevisionID, fixture.routeRevisionID, fixture.activityID, fixture.attemptID, fixture.knowledgeRevisionID,
		markerJSON, fixture.marker, []string{fixture.marker}, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO tutoring_free_answers(
			id,session_id,focus_frame_id,free_question_id,answer_text,knowledge_revision_id,references_snapshot,source_proposal_id,received_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, fixture.freeAnswerID, fixture.sessionID, fixture.focusFrameID,
		fixture.freeQuestionID, fixture.marker, fixture.knowledgeRevisionID, markerJSON, fixture.proposalArtifactID, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_activities(
			id,revision,session_id,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,target_node_id,
			target_node_revision_id,prompt,activity_type,rubric_revision,rubric,difficulty,allowed_help,
			activity_policy_version,assessment_policy_version,review_policy_version,source_proposal_id,
			attached_free_question_id,attached_free_answer_id,is_review,created_at)
		VALUES($1,1,$2,$3,$4,$5,$6,$7,$8,$9,'objective',$9,$10,1,ARRAY['none'],$11,$12,$13,$14,$15,$16,FALSE,$17)`,
		fixture.activityID, fixture.sessionID, fixture.goalRevisionID, fixture.routeRevisionID, fixture.routeStepID,
		fixture.knowledgeRevisionID, fixture.nodeID, fixture.nodeRevisionID, fixture.marker, markerJSON,
		learning.ActivityPolicyVersion, learning.AssessmentPolicyVersion, learning.ReviewPolicyVersion,
		fixture.proposalArtifactID, fixture.freeQuestionID, fixture.freeAnswerID, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_activity_references(
			activity_id,ordinal,knowledge_revision_id,node_id,node_revision_id,document_revision_id,
			source_start,source_end,slice_text,slice_hash)
		VALUES($1,0,$2,$3,$4,$5,0,1,$6,$7)`, fixture.activityID, fixture.knowledgeRevisionID,
		fixture.nodeID, fixture.nodeRevisionID, fixture.documentRevisionID, fixture.marker, markerHash)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_attempt_payloads(id,answer_text,payload_hash,created_at) VALUES($1,$2,$3,$4)`,
		fixture.attemptPayloadID, fixture.marker, markerHash, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_attempts(
			id,session_id,activity_id,activity_revision,answer_payload_id,help_level,actor_device_id,received_at,payload_hash)
		VALUES($1,$2,$3,1,$4,'none',$5,$6,$7)`, fixture.attemptID, fixture.sessionID, fixture.activityID,
		fixture.attemptPayloadID, fixture.deviceID, now, markerHash)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_assessments(
			id,session_id,attempt_id,activity_id,activity_revision,rubric_complete,confidence,risk_flags,
			trusted_model_id,model_parameters,prompt_revision,proposal_input_hash,model_attempts,attempt_categories,created_at)
		VALUES($1,$2,$3,$4,1,TRUE,900,$5,$6,$7,$6,$8,1,$5,$9)`, fixture.assessmentID,
		fixture.sessionID, fixture.attemptID, fixture.activityID, []string{fixture.marker}, fixture.marker,
		markerJSON, markerHash, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_assessment_items(
			assessment_id,ordinal,rubric_item_id,conclusion,answer_start,answer_end,answer_quote,answer_quote_hash,
			knowledge_revision_id,knowledge_node_revision_id,knowledge_node_id,knowledge_document_revision_id,
			knowledge_start,knowledge_end,knowledge_quote,knowledge_quote_hash,misconception_candidate)
		VALUES($1,0,$2,'pass',0,1,$2,$3,$4,$5,$6,$7,0,1,$2,$3,$2)`, fixture.assessmentID,
		fixture.marker, markerHash, fixture.knowledgeRevisionID, fixture.nodeRevisionID, fixture.nodeID, fixture.documentRevisionID)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_assessment_decisions(
			id,assessment_id,version,disposition,conclusions,reason,actor_device_id,created_at)
		VALUES($1,$2,1,'accepted',$3,$4,$5,$6)`, fixture.assessmentDecisionID, fixture.assessmentID,
		markerJSON, fixture.marker, fixture.deviceID, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_evidence(
			id,decision_id,assessment_id,session_id,attempt_id,activity_id,activity_revision,goal_revision_id,
			route_revision_id,knowledge_revision_id,node_revision_id,node_id,document_revision_id,rubric_revision,
			evidence_kind,activity_type,outcome,help_level,received_at,acceptance_policy_version,reducer_policy_version,
			review_policy_version,misconception_candidates,rubric_outcomes)
		VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$9,$10,$11,$12,$13,'practice_recall','objective','pass','none',$14,$15,$16,$17,$18,$18)`,
		fixture.evidenceID, fixture.assessmentDecisionID, fixture.assessmentID, fixture.sessionID, fixture.attemptID,
		fixture.activityID, fixture.goalRevisionID, fixture.routeRevisionID, fixture.knowledgeRevisionID,
		fixture.nodeRevisionID, fixture.nodeID, fixture.documentRevisionID, fixture.marker, now,
		learning.AssessmentPolicyVersion, learning.MasteryReducerVersion, learning.ReviewPolicyVersion, markerJSON)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_evidence_invalidations(id,evidence_id,decision_id,reason,event_seq,created_at)
		VALUES($1,$2,$3,$4,1,$5)`, fixture.invalidationID, fixture.evidenceID, fixture.assessmentDecisionID, fixture.marker, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_exposures(id,session_id,exposure_kind,content,references_snapshot,source_proposal_id,received_at)
		VALUES($1,$2,'reading',$3,$4,$5,$6)`, fixture.exposureID, fixture.sessionID, fixture.marker,
		markerJSON, fixture.proposalArtifactID, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_misconception_revisions(
			id,misconception_id,revision,node_revision_id,rubric_item_id,candidate_hash,candidate_text,status,
			source_evidence_ids,counter_evidence_ids,caused_by_evidence_id,created_at)
		VALUES($1,$2,1,$3,$4,$5,$4,'proposed',ARRAY[$6]::uuid[],'{}'::uuid[],$6,$7)`,
		uuid.NewString(), fixture.misconceptionID, fixture.nodeRevisionID, fixture.marker, markerHash, fixture.evidenceID, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_aggregate_heads(aggregate_type,aggregate_id,aggregate_version,last_event_seq,updated_at)
		VALUES('goal',$1,1,1,$2)`, fixture.goalID, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_event_payloads(id,payload,payload_hash,created_at) VALUES($1,$2,$3,$4)`,
		fixture.learningPayloadID, markerJSON, hashBytes(string(markerJSON)), now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_events(
			event_seq,id,event_type,event_schema_version,aggregate_type,aggregate_id,aggregate_version,
			device_id,operation_id,operation_ordinal,received_at,payload_id,payload_hash)
		VALUES(1,$1,'GoalRevisionCreated',1,'goal',$2,1,$3,$4,0,$5,$6,$7)`, fixture.learningEventID,
		fixture.goalID, fixture.deviceID, fixture.learningOperationID, now, fixture.learningPayloadID, hashBytes(string(markerJSON)))
	mustExecPrivacyFixture(t, tx, `UPDATE learning_event_clock SET current_event_seq=1,updated_at=$1 WHERE singleton_id=1`, now)
	mustExecPrivacyFixture(t, tx, `
		INSERT INTO learning_inbox(
			device_id,operation_id,request_hash,aggregate_type,aggregate_id,terminal_status,result,
			first_event_seq,last_event_seq,completed_at)
		VALUES($1,$2,$3,'goal',$4,'succeeded',$5,1,1,$6)`, fixture.deviceID, fixture.learningOperationID,
		markerHash, fixture.goalID, markerJSON, now)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func seedProjectionMarkerFixture(t *testing.T, pool *pgxpool.Pool, fixture *privacyMarkerFixture) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `SELECT active_generation_id::text FROM learning_projection_head WHERE singleton_id=1`).Scan(&fixture.oldActiveProjectionID); err != nil {
		t.Fatal(err)
	}
	fixture.oldRetiredProjectionID = uuid.NewString()
	now := time.Now().UTC()
	retiredFingerprint := hashBytes("retired:" + fixture.marker)
	if _, err := pool.Exec(ctx, `
		INSERT INTO learning_projection_generations(
			id,projection_version,reducer_version,assessment_policy_version,review_policy_version,status,
			target_high_water,checkpoint_event_seq,fingerprint,incomplete,reason_codes,created_at,completed_at)
		VALUES($1,$2,$3,$4,$5,'retired',1,1,$6,FALSE,'{}'::text[],$7,$7)`,
		fixture.oldRetiredProjectionID, learning.ProjectionVersion, learning.MasteryReducerVersion,
		learning.AssessmentPolicyVersion, learning.ReviewPolicyVersion, retiredFingerprint, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO learning_projection_checkpoints(generation_id,event_seq,fingerprint,updated_at)
		VALUES($1,1,$2,$3)`, fixture.oldRetiredProjectionID, retiredFingerprint, now); err != nil {
		t.Fatal(err)
	}
	item, err := json.Marshal(map[string]any{"marker": fixture.marker, "focus_frame_id": fixture.focusFrameID})
	if err != nil {
		t.Fatal(err)
	}
	for index, generationID := range []string{fixture.oldActiveProjectionID, fixture.oldRetiredProjectionID} {
		sequence := int64(100 + index)
		mustExecPrivacyPool(t, pool, `INSERT INTO learning_projection_timeline(generation_id,event_seq,event_id,item) VALUES($1,$2,$3,$4)`, generationID, sequence, uuid.NewString(), item)
		mustExecPrivacyPool(t, pool, `INSERT INTO learning_projection_routes(generation_id,route_revision_id,route_id,revision,event_seq,is_current,item) VALUES($1,$2,$3,1,$4,TRUE,$5)`, generationID, fixture.routeRevisionID, fixture.routeID, sequence, item)
		mustExecPrivacyPool(t, pool, `INSERT INTO learning_projection_sessions(generation_id,session_id,updated_event_seq,item) VALUES($1,$2,$3,$4)`, generationID, fixture.sessionID, sequence, item)
		mustExecPrivacyPool(t, pool, `INSERT INTO learning_projection_nodes(generation_id,node_revision_id,updated_event_seq,item) VALUES($1,$2,$3,$4)`, generationID, fixture.nodeRevisionID, sequence, item)
		mustExecPrivacyPool(t, pool, `INSERT INTO learning_projection_evidence(generation_id,evidence_id,node_revision_id,received_at,item) VALUES($1,$2,$3,$4,$5)`, generationID, fixture.evidenceID, fixture.nodeRevisionID, now, item)
		mustExecPrivacyPool(t, pool, `INSERT INTO learning_projection_reviews(generation_id,node_revision_id,due_at,stable_id,item) VALUES($1,$2,$3,$4,$5)`, generationID, fixture.nodeRevisionID, now, uuid.NewString(), item)
		mustExecPrivacyPool(t, pool, `INSERT INTO learning_projection_misconceptions(generation_id,misconception_id,node_revision_id,item) VALUES($1,$2,$3,$4)`, generationID, fixture.misconceptionID, fixture.nodeRevisionID, item)
		mustExecPrivacyPool(t, pool, `INSERT INTO learning_projection_stats(generation_id,session_id,item) VALUES($1,$2,$3)`, generationID, fixture.sessionID, item)
	}
}

func seedMemoryMarkerFixture(t *testing.T, pool *pgxpool.Pool, fixture *privacyMarkerFixture) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	store := memorydb.New(pool)
	service, err := memory.NewService(store, memory.ServiceOptions{
		Now: func() time.Time { return now }, ReadPermits: privacy.NewReadPermitManager(), DeliveryTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := service.CreateCandidate(ctx, memory.DevicePrincipal{DeviceID: fixture.deviceID}, memory.CreateCandidateCommand{
		OperationID: uuid.NewString(), Content: "background " + fixture.marker, Reason: fixture.marker,
		Category: memory.CategoryPersonalContext, Sensitivity: memory.SensitivityNonSensitive,
		Stability: memory.StabilityStable, ValidUntil: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.pendingCandidateID = pending.Candidate.Candidate.ID
	created, err := service.CreateCandidate(ctx, memory.DevicePrincipal{DeviceID: fixture.deviceID}, memory.CreateCandidateCommand{
		OperationID: uuid.NewString(), Content: "I prefer concise examples " + fixture.marker, Reason: fixture.marker,
		Category: memory.CategoryInteractionPreference, Sensitivity: memory.SensitivitySensitive,
		Stability: memory.StabilityStable, ValidUntil: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.admittedCandidateID = created.Candidate.Candidate.ID
	decided, err := service.DecideCandidate(ctx, memory.DevicePrincipal{DeviceID: fixture.deviceID}, memory.DecideCandidateCommand{
		OperationID: uuid.NewString(), CandidateID: fixture.admittedCandidateID, ExpectedRevision: 1,
		Decision: memory.DecisionAdmit, Reason: fixture.marker,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decided.Record == nil || decided.Delivery == nil {
		t.Fatalf("manual admission did not create record/delivery: %+v", decided)
	}
	fixture.logicalMemoryID = decided.Record.LogicalMemoryID
	fixture.recordRevisionID = decided.Record.ID
	fixture.deliveryID = decided.Delivery.ID
	attempt, err := store.ClaimAttempt(ctx, fixture.deliveryID, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fixture.memoryAttemptID = attempt.ID
	if _, err := store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: "fixture-boot-v1", At: now,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.memoryReceiptID = uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_delivery_receipts(
			id,delivery_id,version,status,reason,verification_method,evidence_digest,created_at)
		VALUES($1,$2,2,'pending',$3,'uri_hash_or_absence',$4,$5)`,
		fixture.memoryReceiptID, fixture.deliveryID, fixture.marker, hashBytes("receipt:"+fixture.marker), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE memory_delivery_heads
		SET current_receipt_id=$1,current_receipt_version=2,updated_at=$3
		WHERE delivery_id=$2`, fixture.memoryReceiptID, fixture.deliveryID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE memory_record_heads SET receipt_id=$1,updated_at=$3 WHERE logical_memory_id=$2`,
		fixture.memoryReceiptID, fixture.logicalMemoryID, now); err != nil {
		t.Fatal(err)
	}
}

func mustExecPrivacyFixture(t *testing.T, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("privacy fixture SQL failed: %v\n%s", err, sql)
	}
}

func mustExecPrivacyPool(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("privacy fixture SQL failed: %v\n%s", err, sql)
	}
}

func hashBytes(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return append([]byte(nil), sum[:]...)
}

func assertPrivacyMarkerFixturePresent(t *testing.T, pool *pgxpool.Pool, marker string) {
	t.Helper()
	var total int64
	for _, store := range []privacy.StoreKind{
		privacy.StoreIdentityMetadata, privacy.StoreKnowledgeContent, privacy.StoreKnowledgeIndex,
		privacy.StoreKnowledgeArtifacts, privacy.StoreLearningEventPayload, privacy.StoreLearningTypedPayload,
		privacy.StoreTutoringPayload, privacy.StoreInboxOutbox, privacy.StoreProjectionGenerations,
		privacy.StoreMemoryCandidateDelivery,
	} {
		for _, location := range privacyMarkerLocationsByStore[store] {
			count := markerCountAtLocation(t, pool, marker, location)
			if count < location.min {
				t.Fatalf("pre-scrub marker missing at %s.%s: count=%d want>=%d", location.table, location.column, count, location.min)
			}
			total += count
		}
	}
	if total == 0 {
		t.Fatal("pre-scrub marker fixture is empty")
	}
}

func markerCountForStore(t *testing.T, pool *pgxpool.Pool, marker string, store privacy.StoreKind) int64 {
	t.Helper()
	var total int64
	for _, location := range privacyMarkerLocationsByStore[store] {
		total += markerCountAtLocation(t, pool, marker, location)
	}
	return total
}

func markerCountAtLocation(t *testing.T, pool *pgxpool.Pool, marker string, location markerLocation) int64 {
	t.Helper()
	var dataType, udtName string
	if err := pool.QueryRow(context.Background(), `
		SELECT data_type,udt_name FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name=$1 AND column_name=$2`,
		location.table, location.column).Scan(&dataType, &udtName); err != nil {
		t.Fatalf("inspect %s.%s: %v", location.table, location.column, err)
	}
	column := pgx.Identifier{location.column}.Sanitize()
	table := pgx.Identifier{location.table}.Sanitize()
	var predicate string
	switch {
	case dataType == "text" || dataType == "character varying" || dataType == "json" || dataType == "jsonb":
		predicate = "position($1 in COALESCE(" + column + "::text,''))>0"
	case dataType == "ARRAY" && udtName == "_text":
		predicate = "position($1 in COALESCE(array_to_string(" + column + ",E'\\n'),''))>0"
	case dataType == "bytea":
		predicate = "position(convert_to($1,'UTF8') in COALESCE(" + column + ",''::bytea))>0"
	default:
		t.Fatalf("unsupported marker column %s.%s type=%s udt=%s", location.table, location.column, dataType, udtName)
	}
	var count int64
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE "+predicate, marker).Scan(&count); err != nil {
		t.Fatalf("scan marker at %s.%s: %v", location.table, location.column, err)
	}
	return count
}

func assertNoMarkerInSchema(t *testing.T, pool *pgxpool.Pool, marker string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT c.table_name,c.column_name,c.data_type,c.udt_name
		FROM information_schema.columns c
		JOIN pg_catalog.pg_tables t ON t.schemaname=c.table_schema AND t.tablename=c.table_name
		WHERE c.table_schema=current_schema()
		  AND (c.data_type IN ('text','character varying','json','jsonb','bytea')
		       OR (c.data_type='ARRAY' AND c.udt_name='_text'))
		ORDER BY c.table_name,c.ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	type column struct{ table, name, dataType, udtName string }
	var columns []column
	for rows.Next() {
		var value column
		if err := rows.Scan(&value.table, &value.name, &value.dataType, &value.udtName); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		columns = append(columns, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	var residual []string
	for _, value := range columns {
		count := markerCountAtLocation(t, pool, marker, markerLocation{table: value.table, column: value.name})
		if count != 0 {
			residual = append(residual, fmt.Sprintf("%s.%s=%d", value.table, value.name, count))
		}
	}
	if len(residual) != 0 {
		t.Fatalf("post-scrub marker remains in schema: %s", strings.Join(residual, ", "))
	}
}

func capturePrivacyPreservedState(t *testing.T, pool *pgxpool.Pool, fixture privacyMarkerFixture) map[string]string {
	t.Helper()
	queries := map[string]struct {
		sql  string
		args []any
	}{
		"identity": {
			sql:  `SELECT d.id::text||':'||t.id::text||':'||encode(t.token_hash,'hex') FROM devices d JOIN device_tokens t ON t.device_id=d.id WHERE d.id=$1`,
			args: []any{fixture.deviceID},
		},
		"knowledge": {
			sql:  `SELECT r.id::text||':'||encode(r.manifest_hash,'hex')||':'||d.id::text||':'||encode(d.canonical_hash,'hex')||':'||encode(d.semantic_hash,'hex')||':'||n.id::text||':'||n.node_id::text||':'||n.heading_start_line::text||':'||n.section_end_line::text||':'||encode(n.semantic_local_body_hash,'hex') FROM knowledge_revisions r JOIN knowledge_snapshot_documents s ON s.knowledge_revision_id=r.id JOIN knowledge_document_revisions d ON d.id=s.document_revision_id JOIN knowledge_node_revisions n ON n.id=$2 WHERE r.id=$1`,
			args: []any{fixture.knowledgeRevisionID, fixture.nodeRevisionID},
		},
		"learning_event_header": {
			sql:  `SELECT e.event_seq::text||':'||e.id::text||':'||e.event_type||':'||e.event_schema_version::text||':'||e.payload_id::text||':'||encode(e.payload_hash,'hex') FROM learning_events e WHERE e.id=$1`,
			args: []any{fixture.learningEventID},
		},
		"learning_typed_ids": {
			sql: `SELECT string_agg(kind||':'||id,',' ORDER BY kind,id) FROM (VALUES ('goal',$1::text),('route',$2::text),('step',$3::text),('session',$4::text),('focus',$5::text),('activity',$6::text),('attempt',$7::text),('assessment',$8::text),('decision',$9::text),('evidence',$10::text),('invalidation',$11::text),('exposure',$12::text),('misconception',$13::text)) ids(kind,id)`,
			args: []any{fixture.goalRevisionID, fixture.routeRevisionID, fixture.routeStepID, fixture.sessionID,
				fixture.focusFrameID, fixture.activityID, fixture.attemptID, fixture.assessmentID,
				fixture.assessmentDecisionID, fixture.evidenceID, fixture.invalidationID, fixture.exposureID, fixture.misconceptionID},
		},
		"memory": {
			sql:  `SELECT string_agg(c.id::text||':'||encode(c.content_hash,'hex'),',' ORDER BY c.id)||':'||r.id::text||':'||r.logical_memory_id::text||':'||encode(r.content_hash,'hex')||':'||d.id::text||':'||encode(d.payload_hash,'hex') FROM memory_candidates c CROSS JOIN memory_record_revisions r JOIN memory_deliveries d ON d.id=r.delivery_id WHERE c.id=ANY($1::uuid[]) AND r.id=$2 GROUP BY r.id,r.logical_memory_id,r.content_hash,d.id,d.payload_hash`,
			args: []any{[]string{fixture.pendingCandidateID, fixture.admittedCandidateID}, fixture.recordRevisionID},
		},
		"projection_generations": {
			sql:  `SELECT string_agg(id::text,',' ORDER BY id) FROM learning_projection_generations WHERE id=ANY($1::uuid[])`,
			args: []any{[]string{fixture.oldActiveProjectionID, fixture.oldRetiredProjectionID}},
		},
	}
	result := make(map[string]string, len(queries))
	for name, query := range queries {
		var value string
		if err := pool.QueryRow(context.Background(), query.sql, query.args...).Scan(&value); err != nil {
			t.Fatalf("capture preserved %s: %v", name, err)
		}
		if value == "" {
			t.Fatalf("capture preserved %s was empty", name)
		}
		result[name] = value
	}
	return result
}

func assertPrivacyPreservedStateEqual(t *testing.T, before, after map[string]string) {
	t.Helper()
	for name, beforeValue := range before {
		if after[name] != beforeValue {
			t.Errorf("preserved %s changed:\nbefore=%s\nafter=%s", name, beforeValue, after[name])
		}
	}
}

func assertPrivacyFixtureTombstones(t *testing.T, pool *pgxpool.Pool, fixture privacyMarkerFixture, barrier privacy.ErasureReceipt) {
	t.Helper()
	ctx := context.Background()
	var eventPayload, inboxResult string
	var eventRedacted bool
	if err := pool.QueryRow(ctx, `
		SELECT p.payload::text,p.redacted_at IS NOT NULL,i.result::text
		FROM learning_events e
		JOIN learning_event_payloads p ON p.id=e.payload_id
		JOIN learning_inbox i ON i.device_id=e.device_id AND i.operation_id=e.operation_id
		WHERE e.id=$1`, fixture.learningEventID).Scan(&eventPayload, &eventRedacted, &inboxResult); err != nil {
		t.Fatal(err)
	}
	if eventPayload != `{"redacted": true}` || !eventRedacted || inboxResult != `{"redacted": true}` {
		t.Fatalf("learning payload tombstones event=%q redacted=%v inbox=%q", eventPayload, eventRedacted, inboxResult)
	}
	var candidatePayloads, deliveryPayloads, badOutbox int64
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memory_candidate_payloads),
		  (SELECT count(*) FROM memory_delivery_payloads),
		  (SELECT count(*) FROM outbox_messages WHERE generation<2 AND (
		    status<>'canceled' OR terminal_disposition IS DISTINCT FROM 'privacy_erasure'
		    OR payload<>'{"redacted":true}'::jsonb OR audit_metadata<>'{}'::jsonb
		    OR lease_token IS NOT NULL OR lease_expires_at IS NOT NULL))`).Scan(&candidatePayloads, &deliveryPayloads, &badOutbox); err != nil {
		t.Fatal(err)
	}
	if candidatePayloads != 0 || deliveryPayloads != 0 || badOutbox != 0 {
		t.Fatalf("payload cleanup candidate=%d delivery=%d bad_outbox=%d", candidatePayloads, deliveryPayloads, badOutbox)
	}
	var pendingStatus string
	var pendingRevision int64
	var pendingAvailable bool
	if err := pool.QueryRow(ctx, `SELECT status,revision,payload_available FROM memory_candidate_heads WHERE candidate_id=$1`, fixture.pendingCandidateID).Scan(&pendingStatus, &pendingRevision, &pendingAvailable); err != nil {
		t.Fatal(err)
	}
	if pendingStatus != "expired" || pendingRevision != 2 || pendingAvailable {
		t.Fatalf("pending candidate scrub status=%s revision=%d available=%v", pendingStatus, pendingRevision, pendingAvailable)
	}
	var receiptReason, deliveryStatus, recordStatus string
	if err := pool.QueryRow(ctx, `
		SELECT r.reason,h.status,rh.status
		FROM memory_delivery_heads h
		JOIN memory_delivery_receipts r ON r.id=h.current_receipt_id
		JOIN memory_record_heads rh ON rh.current_delivery_id=h.delivery_id
		WHERE h.delivery_id=$1`, fixture.deliveryID).Scan(&receiptReason, &deliveryStatus, &recordStatus); err != nil {
		t.Fatal(err)
	}
	if receiptReason != "[redacted]" || deliveryStatus != "fenced" || recordStatus != "delete_pending" {
		t.Fatalf("memory tombstone receipt=%q delivery=%q record=%q", receiptReason, deliveryStatus, recordStatus)
	}
	var activeGenerations, activeTimeline, activeOther, retiredContent int64
	if err := pool.QueryRow(ctx, `
		WITH active AS (
		  SELECT id FROM learning_projection_generations WHERE status='active'
		), retired AS (
		  SELECT id FROM learning_projection_generations WHERE status='retired'
		)
		SELECT
		  (SELECT count(*) FROM active),
		  (SELECT count(*) FROM learning_projection_timeline t JOIN active a ON a.id=t.generation_id
		    WHERE t.item->>'event_type'='EventRedacted' AND t.item->>'aggregate_id'=$1),
		  (SELECT count(*) FROM learning_projection_routes r JOIN active a ON a.id=r.generation_id)+
		  (SELECT count(*) FROM learning_projection_sessions s JOIN active a ON a.id=s.generation_id)+
		  (SELECT count(*) FROM learning_projection_nodes n JOIN active a ON a.id=n.generation_id)+
		  (SELECT count(*) FROM learning_projection_evidence e JOIN active a ON a.id=e.generation_id)+
		  (SELECT count(*) FROM learning_projection_reviews r JOIN active a ON a.id=r.generation_id)+
		  (SELECT count(*) FROM learning_projection_misconceptions m JOIN active a ON a.id=m.generation_id)+
		  (SELECT count(*) FROM learning_projection_stats s JOIN active a ON a.id=s.generation_id),
		  (SELECT count(*) FROM learning_projection_timeline t JOIN retired r ON r.id=t.generation_id)+
		  (SELECT count(*) FROM learning_projection_routes x JOIN retired r ON r.id=x.generation_id)+
		  (SELECT count(*) FROM learning_projection_sessions x JOIN retired r ON r.id=x.generation_id)+
		  (SELECT count(*) FROM learning_projection_nodes x JOIN retired r ON r.id=x.generation_id)+
		  (SELECT count(*) FROM learning_projection_evidence x JOIN retired r ON r.id=x.generation_id)+
		  (SELECT count(*) FROM learning_projection_reviews x JOIN retired r ON r.id=x.generation_id)+
		  (SELECT count(*) FROM learning_projection_misconceptions x JOIN retired r ON r.id=x.generation_id)+
		  (SELECT count(*) FROM learning_projection_stats x JOIN retired r ON r.id=x.generation_id)`, barrier.ErasureID).Scan(
		&activeGenerations, &activeTimeline, &activeOther, &retiredContent); err != nil {
		t.Fatal(err)
	}
	if activeGenerations != 1 || activeTimeline != 1 || activeOther != 0 || retiredContent != 0 {
		t.Fatalf("projection tombstone active=%d redaction_timeline=%d active_other=%d retired_content=%d", activeGenerations, activeTimeline, activeOther, retiredContent)
	}
	var oldGenerations, targetGates int64
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM learning_projection_generations WHERE id=ANY($1::uuid[])),
		  (SELECT count(*) FROM privacy_owner_generation_gates WHERE learner_generation=$2)`,
		[]string{fixture.oldActiveProjectionID, fixture.oldRetiredProjectionID}, barrier.LearnerGeneration).Scan(&oldGenerations, &targetGates); err != nil {
		t.Fatal(err)
	}
	if oldGenerations != 2 || targetGates != int64(len(privacy.AllOwners)) {
		t.Fatalf("preserved generations old=%d target_gates=%d", oldGenerations, targetGates)
	}
	var invalidWording int64
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM privacy_erasure_heads
		   WHERE erasure_id=$1 AND (
		     btrim(stable_reason)='' OR
		     replace(replace(lower(stable_reason),'_',' '),'-',' ') LIKE '%secure erase%' OR
		     replace(replace(lower(stable_reason),'_',' '),'-',' ') LIKE '%securely erased%'))+
		  (SELECT count(*)
		   FROM privacy_erasure_receipt_heads h
		   JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
		   WHERE h.erasure_id=$1 AND (
		     btrim(r.stable_reason)='' OR btrim(r.verification_method)='' OR
		     replace(replace(lower(r.stable_reason||' '||r.verification_method),'_',' '),'-',' ') LIKE '%secure erase%' OR
		     replace(replace(lower(r.stable_reason||' '||r.verification_method),'_',' '),'-',' ') LIKE '%securely erased%'))`,
		barrier.ErasureID).Scan(&invalidWording); err != nil {
		t.Fatal(err)
	}
	if invalidWording != 0 {
		t.Fatalf("privacy summary or receipt uses empty/secure-erase wording: %d", invalidWording)
	}
}

func assertPostErasureReplayConvergence(t *testing.T, pool *pgxpool.Pool, fixture privacyMarkerFixture) {
	t.Helper()
	ctx := context.Background()
	tutoringStore := tutoringdb.New(pool)
	store := learningdb.New(pool, tutoringStore)
	now := time.Now().UTC().Truncate(time.Microsecond)
	sessionID := uuid.NewString()
	session := tutoring.Session{ID: sessionID, State: tutoring.StateGoalReady}
	payload, err := json.Marshal(learning.SessionProjection{Session: session})
	if err != nil {
		t.Fatal(err)
	}
	operationID := uuid.NewString()
	result, err := store.Commit(ctx, learning.CommitRequest{
		DeviceID: fixture.deviceID,
		Operation: learning.OperationEnvelope{
			OperationID: operationID, PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID,
			ExpectedVersion: 0, Payload: json.RawMessage(`{"command":"post_erasure_session"}`),
		},
		RequestHash:  learning.SHA256([]byte("post-erasure:" + operationID)),
		Expectations: []learning.AggregateExpectation{{Type: "session", ID: sessionID, ExpectedVersion: 0}},
		Batch: learning.CommandBatch{
			Session: &session, TutoringState: string(session.State),
			Events: []learning.EventDraft{{
				Type: learning.EventLearningSessionStarted, AggregateType: "session", AggregateID: sessionID, Payload: payload,
			}},
		},
		ReceivedAt: now,
	})
	if err != nil {
		t.Fatalf("append post-erasure generation event: %v", err)
	}
	incremental, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.LastEventSequence != incremental.HighWater || incremental.Metadata.AsOfEventSequence != incremental.HighWater {
		t.Fatalf("incremental checkpoint result=%+v status=%+v", result, incremental)
	}
	assertProjectionSealMatches(t, pool, incremental)
	assertOldProjectionContentAbsent(t, pool, fixture, incremental.ActiveGenerationID)

	rebuilt, err := store.Rebuild(ctx)
	if err != nil {
		t.Fatalf("fresh rebuild after erasure: %v", err)
	}
	if rebuilt.ActiveGenerationID == incremental.ActiveGenerationID || rebuilt.Fingerprint != incremental.Fingerprint ||
		rebuilt.HighWater != incremental.HighWater || rebuilt.Metadata.AsOfEventSequence != incremental.Metadata.AsOfEventSequence {
		t.Fatalf("incremental/fresh replay mismatch incremental=%+v rebuilt=%+v", incremental, rebuilt)
	}
	assertProjectionSealMatches(t, pool, rebuilt)
	assertOldProjectionContentAbsent(t, pool, fixture, rebuilt.ActiveGenerationID)
}

func assertProjectionSealMatches(t *testing.T, pool *pgxpool.Pool, status learning.ProjectionStatus) {
	t.Helper()
	var generationCheckpoint, checkpoint int64
	var generationFingerprint, checkpointFingerprint []byte
	if err := pool.QueryRow(context.Background(), `
		SELECT g.checkpoint_event_seq,c.event_seq,g.fingerprint,c.fingerprint
		FROM learning_projection_generations g
		JOIN learning_projection_checkpoints c ON c.generation_id=g.id
		WHERE g.id=$1`, status.ActiveGenerationID).Scan(
		&generationCheckpoint, &checkpoint, &generationFingerprint, &checkpointFingerprint); err != nil {
		t.Fatal(err)
	}
	if generationCheckpoint != status.HighWater || checkpoint != status.HighWater ||
		hex.EncodeToString(generationFingerprint) != status.Fingerprint || hex.EncodeToString(checkpointFingerprint) != status.Fingerprint {
		t.Fatalf("projection seal mismatch status=%+v generation_checkpoint=%d checkpoint=%d generation_fingerprint=%x checkpoint_fingerprint=%x",
			status, generationCheckpoint, checkpoint, generationFingerprint, checkpointFingerprint)
	}
}

func assertOldProjectionContentAbsent(t *testing.T, pool *pgxpool.Pool, fixture privacyMarkerFixture, generationID string) {
	t.Helper()
	var routes, evidence, oldFocus int64
	if err := pool.QueryRow(context.Background(), `
		SELECT
		  (SELECT count(*) FROM learning_projection_routes WHERE generation_id=$1),
		  (SELECT count(*) FROM learning_projection_evidence WHERE generation_id=$1),
		  (SELECT count(*) FROM learning_projection_sessions WHERE generation_id=$1 AND item::text LIKE '%'||$2||'%')`,
		generationID, fixture.focusFrameID).Scan(&routes, &evidence, &oldFocus); err != nil {
		t.Fatal(err)
	}
	if routes != 0 || evidence != 0 || oldFocus != 0 {
		t.Fatalf("old projection content survived generation=%s routes=%d evidence=%d focus=%d", generationID, routes, evidence, oldFocus)
	}
}

func assertPrivacyStatusConstraintsRejectInvalid(t *testing.T, pool *pgxpool.Pool, erasureID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE privacy_erasure_heads SET status='invalid_status' WHERE erasure_id=$1`, erasureID); err == nil {
		t.Fatal("PostgreSQL accepted invalid erasure status")
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO privacy_erasure_step_receipts(
			id,erasure_id,store_kind,version,scope_digest,started_at,status,stable_reason,verification_method)
		VALUES($1,$2,'identity_metadata',999,decode(repeat('aa',32),'hex'),clock_timestamp(),
		       'invalid_status','invalid fixture','invalid fixture')`, uuid.NewString(), erasureID); err == nil {
		t.Fatal("PostgreSQL accepted invalid erasure step status")
	}
}

func assertPrivacyFaultMatrixCoverage(t *testing.T, stores []privacy.StoreKind) {
	t.Helper()
	expected := map[privacy.StoreKind]bool{
		privacy.StoreIdentityMetadata: true, privacy.StoreKnowledgeContent: true,
		privacy.StoreKnowledgeIndex: true, privacy.StoreKnowledgeArtifacts: true,
		privacy.StoreLearningEventPayload: true, privacy.StoreLearningTypedPayload: true,
		privacy.StoreProjectionGenerations: true, privacy.StoreTutoringPayload: true,
		privacy.StoreMemoryCandidateDelivery: true, privacy.StoreInboxOutbox: true,
	}
	seen := make(map[privacy.StoreKind]bool, len(stores))
	for _, store := range stores {
		if !expected[store] || seen[store] {
			t.Fatalf("unexpected or duplicate fault slot %q", store)
		}
		if _, ok := privacy.OwnerForStore(store); !ok {
			t.Fatalf("fault slot %q has no owner", store)
		}
		seen[store] = true
	}
	if len(seen) != len(expected) {
		t.Fatalf("fault matrix stores=%v want=%v", seen, expected)
	}
	if seen[privacy.StoreProcessCache] {
		t.Fatal("process cache barrier-only slot entered local scrub fault matrix")
	}
}

func assertBarrierAndGenerationPersisted(t *testing.T, pool *pgxpool.Pool, erasureID string, generation int64) {
	t.Helper()
	var barrierCount, generationCount int64
	var summary privacy.ErasureStatus
	if err := pool.QueryRow(context.Background(), `
		SELECT
		  (SELECT count(*) FROM privacy_redaction_barriers WHERE erasure_id=$1),
		  (SELECT count(*) FROM privacy_owner_generation_gates WHERE learner_generation=$2),
		  (SELECT status FROM privacy_erasure_heads WHERE erasure_id=$1)`, erasureID, generation).Scan(
		&barrierCount, &generationCount, &summary); err != nil {
		t.Fatal(err)
	}
	if barrierCount != 1 || generationCount != int64(len(privacy.AllOwners)) || summary != privacy.StatusBarrierCommitted {
		t.Fatalf("barrier rolled back count=%d generation_gates=%d summary=%s", barrierCount, generationCount, summary)
	}
}

func assertCurrentStepStatus(t *testing.T, pool *pgxpool.Pool, erasureID string, store privacy.StoreKind, status privacy.StepStatus, version int64) {
	t.Helper()
	var actual privacy.StepStatus
	var actualVersion int64
	if err := pool.QueryRow(context.Background(), `
		SELECT r.status,r.version
		FROM privacy_erasure_receipt_heads h
		JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
		WHERE h.erasure_id=$1 AND h.store_kind=$2`, erasureID, store).Scan(&actual, &actualVersion); err != nil {
		t.Fatal(err)
	}
	if actual != status || actualVersion != version {
		t.Fatalf("step %s status=%s version=%d want=%s/%d", store, actual, actualVersion, status, version)
	}
}

func currentCompletedReceiptHistory(t *testing.T, pool *pgxpool.Pool, erasureID string) map[privacy.StoreKind]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT h.store_kind,count(history.id)
		FROM privacy_erasure_receipt_heads h
		JOIN privacy_erasure_step_receipts current ON current.id=h.current_receipt_id
		JOIN privacy_erasure_step_receipts history ON history.erasure_id=h.erasure_id AND history.store_kind=h.store_kind
		WHERE h.erasure_id=$1 AND current.status IN ('succeeded','not_applicable')
		GROUP BY h.store_kind`, erasureID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := make(map[privacy.StoreKind]int64)
	for rows.Next() {
		var store privacy.StoreKind
		var count int64
		if err := rows.Scan(&store, &count); err != nil {
			t.Fatal(err)
		}
		result[store] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(result) == 0 {
		t.Fatal("fault fixture had no completed receipt to protect from duplication")
	}
	return result
}

func assertCompletedReceiptHistoryUnchanged(t *testing.T, pool *pgxpool.Pool, erasureID string, before map[privacy.StoreKind]int64) {
	t.Helper()
	for store, count := range before {
		var after int64
		if err := pool.QueryRow(context.Background(), `
			SELECT count(*) FROM privacy_erasure_step_receipts WHERE erasure_id=$1 AND store_kind=$2`,
			erasureID, store).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != count {
			t.Errorf("completed receipt %s duplicated history before=%d after=%d", store, count, after)
		}
	}
}

func assertLocalGatesAfterConvergence(t *testing.T, pool *pgxpool.Pool, erasureID string, generation int64) {
	t.Helper()
	var openNonMemory, memoryClosed int64
	if err := pool.QueryRow(context.Background(), `
		SELECT
		  count(*) FILTER (WHERE owner_kind<>'memory' AND learner_generation=$2 AND read_open AND write_open AND active_erasure_id IS NULL),
		  count(*) FILTER (WHERE owner_kind='memory' AND learner_generation=$2 AND NOT read_open AND NOT write_open AND active_erasure_id=$1)
		FROM privacy_owner_generation_gates`, erasureID, generation).Scan(&openNonMemory, &memoryClosed); err != nil {
		t.Fatal(err)
	}
	if openNonMemory != int64(len(privacy.AllOwners)-1) || memoryClosed != 1 {
		t.Fatalf("post-convergence gates open_non_memory=%d memory_closed=%d", openNonMemory, memoryClosed)
	}
}
