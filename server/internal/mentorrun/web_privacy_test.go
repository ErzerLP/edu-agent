package mentorrun

import (
	"bytes"
	"context"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/research"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestWebPrivacyBarrierClearsSnapshotsAndRejectsLateCommit(t *testing.T) {
	f, h := newWebHost(t)
	ctx := context.Background()
	resource, err := h.registerWebResource(ctx, "https://example.org/private-query", "独立网络快照", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	source := research.Source{ID: resource.ID, RevisionID: uuid.NewString(), Locator: resource.URL, FinalURL: resource.URL, Title: resource.Title, Status: "parsed", Kind: "text/plain", Coverage: "complete_text", Text: "待清除的正文", StorageAllowed: true, FetchedAt: &now, Fragments: []research.Fragment{{ID: uuid.NewString(), Start: 0, End: 18, Text: "待清除的正文"}}}
	document, err := h.storeWebDocument(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	if err = f.pool.QueryRow(ctx, `SELECT ciphertext FROM learning_web_document_payloads WHERE id=$1`, document.ID).Scan(&ciphertext); err != nil || bytes.Contains(ciphertext, []byte(source.Text)) {
		t.Fatalf("snapshot not encrypted: %v", err)
	}
	tutoring := tutoringdb.New(f.pool)
	store := privacydb.New(f.pool, privacydb.WithReadPermits(privacy.NewReadPermitManager()), privacydb.WithLocalOwner(identitydb.New(f.pool)), privacydb.WithLocalOwner(knowledgedb.New(f.pool)), privacydb.WithLocalOwner(learningdb.New(f.pool, tutoring)), privacydb.WithLocalOwner(tutoring), privacydb.WithLocalOwner(memorydb.New(f.pool)), privacydb.WithLocalOwner(outboxdb.New(f.pool)))
	grants, err := privacy.NewErasureGrantService(privacydb.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := grants.Issue(ctx, f.actor.Device.ID, "网络快照清除验收")
	if err != nil {
		t.Fatal(err)
	}
	barrier, err := store.CommitBarrierAuthorized(ctx, privacy.ErasureRequest{DeviceID: f.actor.Device.ID, OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}, privacy.NewErasureGrantAuthorization(f.actor.Device.ID, grant.Token))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.ReadWebDocument(ctx, f.actor, learningspace.DefaultID, document.ID, "", func(research.WebDocument) error { t.Error("erased snapshot emitted"); return nil }); err == nil {
		t.Fatal("old generation still readable")
	}
	source.RevisionID = uuid.NewString()
	if _, err = h.storeWebDocument(ctx, source); err == nil {
		t.Fatal("late response resurrected content")
	}
	if _, err = store.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_web_resources)+(SELECT count(*) FROM learning_web_documents)+(SELECT count(*) FROM learning_web_document_payloads)+(SELECT count(*) FROM learning_web_operations)`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("web copies remain after scrub: %d %v", count, err)
	}
}
