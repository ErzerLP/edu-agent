package postgresstore_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBarrierPersistsAcrossStepFailureAndLocalScrubResumes(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'private label',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO outbox_messages(id,business_type,aggregate_id,idempotency_key,revision,generation,payload,audit_metadata,status,available_at,attempts,max_attempts,created_at,updated_at) VALUES($1,'test','owner','privacy-seed',1,1,'{"body":"secret"}','{"trace":"secret"}','pending',clock_timestamp(),0,3,clock_timestamp(),clock_timestamp())`, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	learningOperationID := uuid.NewString()
	tutoringRequestID := uuid.NewString()
	aggregateID := uuid.NewString()
	candidateID, candidatePayloadID := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO learning_inbox(device_id,operation_id,request_hash,aggregate_type,aggregate_id,terminal_status,result,completed_at) VALUES($1,$2,decode(repeat('ab',32),'hex'),'session',$3,'succeeded','{"secret":"learning"}',clock_timestamp())`, deviceID, learningOperationID, aggregateID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tutoring_proposal_requests(device_id,request_id,request_hash,proposal_type,aggregate_type,aggregate_id,aggregate_version,input,status,created_at,updated_at) VALUES($1,$2,decode(repeat('cd',32),'hex'),'free_answer','session',$3,1,'{"secret":"tutoring"}','pending',clock_timestamp(),clock_timestamp())`, deviceID, tutoringRequestID, aggregateID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memory_candidates(id,candidate_uri,payload_id,content_hash,source_kind,source_hashes,proposer_id,reason,category,sensitivity,stability,valid_until,admission_policy_version,created_at) VALUES($1::uuid,'candidate://'||$1::uuid::text,$2::uuid,decode(repeat('ef',32),'hex'),'user_statement','{}'::bytea[],$3::uuid,'private reason','interaction_preference','non_sensitive','stable',clock_timestamp()+interval '1 hour','memory-admission-v1',clock_timestamp())`, candidateID, candidatePayloadID, deviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memory_candidate_heads(candidate_id,revision,status,payload_available,updated_at) VALUES($1,1,'pending_review',TRUE,clock_timestamp())`, candidateID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memory_candidate_payloads(id,candidate_id,content,content_hash,valid_until,created_at) VALUES($1,$2,'private memory',decode(repeat('ef',32),'hex'),clock_timestamp()+interval '1 hour',clock_timestamp())`, candidatePayloadID, candidateID); err != nil {
		t.Fatal(err)
	}
	manager := privacy.NewReadPermitManager()
	identityStore := identitydb.New(pool)
	knowledgeStore := knowledgedb.New(pool)
	tutoringStore := tutoringdb.New(pool)
	learningStore := learningdb.New(pool, tutoringStore)
	memoryStore := memorydb.New(pool)
	outboxStore := outboxdb.New(pool)
	failed := false
	store := privacydb.New(pool,
		privacydb.WithReadPermits(manager),
		privacydb.WithLocalOwner(identityStore),
		privacydb.WithLocalOwner(knowledgeStore),
		privacydb.WithLocalOwner(learningStore),
		privacydb.WithLocalOwner(tutoringStore),
		privacydb.WithLocalOwner(memoryStore),
		privacydb.WithLocalOwner(outboxStore),
		privacydb.WithBeforeStep(func(kind privacy.StoreKind) error {
			if kind == privacy.StoreIdentityMetadata && !failed {
				failed = true
				return errors.New("injected identity scrub failure")
			}
			return nil
		}),
	)
	now := time.Now().UTC()
	request := privacy.ErasureRequest{DeviceID: deviceID, OperationID: uuid.NewString(), ActorDeviceID: deviceID, ReasonCode: "learner_request", RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}
	blockingPermit, err := manager.Acquire(ctx, privacy.OwnerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	drainCtx, cancelDrain := context.WithTimeout(ctx, 10*time.Millisecond)
	if _, err := store.CommitBarrier(drainCtx, request); err == nil {
		cancelDrain()
		blockingPermit.Release()
		t.Fatal("barrier drain timeout unexpectedly committed")
	}
	cancelDrain()
	blockingPermit.Release()
	reopenedPermit, err := manager.Acquire(ctx, privacy.OwnerIdentity)
	if err != nil {
		t.Fatalf("failed barrier drain left manager closed: %v", err)
	}
	reopenedPermit.Release()
	barrier, err := store.CommitBarrier(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if barrier.Status != privacy.StatusBarrierCommitted || barrier.LearnerGeneration != 2 || barrier.RedactedThroughEventSeq != 0 || len(barrier.Steps) != len(privacy.ReceiptSlots) {
		t.Fatalf("barrier receipt=%+v", barrier)
	}
	var redactionSchema int
	var canonicalRedactionPayload bool
	if err := pool.QueryRow(ctx, `
		SELECT e.event_schema_version,
		       (SELECT count(*) FROM jsonb_object_keys(p.payload))=5
		       AND p.payload ? 'erasure_id'
		       AND p.payload ? 'generation'
		       AND p.payload ? 'redacted_through_event_seq'
		       AND NOT p.payload ? 'redacted_through'
		       AND p.payload ? 'policy_version'
		       AND p.payload ? 'reason_code'
		FROM privacy_redaction_barriers b
		JOIN learning_events e ON e.id=b.event_id
		JOIN learning_event_payloads p ON p.id=e.payload_id
		WHERE b.erasure_id=$1`, barrier.ErasureID).Scan(&redactionSchema, &canonicalRedactionPayload); err != nil {
		t.Fatal(err)
	}
	if redactionSchema != learning.EventRedactedSchemaVersion || !canonicalRedactionPayload {
		t.Fatalf("EventRedacted schema=%d canonical=%v", redactionSchema, canonicalRedactionPayload)
	}
	replayed, err := store.CommitBarrier(ctx, request)
	if err != nil || replayed.ErasureID != barrier.ErasureID {
		t.Fatalf("idempotent barrier=%+v err=%v", replayed, err)
	}
	conflict := request
	conflict.ReasonCode = string(privacy.ReasonAccountClosure)
	if _, err := store.CommitBarrier(ctx, conflict); privacy.ErrorCode(err) != privacy.CodeIdempotencyConflict {
		t.Fatalf("operation hash conflict err=%v", err)
	}
	if _, err := manager.Acquire(ctx, privacy.OwnerIdentity); privacy.ErrorCode(err) != privacy.CodeContentRedacted {
		t.Fatalf("closed in-process gate err=%v", err)
	}
	if _, err := identityStore.ListDevices(ctx); privacy.ErrorCode(err) != privacy.CodeContentRedacted {
		t.Fatalf("identity persistent read gate err=%v", err)
	}
	if _, err := knowledgeStore.Head(ctx); privacy.ErrorCode(err) != privacy.CodeContentRedacted {
		t.Fatalf("knowledge persistent read gate err=%v", err)
	}
	if _, err := learningStore.Session(ctx, uuid.NewString()); privacy.ErrorCode(err) != privacy.CodeContentRedacted {
		t.Fatalf("learning persistent read gate err=%v", err)
	}
	if _, err := tutoringStore.LoadSession(ctx, uuid.NewString()); privacy.ErrorCode(err) != privacy.CodeContentRedacted {
		t.Fatalf("tutoring persistent read gate err=%v", err)
	}
	partial, err := store.RunLocalScrub(ctx, barrier.ErasureID)
	if err == nil || partial.Status != privacy.StatusBarrierCommitted {
		t.Fatalf("injected failure receipt=%+v err=%v", partial, err)
	}
	var generation int64
	var readOpen, writeOpen bool
	if err := pool.QueryRow(ctx, `SELECT learner_generation,read_open,write_open FROM privacy_owner_generation_gates WHERE owner_kind='identity'`).Scan(&generation, &readOpen, &writeOpen); err != nil || generation != 2 || readOpen || writeOpen {
		t.Fatalf("barrier rolled back generation=%d read=%v write=%v err=%v", generation, readOpen, writeOpen, err)
	}
	complete, err := store.RunLocalScrub(ctx, barrier.ErasureID)
	if err != nil {
		t.Fatal(err)
	}
	if complete.Status != privacy.StatusLocalScrubbed {
		t.Fatalf("local receipt=%+v", complete)
	}
	resumed, err := store.RunLocalScrub(ctx, barrier.ErasureID)
	if err != nil || resumed.Status != privacy.StatusLocalScrubbed || resumed.ErasureID != complete.ErasureID {
		t.Fatalf("idempotent local scrub resume=%+v err=%v", resumed, err)
	}
	if replayed, err := store.CommitBarrier(ctx, request); err != nil || replayed.Status != privacy.StatusLocalScrubbed {
		t.Fatalf("post-scrub barrier replay=%+v err=%v", replayed, err)
	}
	identityPermit, err := manager.Acquire(ctx, privacy.OwnerIdentity)
	if err != nil {
		t.Fatalf("post-scrub replay closed reopened identity gate: %v", err)
	}
	identityPermit.Release()
	for _, step := range complete.Steps {
		switch step.Store {
		case privacy.StoreNocturnePaths, privacy.StoreNocturneOrphanHistory, privacy.StoreNocturneSnapshotChangeset, privacy.StoreManagedBackup:
			if step.Status != privacy.StepPending {
				t.Fatalf("remote step claimed complete: %+v", step)
			}
		case privacy.StoreExternalProvider:
			if step.Status != privacy.StepUnsupported {
				t.Fatalf("external step=%+v", step)
			}
		default:
			if step.Status != privacy.StepSucceeded {
				t.Fatalf("local step=%+v", step)
			}
		}
	}
	var label, outboxStatus, payload string
	if err := pool.QueryRow(ctx, `SELECT display_name FROM devices WHERE id=$1`, deviceID).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status,payload::text FROM outbox_messages WHERE idempotency_key='privacy-seed'`).Scan(&outboxStatus, &payload); err != nil {
		t.Fatal(err)
	}
	if label != "[redacted]" || outboxStatus != "canceled" || payload != `{"redacted": true}` {
		t.Fatalf("scrub label=%q outbox=%q payload=%q", label, outboxStatus, payload)
	}
	var learningHash, learningResult, tutoringHash, tutoringInput, tutoringStatus string
	if err := pool.QueryRow(ctx, `SELECT encode(request_hash,'hex'),result::text FROM learning_inbox WHERE device_id=$1 AND operation_id=$2`, deviceID, learningOperationID).Scan(&learningHash, &learningResult); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT encode(request_hash,'hex'),input::text,status FROM tutoring_proposal_requests WHERE device_id=$1 AND request_id=$2`, deviceID, tutoringRequestID).Scan(&tutoringHash, &tutoringInput, &tutoringStatus); err != nil {
		t.Fatal(err)
	}
	var candidatePayloadAvailable bool
	var candidateStatus string
	var candidateRevision, candidatePayloads, candidateDecisions int
	if err := pool.QueryRow(ctx, `SELECT status,revision,payload_available FROM memory_candidate_heads WHERE candidate_id=$1`, candidateID).Scan(&candidateStatus, &candidateRevision, &candidatePayloadAvailable); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memory_candidate_payloads WHERE candidate_id=$1`, candidateID).Scan(&candidatePayloads); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memory_candidate_decisions WHERE candidate_id=$1 AND decision='expire' AND reason='[redacted]'`, candidateID).Scan(&candidateDecisions); err != nil {
		t.Fatal(err)
	}
	if learningHash != strings.Repeat("ab", 32) || learningResult != `{"redacted": true}` || tutoringHash != strings.Repeat("cd", 32) || tutoringInput != `{"redacted": true}` || tutoringStatus != "failed" || candidateStatus != "expired" || candidateRevision != 2 || candidatePayloadAvailable || candidatePayloads != 0 || candidateDecisions != 1 {
		t.Fatalf("owner scrub learning_hash=%q learning_result=%q tutoring_hash=%q tutoring_input=%q tutoring_status=%q candidate_status=%q candidate_revision=%d candidate_available=%v candidate_payloads=%d candidate_decisions=%d", learningHash, learningResult, tutoringHash, tutoringInput, tutoringStatus, candidateStatus, candidateRevision, candidatePayloadAvailable, candidatePayloads, candidateDecisions)
	}
	var activeGenerations, timelineItems, oldProjectionItems int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_projection_generations WHERE status='active'`).Scan(&activeGenerations); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_projection_timeline t JOIN learning_projection_head h ON h.active_generation_id=t.generation_id WHERE t.item->>'event_type'='EventRedacted'`).Scan(&timelineItems); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_projection_routes)+(SELECT count(*) FROM learning_projection_sessions)+(SELECT count(*) FROM learning_projection_nodes)+(SELECT count(*) FROM learning_projection_evidence)+(SELECT count(*) FROM learning_projection_reviews)+(SELECT count(*) FROM learning_projection_misconceptions)+(SELECT count(*) FROM learning_projection_stats)`).Scan(&oldProjectionItems); err != nil {
		t.Fatal(err)
	}
	if activeGenerations != 1 || timelineItems != 1 || oldProjectionItems != 0 {
		t.Fatalf("redacted projection active=%d audit=%d residual=%d", activeGenerations, timelineItems, oldProjectionItems)
	}
	var closed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM privacy_owner_generation_gates WHERE learner_generation<>2 OR NOT read_open OR NOT write_open OR active_erasure_id IS NOT NULL`).Scan(&closed); err != nil || closed != 1 {
		t.Fatalf("unexpected closed generation gates=%d err=%v", closed, err)
	}
	var memoryReadOpen, memoryWriteOpen bool
	var memoryErasureID string
	if err := pool.QueryRow(ctx, `SELECT read_open,write_open,active_erasure_id::text FROM privacy_owner_generation_gates WHERE owner_kind='memory'`).Scan(&memoryReadOpen, &memoryWriteOpen, &memoryErasureID); err != nil || memoryReadOpen || memoryWriteOpen || memoryErasureID != barrier.ErasureID {
		t.Fatalf("memory gate reopened before remote purge read=%v write=%v erasure=%q err=%v", memoryReadOpen, memoryWriteOpen, memoryErasureID, err)
	}
	if _, err := manager.Acquire(ctx, privacy.OwnerMemory); privacy.ErrorCode(err) != privacy.CodeContentRedacted {
		t.Fatalf("in-process memory read gate reopened before remote purge err=%v", err)
	}
}

func TestKnowledgeRedactedRevisionTombstoneAllowsFreshImport(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'knowledge actor',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	knowledgeStore := knowledgedb.New(pool)
	knowledgeService, err := knowledge.NewService(knowledgeStore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := knowledgeService.Import(ctx, knowledge.ImportCommand{
		OperationID: uuid.NewString(), ExpectedParentProvided: true,
		Source: "private-import-source", ActorDeviceID: deviceID,
		Documents: []knowledge.ImportDocument{{Path: "private.md", Markdown: "# Private title\nprivate body needle\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	nodeRevisionID := first.Revision.Documents[0].Revision.Nodes[1].ID
	if _, err := pool.Exec(ctx, `
		INSERT INTO knowledge_node_artifacts(
			id,node_revision_id,kind,producer_version,prompt_version,model_version,
			input_hash,content,status,created_at)
		VALUES($1,$2,'summary','producer-v1','prompt-v1','model-v1',
		       decode(repeat('ab',32),'hex'),'private summary body','ready',clock_timestamp())`,
		uuid.NewString(), nodeRevisionID); err != nil {
		t.Fatal(err)
	}

	manager := privacy.NewReadPermitManager()
	tutoringStore := tutoringdb.New(pool)
	store := privacydb.New(pool,
		privacydb.WithReadPermits(manager),
		privacydb.WithLocalOwner(identitydb.New(pool)),
		privacydb.WithLocalOwner(knowledgeStore),
		privacydb.WithLocalOwner(learningdb.New(pool, tutoringStore)),
		privacydb.WithLocalOwner(tutoringStore),
		privacydb.WithLocalOwner(memorydb.New(pool)),
		privacydb.WithLocalOwner(outboxdb.New(pool)),
	)
	now := time.Now().UTC()
	barrier, err := store.CommitBarrier(ctx, privacy.ErasureRequest{
		DeviceID: deviceID, OperationID: uuid.NewString(), ActorDeviceID: deviceID,
		ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now,
		ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}

	head, err := knowledgeService.Head(ctx)
	if err != nil || head == nil || head.ID != first.Revision.ID || !head.Redacted || len(head.Documents) != 0 || len(head.Lineages) != 0 {
		t.Fatalf("redacted head=%+v err=%v", head, err)
	}
	tree, err := knowledgeService.Tree(ctx, first.Revision.ID)
	if err != nil || !tree.Revision.Redacted || len(tree.Revision.Documents) != 0 || len(tree.Revision.Lineages) != 0 {
		t.Fatalf("redacted tree=%+v err=%v", tree, err)
	}
	if _, err := knowledgeService.Export(ctx, first.Revision.ID); knowledge.ErrorCode(err) != knowledge.CodeContentRedacted {
		t.Fatalf("old revision export error=%v", err)
	}
	oldRevisionID := first.Revision.ID
	if _, err := knowledgeService.Retrieve(ctx, knowledge.RetrievalCommand{Query: "private", KnowledgeRevisionID: &oldRevisionID}); knowledge.ErrorCode(err) != knowledge.CodeContentRedacted {
		t.Fatalf("old revision retrieval error=%v", err)
	}
	var placeholderRows, residualBodies int64
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM knowledge_document_payloads WHERE canonical_markdown LIKE '%[redacted]%')+
		  (SELECT count(*) FROM knowledge_node_revisions WHERE title LIKE '%[redacted]%')+
		  (SELECT count(*) FROM knowledge_node_artifacts WHERE content LIKE '%[redacted]%')+
		  (SELECT count(*) FROM knowledge_snapshot_documents WHERE canonical_path LIKE '%[redacted]%' OR folded_path LIKE '%[redacted]%')+
		  (SELECT count(*) FROM knowledge_revisions WHERE source LIKE '%[redacted]%')+
		  (SELECT count(*) FROM knowledge_lineages WHERE reason LIKE '%[redacted]%'),
		  (SELECT count(*) FROM knowledge_document_payloads WHERE canonical_markdown<>'')+
		  (SELECT count(*) FROM knowledge_node_revisions WHERE title<>'')+
		  (SELECT count(*) FROM knowledge_node_artifacts WHERE content<>'')`).Scan(&placeholderRows, &residualBodies); err != nil {
		t.Fatal(err)
	}
	if placeholderRows != 0 || residualBodies != 0 {
		t.Fatalf("knowledge scrub placeholders=%d residual_bodies=%d", placeholderRows, residualBodies)
	}

	parent := first.Revision.ID
	fresh, err := knowledgeService.Import(ctx, knowledge.ImportCommand{
		OperationID: uuid.NewString(), ExpectedParentRevisionID: &parent, ExpectedParentProvided: true,
		Source: "post-erasure-import", ActorDeviceID: deviceID,
		Documents: []knowledge.ImportDocument{{Path: "fresh.md", Markdown: "# Fresh title\nfresh body needle\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Revision.Redacted || fresh.Revision.RevisionNo != first.Revision.RevisionNo+1 || len(fresh.Revision.Documents) != 1 {
		t.Fatalf("fresh revision=%+v", fresh.Revision)
	}
	freshExport, err := knowledgeService.Export(ctx, fresh.Revision.ID)
	if err != nil || len(freshExport.Documents) != 1 || !strings.Contains(freshExport.Documents[0].Markdown, "fresh body needle") {
		t.Fatalf("fresh export=%+v err=%v", freshExport, err)
	}
	if _, err := knowledgeService.Export(ctx, first.Revision.ID); knowledge.ErrorCode(err) != knowledge.CodeContentRedacted {
		t.Fatalf("old revision export after fresh import error=%v", err)
	}
}

func TestBarrierDestroysManagedBackupKeysAtomically(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	deviceID, keyID, backupID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'backup actor',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memory_generation_keys(id,learner_generation,wrapped_key,key_digest,created_at) VALUES($1,1,decode(repeat('aa',48),'hex'),decode(repeat('bb',32),'hex'),clock_timestamp())`, keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memory_managed_backup_inventory(id,relative_path,created_at,size_bytes,artifact_hash,learner_generation,wrapped_key_id) VALUES($1,'generation-1.backup.enc',clock_timestamp(),128,decode(repeat('cc',32),'hex'),1,$2)`, backupID, keyID); err != nil {
		t.Fatal(err)
	}
	manager := privacy.NewReadPermitManager()
	now := time.Now().UTC()
	request := privacy.ErasureRequest{DeviceID: deviceID, OperationID: uuid.NewString(), ActorDeviceID: deviceID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}
	failing := privacydb.New(pool, privacydb.WithReadPermits(manager))
	if _, err := failing.CommitBarrier(ctx, request); err == nil {
		t.Fatal("barrier without learning owner unexpectedly committed")
	}
	var wrappedStillPresent, inventoryStillUnbound bool
	if err := pool.QueryRow(ctx, `SELECT wrapped_key IS NOT NULL AND destroyed_at IS NULL FROM memory_generation_keys WHERE id=$1`, keyID).Scan(&wrappedStillPresent); err != nil || !wrappedStillPresent {
		t.Fatalf("failed barrier did not roll back key destruction present=%v err=%v", wrappedStillPresent, err)
	}
	if err := pool.QueryRow(ctx, `SELECT erasure_id IS NULL FROM memory_managed_backup_inventory WHERE id=$1`, backupID).Scan(&inventoryStillUnbound); err != nil || !inventoryStillUnbound {
		t.Fatalf("failed barrier did not roll back inventory binding unbound=%v err=%v", inventoryStillUnbound, err)
	}
	tutoringStore := tutoringdb.New(pool)
	store := privacydb.New(pool,
		privacydb.WithReadPermits(manager),
		privacydb.WithLocalOwner(identitydb.New(pool)),
		privacydb.WithLocalOwner(knowledgedb.New(pool)),
		privacydb.WithLocalOwner(learningdb.New(pool, tutoringStore)),
		privacydb.WithLocalOwner(tutoringStore),
		privacydb.WithLocalOwner(memorydb.New(pool)),
		privacydb.WithLocalOwner(outboxdb.New(pool)),
	)
	barrier, err := store.CommitBarrier(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	var wrappedDestroyed, evidencePresent bool
	var destroyedAt, verifiedAt time.Time
	var boundErasureID string
	if err := pool.QueryRow(ctx, `SELECT wrapped_key IS NULL,destruction_evidence_digest IS NOT NULL,destroyed_at FROM memory_generation_keys WHERE id=$1`, keyID).Scan(&wrappedDestroyed, &evidencePresent, &destroyedAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT erasure_id::text FROM memory_managed_backup_inventory WHERE id=$1`, backupID).Scan(&boundErasureID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT managed_backup_verified_unrecoverable_at FROM privacy_erasures WHERE id=$1`, barrier.ErasureID).Scan(&verifiedAt); err != nil {
		t.Fatal(err)
	}
	if !wrappedDestroyed || !evidencePresent || destroyedAt.IsZero() || !verifiedAt.Equal(destroyedAt) || verifiedAt.After(request.ManagedBackupUnrecoverableAfter) || boundErasureID != barrier.ErasureID {
		t.Fatalf("backup key destruction wrapped=%v evidence=%v destroyed=%s verified=%s deadline=%s bound_erasure=%s", wrappedDestroyed, evidencePresent, destroyedAt, verifiedAt, request.ManagedBackupUnrecoverableAfter, boundErasureID)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_managed_backup_inventory SET erasure_id=NULL WHERE id=$1`, backupID); err == nil || !strings.Contains(err.Error(), "managed backup erasure binding is immutable") {
		t.Fatalf("managed backup erasure binding was cleared: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_managed_backup_inventory SET erasure_id=$2 WHERE id=$1`, backupID, uuid.NewString()); err == nil || !strings.Contains(err.Error(), "managed backup erasure binding is immutable") {
		t.Fatalf("managed backup erasure binding was rewritten: %v", err)
	}
}

func TestMigrationLeaseAndPrivacyBarrierAreMutuallyExclusive(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'migration actor',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	tutoringStore := tutoringdb.New(pool)
	store := privacydb.New(pool,
		privacydb.WithReadPermits(privacy.NewReadPermitManager()),
		privacydb.WithLocalOwner(identitydb.New(pool)),
		privacydb.WithLocalOwner(knowledgedb.New(pool)),
		privacydb.WithLocalOwner(learningdb.New(pool, tutoringStore)),
		privacydb.WithLocalOwner(tutoringStore),
		privacydb.WithLocalOwner(memorydb.New(pool)),
		privacydb.WithLocalOwner(outboxdb.New(pool)),
	)
	leaseRequest := privacy.MigrationLeaseRequest{OperationID: uuid.NewString(), BackupIdentity: strings.Repeat("ab", 32)}
	lease, err := store.AcquireMigrationLease(ctx, leaseRequest)
	if err != nil || lease.OperationID != leaseRequest.OperationID || lease.Replayed {
		t.Fatalf("acquire migration lease=%+v err=%v", lease, err)
	}
	replayed, err := store.AcquireMigrationLease(ctx, leaseRequest)
	if err != nil || !replayed.Replayed || !replayed.AcquiredAt.Equal(lease.AcquiredAt) {
		t.Fatalf("reacquire migration lease=%+v err=%v", replayed, err)
	}
	changedIdentity := leaseRequest
	changedIdentity.BackupIdentity = strings.Repeat("cd", 32)
	if _, err := store.AcquireMigrationLease(ctx, changedIdentity); privacy.ErrorCode(err) != privacy.CodeMigrationLeaseConflict {
		t.Fatalf("migration identity mismatch error=%v", err)
	}
	otherOperation := leaseRequest
	otherOperation.OperationID = uuid.NewString()
	if _, err := store.AcquireMigrationLease(ctx, otherOperation); privacy.ErrorCode(err) != privacy.CodeMigrationLeaseConflict {
		t.Fatalf("second migration operation error=%v", err)
	}

	now := time.Now().UTC()
	erasureRequest := privacy.ErasureRequest{
		DeviceID: deviceID, OperationID: uuid.NewString(), ActorDeviceID: deviceID,
		ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now,
		ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1,
	}
	if _, err := store.CommitBarrier(ctx, erasureRequest); privacy.ErrorCode(err) != privacy.CodeMigrationLeaseConflict {
		t.Fatalf("active migration did not block barrier: %v", err)
	}
	if err := store.ReleaseMigrationLease(ctx, changedIdentity); privacy.ErrorCode(err) != privacy.CodeMigrationLeaseConflict {
		t.Fatalf("migration release identity mismatch error=%v", err)
	}
	if err := store.ReleaseMigrationLease(ctx, leaseRequest); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseMigrationLease(ctx, leaseRequest); err != nil {
		t.Fatalf("idempotent migration release: %v", err)
	}
	barrier, err := store.CommitBarrier(ctx, erasureRequest)
	if err != nil || barrier.Status != privacy.StatusBarrierCommitted {
		t.Fatalf("barrier after release=%+v err=%v", barrier, err)
	}
	if _, err := store.AcquireMigrationLease(ctx, privacy.MigrationLeaseRequest{
		OperationID: uuid.NewString(), BackupIdentity: strings.Repeat("ef", 32),
	}); privacy.ErrorCode(err) != privacy.CodeMigrationLeaseConflict {
		t.Fatalf("active erasure did not block migration: %v", err)
	}
}

type scriptedRemoteEraser struct {
	results  []privacy.RemoteEraseResult
	requests []privacy.RemoteEraseRequest
}

func (s *scriptedRemoteEraser) Erase(_ context.Context, request privacy.RemoteEraseRequest) (privacy.RemoteEraseResult, error) {
	s.requests = append(s.requests, request)
	if len(s.results) == 0 {
		return privacy.RemoteEraseResult{}, errors.New("unexpected remote eraser call")
	}
	result := s.results[0]
	s.results = s.results[1:]
	return result, nil
}

func TestRunNocturneEraseResumesAndOpensMemoryGateOnlyAfterSuccess(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'remote actor',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	manager := privacy.NewReadPermitManager()
	identityStore := identitydb.New(pool)
	knowledgeStore := knowledgedb.New(pool)
	tutoringStore := tutoringdb.New(pool)
	learningStore := learningdb.New(pool, tutoringStore)
	memoryStore := memorydb.New(pool)
	store := privacydb.New(pool,
		privacydb.WithReadPermits(manager),
		privacydb.WithLocalOwner(identityStore),
		privacydb.WithLocalOwner(knowledgeStore),
		privacydb.WithLocalOwner(learningStore),
		privacydb.WithLocalOwner(tutoringStore),
		privacydb.WithLocalOwner(memoryStore),
		privacydb.WithLocalOwner(outboxdb.New(pool)),
	)
	now := time.Now().UTC()
	request := privacy.ErasureRequest{DeviceID: deviceID, OperationID: uuid.NewString(), ActorDeviceID: deviceID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}
	barrier, err := store.CommitBarrier(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
	eraser := &scriptedRemoteEraser{results: []privacy.RemoteEraseResult{
		{Status: privacy.StepUnknown, StableReason: "nocturne_unavailable", EvidenceDigest: strings.Repeat("11", 32), CompletedAt: time.Now().UTC()},
		{Status: privacy.StepSucceeded, StableReason: "nocturne_absence_verified", EvidenceDigest: strings.Repeat("22", 32), CompletedAt: time.Now().UTC()},
	}}
	partial, err := store.RunNocturneErase(ctx, barrier.ErasureID, eraser)
	if err != nil || partial.Status != privacy.StatusPartial {
		t.Fatalf("partial remote erase=%+v err=%v", partial, err)
	}
	var gateOpen bool
	if err := pool.QueryRow(ctx, `SELECT read_open AND write_open FROM privacy_owner_generation_gates WHERE owner_kind='memory'`).Scan(&gateOpen); err != nil || gateOpen {
		t.Fatalf("memory gate opened before remote success open=%v err=%v", gateOpen, err)
	}
	complete, err := store.RunNocturneErase(ctx, barrier.ErasureID, eraser)
	if err != nil || complete.Status != privacy.StatusRemotePurged {
		t.Fatalf("complete remote erase=%+v err=%v", complete, err)
	}
	if err := pool.QueryRow(ctx, `SELECT read_open AND write_open FROM privacy_owner_generation_gates WHERE owner_kind='memory'`).Scan(&gateOpen); err != nil || !gateOpen {
		t.Fatalf("memory gate remained closed after remote success open=%v err=%v", gateOpen, err)
	}
	var remoteSucceeded, managedBackupPending int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE h.store_kind IN ('nocturne_paths','nocturne_orphan_history','nocturne_snapshot_changeset') AND r.version=3 AND r.status='succeeded'),
		  count(*) FILTER (WHERE h.store_kind='managed_backup' AND r.status='pending')
		FROM privacy_erasure_receipt_heads h
		JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
		WHERE h.erasure_id=$1`, barrier.ErasureID).Scan(&remoteSucceeded, &managedBackupPending); err != nil {
		t.Fatal(err)
	}
	if remoteSucceeded != 3 || managedBackupPending != 1 {
		t.Fatalf("remote receipts succeeded=%d managed_backup_pending=%d", remoteSucceeded, managedBackupPending)
	}
	if _, err := store.RunNocturneErase(ctx, barrier.ErasureID, eraser); err != nil || len(eraser.requests) != 2 {
		t.Fatalf("remote purge replay called eraser again calls=%d err=%v", len(eraser.requests), err)
	}
}

func privacyIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := "privacy_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+identifier+` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+identifier+` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, pool); err != nil {
		pool.Close()
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+identifier+` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+identifier+` CASCADE`)
		admin.Close()
	})
	return pool
}

func TestBarrierWaitsForPriorOwnerWriteAndRejectsStaleWriter(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	actorID, committedWriterID, staleWriterID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'actor',clock_timestamp())`, actorID); err != nil {
		t.Fatal(err)
	}
	tutoringStore := tutoringdb.New(pool)
	store := privacydb.New(pool,
		privacydb.WithReadPermits(privacy.NewReadPermitManager()),
		privacydb.WithLocalOwner(identitydb.New(pool)),
		privacydb.WithLocalOwner(knowledgedb.New(pool)),
		privacydb.WithLocalOwner(learningdb.New(pool, tutoringStore)),
		privacydb.WithLocalOwner(tutoringStore),
		privacydb.WithLocalOwner(memorydb.New(pool)),
		privacydb.WithLocalOwner(outboxdb.New(pool)),
	)
	staleTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = staleTx.Rollback(context.Background()) }()
	if _, err := staleTx.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	writerTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writerTx.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'committed before barrier',clock_timestamp())`, committedWriterID); err != nil {
		_ = writerTx.Rollback(ctx)
		t.Fatal(err)
	}
	type barrierResult struct {
		receipt privacy.ErasureReceipt
		err     error
	}
	result := make(chan barrierResult, 1)
	request := privacy.ErasureRequest{DeviceID: actorID, OperationID: uuid.NewString(), ActorDeviceID: actorID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: time.Now().UTC(), ManagedBackupUnrecoverableAfter: time.Now().UTC().Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}
	go func() {
		receipt, err := store.CommitBarrier(ctx, request)
		result <- barrierResult{receipt: receipt, err: err}
	}()
	select {
	case early := <-result:
		t.Fatalf("barrier bypassed prior owner write: receipt=%+v err=%v", early.receipt, early.err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := writerTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var barrier barrierResult
	select {
	case barrier = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("barrier did not resume after prior owner write committed")
	}
	if barrier.err != nil {
		t.Fatal(barrier.err)
	}
	if _, err := store.RunLocalScrub(ctx, barrier.receipt.ErasureID); err != nil {
		t.Fatal(err)
	}
	if _, err := staleTx.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'stale writer',clock_timestamp())`, staleWriterID); err == nil || !strings.Contains(err.Error(), "privacy owner generation changed") {
		t.Fatalf("stale owner write crossed barrier and reopen: %v", err)
	}
	var label string
	if err := pool.QueryRow(ctx, `SELECT display_name FROM devices WHERE id=$1`, committedWriterID).Scan(&label); err != nil || label != "[redacted]" {
		t.Fatalf("pre-barrier committed write was not scrubbed label=%q err=%v", label, err)
	}
}
