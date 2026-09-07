package fileeffects

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

func TestFileBatchPurgeFailedSettlementPrefixAndPrivacy(t *testing.T) {
	for _, test := range []struct {
		name                        string
		metadata, publish           bool
		restoredCompleted, attempts int
	}{
		{"metadata-not-published", true, false, 0, 2},
		{"metadata-published-error", true, true, 1, 2},
		{"event-published-error", false, true, 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			const owner, call = "owner", "failed-purge"
			store := newFileBatchTestStore()
			m := NewBatchManager(BatchOptions{})
			fileBatchTestOK(t, m.Bind(ctx, owner, store))
			plan := fileBatchPurgePlan(1)
			id := fileBatchTestBegin(t, m, owner, call, plan)
			fileBatchTestOK(t, m.Pending(ctx, owner, id, 0))
			prefix, prefixInfo := fileBatchTestReadAll(t, m, owner, id)
			store.failName = batchSegmentName(id, false, 0)
			if test.metadata {
				store.failName = batchMetaName(id)
			}
			store.failOrdinal, store.publishFail = store.writes[store.failName]+1, test.publish
			writes := store.ioCounts()[1]
			actual := BatchActual{Outcome: "completed", Bytes: plan.Items[0].Bytes}
			fileBatchTestCode(t, m.Settle(ctx, owner, id, 0, actual), "store_failed")
			if store.ioCounts()[1]-writes != test.attempts || store.writes[store.failName] != store.failOrdinal {
				t.Fatal("uncertain purge settlement was retried")
			}
			suffix := fileBatchTestLine(t, batchActualLine{Version: 2, Type: "actual", Sequence: 2, Index: 0, Actual: actual})
			body, info := fileBatchTestReadAll(t, m, owner, id)
			status := fileBatchTestStatus(t, m, owner, id)
			if !bytes.Equal(body, append(bytes.Clone(prefix), suffix...)) || info.Saved || status.Completed != 1 || status.Unknown != 0 || status.Started != 1 || status.Bytes != int64(len(body)) || status.SavedBytes != prefixInfo.Bytes || status.PersistenceError != "file_batch_store_failed" {
				t.Fatalf("lost real purge suffix or promoted unconfirmed prefix: %+v", status)
			}
			fileBatchTestSearchAll(t, m, owner, id, `"outcome":"completed"`, body, 1)
			writes = store.ioCounts()[1]
			fileBatchTestCode(t, m.Pending(ctx, owner, id, 1), "writer_stopped")
			fileBatchTestCode(t, m.Settle(ctx, owner, id, 0, actual), "writer_stopped")
			fileBatchTestCode(t, m.Finish(ctx, owner, id, false, "stopped"), "writer_stopped")
			_, err := m.Begin(ctx, owner, call, plan)
			fileBatchTestCode(t, err, "duplicate_call")
			orphan := batchSegmentName(id, false, 99)
			store.blobs[orphan] = []byte("uncommitted purge body")
			restored := NewBatchManager(BatchOptions{})
			fileBatchTestOK(t, restored.Bind(ctx, owner, store))
			got, gotInfo := fileBatchTestReadAll(t, restored, owner, id)
			want := prefix
			if test.restoredCompleted == 1 {
				want = body
			}
			recovered := fileBatchTestStatus(t, restored, owner, id)
			if !bytes.Equal(got, want) || !gotInfo.Saved || !recovered.Restored || recovered.Completed != test.restoredCompleted || recovered.Unknown != 1-test.restoredCompleted || recovered.Items-recovered.Started != len(plan.Items)-1 || recovered.Bytes != int64(len(want)) || recovered.SavedBytes != recovered.Bytes || recovered.Finished || store.reads[orphan] != 0 {
				t.Fatalf("restoration promoted orphan or lost pending uncertainty: %+v", recovered)
			}
			fileBatchTestCode(t, restored.Pending(ctx, owner, id, 1), "read_only")
			fileBatchTestCode(t, restored.Settle(ctx, owner, id, 0, actual), "read_only")
			fileBatchTestCode(t, restored.Finish(ctx, owner, id, false, "stopped"), "read_only")
			differentPlan := fileBatchPurgeSingle("link")
			_, err = restored.Begin(ctx, owner, call, differentPlan)
			fileBatchTestCode(t, err, "duplicate_call")
			if store.ioCounts()[1] != writes || !restored.HasCall(owner, call) {
				t.Fatal("old call was replayed or recovery rewrote evidence")
			}
			newID := fileBatchTestBegin(t, restored, owner, "new-purge", differentPlan)
			if newID == id {
				t.Fatal("new call reused old batch identity")
			}
			fileBatchPurgeFinish(t, restored, owner, newID, differentPlan)

			// Model a revoked/cleared authenticated store after warming both the
			// saved prefix and the locally retained, real but unconfirmed suffix.
			store.revoked = true
			for _, reader := range []*BatchManager{m, restored} {
				current := fileBatchTestStatus(t, reader, owner, id)
				for _, offset := range []int64{0, current.SavedBytes, current.Bytes} {
					reads := store.reads[batchMetaName(id)]
					page, err := reader.Read(ctx, owner, id, offset, 1)
					fileBatchTestCode(t, err, "unavailable")
					if len(page.Data) != 0 || page.Info.ID != "" || store.reads[batchMetaName(id)] != reads+1 {
						t.Fatal("revoked read/EOF exposed cached data")
					}
					search, err := reader.Search(ctx, owner, id, `"type":"actual"`, offset, 1)
					fileBatchTestCode(t, err, "unavailable")
					if len(search.Offsets) != 0 || search.Info.ID != "" {
						t.Fatal("revoked search exposed data")
					}
				}
				items, err := reader.List(ctx, owner)
				fileBatchTestCode(t, err, "unavailable")
				if len(items) != 0 {
					t.Fatal("revoked List exposed catalog")
				}
			}
			io := store.ioCounts()
			fileBatchTestOK(t, restored.Bind(ctx, owner, nil))
			items, err := restored.List(ctx, owner)
			fileBatchTestOK(t, err)
			if len(items) != 0 || restored.HasCall(owner, call) || store.ioCounts() != io {
				t.Fatal("detach retained revoked persisted history or accessed backend")
			}
		})
	}
}

func TestFileBatchPurgeInitialMetadataFailureDoesNotReopenWriter(t *testing.T) {
	ctx := context.Background()
	for _, publish := range []bool{false, true} {
		t.Run(fmt.Sprint(publish), func(t *testing.T) {
			store := newFileBatchTestStore()
			m := NewBatchManager(BatchOptions{})
			fileBatchTestOK(t, m.Bind(ctx, "owner", store))
			marker := batchMakeMarker("owner", "call")
			store.failName, store.failOrdinal, store.publishFail = batchMetaName(marker.ID), 1, publish
			plan := fileBatchPurgeSingle("directory")
			id, err := m.Begin(ctx, "owner", "call", plan)
			fileBatchTestCode(t, err, "store_failed")
			if id != marker.ID || store.writes[store.failName] != 1 {
				t.Fatal("initial metadata failure retried or lost identity")
			}
			status := fileBatchTestStatus(t, m, "owner", id)
			if status.Started != 0 || status.SavedBytes != 0 || status.PersistenceError != "file_batch_store_failed" {
				t.Fatalf("incorrect failed begin status: %+v", status)
			}
			writes := store.ioCounts()[1]
			fileBatchTestCode(t, m.Pending(ctx, "owner", id, 0), "writer_stopped")
			_, err = m.Begin(ctx, "owner", "call", plan)
			fileBatchTestCode(t, err, "duplicate_call")
			restored := NewBatchManager(BatchOptions{})
			fileBatchTestOK(t, restored.Bind(ctx, "owner", store))
			_, err = restored.Begin(ctx, "owner", "call", plan)
			fileBatchTestCode(t, err, "duplicate_call")
			if publish {
				fileBatchTestCode(t, restored.Pending(ctx, "owner", id, 0), "read_only")
			} else {
				_, err = restored.Status("owner", id)
				fileBatchTestCode(t, err, "not_found")
			}
			if store.ioCounts()[1] != writes || !restored.HasCall("owner", "call") {
				t.Fatal("failed Begin was replayed or identity discarded")
			}
		})
	}
}

func TestFileBatchPurgeNoSaveAndBudgets(t *testing.T) {
	ctx := context.Background()
	t.Run("no-save", func(t *testing.T) {
		store := newFileBatchTestStore()
		m := NewBatchManager(BatchOptions{})
		fileBatchTestOK(t, m.Bind(ctx, "owner", store))
		fileBatchTestOK(t, m.Bind(ctx, "owner", nil))
		io := store.ioCounts()
		for i, plan := range []BatchPlan{fileBatchPurgeSingle("file"), fileBatchPurgeSingle("link"), fileBatchPurgeSingle("directory"), fileBatchPurgePlan(2)} {
			id := fileBatchTestBegin(t, m, "owner", fmt.Sprintf("call-%d", i), plan)
			expected := fileBatchPurgeFinish(t, m, "owner", id, plan)
			body, info := fileBatchTestReadAll(t, m, "owner", id)
			fileBatchTestSearchAll(t, m, "owner", id, `"type":"actual"`, body, 2)
			if !bytes.Equal(body, expected) || info.Saved || fileBatchTestStatus(t, m, "owner", id).SavedBytes != 0 {
				t.Fatal("no-save purge lost body or fabricated durability")
			}
		}
		items, err := m.List(ctx, "owner")
		fileBatchTestOK(t, err)
		if len(items) != 4 || store.ioCounts() != io {
			t.Fatal("no-save purge touched backend or lost records")
		}
		m.DropOwner("owner")
		if m.memory != 0 || m.count != 0 || len(m.owners) != 0 || store.ioCounts() != io {
			t.Fatal("no-save DropOwner leaked resources or touched backend")
		}
	})
	for _, test := range []struct {
		name    string
		options BatchOptions
	}{
		{"entry-limit", BatchOptions{Entries: 1}},
		{"plan-limit", BatchOptions{PlanBytes: 256}},
		{"memory-limit", BatchOptions{MemoryBytes: 1 << 20}},
		{"record-limit", BatchOptions{MaxRecords: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newFileBatchTestStore()
			m := NewBatchManager(test.options)
			fileBatchTestOK(t, m.Bind(ctx, "owner", store))
			if test.options.MaxRecords == 1 {
				fileBatchTestOK(t, m.RecordCall(ctx, "owner", "retained"))
			}
			io, memory, count := store.ioCounts(), m.memory, m.count
			for attempt := 0; attempt < 2; attempt++ {
				call := fmt.Sprintf("rejected-%d", attempt)
				_, err := m.Begin(ctx, "owner", call, fileBatchPurgePlan(2))
				fileBatchTestCode(t, err, "limit")
				if m.HasCall("owner", call) || store.ioCounts() != io || m.memory != memory || m.count != count {
					t.Fatal("budget rejection consumed identity, memory or backend I/O")
				}
			}
		})
	}
}
