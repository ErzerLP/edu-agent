package mentorrun

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacepg "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
)

// Explicit opt-in only. Credentials come from the caller's environment and are
// never printed; the temporary settings directory is removed by testing.
func TestPostgreSQLStudyLiveResponsesSmoke(t *testing.T) {
	if os.Getenv("C2_LIVE_SMOKE") != "1" {
		t.Skip("explicit live model opt-in required")
	}
	endpoint, key := os.Getenv("EDU_ACC_MODEL_BASE_URL"), os.Getenv("EDU_ACC_MODEL_API_KEY")
	if endpoint == "" || key == "" {
		t.Fatal("acceptance model environment missing")
	}
	f := fixture(t, func(http.ResponseWriter, *http.Request, int) { t.Error("live smoke used fixture model") })
	limits := settings.DefaultLimits()
	limits.OutputTokens = 8192

	limits.IdleTimeoutSeconds = 180
	configuration, err := settings.Open(settings.Options{Path: filepath.Join(t.TempDir(), "private", "settings.json"), Teaching: settings.Connection{Enabled: true, Provider: "openai_compatible", Endpoint: endpoint, Model: "deepseek-v4-flash", AuthMode: "bearer", APIStyle: "responses"}, TeachingKey: key, Limits: &limits})
	if err != nil {
		t.Fatal("live settings initialization failed")
	}
	useTeaching := true
	if _, err = configuration.Update(settings.Update{ExpectedRevision: configuration.View().Revision, Target: settings.Mentor, MentorUsesTeaching: &useTeaching}); err != nil {
		t.Fatal("mentor teaching reuse failed")
	}
	f.settings = configuration
	f.service, err = New(f.pool, configuration, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 210*time.Second)
	defer cancel()
	direction := "理解线性无关和基的直观含义"
	_, err = spacepg.New(f.pool, f.service.directions).Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "线性代数", Direction: &direction, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	source := selectedDocument(t, f, "向量组线性无关，指只有各系数都为零时，它们的线性组合才为零向量。一组基既线性无关，又能张成整个向量空间。")
	id, err := f.service.NewConversation(ctx, f.actor, learningspace.DefaultID, NewConversation{ID: uuid.NewString(), Mode: "study"})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: 1, DirectionVersion: 1, Prompt: "请用两句话解释线性无关，并引用所选资料。", SourceSelection: &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{source}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal("live worker failed")
	}
	snapshot := f.snapshot(t, receipt.RunID)
	if snapshot.Status != "succeeded" || len(snapshot.Citations) == 0 || snapshot.Output == "" {
		t.Fatalf("live smoke did not produce verified answer: status=%s reason=%s requests=%d", snapshot.Status, snapshot.Reason, snapshot.RequestsUsed)
	}
	for _, citation := range snapshot.Citations {
		if err = f.service.ReadCitation(ctx, f.actor, learningspace.DefaultID, receipt.RunID, citation.ID, func(SourceCitation) error { return nil }); err != nil {
			t.Fatal("live citation could not be resolved")
		}
	}
	t.Logf("responses deepseek-v4-flash output_budget=8192 requests=%d verified_citations=%d saved_turns=%d", snapshot.RequestsUsed, len(snapshot.Citations), len(historyPage(t, f, id, 0, 10).Items))
}
