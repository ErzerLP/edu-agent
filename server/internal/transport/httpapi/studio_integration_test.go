package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLStudioVersionsSnapshotRotationAndErasure(t *testing.T) {
	pool := webTestPool(t)
	ctx := context.Background()
	ids, err := identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := ids.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ids.ExchangePairingCode(ctx, code, "Studio integration")
	if err != nil {
		t.Fatal(err)
	}
	actor, err := ids.Authenticate(ctx, issued.Token, "learning:write")
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{41}, 32)
	content, err := learningcontent.New(pool, nil, key)
	if err != nil {
		t.Fatal(err)
	}
	kstore := knowledgedb.New(pool)
	ks, err := knowledge.NewService(kstore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Options{Identity: ids, Knowledge: ks, LearningSpaces: spacedb.New(pool), LearningContent: content, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: NewFixedWindowLimiter(1000, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	space := learningspace.DefaultID
	call := func(method, path, collection string, body any) (int, []byte) {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+issued.Token)
		r.Header.Set(learningspace.Header, space)
		if collection != "" {
			r.Header.Set(knowledge.CollectionHeader, collection)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.Bytes()
	}
	want := func(status int, method, path, collection string, body any) []byte {
		t.Helper()
		got, raw := call(method, path, collection, body)
		if got != status {
			t.Fatalf("%s %s: want %d got %d %s", method, path, status, got, raw)
		}
		return raw
	}
	base := "/v1/learning/studio/artifacts"
	command := learningcontent.StudioSave{OperationID: uuid.NewString(), Generation: 1, Kind: "note", Title: "私密笔记", Markdown: "# 我的理解\n\nSTUDIO_SNAPSHOT_V1\n"}
	var saved learningcontent.StudioRevision
	raw := want(200, "POST", base, "", command)
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Version != 1 || saved.GoalID != nil || saved.Body.Markdown != command.Markdown {
		t.Fatal("invalid saved note", saved)
	}
	raw = want(200, "POST", base, "", command)
	var replay learningcontent.StudioRevision
	_ = json.Unmarshal(raw, &replay)
	if replay.ArtifactID != saved.ArtifactID || replay.Version != 1 {
		t.Fatal("create not idempotent")
	}
	command.Title = "changed operation"
	want(409, "POST", base, "", command)
	command.Title = saved.Body.Title
	path := base + "/" + saved.ArtifactID
	var candidate knowledge.NoteSourceCandidate
	_ = json.Unmarshal(want(200, "POST", path+"/import-candidate", "", map[string]any{"version": 1}), &candidate)
	if candidate.DerivedFrom.Version != 1 || candidate.Document.Markdown != saved.Body.Markdown {
		t.Fatal("candidate not saved snapshot")
	}
	collection := uuid.NewString()
	want(200, "POST", "/v1/knowledge/collections", "", map[string]any{"id": collection, "action": "create", "name": "笔记副本", "source": "test"})
	importCmd := knowledge.ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, Source: "note snapshot", Documents: []knowledge.ImportDocument{candidate.Document}}
	var preview knowledge.ImportPreview
	_ = json.Unmarshal(want(200, "POST", "/v1/knowledge/imports/previews", collection, importCmd), &preview)
	want(200, "GET", "/v1/knowledge/sources", "", nil)
	var imported knowledge.ImportResult
	_ = json.Unmarshal(want(200, "POST", "/v1/knowledge/imports/confirm", collection, knowledge.ConfirmImportCommand{Request: importCmd, Receipt: preview.Receipt}), &imported)
	want(200, "GET", "/v1/knowledge/imports/operations/"+importCmd.OperationID, collection, nil)
	var page knowledge.SourcePage
	_ = json.Unmarshal(want(200, "GET", "/v1/knowledge/sources", "", nil), &page)
	if len(page.Items) != 1 || page.Items[0].Metadata.DerivedFrom == nil || *page.Items[0].Metadata.DerivedFrom != candidate.DerivedFrom {
		t.Fatal("missing derived identity", page)
	}
	command.ExpectedVersion = 1
	command.Markdown = "# 新理解\n\nSTUDIO_V2\n"
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := command
			c.OperationID = uuid.NewString()
			status, _ := call("PUT", path, "", c)
			codes <- status
		}()
	}
	wg.Wait()
	close(codes)
	count := map[int]int{}
	for code := range codes {
		count[code]++
	}
	if count[200] != 1 || count[409] != 1 {
		t.Fatal("concurrent save must yield one conflict", count)
	}
	_ = json.Unmarshal(want(200, "GET", path, "", nil), &saved)
	if saved.Version != 2 {
		t.Fatal(saved)
	}
	want(200, "GET", path+"?version=1", "", nil)
	want(200, "GET", path+"/revisions", "", nil)
	want(200, "GET", "/v1/learning/studio/operations/"+command.OperationID, "", nil)
	var detail knowledge.SourceDetail
	item := page.Items[0]
	_ = json.Unmarshal(want(200, "GET", "/v1/knowledge/sources/"+item.RevisionID+"/documents/"+item.DocumentRevisionID, item.CollectionID, nil), &detail)
	found := false
	for _, f := range detail.Fragments {
		if bytes.Contains([]byte(f.Text), []byte("STUDIO_SNAPSHOT_V1")) {
			found = true
		}
		if bytes.Contains([]byte(f.Text), []byte("STUDIO_V2")) {
			t.Fatal("snapshot drift")
		}
	}
	if !found {
		t.Fatal("snapshot lost")
	}
	var ciphertext []byte
	if err = pool.QueryRow(ctx, `SELECT ciphertext FROM learning_studio_revisions WHERE artifact_id=$1 AND version=1`, saved.ArtifactID).Scan(&ciphertext); err != nil || bytes.Contains(ciphertext, []byte("STUDIO_SNAPSHOT_V1")) {
		t.Fatal("plaintext at rest", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE learning_studio_revisions SET ciphertext=$2 WHERE artifact_id=$1`, saved.ArtifactID, ciphertext); err == nil {
		t.Fatal("immutable history bypassed")
	}
	newKey := bytes.Repeat([]byte{72}, 32)
	rewrap, _ := keyrotation.New(key, newKey)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `LOCK TABLE learning_content_revisions IN ACCESS EXCLUSIVE MODE; SET LOCAL app.mentor_key_rotation='on'`); err != nil {
		t.Fatal(err)
	}
	if err = learningcontent.RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rotated, _ := learningcontent.New(pool, nil, newKey)
	read, err := rotated.StudioGet(ctx, actor, saved.ArtifactID, 1)
	if err != nil || read.Body.Markdown != candidate.Document.Markdown {
		t.Fatal("rotation lost note", err)
	}
	spaceStore := spacedb.New(pool)
	other, err := spaceStore.Mutate(ctx, actor.Device.ID, "", learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 0, Name: "other", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	space = other.ID
	want(404, "GET", path, "", nil)
	space = learningspace.DefaultID
	tutoring := tutoringdb.New(pool)
	ps := privacydb.New(pool, privacydb.WithLocalOwner(identitydb.New(pool)), privacydb.WithLocalOwner(kstore), privacydb.WithLocalOwner(learningdb.New(pool, tutoring)), privacydb.WithLocalOwner(tutoring), privacydb.WithLocalOwner(memorydb.New(pool)), privacydb.WithLocalOwner(outboxdb.New(pool)))
	grants, _ := privacy.NewErasureGrantService(privacydb.NewGrantStore(pool), privacy.ErasureGrantOptions{})
	grant, err := grants.Issue(ctx, actor.Device.ID, "Studio clear")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	barrier, err := ps.CommitBarrierAuthorized(ctx, privacy.ErasureRequest{DeviceID: actor.Device.ID, OperationID: uuid.NewString(), ActorDeviceID: actor.Device.ID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}, privacy.NewErasureGrantAuthorization(actor.Device.ID, grant.Token))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rotated.StudioSave(ctx, actor, "", learningcontent.StudioSave{OperationID: uuid.NewString(), Generation: 1, Kind: "note", Title: "late", Markdown: "late"}); err == nil {
		t.Fatal("privacy write gate bypassed")
	}
	if _, err = ps.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
	n, err := learningcontent.Remaining(ctx, pool)
	if err != nil || n != 0 {
		t.Fatal("Studio survived owner scrub", n, err)
	}
	command.ExpectedVersion = 0
	if _, err = rotated.StudioSave(ctx, actor, "", command); err == nil {
		t.Fatal("old create resurrected after clear")
	}
	if _, err = rotated.StudioGet(ctx, actor, saved.ArtifactID, 1); err == nil {
		t.Fatal("erased note readable")
	}
	if _, err = ks.PreviewImport(ctx, importCmd); err == nil {
		t.Fatal("erased candidate replayed")
	}
}
