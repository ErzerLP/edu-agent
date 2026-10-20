package settings

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
)

func TestDefaultAPIStylePreservesMentorFingerprint(t *testing.T) {
	s, _ := testService(t, "http://localhost:11434/v1")
	saveModel(t, s, Mentor, "http://localhost:11434/v1", "mentor-test-key")
	_, _, fingerprint, err := s.MentorClient()
	if err != nil {
		t.Fatal(err)
	}
	// Pre-api_style connection identity: unchanged settings must not invalidate ongoing runs on upgrade.
	legacy := `{"Connection":{"enabled":true,"provider":"openai_compatible","endpoint":"http://localhost:11434/v1","model":"test-model","auth_mode":"bearer"},"Key":"mentor-test-key","Limits":{"output_tokens":2048,"context_tokens":32768,"idle_timeout_seconds":60,"concurrency":10,"storage_mib":512}}`
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(legacy)))
	if fingerprint != want {
		t.Fatal("default API style changed existing mentor identity")
	}
}

func TestOldSettingsDefaultToChatCompletions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	const old = `{"version":1,"revision":2,"teaching":{"enabled":true,"provider":"openai_compatible","endpoint":"http://localhost:11434/v1","model":"old-model","auth_mode":"bearer","key":"old-test-key","source":"server","probe":null},"mentor":{"provider":"openai_compatible","auth_mode":"bearer","source":"server"},"search":{"provider":"brave","auth_mode":"bearer","source":"server"},"mentor_uses_teaching":false,"limits":{"output_tokens":2048,"context_tokens":32768,"idle_timeout_seconds":60,"concurrency":2,"storage_mib":512},"policy":{"private_queries":false}}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(Options{Path: path, ModelEndpoints: []string{"http://localhost:11434/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	v := s.View()
	if v.Teaching.APIStyle != "chat_completions" || v.Mentor.APIStyle != "chat_completions" || !v.Teaching.HasKey || v.TeachingRestartRequired || v.Revision != 2 {
		t.Fatalf("legacy settings changed: %+v", v)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != old {
		t.Fatal("opening legacy settings rewrote the file")
	}
}

func TestAPIStyleBindsCredentialsProbeAndStartup(t *testing.T) {
	var calls int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer responses-test-key" {
			t.Error("API style or credential crossed interfaces")
		}
		var body struct {
			MaxOutputTokens int `json:"max_output_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.MaxOutputTokens != 4096 {
			t.Error("Responses probe/generation did not use configured output budget")
		}
		fmt.Fprint(w, `{"status":"completed","output":[{"content":[{"type":"output_text","text":"{\"capability_probe\":true}"}]}]}`)
	}))
	defer provider.Close()
	s, o := testService(t, provider.URL)
	v := saveModel(t, s, Teaching, provider.URL, "chat-test-key")
	yes := true
	v, err := s.Update(Update{ExpectedRevision: v.Revision, MentorUsesTeaching: &yes})
	if err != nil {
		t.Fatal(err)
	}
	s, err = Open(o)
	if err != nil {
		t.Fatal(err)
	}
	s.state.Teaching.Probe = &Probe{Status: "ready"}
	s.state.Mentor.Probe = &Probe{Status: "ready"}
	c := v.Teaching.Connection
	c.APIStyle = "responses"
	v, err = s.Update(Update{ExpectedRevision: v.Revision, Target: Teaching, Connection: &c})
	if err != nil {
		t.Fatal(err)
	}
	if v.Teaching.HasKey || v.Teaching.Probe != nil || v.Mentor.Probe != nil || !v.TeachingRestartRequired || calls != 0 {
		t.Fatal("API switch retained key/probes or made an implicit request")
	}
	limits := v.Limits
	limits.OutputTokens = 4096
	v, err = s.Update(Update{ExpectedRevision: v.Revision, Target: Teaching, NewKey: key("responses-test-key"), Limits: &limits})
	if err != nil {
		t.Fatal(err)
	}
	v, err = s.Probe(t.Context(), Teaching, v.Revision, true)
	if err != nil || v.Teaching.Status != "ready" || !v.Teaching.Probe.NativeSchema {
		t.Fatalf("Responses probe failed: %v %+v", err, v.Teaching)
	}
	reopened, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	client, err := reopened.TeachingClient()
	if err != nil || client == nil {
		t.Fatalf("no teaching client: %v", err)
	}
	if _, err := client.Chat(t.Context(), llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "Return JSON."}}}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || reopened.View().TeachingRestartRequired {
		t.Fatal("startup did not consume the saved API style")
	}
	c.APIStyle = "unknown"
	if _, err := reopened.Update(Update{ExpectedRevision: v.Revision, Target: Teaching, Connection: &c}); !errors.Is(err, ErrInvalid) {
		t.Fatal("accepted unknown API style")
	}
}
