package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const validMemoryCandidate = `{"candidate_id":"40000000-0000-4000-8000-000000000001","candidate_uri":"urn:edu-agent:memory-candidate:40000000-0000-4000-8000-000000000001","payload_id":"50000000-0000-4000-8000-000000000001","content_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source_kind":"device","source_reference":{},"proposer_id":"10000000-0000-4000-8000-000000000001","reason":"explicit preference","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable","valid_until":"2030-01-01T00:00:00Z","admission_policy_version":"memory-admission-v1","status":"pending_review","revision":1,"created_at":"2026-08-29T00:00:00Z"}`

func TestMemoryClientListsAndCreatesCandidates(t *testing.T) {
	var createBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/memory/candidates":
			if r.URL.Query().Get("limit") != "20" {
				t.Fatalf("query=%s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"items":[{"candidate":`+validMemoryCandidate+`,"content_status":"available","proposed_content":"请使用简洁的中文回答","read_generation":{"learner_generation":1,"memory_generation":1}}],"read_generation":{"learner_generation":1,"memory_generation":1}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/memory/candidates":
			data, _ := io.ReadAll(r.Body)
			createBody = string(data)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"candidate":{"candidate":`+validMemoryCandidate+`,"content_status":"available","proposed_content":"请使用简洁的中文回答"},"replayed":false}`)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, "device-token", time.Second, nil)
	page, err := client.MemoryCandidates(t.Context(), "", 20)
	if err != nil || len(page.Items) != 1 || page.Items[0].ProposedContent == "" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	result, err := client.CreateMemoryCandidate(t.Context(), MemoryCandidateRequest{
		OperationID: "60000000-0000-4000-8000-000000000001", PayloadSchemaVersion: 1,
		Content: "请使用简洁的中文回答", Reason: "用户明确要求", Category: "interaction_preference",
		Sensitivity: "non_sensitive", Stability: "stable", ValidUntil: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil || result.Candidate == nil || !strings.Contains(createBody, `"payload_schema_version":1`) {
		t.Fatalf("result=%+v body=%s err=%v", result, createBody, err)
	}
}
