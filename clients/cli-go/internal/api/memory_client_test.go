package api

import (
	"errors"
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

func TestMemoryClientDecidesPendingCandidate(t *testing.T) {
	const candidateID = "40000000-0000-4000-8000-000000000001"
	var decisionBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/memory/candidates/"+candidateID+"/decisions" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		data, _ := io.ReadAll(r.Body)
		decisionBody = string(data)
		admittedCandidate := strings.Replace(validMemoryCandidate, `"status":"pending_review","revision":1`, `"status":"admitted","revision":2`, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidate":{"candidate":`+admittedCandidate+`,"content_status":"unavailable"},"replayed":false}`)
	}))
	defer server.Close()

	client := NewClient(server.URL, "device-token", time.Second, nil)
	result, err := client.DecideMemoryCandidate(t.Context(), candidateID, MemoryCandidateDecisionRequest{
		OperationID: "60000000-0000-4000-8000-000000000002", PayloadSchemaVersion: 1,
		ExpectedRevision: 1, Decision: "admit", Reason: "user_confirmed_preference_save",
	})
	if err != nil || result.Candidate == nil || result.Candidate.Candidate.Status != "admitted" ||
		!strings.Contains(decisionBody, `"expected_revision":1`) || !strings.Contains(decisionBody, `"decision":"admit"`) {
		t.Fatalf("result=%+v body=%s err=%v", result, decisionBody, err)
	}
}

func TestMemoryClientExportsLiveMemory(t *testing.T) {
	const exportPage = `{"items":[{"record":{"logical_memory_id":"40000000-0000-4000-8000-000000000001","record_revision_id":"50000000-0000-4000-8000-000000000001","revision":1,"record_generation":1,"learner_generation":1,"candidate_id":"60000000-0000-4000-8000-000000000001","external_uri":"nocturne://core/edu-agent/40000000-0000-4000-8000-000000000001","external_uri_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","external_node_id":"70000000-0000-4000-8000-000000000001","external_memory_id":42,"content_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","status":"applied","delivery_id":"80000000-0000-4000-8000-000000000001","receipt_id":"90000000-0000-4000-8000-000000000001","created_at":"2026-08-29T00:00:00Z"},"delivery_status":"applied","receipt":{"receipt_id":"90000000-0000-4000-8000-000000000001","delivery_id":"80000000-0000-4000-8000-000000000001","version":1,"status":"succeeded","reason":"hash_verified","verification_method":"remote_readback","created_at":"2026-08-29T00:00:00Z"},"content_status":"available","content":"回答时先给结论"}],"next_cursor":"next-page","read_generation":{"learner_generation":1,"memory_generation":1},"degraded":false,"reason_codes":[]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/memory/export" || r.URL.Query().Get("cursor") != "current" || r.URL.Query().Get("limit") != "20" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, exportPage)
	}))
	defer server.Close()

	client := NewClient(server.URL, "device-token", time.Second, nil)
	page, err := client.ExportMemory(t.Context(), "current", 20)
	if err != nil || len(page.Items) != 1 || page.Items[0].Content != "回答时先给结论" || page.NextCursor != "next-page" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func TestMemoryClientRejectsInvalidExportPageRequestBeforeNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()

	client := NewClient(server.URL, "device-token", time.Second, nil)
	_, err := client.ExportMemory(t.Context(), "", 201)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Category != "invalid_memory_export_request" || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestMemoryClientRejectsAvailableExportWithoutContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"record":{"logical_memory_id":"40000000-0000-4000-8000-000000000001","record_revision_id":"50000000-0000-4000-8000-000000000001","revision":1,"record_generation":1,"learner_generation":1,"candidate_id":"60000000-0000-4000-8000-000000000001","external_uri":"nocturne://core/edu-agent/40000000-0000-4000-8000-000000000001","external_uri_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","content_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","status":"applied","delivery_id":"80000000-0000-4000-8000-000000000001","receipt_id":"90000000-0000-4000-8000-000000000001","created_at":"2026-08-29T00:00:00Z"},"delivery_status":"applied","receipt":{"receipt_id":"90000000-0000-4000-8000-000000000001","delivery_id":"80000000-0000-4000-8000-000000000001","version":1,"status":"succeeded","reason":"hash_verified","verification_method":"remote_readback","created_at":"2026-08-29T00:00:00Z"},"content_status":"available"}],"read_generation":{"learner_generation":1,"memory_generation":1},"degraded":false,"reason_codes":[]}`)
	}))
	defer server.Close()

	client := NewClient(server.URL, "device-token", time.Second, nil)
	_, err := client.ExportMemory(t.Context(), "", 20)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Category != "invalid_success_response" {
		t.Fatalf("err=%v", err)
	}
}
