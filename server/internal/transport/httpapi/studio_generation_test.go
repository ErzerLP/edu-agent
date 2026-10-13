package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learning"
)

func TestStudioDirectionHTTPRejectsNullAndMixedMode(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000001"
	base := map[string]any{"operation_id": id, "privacy_generation": 1, "kind": "guide", "goal_id": nil, "goal_version": 0, "artifact_id": nil, "expected_version": 0, "source_selection": map[string]any{"mode": "items", "items": []any{map[string]string{"collection_id": id, "revision_id": id, "document_id": id, "document_revision_id": id}}}, "request_budget": 1, "token_budget": 50000}
	for _, change := range []map[string]any{{"mode": nil}, {"mode": ""}, {"direction_version": nil}, {"direction_version": 0}, {"kind": "quiz", "mode": "direction", "direction_version": nil}, {"kind": "quiz", "mode": "direction", "direction_version": 1, "goal_id": id, "goal_version": 1}} {
		input := map[string]any{}
		for k, v := range base {
			input[k] = v
		}
		for k, v := range change {
			input[k] = v
		}
		raw, _ := json.Marshal(input)
		w := httptest.NewRecorder()
		(&API{}).studioGenerate(w, httptest.NewRequest("POST", "/", strings.NewReader(string(raw))))
		if w.Code != 400 {
			t.Fatalf("ambiguous mode admitted: %v status=%d", change, w.Code)
		}
	}
}

func TestStudioGenerationHTTPClosedInputAndRoutes(t *testing.T) {
	a := &API{}
	for _, body := range []string{`{}`, `{"source_selection":null}`, `{"kind":"guide","kind":"brief"}`, `{"studio":{}}`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		w := httptest.NewRecorder()
		a.studioGenerate(w, r)
		if w.Code != 400 {
			t.Fatalf("ambiguous Studio request accepted: %d", w.Code)
		}
	}
	for _, path := range []string{"/v1/learning/studio/exercises/00000000-0000-4000-8000-000000000001", "/v1/learning/studio/exercises/00000000-0000-4000-8000-000000000001/help", "/v1/learning/studio/exercises/00000000-0000-4000-8000-000000000001/assessment/resume", "/v1/learning/studio/generations", "/v1/learning/studio/artifacts/00000000-0000-4000-8000-000000000001/sources/1", "/v1/learning/studio/artifacts/00000000-0000-4000-8000-000000000001/quiz", "/v1/learning/studio/artifacts/00000000-0000-4000-8000-000000000001/quiz/start", "/v1/learning/studio/quiz-operations/00000000-0000-4000-8000-000000000001"} {
		if !studioPath(path) || collectionSelectionPath(path) {
			t.Fatal("Studio must use the explicit learning space, never a client collection")
		}
	}
	// The internal task type cannot be smuggled through an old mentor endpoint.
	w := httptest.NewRecorder()
	a.mentorCreate(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"save":true,"studio":{}}`)))
	if w.Code != 400 {
		t.Fatal("legacy mentor accepted internal Studio task")
	}
}

func TestStudioQuizClosedStartRequest(t *testing.T) {
	a := &API{maxLearningRequestBody: 64 << 10}
	for _, body := range []string{`{}`, `{"operation_id":"00000000-0000-4000-8000-000000000001","version":1,"privacy_generation":1,"goal_version":1,"question":0}`, `{"operation_id":"00000000-0000-4000-8000-000000000001","version":1,"privacy_generation":1,"goal_version":1,"question":1,"question":2}`, `{"operation_id":"00000000-0000-4000-8000-000000000001","version":1,"privacy_generation":1,"goal_version":1,"question":1,"goal_id":"00000000-0000-4000-8000-000000000001"}`} {
		w := httptest.NewRecorder()
		a.studioQuizStart(w, httptest.NewRequest("POST", "/", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("ambiguous start accepted: %d", w.Code)
		}
	}
}

func TestStudioQuizPreservesLearningErrorClassification(t *testing.T) {
	a := &API{}
	for _, code := range []string{learning.CodeVersionConflict, learning.CodeInvalidTransition} {
		w := httptest.NewRecorder()
		a.studioQuizError(w, httptest.NewRequest("POST", "/", nil), &learning.Error{Code: code})
		if w.Code != 409 {
			t.Fatalf("formal conflict became an unknown result: %d", w.Code)
		}
	}
}
