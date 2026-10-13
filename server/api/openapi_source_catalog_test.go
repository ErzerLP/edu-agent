package api_test

import (
	"encoding/json"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"testing"
)

func TestSourceCatalogContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/knowledge/sources", "/v1/knowledge/sources/{revisionID}/documents/{documentRevisionID}"} {
		item := doc.Paths.Find(path)
		if item == nil || item.Get == nil {
			t.Fatalf("missing %s", path)
		}
		if item.Get.Security == nil || item.Get.Extensions["x-required-scope"] != "knowledge:read" {
			t.Fatal("missing read authorization", path)
		}
	}
	item := knowledge.SourceItem{CollectionID: uuid.NewString(), CollectionName: "集合", RevisionID: uuid.NewString(), DocumentID: uuid.NewString(), DocumentRevisionID: uuid.NewString(), Path: "a.md"}
	metadata := knowledge.SourceMetadata{Kind: "url", Title: "真实网页标题", Parser: "html-v1", Coverage: "partial_html_text_only", Fingerprint: "a", Status: "partial"}
	for name, value := range map[string]any{"KnowledgeSourceMetadata": metadata, "KnowledgeSourcePage": knowledge.SourcePage{Items: []knowledge.SourceItem{item}}, "KnowledgeSourceDetail": knowledge.SourceDetail{SourceItem: item, Fragments: []knowledge.SourceFragment{}}} {
		schema := doc.Components.Schemas[name]
		if schema == nil {
			t.Fatal("missing DTO", name)
		}
		raw, _ := json.Marshal(value)
		var decoded any
		_ = json.Unmarshal(raw, &decoded)
		if err := schema.Value.VisitJSON(decoded, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatal(name, err)
		}
	}
}
