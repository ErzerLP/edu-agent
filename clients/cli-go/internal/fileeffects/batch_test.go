package fileeffects

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
)

// This store models authenticated artifact access and unknown publication, not
// encryption or filesystem execution. Tests only journal synthetic facts.
type fileBatchTestStore struct {
	blobs       map[string][]byte
	reads       map[string]int
	writes      map[string]int
	lists       int
	revoked     bool
	failName    string
	failOrdinal int
	publishFail bool
}

var _ localartifact.Store = (*fileBatchTestStore)(nil)

func newFileBatchTestStore() *fileBatchTestStore {
	return &fileBatchTestStore{blobs: make(map[string][]byte), reads: make(map[string]int), writes: make(map[string]int)}
}

func (s *fileBatchTestStore) ReadArtifact(ctx context.Context, name string) ([]byte, error) {
	s.reads[name]++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.revoked {
		return nil, errors.New("private backend revocation detail")
	}
	data, ok := s.blobs[name]
	if !ok {
		return nil, errors.New("missing artifact")
	}
	return bytes.Clone(data), nil
}

func (s *fileBatchTestStore) WriteArtifact(ctx context.Context, name string, data []byte) error {
	s.writes[name]++
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.revoked {
		return errors.New("private backend revocation detail")
	}
	fail := name == s.failName && s.writes[name] == s.failOrdinal
	if !fail || s.publishFail {
		s.blobs[name] = bytes.Clone(data)
	}
	if fail {
		return errors.New("private backend publication detail")
	}
	return nil
}

func (s *fileBatchTestStore) ListArtifacts(ctx context.Context, prefix string) ([]string, error) {
	s.lists++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.revoked {
		return nil, errors.New("private backend revocation detail")
	}
	var names []string
	for name := range s.blobs {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s *fileBatchTestStore) clone() *fileBatchTestStore {
	other := newFileBatchTestStore()
	for name, data := range s.blobs {
		other.blobs[name] = bytes.Clone(data)
	}
	return other
}

func (s *fileBatchTestStore) ioCounts() [3]int {
	counts := [3]int{0, 0, s.lists}
	for _, n := range s.reads {
		counts[0] += n
	}
	for _, n := range s.writes {
		counts[1] += n
	}
	return counts
}

func fileBatchTestHash(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

func fileBatchTestPlan(files int) BatchPlan {
	root := New("copy", "src", "dst", "directory")
	root.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
	plan := BatchPlan{Root: root, Items: []BatchItem{{Source: root.Source, Target: root.Target}}}
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("file-%04d.bin", i)
		plan.Items = append(plan.Items, BatchItem{
			Source: Endpoint{Path: "src/" + name, Kind: "file", Version: root.Source.Version},
			Target: Endpoint{Path: "dst/" + name, Kind: "file"}, Bytes: int64(i%7 + 1),
		})
	}
	return plan
}

func fileBatchTestActual(item BatchItem) BatchActual {
	actual := BatchActual{Outcome: "completed", Bytes: item.Bytes}
	if item.Source.Kind == "file" {
		actual.ContentHash = fileBatchTestHash(bytes.Repeat([]byte{'x'}, int(item.Bytes)))
	}
	return actual
}

func fileBatchTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fileBatchTestLine(t *testing.T, value any) []byte {
	t.Helper()
	return append(fileBatchTestJSON(t, value), '\n')
}

func fileBatchTestOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func fileBatchTestCode(t *testing.T, err error, code string) {
	t.Helper()
	var batchErr *BatchError
	if !errors.As(err, &batchErr) || batchErr.Code != "file_batch_"+code || err.Error() != batchErr.Code {
		t.Fatalf("error = %v, want file_batch_%s", err, code)
	}
}

func fileBatchTestBegin(t *testing.T, m *BatchManager, owner, call string, plan BatchPlan) string {
	t.Helper()
	id, err := m.Begin(context.Background(), owner, call, plan)
	fileBatchTestOK(t, err)
	if len(id) != 34 || !strings.HasPrefix(id, "b_") || strings.Trim(id[2:], "0123456789abcdef") != "" {
		t.Fatalf("invalid opaque batch ID %q", id)
	}
	return id
}

func fileBatchTestStatus(t *testing.T, m *BatchManager, owner, id string) BatchStatus {
	t.Helper()
	status, err := m.Status(owner, id)
	fileBatchTestOK(t, err)
	return status
}

func fileBatchTestReadAll(t *testing.T, m *BatchManager, owner, id string) ([]byte, localartifact.Info) {
	t.Helper()
	var body []byte
	var info localartifact.Info
	for pageNumber := 0; pageNumber < 10000; pageNumber++ {
		offset := int64(len(body))
		page, err := m.Read(context.Background(), owner, id, offset, 32749)
		fileBatchTestOK(t, err)
		if page.Offset != offset || page.NextOffset != offset+int64(len(page.Data)) || len(page.Data) > 32749 || page.Info.ID != id || page.Info.Kind != "receipt" {
			t.Fatalf("invalid page at %d: %+v", offset, page.Info)
		}
		if pageNumber > 0 && page.Info != info {
			t.Fatal("metadata changed while reading a stable journal")
		}
		info = page.Info
		body = append(body, page.Data...)
		if !page.More {
			if info.Bytes != int64(len(body)) || info.Hash != fileBatchTestHash(body) {
				t.Fatalf("raw-byte size/hash mismatch: %+v, read %d bytes", info, len(body))
			}
			eof, err := m.Read(context.Background(), owner, id, info.Bytes, 1)
			fileBatchTestOK(t, err)
			if len(eof.Data) != 0 || eof.More || eof.Offset != info.Bytes || eof.NextOffset != info.Bytes || eof.Info != info {
				t.Fatalf("invalid EOF page: %+v", eof)
			}
			return body, info
		}
		if len(page.Data) == 0 {
			t.Fatal("non-progressing read page")
		}
	}
	t.Fatal("read exceeded bounded page count")
	return nil, info
}

func fileBatchTestSearchAll(t *testing.T, m *BatchManager, owner, id, needle string, body []byte, limit int) {
	t.Helper()
	var want, got []int64
	for start := 0; start+len(needle) <= len(body); start++ {
		if bytes.Equal(body[start:start+len(needle)], []byte(needle)) {
			want = append(want, int64(start))
		}
	}
	var offset int64
	for pageNumber := 0; pageNumber < len(want)+100; pageNumber++ {
		page, err := m.Search(context.Background(), owner, id, needle, offset, limit)
		fileBatchTestOK(t, err)
		if page.Offset != offset || len(page.Offsets) > limit || page.Info.Hash != fileBatchTestHash(body) || page.Scanned < 0 || page.Scanned > 1<<20 {
			t.Fatalf("invalid search page: %+v", page)
		}
		for _, hit := range page.Offsets {
			if hit < offset || hit+int64(len(needle)) > int64(len(body)) || !bytes.Equal(body[hit:hit+int64(len(needle))], []byte(needle)) {
				t.Fatalf("invalid search hit %d", hit)
			}
		}
		got = append(got, page.Offsets...)
		if !page.More {
			if !slices.Equal(got, want) {
				t.Fatalf("search %q: got %d offsets, want %d (offsets equal: false)", needle, len(got), len(want))
			}
			return
		}
		if page.NextOffset <= offset || page.NextOffset > int64(len(body)) {
			t.Fatalf("non-progressing search page: %+v", page)
		}
		offset = page.NextOffset
	}
	t.Fatal("search exceeded bounded page count")
}

func TestFileBatchLargePlanPagesAndSearch(t *testing.T) {
	ctx := context.Background()
	const owner, call = "journal-owner", "private-original-call-id-must-not-leak"
	store := newFileBatchTestStore()
	m := NewBatchManager(BatchOptions{})
	fileBatchTestOK(t, m.Bind(ctx, owner, store))
	plan := fileBatchTestPlan(2501)
	id := fileBatchTestBegin(t, m, owner, call, plan)
	var expected bytes.Buffer
	expected.Write(fileBatchTestLine(t, batchRootLine{Version: 1, Type: "root", Root: plan.Root}))
	for index, item := range plan.Items {
		expected.Write(fileBatchTestLine(t, batchPlanLine{Version: 1, Type: "plan", Index: index, Item: item}))
	}
	for index, item := range plan.Items {
		if err := m.Pending(ctx, owner, id, index); err != nil {
			t.Fatalf("Pending(%d): %v", index, err)
		}
		expected.Write(fileBatchTestLine(t, batchPendingLine{Version: 1, Type: "pending", Sequence: 2*index + 1, Index: index}))
		actual := fileBatchTestActual(item)
		if err := m.Settle(ctx, owner, id, index, actual); err != nil {
			t.Fatalf("Settle(%d): %v", index, err)
		}
		expected.Write(fileBatchTestLine(t, batchActualLine{Version: 1, Type: "actual", Sequence: 2*index + 2, Index: index, Actual: actual}))
	}
	fileBatchTestOK(t, m.Finish(ctx, owner, id, true, ""))
	expected.Write(fileBatchTestLine(t, batchEndLine{Version: 1, Type: "end", Sequence: 2*len(plan.Items) + 1, Complete: true}))
	status := fileBatchTestStatus(t, m, owner, id)
	if status.Items != 2502 || status.Started != 2502 || status.Completed != 2502 || status.Unchanged != 0 || status.Unknown != 0 || !status.Finished || status.Restored || status.PersistenceError != "" || status.Bytes != status.SavedBytes {
		t.Fatalf("incorrect completed status: %+v", status)
	}
	body, info := fileBatchTestReadAll(t, m, owner, id)
	if !bytes.Equal(body, expected.Bytes()) || !info.Saved || len(body) <= 1<<20 {
		t.Fatalf("complete JSONL differs or did not cross scan window: bytes=%d, saved=%v", len(body), info.Saved)
	}
	var meta batchMetadata
	fileBatchTestOK(t, json.Unmarshal(store.blobs[batchMetaName(id)], &meta))
	if len(meta.Plan) < 2 || len(meta.Events) < 2 {
		t.Fatalf("fixture did not cross plan and event segments: %d, %d", len(meta.Plan), len(meta.Events))
	}
	for name, data := range store.blobs {
		if bytes.Contains(data, []byte(call)) || strings.Contains(name, call) {
			t.Fatalf("raw call ID leaked into %s", name)
		}
		if (strings.HasPrefix(name, "result_bp_") || strings.HasPrefix(name, "result_be_")) && len(data) > 256<<10 {
			t.Fatalf("oversized segment %s: %d", name, len(data))
		}
	}
	fileBatchTestSearchAll(t, m, owner, id, `"type":"actual"`, body, 97)
	fileBatchTestSearchAll(t, m, owner, id, "no-such-journal-literal", body, 7)
	boundary := int(meta.Plan[0].Bytes)
	needle := string(body[boundary-12 : boundary+24])
	if !strings.Contains(needle, "\n") {
		t.Fatal("cross-segment search fixture lacks JSONL boundary")
	}
	fileBatchTestSearchAll(t, m, owner, id, needle, body, 2)
	restored := NewBatchManager(BatchOptions{})
	writes := store.ioCounts()[1]
	fileBatchTestOK(t, restored.Bind(ctx, owner, store))
	restoredBody, restoredInfo := fileBatchTestReadAll(t, restored, owner, id)
	if !bytes.Equal(restoredBody, body) || restoredInfo != info || !fileBatchTestStatus(t, restored, owner, id).Restored || store.ioCounts()[1] != writes {
		t.Fatal("large journal restoration changed bytes, metadata, or backend")
	}
}

func TestFileBatchNoSaveOverlapAndLiveTail(t *testing.T) {
	ctx := context.Background()
	const owner = "memory-owner"
	store := newFileBatchTestStore()
	m := NewBatchManager(BatchOptions{})
	fileBatchTestOK(t, m.Bind(ctx, owner, store))
	fileBatchTestOK(t, m.Bind(ctx, owner, nil))
	baseline := store.ioCounts()
	plan := fileBatchTestPlan(2)
	id := fileBatchTestBegin(t, m, owner, "memory-call", plan)
	body, info := fileBatchTestReadAll(t, m, owner, id)
	if info.Saved {
		t.Fatal("no-save plan claims durable storage")
	}
	fileBatchTestSearchAll(t, m, owner, id, "aaa", body, 7) // Overlapping source-version matches.
	needle := "}\n{\"version\":1,\"type\":\"pending\""
	before, err := m.Search(ctx, owner, id, needle, 0, 10)
	fileBatchTestOK(t, err)
	if len(before.Offsets) != 0 || before.More || before.NextOffset >= info.Bytes {
		t.Fatalf("active search discarded partial-literal candidates: %+v", before)
	}
	fileBatchTestOK(t, m.Pending(ctx, owner, id, 0))
	after, err := m.Search(ctx, owner, id, needle, before.NextOffset, 10)
	fileBatchTestOK(t, err)
	if !slices.Equal(after.Offsets, []int64{info.Bytes - 2}) {
		t.Fatalf("live-tail continuation lost boundary hit: %+v", after)
	}
	fileBatchTestOK(t, m.Settle(ctx, owner, id, 0, fileBatchTestActual(plan.Items[0])))
	for index := 1; index < len(plan.Items); index++ {
		fileBatchTestOK(t, m.Pending(ctx, owner, id, index))
		fileBatchTestOK(t, m.Settle(ctx, owner, id, index, fileBatchTestActual(plan.Items[index])))
	}
	fileBatchTestOK(t, m.Finish(ctx, owner, id, true, ""))
	_, info = fileBatchTestReadAll(t, m, owner, id)
	status := fileBatchTestStatus(t, m, owner, id)
	items, err := m.List(ctx, owner)
	fileBatchTestOK(t, err)
	fileBatchTestOK(t, m.RecordCall(ctx, owner, "memory-marker-only"))
	if status.SavedBytes != 0 || !status.Finished || status.Completed != 3 || info.Saved || len(items) != 1 || items[0] != info || !m.HasCall(owner, "memory-marker-only") || store.ioCounts() != baseline {
		t.Fatalf("no-save boundary violated: status=%+v, backend I/O=%v -> %v", status, baseline, store.ioCounts())
	}
	m.DropOwner(owner)
	if m.memory != 0 || m.count != 0 || len(m.owners) != 0 || store.ioCounts() != baseline {
		t.Fatal("closed memory owner leaked charged memory, identities, or backend I/O")
	}
}

func TestFileBatchBudgetsRejectWithoutRetaining(t *testing.T) {
	ctx := context.Background()
	plan := fileBatchTestPlan(2)
	for _, test := range []struct {
		name    string
		options BatchOptions
	}{
		{"entries", BatchOptions{Entries: 2}},
		{"plan-bytes", BatchOptions{PlanBytes: 256}},
		{"memory-bytes", BatchOptions{MemoryBytes: 1 << 20}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := NewBatchManager(test.options)
			for attempt := 0; attempt < 5; attempt++ {
				call := fmt.Sprintf("rejected-%d", attempt)
				id, err := m.Begin(ctx, "budget-owner", call, plan)
				fileBatchTestCode(t, err, "limit")
				if id != "" || m.HasCall("budget-owner", call) || m.memory != 0 || m.count != 0 || len(m.owners) != 0 {
					t.Fatalf("budget rejection retained state: id=%q memory=%d count=%d owners=%d", id, m.memory, m.count, len(m.owners))
				}
			}
		})
	}
	t.Run("max-records-includes-marker-only", func(t *testing.T) {
		m := NewBatchManager(BatchOptions{MaxRecords: 1})
		fileBatchTestOK(t, m.RecordCall(ctx, "budget-owner", "retained-marker"))
		memory := m.memory
		for attempt := 0; attempt < 5; attempt++ {
			call := fmt.Sprintf("rejected-%d", attempt)
			_, err := m.Begin(ctx, "another-owner", call, plan)
			fileBatchTestCode(t, err, "limit")
			fileBatchTestCode(t, m.RecordCall(ctx, "budget-owner", call), "limit")
			if m.memory != memory || m.count != 1 || len(m.owners) != 1 || !m.HasCall("budget-owner", "retained-marker") || m.HasCall("budget-owner", call) {
				t.Fatalf("record budget leaked or evicted identity: memory=%d count=%d owners=%d", m.memory, m.count, len(m.owners))
			}
		}
	})
}
