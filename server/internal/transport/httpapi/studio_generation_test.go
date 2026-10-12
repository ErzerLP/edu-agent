package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

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
	for _, path := range []string{"/v1/learning/studio/generations", "/v1/learning/studio/artifacts/00000000-0000-4000-8000-000000000001/sources/1"} {
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
