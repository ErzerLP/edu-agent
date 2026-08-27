package app

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/notesync"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepostgres "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
)

func TestNotesyncCompositionBuildsPublicationPathOnlyWhenEnabled(t *testing.T) {
	disabledStores := newApplicationStores(nil)
	disabled, err := composeNotesync(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if disabledStores.notesyncPublication || disabled.client != nil || disabled.remote != nil || disabled.review != nil || disabled.probe != nil || len(disabled.workers) != 0 {
		t.Fatal("disabled NoteSync was partially composed")
	}

	baseURL, err := url.Parse("https://notes.example.test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Notesync: config.NotesyncConfig{
		Enabled: true, BaseURL: baseURL, APIToken: "0123456789abcdef0123456789abcdef",
		Vault: "Knowledge", PathPrefix: "edu-agent", HTTPTimeout: time.Second,
		WorkerInterval: 3 * time.Second, WorkerBatch: 20, ScanPageSize: 7, ScanMaxPages: 2,
	}}
	enabledStores := newApplicationStores(nil, knowledgepostgres.WithNotesyncPublication(knowledgepostgres.NotesyncPublicationConfig{
		Vault: cfg.Notesync.Vault, PathPrefix: cfg.Notesync.PathPrefix,
	}))
	canonicalizer := knowledge.NewCanonicalizer()
	knowledgeService, err := knowledge.NewService(enabledStores.knowledge, canonicalizer, knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := composeNotesync(cfg, notesyncDependencies{
		publicationStore: enabledStores.knowledge, reviewStore: enabledStores.knowledge,
		outboxStore: enabledStores.outbox, importer: knowledgeService, canonicalizer: canonicalizer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !enabledStores.notesyncPublication || enabled.client == nil || enabled.remote == nil || enabled.review == nil || enabled.probe == nil ||
		len(enabled.workers) != 1 || enabled.workers[0].name != "notesync_outbox" || enabled.workers[0].runOnce == nil ||
		enabled.workers[0].interval != cfg.Notesync.WorkerInterval || enabled.workers[0].batch != cfg.Notesync.WorkerBatch ||
		enabled.lease < time.Minute || enabled.lease < 12*cfg.Notesync.HTTPTimeout {
		t.Fatal("enabled NoteSync publication path was not fully composed")
	}
	if _, err := enabled.review.Preview(context.Background(), notesync.PreviewCommand{Page: 3, PageSize: 7}); notesync.ReviewErrorCode(err) != notesync.CodeReviewInvalidRequest {
		t.Fatalf("composition accepted page beyond configured scan maximum: %v", err)
	}
	if _, err := enabled.review.Preview(context.Background(), notesync.PreviewCommand{Page: 1, PageSize: 8}); notesync.ReviewErrorCode(err) != notesync.CodeReviewInvalidRequest {
		t.Fatalf("composition accepted page size beyond configured scan maximum: %v", err)
	}
}

func TestNotesyncCompositionRetriesBootstrapWithoutBlockingCore(t *testing.T) {
	bootstrapCalls := 0
	workerCalls := 0
	runOnce := runAfterNotesyncBootstrap(
		func(context.Context) (int, error) {
			bootstrapCalls++
			if bootstrapCalls == 1 {
				return 0, errors.New("privacy gate closed")
			}
			return 2, nil
		},
		func(context.Context) (int, error) {
			workerCalls++
			return 3, nil
		},
	)
	if count, err := runOnce(context.Background()); err == nil || count != 0 || workerCalls != 0 {
		t.Fatalf("failed bootstrap count=%d err=%v worker_calls=%d", count, err, workerCalls)
	}
	if count, err := runOnce(context.Background()); err != nil || count != 5 || bootstrapCalls != 2 || workerCalls != 1 {
		t.Fatalf("successful bootstrap count=%d err=%v bootstrap_calls=%d worker_calls=%d", count, err, bootstrapCalls, workerCalls)
	}
	if count, err := runOnce(context.Background()); err != nil || count != 3 || bootstrapCalls != 2 || workerCalls != 2 {
		t.Fatalf("post-bootstrap count=%d err=%v bootstrap_calls=%d worker_calls=%d", count, err, bootstrapCalls, workerCalls)
	}
}
