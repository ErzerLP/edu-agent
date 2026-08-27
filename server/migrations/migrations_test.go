package migrations

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEmbeddedMigrationsAreOrderedAndUnique(t *testing.T) {
	items, err := load()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].version >= items[i].version {
			t.Fatalf("migrations are not strictly ordered: %+v", items)
		}
	}
	if items[0].checksum == "" || len(items[0].body) == 0 {
		t.Fatal("migration checksum or body is empty")
	}
	latest := items[len(items)-1]
	if latest.version != 11 || latest.name != "000011_knowledge_maintenance.sql" || len(latest.checksum) != 64 {
		t.Fatalf("knowledge maintenance migration was not embedded with checksum: %+v", latest)
	}
}

func TestKnowledgeMaintenanceMigrationDeclaresPairingScopeProfiles(t *testing.T) {
	items, err := load()
	if err != nil {
		t.Fatal(err)
	}
	body := migrationBody(t, items, 11)
	for _, required := range []string{
		"ALTER TABLE pairing_codes ADD COLUMN scopes TEXT[]",
		"'learning:approve'",
		"ALTER TABLE pairing_codes ALTER COLUMN scopes SET NOT NULL",
		"cardinality(scopes)>0",
		"array_position(scopes,NULL) IS NULL",
		"array_position(scopes,'') IS NULL",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("000011 pairing scope migration is missing %q", required)
		}
	}
}

func TestKnowledgeMaintenanceMigrationBackfillsPairingCodeScopes(t *testing.T) {
	pool := migrationPoolThrough(t, 10)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO pairing_codes(lookup_id,code_hash,created_at,expires_at,max_attempts)
		VALUES('legacy-user-code',decode(repeat('11',32),'hex'),now(),now()+interval '10 minutes',5)`); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatalf("upgrade pairing code scopes through 000011: %v", err)
	}

	var scopes []string
	if err := pool.QueryRow(ctx, `SELECT scopes FROM pairing_codes WHERE lookup_id='legacy-user-code'`).Scan(&scopes); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"devices:manage", "knowledge:read", "knowledge:write", "learning:read", "learning:write", "learning:approve", "privacy:device"} {
		if !containsScope(scopes, required) {
			t.Fatalf("upgraded user pairing code missing %s: %v", required, scopes)
		}
	}

	invalidScopes := []struct {
		name  string
		value string
	}{
		{name: "null", value: "NULL"},
		{name: "empty array", value: "ARRAY[]::TEXT[]"},
		{name: "null element", value: "ARRAY['knowledge:read',NULL]::TEXT[]"},
		{name: "empty element", value: "ARRAY['']::TEXT[]"},
	}
	for index, test := range invalidScopes {
		t.Run(test.name, func(t *testing.T) {
			query := fmt.Sprintf(`
				INSERT INTO pairing_codes(lookup_id,code_hash,scopes,created_at,expires_at,max_attempts)
				VALUES($1,decode(repeat('22',32),'hex'),%s,now(),now()+interval '10 minutes',5)`, test.value)
			if _, err := pool.Exec(ctx, query, fmt.Sprintf("invalid-scopes-%d", index)); err == nil {
				t.Fatalf("pairing code accepted invalid scopes %s", test.value)
			}
		})
	}
}

func TestProjectionMigrationsPreserveV1SeedAndRequireV2Rebuild(t *testing.T) {
	const projectionV1EmptyFingerprint = "2b2fe0642e3c18f6c9a9adb8fc4e8195acf5d426c906a13db6ff1434086fe831"

	items, err := load()
	if err != nil {
		t.Fatal(err)
	}
	currentFingerprint, err := learning.ProjectionFingerprint(learning.EmptyProjection("migration-generation"))
	if err != nil {
		t.Fatal(err)
	}
	if currentFingerprint == projectionV1EmptyFingerprint {
		t.Fatal("projection v2 must not reuse the projection v1 semantic fingerprint")
	}
	legacyBody := migrationBody(t, items, 3)
	legacySeed := "decode('" + projectionV1EmptyFingerprint + "', 'hex')"
	if count := strings.Count(legacyBody, legacySeed); count != 2 {
		t.Fatalf("000003 must preserve the versioned v1 generation and checkpoint fingerprint; occurrences=%d", count)
	}
	if !strings.Contains(legacyBody, "'learning-projection-v1'") {
		t.Fatal("000003 no longer declares the projection version for its historical fingerprint")
	}
	if strings.Contains(legacyBody, "decode(repeat('00', 32), 'hex')") {
		t.Fatal("000003 uses the obsolete all-zero projection fingerprint sentinel")
	}

	upgradeBody := migrationBody(t, items, 7)
	for _, required := range []string{
		"projection_version_upgrade_required",
		"projection_version<>'learning-projection-v2'",
	} {
		if !strings.Contains(upgradeBody, required) {
			t.Fatalf("000007 does not require a versioned projection rebuild: missing %q", required)
		}
	}
}

func TestLearningCoreMigrationDeclaresRequiredDurabilityBoundaries(t *testing.T) {
	items, err := load()
	if err != nil {
		t.Fatal(err)
	}
	body := migrationBody(t, items, 3)
	for _, required := range []string{
		"CREATE TABLE learning_event_clock",
		"CREATE TABLE learning_aggregate_heads",
		"CREATE TABLE learning_inbox",
		"CREATE TABLE learning_event_payloads",
		"CREATE TABLE learning_events",
		"CREATE TABLE learning_goal_revisions",
		"CREATE TABLE learning_route_revisions",
		"CREATE TABLE learning_route_steps",
		"CREATE TABLE tutoring_sessions",
		"CREATE TABLE tutoring_focus_frames",
		"CREATE TABLE tutoring_free_questions",
		"CREATE TABLE tutoring_free_answers",
		"CREATE TABLE tutoring_proposal_requests",
		"CREATE TABLE tutoring_proposal_artifacts",
		"CREATE TABLE learning_activities",
		"CREATE TABLE learning_attempts",
		"CREATE TABLE learning_assessments",
		"CREATE TABLE learning_assessment_decisions",
		"CREATE TABLE learning_evidence",
		"CREATE TABLE learning_evidence_invalidations",
		"CREATE TABLE learning_misconception_revisions",
		"CREATE TABLE learning_projection_generations",
		"knowledge_revision_id UUID REFERENCES knowledge_revisions(id)",
		"CREATE TABLE learning_projection_head",
		"CREATE TABLE learning_projection_checkpoints",
		"CREATE TABLE learning_projection_stats",
		"tutoring_focus_frame_single_active",
		"learning_projection_single_active",
		"learning_event_payload_owner",
		"learning_assessment_item_node_owner",
		"learning_assessment_item_snapshot_owner",
		"learning_assessment_item_provenance_shape",
		"learning_assessment_attempt_owner",
		"learning_evidence_decision_owner",
		"learning_evidence_assessment_owner",
		"learning_evidence_attempt_owner",
		"learning_evidence_activity_owner",
		"learning_evidence_node_owner",
		"learning_evidence_snapshot_owner",
		"learning_projection_rebuild_lease_shape",
		"rebuild_lease_token UUID",
		"rebuild_lease_expires_at TIMESTAMPTZ",
		"rubric_outcomes JSONB NOT NULL",
		"reject_learning_history_mutation",
		"ARRAY['learning:read', 'learning:write']",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("000003 is missing %q", required)
		}
	}
	if strings.Contains(body, "CREATE SEQUENCE") {
		t.Fatal("learning events must use the locked singleton clock, not a PostgreSQL sequence")
	}
	if strings.Count(body, "MATCH FULL") < 2 {
		t.Fatal("assessment provenance composite foreign keys must reject partial NULL ownership")
	}
	if strings.Index(body, "knowledge_snapshot_revision_document_unique") > strings.Index(body, "learning_assessment_item_snapshot_owner") ||
		strings.Index(body, "knowledge_node_revision_full_identity_unique") > strings.Index(body, "learning_assessment_item_node_owner") {
		t.Fatal("assessment provenance composite unique prerequisites must precede their foreign keys")
	}
	if strings.Contains(body, "CREATE EXTENSION") {
		t.Fatal("learning core must not introduce a PostgreSQL extension dependency")
	}
}

func TestMemoryBridgeMigrationDeclaresDurabilityAndPrivacyBoundaries(t *testing.T) {
	items, err := load()
	if err != nil {
		t.Fatal(err)
	}
	body := migrationBody(t, items, 4)
	for _, required := range []string{
		"LOCK TABLE outbox_messages IN ACCESS EXCLUSIVE MODE",
		"ALTER COLUMN status TYPE TEXT",
		"'canceled'",
		"terminal_disposition",
		"CREATE TABLE memory_candidates",
		"CREATE TABLE memory_candidate_payloads",
		"CREATE TABLE memory_candidate_heads",
		"CREATE TABLE memory_candidate_decisions",
		"CREATE TABLE memory_operation_inbox",
		"CREATE TABLE memory_record_revisions",
		"CREATE TABLE memory_record_tombstones",
		"CREATE TABLE memory_record_heads",
		"CREATE TABLE memory_deliveries",
		"CREATE TABLE memory_delivery_payloads",
		"CREATE TABLE memory_delivery_heads",
		"CREATE TABLE memory_delivery_attempts",
		"CREATE TABLE memory_delivery_attempt_heads",
		"memory_delivery_attempt_single_active",
		"CREATE TABLE memory_expiry_reconciliations",
		"memory_expiry_reconciliation_single_claim",
		"CREATE TABLE memory_reconciliation_maintenance_claims",
		"CREATE TABLE memory_delivery_receipts",
		"CREATE TABLE privacy_erasures",
		"privacy_erasure_single_active",
		"CREATE TABLE privacy_erasure_step_receipts",
		"CREATE TABLE privacy_erasure_grants",
		"privacy_erasure_grant_attempt_budget",
		"protect_privacy_erasure_grant_state",
		"privacy_erasure_grant_state_guard",
		"privacy erasure grant consumption is irreversible",
		"UPDATE device_tokens AS token",
		"token.revoked_at IS NULL",
		"device.revoked_at IS NULL",
		"ARRAY['memory:read','memory:write','privacy:read']",
		"CREATE TABLE privacy_redaction_barriers",
		"CREATE TABLE privacy_owner_generation_gates",
		"CREATE TABLE privacy_owner_redaction_audit",
		"CREATE TABLE privacy_owner_scrub_permits",
		"privacy_begin_owner_scrub",
		"CREATE OR REPLACE FUNCTION reject_learning_history_mutation",
		"CREATE TABLE memory_generation_keys",
		"memory_generation_key_id_generation",
		"protect_memory_generation_key_lifecycle",
		"memory_generation_key_lifecycle_immutable",
		"CREATE TABLE memory_managed_backup_inventory",
		"memory_managed_backup_inventory_key_generation",
		"FOREIGN KEY (wrapped_key_id,learner_generation)",
		"reject_memory_history_mutation",
		"memory_delivery_attempt_sent_not_failed",
		"memory_candidate_decision_identity",
		"memory_record_revision_identity",
		"memory_record_previous_owner",
		"memory_record_delivery_owner",
		"DEFERRABLE INITIALLY DEFERRED",
		"memory_delivery_record_owner",
		"memory_delivery_expiry_identity",
		"memory_delivery_attempt_token_identity",
		"memory_expiry_reconciliation_delivery_fk",
		"memory_expiry_reconciliation_attempt_fk",
		"protect_memory_expiry_reconciliation_identity",
		"memory_expiry_reconciliation_identity_immutable",
		"privacy_erasure_receipt_head_owner",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("000004 is missing %q", required)
		}
	}
	if strings.Contains(body, "privacy:erase") {
		t.Fatal("000004 must never grant privacy:erase")
	}
	if strings.Contains(body, "nocturne_") && !strings.Contains(body, "nocturne_paths") {
		t.Fatal("000004 must not introduce a private Nocturne database schema")
	}
	if strings.Contains(body, "CREATE TABLE memory_generation_gate") {
		t.Fatal("memory must use the shared privacy owner generation gate")
	}
	if !strings.Contains(body, "terminal_disposition IS NOT NULL") || !strings.Contains(body, "'deleted'") {
		t.Fatal("canceled outbox rows require an explicit complete terminal disposition")
	}
}

func TestNoteSyncBridgeMigrationDeclaresPersistenceAndPrivacyBoundaries(t *testing.T) {
	items, err := load()
	if err != nil {
		t.Fatal(err)
	}
	body := migrationBody(t, items, 10)
	for _, required := range []string{
		"'review_required'",
		"knowledge_revision_id_number_unique",
		"knowledge_snapshot_revision_document_revision_unique",
		"CREATE TABLE knowledge_notesync_publications",
		"knowledge_notesync_publication_remote_current",
		"protect_knowledge_notesync_publication_progress",
		"knowledge notesync publication cannot move backward",
		"published_knowledge_revision_id",
		"published_document_revision_id",
		"published_revision_no BIGINT NOT NULL CHECK (published_revision_no >= 1)",
		"base_markdown TEXT NOT NULL CHECK (octet_length(base_markdown) <= 4194304)",
		"base_sha256 BYTEA NOT NULL CHECK (octet_length(base_sha256) = 32)",
		"remote_version BIGINT",
		"remote_last_time BIGINT",
		"generation BIGINT NOT NULL CHECK (generation >= 1)",
		"CREATE TABLE knowledge_notesync_publication_attempts",
		"outbox_id UUID NOT NULL UNIQUE REFERENCES outbox_messages(id)",
		"'prepared','unknown','retryable','applied','review_required','superseded','redacted'",
		"knowledge_notesync_attempt_idempotency_shape",
		"document_revision_id::text || ':' || knowledge_revision_no::text || ':'",
		"knowledge_notesync_attempt_base_shape",
		"protect_knowledge_notesync_attempt_identity",
		"knowledge notesync publication attempt identity is immutable",
		"CREATE TABLE knowledge_notesync_reviews",
		"knowledge_notesync_review_single_open_basis",
		"knowledge_notesync_review_status_shape",
		"knowledge_notesync_review_base_shape",
		"knowledge_notesync_review_local_shape",
		"knowledge_notesync_review_remote_shape",
		"remote_version IS NOT NULL AND remote_last_time IS NOT NULL",
		"base_remote_path TEXT",
		"base_remote_version BIGINT",
		"base_remote_last_time BIGINT",
		"base_to_local_diff TEXT NOT NULL",
		"base_to_remote_diff TEXT NOT NULL",
		"knowledge_notesync_resolution_review_operation_unique",
		"UNIQUE(review_id,device_id,operation_id)",
		"FOREIGN KEY(review_id,resolved_by_device_id,resolution_operation_id)",
		"REFERENCES knowledge_notesync_resolution_operations(review_id,device_id,operation_id)",
		"knowledge notesync review must leave open exactly once",
		"knowledge notesync review terminal resolution is immutable",
		"knowledge notesync resolution operation is immutable",
		"resolved_document_id IS NULL",
		"protect_knowledge_notesync_review_snapshot",
		"knowledge notesync review snapshots are immutable",
		"privacy_enforce_owner_write",
		"'knowledge_notesync_publications'",
		"'knowledge_notesync_publication_attempts'",
		"'knowledge_notesync_reviews'",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("000010 is missing %q", required)
		}
	}
	if strings.Contains(body, "FOREIGN KEY(resolved_by_device_id,resolution_operation_id)") {
		t.Fatal("000010 resolution operation ownership FK omits review_id")
	}
	operationGuard := body[strings.Index(body, "CREATE FUNCTION protect_knowledge_notesync_resolution_operation"):]
	if !strings.Contains(operationGuard, "TG_OP='DELETE' OR NEW IS DISTINCT FROM OLD") ||
		!strings.Contains(operationGuard, "privacy_owner_scrub_permitted('knowledge')") {
		t.Fatal("000010 resolution operations are not fully immutable outside privacy scrub")
	}
	if strings.Contains(strings.ToLower(body), "api_token") || strings.Contains(strings.ToLower(body), "authorization") {
		t.Fatal("000010 must not persist Fast Note Sync credentials")
	}
}

func TestMemoryContractRepairMigrationDeclaresRevisionAndErasureBoundaries(t *testing.T) {
	items, err := load()
	if err != nil {
		t.Fatal(err)
	}
	body := migrationBody(t, items, 5)
	for _, required := range []string{
		"CREATE TABLE memory_record_external_refs",
		"memory_record_external_refs_immutable",
		"CREATE TABLE memory_erasure_deliveries",
		"memory_erasure_delivery_once",
		"CREATE TABLE memory_erasure_delivery_sources",
		"CREATE TABLE memory_erasure_delivery_attempts",
		"CREATE TABLE memory_erasure_delivery_attempt_heads",
		"memory_erasure_delivery_single_active_attempt",
		"CREATE TABLE memory_erasure_delivery_receipts",
		"CREATE TABLE memory_erasure_delivery_scopes",
		"ADD COLUMN erasure_delivery_id UUID UNIQUE",
		"ADD COLUMN redacted_at TIMESTAMPTZ",
		"ADD COLUMN redacted_by_erasure_id UUID",
		"knowledge_revision_redaction_shape",
		"memory_managed_backup_erasure_binding_immutable",
		"managed backup erasure binding is immutable",
		"CREATE TABLE privacy_migration_lease",
		"privacy_migration_lease_shape",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("000005 is missing %q", required)
		}
	}
}

func TestOfflineSyncCoreMigrationDeclaresAuthorityAndPrivacyBoundaries(t *testing.T) {
	items, err := load()
	if err != nil {
		t.Fatal(err)
	}
	body := migrationBody(t, items, 7)
	for _, required := range []string{
		"CREATE TABLE offline_prepare_claims",
		"CREATE TABLE offline_activities",
		"CREATE TABLE offline_activity_references",
		"CREATE TABLE offline_packs",
		"CREATE TABLE offline_submission_authorizations",
		"CREATE TABLE offline_device_sequence_heads",
		"CREATE TABLE offline_device_sequence_reservations",
		"CREATE TABLE offline_device_sequence_claims",
		"CREATE TABLE offline_attempt_heads",
		"CREATE TABLE learning_activity_evidence_claims",
		"CREATE TABLE offline_operation_statuses",
		"CREATE TABLE offline_operation_status_revisions",
		"CREATE TABLE offline_evaluation_jobs",
		"CREATE TABLE offline_device_possessions",
		"CREATE TABLE privacy_offline_device_children",
		"CREATE TABLE privacy_offline_device_child_revisions",
		"accepted_event_seq BIGINT",
		"parent_session_id UUID",
		"'offline_attempt'",
		"multiple active evidence rows exist for one activity revision",
		"learning_evidence_winning_attempt",
		"privacy_enforce_owner_write",
		"reject_learning_history_mutation",
		"ARRAY['privacy:device']",
		"sha256(convert_to(",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("000007 is missing %q", required)
		}
	}
	if strings.Contains(body, "digest(") {
		t.Fatal("offline sync migration must not require the optional pgcrypto extension")
	}
	if strings.Contains(body, "CREATE SEQUENCE") {
		t.Fatal("offline sync must reserve device sequences and use the learning event clock, not PostgreSQL sequences")
	}
}

func TestOfflineEvaluationAndPrivacyPurgeMigrationsUpgrade000007(t *testing.T) {
	pool := migrationPoolThrough(t, 7)
	ctx := context.Background()
	if err := Run(ctx, pool); err != nil {
		t.Fatalf("upgrade 000007 schema through 000009: %v", err)
	}
	if err := Check(ctx, pool); err != nil {
		t.Fatalf("check upgraded offline evaluation migration: %v", err)
	}
	for _, column := range []string{
		"frozen_request_hash", "model_artifact", "model_artifact_hash", "last_error_at",
		"result_assessment_id", "result_decision_id", "result_evidence_id",
	} {
		var exists bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS(
			  SELECT 1 FROM information_schema.columns
			  WHERE table_schema=current_schema() AND table_name='offline_evaluation_jobs' AND column_name=$1
			)`, column).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("000008 column %s was not created", column)
		}
	}
	var statusConstraint string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE connamespace=current_schema()::regnamespace
		  AND conname='offline_operation_status_combination'`).Scan(&statusConstraint); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusConstraint, "pending_retry") || !strings.Contains(statusConstraint, "failed") {
		t.Fatalf("000008 status constraint=%q", statusConstraint)
	}
	for _, table := range []string{"privacy_offline_device_children", "privacy_offline_device_child_revisions", "privacy_offline_device_child_heads"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass(current_schema()||'.'||$1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("offline privacy table %s is missing after upgrade", table)
		}
	}
	for _, column := range []string{"challenge_revision", "issued_at", "acknowledged_at"} {
		var exists bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS(
			  SELECT 1 FROM information_schema.columns
			  WHERE table_schema=current_schema() AND table_name='privacy_offline_device_child_revisions' AND column_name=$1
			)`, column).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("000009 column %s was not created", column)
		}
	}
	var acknowledgmentShape string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE connamespace=current_schema()::regnamespace
		  AND conname='privacy_offline_device_child_ack_shape'`).Scan(&acknowledgmentShape); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(acknowledgmentShape, "acknowledged_at IS NOT NULL") || !strings.Contains(acknowledgmentShape, "acknowledged_at IS NULL") {
		t.Fatalf("000009 acknowledgment shape=%q", acknowledgmentShape)
	}
	var offlineReceiptSlots int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM privacy_erasure_step_receipts WHERE store_kind='offline_device_cache'`).Scan(&offlineReceiptSlots); err != nil {
		t.Fatal(err)
	}
	if offlineReceiptSlots != 0 {
		t.Fatalf("fresh 000007 upgrade unexpectedly backfilled offline receipt slots=%d", offlineReceiptSlots)
	}
}

func TestOfflineSyncCoreMigrationUpgrades000006(t *testing.T) {
	pool := migrationPoolThrough(t, 6)
	ctx := context.Background()
	if err := Run(ctx, pool); err != nil {
		t.Fatalf("upgrade 000006 schema through 000007: %v", err)
	}
	if err := Check(ctx, pool); err != nil {
		t.Fatalf("check upgraded offline sync migration: %v", err)
	}
	for _, table := range []string{
		"offline_prepare_claims", "offline_activities", "offline_submission_authorizations",
		"offline_device_sequence_reservations", "offline_device_sequence_claims",
		"offline_attempt_heads", "learning_activity_evidence_claims",
		"offline_operation_status_revisions", "offline_evaluation_jobs",
		"offline_device_possessions", "privacy_offline_device_children",
	} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("000007 table %s was not created", table)
		}
	}
	var aggregateAllowed bool
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) LIKE '%offline_attempt%'
		FROM pg_constraint
		WHERE connamespace=current_schema()::regnamespace
		  AND conname='learning_events_aggregate_type_check'`).Scan(&aggregateAllowed); err != nil || !aggregateAllowed {
		t.Fatalf("offline aggregate constraint allowed=%v err=%v", aggregateAllowed, err)
	}
	var projectionVersion string
	var projectionIncomplete bool
	var projectionReasons []string
	if err := pool.QueryRow(ctx, `
		SELECT generation.projection_version,generation.incomplete,generation.reason_codes
		FROM learning_projection_head head
		JOIN learning_projection_generations generation ON generation.id=head.active_generation_id
		WHERE head.singleton_id=1`).Scan(&projectionVersion, &projectionIncomplete, &projectionReasons); err != nil {
		t.Fatal(err)
	}
	if projectionVersion != "learning-projection-v1" || !projectionIncomplete || !containsScope(projectionReasons, "projection_version_upgrade_required") {
		t.Fatalf("legacy projection upgrade state version=%q incomplete=%v reasons=%v", projectionVersion, projectionIncomplete, projectionReasons)
	}
	deviceID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'offline-migration',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	var credentialRows, sequenceRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM offline_device_credentials WHERE device_id=$1`, deviceID).Scan(&credentialRows); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM offline_device_sequence_heads WHERE device_id=$1`, deviceID).Scan(&sequenceRows); err != nil {
		t.Fatal(err)
	}
	if credentialRows != 1 || sequenceRows != 1 {
		t.Fatalf("new device offline state credential=%d sequence=%d", credentialRows, sequenceRows)
	}
}

func TestGoCLIM1MigrationDeclaresFreeQuestionVersionBoundaries(t *testing.T) {
	items, err := load()
	if err != nil {
		t.Fatal(err)
	}
	body := migrationBody(t, items, 6)
	for _, required := range []string{
		"ADD COLUMN session_aggregate_version BIGINT",
		"LOCK TABLE tutoring_free_questions IN ACCESS EXCLUSIVE MODE",
		"LOCK TABLE learning_events IN SHARE MODE",
		"LOCK TABLE learning_event_payloads IN SHARE MODE",
		"DROP TRIGGER tutoring_free_questions_immutable",
		"CREATE TRIGGER tutoring_free_questions_immutable",
		"event.event_type='FreeQuestionAsked'",
		"payload.payload->>'free_question_id'=question.id::text",
		"payload.redacted_at IS NOT NULL",
		"max(batch.aggregate_version)",
		"tutoring_focus_frame_session_owner_unique",
		"tutoring_free_question_frame_owner",
		"tutoring_free_question_session_frame_version_unique",
		"tutoring_free_question_current_lookup",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("000006 is missing %q", required)
		}
	}
	if strings.Contains(body, "row_number()") || strings.Contains(body, "ordinal=rows.ordinal") {
		t.Fatal("000006 must not guess Question/Event ownership by ordinal")
	}
}

func TestGoCLIM1MigrationUpgrades000005(t *testing.T) {
	pool := migrationPoolThrough(t, 5)
	ctx := context.Background()
	now := time.Now().UTC()
	deviceID := uuid.NewString()
	knowledgeRevisionID := uuid.NewString()
	sessionID := uuid.NewString()
	focusFrameID := uuid.NewString()
	questionID := uuid.NewString()
	payloadID := uuid.NewString()
	eventID := uuid.NewString()
	operationID := uuid.NewString()
	payload := fmt.Sprintf(`{"free_question_id":%q,"session_id":%q,"focus_frame_id":%q}`, questionID, sessionID, focusFrameID)
	payloadHash := sha256.Sum256([]byte(payload))
	batch := &pgx.Batch{}
	batch.Queue(`INSERT INTO devices(id,display_name,created_at) VALUES($1,'go-cli-migration',$2)`, deviceID, now)
	batch.Queue(`
		INSERT INTO knowledge_revisions(
			id,revision_no,manifest_hash,source,created_by_device_id,created_at,
			canonicalizer_version,parser_version,indexer_version,identity_policy_version)
		VALUES($1,1,decode(repeat('11',32),'hex'),'go-cli-migration',$2,$3,
		       'canonical-v1','parser-v1','indexer-v1','identity-v1')`, knowledgeRevisionID, deviceID, now)
	batch.Queue(`
		INSERT INTO tutoring_sessions(
			id,aggregate_version,state,knowledge_revision_id,started_at,updated_at)
		VALUES($1,3,'FreeQuestion',$2,$3,$3)`, sessionID, knowledgeRevisionID, now)
	batch.Queue(`
		INSERT INTO tutoring_focus_frames(
			id,session_id,saved_state,knowledge_revision_id,saved_aggregate_version,created_event_seq)
		VALUES($1,$2,'RouteActive',$3,1,1)`, focusFrameID, sessionID, knowledgeRevisionID)
	batch.Queue(`
		INSERT INTO tutoring_free_questions(
			id,session_id,focus_frame_id,question_text,knowledge_revision_id,references_snapshot,actor_device_id,received_at)
		VALUES($1,$2,$3,'Why?',$4,'[]',$5,$6)`, questionID, sessionID, focusFrameID, knowledgeRevisionID, deviceID, now)
	batch.Queue(`
		INSERT INTO learning_event_payloads(id,payload,payload_hash,created_at)
		VALUES($1,$2::jsonb,$3,$4)`, payloadID, payload, payloadHash[:], now)
	batch.Queue(`
		INSERT INTO learning_events(
			event_seq,id,event_type,event_schema_version,aggregate_type,aggregate_id,aggregate_version,
			device_id,operation_id,operation_ordinal,received_at,payload_id,payload_hash)
		VALUES(1,$1,'FreeQuestionAsked',1,'session',$2,3,$3,$4,0,$5,$6,$7)`,
		eventID, sessionID, deviceID, operationID, now, payloadID, payloadHash[:])
	results := pool.SendBatch(ctx, batch)
	if err := results.Close(); err != nil {
		t.Fatalf("seed 000005 free question: %v", err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatalf("upgrade 000005 schema through 000006: %v", err)
	}
	if err := Check(ctx, pool); err != nil {
		t.Fatalf("check upgraded migrations: %v", err)
	}
	var nullable string
	if err := pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='tutoring_free_questions' AND column_name='session_aggregate_version'`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable != "NO" {
		t.Fatalf("session_aggregate_version nullable=%s", nullable)
	}
	var storedVersion int64
	if err := pool.QueryRow(ctx, `SELECT session_aggregate_version FROM tutoring_free_questions WHERE id=$1`, questionID).Scan(&storedVersion); err != nil {
		t.Fatal(err)
	}
	if storedVersion != 3 {
		t.Fatalf("backfilled session aggregate version=%d want=3", storedVersion)
	}
	if _, err := pool.Exec(ctx, `UPDATE tutoring_free_questions SET session_aggregate_version=4 WHERE id=$1`, questionID); err == nil || !strings.Contains(err.Error(), "tutoring history is append-only") {
		t.Fatalf("restored immutable trigger error=%v", err)
	}
	for _, constraint := range []string{"tutoring_free_question_frame_owner", "tutoring_free_question_session_frame_version_unique"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE connamespace=current_schema()::regnamespace AND conname=$1)`, constraint).Scan(&exists); err != nil || !exists {
			t.Fatalf("constraint %s exists=%v err=%v", constraint, exists, err)
		}
	}
}

func TestGoCLIM1MigrationFailsClosedOnUnrecoverableQuestionEvents(t *testing.T) {
	for _, fixture := range []string{"missing", "redacted", "duplicate", "ownership"} {
		t.Run(fixture, func(t *testing.T) {
			pool := migrationPoolThrough(t, 5)
			seedGoCLIM1MigrationFailure(t, pool, fixture)
			err := Run(context.Background(), pool)
			if err == nil || !strings.Contains(err.Error(), "cannot recover") {
				t.Fatalf("fixture %s migration error=%v", fixture, err)
			}
		})
	}
}

func seedGoCLIM1MigrationFailure(t *testing.T, pool *pgxpool.Pool, fixture string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	deviceID := uuid.NewString()
	knowledgeRevisionID := uuid.NewString()
	sessionID := uuid.NewString()
	focusFrameID := uuid.NewString()
	questionID := uuid.NewString()
	batch := &pgx.Batch{}
	batch.Queue(`INSERT INTO devices(id,display_name,created_at) VALUES($1,'go-cli-migration-failure',$2)`, deviceID, now)
	batch.Queue(`
		INSERT INTO knowledge_revisions(
			id,revision_no,manifest_hash,source,created_by_device_id,created_at,
			canonicalizer_version,parser_version,indexer_version,identity_policy_version)
		VALUES($1,1,decode(repeat('11',32),'hex'),'go-cli-migration-failure',$2,$3,
		       'canonical-v1','parser-v1','indexer-v1','identity-v1')`, knowledgeRevisionID, deviceID, now)
	batch.Queue(`
		INSERT INTO tutoring_sessions(id,aggregate_version,state,knowledge_revision_id,started_at,updated_at)
		VALUES($1,4,'FreeQuestion',$2,$3,$3)`, sessionID, knowledgeRevisionID, now)
	batch.Queue(`
		INSERT INTO tutoring_focus_frames(id,session_id,saved_state,knowledge_revision_id,saved_aggregate_version,created_event_seq)
		VALUES($1,$2,'RouteActive',$3,1,1)`, focusFrameID, sessionID, knowledgeRevisionID)
	batch.Queue(`
		INSERT INTO tutoring_free_questions(id,session_id,focus_frame_id,question_text,knowledge_revision_id,references_snapshot,actor_device_id,received_at)
		VALUES($1,$2,$3,'Why?',$4,'[]',$5,$6)`, questionID, sessionID, focusFrameID, knowledgeRevisionID, deviceID, now)
	if err := pool.SendBatch(ctx, batch).Close(); err != nil {
		t.Fatal(err)
	}
	payloadFrameID := focusFrameID
	payload := fmt.Sprintf(`{"free_question_id":%q,"session_id":%q,"focus_frame_id":%q}`, questionID, sessionID, payloadFrameID)
	redacted := false
	switch fixture {
	case "missing":
		payload = `{}`
	case "redacted":
		payload = `{"redacted":true}`
		redacted = true
	case "ownership":
		payloadFrameID = uuid.NewString()
		payload = fmt.Sprintf(`{"free_question_id":%q,"session_id":%q,"focus_frame_id":%q}`, questionID, sessionID, payloadFrameID)
	case "duplicate":
	default:
		t.Fatalf("unknown migration failure fixture %q", fixture)
	}
	insertEvent := func(sequence, aggregateVersion int64, operationID string) {
		payloadID := uuid.NewString()
		eventID := uuid.NewString()
		hash := sha256.Sum256([]byte(payload))
		var redactedAt any
		if redacted {
			redactedAt = now
		}
		batch := &pgx.Batch{}
		batch.Queue(`
			INSERT INTO learning_event_payloads(id,payload,payload_hash,created_at,redacted_at)
			VALUES($1,$2::jsonb,$3,$4,$5)`, payloadID, payload, hash[:], now, redactedAt)
		batch.Queue(`
			INSERT INTO learning_events(
				event_seq,id,event_type,event_schema_version,aggregate_type,aggregate_id,aggregate_version,
				device_id,operation_id,operation_ordinal,received_at,payload_id,payload_hash)
			VALUES($1,$2,'FreeQuestionAsked',1,'session',$3,$4,$5,$6,0,$7,$8,$9)`, sequence, eventID, sessionID, aggregateVersion, deviceID, operationID, now, payloadID, hash[:])
		if err := pool.SendBatch(ctx, batch).Close(); err != nil {
			t.Fatal(err)
		}
	}
	insertEvent(1, 3, uuid.NewString())
	if fixture == "duplicate" {
		insertEvent(2, 4, uuid.NewString())
	}
}

func TestMemoryContractRepairMigrationUpgrades000004(t *testing.T) {
	pool := migrationPoolThrough(t, 4)
	ctx := context.Background()
	if err := Run(ctx, pool); err != nil {
		t.Fatalf("upgrade 000004 schema through 000005: %v", err)
	}
	if err := Check(ctx, pool); err != nil {
		t.Fatalf("check upgraded migrations: %v", err)
	}
	for _, table := range []string{
		"memory_record_external_refs",
		"memory_erasure_deliveries",
		"memory_erasure_delivery_sources",
		"memory_erasure_delivery_attempts",
		"memory_erasure_delivery_receipts",
		"privacy_migration_lease",
	} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("000005 table %s was not created", table)
		}
	}
	var redactionColumns int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='knowledge_revisions'
		  AND column_name IN ('redacted_at','redacted_by_erasure_id')`).Scan(&redactionColumns); err != nil {
		t.Fatal(err)
	}
	if redactionColumns != 2 {
		t.Fatalf("knowledge redaction columns=%d", redactionColumns)
	}
	var singletonRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM privacy_migration_lease WHERE singleton_id=1`).Scan(&singletonRows); err != nil || singletonRows != 1 {
		t.Fatalf("privacy migration singleton rows=%d err=%v", singletonRows, err)
	}
}

func TestMemoryContractRepairMigrationBackfillsCurrentRevisionExternalReference(t *testing.T) {
	pool := migrationPoolThrough(t, 4)
	ctx := context.Background()
	now := time.Now().UTC()
	candidateID := uuid.NewString()
	candidatePayloadID := uuid.NewString()
	logicalMemoryID := uuid.NewString()
	recordRevisionID := uuid.NewString()
	deliveryID := uuid.NewString()
	deliveryPayloadID := uuid.NewString()
	outboxID := uuid.NewString()
	attemptID := uuid.NewString()
	attemptToken := uuid.NewString()
	appliedReceiptID := uuid.NewString()
	externalNodeID := uuid.NewString()
	historicalCandidateID := uuid.NewString()
	historicalCandidatePayloadID := uuid.NewString()
	historicalRecordRevisionID := uuid.NewString()
	historicalDeliveryID := uuid.NewString()
	historicalDeliveryPayloadID := uuid.NewString()
	historicalOutboxID := uuid.NewString()
	historicalReceiptID := uuid.NewString()
	historicalAttemptID := uuid.NewString()
	historicalAttemptToken := uuid.NewString()
	historicalLeaseToken := uuid.NewString()
	erasureDeviceID := uuid.NewString()
	erasureID := uuid.NewString()
	reconciliationID := uuid.NewString()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	batch := &pgx.Batch{}
	batch.Queue(`
		INSERT INTO outbox_messages(
			id,business_type,aggregate_id,idempotency_key,revision,generation,payload,audit_metadata,
			status,available_at,attempts,max_attempts,created_at,updated_at)
		VALUES($1,'memory.delivery',$2,$3,1,1,'{}','{}','applied',$4,1,5,$4,$4)`,
		outboxID, logicalMemoryID, "memory.delivery:"+deliveryID, now)
	batch.Queue(`
		INSERT INTO memory_candidates(
			id,candidate_uri,logical_memory_id,payload_id,content_hash,source_kind,source_hashes,
			proposer_id,reason,category,sensitivity,stability,valid_until,admission_policy_version,created_at)
		VALUES($1::uuid,'candidate://' || $1::uuid::text,$2::uuid,$3,decode(repeat('ab',32),'hex'),
		       'user_statement','{}',$4,'migration fixture','interaction_preference',
		       'non_sensitive','stable',$5::timestamptz+interval '1 hour','memory-admission-v1',$5::timestamptz)`,
		candidateID, logicalMemoryID, candidatePayloadID, uuid.NewString(), now)
	batch.Queue(`
		INSERT INTO memory_logical_memories(id,created_from_candidate_id,created_at)
		VALUES($1,$2,$3)`, logicalMemoryID, candidateID, now)
	batch.Queue(`
		INSERT INTO memory_record_revisions(
			id,logical_memory_id,revision,record_generation,learner_generation,candidate_id,
			previous_revision_id,external_uri,external_uri_digest,content_hash,delivery_id,created_at)
		VALUES($1,$2::uuid,1,1,1,$3,NULL,'memory://' || $2::uuid::text,decode(repeat('cd',32),'hex'),
		       decode(repeat('ab',32),'hex'),$4,$5)`,
		recordRevisionID, logicalMemoryID, candidateID, deliveryID, now)
	batch.Queue(`
		INSERT INTO memory_deliveries(
			id,kind,logical_memory_id,record_revision_id,record_revision,learner_generation,record_generation,
			payload_id,payload_hash,external_uri,outbox_id,outbox_idempotency_key,valid_until,created_at)
		VALUES($1,'admit',$2::uuid,$3,1,1,1,$4,decode(repeat('ab',32),'hex'),
		       'memory://' || $2::uuid::text,$5,$6,$7::timestamptz+interval '1 hour',$7::timestamptz)`,
		deliveryID, logicalMemoryID, recordRevisionID, deliveryPayloadID,
		outboxID, "memory.delivery:"+deliveryID, now)
	batch.Queue(`
		INSERT INTO memory_delivery_receipts(
			id,delivery_id,version,status,reason,verification_method,created_at)
		VALUES($1,$2,1,'succeeded','applied','migration_fixture',$3)`,
		appliedReceiptID, deliveryID, now)
	batch.Queue(`
		INSERT INTO memory_delivery_attempts(id,delivery_id,attempt_token,created_at)
		VALUES($1,$2,$3,$4)`, attemptID, deliveryID, attemptToken, now)
	batch.Queue(`
		INSERT INTO memory_delivery_attempt_heads(
			attempt_id,delivery_id,state,boot_epoch,sent_at,updated_at)
		VALUES($1,$2,'confirmed','migration-backfill',$3,$3)`, attemptID, deliveryID, now)
	batch.Queue(`
		INSERT INTO memory_delivery_heads(
			delivery_id,logical_memory_id,status,public_status,attempt_state,current_attempt_id,
			attempt_count,current_receipt_id,current_receipt_version,updated_at)
		VALUES($1,$2,'applied','applied','confirmed',$3,1,$4,1,$5)`,
		deliveryID, logicalMemoryID, attemptID, appliedReceiptID, now)
	batch.Queue(`
		INSERT INTO memory_record_heads(
			logical_memory_id,current_record_revision_id,current_revision,record_generation,status,
			current_delivery_id,receipt_id,external_node_id,external_memory_id,applied_at,updated_at)
		VALUES($1,$2,1,1,'applied',$3,$4,$5,73,$6,$6)`,
		logicalMemoryID, recordRevisionID, deliveryID, appliedReceiptID, externalNodeID, now)
	batch.Queue(`
		INSERT INTO outbox_messages(
			id,business_type,aggregate_id,idempotency_key,revision,generation,payload,audit_metadata,
			status,available_at,attempts,max_attempts,created_at,updated_at)
		VALUES($1,'memory.delivery',$2,$3,2,1,'{}','{}','applied',$4,1,5,$4,$4)`,
		historicalOutboxID, logicalMemoryID, "memory.delivery:"+historicalDeliveryID, now)
	batch.Queue(`
		INSERT INTO memory_candidates(
			id,candidate_uri,logical_memory_id,payload_id,content_hash,source_kind,source_hashes,
			proposer_id,reason,category,sensitivity,stability,valid_until,admission_policy_version,created_at)
		VALUES($1::uuid,'candidate://' || $1::uuid::text,$2::uuid,$3,decode(repeat('ef',32),'hex'),
		       'user_statement','{}',$4,'historical identity unavailable','interaction_preference',
		       'non_sensitive','stable',$5::timestamptz+interval '1 hour','memory-admission-v1',$5::timestamptz)`,
		historicalCandidateID, logicalMemoryID, historicalCandidatePayloadID, uuid.NewString(), now)
	batch.Queue(`
		INSERT INTO memory_record_revisions(
			id,logical_memory_id,revision,record_generation,learner_generation,candidate_id,
			previous_revision_id,external_uri,external_uri_digest,content_hash,delivery_id,created_at)
		VALUES($1,$2::uuid,2,2,1,$3,$4,'memory://' || $2::uuid::text,decode(repeat('cd',32),'hex'),
		       decode(repeat('ef',32),'hex'),$5,$6)`,
		historicalRecordRevisionID, logicalMemoryID, historicalCandidateID, recordRevisionID, historicalDeliveryID, now)
	batch.Queue(`
		INSERT INTO memory_deliveries(
			id,kind,logical_memory_id,record_revision_id,record_revision,learner_generation,record_generation,
			payload_id,payload_hash,external_uri,outbox_id,outbox_idempotency_key,valid_until,created_at)
		VALUES($1,'correction',$2::uuid,$3,2,1,2,$4,decode(repeat('ef',32),'hex'),
		       'memory://' || $2::uuid::text,$5,$6,$7::timestamptz+interval '1 hour',$7::timestamptz)`,
		historicalDeliveryID, logicalMemoryID, historicalRecordRevisionID, historicalDeliveryPayloadID,
		historicalOutboxID, "memory.delivery:"+historicalDeliveryID, now)
	batch.Queue(`
		INSERT INTO memory_delivery_receipts(
			id,delivery_id,version,status,reason,verification_method,created_at)
		VALUES($1,$2,1,'pending','queued correction','migration_fixture',$3)`,
		historicalReceiptID, historicalDeliveryID, now)
	batch.Queue(`
		INSERT INTO memory_delivery_attempts(id,delivery_id,attempt_token,created_at)
		VALUES($1,$2,$3,$4)`, historicalAttemptID, historicalDeliveryID, historicalAttemptToken, now)
	batch.Queue(`
		INSERT INTO memory_delivery_attempt_heads(
			attempt_id,delivery_id,state,lease_token,lease_expires_at,boot_epoch,sent_at,updated_at)
		VALUES($1,$2,'sent',$3,$4::timestamptz+interval '10 minutes','migration-backfill',$4::timestamptz,$4::timestamptz)`,
		historicalAttemptID, historicalDeliveryID, historicalLeaseToken, now)
	batch.Queue(`
		INSERT INTO memory_delivery_heads(
			delivery_id,logical_memory_id,status,public_status,attempt_state,current_attempt_id,
			attempt_count,current_receipt_id,current_receipt_version,updated_at)
		VALUES($1,$2,'queued','queued','sent',$3,1,$4,1,$5)`,
		historicalDeliveryID, logicalMemoryID, historicalAttemptID, historicalReceiptID, now)
	batch.Queue(`
		UPDATE memory_record_heads
		SET current_record_revision_id=$2,current_revision=2,record_generation=2,status='queued',
		    current_delivery_id=$3,receipt_id=$4,updated_at=$5
		WHERE logical_memory_id=$1`, logicalMemoryID, historicalRecordRevisionID, historicalDeliveryID, historicalReceiptID, now)
	batch.Queue(`INSERT INTO devices(id,display_name,created_at) VALUES($1,'migration-erasure-device',$2)`, erasureDeviceID, now)
	batch.Queue(`
		INSERT INTO privacy_erasures(
			id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,
			target_learner_generation,managed_backup_scheduled_unrecoverable_after,managed_backup_verified_unrecoverable_at)
		VALUES($1,$2,$3,decode(repeat('aa',32),'hex'),'learner_request',$2,$4::timestamptz,2,$4::timestamptz+interval '1 day',$4::timestamptz)`,
		erasureID, erasureDeviceID, uuid.NewString(), now)
	batch.Queue(`
		INSERT INTO privacy_erasure_heads(erasure_id,status,summary_version,stable_reason,updated_at)
		VALUES($1,'partial',1,'migration active erasure',$2)`, erasureID, now)
	batch.Queue(`
		INSERT INTO memory_expiry_reconciliations(
			id,delivery_id,logical_memory_id,external_uri,content_hash,attempt_token,sent_boot_epoch,
			learner_generation,record_generation,status,created_at,updated_at)
		VALUES($1,$2,$3,'memory://' || $3::uuid::text,decode(repeat('ab',32),'hex'),$4,
		       'migration-backfill',1,1,'verified',$5,$5)`,
		reconciliationID, deliveryID, logicalMemoryID, attemptToken, now)
	batch.Queue(`
		UPDATE privacy_owner_generation_gates
		SET learner_generation=2,read_open=FALSE,write_open=FALSE,active_erasure_id=$1,updated_at=$2
		WHERE owner_kind='memory'`, erasureID, now)
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatalf("upgrade applied memory head through 000005: %v", err)
	}
	var storedDeliveryID, storedNodeID, storedAttemptID, storedReceiptID string
	var storedMemoryID int64
	if err := pool.QueryRow(ctx, `
		SELECT delivery_id,external_node_id,external_memory_id,delivery_attempt_id,delivery_receipt_id
		FROM memory_record_external_refs WHERE record_revision_id=$1`,
		recordRevisionID).Scan(&storedDeliveryID, &storedNodeID, &storedMemoryID, &storedAttemptID, &storedReceiptID); err != nil {
		t.Fatal(err)
	}
	if storedDeliveryID != deliveryID || storedNodeID != externalNodeID || storedMemoryID != 73 ||
		storedAttemptID != attemptID || storedReceiptID != appliedReceiptID {
		t.Fatalf("backfilled ref delivery=%s node=%s memory=%d attempt=%s receipt=%s",
			storedDeliveryID, storedNodeID, storedMemoryID, storedAttemptID, storedReceiptID)
	}
	var queuedRefCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_record_external_refs WHERE record_revision_id=$1`, historicalRecordRevisionID).
		Scan(&queuedRefCount); err != nil {
		t.Fatal(err)
	}
	if queuedRefCount != 0 {
		t.Fatalf("queued correction received a premature external ref count=%d", queuedRefCount)
	}
	var scopeCount, succeededScopes, attempts int
	if err := pool.QueryRow(ctx, `
		SELECT count(*),count(*) FILTER (WHERE s.status='succeeded'),COALESCE(sum(s.attempt_count),0)
		FROM memory_erasure_delivery_scopes s
		JOIN memory_erasure_deliveries d ON d.id=s.erasure_delivery_id
		WHERE d.erasure_id=$1`, erasureID).Scan(&scopeCount, &succeededScopes, &attempts); err != nil {
		t.Fatal(err)
	}
	if scopeCount != 2 || succeededScopes != 2 || attempts != 0 {
		t.Fatalf("migrated terminal scopes count=%d succeeded=%d attempts=%d", scopeCount, succeededScopes, attempts)
	}
}

func TestMemoryBridgeMigrationUpgradesLegacyOutboxRows(t *testing.T) {
	pool := legacyMigrationPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO outbox_messages(
			id,business_type,aggregate_id,idempotency_key,revision,generation,payload,audit_metadata,
			status,available_at,attempts,max_attempts,lease_expires_at,lease_token,created_at,updated_at)
		VALUES
		('41000000-0000-4000-8000-000000000001','legacy','pending','legacy-pending',1,1,'{}','{}','pending',now(),0,3,now()+interval '1 hour','41000000-0000-4000-8000-000000000101',now(),now()),
		('41000000-0000-4000-8000-000000000002','legacy','valid','legacy-valid',1,1,'{}','{}','processing',now(),1,3,now()+interval '1 hour','41000000-0000-4000-8000-000000000102',now(),now()),
		('41000000-0000-4000-8000-000000000003','legacy','missing','legacy-missing',1,1,'{}','{}','processing',now(),1,3,NULL,NULL,now(),now()),
		('41000000-0000-4000-8000-000000000004','legacy','expired','legacy-expired',1,1,'{}','{}','processing',now()+interval '1 hour',1,3,now()-interval '1 hour','41000000-0000-4000-8000-000000000104',now(),now()),
		('41000000-0000-4000-8000-000000000005','legacy','applied','legacy-applied',1,1,'{}','{}','applied',now(),1,3,now()+interval '1 hour','41000000-0000-4000-8000-000000000105',now(),now())`)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatalf("upgrade legacy schema through 000004: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT idempotency_key,status,lease_token IS NULL,lease_expires_at IS NULL FROM outbox_messages ORDER BY idempotency_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var key, status string
		var tokenNull, expiresNull bool
		if err := rows.Scan(&key, &status, &tokenNull, &expiresNull); err != nil {
			t.Fatal(err)
		}
		got[key] = fmt.Sprintf("%s/%t/%t", status, tokenNull, expiresNull)
	}
	want := map[string]string{
		"legacy-applied": "applied/true/true", "legacy-expired": "pending/true/true",
		"legacy-missing": "pending/true/true", "legacy-pending": "pending/true/true",
		"legacy-valid": "processing/false/false",
	}
	for key, expected := range want {
		if got[key] != expected {
			t.Fatalf("legacy row %s got %s want %s", key, got[key], expected)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_messages SET status='canceled',terminal_disposition=NULL WHERE idempotency_key='legacy-pending'`); err == nil {
		t.Fatal("canceled outbox row accepted a NULL terminal disposition")
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_messages SET status='canceled',terminal_disposition='deleted' WHERE idempotency_key='legacy-pending'`); err != nil {
		t.Fatalf("deleted cancellation disposition rejected: %v", err)
	}
}

func TestMemoryBridgeMigrationBackfillsOnlyActiveDeviceTokens(t *testing.T) {
	pool := legacyMigrationPool(t)
	ctx := context.Background()
	activeDevice := "42000000-0000-4000-8000-000000000001"
	revokedDevice := "42000000-0000-4000-8000-000000000002"
	if _, err := pool.Exec(ctx, `
		INSERT INTO devices(id,display_name,created_at,revoked_at) VALUES
		($1,'active',clock_timestamp(),NULL),
		($2,'revoked',clock_timestamp(),clock_timestamp())`, activeDevice, revokedDevice); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO device_tokens(id,device_id,token_hash,scopes,created_at,revoked_at) VALUES
		('42000000-0000-4000-8000-000000000011',$1,decode(repeat('11',32),'hex'),ARRAY['devices:read'],clock_timestamp(),NULL),
		('42000000-0000-4000-8000-000000000012',$1,decode(repeat('12',32),'hex'),ARRAY['devices:read'],clock_timestamp(),clock_timestamp()),
		('42000000-0000-4000-8000-000000000013',$2,decode(repeat('13',32),'hex'),ARRAY['devices:read'],clock_timestamp(),NULL)`, activeDevice, revokedDevice); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatalf("apply 000004 scope migration: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT id::text,scopes FROM device_tokens ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make(map[string][]string)
	for rows.Next() {
		var id string
		var scopes []string
		if err := rows.Scan(&id, &scopes); err != nil {
			t.Fatal(err)
		}
		got[id] = scopes
	}
	active := got["42000000-0000-4000-8000-000000000011"]
	for _, required := range []string{"devices:read", "memory:read", "memory:write", "privacy:read"} {
		if !containsScope(active, required) {
			t.Fatalf("active token missing %s: %v", required, active)
		}
	}
	if containsScope(active, "privacy:erase") {
		t.Fatalf("active token received privacy:erase: %v", active)
	}
	for _, id := range []string{"42000000-0000-4000-8000-000000000012", "42000000-0000-4000-8000-000000000013"} {
		if scopes := got[id]; len(scopes) != 1 || scopes[0] != "devices:read" {
			t.Fatalf("inactive token %s was changed: %v", id, scopes)
		}
	}
}

func containsScope(scopes []string, expected string) bool {
	for _, scope := range scopes {
		if scope == expected {
			return true
		}
	}
	return false
}

func legacyMigrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return migrationPoolThrough(t, 3)
}

func migrationPoolThrough(t *testing.T, maxVersion int64) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; legacy migration integration test not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("migration_upgrade_%d_%d", maxVersion, time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations(version BIGINT PRIMARY KEY,name TEXT NOT NULL,checksum TEXT NOT NULL,applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	items, err := load()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.version > maxVersion {
			break
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, string(item.body)); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version,name,checksum) VALUES($1,$2,$3)`, item.version, item.name, item.checksum)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

func migrationBody(t *testing.T, items []migration, version int64) string {
	t.Helper()
	for _, item := range items {
		if item.version == version {
			return string(item.body)
		}
	}
	t.Fatalf("migration %06d not found", version)
	return ""
}
