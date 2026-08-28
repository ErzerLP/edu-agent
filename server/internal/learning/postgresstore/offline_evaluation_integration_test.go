package postgresstore_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxpostgres "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	tutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
)

func TestPostgreSQLOfflineOpenEvaluationConvergesWithoutRewritingInbox(t *testing.T) {
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'offline-open',clock_timestamp())`, learningDeviceOne); err != nil {
		t.Fatal(err)
	}
	insertLearningKnowledgeFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE knowledge_catalog SET head_revision_id=$1,updated_at=clock_timestamp() WHERE singleton_id=1`, learningKnowledgeRevision); err != nil {
		t.Fatal(err)
	}
	store := postgresstore.New(pool, tutoringpostgres.New(pool), knowledgedb.New(pool))
	goalRevisionID := "71000000-0000-4000-8000-000000000001"
	if _, err := store.Commit(ctx, goalCommit(t, learningDeviceOne, "71000000-0000-4000-8000-000000000002", goalRevisionID, 0, 1, 1)); err != nil {
		t.Fatal(err)
	}
	_, version := seedOfflinePrepareSessionWithType(t, store, goalRevisionID, learning.ActivityOpen)
	signer := offlineIntegrationSigner(t)
	offlineService, err := learning.NewOfflineService(store, signer, signer.Origin(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	handler := offlineIntegrationHTTPHandler(t, offlineService, learningDeviceOne)
	prepareRequest := learning.OfflinePrepareRequest{
		OperationID: "71000000-0000-4000-8000-000000000003", PayloadSchemaVersion: 1,
		ExpectedSessionVersion: strconv.FormatInt(version, 10), TrustedManifestRevision: "0",
		TrustedManifestDigest: learning.OfflineZeroDigest,
	}
	var prepared learning.OfflinePrepareResponse
	if status := offlineIntegrationRequest(t, handler, http.MethodPost, "/v1/learning/offline/packs", prepareRequest, &prepared); status != http.StatusCreated {
		t.Fatalf("prepare status=%d", status)
	}
	var pack learning.OfflinePackPayloadV1
	if err := json.Unmarshal(prepared.Pack.Payload, &pack); err != nil || len(pack.Items) != 1 || pack.Items[0].Activity.Type != learning.ActivityOpen {
		t.Fatalf("open pack=%+v err=%v", pack, err)
	}
	item := pack.Items[0]
	var authorization learning.OfflineAuthorizationPayloadV1
	if err := json.Unmarshal(item.Authorization.Payload, &authorization); err != nil {
		t.Fatal(err)
	}
	answer := "topic"
	attemptPayload, _ := json.Marshal(learning.OfflineAttemptPayload{
		Answer: answer, AnswerSHA256: learning.SHA256([]byte(answer)), Help: learning.HelpNone,
		Observations: []learning.OfflineObservation{{Kind: "activity_presented"}, {Kind: "answer_recorded"}},
	})
	wire := learning.OfflineOperationWireV1{
		OperationID: authorization.OperationID, DeviceID: learningDeviceOne,
		DeviceSequence: authorization.DeviceSequence, SubmissionID: authorization.SubmissionID,
		PayloadSchemaVersion: 1, AggregateType: "offline_attempt", AggregateID: authorization.SubmissionID,
		ExpectedVersion: authorization.ExpectedVersion, OfflineActivityID: authorization.OfflineActivityID,
		ActivityRevision: authorization.ActivityRevision, Authorization: item.Authorization.Payload,
		Signature: item.Authorization.Signature, OperationType: learning.OfflineAttemptCompleted, Payload: attemptPayload,
	}
	wireBody, _ := json.Marshal(wire)
	syncRequest := learning.OfflineSyncRequest{SyncRequestID: "71000000-0000-4000-8000-000000000004", PayloadSchemaVersion: 1, Operations: []json.RawMessage{wireBody}}
	var synced learning.OfflineSyncResponse
	if status := offlineIntegrationRequest(t, handler, http.MethodPost, "/v1/learning/offline/sync", syncRequest, &synced); status != http.StatusOK || len(synced.Results) != 1 || synced.Results[0].AssessmentStatus != learning.OfflineAssessmentQueued || synced.Results[0].EvidenceStatus != learning.OfflineEvidencePendingEvaluation {
		t.Fatalf("open sync status=%d response=%+v", status, synced)
	}
	var inboxBefore string
	if err := pool.QueryRow(ctx, `SELECT result::text FROM learning_inbox WHERE operation_id=$1`, authorization.OperationID).Scan(&inboxBefore); err != nil {
		t.Fatal(err)
	}

	model := offlineEvaluationIntegrationModel{}
	learningService, err := learning.NewService(store, store, offlineEvaluationIntegrationResolver{}, learning.ServiceOptions{
		Model: model, ModelID: "offline-integration-model", ModelParameters: map[string]any{"temperature": 0}, PromptRevision: "offline-integration-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := learning.NewOfflineEvaluationConsumer(learningService, store)
	if err != nil {
		t.Fatal(err)
	}
	recordingConsumer := &offlineEvaluationRecordingConsumer{inner: consumer}
	worker, err := outbox.NewWorker(outboxpostgres.New(pool), map[string]outbox.Consumer{"learning.offline-evaluation": recordingConsumer}, outbox.WorkerOptions{
		BatchSize: 5, Lease: time.Minute, BaseBackoff: time.Second, MaxBackoff: time.Minute,
		Now: time.Now, Jitter: func(time.Duration) time.Duration { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := worker.RunOnce(ctx); err != nil || processed != 1 {
		t.Fatalf("worker processed=%d err=%v", processed, err)
	}
	if recordingConsumer.err != nil {
		t.Fatalf("offline evaluation consumer: %v", recordingConsumer.err)
	}
	var status learning.OfflineOperationStatus
	if code := offlineIntegrationRequest(t, handler, http.MethodGet, "/v1/learning/offline/operations/"+authorization.OperationID, nil, &status); code != http.StatusOK || status.AssessmentStatus != learning.OfflineAssessmentCompleted || status.EvidenceStatus != learning.OfflineEvidenceAccepted || status.AssessmentID == "" || status.EvidenceID == "" {
		var jobStatus, jobError, outboxError string
		if err := pool.QueryRow(ctx, `SELECT j.status,COALESCE(j.last_error_category,''),COALESCE(m.last_error_category,'') FROM offline_evaluation_jobs j JOIN outbox_messages m ON m.id=j.outbox_id WHERE j.submission_id=$1`, authorization.SubmissionID).Scan(&jobStatus, &jobError, &outboxError); err != nil {
			t.Fatalf("load failed worker diagnostics: %v", err)
		}
		t.Fatalf("completed status HTTP=%d result=%+v job_status=%s job_error=%v outbox_error=%v", code, status, jobStatus, jobError, outboxError)
	}
	var inboxAfter string
	if err := pool.QueryRow(ctx, `SELECT result::text FROM learning_inbox WHERE operation_id=$1`, authorization.OperationID).Scan(&inboxAfter); err != nil {
		t.Fatal(err)
	}
	if inboxAfter != inboxBefore {
		t.Fatalf("immutable inbox changed after evaluation\nbefore=%s\nafter=%s", inboxBefore, inboxAfter)
	}
	if processed, err := worker.RunOnce(ctx); err != nil || processed != 0 {
		t.Fatalf("worker replay processed=%d err=%v", processed, err)
	}
	var assessments, decisions, evidence, statusRevisions int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM learning_assessments WHERE attempt_id=$1),
		  (SELECT count(*) FROM learning_assessment_decisions d JOIN learning_assessments a ON a.id=d.assessment_id WHERE a.attempt_id=$1),
		  (SELECT count(*) FROM learning_evidence WHERE attempt_id=$1),
		  (SELECT count(*) FROM offline_operation_status_revisions r JOIN offline_operation_statuses s ON s.ticket_id=r.ticket_id WHERE s.submission_id=$1)`,
		authorization.SubmissionID).Scan(&assessments, &decisions, &evidence, &statusRevisions); err != nil {
		t.Fatal(err)
	}
	if assessments != 1 || decisions != 1 || evidence != 1 || statusRevisions < 3 {
		t.Fatalf("assessment=%d decisions=%d evidence=%d status revisions=%d", assessments, decisions, evidence, statusRevisions)
	}
}

func TestPostgreSQLOfflineAttemptRespectsExistingOnlineWinner(t *testing.T) {
	pool, store, _ := newOfflineIngestFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	activityID := seedOfflineActivity(t, pool, "objective", now.Add(-time.Hour), now.Add(time.Hour), now.Add(24*time.Hour))
	operation := seedOfflineSubmission(t, pool, learningDeviceOne, 1, activityID, now.Add(time.Hour), now.Add(24*time.Hour), learning.OfflineAttemptCompleted)
	if _, err := pool.Exec(ctx, `
		INSERT INTO learning_activities(
		  id,revision,session_id,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,
		  target_node_id,target_node_revision_id,prompt,activity_type,rubric_revision,rubric,difficulty,
		  allowed_help,activity_policy_version,assessment_policy_version,review_policy_version,
		  source_proposal_id,attached_free_question_id,attached_free_answer_id,is_review,created_at)
		SELECT id,revision,parent_session_id,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,
		       target_node_id,target_node_revision_id,prompt,activity_type,rubric_revision,rubric,difficulty,
		       allowed_help,activity_policy_version,assessment_policy_version,review_policy_version,
		       NULL,NULL,NULL,FALSE,created_at
		FROM offline_activities WHERE id=$1 AND revision=1`, activityID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO learning_activity_references(
		  activity_id,ordinal,knowledge_revision_id,node_id,node_revision_id,document_revision_id,
		  source_start,source_end,slice_text,slice_hash)
		SELECT activity_id,ordinal,knowledge_revision_id,node_id,node_revision_id,document_revision_id,
		       source_start,source_end,slice_text,slice_hash
		FROM offline_activity_references WHERE activity_id=$1 AND activity_revision=1`, activityID); err != nil {
		t.Fatal(err)
	}
	onlineAttemptID := "72000000-0000-4000-8000-000000000001"
	onlinePayloadID := "72000000-0000-4000-8000-000000000002"
	answer := "online"
	answerHash := learning.SHA256([]byte(answer))
	if _, err := pool.Exec(ctx, `INSERT INTO learning_attempt_payloads(id,answer_text,payload_hash,created_at) VALUES($1,$2,decode($3,'hex'),$4)`, onlinePayloadID, answer, answerHash, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO learning_attempts(
		  id,session_id,activity_id,activity_revision,answer_payload_id,help_level,actor_device_id,
		  occurred_at,received_at,payload_hash,evidence_eligibility,archive_disposition)
		VALUES($1,$2,$3,1,$4,'none',$5,$6,$6,decode($7,'hex'),TRUE,'online')`,
		onlineAttemptID, "50000000-0000-4000-8000-000000000001", activityID, onlinePayloadID, learningDeviceTwo, now, answerHash); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO learning_activity_evidence_claims(
		  activity_id,activity_revision,winning_attempt_id,claim_source,claimed_at)
		VALUES($1,1,$2,'online',$3)`, activityID, onlineAttemptID, now); err != nil {
		t.Fatal(err)
	}
	result, err := store.IngestOffline(ctx, learning.OfflineIngestRequest{Operation: operation})
	if err != nil || result.EvidenceStatus != learning.OfflineEvidenceProvisional || len(result.ReasonCodes) != 1 || result.ReasonCodes[0] != learning.OfflineReasonDuplicateActivity {
		t.Fatalf("offline contender result=%+v err=%v", result, err)
	}
	var winner, source string
	var attempts, evidence int
	if err := pool.QueryRow(ctx, `SELECT winning_attempt_id::text,claim_source FROM learning_activity_evidence_claims WHERE activity_id=$1 AND activity_revision=1`, activityID).Scan(&winner, &source); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_attempts WHERE activity_id=$1`, activityID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_evidence WHERE activity_id=$1`, activityID).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if winner != onlineAttemptID || source != "online" || attempts != 2 || evidence != 0 {
		t.Fatalf("winner=%s source=%s attempts=%d evidence=%d", winner, source, attempts, evidence)
	}
}

func TestPostgreSQLOfflineEvaluationRetriesThenConvergesProvisionally(t *testing.T) {
	pool, store, _ := newOfflineIngestFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	activityID := seedOfflineActivity(t, pool, "open", now.Add(-time.Hour), now.Add(time.Hour), now.Add(24*time.Hour))
	operation := seedOfflineSubmission(t, pool, learningDeviceOne, 1, activityID, now.Add(time.Hour), now.Add(24*time.Hour), learning.OfflineAttemptCompleted)
	queued, err := store.IngestOffline(ctx, learning.OfflineIngestRequest{Operation: operation})
	if err != nil || queued.AssessmentStatus != learning.OfflineAssessmentQueued {
		t.Fatalf("queue invalid-schema evaluation=%+v err=%v", queued, err)
	}
	learningService, err := learning.NewService(store, store, offlineEvaluationIntegrationResolver{}, learning.ServiceOptions{
		Model: offlineEvaluationInvalidModel{}, ModelID: "offline-invalid-model", ModelParameters: map[string]any{}, PromptRevision: "offline-invalid-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := learning.NewOfflineEvaluationConsumer(learningService, store)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := outbox.NewWorker(outboxpostgres.New(pool), map[string]outbox.Consumer{"learning.offline-evaluation": consumer}, outbox.WorkerOptions{
		BatchSize: 5, Lease: time.Minute, BaseBackoff: time.Second, MaxBackoff: time.Minute,
		Now: time.Now, Jitter: func(time.Duration) time.Duration { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := worker.RunOnce(ctx); err != nil || processed != 1 {
		t.Fatalf("first invalid-schema worker processed=%d err=%v", processed, err)
	}
	pending, err := store.OfflineOperationStatus(ctx, learningDeviceOne, operation.OperationID)
	if err != nil || pending.AssessmentStatus != learning.OfflineAssessmentPendingRetry || len(pending.ReasonCodes) != 1 || pending.ReasonCodes[0] != "schema_error" {
		t.Fatalf("pending retry status=%+v err=%v", pending, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE offline_evaluation_jobs SET retry_deadline=created_at,available_at=clock_timestamp()-interval '1 second' WHERE submission_id=$1`, operation.SubmissionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_messages SET available_at=clock_timestamp()-interval '1 second' WHERE idempotency_key=$1`, "learning.offline-evaluation:"+operation.SubmissionID); err != nil {
		t.Fatal(err)
	}
	if processed, err := worker.RunOnce(ctx); err != nil || processed != 1 {
		t.Fatalf("terminal invalid-schema worker processed=%d err=%v", processed, err)
	}
	completed, err := store.OfflineOperationStatus(ctx, learningDeviceOne, operation.OperationID)
	if err != nil || completed.AssessmentStatus != learning.OfflineAssessmentCompleted || completed.EvidenceStatus != learning.OfflineEvidenceProvisional || !slices.Contains(completed.ReasonCodes, "schema_error") || completed.AssessmentID == "" || completed.EvidenceID != "" {
		t.Fatalf("provisional status=%+v err=%v", completed, err)
	}
	var assessments, decisions, evidence int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_assessments WHERE id=$1`, completed.AssessmentID).Scan(&assessments); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_assessment_decisions WHERE assessment_id=$1`, completed.AssessmentID).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_evidence WHERE assessment_id=$1`, completed.AssessmentID).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if assessments != 1 || decisions != 1 || evidence != 0 {
		t.Fatalf("schema fallback counts assessments=%d decisions=%d evidence=%d", assessments, decisions, evidence)
	}

	directPage, err := learningService.ListOfflineAssessments(ctx, learningDeviceOne, learning.OfflineAssessmentQuery{
		Status: learning.OfflineAssessmentFilterProvisional,
		Page:   learning.CursorPageRequest{Limit: 10},
	})
	if err != nil || len(directPage.Items) != 1 || directPage.Items[0].AssessmentID != completed.AssessmentID {
		t.Fatalf("offline assessment service list page=%+v err=%v", directPage, err)
	}
	signer := offlineIntegrationSigner(t)
	offlineService, err := learning.NewOfflineServiceWithGenerator(store, learningService, signer, signer.Origin(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	handler := offlineIntegrationHTTPHandler(t, offlineService, learningDeviceOne)
	var page learning.OfflineAssessmentPage
	if code := offlineIntegrationRequest(t, handler, http.MethodGet, "/v1/learning/offline/assessments?status=provisional&limit=10", nil, &page); code != http.StatusOK || len(page.Items) != 1 || page.Items[0].AssessmentID != completed.AssessmentID {
		t.Fatalf("offline assessment list HTTP=%d page=%+v", code, page)
	}
	var view learning.OfflineAssessmentView
	assessmentPath := "/v1/learning/offline/assessments/" + completed.AssessmentID
	if code := offlineIntegrationRequest(t, handler, http.MethodGet, assessmentPath, nil, &view); code != http.StatusOK || view.Attempt.ID != operation.SubmissionID || view.Decision.Version != 1 || view.Decision.Disposition != learning.DispositionProvisional {
		t.Fatalf("offline assessment show HTTP=%d view=%+v", code, view)
	}
	decisionRequest := map[string]any{
		"operation_id":                 "73000000-0000-4000-8000-000000000001",
		"payload_schema_version":       1,
		"attempt_id":                   view.Attempt.ID,
		"expected_version":             view.AggregateVersion,
		"kind":                         "void",
		"expected_disposition_version": strconv.FormatInt(view.Decision.Version, 10),
		"reason":                       "invalid model assessment",
	}
	var decided learning.OfflineAssessmentDecisionReceipt
	decisionPath := assessmentPath + "/decisions"
	if code := offlineIntegrationRequest(t, handler, http.MethodPost, decisionPath, decisionRequest, &decided); code != http.StatusCreated || decided.Replayed || decided.Decision.Disposition != learning.DispositionVoided || decided.AssessmentID != completed.AssessmentID {
		t.Fatalf("offline assessment decision HTTP=%d receipt=%+v", code, decided)
	}
	var replayed learning.OfflineAssessmentDecisionReceipt
	if code := offlineIntegrationRequest(t, handler, http.MethodPost, decisionPath, decisionRequest, &replayed); code != http.StatusOK || !replayed.Replayed || replayed.Decision.ID != decided.Decision.ID {
		t.Fatalf("offline assessment decision replay HTTP=%d receipt=%+v", code, replayed)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_evidence WHERE assessment_id=$1`, completed.AssessmentID).Scan(&evidence); err != nil || evidence != 0 {
		t.Fatalf("voided offline assessment evidence=%d err=%v", evidence, err)
	}
}

type offlineEvaluationInvalidModel struct{}

func (offlineEvaluationInvalidModel) Generate(context.Context, learning.ProposalRequest) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

type offlineEvaluationRecordingConsumer struct {
	inner outbox.Consumer
	err   error
}

func (c *offlineEvaluationRecordingConsumer) CanApply(ctx context.Context, message outbox.Message) (outbox.ApplyDecision, error) {
	return c.inner.CanApply(ctx, message)
}

func (c *offlineEvaluationRecordingConsumer) Apply(ctx context.Context, message outbox.Message) error {
	c.err = c.inner.Apply(ctx, message)
	return c.err
}

type offlineEvaluationIntegrationResolver struct{}

func (offlineEvaluationIntegrationResolver) Resolve(context.Context, string, string) (learning.KnowledgeReference, error) {
	return learning.KnowledgeReference{
		KnowledgeRevisionID: learningKnowledgeRevision, NodeID: learningNodeID,
		NodeRevisionID: learningNodeRevisionID, DocumentRevisionID: learningDocumentRevisionID,
		Range: learning.SourceRange{Start: 0, End: 5}, Slice: "topic", SliceSHA256: learning.SHA256([]byte("topic")),
	}, nil
}

type offlineEvaluationIntegrationModel struct{}

func (offlineEvaluationIntegrationModel) Generate(context.Context, learning.ProposalRequest) (json.RawMessage, error) {
	quoteHash := learning.SHA256([]byte("topic"))
	return json.Marshal(map[string]any{"assessment": map[string]any{
		"items": []any{map[string]any{
			"rubric_item_id": "item-1", "conclusion": "pass",
			"answer_quote": "topic", "answer_range": map[string]any{"start": 0, "end": 5}, "answer_quote_sha256": quoteHash,
			"knowledge_reference_id": learningNodeRevisionID, "knowledge_quote": "topic",
			"knowledge_range": map[string]any{"start": 0, "end": 5}, "knowledge_quote_sha256": quoteHash,
		}},
		"rubric_complete": true, "confidence": 900, "risk_flags": []any{},
	}})
}
