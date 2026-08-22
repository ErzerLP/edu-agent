package migrations

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
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
	if latest.version != 4 || latest.name != "000004_memory_bridge.sql" || len(latest.checksum) != 64 {
		t.Fatalf("memory migration was not embedded with checksum: %+v", latest)
	}
}

func TestLearningCoreMigrationSeedsCurrentEmptyProjectionFingerprint(t *testing.T) {
	items, err := load()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := learning.ProjectionFingerprint(learning.EmptyProjection("migration-generation"))
	if err != nil {
		t.Fatal(err)
	}
	body := migrationBody(t, items, 3)
	seed := "decode('" + fingerprint + "', 'hex')"
	if count := strings.Count(body, seed); count != 2 {
		t.Fatalf("000003 must seed generation and checkpoint with empty projection fingerprint %s; occurrences=%d", fingerprint, count)
	}
	if strings.Contains(body, "decode(repeat('00', 32), 'hex')") {
		t.Fatal("000003 uses the obsolete all-zero projection fingerprint sentinel")
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
	schema := fmt.Sprintf("migration_upgrade_%d", time.Now().UnixNano())
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
		if item.version > 3 {
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
