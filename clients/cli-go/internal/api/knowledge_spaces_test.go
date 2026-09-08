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
