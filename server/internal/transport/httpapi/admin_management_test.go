package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminManagementResourceReads(t *testing.T) {
	handler, _, _, notesyncService := newAdminResourceTestAPI(t, "")
	session := loginAdmin(t, handler)

	memoryResponse := httptest.NewRecorder()
	handler.ServeHTTP(memoryResponse, authenticatedAdminRequest(http.MethodGet, "/admin/api/memory", "", session))
	if memoryResponse.Code != http.StatusOK || !strings.Contains(memoryResponse.Body.String(), "prefers focused sessions") {
		t.Fatalf("memory status/body = %d/%s", memoryResponse.Code, memoryResponse.Body.String())
	}

	knowledgeResponse := httptest.NewRecorder()
	handler.ServeHTTP(knowledgeResponse, authenticatedAdminRequest(http.MethodGet, "/admin/api/knowledge", "", session))
	if knowledgeResponse.Code != http.StatusOK || !strings.Contains(knowledgeResponse.Body.String(), "Learning map") {
		t.Fatalf("knowledge status/body = %d/%s", knowledgeResponse.Code, knowledgeResponse.Body.String())
	}

	notesyncResponse := httptest.NewRecorder()
	handler.ServeHTTP(notesyncResponse, authenticatedAdminRequest(http.MethodGet, "/admin/api/notesync", "", session))
	if notesyncResponse.Code != http.StatusOK || strings.Contains(notesyncResponse.Body.String(), strings.Repeat("n", 32)) {
		t.Fatalf("NoteSync status/body = %d/%s", notesyncResponse.Code, notesyncResponse.Body.String())
	}
	if !strings.Contains(notesyncResponse.Body.String(), `"api_key_configured":true`) {
		t.Fatalf("NoteSync response omitted redacted credential state: %s", notesyncResponse.Body.String())
	}

	previewResponse := httptest.NewRecorder()
	handler.ServeHTTP(previewResponse, authenticatedAdminRequest(http.MethodPost, "/admin/api/notesync/preview", `{"path":"learning.md","page":1,"page_size":20}`, session))
	if previewResponse.Code != http.StatusOK || notesyncService.previewCmd.Path != "learning.md" {
		t.Fatalf("preview status/command/body = %d/%+v/%s", previewResponse.Code, notesyncService.previewCmd, previewResponse.Body.String())
	}
}
