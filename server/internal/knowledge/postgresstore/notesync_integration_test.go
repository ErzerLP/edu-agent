package postgresstore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	notesyncintegration "github.com/edu-agent/edu-agent/server/internal/integrations/notesync"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxpostgresstore "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgreSQLKnowledgeNotesyncAcceptRemoteRepublishesWithoutLoop(t *testing.T) {
	pool := notesyncKnowledgePool(t)
	exerciseNotesyncAcceptRemoteRepublish(t, pool, &notesyncBridgeRemote{vault: "Knowledge"}, "Knowledge", "edu-agent", "reviewed/topic.md")
}

func TestPostgreSQLKnowledgeNotesyncRealUpstreamAcceptRemoteRepublishesWithoutLoop(t *testing.T) {
	baseURLValue := os.Getenv("NOTESYNC_REAL_BASE_URL")
	apiToken := os.Getenv("NOTESYNC_REAL_API_TOKEN")
	vault := os.Getenv("NOTESYNC_REAL_VAULT")
	if baseURLValue == "" || apiToken == "" || vault == "" {
		t.Skip("real NoteSync candidate environment is not configured")
	}
	baseURL, err := url.Parse(baseURLValue)
	if err != nil {
		t.Fatal(err)
	}
	client, err := notesyncintegration.New(notesyncintegration.Options{
		BaseURL: baseURL, APIToken: apiToken, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := notesyncKnowledgePool(t)
	canonicalPath := fmt.Sprintf("reviewed-real/topic-%d.md", time.Now().UTC().UnixNano())
	exerciseNotesyncAcceptRemoteRepublish(t, pool, client, vault, "edu-agent", canonicalPath)
}

type notesyncBridgeRemoteClient interface {
	Probe(context.Context, string) notesyncintegration.Capability
	GetNote(context.Context, string, string) (notesyncintegration.Note, error)
	ListNotes(context.Context, string, int, int) (notesyncintegration.NotePage, error)
	CreateOrUpdateNote(context.Context, notesyncintegration.NoteWrite) (notesyncintegration.Note, error)
}

func exerciseNotesyncAcceptRemoteRepublish(
	t *testing.T,
	pool *pgxpool.Pool,
	remote notesyncBridgeRemoteClient,
	vault string,
	pathPrefix string,
	canonicalPath string,
) {
	t.Helper()
	ctx := context.Background()
	store := postgresstore.New(pool, postgresstore.WithNotesyncPublication(postgresstore.NotesyncPublicationConfig{
		Vault: vault, PathPrefix: pathPrefix,
	}))
	service, err := knowledge.NewService(store, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "37000000-0000-4000-8000-000000000001", ExpectedParentProvided: true,
		Source: "notesync-reviewed-import-first", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{
			Path:     canonicalPath,
			Markdown: "# Topic\nshared lesson body uses enough stable words for identity continuity alpha state\n",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstExport, err := service.Export(ctx, first.Revision.ID)
	if err != nil || len(firstExport.Documents) != 1 {
		t.Fatalf("first export=%+v err=%v", firstExport, err)
	}
	remotePath, err := notesyncintegration.ManagedPath(pathPrefix, canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	seedTime := time.Now().UTC().Add(-2 * time.Minute).UnixMilli()
	seeded, err := remote.CreateOrUpdateNote(ctx, notesyncintegration.NoteWrite{
		Vault: vault, Path: remotePath, Content: firstExport.Documents[0].Markdown,
		Ctime: seedTime, Mtime: seedTime, CreateOnly: true,
	})
	if err != nil {
		t.Fatalf("seed real publication target: %v", err)
	}
	consumer, err := notesyncintegration.NewConsumer(notesyncintegration.ConsumerOptions{
		Store: store, Remote: remote, Vault: vault, PathPrefix: pathPrefix,
		RetryBackoff: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	messageStore := outboxpostgresstore.New(pool)
	firstMessage := claimNotesyncPublication(t, messageStore, time.Now().UTC().Add(time.Minute))
	decision, err := consumer.CanApply(ctx, firstMessage)
	if err != nil || !decision.Apply {
		t.Fatalf("initial publication decision=%+v err=%v", decision, err)
	}
	if err := consumer.Apply(ctx, firstMessage); !errors.Is(err, outbox.ErrConsumerFinalized) {
		t.Fatalf("initial publication apply err=%v", err)
	}

	remoteMarkdown := strings.Replace(firstExport.Documents[0].Markdown, "alpha state", "beta state", 1)
	drifted, err := remote.CreateOrUpdateNote(ctx, notesyncintegration.NoteWrite{
		Vault: vault, Path: remotePath, Content: remoteMarkdown,
		Ctime: seeded.Ctime, Mtime: time.Now().UTC().Add(-time.Minute).UnixMilli(), CreateOnly: false,
	})
	if err != nil {
		t.Fatalf("write remote drift: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	reviews, err := notesyncintegration.NewReviewService(notesyncintegration.ReviewServiceOptions{
		Store: store, Remote: remote, Importer: service, Canonicalizer: knowledge.NewCanonicalizer(),
		Vault: vault, PathPrefix: pathPrefix, ScanPageSize: 25, ScanMaxPages: 20,
		NewUUID: func() string { return "37000000-0000-4000-8000-000000000002" },
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := reviews.Preview(ctx, notesyncintegration.PreviewCommand{Path: remotePath})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Items) != 1 || preview.Items[0].Category != notesyncintegration.PreviewCategoryRemoteChanged ||
		preview.Items[0].ReviewID == "" {
		t.Fatalf("remote drift preview=%+v", preview)
	}
	item := preview.Items[0]
	if item.Remote.SHA256 == "" || item.Remote.RemoteVersion != drifted.Version {
		t.Fatalf("remote drift snapshot=%+v note=%+v", item.Remote, drifted)
	}
	resolved, err := reviews.Resolve(ctx, notesyncintegration.ResolutionCommand{
		ReviewID: item.ReviewID, BasisHash: item.BasisHash,
		OperationID: "37000000-0000-4000-8000-000000000003",
		DeviceID:    integrationActorID,
		Kind:        notesyncintegration.ResolutionAcceptRemote,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.KnowledgeRevisionID == first.Revision.ID || resolved.DocumentRevisionID == "" {
		t.Fatalf("accept_remote result=%+v", resolved)
	}
	secondMessage := claimNotesyncPublication(t, messageStore, time.Now().UTC().Add(2*time.Minute))
	secondIntent, err := notesyncintegration.DecodePublicationIntent(secondMessage)
	if err != nil {
		t.Fatal(err)
	}
	if secondIntent.PublicationReason != notesyncintegration.PublicationReasonReviewImport ||
		secondIntent.ReviewID != item.ReviewID || secondIntent.KnowledgeRevisionID != resolved.KnowledgeRevisionID ||
		secondMessage.IdempotencyKey != notesyncintegration.ReviewPublicationIdempotencyKey(item.ReviewID, secondIntent.DocumentRevisionID, secondMessage.Generation) {
		t.Fatalf("reviewed import intent=%+v message=%+v", secondIntent, secondMessage)
	}
	decision, err = consumer.CanApply(ctx, secondMessage)
	if err != nil || !decision.Apply {
		t.Fatalf("reviewed import decision=%+v err=%v", decision, err)
	}
	if err := consumer.Apply(ctx, secondMessage); !errors.Is(err, outbox.ErrConsumerFinalized) {
		t.Fatalf("reviewed import apply err=%v", err)
	}
	published, err := remote.GetNote(ctx, vault, remotePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(published.Content, "beta state") ||
		!strings.Contains(published.Content, "edu-agent-source-revision-id: "+resolved.KnowledgeRevisionID) ||
		strings.Contains(published.Content, "edu-agent-source-revision-id: "+first.Revision.ID) {
		t.Fatalf("reviewed import remote content=%q", published.Content)
	}
	var publishedRevision int64
	var openReviews int
	if err := pool.QueryRow(ctx, `
		SELECT published_revision_no,
		       (SELECT count(*) FROM knowledge_notesync_reviews WHERE status='open')
		FROM knowledge_notesync_publications WHERE document_id=$1`, secondIntent.DocumentID).Scan(&publishedRevision, &openReviews); err != nil {
		t.Fatal(err)
	}
	if publishedRevision != secondMessage.Revision || openReviews != 0 {
		t.Fatalf("reviewed import final state published=%d want=%d open_reviews=%d", publishedRevision, secondMessage.Revision, openReviews)
	}
}

type notesyncBridgeRemote struct {
	vault  string
	note   notesyncintegration.Note
	writes int
}

func (r *notesyncBridgeRemote) Probe(context.Context, string) notesyncintegration.Capability {
	return notesyncintegration.Capability{Compatible: true, Version: notesyncintegration.SupportedVersion, Vault: r.vault}
}

func (r *notesyncBridgeRemote) GetNote(context.Context, string, string) (notesyncintegration.Note, error) {
	return r.note, nil
}

func (r *notesyncBridgeRemote) ListNotes(context.Context, string, int, int) (notesyncintegration.NotePage, error) {
	return notesyncintegration.NotePage{}, errors.New("unexpected list notes")
}

func (r *notesyncBridgeRemote) CreateOrUpdateNote(_ context.Context, write notesyncintegration.NoteWrite) (notesyncintegration.Note, error) {
	r.writes++
	if r.note.Path == "" {
		r.note.Vault = write.Vault
		r.note.Path = write.Path
		r.note.Ctime = write.Ctime
	}
	r.note.Content = write.Content
	r.note.Version++
	r.note.Mtime = write.Mtime
	r.note.LastTime++
	return r.note, nil
}

func TestPostgreSQLKnowledgeNotesyncCanonicalABACommitsDistinctOutboxIntent(t *testing.T) {
	pool := notesyncKnowledgePool(t)
	ctx := context.Background()
	store := postgresstore.New(pool, postgresstore.WithNotesyncPublication())
	service, err := knowledge.NewService(store, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}

	first, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "36000000-0000-4000-8000-000000000001", ExpectedParentProvided: true,
		Source: "notesync-aba-a1", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "topic.md", Markdown: "# Topic\nshared lesson body uses enough stable words for identity continuity alpha state\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstExport, err := service.Export(ctx, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := first.Revision.ID
	second, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "36000000-0000-4000-8000-000000000002", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "notesync-aba-b", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "topic.md", Markdown: strings.Replace(firstExport.Documents[0].Markdown, "alpha state", "beta state", 1)}},
	})
	if err != nil {
		var knowledgeErr *knowledge.Error
		if errors.As(err, &knowledgeErr) {
			t.Fatalf("B import failed: %v review=%+v", err, knowledgeErr.Review)
		}
		t.Fatal(err)
	}
	secondExport, err := service.Export(ctx, second.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent = second.Revision.ID
	third, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "36000000-0000-4000-8000-000000000003", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "notesync-aba-a2", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "topic.md", Markdown: strings.Replace(secondExport.Documents[0].Markdown, "beta state", "alpha state", 1)}},
	})
	if err != nil {
		t.Fatalf("A->B->A canonical commit rolled back: %v", err)
	}
	firstDocument := first.Revision.Documents[0].Revision
	secondDocument := second.Revision.Documents[0].Revision
	thirdDocument := third.Revision.Documents[0].Revision
	if firstDocument.ID != thirdDocument.ID || firstDocument.ID == secondDocument.ID || firstDocument.DocumentID != thirdDocument.DocumentID {
		t.Fatalf("unexpected A->B->A document identities first=%+v second=%+v third=%+v", firstDocument, secondDocument, thirdDocument)
	}
	intents := readNotesyncIntents(t, pool)
	if len(intents) != 3 {
		t.Fatalf("A->B->A intents=%d want=3", len(intents))
	}
	firstIntent, firstOK := findNotesyncIntent(intents, firstDocument.DocumentID, first.Revision.RevisionNo)
	thirdIntent, thirdOK := findNotesyncIntent(intents, thirdDocument.DocumentID, third.Revision.RevisionNo)
	if !firstOK || !thirdOK || firstIntent.idempotencyKey == thirdIntent.idempotencyKey ||
		thirdIntent.idempotencyKey != notesyncintegration.CanonicalPublicationIdempotencyKey(
			thirdDocument.DocumentID, thirdDocument.ID, third.Revision.RevisionNo, 1,
		) {
		t.Fatalf("A->B->A outbox keys first=%+v third=%+v", firstIntent, thirdIntent)
	}
	var headID string
	var operationCount int
	if err := pool.QueryRow(ctx, `SELECT head_revision_id::text FROM knowledge_catalog WHERE singleton_id=1`).Scan(&headID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_import_operations WHERE operation_id::text LIKE '36000000-0000-4000-8000-%'`).Scan(&operationCount); err != nil {
		t.Fatal(err)
	}
	if headID != third.Revision.ID || operationCount != 3 {
		t.Fatalf("A->B->A atomic state head=%s third=%s operations=%d", headID, third.Revision.ID, operationCount)
	}
}

func TestPostgreSQLKnowledgeNotesyncPublicationAtomicityAndPrivacy(t *testing.T) {
	pool := notesyncKnowledgePool(t)
	ctx := context.Background()
	enabledStore := postgresstore.New(pool, postgresstore.WithNotesyncPublication())
	service, err := knowledge.NewService(enabledStore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}

	first, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "31000000-0000-4000-8000-000000000001", ExpectedParentProvided: true,
		Source: "notesync-first", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{
			{Path: "a.md", Markdown: "# Alpha\nfirst body keeps enough stable words for identity matching across edits\n"},
			{Path: "b.md", Markdown: "# Beta\nsecond body keeps enough stable words for identity matching across edits\n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	intents := readNotesyncIntents(t, pool)
	if len(intents) != 2 {
		t.Fatalf("first publication intents=%d want=2", len(intents))
	}
	firstDocuments := make(map[string]knowledge.SnapshotDocument, len(first.Revision.Documents))
	for _, snapshot := range first.Revision.Documents {
		firstDocuments[snapshot.Revision.DocumentID] = snapshot
		intent, ok := findNotesyncIntent(intents, snapshot.Revision.DocumentID, first.Revision.RevisionNo)
		if !ok {
			t.Fatalf("missing publication intent for document %s", snapshot.Revision.DocumentID)
		}
		if intent.revision != first.Revision.RevisionNo || intent.generation != 1 ||
			intent.idempotencyKey != notesyncintegration.CanonicalPublicationIdempotencyKey(
				snapshot.Revision.DocumentID, snapshot.Revision.ID, first.Revision.RevisionNo, 1,
			) {
			t.Fatalf("publication tuple=%+v", intent)
		}
		var payload postgresstore.NotesyncPublicationIntent
		if err := json.Unmarshal(intent.payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload != (postgresstore.NotesyncPublicationIntent{
			SchemaVersion: 1, DocumentID: snapshot.Revision.DocumentID,
			KnowledgeRevisionID: first.Revision.ID, DocumentRevisionID: snapshot.Revision.ID,
			PublicationReason: "canonical_revision",
		}) {
			t.Fatalf("publication payload=%+v", payload)
		}
	}

	replay, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "31000000-0000-4000-8000-000000000001", ExpectedParentProvided: true,
		Source: "notesync-first", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{
			{Path: "a.md", Markdown: "# Alpha\nfirst body keeps enough stable words for identity matching across edits\n"},
			{Path: "b.md", Markdown: "# Beta\nsecond body keeps enough stable words for identity matching across edits\n"},
		},
	})
	if err != nil || !replay.Replayed || len(readNotesyncIntents(t, pool)) != 2 {
		t.Fatalf("operation replay result=%+v intents=%d err=%v", replay, len(readNotesyncIntents(t, pool)), err)
	}

	exported, err := service.Export(ctx, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := first.Revision.ID
	unchanged, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "31000000-0000-4000-8000-000000000002", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "notesync-unchanged", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: exported.Documents[0].Path, Markdown: exported.Documents[0].Markdown}},
	})
	if err != nil || !unchanged.Unchanged || len(readNotesyncIntents(t, pool)) != 2 {
		t.Fatalf("unchanged result=%+v intents=%d err=%v", unchanged, len(readNotesyncIntents(t, pool)), err)
	}

	moved, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "31000000-0000-4000-8000-000000000003", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "notesync-path-only", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "moved/a.md", Markdown: exported.Documents[0].Markdown}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(readNotesyncIntents(t, pool)) != 2 {
		t.Fatal("path-only rename created a publication intent")
	}

	movedParent := moved.Revision.ID
	changed, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "31000000-0000-4000-8000-000000000004", ExpectedParentRevisionID: &movedParent,
		ExpectedParentProvided: true, Source: "notesync-content-change", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "moved/a.md", Markdown: exported.Documents[0].Markdown + "\nchanged body\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(readNotesyncIntents(t, pool)); got != 3 {
		t.Fatalf("changed publication intents=%d want=3", got)
	}

	disabledStore := postgresstore.New(pool)
	disabledService, err := knowledge.NewService(disabledStore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	changedParent := changed.Revision.ID
	disabled, err := disabledService.Import(ctx, knowledge.ImportCommand{
		OperationID: "31000000-0000-4000-8000-000000000005", ExpectedParentRevisionID: &changedParent,
		ExpectedParentProvided: true, Source: "notesync-disabled", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "disabled.md", Markdown: "# Disabled\nno intent\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(readNotesyncIntents(t, pool)); got != 3 {
		t.Fatalf("disabled publication intents=%d want=3", got)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET learner_generation=2,updated_at=clock_timestamp()
		WHERE owner_kind='outbox'`); err != nil {
		t.Fatal(err)
	}
	disabledParent := disabled.Revision.ID
	if _, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "31000000-0000-4000-8000-000000000006", ExpectedParentRevisionID: &disabledParent,
		ExpectedParentProvided: true, Source: "notesync-generation-mismatch", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "rollback.md", Markdown: "# Rollback\nno partial state\n"}},
	}); err == nil {
		t.Fatal("generation mismatch import unexpectedly succeeded")
	}
	var headID string
	if err := pool.QueryRow(ctx, `SELECT head_revision_id::text FROM knowledge_catalog WHERE singleton_id=1`).Scan(&headID); err != nil {
		t.Fatal(err)
	}
	var operationRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM knowledge_import_operations
		WHERE operation_id='31000000-0000-4000-8000-000000000006'`).Scan(&operationRows); err != nil {
		t.Fatal(err)
	}
	if headID != disabled.Revision.ID || operationRows != 0 || len(readNotesyncIntents(t, pool)) != 3 {
		t.Fatalf("generation mismatch left partial state head=%s operations=%d intents=%d", headID, operationRows, len(readNotesyncIntents(t, pool)))
	}

	redaction := privacy.LocalRedactionRequest{
		ErasureID: "32000000-0000-4000-8000-000000000001", Store: privacy.StoreKnowledgeContent,
		ReceiptID: "32000000-0000-4000-8000-000000000002", LearnerGeneration: 2,
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO devices(id,display_name,created_at)
		VALUES($1,'notesync privacy actor',clock_timestamp())
		ON CONFLICT (id) DO NOTHING`, integrationActorID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasures(
			id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,
			target_learner_generation,managed_backup_scheduled_unrecoverable_after)
		VALUES($1,$2,$3,decode(repeat('32',32),'hex'),'learner_request',$2,
		       clock_timestamp(),$4,clock_timestamp()+interval '1 day')`,
		redaction.ErasureID, integrationActorID, "32000000-0000-4000-8000-000000000003", redaction.LearnerGeneration); err != nil {
		t.Fatal(err)
	}
	seedNotesyncPrivacyState(t, pool, first, firstDocuments, intents)
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION privacy_begin_owner_scrub(
			requested_erasure_id UUID,requested_target_generation BIGINT,
			requested_owner_kind TEXT,requested_receipt_id UUID
		) RETURNS UUID LANGUAGE sql AS $$ SELECT requested_receipt_id $$;
		CREATE OR REPLACE FUNCTION privacy_owner_scrub_permitted(requested_owner_kind TEXT)
		RETURNS BOOLEAN LANGUAGE sql STABLE AS $$ SELECT TRUE $$`); err != nil {
		t.Fatal(err)
	}
	if err := enabledStore.RedactTx(ctx, redaction); err != nil {
		t.Fatal(err)
	}
	residual, err := enabledStore.VerifyRedacted(ctx, redaction)
	if err != nil || residual != 0 {
		t.Fatalf("notesync privacy residual=%d err=%v", residual, err)
	}
	var publicationStatus, attemptStatus string
	var publicationRemoteCleared, reviewsCleared, attemptContentCleared bool
	if err := pool.QueryRow(ctx, `
		SELECT status,remote_version IS NULL AND remote_last_time IS NULL
		FROM knowledge_notesync_publications LIMIT 1`).Scan(&publicationStatus, &publicationRemoteCleared); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT bool_and(
			status='closed' AND resolution_kind='privacy_redaction' AND
			CASE WHEN remote_missing
				THEN remote_version IS NULL AND remote_last_time IS NULL
				ELSE remote_version=0 AND remote_last_time=0
			END)
		FROM knowledge_notesync_reviews`).Scan(&reviewsCleared); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT status,base_missing AND base_markdown IS NULL AND base_sha256 IS NULL
		FROM knowledge_notesync_publication_attempts LIMIT 1`).Scan(&attemptStatus, &attemptContentCleared); err != nil {
		t.Fatal(err)
	}
	if publicationStatus != "redacted" || !publicationRemoteCleared || !reviewsCleared ||
		attemptStatus != "redacted" || !attemptContentCleared {
		t.Fatalf("redacted states publication=%s/%v reviews=%v attempt=%s/%v",
			publicationStatus, publicationRemoteCleared, reviewsCleared, attemptStatus, attemptContentCleared)
	}
}

func TestPostgreSQLKnowledgeNotesyncPublicationConsumerTransactionAndBootstrap(t *testing.T) {
	pool := notesyncKnowledgePool(t)
	ctx := context.Background()
	disabledService, err := knowledge.NewService(postgresstore.New(pool), knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := disabledService.Import(ctx, knowledge.ImportCommand{
		OperationID: "34000000-0000-4000-8000-000000000001", ExpectedParentProvided: true,
		Source: "notesync-bootstrap", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "topic/main.md", Markdown: "# Topic\nfirst body keeps enough stable words for identity matching across edits\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if intents := readNotesyncIntents(t, pool); len(intents) != 0 {
		t.Fatalf("disabled import emitted %d NoteSync intents", len(intents))
	}
	enabledStore := postgresstore.New(pool, postgresstore.WithNotesyncPublication(postgresstore.NotesyncPublicationConfig{
		Vault: "Knowledge", PathPrefix: "edu-agent",
	}))
	if inserted, err := enabledStore.BootstrapNotesyncPublications(ctx); err != nil || inserted != 1 {
		t.Fatalf("bootstrap inserted=%d err=%v", inserted, err)
	}
	if inserted, err := enabledStore.BootstrapNotesyncPublications(ctx); err != nil || inserted != 0 {
		t.Fatalf("idempotent bootstrap inserted=%d err=%v", inserted, err)
	}
	messageStore := outboxpostgresstore.New(pool)
	firstMessage := claimNotesyncPublication(t, messageStore, time.Now().UTC().Add(time.Minute))
	firstIntent, err := notesyncintegration.DecodePublicationIntent(firstMessage)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := enabledStore.CanApplyNotesyncPublication(ctx, firstMessage, firstIntent)
	if err != nil || !decision.Apply {
		t.Fatalf("first authority decision=%+v err=%v", decision, err)
	}
	var firstTarget string
	if err := enabledStore.ApplyNotesyncPublication(ctx, firstMessage, firstIntent, func(_ context.Context, work notesyncintegration.PublicationWork) (notesyncintegration.PublicationOutcome, error) {
		firstTarget = work.TargetMarkdown
		if work.RemoteVault != "Knowledge" || work.RemotePath != "edu-agent/topic/main.md" ||
			!strings.Contains(work.TargetMarkdown, "edu-agent-source-revision-id: "+first.Revision.ID) || work.Mapping != nil {
			t.Fatalf("unexpected first publication work: %+v", work)
		}
		return notesyncintegration.PublicationOutcome{Kind: notesyncintegration.OutcomeApplied, Remote: notesyncintegration.RemoteObservation{
			Markdown: work.TargetMarkdown, Version: 1, Ctime: work.TargetModifiedAt.UnixMilli(),
			Mtime: work.TargetModifiedAt.UnixMilli(), LastTime: work.TargetModifiedAt.UnixMilli() + 1,
		}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	firstHash := sha256.Sum256([]byte(firstTarget))
	var status, attemptStatus, baseMarkdown, baseHash string
	var publishedRevision int64
	if err := pool.QueryRow(ctx, `
		SELECT o.status,a.status,p.published_revision_no,p.base_markdown,encode(p.base_sha256,'hex')
		FROM outbox_messages o
		JOIN knowledge_notesync_publication_attempts a ON a.outbox_id=o.id
		JOIN knowledge_notesync_publications p ON p.document_id=a.document_id
		WHERE o.id=$1`, firstMessage.ID).Scan(&status, &attemptStatus, &publishedRevision, &baseMarkdown, &baseHash); err != nil {
		t.Fatal(err)
	}
	if status != "applied" || attemptStatus != "applied" || publishedRevision != first.Revision.RevisionNo ||
		baseMarkdown != firstTarget || baseHash != hex.EncodeToString(firstHash[:]) {
		t.Fatalf("atomic first finalization status=%s attempt=%s revision=%d base_match=%v hash=%s",
			status, attemptStatus, publishedRevision, baseMarkdown == firstTarget, baseHash)
	}

	enabledService, err := knowledge.NewService(enabledStore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	firstExport, err := enabledService.Export(ctx, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := first.Revision.ID
	second, err := enabledService.Import(ctx, knowledge.ImportCommand{
		OperationID: "34000000-0000-4000-8000-000000000002", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "notesync-second", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "topic/main.md", Markdown: strings.Replace(firstExport.Documents[0].Markdown, "first body", "second body", 1)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	secondMessage := claimNotesyncPublication(t, messageStore, time.Now().UTC().Add(2*time.Minute))
	secondIntent, err := notesyncintegration.DecodePublicationIntent(secondMessage)
	if err != nil {
		t.Fatal(err)
	}
	if err := enabledStore.ApplyNotesyncPublication(ctx, secondMessage, secondIntent, func(_ context.Context, work notesyncintegration.PublicationWork) (notesyncintegration.PublicationOutcome, error) {
		if work.Mapping == nil || work.Mapping.BaseMarkdown != firstTarget {
			t.Fatalf("second publication did not load exact durable base: %+v", work.Mapping)
		}
		return notesyncintegration.PublicationOutcome{Kind: notesyncintegration.OutcomeApplied, Remote: notesyncintegration.RemoteObservation{
			Markdown: work.TargetMarkdown, Version: 2, Ctime: work.TargetModifiedAt.Add(-time.Hour).UnixMilli(),
			Mtime: work.TargetModifiedAt.UnixMilli(), LastTime: work.TargetModifiedAt.UnixMilli() + 1,
		}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE knowledge_notesync_publications
		SET published_revision_no=published_revision_no-1
		WHERE document_id=$1`, second.Revision.Documents[0].Revision.DocumentID); err == nil {
		t.Fatal("notesync publication mapping moved backward")
	}
	if err := pool.QueryRow(ctx, `SELECT published_revision_no FROM knowledge_notesync_publications WHERE document_id=$1`, second.Revision.Documents[0].Revision.DocumentID).Scan(&publishedRevision); err != nil {
		t.Fatal(err)
	}
	if publishedRevision != second.Revision.RevisionNo {
		t.Fatalf("mapping revision=%d want=%d", publishedRevision, second.Revision.RevisionNo)
	}

	secondExport, err := enabledService.Export(ctx, second.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent = second.Revision.ID
	third, err := enabledService.Import(ctx, knowledge.ImportCommand{
		OperationID: "34000000-0000-4000-8000-000000000003", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "notesync-third", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "topic/main.md", Markdown: strings.Replace(secondExport.Documents[0].Markdown, "second body", "third body", 1)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	thirdMessage := claimNotesyncPublication(t, messageStore, time.Now().UTC().Add(3*time.Minute))
	thirdIntent, err := notesyncintegration.DecodePublicationIntent(thirdMessage)
	if err != nil {
		t.Fatal(err)
	}
	reviewOperation := func(_ context.Context, work notesyncintegration.PublicationWork) (notesyncintegration.PublicationOutcome, error) {
		return notesyncintegration.PublicationOutcome{
			Kind: notesyncintegration.OutcomeReview, ReviewKind: notesyncintegration.ReviewKindRemoteChanged,
			ReasonCode: notesyncintegration.ReviewReasonRemoteContentChanged,
			Remote:     notesyncintegration.RemoteObservation{Markdown: "remote drift", Version: 9, Ctime: 1, Mtime: 2, LastTime: 3},
		}, nil
	}
	if err := enabledStore.ApplyNotesyncPublication(ctx, thirdMessage, thirdIntent, reviewOperation); err != nil {
		t.Fatal(err)
	}
	var disposition string
	var reviewCount int
	if err := pool.QueryRow(ctx, `
		SELECT o.status,COALESCE(o.terminal_disposition,''),a.status,
		       (SELECT count(*) FROM knowledge_notesync_reviews r WHERE r.document_id=a.document_id AND r.status='open')
		FROM outbox_messages o JOIN knowledge_notesync_publication_attempts a ON a.outbox_id=o.id
		WHERE o.id=$1`, thirdMessage.ID).Scan(&status, &disposition, &attemptStatus, &reviewCount); err != nil {
		t.Fatal(err)
	}
	if status != "canceled" || disposition != "review_required" || attemptStatus != "review_required" || reviewCount != 1 {
		t.Fatalf("atomic review status=%s disposition=%s attempt=%s reviews=%d", status, disposition, attemptStatus, reviewCount)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE outbox_messages
		SET status='processing',terminal_disposition=NULL,lease_token=$2,lease_expires_at=clock_timestamp()+interval '1 minute'
		WHERE id=$1`, thirdMessage.ID, thirdMessage.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := enabledStore.ApplyNotesyncPublication(ctx, thirdMessage, thirdIntent, reviewOperation); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_notesync_reviews WHERE document_id=$1 AND status='open'`, third.Revision.Documents[0].Revision.DocumentID).Scan(&reviewCount); err != nil {
		t.Fatal(err)
	}
	if reviewCount != 1 {
		t.Fatalf("idempotent conflict replay created %d open reviews", reviewCount)
	}

	thirdExport, err := enabledService.Export(ctx, third.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent = third.Revision.ID
	_, err = enabledService.Import(ctx, knowledge.ImportCommand{
		OperationID: "34000000-0000-4000-8000-000000000004", ExpectedParentRevisionID: &parent,
		ExpectedParentProvided: true, Source: "notesync-fourth", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "topic/main.md", Markdown: strings.Replace(thirdExport.Documents[0].Markdown, "third body", "fourth body", 1)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fourthMessage := claimNotesyncPublication(t, messageStore, time.Now().UTC().Add(4*time.Minute))
	fourthIntent, err := notesyncintegration.DecodePublicationIntent(fourthMessage)
	if err != nil {
		t.Fatal(err)
	}
	deferredAt := time.Now().UTC().Add(time.Minute)
	if err := enabledStore.ApplyNotesyncPublication(ctx, fourthMessage, fourthIntent, func(context.Context, notesyncintegration.PublicationWork) (notesyncintegration.PublicationOutcome, error) {
		return notesyncintegration.PublicationOutcome{
			Kind: notesyncintegration.OutcomeDeferred, Category: "notesync_capability_unavailable", AvailableAt: deferredAt,
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status,attempts FROM outbox_messages WHERE id=$1`, fourthMessage.ID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 0 {
		t.Fatalf("deferred publication consumed budget: status=%s attempts=%d", status, attempts)
	}
	fourthMessage = claimNotesyncPublication(t, messageStore, deferredAt.Add(time.Second))
	failureAt := deferredAt.Add(2 * time.Second)
	failureErr := enabledStore.ApplyNotesyncPublication(ctx, fourthMessage, fourthIntent, func(context.Context, notesyncintegration.PublicationWork) (notesyncintegration.PublicationOutcome, error) {
		return notesyncintegration.PublicationOutcome{
			Kind: notesyncintegration.OutcomeFailed, Category: "notesync_publication_outcome_unknown",
		}, nil
	})
	var classified outbox.ClassifiedError
	if !errors.As(failureErr, &classified) {
		t.Fatalf("publication failure is not classified: %v", failureErr)
	}
	if classified.Category() != "notesync_publication_outcome_unknown" || classified.Permanent() {
		t.Fatalf("classified publication failure category=%q permanent=%t", classified.Category(), classified.Permanent())
	}
	if err := pool.QueryRow(ctx, `
		SELECT o.status,o.attempts,a.status
		FROM outbox_messages o
		JOIN knowledge_notesync_publication_attempts a ON a.outbox_id=o.id
		WHERE o.id=$1`, fourthMessage.ID).Scan(&status, &attempts, &attemptStatus); err != nil {
		t.Fatal(err)
	}
	if status != "processing" || attempts != 1 || attemptStatus != "unknown" {
		t.Fatalf("failed publication before worker finalization: status=%s attempts=%d attempt_status=%s", status, attempts, attemptStatus)
	}
	nextAttemptAt := failureAt.Add(time.Minute)
	if err := messageStore.MarkFailed(ctx, fourthMessage.ID, fourthMessage.LeaseToken, classified.Category(), failureAt, nextAttemptAt, false); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status,attempts FROM outbox_messages WHERE id=$1`, fourthMessage.ID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 1 {
		t.Fatalf("failed publication did not preserve retry budget: status=%s attempts=%d", status, attempts)
	}
	fourthMessage = claimNotesyncPublication(t, messageStore, nextAttemptAt.Add(time.Second))
	if _, err := pool.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET learner_generation=2,updated_at=clock_timestamp()
		WHERE owner_kind IN ('knowledge','outbox')`); err != nil {
		t.Fatal(err)
	}
	decision, err = enabledStore.CanApplyNotesyncPublication(ctx, fourthMessage, fourthIntent)
	if err != nil || decision.Apply || decision.TerminalDisposition != outbox.DispositionPrivacyErasure {
		t.Fatalf("privacy fence decision=%+v err=%v", decision, err)
	}
	remoteCalls := 0
	if err := enabledStore.ApplyNotesyncPublication(ctx, fourthMessage, fourthIntent, func(context.Context, notesyncintegration.PublicationWork) (notesyncintegration.PublicationOutcome, error) {
		remoteCalls++
		return notesyncintegration.PublicationOutcome{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status,COALESCE(terminal_disposition,'') FROM outbox_messages WHERE id=$1`, fourthMessage.ID).Scan(&status, &disposition); err != nil {
		t.Fatal(err)
	}
	if remoteCalls != 0 || status != "canceled" || disposition != "privacy_erasure" {
		t.Fatalf("privacy fence remote_calls=%d status=%s disposition=%s", remoteCalls, status, disposition)
	}
}

func TestPostgreSQLKnowledgeNotesyncPublicationFencesConcurrentDocumentChange(t *testing.T) {
	pool := notesyncKnowledgePool(t)
	ctx := context.Background()
	store := postgresstore.New(pool, postgresstore.WithNotesyncPublication(postgresstore.NotesyncPublicationConfig{
		Vault: "Knowledge", PathPrefix: "edu-agent",
	}))
	service, err := knowledge.NewService(store, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "35000000-0000-4000-8000-000000000001", ExpectedParentProvided: true,
		Source: "notesync-concurrency-first", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "topic.md", Markdown: "# Topic\nfirst body keeps enough stable words for identity matching across edits\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	message := claimNotesyncPublication(t, outboxpostgresstore.New(pool), time.Now().UTC())
	intent, err := notesyncintegration.DecodePublicationIntent(message)
	if err != nil {
		t.Fatal(err)
	}
	remoteEntered := make(chan struct{})
	releaseRemote := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseRemote)
		}
	}()
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- store.ApplyNotesyncPublication(ctx, message, intent, func(_ context.Context, work notesyncintegration.PublicationWork) (notesyncintegration.PublicationOutcome, error) {
			close(remoteEntered)
			<-releaseRemote
			return notesyncintegration.PublicationOutcome{Kind: notesyncintegration.OutcomeApplied, Remote: notesyncintegration.RemoteObservation{
				Markdown: work.TargetMarkdown, Version: 1, Ctime: work.TargetModifiedAt.UnixMilli(),
				Mtime: work.TargetModifiedAt.UnixMilli(), LastTime: work.TargetModifiedAt.UnixMilli() + 1,
			}}, nil
		})
	}()
	select {
	case <-remoteEntered:
	case err := <-applyDone:
		t.Fatalf("publication exited before remote operation: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("publication did not reach remote operation")
	}

	exported, err := service.Export(ctx, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := first.Revision.ID
	type importResult struct {
		result knowledge.ImportResult
		err    error
	}
	importDone := make(chan importResult, 1)
	go func() {
		result, importErr := service.Import(ctx, knowledge.ImportCommand{
			OperationID: "35000000-0000-4000-8000-000000000002", ExpectedParentRevisionID: &parent,
			ExpectedParentProvided: true, Source: "notesync-concurrency-second", ActorDeviceID: integrationActorID,
			Documents: []knowledge.ImportDocument{{Path: "topic.md", Markdown: strings.Replace(exported.Documents[0].Markdown, "first body", "second body", 1)}},
		})
		importDone <- importResult{result: result, err: importErr}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case result := <-importDone:
			t.Fatalf("document change crossed active publication lock: result=%+v err=%v", result.result, result.err)
		default:
		}
		var waiting bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS(
			  SELECT 1 FROM pg_stat_activity activity
			  JOIN pg_locks lock ON lock.pid=activity.pid
			  WHERE activity.datname=current_database()
			    AND activity.query LIKE '%notesync-publication:%'
			    AND lock.locktype='advisory' AND NOT lock.granted
			)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("document change did not wait on the publication authority lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(releaseRemote)
	released = true
	if err := <-applyDone; err != nil {
		t.Fatal(err)
	}
	var second knowledge.ImportResult
	select {
	case result := <-importDone:
		if result.err != nil {
			t.Fatal(result.err)
		}
		second = result.result
	case <-time.After(2 * time.Second):
		t.Fatal("document change did not resume after publication committed")
	}
	var publishedRevision int64
	var headID string
	if err := pool.QueryRow(ctx, `
		SELECT publication.published_revision_no,catalog.head_revision_id::text
		FROM knowledge_notesync_publications publication
		CROSS JOIN knowledge_catalog catalog
		WHERE publication.document_id=$1 AND catalog.singleton_id=1`, intent.DocumentID).Scan(
		&publishedRevision, &headID,
	); err != nil {
		t.Fatal(err)
	}
	if publishedRevision != first.Revision.RevisionNo || headID != second.Revision.ID {
		t.Fatalf("publication/import ordering published=%d head=%s second=%s", publishedRevision, headID, second.Revision.ID)
	}
}

func claimNotesyncPublication(t *testing.T, store *outboxpostgresstore.Store, now time.Time) outbox.Message {
	t.Helper()
	messages, err := store.ClaimBusinessTypes(context.Background(), now, time.Minute, 1, []string{postgresstore.NotesyncPublicationBusinessType})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("claimed NoteSync publications=%d want=1", len(messages))
	}
	return messages[0]
}

type notesyncIntentRow struct {
	aggregateID    string
	outboxID       string
	idempotencyKey string
	revision       int64
	generation     int64
	payload        []byte
}

func readNotesyncIntents(t *testing.T, pool *pgxpool.Pool) []notesyncIntentRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT id::text,aggregate_id,idempotency_key,revision,generation,payload
		FROM outbox_messages
		WHERE business_type=$1
		ORDER BY aggregate_id,revision`, postgresstore.NotesyncPublicationBusinessType)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var intents []notesyncIntentRow
	for rows.Next() {
		var row notesyncIntentRow
		if err := rows.Scan(&row.outboxID, &row.aggregateID, &row.idempotencyKey, &row.revision, &row.generation, &row.payload); err != nil {
			t.Fatal(err)
		}
		intents = append(intents, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return intents
}

func findNotesyncIntent(intents []notesyncIntentRow, aggregateID string, revision int64) (notesyncIntentRow, bool) {
	for _, intent := range intents {
		if intent.aggregateID == aggregateID && intent.revision == revision {
			return intent, true
		}
	}
	return notesyncIntentRow{}, false
}

func seedNotesyncPrivacyState(
	t *testing.T,
	pool *pgxpool.Pool,
	first knowledge.ImportResult,
	documents map[string]knowledge.SnapshotDocument,
	intents []notesyncIntentRow,
) {
	t.Helper()
	var document knowledge.SnapshotDocument
	for _, candidate := range documents {
		document = candidate
		break
	}
	intent, ok := findNotesyncIntent(intents, document.Revision.DocumentID, first.Revision.RevisionNo)
	if !ok {
		t.Fatalf("missing seeded notesync intent for document %s", document.Revision.DocumentID)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO knowledge_notesync_publications(
			document_id,remote_vault,remote_path,published_knowledge_revision_id,
			published_document_revision_id,published_revision_no,base_markdown,base_sha256,
			remote_version,remote_last_time,generation,status,created_at,updated_at)
		VALUES($1,'vault','managed/note.md',$2,$3,$4,$5,decode(repeat('11',32),'hex'),7,1000,1,'active',$6,$6)`,
		document.Revision.DocumentID, first.Revision.ID, document.Revision.ID,
		first.Revision.RevisionNo, document.Revision.CanonicalMarkdown, first.Revision.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO knowledge_notesync_publication_attempts(
			id,outbox_id,idempotency_key,document_id,knowledge_revision_id,document_revision_id,
			knowledge_revision_no,generation,publication_reason,status,base_missing,base_markdown,
			base_sha256,created_at,updated_at)
		VALUES('33000000-0000-4000-8000-000000000001',$1,$2,$3,$4,$5,$6,1,
		       'canonical_revision','prepared',FALSE,$7,decode(repeat('22',32),'hex'),$8,$8)`,
		intent.outboxID, intent.idempotencyKey, document.Revision.DocumentID, first.Revision.ID,
		document.Revision.ID, first.Revision.RevisionNo, document.Revision.CanonicalMarkdown,
		first.Revision.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO knowledge_notesync_reviews(
			review_id,document_id,remote_vault,remote_path,kind,reason_code,status,
			head_knowledge_revision_id,head_knowledge_revision_no,canonical_path,
			base_missing,base_knowledge_revision_id,base_knowledge_revision_no,
			base_document_revision_id,base_remote_path,base_remote_version,base_remote_last_time,base_markdown,base_sha256,
			local_missing,local_knowledge_revision_id,local_knowledge_revision_no,
			local_document_revision_id,local_markdown,local_sha256,
			remote_missing,remote_markdown,remote_sha256,remote_version,remote_last_time,
			base_to_local_diff,base_to_remote_diff,local_diff_truncated,remote_diff_truncated,
			basis_hash,generation,created_at,updated_at)
		VALUES('33000000-0000-4000-8000-000000000002',$1,'vault','managed/note.md',
		       'remote_changed','remote_content_changed','open',$2,$3,$6,
		       FALSE,$2,$3,$4,'managed/note.md',7,1000,$5,decode(repeat('11',32),'hex'),
		       FALSE,$2,$3,$4,$5,decode(repeat('22',32),'hex'),
		       FALSE,'remote body',decode(repeat('33',32),'hex'),8,2000,
		       '','',FALSE,FALSE,decode(repeat('44',32),'hex'),1,$7,$7)`,
		document.Revision.DocumentID, first.Revision.ID, first.Revision.RevisionNo,
		document.Revision.ID, document.Revision.CanonicalMarkdown, document.Path, first.Revision.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO knowledge_notesync_reviews(
			review_id,document_id,remote_vault,remote_path,kind,reason_code,status,
			head_knowledge_revision_id,head_knowledge_revision_no,canonical_path,
			base_missing,base_knowledge_revision_id,base_knowledge_revision_no,
			base_document_revision_id,base_remote_path,base_remote_version,base_remote_last_time,base_markdown,base_sha256,
			local_missing,local_knowledge_revision_id,local_knowledge_revision_no,
			local_document_revision_id,local_markdown,local_sha256,
			remote_missing,remote_markdown,remote_sha256,remote_version,remote_last_time,
			base_to_local_diff,base_to_remote_diff,local_diff_truncated,remote_diff_truncated,
			basis_hash,generation,created_at,updated_at)
		VALUES('33000000-0000-4000-8000-000000000003',$1,'vault','managed/missing.md',
		       'remote_missing','remote_note_missing','open',$2,$3,$6,
		       FALSE,$2,$3,$4,'managed/missing.md',7,1000,$5,decode(repeat('11',32),'hex'),
		       FALSE,$2,$3,$4,$5,decode(repeat('22',32),'hex'),
		       TRUE,NULL,NULL,NULL,NULL,
		       '','',FALSE,FALSE,decode(repeat('45',32),'hex'),1,$7,$7)`,
		document.Revision.DocumentID, first.Revision.ID, first.Revision.RevisionNo,
		document.Revision.ID, document.Revision.CanonicalMarkdown, document.Path, first.Revision.CreatedAt); err != nil {
		t.Fatal(err)
	}
}

func notesyncKnowledgePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL knowledge NoteSync integration test not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("knowledge_notesync_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
