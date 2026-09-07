package fileeffects

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func fileBatchPurgeItem(path, kind string, size int64) BatchItem {
	return BatchItem{Source: Endpoint{Path: path, Kind: kind, Version: "entry-v1:" + strings.Repeat("a", 64)}, Target: Endpoint{Path: path, Kind: "absent"}, Bytes: size}
}

func fileBatchPurgeSingle(kind string) BatchPlan {
	path := ArchiveDirectory + "/selected"
	item := fileBatchPurgeItem(path, kind, 0)
	if kind == "file" {
		item.Bytes = 1 << 40 // Logical size is not constrained by copy's byte budget.
	}
	root := New("purge_archive", path, path, kind)
	root.Source = item.Source
	return BatchPlan{Root: root, Items: []BatchItem{item}}
}

func fileBatchPurgePlan(files int) BatchPlan {
	plan := fileBatchPurgeSingle("directory")
	root := plan.Root.Source.Path
	for i := 0; i < files; i++ {
		plan.Items = append(plan.Items, fileBatchPurgeItem(fmt.Sprintf("%s/file-%04d.bin", root, i), "file", int64(i%7+1)))
	}
	plan.Items = append(plan.Items,
		fileBatchPurgeItem(root+"/nested/data.bin", "file", 1<<40),
		fileBatchPurgeItem(root+"/nested", "directory", 0),
		fileBatchPurgeItem(root+"/link", "link", 0),
		fileBatchPurgeItem(root+"/empty", "directory", 0))
	sort.Slice(plan.Items, func(i, j int) bool { return plan.Items[i].Source.Path > plan.Items[j].Source.Path })
	return plan
}

func fileBatchPurgeFinish(t *testing.T, m *BatchManager, owner, id string, plan BatchPlan) []byte {
	t.Helper()
	ctx := context.Background()
	var body bytes.Buffer
	body.Write(fileBatchTestLine(t, batchRootLine{Version: 2, Type: "root", Root: plan.Root}))
	for i, item := range plan.Items {
		body.Write(fileBatchTestLine(t, batchPlanLine{Version: 2, Type: "plan", Index: i, Item: item}))
	}
	for i, item := range plan.Items {
		fileBatchTestOK(t, m.Pending(ctx, owner, id, i))
		actual := BatchActual{Outcome: "completed", Bytes: item.Bytes}
		fileBatchTestOK(t, m.Settle(ctx, owner, id, i, actual))
		body.Write(fileBatchTestLine(t, batchPendingLine{Version: 2, Type: "pending", Sequence: 2*i + 1, Index: i}))
		body.Write(fileBatchTestLine(t, batchActualLine{Version: 2, Type: "actual", Sequence: 2*i + 2, Index: i, Actual: actual}))
	}
	fileBatchTestOK(t, m.Finish(ctx, owner, id, true, ""))
	body.Write(fileBatchTestLine(t, batchEndLine{Version: 2, Type: "end", Sequence: 2*len(plan.Items) + 1, Complete: true}))
	return body.Bytes()
}

func TestFileBatchPurgeLargeMixedVersions(t *testing.T) {
	ctx := context.Background()
	const owner, call = "purge-owner", "private-purge-call"
	store := newFileBatchTestStore()
	m := NewBatchManager(BatchOptions{})
	fileBatchTestOK(t, m.Bind(ctx, owner, store))
	copyPlan := fileBatchTestPlan(1)
	copyID := fileBatchTestBegin(t, m, owner, "old-copy", copyPlan)
	for i, item := range copyPlan.Items {
		fileBatchTestOK(t, m.Pending(ctx, owner, copyID, i))
		fileBatchTestOK(t, m.Settle(ctx, owner, copyID, i, fileBatchTestActual(item)))
	}
	fileBatchTestOK(t, m.Finish(ctx, owner, copyID, true, ""))
	copyBody, copyInfo := fileBatchTestReadAll(t, m, owner, copyID)
	copyBlobs := store.clone()
	plan := fileBatchPurgePlan(2501)
	id := fileBatchTestBegin(t, m, owner, call, plan)
	expected := fileBatchPurgeFinish(t, m, owner, id, plan)
	body, info := fileBatchTestReadAll(t, m, owner, id)
	if !bytes.Equal(body, expected) || !info.Saved || len(body) <= 1<<20 {
		t.Fatalf("v2 JSONL differs or did not span scan window: bytes=%d, saved=%v", len(body), info.Saved)
	}
	status := fileBatchTestStatus(t, m, owner, id)
	if status.Items != len(plan.Items) || status.Started != status.Items || status.Completed != status.Items || status.Unknown != 0 || status.Unchanged != 0 || !status.Finished || status.Restored || status.PersistenceError != "" || status.SavedBytes != status.Bytes {
		t.Fatalf("incorrect purge status: %+v", status)
	}
	var meta batchMetadata
	fileBatchTestOK(t, json.Unmarshal(store.blobs[batchMetaName(id)], &meta))
	if meta.Version != 2 || len(meta.Plan) < 2 || len(meta.Events) < 2 {
		t.Fatalf("v2 fixture did not cross plan/event segments: %+v", meta)
	}
	marker := batchMakeMarker(owner, call)
	if !bytes.Equal(store.blobs[batchMarkerName(marker.CallHash)], fileBatchTestJSON(t, marker)) {
		t.Fatal("purge changed the v1 identity contract")
	}
	for name, data := range store.blobs {
		if bytes.Contains(data, []byte(call)) || strings.Contains(name, call) {
			t.Fatal("purge leaked raw call identity")
		}
	}
	for name, data := range copyBlobs.blobs {
		if !bytes.Equal(data, store.blobs[name]) {
			t.Fatalf("purge rewrote v1 evidence %s", name)
		}
	}
	fileBatchTestSearchAll(t, m, owner, id, `"type":"actual"`, body, 97)
	fileBatchTestSearchAll(t, m, owner, id, "no-such-purge-literal", body, 3)
	boundary := int(meta.Plan[0].Bytes)
	fileBatchTestSearchAll(t, m, owner, id, string(body[boundary-12:boundary+24]), body, 2)
	restored := NewBatchManager(BatchOptions{})
	writes := store.ioCounts()[1]
	fileBatchTestOK(t, restored.Bind(ctx, owner, store))
	got, gotInfo := fileBatchTestReadAll(t, restored, owner, id)
	old, oldInfo := fileBatchTestReadAll(t, restored, owner, copyID)
	items, err := restored.List(ctx, owner)
	fileBatchTestOK(t, err)
	if !bytes.Equal(got, body) || gotInfo != info || !bytes.Equal(old, copyBody) || oldInfo != copyInfo || len(items) != 2 || !fileBatchTestStatus(t, restored, owner, id).Restored || store.ioCounts()[1] != writes {
		t.Fatal("mixed-version Bind changed bytes, catalog, watermarks or backend")
	}
	fileBatchTestCode(t, restored.Pending(ctx, owner, id, 0), "read_only")
	fileBatchTestCode(t, restored.Settle(ctx, owner, id, 0, BatchActual{Outcome: "completed"}), "read_only")
	fileBatchTestCode(t, restored.Finish(ctx, owner, id, true, ""), "read_only")
}

func TestFileBatchPurgeActualAndSingleRoots(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"file", "link", "directory"} {
		for _, outcome := range []string{"completed", "unchanged", "unknown"} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				store := newFileBatchTestStore()
				m := NewBatchManager(BatchOptions{})
				fileBatchTestOK(t, m.Bind(ctx, "owner", store))
				plan := fileBatchPurgeSingle(kind)
				id := fileBatchTestBegin(t, m, "owner", "call", plan)
				fileBatchTestOK(t, m.Pending(ctx, "owner", id, 0))
				before := fileBatchTestStatus(t, m, "owner", id)
				io := store.ioCounts()
				actual := BatchActual{Outcome: outcome}
				if outcome == "completed" {
					actual.Bytes = plan.Items[0].Bytes
				}
				badHash, badSize, badCode, badOutcome := actual, actual, actual, actual
				badHash.ContentHash = fileBatchTestHash([]byte("not read"))
				badSize.Bytes++
				badCode.Code = "not-a-code"
				badOutcome.Outcome = "not_started"
				for _, invalid := range []BatchActual{badHash, badSize, badCode, badOutcome, {Outcome: outcome, Bytes: -1}} {
					fileBatchTestCode(t, m.Settle(ctx, "owner", id, 0, invalid), "invalid_actual")
					if fileBatchTestStatus(t, m, "owner", id) != before || store.ioCounts() != io {
						t.Fatal("invalid actual consumed pending or touched backend")
					}
				}
				fileBatchTestOK(t, m.Settle(ctx, "owner", id, 0, actual))
				fileBatchTestOK(t, m.Finish(ctx, "owner", id, outcome == "completed", ""))
				restored := NewBatchManager(BatchOptions{})
				fileBatchTestOK(t, restored.Bind(ctx, "owner", store))
				status := fileBatchTestStatus(t, restored, "owner", id)
				if status.Started != 1 || !status.Finished || (status.Completed == 1) != (outcome == "completed") || (status.Unchanged == 1) != (outcome == "unchanged") || (status.Unknown == 1) != (outcome == "unknown") {
					t.Fatalf("incorrect single-root settlement: %+v", status)
				}
				fileBatchTestReadAll(t, restored, "owner", id)
			})
		}
	}
}
