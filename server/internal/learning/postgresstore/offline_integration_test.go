package postgresstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	tutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgreSQLOfflineIngestSequenceWinnerRejectionAndOpenQueue(t *testing.T) {
	pool, store, sessionBefore := newOfflineIngestFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	activityA := seedOfflineActivity(t, pool, "objective", now.Add(-time.Hour), now.Add(time.Hour), now.Add(24*time.Hour))
	activityB := seedOfflineActivity(t, pool, "objective", now.Add(-time.Hour), now.Add(time.Hour), now.Add(24*time.Hour))
	deviceOneA := seedOfflineSubmission(t, pool, learningDeviceOne, 1, activityA, now.Add(time.Hour), now.Add(24*time.Hour), learning.OfflineAttemptCompleted)
	deviceOneB := seedOfflineSubmission(t, pool, learningDeviceOne, 2, activityB, now.Add(time.Hour), now.Add(24*time.Hour), learning.OfflineAttemptCompleted)
	deviceTwoA := seedOfflineSubmission(t, pool, learningDeviceTwo, 1, activityA, now.Add(time.Hour), now.Add(24*time.Hour), learning.OfflineAttemptCompleted)

	outOfOrder, err := store.IngestOffline(ctx, learning.OfflineIngestRequest{Operation: deviceOneB})
	if err != nil || outOfOrder.ArchiveStatus != learning.OfflineArchivedSucceeded || outOfOrder.EvidenceStatus != learning.OfflineEvidenceAccepted {
		t.Fatalf("out-of-order sequence ingest=%+v err=%v", outOfOrder, err)
	}

	start := make(chan struct{})
	results := make(chan learning.OfflineIngestResult, 2)
	errors := make(chan error, 2)
	var wait sync.WaitGroup
	for _, operation := range []learning.OfflineOperation{deviceOneA, deviceTwoA} {
		operation := operation
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := store.IngestOffline(context.Background(), learning.OfflineIngestRequest{Operation: operation})
			results <- result
			errors <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent offline winner ingest: %v", err)
		}
	}
	accepted, provisional := 0, 0
	var replayOperation learning.OfflineOperation
	for result := range results {
		switch result.EvidenceStatus {
		case learning.OfflineEvidenceAccepted:
			accepted++
			if result.OperationID == deviceOneA.OperationID {
				replayOperation = deviceOneA
			} else {
				replayOperation = deviceTwoA
			}
		case learning.OfflineEvidenceProvisional:
			provisional++
			if len(result.ReasonCodes) != 1 || result.ReasonCodes[0] != learning.OfflineReasonDuplicateActivity {
				t.Fatalf("duplicate contender result=%+v", result)
			}
		default:
			t.Fatalf("unexpected concurrent result=%+v", result)
		}
	}
	if accepted != 1 || provisional != 1 {
		t.Fatalf("winner outcomes accepted=%d provisional=%d", accepted, provisional)
	}
	var claims, attempts, evidence int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_activity_evidence_claims WHERE activity_id=$1`, activityA).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_attempts WHERE activity_id=$1`, activityA).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_evidence WHERE activity_id=$1`, activityA).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if claims != 1 || attempts != 2 || evidence != 1 {
		t.Fatalf("activity winner claims=%d attempts=%d evidence=%d", claims, attempts, evidence)
	}
	var claimedSequences []int64
	rows, err := pool.Query(ctx, `SELECT device_seq FROM offline_device_sequence_claims WHERE device_id=$1 ORDER BY claimed_at,device_seq`, learningDeviceOne)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sequence int64
		if err := rows.Scan(&sequence); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		claimedSequences = append(claimedSequences, sequence)
	}
	rows.Close()
	if len(claimedSequences) < 2 || claimedSequences[0] != 2 {
		t.Fatalf("device sequence did not allow out-of-order claim: %v", claimedSequences)
	}

	replayed, err := store.IngestOffline(ctx, learning.OfflineIngestRequest{Operation: replayOperation})
	if err != nil || !replayed.Replayed || replayed.Receipt == nil {
		t.Fatalf("offline replay=%+v err=%v", replayed, err)
	}

	expiredActivity := seedOfflineActivity(t, pool, "objective", now.Add(-48*time.Hour), now.Add(-36*time.Hour), now.Add(-time.Hour))
	expired := seedOfflineSubmission(t, pool, learningDeviceOne, 3, expiredActivity, now.Add(-36*time.Hour), now.Add(-time.Hour), learning.OfflineAttemptCompleted)
	expired.Attempt.Answer = "rejected secret body"
	expired.Attempt.AnswerSHA256 = learning.SHA256([]byte(expired.Attempt.Answer))
	rejection, err := store.IngestOffline(ctx, learning.OfflineIngestRequest{Operation: expired})
	if err != nil || rejection.ArchiveStatus != learning.OfflineArchivedRejected || rejection.EvidenceStatus != learning.OfflineEvidenceUnchanged || rejection.Receipt == nil {
		t.Fatalf("deterministic rejection=%+v err=%v", rejection, err)
	}
	var rejectionType string
	var rejectionPayload []byte
	if err := pool.QueryRow(ctx, `
		SELECT event.event_type,payload.payload::text
		FROM learning_events event
		JOIN learning_event_payloads payload ON payload.id=event.payload_id
		WHERE event.operation_id=$1`, expired.OperationID).Scan(&rejectionType, &rejectionPayload); err != nil {
		t.Fatal(err)
	}
	if rejectionType != string(learning.EventOfflineOperationRejected) || strings.Contains(string(rejectionPayload), "rejected secret body") || strings.Contains(string(rejectionPayload), "prompt") || strings.Contains(string(rejectionPayload), "slice") {
		t.Fatalf("rejection event leaked body type=%s payload=%s", rejectionType, rejectionPayload)
	}

	skipActivity := seedOfflineActivity(t, pool, "objective", now.Add(-time.Hour), now.Add(time.Hour), now.Add(24*time.Hour))
	skip := seedOfflineSubmission(t, pool, learningDeviceOne, 4, skipActivity, now.Add(time.Hour), now.Add(24*time.Hour), learning.OfflineActivitySkipped)
	skipped, err := store.IngestOffline(ctx, learning.OfflineIngestRequest{Operation: skip})
	if err != nil || skipped.EvidenceStatus != learning.OfflineEvidenceNotApplicable {
		t.Fatalf("offline skip=%+v err=%v", skipped, err)
	}
	var skippedAttempts int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_attempts WHERE id=$1`, skip.SubmissionID).Scan(&skippedAttempts); err != nil || skippedAttempts != 0 {
		t.Fatalf("skip created attempt count=%d err=%v", skippedAttempts, err)
	}

	openActivity := seedOfflineActivity(t, pool, "open", now.Add(-time.Hour), now.Add(time.Hour), now.Add(24*time.Hour))
	open := seedOfflineSubmission(t, pool, learningDeviceOne, 5, openActivity, now.Add(time.Hour), now.Add(24*time.Hour), learning.OfflineAttemptCompleted)
	queued, err := store.IngestOffline(ctx, learning.OfflineIngestRequest{Operation: open})
	if err != nil || queued.AssessmentStatus != learning.OfflineAssessmentQueued || queued.EvidenceStatus != learning.OfflineEvidencePendingEvaluation {
		t.Fatalf("open offline queue=%+v err=%v", queued, err)
	}
	var jobs, messages int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM offline_evaluation_jobs WHERE submission_id=$1 AND status='queued'`, open.SubmissionID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_messages WHERE idempotency_key=$1`, "learning.offline-evaluation:"+open.SubmissionID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || messages != 1 {
		t.Fatalf("open queue jobs=%d outbox=%d", jobs, messages)
	}

	sessionAfter, err := store.Session(ctx, sessionBefore.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sessionAfter.Session.State != sessionBefore.Session.State || sessionAfter.Session.AggregateVer != sessionBefore.Session.AggregateVer || sessionAfter.Session.Context.RouteRevisionID != sessionBefore.Session.Context.RouteRevisionID {
		t.Fatalf("offline ingest advanced session before=%+v after=%+v", sessionBefore.Session, sessionAfter.Session)
	}
	timeline, err := store.Timeline(ctx, learning.TimelineQuery{SessionID: sessionBefore.Session.ID, Page: learning.CursorPageRequest{Limit: 200}})
	if err != nil {
		t.Fatal(err)
	}
	offlineItems := 0
	for _, item := range timeline.Items {
		if item.Source == "offline" {
			offlineItems++
			if item.ParentSessionID != sessionBefore.Session.ID {
				t.Fatalf("offline timeline parent=%q want=%q", item.ParentSessionID, sessionBefore.Session.ID)
			}
		}
	}
	if offlineItems == 0 {
		t.Fatal("session-filtered timeline omitted offline events")
	}
}

func TestPostgreSQLOfflinePrivacyPossessionAndRedaction(t *testing.T) {
	pool, learningStore, _ := newOfflineIngestFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	activityID := seedOfflineActivity(t, pool, "open", now.Add(-time.Hour), now.Add(time.Hour), now.Add(24*time.Hour))
	operation := seedOfflineSubmission(t, pool, learningDeviceOne, 60, activityID, now.Add(time.Hour), now.Add(24*time.Hour), learning.OfflineAttemptCompleted)
	operation.Attempt.Answer = "offline privacy marker"
	operation.Attempt.AnswerSHA256 = learning.SHA256([]byte(operation.Attempt.Answer))
	result, err := learningStore.IngestOffline(ctx, learning.OfflineIngestRequest{Operation: operation})
	if err != nil || result.AssessmentStatus != learning.OfflineAssessmentQueued {
		t.Fatalf("seed offline privacy operation=%+v err=%v", result, err)
	}

	privacyStore := privacydb.New(pool,
		privacydb.WithReadPermits(privacy.NewReadPermitManager()),
		privacydb.WithLocalOwner(identitydb.New(pool)),
		privacydb.WithLocalOwner(knowledgedb.New(pool)),
		privacydb.WithLocalOwner(learningStore),
		privacydb.WithLocalOwner(tutoringpostgres.New(pool)),
		privacydb.WithLocalOwner(memorydb.New(pool)),
		privacydb.WithLocalOwner(outboxdb.New(pool)),
	)
	request := privacy.ErasureRequest{
		DeviceID: learningDeviceOne, OperationID: uuid.NewString(), ActorDeviceID: learningDeviceOne,
		ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now,
		ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1,
	}
	barrier, err := privacyStore.CommitBarrier(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if barrier.Status != privacy.StatusBarrierCommitted || barrier.LearnerGeneration != 2 {
		t.Fatalf("offline privacy barrier=%+v", barrier)
	}
	var childStatus string
	if err := pool.QueryRow(ctx, `
		SELECT head.status
		FROM privacy_offline_device_children child
		JOIN privacy_offline_device_child_heads head ON head.child_id=child.id
		WHERE child.erasure_id=$1 AND child.device_id=$2`, barrier.ErasureID, learningDeviceOne).Scan(&childStatus); err != nil {
		t.Fatal(err)
	}
	if childStatus != string(privacy.OfflineDeviceChildUnknown) {
		t.Fatalf("offline privacy child status=%q", childStatus)
	}
	var summaryStatus string
	if err := pool.QueryRow(ctx, `
		SELECT receipt.status
		FROM privacy_erasure_receipt_heads head
		JOIN privacy_erasure_step_receipts receipt ON receipt.id=head.current_receipt_id
		WHERE head.erasure_id=$1 AND head.store_kind='offline_device_cache'`, barrier.ErasureID).Scan(&summaryStatus); err != nil {
		t.Fatal(err)
	}
	if summaryStatus != string(privacy.StepUnknown) {
		t.Fatalf("offline privacy summary=%q", summaryStatus)
	}

	scrubbed, err := privacyStore.RunLocalScrub(ctx, barrier.ErasureID)
	if err != nil {
		t.Fatal(err)
	}
	if scrubbed.Status != privacy.StatusLocalScrubbed {
		t.Fatalf("offline local scrub receipt=%+v", scrubbed)
	}
	var remaining int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(sum(remaining),0)::bigint FROM (
			SELECT count(*)::bigint AS remaining FROM offline_activities WHERE prompt<>'[redacted]' OR rubric<>'{"redacted": true}'::jsonb
			UNION ALL SELECT count(*) FROM offline_activity_references WHERE slice_text<>''
			UNION ALL SELECT count(*) FROM offline_packs WHERE response_body<>'{"redacted": true}'::jsonb OR signer_key_id<>'[redacted]'
			UNION ALL SELECT count(*) FROM offline_submission_authorizations WHERE authorization_payload<>'{"redacted": true}'::jsonb OR signer_key_id<>'[redacted]'
			UNION ALL SELECT count(*) FROM learning_attempt_payloads WHERE answer_text<>''
			UNION ALL SELECT count(*) FROM offline_evaluation_jobs WHERE frozen_request<>'{"redacted": true}'::jsonb
		) residuals`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("offline privacy residual rows=%d", remaining)
	}
	if err := pool.QueryRow(ctx, `
		SELECT head.status
		FROM privacy_offline_device_children child
		JOIN privacy_offline_device_child_heads head ON head.child_id=child.id
		WHERE child.erasure_id=$1 AND child.device_id=$2`, barrier.ErasureID, learningDeviceOne).Scan(&childStatus); err != nil {
		t.Fatal(err)
	}
	if childStatus != string(privacy.OfflineDeviceChildUnknown) {
		t.Fatalf("local scrub falsely acknowledged offline device child=%q", childStatus)
	}
}

func TestPostgreSQLOfflineIngestCriticalWriteGroupsRollback(t *testing.T) {
	pool, store, _ := newOfflineIngestFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	cases := []struct {
		name, table, operation, activityType string
	}{
		{"activity", "learning_activities", "INSERT", "objective"},
		{"attempt_payload", "learning_attempt_payloads", "INSERT", "objective"},
		{"attempt", "learning_attempts", "INSERT", "objective"},
		{"winner", "learning_activity_evidence_claims", "INSERT", "objective"},
		{"assessment", "learning_assessments", "INSERT", "objective"},
		{"evidence", "learning_evidence", "INSERT", "objective"},
		{"event_payload", "learning_event_payloads", "INSERT", "objective"},
		{"event", "learning_events", "INSERT", "objective"},
		{"sequence_claim", "offline_device_sequence_claims", "INSERT", "objective"},
		{"attempt_head", "offline_attempt_heads", "UPDATE", "objective"},
		{"aggregate_head", "learning_aggregate_heads", "UPDATE", "objective"},
		{"event_clock", "learning_event_clock", "UPDATE", "objective"},
		{"projection", "learning_projection_timeline", "INSERT", "objective"},
		{"inbox", "learning_inbox", "INSERT", "objective"},
		{"status", "offline_operation_status_revisions", "INSERT", "objective"},
		{"outbox", "outbox_messages", "INSERT", "open"},
		{"evaluation_job", "offline_evaluation_jobs", "INSERT", "open"},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			activityID := seedOfflineActivity(t, pool, test.activityType, now.Add(-time.Hour), now.Add(time.Hour), now.Add(24*time.Hour))
			operation := seedOfflineSubmission(t, pool, learningDeviceOne, int64(index+10), activityID, now.Add(time.Hour), now.Add(24*time.Hour), learning.OfflineAttemptCompleted)
			before := captureOfflineWriteSnapshot(t, pool)
			installOfflineWriteFault(t, pool, test.table, test.operation, "offline rollback "+test.name)
			_, err := store.IngestOffline(ctx, learning.OfflineIngestRequest{Operation: operation})
			removeOfflineWriteFault(t, pool, test.table)
			if err == nil || !strings.Contains(err.Error(), "offline rollback "+test.name) {
				t.Fatalf("fault %s error=%v", test.name, err)
			}
			after := captureOfflineWriteSnapshot(t, pool)
			if fmt.Sprint(before) != fmt.Sprint(after) {
				t.Fatalf("fault %s left sibling writes\nbefore=%v\nafter=%v", test.name, before, after)
			}
			var state string
			if err := pool.QueryRow(ctx, `SELECT state FROM offline_attempt_heads WHERE submission_id=$1`, operation.SubmissionID).Scan(&state); err != nil || state != "reserved" {
				t.Fatalf("fault %s consumed reservation state=%q err=%v", test.name, state, err)
			}
			if index == len(cases)-1 {
				result, err := store.IngestOffline(ctx, learning.OfflineIngestRequest{Operation: operation})
				if err != nil || result.ArchiveStatus != learning.OfflineArchivedSucceeded {
					t.Fatalf("valid retry after rollback=%+v err=%v", result, err)
				}
			}
		})
	}
}

func newOfflineIngestFixture(t *testing.T) (*pgxpool.Pool, *postgresstore.Store, learning.SessionView) {
	t.Helper()
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'offline-one',clock_timestamp()),($2,'offline-two',clock_timestamp())`, learningDeviceOne, learningDeviceTwo); err != nil {
		t.Fatal(err)
	}
	insertLearningKnowledgeFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE knowledge_catalog SET head_revision_id=$1,updated_at=clock_timestamp() WHERE singleton_id=1`, learningKnowledgeRevision); err != nil {
		t.Fatal(err)
	}
	store := postgresstore.New(pool, tutoringpostgres.New(pool), knowledgedb.New(pool))
	goal := goalCommit(t, learningDeviceOne, "20000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000001", 0, 1, 1)
	if _, err := store.Commit(ctx, goal); err != nil {
		t.Fatal(err)
	}
	sessionID, _ := commitLearningAuthorityFixture(t, store, false)
	session, err := store.Session(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return pool, store, session
}

func seedOfflineActivity(t *testing.T, pool *pgxpool.Pool, activityType string, issuedAt, eligibleUntil, archiveUntil time.Time) string {
	t.Helper()
	activityID := uuid.NewString()
	rubric := map[string]any{"rubric_revision": "offline-r1", "items": []any{}}
	if activityType == "objective" {
		rubric["objective_rule"] = map[string]any{"accepted_answers": []string{"ok"}, "case_sensitive": false, "trim_space": true}
	}
	rubricJSON, _ := json.Marshal(rubric)
	payloadHash := learning.SHA256([]byte("offline prompt " + activityID))
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO offline_activities(
			id,revision,learner_generation,parent_session_id,goal_revision_id,route_revision_id,
			route_step_id,knowledge_revision_id,target_node_id,target_node_revision_id,prompt,
			activity_type,rubric_revision,rubric,difficulty,allowed_help,activity_policy_version,
			assessment_policy_version,review_policy_version,practice_kind,payload_hash,issued_at,
			eligible_until,archive_until,created_at)
		VALUES($1::uuid,1,1,'50000000-0000-4000-8000-000000000001','30000000-0000-4000-8000-000000000001',
		       '50000000-0000-4000-8000-000000000002','50000000-0000-4000-8000-000000000004',
		       $2,$3,$4,'offline prompt '||$1::uuid::text,$5,'offline-r1',$6,1,ARRAY['none','answer_revealed'],
		       $7,$8,$9,'practice',decode($10,'hex'),$11,$12,$13,$11)`, activityID,
		learningKnowledgeRevision, learningNodeID, learningNodeRevisionID, activityType, rubricJSON,
		learning.ActivityPolicyVersion, learning.AssessmentPolicyVersion, learning.ReviewPolicyVersion,
		payloadHash, issuedAt, eligibleUntil, archiveUntil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO offline_activity_references(
			activity_id,activity_revision,ordinal,knowledge_revision_id,node_id,node_revision_id,
			document_revision_id,source_start,source_end,slice_text,slice_hash)
		VALUES($1,1,0,$2,$3,$4,$5,0,5,'topic',decode($6,'hex'))`, activityID,
		learningKnowledgeRevision, learningNodeID, learningNodeRevisionID,
		learningDocumentRevisionID, learning.SHA256([]byte("topic"))); err != nil {
		t.Fatal(err)
	}
	return activityID
}

func seedOfflineSubmission(t *testing.T, pool *pgxpool.Pool, deviceID string, sequence int64, activityID string, eligibleUntil, archiveUntil time.Time, operationType learning.OfflineOperationType) learning.OfflineOperation {
	t.Helper()
	ctx := context.Background()
	packID, prepareOperationID := uuid.NewString(), uuid.NewString()
	operationID, submissionID := uuid.NewString(), uuid.NewString()
	authorizationPayload := json.RawMessage(`{}`)
	authorizationHash := learning.SHA256(authorizationPayload)
	authorizationSignature := make([]byte, 64)
	if _, err := pool.Exec(ctx, `
		INSERT INTO offline_packs(
			id,revision,prepare_device_id,prepare_operation_id,learner_generation,parent_session_id,
			response_body,response_hash,signer_key_id,signature,issued_at,eligible_until,archive_until,created_at)
		VALUES($1,1,$2,$3,1,'50000000-0000-4000-8000-000000000001','{}',decode($4,'hex'),
		       'test-key',decode(repeat('00',64),'hex'),LEAST(clock_timestamp(),$5::timestamptz-interval '1 second'),$5,$6,
		       LEAST(clock_timestamp(),$5::timestamptz-interval '1 second'))`,
		packID, deviceID, prepareOperationID, learning.SHA256([]byte(packID)), eligibleUntil, archiveUntil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO offline_submission_authorizations(
			submission_id,pack_id,device_id,operation_id,device_seq,offline_activity_id,
			activity_revision,learner_generation,credential_epoch,expected_version,
			authorization_payload,authorization_hash,signer_key_id,signature,eligible_until,
			archive_until,created_at)
		VALUES($1,$2,$3,$4,$5,$6,1,1,1,0,'{}',decode($7,'hex'),'test-key',
		       decode(repeat('00',64),'hex'),$8,$9,clock_timestamp())`, submissionID, packID,
		deviceID, operationID, sequence, activityID, authorizationHash, eligibleUntil, archiveUntil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO offline_device_sequence_reservations(
			device_id,device_seq,operation_id,submission_id,authorization_hash,reserved_at)
		VALUES($1,$2,$3,$4,decode($5,'hex'),clock_timestamp())`, deviceID, sequence,
		operationID, submissionID, authorizationHash); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO offline_attempt_heads(
			submission_id,device_id,offline_activity_id,activity_revision,state,reserved_operation_id,
			aggregate_version,updated_at)
		VALUES($1,$2,$3,1,'reserved',$4,0,clock_timestamp())`, submissionID, deviceID,
		activityID, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE offline_device_sequence_heads SET high_water=GREATEST(high_water,$2),updated_at=clock_timestamp() WHERE device_id=$1`, deviceID, sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO offline_device_possessions(id,device_id,learner_generation,first_pack_id,first_seen_at)
		VALUES($1,$2,1,$3,clock_timestamp()) ON CONFLICT(device_id,learner_generation) DO NOTHING`,
		uuid.NewString(), deviceID, packID); err != nil {
		t.Fatal(err)
	}
	operation := learning.OfflineOperation{
		OperationID: operationID, DeviceID: deviceID, CredentialEpoch: 1, LearnerGeneration: 1,
		DeviceSequence: uint64(sequence), SubmissionID: submissionID, PayloadSchemaVersion: 1,
		AggregateType: "offline_attempt", AggregateID: submissionID, ExpectedVersion: 0,
		OfflineActivityID: activityID, ActivityRevision: 1, AuthorizationHash: authorizationHash,
		Authorization: authorizationPayload, AuthorizationSig: authorizationSignature,
		Type: operationType,
	}
	if operationType == learning.OfflineAttemptCompleted {
		operation.Attempt = &learning.OfflineAttemptPayload{Answer: "ok", AnswerSHA256: learning.SHA256([]byte("ok")), Help: learning.HelpNone, Observations: []learning.OfflineObservation{{Kind: "answer_recorded"}}}
	} else {
		operation.Skip = &learning.OfflineSkipPayload{Reason: "user_skipped"}
	}
	return operation
}

func captureOfflineWriteSnapshot(t *testing.T, pool *pgxpool.Pool) map[string]int64 {
	t.Helper()
	tables := []string{
		"learning_activities", "learning_activity_references", "learning_attempt_payloads", "learning_attempts",
		"learning_activity_evidence_claims", "learning_assessments", "learning_assessment_decisions", "learning_evidence",
		"learning_event_payloads", "learning_events", "offline_device_sequence_claims", "learning_inbox",
		"offline_operation_statuses", "offline_operation_status_revisions", "offline_operation_status_heads",
		"offline_evaluation_jobs", "outbox_messages", "learning_projection_timeline", "learning_projection_evidence",
	}
	result := make(map[string]int64, len(tables)+1)
	for _, table := range tables {
		query := `SELECT count(*) FROM ` + pgxIdentifier(table)
		var count int64
		if err := pool.QueryRow(context.Background(), query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		result[table] = count
	}
	var eventClock int64
	if err := pool.QueryRow(context.Background(), `SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1`).Scan(&eventClock); err != nil {
		t.Fatal(err)
	}
	result["event_clock"] = eventClock
	return result
}

func installOfflineWriteFault(t *testing.T, pool *pgxpool.Pool, table, operation, marker string) {
	t.Helper()
	statement := fmt.Sprintf(`
		CREATE FUNCTION offline_reject_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION %s; END $$;
		CREATE TRIGGER offline_reject_write BEFORE %s ON %s
		FOR EACH ROW EXECUTE FUNCTION offline_reject_write()`, quoteSQL(marker), operation, pgxIdentifier(table))
	if _, err := pool.Exec(context.Background(), statement); err != nil {
		t.Fatal(err)
	}
}

func removeOfflineWriteFault(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	statement := fmt.Sprintf(`DROP TRIGGER offline_reject_write ON %s; DROP FUNCTION offline_reject_write()`, pgxIdentifier(table))
	if _, err := pool.Exec(context.Background(), statement); err != nil {
		t.Fatal(err)
	}
}

func pgxIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func quoteSQL(value string) string {
	return `'` + strings.ReplaceAll(value, `'`, `''`) + `'`
}
