//go:build linux || darwin

package localexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type byteArtifactStore struct {
	mu                   sync.Mutex
	blobs                map[string][]byte
	reads, writes, lists int
	beforeWrite          func(context.Context, string, []byte) error
	readError            error
	accessError          error
}

func newByteArtifactStore() *byteArtifactStore {
	return &byteArtifactStore{blobs: make(map[string][]byte)}
}
func (s *byteArtifactStore) CheckArtifactAccess(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accessError
}
func (s *byteArtifactStore) ReadArtifact(ctx context.Context, name string) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.readError != nil {
		return nil, s.readError
	}
	data, exists := s.blobs[name]
	if !exists {
		return nil, failure("output_unavailable")
	}
	return append([]byte(nil), data...), nil
}
func (s *byteArtifactStore) WriteArtifact(ctx context.Context, name string, data []byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(name) > 160 || len(data) > archiveSegmentBytes {
		return errors.New("private backend limits")
	}
	for _, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return errors.New("private invalid path")
		}
	}
	s.mu.Lock()
	s.writes++
	hook := s.beforeWrite
	s.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, name, data); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.mu.Lock()
	s.blobs[name] = append([]byte(nil), data...)
	s.mu.Unlock()
	return nil
}
func (s *byteArtifactStore) ListArtifacts(ctx context.Context, prefix string) ([]string, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	var names []string
	for name := range s.blobs {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}
func (s *byteArtifactStore) copy() *byteArtifactStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := newByteArtifactStore()
	for name, data := range s.blobs {
		result.blobs[name] = append([]byte(nil), data...)
	}
	return result
}
func bindOutput(t *testing.T, m *Manager, owner string, store ArtifactStore) {
	t.Helper()
	if err := m.BindArchive(owner, store); err != nil {
		t.Fatal(err)
	}
}
func fileOutput(t *testing.T, m *Manager, owner, callID string, stdout, stderr []byte) Snapshot {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string][]byte{"stdout": stdout, "stderr": stderr} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := m.Start(context.Background(), owner, callID, StartArgs{CWD: dir, Shell: "/bin/sh", Command: "cat stdout; cat stderr >&2; exit 7"})
	if err != nil {
		t.Fatal(err)
	}
	return finish(t, m, owner, s.TaskID)
}

func TestPersistentOutputUncertainMetadataDoesNotReplay(t *testing.T) {
	store := newByteArtifactStore()
	dataWrites := 0
	store.beforeWrite = func(_ context.Context, name string, data []byte) error {
		if strings.HasPrefix(name, "out_") {
			dataWrites++
		}
		if strings.HasPrefix(name, "task_") {
			var meta archiveMetadata
			if err := json.Unmarshal(data, &meta); err != nil {
				return err
			}
			if !meta.Final && meta.StdoutSaved > 0 {
				store.mu.Lock()
				store.blobs[name] = append([]byte(nil), data...)
				store.mu.Unlock()
				return errors.New("publication may have happened")
			}
		}
		return nil
	}
	m := testManager(t, Options{OutputBytesPerTask: 17})
	bindOutput(t, m, "owner", store)
	s := fileOutput(t, m, "owner", "uncertain", bytes.Repeat([]byte("uncertain-output"), 10000), nil)
	if dataWrites != 1 || s.StdoutSaved != 0 || s.PersistenceError == "" || s.ExitCode == nil || *s.ExitCode != 7 {
		t.Fatalf("uncertain write was retried/claimed saved: %+v writes=%d", s, dataWrites)
	}
	restored := testManager(t, Options{})
	bindOutput(t, restored, "owner", store.copy())
	page, err := restored.Read("owner", s.TaskID, "stdout", 0, 64)
	if err != nil || len(page.Data) != 0 || !page.Truncated {
		t.Fatalf("orphaned output promoted: %+v %v", page, err)
	}
	dir := t.TempDir()
	again, err := restored.Start(t.Context(), "owner", "uncertain", StartArgs{CWD: dir, Command: "touch replayed", Shell: "/bin/sh"})
	if err != nil || again.TaskID != s.TaskID || again.Controllable {
		t.Fatalf("restored identity replay: %+v %v", again, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "replayed")); !os.IsNotExist(err) {
		t.Fatal("uncertain historical invocation executed again")
	}
}

func TestPersistentOutputRoundTripBeyondMemory(t *testing.T) {
	store := newByteArtifactStore()
	m := testManager(t, Options{OutputBytesPerTask: 17, OutputBytesTotal: 17})
	bindOutput(t, m, "owner", store)
	stdout := append(bytes.Repeat([]byte("abcdefg"), 80000), 0xff, 0, 'x')
	stderr := bytes.Repeat([]byte("error\n"), 100)
	s := fileOutput(t, m, "owner", "roundtrip", stdout, stderr)
	if s.Persistence != "saved" || s.PersistenceError != "" || s.OutputState != "complete" || s.StdoutSaved != int64(len(stdout)) || s.StderrSaved != int64(len(stderr)) || s.ExitCode == nil || *s.ExitCode != 7 {
		t.Fatalf("saved result: %+v", s)
	}
	if m.retained != 17 {
		t.Fatalf("memory grew beyond quota: %d", m.retained)
	}
	for stream, want := range map[string][]byte{"stdout": stdout, "stderr": stderr} {
		if got := output(t, m, "owner", s.TaskID, stream, 17001); !bytes.Equal(got, want) {
			t.Fatalf("live %s length=%d want=%d", stream, len(got), len(want))
		}
	}
	page, err := m.Read("owner", s.TaskID, "stdout", archiveSegmentBytes-3, 10)
	if err != nil || page.Availability != "saved" || page.Historical || page.Saved != int64(len(stdout)) || !bytes.Equal(page.Data, stdout[archiveSegmentBytes-3:archiveSegmentBytes+7]) {
		t.Fatalf("cross segment page: %+v %v", page, err)
	}
	page.Data[0] = 0
	page2, _ := m.Read("owner", s.TaskID, "stdout", archiveSegmentBytes-3, 10)
	if !bytes.Equal(page2.Data, stdout[archiveSegmentBytes-3:archiveSegmentBytes+7]) {
		t.Fatal("aliased archive read")
	}

	// Exhausted memory must not reject another task whose archive is available.
	next := launch(t, m, "owner", "after-memory-full", "printf second", false)
	next = finish(t, m, "owner", next.TaskID)
	if next.StdoutSaved != 6 || next.Persistence != "saved" {
		t.Fatalf("archive-only task: %+v", next)
	}
	reopened := testManager(t, Options{})
	bindOutput(t, reopened, "reopened-owner", store)
	history, err := reopened.Status("reopened-owner", s.TaskID)
	if err != nil || !history.Restored || history.Controllable || history.State != StateExited || history.Persistence != "saved" {
		t.Fatalf("history: %+v %v", history, err)
	}
	for stream, want := range map[string][]byte{"stdout": stdout, "stderr": stderr} {
		if got := output(t, reopened, "reopened-owner", s.TaskID, stream, MaxReadBytes); !bytes.Equal(got, want) {
			t.Fatalf("restored %s mismatch", stream)
		}
	}
	historical, _ := reopened.Read("reopened-owner", s.TaskID, "stdout", 0, 1)
	if !historical.Historical || historical.Availability != "saved" {
		t.Fatalf("page: %+v", historical)
	}
	retry, err := reopened.Start(context.Background(), "reopened-owner", "roundtrip", StartArgs{Command: "must never execute"})
	if err != nil || retry.TaskID != s.TaskID || !retry.Restored {
		t.Fatalf("replayed identity: %+v %v", retry, err)
	}
	_, err = reopened.Read("wrong-owner", s.TaskID, "stdout", 0, 1)
	requireCode(t, err, "task_not_found")
	store.mu.Lock()
	for name, blob := range store.blobs {
		if strings.HasPrefix(name, "task_") && (bytes.Contains(blob, []byte("cat stdout")) || bytes.Contains(blob, []byte("roundtrip")) || bytes.Contains(blob, []byte("\"pid\""))) {
			t.Errorf("execution input in metadata: %s", name)
		}
	}
	store.mu.Unlock()
}

func TestPersistentOutputLiveRebindUnknownAndDetach(t *testing.T) {
	store := newByteArtifactStore()
	m := testManager(t, Options{OutputBytesPerTask: 2})
	bindOutput(t, m, "owner", store)
	s := launch(t, m, "owner", "live", "printf hello; cat", true)
	deadline := time.Now().Add(time.Second)
	for {
		current, _ := m.Status("owner", s.TaskID)
		if current.StdoutSaved == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prefix not committed")
		}
		time.Sleep(time.Millisecond)
	}
	crash := store.copy()
	restarted := testManager(t, Options{})
	bindOutput(t, restarted, "owner", crash)
	unknown, _ := restarted.Status("owner", s.TaskID)
	if unknown.State != StateUnknown || unknown.Controllable || !unknown.Restored || unknown.OutputState != "incomplete" || unknown.Persistence != "partial" {
		t.Fatalf("unknown: %+v", unknown)
	}
	stopped, err := restarted.Stop(context.Background(), "owner", s.TaskID)
	if err != nil || stopped.State != StateUnknown {
		t.Fatalf("historical Stop: %+v %v", stopped, err)
	}
	stillLive, _ := m.Status("owner", s.TaskID)
	if !stillLive.Controllable {
		t.Fatal("historical stop affected live process")
	}
	other := launch(t, m, "other", "independent", "printf other", false)
	finish(t, m, "other", other.TaskID)

	// A new handle sees the same data; bindings, rather than tasks, own handles.
	newHandle := store.copy()
	bindOutput(t, m, "owner", newHandle)
	current, _ := m.Status("owner", s.TaskID)
	if current.Restored || !current.Controllable || len(m.List("other")) != 1 {
		t.Fatalf("rebind replaced task: %+v", current)
	}
	if _, err := m.WriteInput(context.Background(), "owner", s.TaskID, []byte("world")); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseInput("owner", s.TaskID); err != nil {
		t.Fatal(err)
	}
	s = finish(t, m, "owner", s.TaskID)
	if got := string(output(t, m, "owner", s.TaskID, "stdout", 2)); got != "helloworld" {
		t.Fatalf("rebound output %q", got)
	}
	if s.Persistence != "saved" {
		t.Fatalf("rebound save: %+v", s)
	}
	bindOutput(t, m, "owner", nil)
	unavailable, err := m.Read("owner", s.TaskID, "stdout", 2, 8)
	if err != nil || unavailable.Availability != "unavailable" || unavailable.Retained != 2 || !unavailable.Truncated || unavailable.PersistenceError != "output_unavailable" || len(unavailable.Data) != 0 {
		t.Fatalf("detached: %+v %v", unavailable, err)
	}
	state, _ := m.Status("owner", s.TaskID)
	if state.Persistence != "unavailable" || state.StdoutRetained != 2 {
		t.Fatalf("unavailable stats: %+v", state)
	}
	bindOutput(t, m, "owner", newHandle)
	if got := string(output(t, m, "owner", s.TaskID, "stdout", 4)); got != "helloworld" {
		t.Fatalf("reattached: %q", got)
	}
}

func TestPersistentOutputQuotaAndFailuresRemainIndependent(t *testing.T) {
	for _, mode := range []string{"quota", "data", "metadata", "final", "initial"} {
		t.Run(mode, func(t *testing.T) {
			store := newByteArtifactStore()
			options := Options{OutputBytesPerTask: 3}
			if mode == "quota" {
				options.SavedOutputBytesPerTask = 19
			}
			dataWrites := 0
			store.beforeWrite = func(_ context.Context, name string, data []byte) error {
				if strings.HasPrefix(name, "out_") {
					dataWrites++
					if mode == "data" && dataWrites == 2 {
						return failure("output_store_full")
					}
				}
				if strings.HasPrefix(name, "task_") {
					var meta archiveMetadata
					if err := json.Unmarshal(data, &meta); err != nil {
						return err
					}
					if mode == "initial" || mode == "final" && meta.Final || mode == "metadata" && !meta.Final && meta.StdoutSaved > 0 {
						return errors.New("private backend path and secret")
					}
				}
				return nil
			}
			m := testManager(t, options)
			bindOutput(t, m, "owner", store)
			s := fileOutput(t, m, "owner", mode, bytes.Repeat([]byte("x"), 128<<10), []byte("end"))
			if s.State != StateExited || s.ExitCode == nil || *s.ExitCode != 7 || s.StdoutBytes != 128<<10 || s.StderrBytes != 3 || s.Controllable {
				t.Fatalf("store error changed execution: %+v", s)
			}
			if s.PersistenceError == "" || s.Persistence == "saved" || s.Persistence == "collecting" || strings.Contains(s.PersistenceError, "private") {
				t.Fatalf("dishonest save: %+v", s)
			}
			if mode == "quota" && (s.StdoutSaved+s.StderrSaved != 19 || s.Persistence != "partial" || s.PersistenceError != "output_store_full") {
				t.Fatalf("quota: %+v", s)
			}
			if mode == "data" && (dataWrites != 2 || s.StdoutSaved <= 0 || s.Persistence != "partial") {
				t.Fatalf("data retries/prefix: writes=%d %+v", dataWrites, s)
			}
			if (mode == "metadata" || mode == "initial") && (s.StdoutSaved != 0 || s.Persistence != "failed") {
				t.Fatalf("unpublished prefix: %+v", s)
			}
			if mode == "metadata" && dataWrites != 1 {
				t.Fatalf("retried failed journal: %d", dataWrites)
			}
			restarted := testManager(t, Options{})
			bindOutput(t, restarted, "owner", store)
			history := restarted.List("owner")
			if mode == "initial" {
				if len(history) != 0 {
					t.Fatalf("failed initial record appeared: %+v", history)
				}
				return
			}
			if len(history) != 1 {
				t.Fatalf("history count: %d", len(history))
			}
			if mode == "final" {
				if history[0].State != StateUnknown || history[0].Controllable || history[0].OutputState != "incomplete" {
					t.Fatalf("failed final restored terminal: %+v", history[0])
				}
			} else if history[0].State != StateExited || history[0].Persistence != s.Persistence {
				t.Fatalf("save/exit conflated: %+v", history[0])
			}
			if history[0].StdoutSaved != s.StdoutSaved {
				t.Fatalf("orphan promoted: disk=%d memory=%d", history[0].StdoutSaved, s.StdoutSaved)
			}
		})
	}
}

func TestPersistentOutputStrictMetadataAndTransactionalBind(t *testing.T) {
	store := newByteArtifactStore()
	m := testManager(t, Options{})
	bindOutput(t, m, "owner", store)
	s := fileOutput(t, m, "owner", "metadata", []byte("good"), nil)
	for _, mutation := range []string{"future", "unknown", "wrong-id", "duplicate", "missing", "negative", "state"} {
		t.Run(mutation, func(t *testing.T) {
			bad := store.copy()
			name := metadataName(s.TaskID)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(bad.blobs[name], &fields); err != nil {
				t.Fatal(err)
			}
			code := "output_corrupt"
			switch mutation {
			case "future":
				fields["version"] = json.RawMessage("3")
				code = "output_version_unsupported"
			case "unknown":
				fields["command"] = json.RawMessage(`"must-not-execute"`)
			case "wrong-id":
				fields["task_id"] = json.RawMessage(`"different-task"`)
			case "missing":
				delete(fields, "stdout_saved")
			case "negative":
				fields["stdout_saved"] = json.RawMessage("-1")
			case "state":
				fields["state"] = json.RawMessage(`"running"`)
			}
			data, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if mutation == "duplicate" {
				data = append([]byte(`{"version":1,`), data[1:]...)
			}
			bad.blobs[name] = data
			requireCode(t, m.BindArchive("owner", bad), code)
			if len(m.List("owner")) != 1 || string(output(t, m, "owner", s.TaskID, "stdout", 2)) != "good" {
				t.Fatal("failed bind changed records")
			}
			restarted := testManager(t, Options{})
			requireCode(t, restarted.BindArchive("owner", bad), code)
			if len(restarted.List("owner")) != 0 {
				t.Fatal("partial bind published")
			}
		})
	}
}

func TestPersistentOutputSearchBoundariesOverlapAndWaterline(t *testing.T) {
	store := newByteArtifactStore()
	m := testManager(t, Options{OutputBytesPerTask: 1})
	bindOutput(t, m, "owner", store)
	data := bytes.Repeat([]byte("."), 2*maxSearchBytes+20)
	copy(data[archiveSegmentBytes-2:], []byte("abcabc"))
	copy(data[maxSearchBytes-2:], []byte("abcabc"))
	copy(data[2*maxSearchBytes:], []byte("aaaaa"))
	s := fileOutput(t, m, "owner", "search", data, nil)
	restarted := testManager(t, Options{})
	bindOutput(t, restarted, "owner", store)
	var offsets []int64
	cursor := int64(0)
	for i := 0; i < 6; i++ {
		page, err := restarted.Search(context.Background(), "owner", s.TaskID, "stdout", "abcabc", cursor, 1)
		if err != nil || page.Scanned > maxSearchBytes || page.Truncated || page.Incomplete {
			t.Fatalf("search: %+v %v", page, err)
		}
		offsets = append(offsets, page.Offsets...)
		if !page.More {
			break
		}
		if page.NextOffset <= cursor {
			t.Fatalf("stalled: %+v", page)
		}
		cursor = page.NextOffset
	}
	if !reflect.DeepEqual(offsets, []int64{archiveSegmentBytes - 2, maxSearchBytes - 2}) {
		t.Fatalf("boundary hits: %v", offsets)
	}
	page, err := restarted.Search(context.Background(), "owner", s.TaskID, "stdout", "aaa", 2*maxSearchBytes, 2)
	if err != nil || !reflect.DeepEqual(page.Offsets, []int64{2 * maxSearchBytes, 2*maxSearchBytes + 1}) || page.NextOffset != 2*maxSearchBytes+2 || !page.More {
		t.Fatalf("overlap: %+v %v", page, err)
	}
	page, err = restarted.Search(context.Background(), "owner", s.TaskID, "stdout", "aaa", page.NextOffset, 2)
	if err != nil || !reflect.DeepEqual(page.Offsets, []int64{2*maxSearchBytes + 2}) || page.More || page.NextOffset != int64(len(data)) {
		t.Fatalf("overlap continuation: %+v %v", page, err)
	}
	page, err = restarted.Search(context.Background(), "owner", s.TaskID, "stdout", "absent", 0, 100)
	if err != nil || len(page.Offsets) != 0 || page.Scanned != maxSearchBytes || !page.More || page.NextOffset != maxSearchBytes-5 {
		t.Fatalf("bounded no match: %+v %v", page, err)
	}

	live := launch(t, m, "owner", "waterline", "printf ab; cat", true)
	awaitOutput(t, m, "owner", live.TaskID, "ab")
	page, err = m.Search(context.Background(), "owner", live.TaskID, "stdout", "abc", 0, 10)
	if err != nil || page.More || page.NextOffset != 0 {
		t.Fatalf("active overlap cursor: %+v %v", page, err)
	}
	if _, err := m.WriteInput(context.Background(), "owner", live.TaskID, []byte("c")); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseInput("owner", live.TaskID); err != nil {
		t.Fatal(err)
	}
	finish(t, m, "owner", live.TaskID)
	page, err = m.Search(context.Background(), "owner", live.TaskID, "stdout", "abc", page.NextOffset, 10)
	if err != nil || !reflect.DeepEqual(page.Offsets, []int64{0}) {
		t.Fatalf("new bytes lost: %+v %v", page, err)
	}
}

func TestPersistentOutputSearchGapValidationAndNoSave(t *testing.T) {
	store := newByteArtifactStore()
	m := testManager(t, Options{OutputBytesPerTask: 3})
	bindOutput(t, m, "owner", store)
	bindOutput(t, m, "owner", nil)
	store.mu.Lock()
	calls := store.reads + store.writes + store.lists
	store.mu.Unlock()
	s := fileOutput(t, m, "owner", "memory", []byte("abcdef"), nil)
	if s.Persistence != "memory_only" || s.StdoutSaved != 0 {
		t.Fatalf("no-save: %+v", s)
	}
	page, err := m.Search(context.Background(), "owner", s.TaskID, "stdout", "z", 0, 10)
	if err != nil || !page.Truncated || page.More || page.NextOffset != 6 || page.Scanned != 3 {
		t.Fatalf("gap: %+v %v", page, err)
	}
	_, err = m.Search(context.Background(), "owner", s.TaskID, "stdout", "", 0, 1)
	requireCode(t, err, "invalid_needle")
	_, err = m.Search(context.Background(), "owner", s.TaskID, "stdout", string([]byte{255}), 0, 1)
	requireCode(t, err, "invalid_needle")
	_, err = m.Search(context.Background(), "owner", s.TaskID, "stdout", strings.Repeat("x", 513), 0, 1)
	requireCode(t, err, "invalid_needle")
	_, err = m.Search(context.Background(), "owner", s.TaskID, "stdout", "a", 0, 101)
	requireCode(t, err, "invalid_limit")
	_, err = m.Search(context.Background(), "owner", s.TaskID, "stdout", "a", 7, 1)
	requireCode(t, err, "invalid_offset")
	_, err = m.Search(context.Background(), "owner", s.TaskID, "bad", "a", 0, 1)
	requireCode(t, err, "invalid_stream")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = m.Search(ctx, "owner", s.TaskID, "stdout", "a", 0, 1)
	requireCode(t, err, "search_canceled")
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.reads+store.writes+store.lists != calls {
		t.Fatal("detached no-save used backend")
	}
}

func TestPersistentOutputSlowStorageAndFinalLease(t *testing.T) {
	store := newByteArtifactStore()
	entered, release := make(chan struct{}), make(chan struct{})
	store.beforeWrite = func(ctx context.Context, name string, data []byte) error {
		if strings.HasPrefix(name, "out_") {
			select {
			case <-time.After(350 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if strings.HasPrefix(name, "task_") {
			var meta archiveMetadata
			if err := json.Unmarshal(data, &meta); err != nil {
				return err
			}
			if meta.Final {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		return nil
	}
	m := testManager(t, Options{OutputBytesPerTask: 1})
	bindOutput(t, m, "owner", store)
	s := launch(t, m, "owner", "slow", "printf slow", false)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("did not reach final metadata")
	}
	current, err := m.Status("owner", s.TaskID)
	if err != nil || !current.Controllable || !current.FinishedAt.IsZero() {
		t.Fatalf("lease released before settlement: %+v %v", current, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = m.Stop(ctx, "owner", s.TaskID)
	requireCode(t, err, "stop_wait_canceled")
	close(release)
	s = finish(t, m, "owner", s.TaskID)
	if s.OutputState != "complete" || s.Persistence != "saved" || s.CleanupIncomplete || s.State != StateExited {
		t.Fatalf("slow save misclassified as leaked pipe: %+v", s)
	}
	restarted := testManager(t, Options{})
	bindOutput(t, restarted, "owner", store)
	if restored, _ := restarted.Status("owner", s.TaskID); restored.State != StateExited {
		t.Fatalf("late Stop changed final metadata: %+v", restored)
	}
}

func TestPersistentOutputReadFailureAndFullMemoryStart(t *testing.T) {
	store := newByteArtifactStore()
	m := testManager(t, Options{OutputBytesPerTask: 1, OutputBytesTotal: 1})
	bindOutput(t, m, "owner", store)
	s := fileOutput(t, m, "owner", "read-fault", []byte("hello"), nil)
	store.mu.Lock()
	store.readError = errors.New("private disk path")
	store.mu.Unlock()
	page, err := m.Read("owner", s.TaskID, "stdout", 1, 4)
	requireCode(t, err, "output_save_failed")
	if page.Retained != 1 || page.Availability != "unavailable" || !page.Truncated {
		t.Fatalf("unavailable prefix: %+v", page)
	}
	state, _ := m.Status("owner", s.TaskID)
	if state.Persistence != "unavailable" || state.StdoutRetained != 1 {
		t.Fatalf("read failure not reflected: %+v", state)
	}
	store.mu.Lock()
	store.readError = nil
	store.beforeWrite = func(context.Context, string, []byte) error { return failure("output_store_full") }
	store.mu.Unlock()
	blocked, err := m.Start(context.Background(), "owner", "full", StartArgs{CWD: t.TempDir(), Command: "exit 0"})
	requireCode(t, err, "output_limit")
	if !blocked.StartedAt.IsZero() || blocked.PersistenceError != "output_store_full" {
		t.Fatalf("no output capacity launched: %+v", blocked)
	}
}
