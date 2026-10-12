package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

func studioCandidate(kind, coverage string) string {
	claim := `{"text":"偶数可被 2 整除。","citations":[{"source":1,"fragment":1}]}`
	if kind == "guide" {
		return `{"title":"偶数学习指南","coverage":"` + coverage + `","key_points":[` + claim + `],"terms":[` + claim + `],"learning_order":[` + claim + `],"misconceptions":[` + claim + `]}`
	}
	return `{"title":"偶数简报","coverage":"` + coverage + `","overview":[` + claim + `],"differences":[` + claim + `],"gaps":[` + claim + `]}`
}
func studioFixture(t *testing.T, handler func(http.ResponseWriter, *http.Request, int)) (*runtimeFixture, StudioGenerate) {
	t.Helper()
	f := fixture(t, handler)
	selected := selectedDocument(t, f, "SELECTED_EVEN_SOURCE")
	_ = selectedDocument(t, f, "UNSELECTED_SECRET_SOURCE")
	content, err := learningcontent.New(f.pool, nil, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	content.ConfigureStudioSources(knowledgepg.New(f.pool))
	f.service.ConfigureStudio(content)
	return f, StudioGenerate{OperationID: uuid.NewString(), StudioTarget: learningcontent.StudioTarget{Generation: 1, Kind: "guide"}, SourceSelection: knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{selected}}, RequestBudget: 1, TokenBudget: 50000}
}
func TestPostgreSQLStudioGenerationPublishesAndRecovers(t *testing.T) {
	kind := "guide"
	f, request := studioFixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte("UNSELECTED_SECRET_SOURCE")) || !bytes.Contains(raw, []byte("SELECTED_EVEN_SOURCE")) || bytes.Contains(raw, []byte("真实绑定目标")) {
			t.Error("source selection leaked or omitted")
		}
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		if payload["response_format"].(map[string]any)["type"] != "json_object" {
			t.Error("chat required unsupported native schema")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": studioCandidate(kind, "complete")}}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 40, "total_tokens": 140}})
	})
	ctx := context.Background()
	for _, value := range []string{"guide", "brief"} {
		kind = value
		request.Kind = kind
		request.OperationID = uuid.NewString()
		receipt, err := f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, request)
		if err != nil {
			t.Fatal(err)
		}
		f.work(t)
		snapshot := f.snapshot(t, receipt.RunID)
		if snapshot.Status != "succeeded" || snapshot.Studio == nil || snapshot.Studio.Result == nil {
			t.Fatalf("not published: %+v", snapshot)
		}
		result := snapshot.Studio.Result
		artifact, err := f.service.studio.StudioGet(ctx, f.actor, result.ArtifactID, result.Version)
		if err != nil || artifact.Kind != kind || len(artifact.Body.Citations) != 1 {
			t.Fatalf("artifact unavailable: %+v %v", artifact, err)
		}
		if strings.Contains(artifact.Body.Title, "UNSELECTED") {
			t.Fatal("source leak")
		}
		again, err := f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, request)
		if err != nil || again.RunID != receipt.RunID {
			t.Fatal("operation not idempotent", err)
		}
	}
}

func TestStudioGenerateRequiresSourcesAndExplicitVersions(t *testing.T) {
	id := uuid.NewString()
	c := StudioGenerate{StudioTarget: learningcontent.StudioTarget{Kind: "brief", Generation: 1}, OperationID: uuid.NewString(), SourceSelection: knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{{CollectionID: id, RevisionID: id, DocumentID: id, DocumentRevisionID: id}}}, RequestBudget: 1, TokenBudget: 8192}
	if c.Validate() != nil {
		t.Fatal("goal-less generation rejected")
	}
	for _, mutate := range []func(*StudioGenerate){
		func(c *StudioGenerate) {
			c.SourceSelection = knowledge.SourceSelection{Mode: "none", Items: []knowledge.SourceIdentity{}}
		},
		func(c *StudioGenerate) { c.GoalID = &id },
		func(c *StudioGenerate) { c.ArtifactID = &id },
		func(c *StudioGenerate) { c.Kind = "quiz" },
		func(c *StudioGenerate) { c.Generation = 0 },
	} {
		bad := c
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("invalid generation admitted")
		}
	}
	c.GoalID = &id
	c.GoalVersion = 3
	c.ArtifactID = &id
	c.ExpectedVersion = 2
	if c.Validate() != nil {
		t.Fatal("explicit target and version rejected")
	}
}
