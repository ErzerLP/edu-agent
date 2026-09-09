package command

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func TestSpaceSelectionIsPerAppAndFlagsAreTemporary(t *testing.T) {
	other := "10000000-0000-4000-8000-000000000002"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/learning-spaces/capabilities" {
			_ = json.NewEncoder(w).Encode(api.LearningSpaceCapabilities{Version: 1, DefaultSpaceID: api.DefaultLearningSpaceID, LegacyScope: "fixed_default", Modules: map[string]string{"knowledge": "default_only", "learning": "default_only", "tutoring": "default_only", "memory": "default_only"}})
			return
		}
		if r.URL.Path == "/v1/learning-spaces" {
			items := []api.LearningSpace{}
			if r.URL.Query().Get("search") == "Go" {
				items = append(items, api.LearningSpace{ID: other, Name: "Go", Status: "active", Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()})
			}
			_ = json.NewEncoder(w).Encode(api.LearningSpacePage{Items: items})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/learning-spaces/") {
			_ = json.NewEncoder(w).Encode(api.LearningSpace{ID: strings.TrimPrefix(r.URL.Path, "/v1/learning-spaces/"), Name: "name", Description: "description", Status: "active", Version: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()})
			return
		}
		w.WriteHeader(501)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "learning_space_module_unavailable", "message": "unavailable", "request_id": "test"}})
	}))
	defer server.Close()
	cfg, creds := pairedStores(server.URL, "token")
	one, _, errOut := newTestApp(cfg, creds, &fakeTerminal{})
	two, _, _ := newTestApp(cfg, creds, &fakeTerminal{})
	if exit := one.Run(t.Context(), []string{"space", "select", "--id", other}); exit != ExitOK {
		t.Fatalf("exit=%d %s", exit, errOut)
	}
	if one.learningSpace != other || two.learningSpace != "" || cfg.saveCalls != 0 {
		t.Fatal("selection leaked to another instance or config")
	}
	if exit := one.Run(t.Context(), []string{"--space", api.DefaultLearningSpaceID, "space", "show", "--id", api.DefaultLearningSpaceID}); exit != ExitOK || one.learningSpace != other {
		t.Fatal("temporary override changed selection")
	}
	if exit := one.Run(t.Context(), []string{"offline", "status"}); exit != ExitUnavailable {
		t.Fatalf("nondefault offline exit=%d", exit)
	}
	if exit := one.Run(t.Context(), []string{"--space=bad", "space", "list"}); exit != ExitInput || one.learningSpace != other {
		t.Fatal("malformed scope changed selection")
	}
	if exit := one.Run(t.Context(), []string{"--space", api.DefaultLearningSpaceID, "space", "list", "--space"}); exit != ExitInput || one.learningSpace != other {
		t.Fatal("incomplete duplicate scope changed selection")
	}
	term := &fakeTerminal{lines: []string{"/Go", "1", "s"}}
	three, out, browseErr := newTestApp(cfg, creds, term)
	three.InputIsTTY = func() bool { return true }
	three.OutputIsTTY = func() bool { return true }
	if exit := three.Run(t.Context(), []string{"space", "browse"}); exit != ExitOK || three.learningSpace != other || !strings.Contains(out.String(), "No learning spaces match") || !strings.Contains(out.String(), "version: 1") {
		t.Fatalf("browse exit=%d out=%s err=%s", exit, out, browseErr)
	}
	if three.dashboardSnapshot().LearningSpaceName != "name" {
		t.Fatal("selected space name is missing from dashboard")
	}
}
