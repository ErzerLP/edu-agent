package agentcontroller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/filelock"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type leaseTestModel struct{}

func (*leaseTestModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	if isTitleRequest(request) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"后台任务"}`}}, nil
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role == "user" && last.Content == "launch" {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{
			ID: "lease-shell", Type: "function", Function: modelclient.ToolFunction{Name: "shell", Arguments: `{"command":"printf lease-ready; while IFS= read -r line; do printf '%s\\n' \"$line\"; done","stdin":true,"wait_ms":30}`},
		}}}}, nil
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "done"}}, nil
}

type leaseTestServer struct {
	controllerServer
	mu   sync.Mutex
	hook func()
}

func (s *leaseTestServer) ExportMemory(ctx context.Context, cursor string, limit int) (api.MemoryExportPage, error) {
	s.mu.Lock()
	hook := s.hook
	s.hook = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return s.controllerServer.ExportMemory(ctx, cursor, limit)
}

type leaseFixture struct {
	controller *Controller
	manager    *localexec.Manager
	server     *leaseTestServer
	secrets    *controllerSecretBackend
	root       string
	workspace  string
}

func newLeaseFixture(t *testing.T, noSave bool) *leaseFixture {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("real shell leases require Linux/macOS")
	}
	t.Setenv("SHELL", "/bin/sh")
	f := &leaseFixture{root: t.TempDir(), workspace: t.TempDir(), secrets: &controllerSecretBackend{}, manager: localexec.New(localexec.Options{StopGrace: 40 * time.Millisecond})}
	f.server = &leaseTestServer{controllerServer: controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}}
	var store *agentsession.Store
	if !noSave {
		store = f.openStore(t)
	}
	dependencies := controllerDependencies(store, &leaseTestModel{}, f.server, f.workspace, Provider{Name: "local", Endpoint: "http://localhost:11434/v1", Model: "test"})
	dependencies.LoopOptions.ContextWindow = 32768
	dependencies.LoopOptions.LocalExec = f.manager
	var err error
	f.controller, err = Start(t.Context(), dependencies, noSave)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.controller.Close)
	return f
}

func (f *leaseFixture) openStore(t *testing.T) *agentsession.Store {
	t.Helper()
	profile, err := agentsession.ProfileFingerprint("https://server.example/api")
	if err != nil {
		t.Fatal(err)
	}
	store, err := agentsession.Open(t.Context(), agentsession.Options{Root: f.root, ProfileFingerprint: profile, Secrets: f.secrets, LockTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func leaseLaunch(t *testing.T, c *Controller) localexec.Snapshot {
	t.Helper()
	// Keep revision assertions independent of the unrelated automatic title job.
	if c.Status().Persistent {
		summary := leaseSummary(t, c, c.SessionID())
		if _, err := c.RenameSession(t.Context(), summary.SessionID, "lease test", summary.RecordRevision); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Send(t.Context(), "launch"); err != nil {
		t.Fatal(err)
	}
	tasks, err := c.LocalTasks()
	if err != nil || len(tasks) != 1 || !tasks[0].Controllable || tasks[0].State != localexec.StateRunning {
		t.Fatalf("launch tasks=%+v err=%v", tasks, err)
	}
	return tasks[0]
}

func leaseSummary(t *testing.T, c *Controller, id string) agentsession.Summary {
	t.Helper()
	items, err := c.ListSessions(t.Context(), SessionListRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Summary.SessionID == id {
			return item.Summary
		}
	}
	t.Fatalf("missing Session %s", id)
	return agentsession.Summary{}
}

func leaseAssertLocked(t *testing.T, store *agentsession.Store, summary agentsession.Summary) {
	t.Helper()
	if handle, _, err := store.OpenSession(t.Context(), summary.SessionID); !errors.Is(err, agentsession.ErrInUse) {
		if handle != nil {
			_ = handle.Close()
		}
		t.Fatalf("external restore escaped lease: %v", err)
	}
	if err := store.Delete(t.Context(), agentsession.DeleteTarget{SessionID: summary.SessionID, StorageID: summary.StorageID, ExpectedRecordRevision: summary.RecordRevision}); !errors.Is(err, agentsession.ErrInUse) {
		t.Fatalf("external delete escaped lease: %v", err)
	}
}

func leaseAssertRunning(t *testing.T, f *leaseFixture, owner, id string) {
	t.Helper()
	task, err := f.manager.Status(owner, id)
	if err != nil || !task.Controllable || task.State != localexec.StateRunning {
		t.Fatalf("background task changed: %+v err=%v", task, err)
	}
}

func TestLocalSessionLeaseSwitchRoundTrip(t *testing.T) {
	f := newLeaseFixture(t, false)
	c := f.controller
	a := c.SessionID()
	task := leaseLaunch(t, c)
	if err := c.SetFileAuthorizationMode(agentloop.FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	original := c.localSessionLease
	c.mu.Unlock()
	if _, err := c.NewSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	b := c.SessionID()
	if a == b || !c.SwitchGate().Allowed || c.FileAuthorizationMode() != agentloop.FileAuthorizationConfirm {
		t.Fatal("idle switch did not install fresh target")
	}
	if tasks, err := c.LocalTasks(); err != nil || len(tasks) != 0 {
		t.Fatalf("B acquired A's tasks: %+v %v", tasks, err)
	}
	leaseAssertRunning(t, f, a, task.TaskID)
	summary := leaseSummary(t, c, a)
	if summary.Locked {
		t.Fatal("own parked Session is not selectable")
	}
	external := f.openStore(t)
	defer external.Close()
	leaseAssertLocked(t, external, summary)
	items, err := NewSelector(external, "", c.provider).ListSessions(t.Context(), SessionListRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Summary.SessionID == a && !item.Summary.Locked {
			t.Fatal("external selector hid the held lock")
		}
	}
	for _, target := range []agentsession.DeleteTarget{{SessionID: a, ExpectedRecordRevision: summary.RecordRevision}, {StorageID: summary.StorageID}} {
		if err := c.DeleteSession(t.Context(), target); !errors.Is(err, agentsession.ErrInUse) {
			t.Fatalf("local parked deletion: %v", err)
		}
	}
	// Changing provider must still install only a local view until confirmation.
	c.mu.Lock()
	c.provider.Endpoint = "https://different.example/v1"
	c.mu.Unlock()
	plan := c.PlanSwitch(summary)
	if !plan.NeedProviderConfirm {
		t.Fatal("missing provider confirmation plan")
	}
	if _, err := c.CommitSwitch(t.Context(), plan, SwitchConfirmation{}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	if c.localSessionLease != original || c.parkedLocalSessions[a] != nil {
		t.Error("switch did not transfer the same handle lease")
	}
	c.mu.Unlock()
	if c.SessionID() != a || !c.Status().ProviderConfirmationRequired || c.FileAuthorizationMode() != agentloop.FileAuthorizationConfirm {
		t.Fatal("restore gates changed")
	}
	if _, err := c.Send(t.Context(), "must not transmit"); !errors.Is(err, ErrProviderConfirmationRequired) {
		t.Fatalf("provider gate bypass: %v", err)
	}
	tasks, err := c.LocalTasks()
	if err != nil || len(tasks) != 1 || tasks[0].TaskID != task.TaskID {
		t.Fatalf("task identity lost: %+v err=%v", tasks, err)
	}
	page, err := c.ReadLocalTask(task.TaskID, "stdout", 0, 1024)
	if err != nil || !strings.Contains(string(page.Data), "lease-ready") {
		t.Fatalf("retained output inaccessible: %+v err=%v", page, err)
	}
	stopped, err := c.StopLocalTask(t.Context(), task.TaskID)
	if err != nil || localTaskUnsettled(stopped) {
		t.Fatalf("stop after restore: %+v err=%v", stopped, err)
	}
	leaseAssertLocked(t, external, leaseSummary(t, c, a))
	if err := c.ConfirmProvider(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestLocalSessionLeaseFailedPreflightKeepsOwner(t *testing.T) {
	for _, failure := range []string{"revision", "cancel", "workspace"} {
		t.Run(failure, func(t *testing.T) {
			f := newLeaseFixture(t, false)
			c := f.controller
			a := c.SessionID()
			task := leaseLaunch(t, c)
			if _, err := c.NewSession(t.Context()); err != nil {
				t.Fatal(err)
			}
			b, generation := c.SessionID(), c.Generation()
			summary := leaseSummary(t, c, a)
			plan := c.PlanSwitch(summary)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := agentsession.ErrCheckpointConflict
			switch failure {
			case "revision":
				plan.ExpectedRevision++
			case "cancel":
				f.server.mu.Lock()
				f.server.hook = cancel // Cancellation after Handle.Load and target construction.
				f.server.mu.Unlock()
				want = context.Canceled
			case "workspace":
				c.mu.Lock()
				c.record.WorkspaceRoot = t.TempDir() // Force normal Resume's actual binding preflight.
				c.mu.Unlock()
				want = ErrWorkspaceConfirmationRequired
			}
			if _, err := c.CommitSwitch(ctx, plan, SwitchConfirmation{}); !errors.Is(err, want) {
				t.Fatalf("failure=%s err=%v want=%v", failure, err, want)
			}
			if c.SessionID() != b || c.Generation() != generation || !c.SwitchGate().Allowed {
				t.Fatal("failed preflight replaced or blocked current Session")
			}
			leaseAssertRunning(t, f, a, task.TaskID)
			external := f.openStore(t)
			defer external.Close()
			leaseAssertLocked(t, external, summary)
			c.mu.Lock()
			lease := c.parkedLocalSessions[a]
			c.mu.Unlock()
			lease.mu.Lock()
			refs := lease.refs
			lease.mu.Unlock()
			if refs != 1 {
				t.Fatalf("failed target retained %d lease references", refs)
			}
			// The same task can still be activated, with workspace confirmation if needed.
			if _, err := c.CommitSwitch(t.Context(), c.PlanSwitch(summary), SwitchConfirmation{Workspace: true}); err != nil {
				t.Fatal(err)
			}
			leaseAssertRunning(t, f, a, task.TaskID)
		})
	}
}

func TestLocalSessionLeaseReaperReleasesSettledOwner(t *testing.T) {
	f := newLeaseFixture(t, false)
	c := f.controller
	a := c.SessionID()
	task := leaseLaunch(t, c)
	c.mu.Lock()
	lease := c.localSessionLease
	c.mu.Unlock()
	if _, err := c.NewSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.CloseInput(a, task.TaskID); err != nil {
		t.Fatal(err)
	}
	settled, err := f.manager.Wait(t.Context(), a, task.TaskID, 2*time.Second)
	if err != nil || localTaskUnsettled(settled) {
		t.Fatalf("natural settlement=%+v err=%v", settled, err)
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		c.mu.Lock()
		reaped := c.parkedLocalSessions[a] == nil && c.localLeaseReaperCancel == nil
		c.mu.Unlock()
		if reaped {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("idle lease/reaper retained after the bounded release point")
		case <-ticker.C:
		}
	}
	if _, err := lease.handle.Load(); !errors.Is(err, agentsession.ErrNotFound) {
		t.Fatalf("reaped handle remains open: %v", err)
	}
	if peer, err := lease.store.Reopen(t.Context()); !errors.Is(err, agentsession.ErrKeyUnavailable) {
		if peer != nil {
			_ = peer.Close()
		}
		t.Fatalf("reaped store remains open: %v", err)
	}
	external := f.openStore(t)
	defer external.Close()
	handle, loaded, err := external.OpenSession(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	_ = handle.Close()
	// Reopening the settled Session still exposes its original in-process output.
	if _, err := c.CommitSwitch(t.Context(), c.PlanSwitch(summaryFromRecord(loaded.Record)), SwitchConfirmation{}); err != nil {
		t.Fatal(err)
	}
	if tasks, err := c.LocalTasks(); err != nil || len(tasks) != 1 || tasks[0].TaskID != task.TaskID || localTaskUnsettled(tasks[0]) {
		t.Fatalf("settled task lost after ordinary reopen: %+v %v", tasks, err)
	}
}

func TestLocalSessionLeasePrivacyInvalidationCannotBorrow(t *testing.T) {
	f := newLeaseFixture(t, false)
	c := f.controller
	a := c.SessionID()
	task := leaseLaunch(t, c)
	if _, err := c.NewSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	summary := leaseSummary(t, c, a)
	plan := c.PlanSwitch(summary)
	external := f.openStore(t)
	defer external.Close()
	if err := external.Clear(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CommitSwitch(t.Context(), plan, SwitchConfirmation{}); !errors.Is(err, agentsession.ErrPrivacyInvalidated) {
		t.Fatalf("borrow bypassed profile generation: %v", err)
	}
	leaseAssertRunning(t, f, a, task.TaskID)
	lock, err := filelock.Acquire(t.Context(), filepath.Join(f.root, "session-"+summary.StorageID+".lock"), filelock.Exclusive, 0)
	if !errors.Is(err, filelock.ErrBusy) {
		if lock != nil {
			_ = lock.Close()
		}
		t.Fatalf("invalidated preflight released running owner lock: %v", err)
	}
	c.abort() // Privacy-invalidated stores cannot publish a final checkpoint.
	status, _ := f.manager.Status(a, task.TaskID)
	if localTaskUnsettled(status) {
		t.Fatal("abort left owned task running")
	}
}

func TestLocalSessionLeaseShutdownStopsAllOwnersBeforeRelease(t *testing.T) {
	f := newLeaseFixture(t, false)
	c := f.controller
	tasks := make(map[string]string)
	leases := make([]*localSessionLease, 0, 3)
	for i := 0; i < 3; i++ {
		task := leaseLaunch(t, c)
		tasks[c.SessionID()] = task.TaskID
		c.mu.Lock()
		leases = append(leases, c.localSessionLease)
		c.mu.Unlock()
		if i < 2 {
			if _, err := c.NewSession(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	external := f.openStore(t)
	defer external.Close()
	for owner, id := range tasks {
		status, err := f.manager.Status(owner, id)
		if err != nil || localTaskUnsettled(status) {
			t.Fatalf("shutdown task=%+v err=%v", status, err)
		}
		handle, _, err := external.OpenSession(t.Context(), owner)
		if err != nil {
			t.Fatalf("shutdown retained Session %s: %v", owner, err)
		}
		_ = handle.Close()
	}
	for _, lease := range leases {
		if _, err := lease.handle.Load(); !errors.Is(err, agentsession.ErrNotFound) {
			t.Fatalf("shutdown retained handle: %v", err)
		}
		if peer, err := lease.store.Reopen(t.Context()); !errors.Is(err, agentsession.ErrKeyUnavailable) {
			if peer != nil {
				_ = peer.Close()
			}
			t.Fatalf("shutdown retained store: %v", err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.parkedLocalSessions) != 0 || c.localLeaseReaperCancel != nil || c.localSessionLease != nil {
		t.Fatal("shutdown retained resource bookkeeping")
	}
}

func TestLocalSessionLeaseNoSaveAndCanceledShutdown(t *testing.T) {
	f := newLeaseFixture(t, true)
	task := leaseLaunch(t, f.controller)
	owner := f.controller.localOwner
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_ = f.controller.Shutdown(ctx) // Caller cancellation must not cancel resource settlement.
	status, err := f.manager.Status(owner, task.TaskID)
	if err != nil || localTaskUnsettled(status) {
		t.Fatalf("canceled shutdown released before settlement: %+v err=%v", status, err)
	}
	entries, err := os.ReadDir(f.root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("no-save produced Session files: %+v err=%v", entries, err)
	}
	if f.controller.localSessionLease != nil || len(f.controller.parkedLocalSessions) != 0 {
		t.Fatal("no-save allocated persistent leases")
	}
}

func TestLocalSessionLeaseSaveDegradationKeepsRunningOwner(t *testing.T) {
	f := newLeaseFixture(t, false)
	c := f.controller
	a := c.SessionID()
	task := leaseLaunch(t, c)
	summary := leaseSummary(t, c, a)
	c.mu.Lock()
	lease := c.localSessionLease
	if c.dirty != nil {
		t.Error("test requires a task from an already committed turn")
	}
	err := c.degradeNewSessionAfterSaveFailureLocked(agentsession.ErrCheckpointSaveFailed)
	retained := c.persistent && c.localSessionLease == lease
	c.mu.Unlock()
	if err != nil || !retained {
		t.Fatalf("save degradation released running owner: %v", err)
	}
	external := f.openStore(t)
	defer external.Close()
	leaseAssertLocked(t, external, summary)
	if _, err := c.StopLocalTask(t.Context(), task.TaskID); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	err = c.degradeNewSessionAfterSaveFailureLocked(agentsession.ErrCheckpointSaveFailed)
	degraded := !c.persistent && c.localSessionLease == nil
	c.mu.Unlock()
	if err != nil || !degraded {
		t.Fatalf("settled owner did not release during degradation: %v", err)
	}
	handle, _, err := external.OpenSession(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	_ = handle.Close()
}

func TestLocalSessionLeaseUnsettledIncludesLaunchAndCleanup(t *testing.T) {
	for _, state := range []string{localexec.StateStarting, localexec.StateRunning, localexec.StateStopping, localexec.StateFinishing} {
		if !localTaskUnsettled(localexec.Snapshot{State: state}) {
			t.Errorf("released %s before retirement", state)
		}
	}
	if !localTaskUnsettled(localexec.Snapshot{State: localexec.StateExited, Controllable: true}) || localTaskUnsettled(localexec.Snapshot{State: localexec.StateUnknown}) {
		t.Fatal("supervisor retirement was confused with shell exit")
	}
}
