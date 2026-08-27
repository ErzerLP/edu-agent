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
		"'open','applied','rejected','stale','redacted'",
		"planned_revision_id UUID NOT NULL",
		"prepared_commit JSONB NOT NULL",
		"evidence_generation BIGINT NOT NULL",
		"knowledge_maintenance_proposal_status_shape",
		"protect_knowledge_maintenance_proposal",
		"knowledge maintenance proposal terminal state is immutable",
		"knowledge maintenance proposal basis is immutable",
		"reject_knowledge_maintenance_history_mutation",
		"knowledge maintenance history is append-only",
		"privacy_enforce_owner_write",
		"'knowledge_maintenance_proposals'",
		"'knowledge_maintenance_decisions'",
		"'knowledge_maintenance_operations'",
		"'knowledge_revision_origins'",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("000011 is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"INSERT INTO learning_",
		"UPDATE learning_",
		"DELETE FROM learning_",
		"CREATE TABLE learning_",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("000011 crosses the learning write boundary with %q", forbidden)
		}
	}
}
