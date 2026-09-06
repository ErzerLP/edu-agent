package localartifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type artifactStore struct {
	mu           sync.Mutex
	blobs        map[string][]byte
	reads        []string
	writes       []string
	prefixes     []string
	listErr      error
	revoked      bool
	failWrite    int
	publishError bool
	listOverride []string
}

func newArtifactStore() *artifactStore { return &artifactStore{blobs: make(map[string][]byte)} }

func checkStoreContext(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("missing context deadline")
	}
	return ctx.Err()
}

func (s *artifactStore) ReadArtifact(ctx context.Context, name string) ([]byte, error) {
	if err := checkStoreContext(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, name)
	if s.revoked {
		return nil, errors.New("secret /profile/key revoked")
	}
	data, exists := s.blobs[name]
	if !exists {
		return nil, errors.New("secret /profile/missing")
	}
	// Deliberately return/retain aliases: manager pages and Put inputs must
	// still be independent of this store's buffers.
	return data, nil
}

func (s *artifactStore) WriteArtifact(ctx context.Context, name string, data []byte) error {
	if err := checkStoreContext(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, name)
	fail := len(s.writes) == s.failWrite
	if !fail || s.publishError {
		s.blobs[name] = data
	}
	if fail {
		return errors.New("unknown publication /secret/path")
	}
	return nil
}

func (s *artifactStore) ListArtifacts(ctx context.Context, prefix string) ([]string, error) {
	if err := checkStoreContext(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prefixes = append(s.prefixes, prefix)
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.revoked {
		return nil, errors.New("secret revoked")
	}
	if s.listOverride != nil {
		return slices.Clone(s.listOverride), nil
	}
	var names []string
	for name := range s.blobs {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

func requireArtifactCode(t *testing.T, err error, code string) {
	t.Helper()
	var stable *Error
	if !errors.As(err, &stable) || stable.Code != code || err.Error() != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

func putArtifact(t *testing.T, m *Manager, owner, kind string, data []byte) Info {
	t.Helper()
	info, err := m.Put(context.Background(), owner, kind, data)
	if err != nil {
		t.Fatal(err)
	}
	if !validID(info.ID) || info.Hash != digest(data) || info.Kind != kind || info.Bytes != int64(len(data)) {
		t.Fatalf("invalid info: %+v", info)
	}
	return info
}

func TestArtifactMemoryIsolationAndPagination(t *testing.T) {
	m := New(Options{})
	owner := "session-secret/call-secret"
	data := bytes.Repeat([]byte{0xff, 0, 0x1b, '\r', '\n'}, segmentBytes/5+100)
	copy(data[65535:], []byte("你好"))
	expected := slices.Clone(data)
	info := putArtifact(t, m, owner, "diff", data)
	if info.Saved || strings.Contains(info.ID, "secret") {
		t.Fatalf("bad memory info: %+v", info)
	}
	data[0] = 'X'
	var got []byte
	for offset := int64(0); ; {
		page, err := m.Read(nil, owner, info.ID, offset, 65536)
		if err != nil {
			t.Fatal(err)
		}
		if page.Offset != offset || page.NextOffset != offset+int64(len(page.Data)) || page.Info != info {
			t.Fatalf("bad cursor: %+v", page)
		}
		got = append(got, page.Data...)
		if len(page.Data) > 0 {
			page.Data[0] ^= 0xff
		}
		if !page.More {
			break
		}
		offset = page.NextOffset
	}
	if !bytes.Equal(got, expected) {
		t.Fatal("raw bytes changed")
	}
	first, err := m.Read(nil, owner, info.ID, 0, 4)
	if err != nil || !bytes.Equal(first.Data, expected[:4]) {
		t.Fatalf("page mutation changed history: %v", err)
	}
	eof, err := m.Read(nil, owner, info.ID, info.Bytes, 1)
	if err != nil || len(eof.Data) != 0 || eof.More || eof.NextOffset != info.Bytes {
		t.Fatalf("bad EOF: %+v %v", eof, err)
	}
	list := m.List(owner)
	list[0].Hash = "changed"
	if len(m.List("other")) != 0 || m.List(owner)[0] != info {
		t.Fatal("list aliases or leaks owners")
	}
	_, err = m.Read(nil, "other", info.ID, 0, 1)
	requireArtifactCode(t, err, "artifact_not_found")
	empty := putArtifact(t, m, owner, "receipt", nil)
	page, err := m.Read(nil, owner, empty.ID, 0, 1)
	if err != nil || page.More || page.NextOffset != 0 || len(page.Data) != 0 {
		t.Fatalf("empty receipt: %+v %v", page, err)
	}
	if err := m.Bind(owner, nil); err != nil || len(m.List(owner)) != 2 {
		t.Fatalf("no-save detach lost memory: %v", err)
	}
	m.DropOwner(owner)
	if len(m.List(owner)) != 0 || m.memory != 0 || len(m.owners) != 0 {
		t.Fatal("DropOwner did not release memory/owner")
	}
}

func TestArtifactPersistentRoundTripAndRevocation(t *testing.T) {
	store := newArtifactStore()
	m := New(Options{})
	if err := m.Bind("alice", store); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("diff\x00\xff\x1b你好\n"), 50000)
	expected := slices.Clone(data)
	info := putArtifact(t, m, "alice", "diff", data)
	data[0] = '!'
	if !info.Saved || m.records[info.ID].body != nil || m.memory != 0 {
		t.Fatal("saved body retained in plaintext cache")
	}
	if len(store.writes) != len(m.records[info.ID].meta.SegmentHashes)+1 || store.writes[len(store.writes)-1] != metadataName(info.ID) {
		t.Fatal("metadata was not the final write")
	}
	for name, blob := range store.blobs {
		if len(name) > 160 || len(blob) > 512<<10 || strings.Contains(name, "alice") {
			t.Fatalf("unbounded or identifying blob: %s", name)
		}
		for _, c := range name {
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
				t.Fatalf("invalid blob name %q", name)
			}
		}
	}
	if bytes.Contains(store.blobs[metadataName(info.ID)], []byte("alice")) {
		t.Fatal("metadata stored raw owner")
	}
	// Orphans and task metadata must never be read or restored.
	store.blobs["result_d_orphan_0"] = []byte("orphan")
	store.blobs["task_unrelated"] = []byte("not result metadata")
	resumed := New(Options{})
	if err := resumed.Bind("alice", store); err != nil {
		t.Fatal(err)
	}
	if list := resumed.List("alice"); len(list) != 1 || list[0] != info {
		t.Fatalf("restored info: %+v", list)
	}
	for _, name := range store.reads {
		if name == "result_d_orphan_0" || name == "task_unrelated" {
			t.Fatal("read outside authenticated result references")
		}
	}
	for _, offset := range []int64{0, segmentBytes - 3, info.Bytes - 9} {
		page, err := resumed.Read(nil, "alice", info.ID, offset, 29)
		if err != nil || !bytes.Equal(page.Data, expected[offset:min(offset+29, info.Bytes)]) {
			t.Fatalf("persistent range at %d: %v", offset, err)
		}
		page.Data[0] ^= 0xff
	}
	page, err := resumed.Read(nil, "alice", info.ID, 0, 10)
	if err != nil || !bytes.Equal(page.Data, expected[:10]) {
		t.Fatal("returned page altered saved bytes")
	}
	other := New(Options{})
	requireArtifactCode(t, other.Bind("bob", store), "artifact_corrupt")
	if len(other.List("bob")) != 0 {
		t.Fatal("cross-owner bind registered data")
	}
	empty := putArtifact(t, m, "alice", "receipt", nil)
	store.revoked = true
	for _, candidate := range []Info{info, empty} {
		for _, offset := range []int64{0, candidate.Bytes} {
			_, err := m.Read(nil, "alice", candidate.ID, offset, 1)
			requireArtifactCode(t, err, "artifact_unavailable")
			_, err = m.Search(nil, "alice", candidate.ID, "x", offset, 1)
			requireArtifactCode(t, err, "artifact_unavailable")
		}
	}
	if err := m.Bind("alice", nil); err != nil || len(m.List("alice")) != 0 {
		t.Fatalf("detach did not clear saved directory: %v", err)
	}
	if len(store.writes) == 0 || len(store.blobs) == 0 {
		t.Fatal("detach deleted persistent evidence")
	}
}

func TestArtifactSearchBoundaries(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "saved"}[persistent], func(t *testing.T) {
			m := New(Options{})
			store := newArtifactStore()
			if persistent {
				if err := m.Bind("owner", store); err != nil {
					t.Fatal(err)
				}
			}
			data := bytes.Repeat([]byte{'x'}, 2*maxSearchBytes+13)
			needle := "你a好"
			positions := []int64{segmentBytes - 2, maxSearchBytes - 2, 2*maxSearchBytes - 5}
			for _, position := range positions {
				copy(data[position:], needle)
			}
			info := putArtifact(t, m, "owner", "diff", data)
			var found []int64
			offset := int64(0)
			for attempts := 0; attempts < 5; attempts++ {
				page, err := m.Search(nil, "owner", info.ID, needle, offset, 100)
				if err != nil || page.Scanned > maxSearchBytes || page.Offset != offset || page.Info != info {
					t.Fatalf("search: %+v %v", page, err)
				}
				found = append(found, page.Offsets...)
				if !page.More {
					if page.NextOffset != info.Bytes {
						t.Fatal("static EOF did not advance to bytes")
					}
					break
				}
				if page.NextOffset <= offset || page.NextOffset > offset+maxSearchBytes-int64(len(needle))+1 {
					t.Fatal("scan cursor skipped overlap or failed to advance")
				}
				offset = page.NextOffset
			}
			if !slices.Equal(found, positions) {
				t.Fatalf("cross-boundary matches: %v want %v", found, positions)
			}
			// A 512-byte needle split across the scan budget must survive continuation.
			longNeedle := strings.Repeat("y", 512)
			longData := bytes.Repeat([]byte{'x'}, maxSearchBytes+600)
			copy(longData[maxSearchBytes-511:], longNeedle)
			long := putArtifact(t, m, "owner", "diff", longData)
			first, err := m.Search(nil, "owner", long.ID, longNeedle, 0, 100)
			if err != nil || len(first.Offsets) != 0 || !first.More || first.NextOffset != maxSearchBytes-511 {
				t.Fatalf("needle overlap lost: %+v %v", first, err)
			}
			second, err := m.Search(nil, "owner", long.ID, longNeedle, first.NextOffset, 100)
			if err != nil || !slices.Equal(second.Offsets, []int64{maxSearchBytes - 511}) {
				t.Fatalf("needle continuation: %+v %v", second, err)
			}
			// Hit-limit pagination uses candidate+1, not needle length.
			overlap := putArtifact(t, m, "owner", "receipt", []byte("aaaaaA"))
			first, err = m.Search(nil, "owner", overlap.ID, "aa", 0, 2)
			if err != nil || !slices.Equal(first.Offsets, []int64{0, 1}) || first.NextOffset != 2 || !first.More || first.Scanned != 3 {
				t.Fatalf("overlap page: %+v %v", first, err)
			}
			first.Offsets[0] = 99
			second, err = m.Search(nil, "owner", overlap.ID, "aa", 2, 100)
			if err != nil || !slices.Equal(second.Offsets, []int64{2, 3}) || second.More || second.NextOffset != overlap.Bytes {
				t.Fatalf("overlap continuation: %+v %v", second, err)
			}
			repeated, err := m.Search(nil, "owner", overlap.ID, "aa", 0, 2)
			if err != nil || !slices.Equal(repeated.Offsets, []int64{0, 1}) {
				t.Fatal("search response alias/shared cursor")
			}
			if !persistent && (len(store.reads) != 0 || len(store.writes) != 0 || len(store.prefixes) != 0) {
				t.Fatal("no-save used backend")
			}
		})
	}
}

func TestArtifactArgumentsAndBudgets(t *testing.T) {
	defaults := New(Options{})
	if defaults.options != (Options{DefaultMaxArtifactBytes, DefaultMemoryBytes, DefaultMaxRecords}) {
		t.Fatal("wrong defaults")
	}
	for _, options := range []Options{{MaxArtifactBytes: -1}, {MemoryBytes: -1}, {MaxRecords: -1},
		{MaxArtifactBytes: maxConfiguredBytes + 1}, {MemoryBytes: maxConfiguredBytes + 1}, {MaxRecords: maxConfiguredRecords + 1}} {
		m := New(options)
		store := newArtifactStore()
		requireArtifactCode(t, m.Bind("owner", store), "artifact_invalid_configuration")
		_, err := m.Put(nil, "owner", "diff", nil)
		requireArtifactCode(t, err, "artifact_invalid_configuration")
		if len(store.prefixes) != 0 || len(store.writes) != 0 {
			t.Fatal("invalid configuration reached backend")
		}
	}
	m := New(Options{MaxArtifactBytes: 5, MemoryBytes: 5, MaxRecords: 2})
	first := putArtifact(t, m, "alice", "diff", []byte("12345"))
	_, err := m.Put(nil, "alice", "receipt", []byte("x"))
	requireArtifactCode(t, err, "artifact_limit")
	second := putArtifact(t, m, "bob", "receipt", nil)
	_, err = m.Put(nil, "other", "diff", nil)
	requireArtifactCode(t, err, "artifact_limit")
	if len(m.List("alice")) != 1 || m.List("alice")[0] != first || m.List("bob")[0] != second {
		t.Fatal("quota evicted existing records")
	}
	m.DropOwner("alice")
	putArtifact(t, m, "bob", "diff", []byte("abcde"))
	_, err = defaults.Put(nil, "", "diff", nil)
	requireArtifactCode(t, err, "artifact_invalid_arguments")
	_, err = defaults.Put(nil, "owner", "upload", nil)
	requireArtifactCode(t, err, "artifact_invalid_arguments")
	requireArtifactCode(t, defaults.Bind("", newArtifactStore()), "artifact_invalid_arguments")
	info := putArtifact(t, defaults, "owner", "diff", []byte("abc"))
	for _, tc := range []struct {
		offset int64
		limit  int
	}{{-1, 1}, {4, 1}, {0, 0}, {0, 65537}} {
		_, err := defaults.Read(nil, "owner", info.ID, tc.offset, tc.limit)
		requireArtifactCode(t, err, "artifact_invalid_arguments")
	}
	for _, tc := range []struct {
		needle string
		offset int64
		limit  int
	}{{"", 0, 1}, {"\xff", 0, 1}, {strings.Repeat("a", 513), 0, 1}, {"a", -1, 1}, {"a", 4, 1}, {"a", 0, 0}, {"a", 0, 101}} {
		_, err := defaults.Search(nil, "owner", info.ID, tc.needle, tc.offset, tc.limit)
		requireArtifactCode(t, err, "artifact_invalid_arguments")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = defaults.Put(ctx, "owner", "diff", nil)
	requireArtifactCode(t, err, "artifact_canceled")
	_, err = defaults.Read(ctx, "owner", info.ID, 0, 1)
	requireArtifactCode(t, err, "artifact_canceled")
	_, err = defaults.Search(ctx, "owner", info.ID, "a", 0, 1)
	requireArtifactCode(t, err, "artifact_canceled")
	// The blob size is checked before a segment can reach storage.
	limited := New(Options{MaxArtifactBytes: 1})
	store := newArtifactStore()
	if err := limited.Bind("owner", store); err != nil {
		t.Fatal(err)
	}
	_, err = limited.Put(nil, "owner", "diff", []byte("xx"))
	requireArtifactCode(t, err, "artifact_limit")
	if len(store.writes) != 0 {
		t.Fatal("oversize result reached storage")
	}
}

func TestArtifactUnknownPublication(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failAt    int
		published bool
		restored  int
	}{
		{"orphan", 2, true, 0}, {"metadata_unpublished", 3, false, 0}, {"metadata_unknown", 3, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newArtifactStore()
			store.failWrite, store.publishError = tc.failAt, tc.published
			m := New(Options{MaxRecords: 1})
			if err := m.Bind("owner", store); err != nil {
				t.Fatal(err)
			}
			_, err := m.Put(nil, "owner", "diff", bytes.Repeat([]byte{'x'}, segmentBytes+1))
			requireArtifactCode(t, err, "artifact_store_failed")
			if len(m.List("owner")) != 0 || len(store.writes) != tc.failAt || m.pending != 0 || m.memory != 0 {
				t.Fatal("failed put registered, retried, or retained reservation")
			}
			before := len(store.blobs)
			if err := m.Bind("owner", store); err != nil {
				t.Fatal(err)
			}
			if len(m.List("owner")) != tc.restored || len(store.blobs) != before || len(store.writes) != tc.failAt {
				t.Fatal("unknown outcome was not preserved/recovered as data")
			}
			if tc.restored == 1 {
				info := m.List("owner")[0]
				page, err := m.Read(nil, "owner", info.ID, segmentBytes, 1)
				if err != nil || !info.Saved || !bytes.Equal(page.Data, []byte{'x'}) {
					t.Fatalf("recovered unknown metadata unreadable: %v", err)
				}
			}
		})
	}
}

func TestArtifactBindAtomicMergeAndDetach(t *testing.T) {
	store := newArtifactStore()
	source := New(Options{})
	if err := source.Bind("owner", store); err != nil {
		t.Fatal(err)
	}
	saved := putArtifact(t, source, "owner", "diff", []byte("saved"))
	m := New(Options{})
	memory := putArtifact(t, m, "owner", "receipt", []byte("memory"))
	if err := m.Bind("owner", store); err != nil || len(m.List("owner")) != 2 {
		t.Fatalf("merge failed: %v", err)
	}
	before := m.List("owner")
	bad := newArtifactStore()
	bad.listErr = errors.New("secret catalog unreadable")
	requireArtifactCode(t, m.Bind("owner", bad), "artifact_unavailable")
	requireArtifactCode(t, m.Bind("owner", newArtifactStore()), "artifact_unavailable")
	if !reflect.DeepEqual(m.List("owner"), before) {
		t.Fatal("failed rebind changed records")
	}
	if _, err := m.Read(nil, "owner", saved.ID, 0, 1); err != nil {
		t.Fatal("failed rebind changed backend")
	}
	if err := m.Bind("owner", nil); err != nil || !reflect.DeepEqual(m.List("owner"), []Info{memory}) {
		t.Fatalf("detach lost real memory or retained saved: %v", err)
	}
	if _, err := m.Read(nil, "owner", memory.ID, 0, 1); err != nil {
		t.Fatal(err)
	}
	// Same ID but different content must not replace memory, even if the
	// alternate metadata and body are individually self-consistent.
	conflict := newArtifactStore()
	meta := makeMetadata("owner", memory.ID, "receipt", []byte("evil"))
	if err := save(context.Background(), conflict, meta, []byte("evil")); err != nil {
		t.Fatal(err)
	}
	requireArtifactCode(t, m.Bind("owner", conflict), "artifact_corrupt")
	if m.List("owner")[0] != memory {
		t.Fatal("immutable conflict replaced memory")
	}
	// An exact match is safely promoted only after authenticating all bytes.
	exact := newArtifactStore()
	if err := save(context.Background(), exact, m.records[memory.ID].meta, []byte("memory")); err != nil {
		t.Fatal(err)
	}
	if err := m.Bind("owner", exact); err != nil || !m.List("owner")[0].Saved || m.memory != 0 || m.records[memory.ID].body != nil {
		t.Fatalf("verified promotion failed: %v", err)
	}
	// A fresh bind cannot silently exceed the manager-wide record budget.
	full := New(Options{MaxRecords: 1})
	retained := putArtifact(t, full, "another", "receipt", nil)
	requireArtifactCode(t, full.Bind("owner", store), "artifact_limit")
	if full.List("another")[0] != retained || len(full.List("owner")) != 0 {
		t.Fatal("bind quota failure evicted data")
	}
	tooSmall := New(Options{MaxArtifactBytes: 1})
	requireArtifactCode(t, tooSmall.Bind("owner", store), "artifact_limit")
}

func TestArtifactStrictMetadataAndSegments(t *testing.T) {
	id := "r_ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	body := []byte("abc")
	base := makeMetadata("owner", id, "diff", body)
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	rewrite := func(field string, value any) []byte {
		var fields map[string]any
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		fields[field] = value
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	cases := []struct {
		name string
		data []byte
		code string
	}{
		{"unknown", rewrite("future", true), "artifact_corrupt"},
		{"future", rewrite("version", 2), "artifact_version_unsupported"},
		{"version_zero", rewrite("version", 0), "artifact_corrupt"},
		{"version_fraction", rewrite("version", 1.5), "artifact_corrupt"},
		{"duplicate", append([]byte(`{"version":1,`), encoded[1:]...), "artifact_corrupt"},
		{"duplicate_future", append([]byte(`{"version":2,`), encoded[1:]...), "artifact_corrupt"},
		{"missing", bytes.Replace(encoded, []byte(`"version":1,`), nil, 1), "artifact_corrupt"},
		{"null", rewrite("hash", nil), "artifact_corrupt"},
		{"null_array", rewrite("segment_hashes", nil), "artifact_corrupt"},
		{"null_segment", rewrite("segment_hashes", []any{nil}), "artifact_corrupt"},
		{"bad_kind", rewrite("kind", "shell"), "artifact_corrupt"},
		{"bad_id", rewrite("id", id+"X"), "artifact_corrupt"},
		{"other_owner", rewrite("owner_hash", digest([]byte("other"))), "artifact_corrupt"},
		{"negative", rewrite("bytes", -1), "artifact_corrupt"},
		{"giant_length", rewrite("bytes", int64(1)<<62), "artifact_corrupt"},
		{"short_digest", rewrite("hash", "sha256:aa"), "artifact_corrupt"},
		{"upper_digest", rewrite("hash", strings.ToUpper(base.Hash)), "artifact_corrupt"},
		{"missing_segment", rewrite("segment_hashes", []string{}), "artifact_corrupt"},
		{"extra_segment", rewrite("segment_hashes", []string{digest(body), digest(body)}), "artifact_corrupt"},
		{"segment_object", rewrite("segment_hashes", []any{map[string]int{"a": 1}}), "artifact_corrupt"},
		{"trailing", append(slices.Clone(encoded), []byte(` {}`)...), "artifact_corrupt"},
		{"oversize", bytes.Repeat([]byte{' '}, metadataBytes+1), "artifact_corrupt"},
		{"invalid_utf8", bytes.Replace(encoded, []byte("diff"), []byte{'d', 0xff}, 1), "artifact_corrupt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newArtifactStore()
			store.blobs[metadataName(id)] = tc.data
			store.blobs[segmentName(id, 0)] = slices.Clone(body)
			m := New(Options{})
			requireArtifactCode(t, m.Bind("owner", store), tc.code)
			if len(m.List("owner")) != 0 {
				t.Fatal("corrupt metadata registered")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*artifactStore)
		code   string
	}{
		{"hash_mismatch", func(s *artifactStore) { s.blobs[metadataName(id)] = rewrite("hash", digest([]byte("xyz"))) }, "artifact_corrupt"},
		{"short_segment", func(s *artifactStore) { s.blobs[segmentName(id, 0)] = []byte("ab") }, "artifact_corrupt"},
		{"changed_segment", func(s *artifactStore) { s.blobs[segmentName(id, 0)] = []byte("xyz") }, "artifact_corrupt"},
		{"unavailable_segment", func(s *artifactStore) { delete(s.blobs, segmentName(id, 0)) }, "artifact_unavailable"},
		{"duplicate_listing", func(s *artifactStore) { s.listOverride = []string{metadataName(id), metadataName(id)} }, "artifact_corrupt"},
		{"wrong_namespace", func(s *artifactStore) { s.listOverride = []string{"task_" + id} }, "artifact_corrupt"},
		{"malformed_id", func(s *artifactStore) { s.listOverride = []string{metadataPrefix + "../secret"} }, "artifact_corrupt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newArtifactStore()
			store.blobs[metadataName(id)] = slices.Clone(encoded)
			store.blobs[segmentName(id, 0)] = slices.Clone(body)
			tc.mutate(store)
			requireArtifactCode(t, New(Options{}).Bind("owner", store), tc.code)
		})
	}
	// Segment authentication is performed again on read/search, not only Bind.
	store := newArtifactStore()
	store.blobs[metadataName(id)] = slices.Clone(encoded)
	store.blobs[segmentName(id, 0)] = slices.Clone(body)
	m := New(Options{})
	if err := m.Bind("owner", store); err != nil {
		t.Fatal(err)
	}
	store.blobs[segmentName(id, 0)] = []byte("xyz")
	_, err = m.Read(nil, "owner", id, 0, 1)
	requireArtifactCode(t, err, "artifact_corrupt")
	_, err = m.Search(nil, "owner", id, "a", 0, 1)
	requireArtifactCode(t, err, "artifact_corrupt")
	store.blobs[metadataName(id)] = rewrite("kind", "receipt")
	_, err = m.Read(nil, "owner", id, 3, 1)
	requireArtifactCode(t, err, "artifact_corrupt")
}

// A store blocked in I/O must not hold the manager-wide mutex. The channels
// coordinate a real pending Put without sleeps or an additional backend thread.
type blockingArtifactStore struct {
	*artifactStore
	entered chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (s *blockingArtifactStore) WriteArtifact(ctx context.Context, name string, data []byte) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.proceed:
		return s.artifactStore.WriteArtifact(ctx, name, data)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestArtifactOwnerGateAndPendingQuota(t *testing.T) {
	store := &blockingArtifactStore{artifactStore: newArtifactStore(), entered: make(chan struct{}), proceed: make(chan struct{})}
	m := New(Options{MaxRecords: 2})
	if err := m.Bind("alice", store); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := m.Put(context.Background(), "alice", "diff", []byte("pending"))
		result <- err
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("put did not reach store")
	}
	other := make(chan error, 1)
	go func() {
		_, err := m.Put(nil, "bob", "receipt", []byte("independent"))
		other <- err
	}()
	select {
	case err := <-other:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(store.proceed)
		t.Fatal("backend I/O held manager mutex")
	}
	_, err := m.Put(nil, "carol", "receipt", nil)
	requireArtifactCode(t, err, "artifact_limit")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = m.Put(ctx, "alice", "receipt", nil)
	requireArtifactCode(t, err, "artifact_canceled")
	bound := make(chan error, 1)
	go func() { bound <- m.Bind("alice", nil) }()
	select {
	case err := <-bound:
		close(store.proceed)
		t.Fatalf("bind bypassed active owner gate: %v", err)
	default:
	}
	close(store.proceed)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := <-bound; err != nil {
		t.Fatal(err)
	}
	if len(m.List("alice")) != 0 || len(m.List("bob")) != 1 || m.pending != 0 {
		t.Fatal("concurrent detach/publication had inconsistent state")
	}
	putArtifact(t, m, "carol", "receipt", nil)
}

func TestArtifactStableStoreErrors(t *testing.T) {
	for _, code := range []string{"artifact_limit", "artifact_unavailable", "artifact_corrupt", "artifact_version_unsupported", "artifact_store_failed", "artifact_canceled"} {
		err := storeError(&Error{Code: code}, "artifact_unavailable")
		requireArtifactCode(t, err, code)
	}
	for _, err := range []error{errors.New("secret backend error"), &Error{Code: "secret backend code"}} {
		requireArtifactCode(t, storeError(err, "artifact_store_failed"), "artifact_store_failed")
	}
}
