package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSourceSelectionHTTPRejectsNullAndUnknownFields(t *testing.T) {
	a := &API{}
	for _, selection := range []string{`null`, `{"mode":"none","items":[],"all":true}`, `{"mode":"none","items":[],"items":[]}`} {
		for _, turn := range []bool{false, true} {
			body := `{"save":false,"source_selection":` + selection + `}`
			if turn {
				body = `{"source_selection":` + selection + `}`
			}
			request := httptest.NewRequest("POST", "/", strings.NewReader(body))
			response := httptest.NewRecorder()
			if turn {
				a.tutorTurn(response, request)
			} else {
				a.mentorCreate(response, request)
			}
			if response.Code != 400 {
				t.Fatalf("accepted ambiguous selection: %s (%d)", selection, response.Code)
			}
		}
	}
	path := "/v1/learning/runs/00000000-0000-4000-8000-000000000001/citations/1"
	if !mentorPath(path) || collectionSelectionPath(path) {
		t.Fatal("citation route must use original run space, never a client collection header")
	}
}
