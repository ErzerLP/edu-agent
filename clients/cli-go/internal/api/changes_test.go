package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLearningChangesNegotiationAndIsolation(t *testing.T) {
	goal := "10000000-0000-4000-8000-000000000001"
	change := "20000000-0000-4000-8000-000000000001"
	wrong := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Learning-Change-Version") != "1" || r.Header.Get("X-Learning-Space-ID") != DefaultLearningSpaceID {
			t.Error("新协议没有显式协商和学习区")
		}
		v := LearningChange{ID: change, SpaceID: DefaultLearningSpaceID, GoalID: goal, Revision: 1, Status: "queued_for_boundary", Base: json.RawMessage(`{}`), Candidate: json.RawMessage(`{}`), Diff: json.RawMessage(`{}`)}
		if wrong {
			v.GoalID = change
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(struct {
			Items []LearningChange `json:"items"`
		}{[]LearningChange{v}})
	}))
	defer server.Close()
	c := NewClient(server.URL, "test", time.Second, nil)
	items, err := c.LearningChanges(context.Background(), goal, "", 0)
	if err != nil || len(items) != 1 || items[0].Status != "queued_for_boundary" {
		t.Fatal(items, err)
	}
	wrong = true
	if _, err = c.LearningChanges(context.Background(), goal, "", 0); err == nil {
		t.Fatal("返回其他目标的变更未拒绝")
	}
}
