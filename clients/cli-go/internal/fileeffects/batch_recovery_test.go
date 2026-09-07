package fileeffects

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestFileBatchUncertainPublicationKeepsConfirmedPrefix(t *testing.T) {
	for _, test := range []struct {
		name             string
		failMetadata     bool
		publishFailure   bool
		restoreCompleted int
		writeAttempts    int
	}{
		{"metadata-not-published", true, false, 1, 2},
		{"metadata-published-but-error", true, true, 2, 2},
		{"event-published-but-error", false, true, 1, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			const owner, call = "failure-owner", "uncertain-call"
			store := newFileBatchTestStore()
			m := NewBatchManager(BatchOptions{})
			fileBatchTestOK(t, m.Bind(ctx, owner, store))
			plan := fileBatchTestPlan(2)
			id := fileBatchTestBegin(t, m, owner, call, plan)
			fileBatchTestOK(t, m.Pending(ctx, owner, id, 0))
			fileBatchTestOK(t, m.Settle(ctx, owner, id, 0, fileBatchTestActual(plan.Items[0])))
			fileBatchTestOK(t, m.Pending(ctx, owner, id, 1))
			oldBody, oldInfo := fileBatchTestReadAll(t, m, owner, id)
			oldMeta := bytes.Clone(store.blobs[batchMetaName(id)])
			oldTail := bytes.Clone(store.blobs[batchSegmentName(id, false, 0)])
			beforeWrites := store.ioCounts()[1]
			store.failName = batchSegmentName(id, false, 0)
			if test.failMetadata {
				store.failName = batchMetaName(id)
			}
			store.failOrdinal = store.writes[store.failName] + 1
			store.publishFail = test.publishFailure
			actual := fileBatchTestActual(plan.Items[1])
			fileBatchTestCode(t, m.Settle(ctx, owner, id, 1, actual), "store_failed")
			if store.ioCounts()[1]-beforeWrites != test.writeAttempts || store.writes[store.failName] != store.failOrdinal {
				t.Fatalf("uncertain publication was retried: writes=%v", store.writes)
			}
			status := fileBatchTestStatus(t, m, owner, id)
			suffix := fileBatchTestLine(t, batchActualLine{Version: 1, Type: "actual", Sequence: 4, Index: 1, Actual: actual})
			if status.Items != 3 || status.Started != 2 || status.Completed != 2 || status.Unchanged != 0 || status.Unknown != 0 || status.Finished || status.Restored || status.SavedBytes != oldInfo.Bytes || status.Bytes != oldInfo.Bytes+int64(len(suffix)) || status.PersistenceError != "file_batch_store_failed" {
				t.Fatalf("failed settlement lost actual state or promoted SavedBytes: %+v", status)
			}
			storedTail := store.blobs[batchSegmentName(id, false, 0)]
			if !bytes.Equal(storedTail, append(bytes.Clone(oldTail), suffix...)) {
				t.Fatal("injected failure did not leave exactly the old event prefix plus attempted actual")
			}
			if test.restoreCompleted == 1 && !bytes.Equal(store.blobs[batchMetaName(id)], oldMeta) {
				t.Fatal("failed/unattempted metadata write changed the acknowledged prefix")
			}
			liveBody, liveInfo := fileBatchTestReadAll(t, m, owner, id)
			if !bytes.Equal(liveBody, append(bytes.Clone(oldBody), suffix...)) || liveInfo.Saved {
				t.Fatal("live reader lost actual suffix or claims its persistence was confirmed")
			}
			fileBatchTestSearchAll(t, m, owner, id, actual.ContentHash, liveBody, 1)
			writesAfterFailure := store.ioCounts()[1]
			fileBatchTestCode(t, m.Pending(ctx, owner, id, 2), "writer_stopped")
			fileBatchTestCode(t, m.Settle(ctx, owner, id, 1, actual), "writer_stopped")
			fileBatchTestCode(t, m.Finish(ctx, owner, id, false, "stopped"), "writer_stopped")
			_, err := m.Begin(ctx, owner, call, plan)
			fileBatchTestCode(t, err, "duplicate_call")
			if store.ioCounts()[1] != writesAfterFailure {
				t.Fatal("stopped writer or duplicate Begin retried storage")
			}

			// An unrelated, malformed orphan is not a discovery source. A suffix
			// in the committed tail is fetched but must not be parsed as evidence.
			orphanName := batchSegmentName(id, false, 99)
			store.blobs[orphanName] = []byte("not authenticated JSONL")
			restored := NewBatchManager(BatchOptions{})
			fileBatchTestOK(t, restored.Bind(ctx, owner, store))
			recovered := fileBatchTestStatus(t, restored, owner, id)
			wantBody := oldBody
			if test.restoreCompleted == 2 {
				wantBody = liveBody
			}
			if !recovered.Restored || recovered.Finished || recovered.Items != 3 || recovered.Started != 2 || recovered.Items-recovered.Started != 1 || recovered.Completed != test.restoreCompleted || recovered.Unchanged != 0 || recovered.Unknown != 2-test.restoreCompleted || recovered.PersistenceError != "" || recovered.Bytes != int64(len(wantBody)) || recovered.SavedBytes != int64(len(wantBody)) {
				t.Fatalf("recovery did not use actual metadata watermark: %+v", recovered)
			}
			recoveredBody, recoveredInfo := fileBatchTestReadAll(t, restored, owner, id)
			if !bytes.Equal(recoveredBody, wantBody) || !recoveredInfo.Saved || store.reads[orphanName] != 0 {
				t.Fatal("recovery exposed unacknowledged actual or read orphan event")
			}
			fileBatchTestCode(t, restored.Pending(ctx, owner, id, 2), "read_only")
			fileBatchTestCode(t, restored.Settle(ctx, owner, id, 1, actual), "read_only")
			fileBatchTestCode(t, restored.Finish(ctx, owner, id, false, "stopped"), "read_only")
			_, err = restored.Begin(ctx, owner, call, plan)
			fileBatchTestCode(t, err, "duplicate_call")
			if store.ioCounts()[1] != writesAfterFailure || !bytes.Equal(store.blobs[orphanName], []byte("not authenticated JSONL")) || fileBatchTestStatus(t, restored, owner, id) != recovered {
				t.Fatal("read-only recovery rewrote, cleaned up, or advanced evidence")
			}
		})
	}
}

func TestFileBatchIdentityOnlySurvivesBind(t *testing.T) {
	for _, publishError := range []bool{false, true} {
		t.Run(fmt.Sprintf("publication-error-%v", publishError), func(t *testing.T) {
			ctx := context.Background()
			const owner, call = "identity-owner", "identity-only-call"
			store := newFileBatchTestStore()
			m := NewBatchManager(BatchOptions{})
			fileBatchTestOK(t, m.Bind(ctx, owner, store))
			marker := batchMakeMarker(owner, call)
			if publishError {
				store.failName, store.failOrdinal, store.publishFail = batchMarkerName(marker.CallHash), 1, true
			}
			err := m.RecordCall(ctx, owner, call)
			if publishError {
				fileBatchTestCode(t, err, "store_failed")
			} else {
				fileBatchTestOK(t, err)
			}
			if m.HasCall(owner, call) == publishError || store.ioCounts()[1] != 1 || len(store.blobs) != 1 {
				t.Fatal("identity confirmation promoted an uncertain write or retried it")
			}
			_, err = m.Begin(ctx, owner, call, fileBatchTestPlan(1))
			fileBatchTestCode(t, err, "duplicate_call")
			m.DropOwner(owner)
			if store.ioCounts()[1] != 1 || len(store.blobs) != 1 {
				t.Fatal("dropping owner removed or rewrote durable identity")
			}
			restored := NewBatchManager(BatchOptions{})
			fileBatchTestOK(t, restored.Bind(ctx, owner, store))
			if !restored.HasCall(owner, call) {
				t.Fatal("authenticated marker-only call lost on Bind")
			}
			fileBatchTestOK(t, restored.RecordCall(ctx, owner, call))
			items, err := restored.List(ctx, owner)
			fileBatchTestOK(t, err)
			if len(items) != 0 {
				t.Fatal("marker-only call fabricated a batch record")
			}
			_, err = restored.Status(owner, marker.ID)
			fileBatchTestCode(t, err, "not_found")
			fileBatchTestCode(t, restored.Pending(ctx, owner, marker.ID, 0), "not_found")
			fileBatchTestCode(t, restored.Settle(ctx, owner, marker.ID, 0, BatchActual{Outcome: "completed"}), "not_found")
			fileBatchTestCode(t, restored.Finish(ctx, owner, marker.ID, false, ""), "not_found")
			_, err = restored.Begin(ctx, owner, call, fileBatchTestPlan(2))
			fileBatchTestCode(t, err, "duplicate_call")
			if store.ioCounts()[1] != 1 {
				t.Fatal("restored identity was rewritten or acquired an executable writer")
			}
		})
	}
}

func TestFileBatchBindRejectsBadEvidenceTransactionally(t *testing.T) {
	ctx := context.Background()
	const owner = "restore-owner"
	store := newFileBatchTestStore()
	m := NewBatchManager(BatchOptions{})
	fileBatchTestOK(t, m.Bind(ctx, owner, store))
	plan := fileBatchTestPlan(1)
	id := fileBatchTestBegin(t, m, owner, "restore-call", plan)
	for index, item := range plan.Items {
		fileBatchTestOK(t, m.Pending(ctx, owner, id, index))
		fileBatchTestOK(t, m.Settle(ctx, owner, id, index, fileBatchTestActual(item)))
	}
	fileBatchTestOK(t, m.Finish(ctx, owner, id, true, ""))
	fileBatchTestOK(t, m.Bind(ctx, owner, store))
	body, info := fileBatchTestReadAll(t, m, owner, id)
	status := fileBatchTestStatus(t, m, owner, id)
	memory, count := m.memory, m.count
	for _, test := range []struct {
		name   string
		code   string
		mutate func(*testing.T, *fileBatchTestStore)
	}{
		{"missing-plan-segment", "unavailable", func(_ *testing.T, s *fileBatchTestStore) {
			delete(s.blobs, batchSegmentName(id, true, 0))
		}},
		{"tampered-event-segment", "corrupt", func(_ *testing.T, s *fileBatchTestStore) {
			s.blobs[batchSegmentName(id, false, 0)][0] ^= 1
		}},
		{"future-metadata", "version_unsupported", func(t *testing.T, s *fileBatchTestStore) {
			var meta batchMetadata
			fileBatchTestOK(t, json.Unmarshal(s.blobs[batchMetaName(id)], &meta))
			meta.Version = 3 // Data v2 is reserved for purge; v3 remains unsupported.
			s.blobs[batchMetaName(id)] = fileBatchTestJSON(t, meta)
		}},
		{"authenticated-illegal-actual-order", "corrupt", func(t *testing.T, s *fileBatchTestStore) {
			name := batchSegmentName(id, false, 0)
			lines := bytes.Split(bytes.TrimSuffix(s.blobs[name], []byte{'\n'}), []byte{'\n'})
			var actual batchActualLine
			fileBatchTestOK(t, json.Unmarshal(lines[1], &actual))
			if actual.Type != "actual" || actual.Index != 0 {
				t.Fatal("illegal-order fixture did not select root settlement")
			}
			actual.Index = 1 // A file result cannot settle the still-pending root.
			lines[1] = fileBatchTestJSON(t, actual)
			s.blobs[name] = append(bytes.Join(lines, []byte{'\n'}), '\n')
			var meta batchMetadata
			fileBatchTestOK(t, json.Unmarshal(s.blobs[batchMetaName(id)], &meta))
			meta.Events[0] = batchSegment{Bytes: int64(len(s.blobs[name])), Hash: fileBatchTestHash(s.blobs[name])}
			var raw []byte
			for index := range meta.Plan {
				raw = append(raw, s.blobs[batchSegmentName(id, true, index)]...)
			}
			raw = append(raw, s.blobs[name]...)
			meta.Bytes, meta.Hash = int64(len(raw)), fileBatchTestHash(raw)
			s.blobs[batchMetaName(id)] = fileBatchTestJSON(t, meta)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			badStore := store.clone()
			test.mutate(t, badStore)
			fileBatchTestCode(t, m.Bind(ctx, owner, badStore), test.code)
			got, gotInfo := fileBatchTestReadAll(t, m, owner, id)
			items, err := m.List(ctx, owner)
			fileBatchTestOK(t, err)
			if !bytes.Equal(got, body) || gotInfo != info || len(items) != 1 || items[0] != info || fileBatchTestStatus(t, m, owner, id) != status || !m.HasCall(owner, "restore-call") || m.memory != memory || m.count != count || badStore.ioCounts()[1] != 0 {
				t.Fatal("failed Bind changed previous backend/catalog, leaked budget, or wrote storage")
			}
		})
	}
}

func TestFileBatchRevocationFencesEOFSearchAndList(t *testing.T) {
	ctx := context.Background()
	const owner = "fenced-owner"
	store := newFileBatchTestStore()
	m := NewBatchManager(BatchOptions{})
	fileBatchTestOK(t, m.Bind(ctx, owner, store))
	plan := fileBatchTestPlan(1)
	id := fileBatchTestBegin(t, m, owner, "fenced-call", plan)
	fileBatchTestOK(t, m.Pending(ctx, owner, id, 0))
	store.failName, store.failOrdinal = batchMetaName(id), store.writes[batchMetaName(id)]+1
	fileBatchTestCode(t, m.Settle(ctx, owner, id, 0, fileBatchTestActual(plan.Items[0])), "store_failed")
	body, info := fileBatchTestReadAll(t, m, owner, id) // Warm both saved-prefix and local-suffix reads.
	fileBatchTestSearchAll(t, m, owner, id, `"type":"actual"`, body, 1)
	items, err := m.List(ctx, owner)
	fileBatchTestOK(t, err)
	if len(items) != 1 || items[0] != info {
		t.Fatal("fixture has no readable failed-writer catalog")
	}
	store.revoked = true
	for _, offset := range []int64{0, info.Bytes} {
		reads := store.reads[batchMetaName(id)]
		page, err := m.Read(ctx, owner, id, offset, 1)
		fileBatchTestCode(t, err, "unavailable")
		if len(page.Data) != 0 || page.Info.ID != "" || store.reads[batchMetaName(id)] != reads+1 {
			t.Fatalf("Read(%d) skipped authentication or returned cached bytes/metadata", offset)
		}
	}
	reads := store.reads[batchMetaName(id)]
	search, err := m.Search(ctx, owner, id, `"type":"actual"`, 0, 1)
	fileBatchTestCode(t, err, "unavailable")
	if len(search.Offsets) != 0 || search.Info.ID != "" || store.reads[batchMetaName(id)] != reads+1 {
		t.Fatal("Search skipped authentication or exposed cached/local-suffix evidence")
	}
	lists := store.lists
	items, err = m.List(ctx, owner)
	fileBatchTestCode(t, err, "unavailable")
	if len(items) != 0 || store.lists != lists+1 {
		t.Fatal("List skipped authentication or exposed a revoked catalog")
	}
}
