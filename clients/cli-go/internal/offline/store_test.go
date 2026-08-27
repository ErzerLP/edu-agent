package offline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorePackImmutableOperationStateSummaryAndDiscard(t *testing.T) {
	root, _, _, store := createTestStore(t)
	ctx := context.Background()
	pack := testPack(testPackID, "PROMPT_DO_NOT_LEAK")
	if err := store.SavePack(ctx, pack); err != nil {
		t.Fatal(err)
	}
	packs, err := store.ListAvailablePacks(ctx, time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || len(packs) != 1 || !packs[0].Available {
		t.Fatalf("available packs: %#v %v", packs, err)
	}
	loaded, err := store.GetPack(ctx, testPackID)
	if err != nil || !bytes.Equal(loaded.Canonical, pack.Canonical) {
		t.Fatalf("get pack: %#v %v", loaded, err)
	}

	operation := testOperation(testOperationID, 9, "ANSWER_DO_NOT_LEAK")
	if err := store.SaveImmutableOperation(ctx, operation); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveImmutableOperation(ctx, operation); err != nil {
		t.Fatalf("exact replay should be idempotent: %v", err)
	}
	changed := operation
	changed.Canonical = json.RawMessage(`{"answer":"changed","operation_id":"` + testOperationID + `"}`)
	requireErrorIs(t, store.SaveImmutableOperation(ctx, changed), ErrImmutableOperation)
	sequenceConflict := testOperation(testOperationTwo, 9, "different")
	requireErrorIs(t, store.SaveImmutableOperation(ctx, sequenceConflict), ErrImmutableOperation)
	queued, err := store.ListQueuedOperations(ctx, 50)
	if err != nil || len(queued) != 1 || queued[0].DeviceSequence != 9 || !bytes.Equal(queued[0].Canonical, operation.Canonical) {
		t.Fatalf("queued operations: %#v %v", queued, err)
	}

	terminal := SyncResult{OperationID: testOperationID, SubmissionID: testSubmissionID, State: StateTerminal, Receipt: json.RawMessage(`{"receipt_id":"r"}`), Status: json.RawMessage(`{"state":"done"}`), UpdatedAt: time.Now().UTC()}
	requireErrorIs(t, store.ApplySyncResult(ctx, terminal), ErrInvalidState)
	batch, err := store.BeginSync(ctx, testJournalID, []string{testOperationID})
	if err != nil || len(batch.Operations) != 1 {
		t.Fatalf("begin sync: %#v %v", batch, err)
	}
	archived := SyncResult{OperationID: testOperationID, SubmissionID: testSubmissionID, State: StateArchivedPendingEvidence, ArchiveStatus: "archived_succeeded", AssessmentStatus: "queued", EvidenceStatus: "pending_evaluation", Receipt: json.RawMessage(`{"receipt_id":"r"}`), Status: json.RawMessage(`{"ticket_id":"t"}`), UpdatedAt: time.Date(2029, 1, 2, 0, 0, 0, 0, time.UTC)}
	if err := store.ApplySyncResult(ctx, archived); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetOperation(ctx, testOperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("durable receipt retained full operation: %v", err)
	}
	if err := store.FinishSync(ctx, testJournalID); err != nil {
		t.Fatal(err)
	}
	archived.State = StateTerminal
	archived.AssessmentStatus = "completed"
	archived.EvidenceStatus = "accepted"
	archived.UpdatedAt = archived.UpdatedAt.Add(time.Hour)
	if err := store.SaveStatus(ctx, archived); err != nil {
		t.Fatal(err)
	}
	status, err := store.GetStatus(ctx, testOperationID)
	if err != nil || status.State != StateTerminal || status.EvidenceStatus != "accepted" {
		t.Fatalf("terminal status: %#v %v", status, err)
	}
	archived.State = StateQueued
	requireErrorIs(t, store.SaveStatus(ctx, archived), ErrInvalidState)

	summary, err := store.Summary(ctx, time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if summary.PackCount != 1 || summary.TerminalCount != 1 || summary.QueuedCount != 0 || summary.LastSuccessfulSync == nil {
		t.Fatalf("unexpected summary: %#v", summary)
	}
	encoded, _ := json.Marshal(summary)
	if bytes.Contains(encoded, []byte("PROMPT_DO_NOT_LEAK")) || bytes.Contains(encoded, []byte("ANSWER_DO_NOT_LEAK")) {
		t.Fatal("summary leaked sealed body")
	}
	if data, err := os.ReadFile(filepath.Join(root, "profile.key")); err != nil || bytes.Contains(data, []byte("ANSWER_DO_NOT_LEAK")) {
		t.Fatal("key file leaked body")
	}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(data, []byte("ANSWER_DO_NOT_LEAK")) || bytes.Contains(data, []byte("PROMPT_DO_NOT_LEAK")) {
			t.Fatalf("plaintext found in %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	preflight, err := store.PreflightLogout(ctx)
	if err != nil || preflight.Nonterminal || preflight.PendingJournals {
		t.Fatalf("terminal store should pass preflight: %#v %v", preflight, err)
	}
	if err := store.Discard(ctx, ObjectPack, testPackID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPack(ctx, testPackID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("discarded pack remains: %v", err)
	}
	if err := store.Discard(ctx, ObjectReceipt, testOperationID); err != nil {
		t.Fatal(err)
	}
	if err := store.DiscardAll(ctx); err != nil {
		t.Fatal(err)
	}
	exists, err := Exists(root)
	if err != nil || exists {
		t.Fatalf("discard all left profile: exists=%v err=%v", exists, err)
	}
	requireErrorIs(t, store.SavePack(ctx, testPack(testPackIDTwo, "x")), ErrClosed)
}

func TestPreflightTracksQueuedAndPrepareJournal(t *testing.T) {
	_, _, _, store := createTestStore(t)
	ctx := context.Background()
	if err := store.SaveImmutableOperation(ctx, testOperation(testOperationID, 1, "answer")); err != nil {
		t.Fatal(err)
	}
	intent := PrepareIntent{RequestID: testJournalID, CreatedAt: time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC), Canonical: json.RawMessage(`{"operation_id":"` + testJournalID + `"}`)}
	if err := store.SavePrepareIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingPrepareIntent(ctx)
	if err != nil || !bytes.Equal(pending.Canonical, intent.Canonical) {
		t.Fatalf("pending prepare: %#v %v", pending, err)
	}
	preflight, err := store.PreflightLogout(ctx)
	if err != nil || !preflight.Nonterminal || !preflight.PendingJournals || preflight.NonterminalCount != 1 {
		t.Fatalf("preflight: %#v %v", preflight, err)
	}
	if err := store.ClearPrepareIntent(ctx, testJournalID); err != nil {
		t.Fatal(err)
	}
	if err := store.Discard(ctx, ObjectOperation, testOperationID); err != nil {
		t.Fatal(err)
	}
	preflight, err = store.PreflightLogout(ctx)
	if err != nil || preflight.Nonterminal || preflight.PendingJournals {
		t.Fatalf("cleared preflight: %#v %v", preflight, err)
	}
}

func TestPurgeSessionPreservesSystemSourceCleanupResponsibility(t *testing.T) {
	root, _, _, store := createTestStore(t)
	if err := atomicWriteManaged(root, keyMigrationSourceBackendFile, []byte{KeyBackendSystem}, false); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	purge, err := BeginPurgeProfile(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if purge.KeyBackend() != KeyBackendSystem {
		t.Fatalf("purge backend=%d", purge.KeyBackend())
	}
	if err := purge.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeSessionPreservesBackendAcrossAcknowledgmentRetry(t *testing.T) {
	root, _, _, store := createTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	purge, err := BeginPurgeProfile(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if purge.KeyBackend() != KeyBackendPassphrase {
		t.Fatalf("initial purge backend=%d", purge.KeyBackend())
	}
	if err := purge.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("retry marker root missing: %v", err)
	}
	retry, err := BeginPurgeProfile(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if retry.KeyBackend() != KeyBackendPassphrase {
		t.Fatalf("retry purge backend=%d", retry.KeyBackend())
	}
	if err := retry.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purged root remains: %v", err)
	}
}

func TestPreparePublicationRecoversEveryCrashBoundary(t *testing.T) {
	stages := []preparePublishStage{
		preparePublishAfterJournalDurable,
		preparePublishAfterPackDurable,
		preparePublishAfterTrustDurable,
		preparePublishAfterIntentCompleted,
		preparePublishAfterJournalDeleted,
	}
	for _, stage := range stages {
		t.Run(string(stage), func(t *testing.T) {
			root, binding, baseTrust, store := createTestStore(t)
			nextTrust, err := NewTrustState(json.RawMessage(`{"manifest_digest":"def","manifest_revision":"2","payload":{"issuer":"edu-agent"}}`))
			if err != nil {
				t.Fatal(err)
			}
			intent := PrepareIntent{
				RequestID:  testJournalID,
				CreatedAt:  time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC),
				Canonical:  json.RawMessage(`{"operation_id":"` + testJournalID + `"}`),
				TrustState: baseTrust,
			}
			if err := store.SavePrepareIntent(t.Context(), intent); err != nil {
				t.Fatal(err)
			}
			pack := testPack(testPackID, "publish crash prompt")
			crash := errors.New("simulated prepare publish crash")
			failed := false
			err = store.publishPreparedPack(t.Context(), intent.RequestID, baseTrust, nextTrust, pack, func(current preparePublishStage) error {
				if !failed && current == stage {
					failed = true
					return crash
				}
				return nil
			})
			if !errors.Is(err, crash) || !failed {
				t.Fatalf("publish at %s returned %v", stage, err)
			}
			journalPath := filepath.Join(root, filepath.FromSlash(objectRelative(ObjectJournal, intent.RequestID)))
			journalBytes, journalErr := os.ReadFile(journalPath)
			if stage == preparePublishAfterJournalDeleted {
				if !errors.Is(journalErr, os.ErrNotExist) {
					t.Fatalf("deleted publication journal remains: %v", journalErr)
				}
			} else {
				if journalErr != nil {
					t.Fatal(journalErr)
				}
				if bytes.Contains(journalBytes, []byte("publish crash prompt")) {
					t.Fatal("sealed publication journal leaked pack plaintext")
				}
				if err := store.withLease(t.Context(), LeaseExclusive, func() error {
					var journal JournalRecord
					if err := store.readRecordLocked(ObjectJournal, intent.RequestID, &journal); err != nil {
						return err
					}
					var detail prepareDetail
					if err := decodeClosed(journal.Detail, &detail); err != nil {
						return err
					}
					expectedRecord, err := packToRecord(pack)
					if err != nil {
						return err
					}
					expectedBytes, err := marshalCanonical(expectedRecord)
					if err != nil {
						return err
					}
					actualBytes, err := marshalCanonical(detail.PackRecord)
					if err != nil {
						return err
					}
					if detail.PublicationVersion != preparePublicationVersion || detail.RequestDigest == "" || detail.BaseTrustStateDigest == "" || detail.NextTrustStateDigest == "" || detail.PackRecordDigest == "" || !bytes.Equal(actualBytes, expectedBytes) {
						return errors.New("publication journal is not self-contained")
					}
					if stage == preparePublishAfterIntentCompleted && (journal.State != "completed" || journal.Revision != "3") {
						return errors.New("prepare intent completion was not durable")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := store.GetPack(t.Context(), pack.ID); !errors.Is(err, ErrCorruptStore) {
					t.Fatalf("business read crossed unfinished publication at %s: %v", stage, err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := OpenPassphrase(t.Context(), root, binding, baseTrust, testPassphrase)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.Recover(t.Context()); err != nil {
				t.Fatalf("first recovery re-entry failed: %v", err)
			}
			if err := reopened.Recover(t.Context()); err != nil {
				t.Fatalf("second recovery re-entry failed: %v", err)
			}
			loaded, err := reopened.GetPack(t.Context(), pack.ID)
			if err != nil || !bytes.Equal(loaded.Canonical, pack.Canonical) {
				t.Fatalf("published pack=%#v err=%v", loaded, err)
			}
			if !bytes.Equal(reopened.TrustState().Bytes(), nextTrust.Bytes()) {
				t.Fatalf("recovered trust=%s", reopened.TrustState().Bytes())
			}
			if _, err := reopened.PendingPrepareIntent(t.Context()); !errors.Is(err, ErrNotFound) {
				t.Fatalf("completed prepare intent remains: %v", err)
			}
			if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("publication journal was not cleared: %v", err)
			}
			packs, err := reopened.ListAvailablePacks(t.Context(), time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC))
			if err != nil || len(packs) != 1 {
				t.Fatalf("prepare recovery duplicated pack: %#v %v", packs, err)
			}
		})
	}
}

func canonicalPackAtSize(t *testing.T, size int) json.RawMessage {
	t.Helper()
	prefix := []byte(`{"marker":"MAX_PACK_DO_NOT_LEAK","pack_id":"` + testPackID + `","payload":"`)
	suffix := []byte(`"}`)
	if size < len(prefix)+len(suffix) {
		t.Fatalf("canonical pack size %d is too small for the test fixture", size)
	}
	value := make([]byte, size)
	offset := copy(value, prefix)
	for offset < size-len(suffix) {
		value[offset] = 'x'
		offset++
	}
	copy(value[offset:], suffix)
	return json.RawMessage(value)
}

func TestPreparePublicationMaximumCanonicalPackRecoversAfterJournalDurable(t *testing.T) {
	root, binding, baseTrust, store := createTestStore(t)
	nextTrust, err := NewTrustState(json.RawMessage(`{"manifest_digest":"def","manifest_revision":"2","payload":{"issuer":"edu-agent"}}`))
	if err != nil {
		t.Fatal(err)
	}
	intent := PrepareIntent{
		RequestID:  testJournalID,
		CreatedAt:  time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC),
		Canonical:  json.RawMessage(`{"operation_id":"` + testJournalID + `"}`),
		TrustState: baseTrust,
	}
	if err := store.SavePrepareIntent(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	pack := testPack(testPackID, "unused")
	pack.ItemCount = 20
	pack.Canonical = canonicalPackAtSize(t, MaxCanonicalPackBytes)
	record, err := packToRecord(pack)
	if err != nil {
		t.Fatal(err)
	}
	recordBytes, err := marshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	packRecordSize := len(recordBytes)
	zeroBytes(recordBytes)
	if packRecordSize <= MaxCanonicalPackBytes || packRecordSize+16 > MaxSealedObject {
		t.Fatalf("pack record size=%d sealed=%d limit=%d", packRecordSize, packRecordSize+16, MaxSealedObject)
	}

	crash := errors.New("stop after maximum-pack journal")
	if err := store.publishPreparedPack(t.Context(), intent.RequestID, baseTrust, nextTrust, pack, func(stage preparePublishStage) error {
		if stage == preparePublishAfterJournalDurable {
			return crash
		}
		return nil
	}); !errors.Is(err, crash) {
		t.Fatalf("interrupted publication returned %v", err)
	}
	packPath := filepath.Join(root, filepath.FromSlash(objectRelative(ObjectPack, pack.ID)))
	if _, err := os.Stat(packPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pack unexpectedly durable before recovery: %v", err)
	}
	journalPath := filepath.Join(root, filepath.FromSlash(objectRelative(ObjectJournal, intent.RequestID)))
	sealedJournal, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealedJournal, []byte("MAX_PACK_DO_NOT_LEAK")) {
		t.Fatal("sealed maximum-pack journal leaked plaintext")
	}
	header, err := DecodeObjectHeader(sealedJournal[:ObjectHeaderSize])
	if err != nil {
		t.Fatal(err)
	}
	var journalPlain []byte
	if err := store.withLease(t.Context(), LeaseExclusive, func() error {
		var readErr error
		journalPlain, readErr = store.rawRecordLocked(ObjectJournal, intent.RequestID)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	journalPlainSize := len(journalPlain)
	zeroBytes(journalPlain)
	if header.SealedLength != uint64(journalPlainSize+16) || journalPlainSize <= packRecordSize || header.SealedLength > MaxSealedObject {
		t.Fatalf("journal plain=%d sealed=%d pack record=%d limit=%d", journalPlainSize, header.SealedLength, packRecordSize, MaxSealedObject)
	}
	t.Logf("canonical_pack=%d pack_record=%d publication_journal_plain=%d publication_journal_sealed=%d sealed_limit=%d", len(pack.Canonical), packRecordSize, journalPlainSize, header.SealedLength, MaxSealedObject)

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPassphrase(t.Context(), root, binding, baseTrust, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.GetPack(t.Context(), pack.ID)
	if err != nil || !bytes.Equal(loaded.Canonical, pack.Canonical) {
		t.Fatalf("recovered maximum pack bytes=%d err=%v", len(loaded.Canonical), err)
	}
	if !bytes.Equal(reopened.TrustState().Bytes(), nextTrust.Bytes()) {
		t.Fatalf("recovered trust=%s", reopened.TrustState().Bytes())
	}
	if _, err := reopened.PendingPrepareIntent(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("completed maximum-pack journal remains: %v", err)
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("maximum-pack publication journal was not cleared: %v", err)
	}
}

func TestPreparePublicationRejectsOversizeCanonicalPackOnWriteAndRead(t *testing.T) {
	root, _, _, store := createTestStore(t)
	oversize := testPack(testPackID, "unused")
	oversize.Canonical = canonicalPackAtSize(t, MaxCanonicalPackBytes+1)
	_, conversionErr := packToRecord(oversize)
	if conversionErr == nil {
		t.Fatal("packToRecord accepted an oversized canonical pack")
	}
	if err := store.SavePack(t.Context(), oversize); err == nil || err.Error() != conversionErr.Error() {
		t.Fatalf("SavePack error=%v, want %v", err, conversionErr)
	}
	packPath := filepath.Join(root, filepath.FromSlash(objectRelative(ObjectPack, oversize.ID)))
	if _, err := os.Stat(packPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized pack reached persistence: %v", err)
	}

	maximum := oversize
	maximum.Canonical = canonicalPackAtSize(t, MaxCanonicalPackBytes)
	record, err := packToRecord(maximum)
	if err != nil {
		t.Fatal(err)
	}
	record.CanonicalBytes = append(json.RawMessage(nil), oversize.Canonical...)
	if err := record.validate(); err == nil || err.Error() != conversionErr.Error() {
		t.Fatalf("PackRecord.validate error=%v, want %v", err, conversionErr)
	}
	if err := store.withLease(t.Context(), LeaseExclusive, func() error {
		return store.writeRecordLocked(ObjectPack, record.PackID, record, false)
	}); err != nil {
		t.Fatal(err)
	}
	sealedPack := readObjectFile(t, root, ObjectPack, record.PackID)
	if bytes.Contains(sealedPack, []byte("MAX_PACK_DO_NOT_LEAK")) {
		t.Fatal("sealed oversized pack record leaked plaintext")
	}
	if _, err := store.GetPack(t.Context(), record.PackID); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("oversized sealed PackRecord read error=%v, want %v", err, ErrCorruptStore)
	}
}

func interruptedPreparePublication(t *testing.T) (string, Binding, TrustState, TrustState, Pack, *Store) {
	t.Helper()
	root, binding, baseTrust, store := createTestStore(t)
	if err := store.SavePack(t.Context(), testPack(testPackIDTwo, "existing readable body")); err != nil {
		t.Fatal(err)
	}
	nextTrust, err := NewTrustState(json.RawMessage(`{"manifest_digest":"def","manifest_revision":"2","payload":{"issuer":"edu-agent"}}`))
	if err != nil {
		t.Fatal(err)
	}
	intent := PrepareIntent{
		RequestID:  testJournalID,
		CreatedAt:  time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC),
		Canonical:  json.RawMessage(`{"operation_id":"` + testJournalID + `"}`),
		TrustState: baseTrust,
	}
	if err := store.SavePrepareIntent(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	pack := testPack(testPackID, "journal payload body")
	crash := errors.New("stop after journal")
	if err := store.publishPreparedPack(t.Context(), intent.RequestID, baseTrust, nextTrust, pack, func(stage preparePublishStage) error {
		if stage == preparePublishAfterJournalDurable {
			return crash
		}
		return nil
	}); !errors.Is(err, crash) {
		t.Fatalf("interrupt publication: %v", err)
	}
	return root, binding, baseTrust, nextTrust, pack, store
}

func mutatePreparePublicationJournal(t *testing.T, store *Store, mutate func(*JournalRecord, *prepareDetail)) {
	t.Helper()
	if err := store.withLease(t.Context(), LeaseExclusive, func() error {
		var journal JournalRecord
		if err := store.readRecordLocked(ObjectJournal, testJournalID, &journal); err != nil {
			return err
		}
		var detail prepareDetail
		if err := decodeClosed(journal.Detail, &detail); err != nil {
			return err
		}
		mutate(&journal, &detail)
		detailBytes, err := marshalCanonical(detail)
		if err != nil {
			return err
		}
		journal.Detail = detailBytes
		return store.writeRecordLocked(ObjectJournal, journal.JournalID, journal, true)
	}); err != nil {
		t.Fatal(err)
	}
}

func requirePublicationOpenFailsClosed(t *testing.T, root string, binding Binding, trust TrustState, store *Store) {
	t.Helper()
	journalPath := filepath.Join(root, filepath.FromSlash(objectRelative(ObjectJournal, testJournalID)))
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenPassphrase(t.Context(), root, binding, trust, testPassphrase)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("corrupt publication journal returned a readable Store")
	}
	if !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("open error=%v, want %v", err, ErrCorruptStore)
	}
	after, readErr := os.ReadFile(journalPath)
	if readErr != nil {
		t.Fatalf("diagnostic journal was not preserved: %v", readErr)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed open rewrote the diagnostic publication journal")
	}
}

func TestPreparePublicationInvalidJournalFailsClosedBeforeBusinessRead(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*JournalRecord, *prepareDetail)
	}{
		{
			name: "legacy detail without payload",
			mutate: func(_ *JournalRecord, detail *prepareDetail) {
				*detail = prepareDetail{
					Purpose: "prepare_publish", Request: detail.Request, TrustState: detail.TrustState,
					BaseTrustState: detail.BaseTrustState, NextTrustState: detail.NextTrustState,
					PackID: detail.PackID, PackRecordDigest: detail.PackRecordDigest,
				}
			},
		},
		{name: "missing pack payload", mutate: func(_ *JournalRecord, detail *prepareDetail) { detail.PackRecord = nil }},
		{name: "unknown publication version", mutate: func(_ *JournalRecord, detail *prepareDetail) { detail.PublicationVersion = 99 }},
		{name: "unknown journal version", mutate: func(journal *JournalRecord, _ *prepareDetail) { journal.JournalVersion = 99 }},
		{
			name: "tampered pack payload",
			mutate: func(_ *JournalRecord, detail *prepareDetail) {
				detail.PackRecord.CanonicalBytes = json.RawMessage(`{"pack_id":"` + testPackID + `","prompt":"tampered"}`)
			},
		},
		{name: "tampered pack digest", mutate: func(_ *JournalRecord, detail *prepareDetail) {
			detail.PackRecordDigest = string(bytes.Repeat([]byte("0"), 64))
		}},
		{
			name: "tampered trust checkpoint",
			mutate: func(_ *JournalRecord, detail *prepareDetail) {
				detail.NextTrustState = json.RawMessage(`{"manifest_digest":"tampered","manifest_revision":"2","payload":{"issuer":"edu-agent"}}`)
			},
		},
		{
			name: "request identity mismatch",
			mutate: func(_ *JournalRecord, detail *prepareDetail) {
				detail.Request = json.RawMessage(`{"operation_id":"88888888-8888-4888-8888-888888888888"}`)
				detail.RequestDigest, _ = canonicalDigest(detail.Request)
			},
		},
		{
			name: "pack identity mismatch",
			mutate: func(_ *JournalRecord, detail *prepareDetail) {
				detail.PackRecord.PackID = testPackIDTwo
				detail.PackRecordDigest, _ = packRecordDigest(*detail.PackRecord)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, binding, trust, _, _, store := interruptedPreparePublication(t)
			mutatePreparePublicationJournal(t, store, test.mutate)
			requirePublicationOpenFailsClosed(t, root, binding, trust, store)
		})
	}
}

func TestPreparePublicationRejectsTamperedSealedJournalBeforeBusinessRead(t *testing.T) {
	root, binding, trust, _, _, store := interruptedPreparePublication(t)
	journalPath := filepath.Join(root, filepath.FromSlash(objectRelative(ObjectJournal, testJournalID)))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	sealed, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 0xff
	if err := os.WriteFile(journalPath, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenPassphrase(t.Context(), root, binding, trust, testPassphrase)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("tampered sealed journal returned a readable Store")
	}
	if !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("open error=%v, want %v", err, ErrCorruptStore)
	}
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("tampered journal was not preserved: %v", err)
	}
}

func TestPreparePublicationRejectsExistingPackConflictAndPreservesJournal(t *testing.T) {
	root, binding, trust, _, pack, store := interruptedPreparePublication(t)
	conflict, err := packToRecord(testPack(pack.ID, "conflicting pack body"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.withLease(t.Context(), LeaseExclusive, func() error {
		return store.writeRecordLocked(ObjectPack, conflict.PackID, conflict, false)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPack(t.Context(), testPackIDTwo); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("existing business pack was readable across a conflict: %v", err)
	}
	requirePublicationOpenFailsClosed(t, root, binding, trust, store)
}

func TestPreparePublicationCompletesFromAlreadyAdvancedTrust(t *testing.T) {
	_, _, baseTrust, store := createTestStore(t)
	nextTrust, err := NewTrustState(json.RawMessage(`{"manifest_digest":"def","manifest_revision":"2","payload":{"issuer":"edu-agent"}}`))
	if err != nil {
		t.Fatal(err)
	}
	intent := PrepareIntent{
		RequestID:  testJournalID,
		CreatedAt:  time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC),
		Canonical:  json.RawMessage(`{"operation_id":"` + testJournalID + `"}`),
		TrustState: baseTrust,
	}
	if err := store.SavePrepareIntent(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTrustState(t.Context(), nextTrust); err != nil {
		t.Fatal(err)
	}
	pack := testPack(testPackID, "advanced checkpoint prompt")
	if err := store.PublishPreparedPack(t.Context(), intent.RequestID, nextTrust, nextTrust, pack); err != nil {
		t.Fatalf("already-advanced checkpoint did not complete: %v", err)
	}
	if _, err := store.GetPack(t.Context(), pack.ID); err != nil {
		t.Fatalf("pack was not published: %v", err)
	}
	if _, err := store.PendingPrepareIntent(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("prepare intent remains after completion: %v", err)
	}
}

func TestTrustCheckpointPersistsAcrossRestart(t *testing.T) {
	root, binding, trustRoot, store := createTestStore(t)
	checkpoint, err := NewTrustState(json.RawMessage(`{"manifest_digest":"def","manifest_revision":"3","payload":{"issuer":"edu-agent"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTrustState(t.Context(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(store.TrustState().Bytes(), checkpoint.Bytes()) {
		t.Fatal("in-memory trust checkpoint was not advanced")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPassphrase(t.Context(), root, binding, trustRoot, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !bytes.Equal(reopened.TrustState().Bytes(), checkpoint.Bytes()) {
		t.Fatalf("checkpoint after restart=%s", reopened.TrustState().Bytes())
	}
}

func TestLegacyRevisionOneProfileUpgradesWithoutChangingTrustRoot(t *testing.T) {
	root, binding, trustRoot, store := createTestStore(t)
	profileID := store.Binding().ProfileUUID()
	legacy := ProfileRecord{
		Format: Format, ProfileVersion: 1, ProfileID: profileID,
		TrustState: trustRoot.Bytes(),
	}
	if err := store.withLease(t.Context(), LeaseExclusive, func() error {
		return store.writeRecordLocked(ObjectProfile, profileID, legacy, true)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	legacyOpen, err := OpenPassphrase(t.Context(), root, binding, trustRoot, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacyOpen.TrustState().Bytes(), trustRoot.Bytes()) {
		t.Fatal("legacy revision-1 checkpoint changed while opening")
	}
	checkpoint, err := NewTrustState(json.RawMessage(`{"manifest_digest":"ghi","manifest_revision":"2","payload":{"issuer":"edu-agent"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyOpen.UpdateTrustState(t.Context(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := legacyOpen.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := OpenPassphrase(t.Context(), root, binding, trustRoot, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if !bytes.Equal(upgraded.TrustState().Bytes(), checkpoint.Bytes()) {
		t.Fatalf("upgraded checkpoint=%s", upgraded.TrustState().Bytes())
	}
	var profile ProfileRecord
	if err := upgraded.withLease(t.Context(), LeaseShared, func() error {
		return upgraded.readRecordLocked(ObjectProfile, profileID, &profile)
	}); err != nil {
		t.Fatal(err)
	}
	if profile.ProfileVersion != 2 || !bytes.Equal(profile.TrustRoot, trustRoot.Bytes()) {
		t.Fatalf("profile version=%d trust_root=%s", profile.ProfileVersion, profile.TrustRoot)
	}
}
