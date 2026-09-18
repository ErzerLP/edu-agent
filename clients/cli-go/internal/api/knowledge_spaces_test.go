package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFrozenKnowledgeScopeSurvivesStrictClientDecoder(t *testing.T) {
	const id = "10000000-0000-4000-8000-000000000001"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request KnowledgeRetrievalRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.ScopeSnapshotID != id || request.KnowledgeRevisionID != "" {
			t.Errorf("冻结范围请求丢失: %+v %v", request, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scope_snapshot_id":"` + id + `","knowledge_revision_id":"` + id + `","retriever_version":"retriever-v1","selector_version":"selector-v1","query_context_schema_version":"query-context-v1","summary_snapshot":[],"document_shortlist":[],"trace":[],"hits":[],"degraded":false,"truncated":false}`))
	}))
	defer server.Close()
	result, err := NewClient(server.URL, "token", time.Second, nil).RetrieveKnowledge(t.Context(), KnowledgeRetrievalRequest{Query: "channel", ScopeSnapshotID: id})
	if err != nil || result.ScopeSnapshotID != id {
		t.Fatalf("客户端拒绝合法范围响应: %+v %v", result, err)
	}
}

func TestPDFSourceExtensionKeepsCanonicalTextFallback(t *testing.T) {
	var doc DocumentRevision
	if err := decodeStrict([]byte(`{"document_revision_id":"10000000-0000-4000-8000-000000000001","document_id":"10000000-0000-4000-8000-000000000002","root_node_id":"10000000-0000-4000-8000-000000000003","canonical_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","semantic_hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","pdf":{"kind":"uploaded_pdf","report":{"parser":"future-parser","pages":[{"number":2,"text":"旧版本第二页原文"}]}},"nodes":[]}`), &doc); err != nil {
		t.Fatal("PDF 元数据破坏兼容文本读取", err)
	}
	if !json.Valid(doc.PDF) {
		t.Fatal("来源元数据未保留")
	}
}
