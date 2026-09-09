package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestExplicitSpaceRefusesOldServerBeforeBusinessRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/learning-spaces/capabilities" {
			calls++
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := NewClient(server.URL, "token", time.Second, nil).WithLearningSpace(DefaultLearningSpaceID)
	_, err := client.CurrentSession(t.Context())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "learning_spaces_unsupported" || calls != 0 {
		t.Fatalf("old server err=%v business calls=%d", err, calls)
	}
	if _, err = client.MutateLearningSpace(t.Context(), "", LearningSpaceCommand{}); !errors.As(err, &apiErr) || apiErr.Code != "learning_spaces_unsupported" || calls != 0 {
		t.Fatalf("old create err=%v calls=%d", err, calls)
	}
}
func TestLearningSpaceClientCopiesDoNotShareSelection(t *testing.T) {
	var scopes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/learning-spaces/capabilities" {
			_, _ = w.Write([]byte(`{"version":1,"default_space_id":"00000000-0000-4000-8000-000000000001","legacy_scope":"fixed_default","modules":{"knowledge":"default_only","learning":"default_only","tutoring":"default_only","memory":"default_only"}}`))
			return
		}
		scopes = append(scopes, r.Header.Get("X-Learning-Space-ID"))
		w.WriteHeader(501)
		_, _ = w.Write([]byte(`{"error":{"code":"learning_space_module_unavailable","message":"unavailable","request_id":"test"}}`))
	}))
	defer server.Close()
	base := NewClient(server.URL, "token", time.Second, nil)
	other := "10000000-0000-4000-8000-000000000001"
	one, two := base.WithLearningSpace(DefaultLearningSpaceID), base.WithLearningSpace(other)
	for _, client := range []*Client{one, two, one, base} {
		_, _ = client.CurrentSession(t.Context())
	}
	want := []string{DefaultLearningSpaceID, other, DefaultLearningSpaceID, ""}
	for i, s := range want {
		if len(scopes) != 4 || scopes[i] != s {
			t.Fatalf("scopes=%v", scopes)
		}
	}
}
