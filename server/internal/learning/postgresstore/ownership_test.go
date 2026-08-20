package postgresstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learning"
)

func TestProductionSourceDoesNotOwnTutoringOrReadKnowledgeTables(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"tutoring_sessions", "tutoring_focus_frames", "tutoring_free_questions", "tutoring_free_answers",
		"knowledge_node_revisions", "knowledge_snapshot_documents",
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		content, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range forbidden {
			if strings.Contains(string(content), table) {
				t.Errorf("production file %s references owner table %s", entry.Name(), table)
			}
		}
	}
}

func TestPayloadWithAuthorityPreservesReplayShapeAndImmutableOwner(t *testing.T) {
	route := learning.RouteRevision{
		ID: "route-revision", RouteID: "route", GoalRevisionID: "goal", KnowledgeRevisionID: "knowledge",
		Steps: []learning.RouteStep{{ID: "step", NodeID: "node", NodeRevisionID: "node-revision"}},
	}
	owner := learning.KnowledgeOwner{KnowledgeRevisionID: "knowledge", NodeID: "node", NodeRevisionID: "node-revision", DocumentRevisionID: "document-revision"}
	payload, err := payloadWithAuthority(learning.EventRouteRevisionCreated, mustMarshal(t, route), learning.AuthorityProvenance{RouteSteps: map[string]learning.KnowledgeOwner{"step": owner}})
	if err != nil {
		t.Fatal(err)
	}
	var replayed learning.RouteRevision
	if err := json.Unmarshal(payload, &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.ID != route.ID || replayed.Steps[0].NodeRevisionID != route.Steps[0].NodeRevisionID {
		t.Fatalf("route replay shape changed: %+v", replayed)
	}
	var envelope struct {
		Authority map[string]learning.KnowledgeOwner `json:"_authority"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Authority["step"] != owner {
		t.Fatalf("event authority=%+v want=%+v", envelope.Authority["step"], owner)
	}
}

func mustMarshal(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
