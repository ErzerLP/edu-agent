package api_test

import (
	"encoding/json"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestStudioContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/learning/studio/artifacts", "/v1/learning/studio/artifacts/{artifactID}", "/v1/learning/studio/artifacts/{artifactID}/revisions", "/v1/learning/studio/artifacts/{artifactID}/import-candidate", "/v1/learning/studio/operations/{operationID}"} {
		if doc.Paths.Find(path) == nil {
			t.Fatal("missing Studio path", path)
		}
	}
	r := learningcontent.StudioRevision{ArtifactID: uuid.NewString(), SpaceID: uuid.NewString(), Kind: "note", Version: 1, Generation: 1, ActorDeviceID: uuid.NewString(), CreatedAt: time.Now(), Body: learningcontent.StudioBody{Schema: 1, Title: "标题", Markdown: "正文", Citations: []learningcontent.StudioCitation{}}}
	raw, _ := json.Marshal(r)
	var value any
	_ = json.Unmarshal(raw, &value)
	if err = doc.Components.Schemas["StudioRevision"].Value.VisitJSON(value, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatal(err)
	}
}
