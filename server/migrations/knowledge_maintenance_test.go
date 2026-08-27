package migrations

import (
	"strings"
	"testing"
)

func TestKnowledgeMaintenanceMigrationDeclaresAtomicAndPrivacyBoundaries(t *testing.T) {
	items, err := load()
	if err != nil {
		t.Fatal(err)
	}
	body := migrationBody(t, items, 11)
	for _, required := range []string{
		"CREATE TABLE knowledge_maintenance_proposals",
		"CREATE TABLE knowledge_maintenance_decisions",
		"CREATE TABLE knowledge_maintenance_operations",
		"CREATE TABLE knowledge_revision_origins",
		"CREATE TABLE learning_evidence_carryover_proposals",
		"CREATE TABLE learning_evidence_carryover_candidates",
		"CREATE TABLE learning_evidence_carryover_decisions",
		"CREATE TABLE learning_evidence_carryover_operations",
		"CREATE TABLE learning_evidence_carryover_links",
		"CREATE TABLE learning_projection_carryovers",
		"'open','applied','rejected','stale','redacted'",
		"'open','approved','rejected','stale','redacted'",
		"planned_revision_id UUID NOT NULL",
		"prepared_commit JSONB NOT NULL",
		"evidence_generation BIGINT NOT NULL",
		"knowledge_maintenance_proposal_status_shape",
		"protect_knowledge_maintenance_proposal",
		"knowledge maintenance proposal terminal state is immutable",
		"knowledge maintenance proposal basis is immutable",
		"protect_learning_evidence_carryover_proposal",
		"learning evidence carryover proposal terminal state is immutable",
		"learning evidence carryover proposal basis is immutable",
		"reject_knowledge_maintenance_history_mutation",
		"knowledge maintenance history is append-only",
		"reject_learning_history_mutation",
		"privacy_enforce_owner_write",
		"'knowledge_maintenance_proposals'",
		"'knowledge_maintenance_decisions'",
		"'knowledge_maintenance_operations'",
		"'knowledge_revision_origins'",
		"'learning_evidence_carryover_proposals'",
		"'learning_evidence_carryover_links'",
		"'evidence_carryover'",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("000011 is missing %q", required)
		}
	}
	if strings.Contains(body, "UPDATE learning_evidence SET") || strings.Contains(body, "INSERT INTO learning_evidence(") {
		t.Fatal("000011 mutates accepted learning evidence authority")
	}
}
