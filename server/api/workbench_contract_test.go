package api_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/getkin/kin-openapi/openapi3"
)

func TestWorkbenchEntrySchemasMatchServerDTOs(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const id = "10000000-0000-4000-8000-000000000001"
	for _, tc := range []struct {
		schema string
		value  any
	}{
		{"LearningSpace", learningspace.Space{ID: learningspace.DefaultID, Name: "默认学习区", Status: "active", Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()}},
		{"LearningSpaceCapabilities", map[string]any{"version": 1, "default_space_id": learningspace.DefaultID, "legacy_scope": "fixed_default", "modules": map[string]string{"knowledge": "collections_v1", "learning": "goals_v1", "tutoring": "sessions_v1", "memory": "default_only"}}},
		{"TutoringSessionPage", learning.SessionPage{Items: []learning.SessionSummary{{SessionID: id, LearningSpaceID: learningspace.DefaultID, GoalID: id, GoalRevisionID: id, NodeRevisionID: id, RouteRevisionID: id, Name: "已有路线", GoalStatus: "active", State: "RouteActive", Position: "学习", LastEventSequence: 1, Resumable: true}}}},
	} {
		t.Run(tc.schema, func(t *testing.T) {
			raw, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			var value any
			if err = json.Unmarshal(raw, &value); err != nil {
				t.Fatal(err)
			}
			if err = doc.Components.Schemas[tc.schema].Value.VisitJSON(value); err != nil {
				t.Fatalf("真实 DTO 不符合公开合同：%v", err)
			}
		})
	}
}
