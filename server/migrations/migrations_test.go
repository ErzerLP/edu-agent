package migrations

import (
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learning"
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
	if latest.version != 3 || latest.name != "000003_learning_core.sql" || len(latest.checksum) != 64 {
		t.Fatalf("learning migration was not embedded with checksum: %+v", latest)
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
	body := string(items[len(items)-1].body)
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
	body := string(items[len(items)-1].body)
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
