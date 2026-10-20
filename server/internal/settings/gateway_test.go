package settings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
)

func TestGatewayUsesCurrentExplicitTargetAndOpaqueRevisions(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		if p["model"] != "mentor-model" || r.Header.Get("Authorization") != "Bearer mentor-secret" {
			t.Error("selected target was replaced")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer provider.Close()
	s, _ := testService(t, provider.URL)
	v := saveModel(t, s, Teaching, provider.URL, "teaching-secret")
	c := v.Teaching.Connection
	c.Model = "mentor-model"
	yes := true
	_, err := s.Update(Update{ExpectedRevision: v.Revision, Target: Mentor, Connection: &c, NewKey: key("mentor-secret"), MentorUsesTeaching: &yes})
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.ModelConnection(Mentor)
	if err != nil {
		t.Fatal(err)
	}
	client, err := f.Client(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Gateway(t.Context(), llm.GatewayRequest{Messages: []llm.GatewayMessage{{Role: "user", Content: "hi"}}}, nil); err != nil {
		t.Fatal(err)
	}
	before := f.Projection()
	raw, _ := json.Marshal(before)
	if strings.Contains(string(raw), provider.URL) || strings.Contains(string(raw), "secret") {
		t.Fatal("secret in projection")
	}
	v = s.View()
	_, err = s.Update(Update{ExpectedRevision: v.Revision, Target: Mentor, NewKey: key("rotated-secret")})
	if err != nil {
		t.Fatal(err)
	}
	f, _ = s.ModelConnection(Mentor)
	after := f.Projection()
	if before.ConfigurationRevision == after.ConfigurationRevision || before.DestinationRevision != after.DestinationRevision {
		t.Fatal("key rotation changed destination or retained configuration")
	}
	if _, err = s.ModelConnection(Search); err == nil {
		t.Fatal("search accepted")
	}
	if len(s.GatewayModels()) != 2 {
		t.Fatal("enabled candidates missing")
	}
}
