package agentcontroller

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
)

type copyJournalFaultStore struct {
	fileArtifactStore
	identity     bool
	failMetadata bool
	failures     int
}

func (s *copyJournalFaultStore) WriteArtifact(ctx context.Context, name string, data []byte) error {
	if s.identity && strings.HasPrefix(name, "result_bi_") || s.failMetadata && strings.HasPrefix(name, "result_bm_") {
		s.failures++
		return &localartifact.Error{Code: "artifact_store_failed"}
	}
	err := s.fileArtifactStore.WriteArtifact(ctx, name, data)
	if !s.identity && err == nil && strings.HasPrefix(name, "result_be_") && bytes.Contains(data, []byte(`"type":"actual"`)) {
		// The segment now physically contains the real outcome; failing the
		// next metadata commit must retain only the older authenticated prefix.
		s.failMetadata = true
	}
	return err
}

func TestRecursiveCopyFailedSettlementRecoversOnlyConfirmedPrefix(t *testing.T) {
	f, c, model := recursiveCopyFixture(t, false)
	fault := &copyJournalFaultStore{fileArtifactStore: fileArtifactStore{c.handle}}
	if err := c.fileBatches.Bind(t.Context(), c.artifactOwner, fault); err != nil {
		t.Fatal(err)
	}
	pending, err := c.Send(t.Context(), "copy")
	if err != nil || pending.PendingFileMutation == nil {
		t.Fatal("prepare", err)
	}
	_, _ = c.ResolveFileMutation(t.Context(), pending.PendingFileMutation.CallID, agentloop.FileMutationApprove)
	if fault.failures != 1 || c.fileJournalErr == nil || c.dirty == nil {
		t.Fatal("missing save failure fence", fault.failures, c.fileJournalErr)
	}
	infos, err := c.fileBatches.List(t.Context(), c.artifactOwner)
	if err != nil || len(infos) != 1 {
		t.Fatal("missing live journal", infos, err)
	}
	id := infos[0].ID
	live, err := c.fileBatches.Status(c.artifactOwner, id)
	if err != nil || live.Started != 1 || live.Completed != 1 || live.SavedBytes >= live.Bytes || live.PersistenceError == "" {
		t.Fatalf("actual result was lost: %+v %v", live, err)
	}
	if entries, err := os.ReadDir(filepath.Join(f.workspace, "target")); err != nil || len(entries) != 0 {
		t.Fatal("did not stop after completed root", entries, err)
	}
	if model.calls != 1 {
		t.Fatal("model continued after partial copy")
	}
	owner, provider := c.SessionID(), c.provider
	c.abort()
	fresh := &recursiveCopyModel{version: model.version, destination: "must-not-replay", callID: model.callID}
	deps := controllerDependencies(f.openStore(t), fresh, f.server, f.workspace, provider)
	deps.LoopOptions.ContextWindow = 32768
	restored, err := Resume(t.Context(), deps, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	status, err := restored.fileBatches.Status(restored.artifactOwner, id)
	if err != nil || !status.Restored || status.Started != 1 || status.Unknown != 1 || status.Completed != 0 || status.Items != 42 {
		t.Fatalf("unconfirmed actual promoted: %+v %v", status, err)
	}
	data := readRecursiveCopyLog(t, restored, id)
	if bytes.Contains(data, []byte(`"type":"actual"`)) || !bytes.Contains(data, []byte(`"type":"pending"`)) {
		t.Fatal("orphan event promoted")
	}
	pending, err = restored.Send(t.Context(), "repeat old call")
	if err != nil || pending.PendingFileMutation == nil {
		t.Fatal("repeat preparation", err)
	}
	_, _ = restored.ResolveFileMutation(t.Context(), pending.PendingFileMutation.CallID, agentloop.FileMutationApprove)
	if _, err := os.Stat(filepath.Join(f.workspace, "must-not-replay")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("recovered call replayed", err)
	}
}

func TestRecursiveCopyWALOnlyIdentitySurvivesConsumption(t *testing.T) {
	f, c, model := recursiveCopyFixture(t, false)
	owner, provider := c.SessionID(), c.provider
	summary := leaseSummary(t, c, owner)
	if _, err := c.RenameSession(t.Context(), owner, "", summary.RecordRevision); err != nil {
		t.Fatal(err)
	}
	fault := &copyJournalFaultStore{fileArtifactStore: fileArtifactStore{c.handle}, identity: true}
	if err := c.fileBatches.Bind(t.Context(), c.artifactOwner, fault); err != nil {
		t.Fatal(err)
	}
	pending, err := c.Send(t.Context(), "copy")
	if err != nil || pending.PendingFileMutation == nil {
		t.Fatal("prepare", err)
	}
	_, _ = c.ResolveFileMutation(t.Context(), pending.PendingFileMutation.CallID, agentloop.FileMutationApprove)
	if c.fileJournalErr == nil || c.dirty == nil || fault.failures != 1 {
		t.Fatal("lost WAL-only failure", c.fileJournalErr, fault.failures)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "target")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("copy ran without journal identity", err)
	}
	c.abort()
	fresh := &recursiveCopyModel{version: model.version, destination: "must-not-replay", callID: model.callID}
	open := func() *Controller {
		t.Helper()
		deps := controllerDependencies(f.openStore(t), fresh, f.server, f.workspace, provider)
		deps.LoopOptions.ContextWindow = 32768
		next, err := Resume(t.Context(), deps, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	restored := open()
	if restored.fileBatches.HasCall(restored.artifactOwner, model.callID) {
		t.Fatal("fixture unexpectedly retained a batch marker")
	}
	name, expected := localCallMarker(model.callID)
	data, err := restored.handle.ReadArtifact(t.Context(), name)
	if err != nil || !bytes.Equal(data, expected) {
		t.Fatal("consumed root WAL lost independent identity", err)
	}
	if err := restored.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	restored = open()
	defer restored.Close()
	// Keep this replay test independent of asynchronous automatic-title work,
	// as in the local-execution WAL recovery fixture.
	summary = leaseSummary(t, restored, owner)
	if _, err := restored.RenameSession(t.Context(), owner, "copy replay test", summary.RecordRevision); err != nil {
		t.Fatal(err)
	}
	pending, err = restored.Send(t.Context(), "repeat old identity")
	if err != nil || pending.PendingFileMutation == nil {
		t.Fatal("repeat preparation", err)
	}
	_, replayErr := restored.ResolveFileMutation(t.Context(), pending.PendingFileMutation.CallID, agentloop.FileMutationApprove)
	if restored.saveFailed != nil {
		_, transcriptErr := agentsession.EncodeTranscript(restored.transcript, restored.limits)
		t.Fatalf("replay rejection must not break persistence: operation=%v save=%v transcript=%v entries=%+v", replayErr, restored.saveFailed, transcriptErr, restored.transcript.Entries)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "must-not-replay")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("consumed WAL replayed copy", err)
	}
	fresh.callID, fresh.destination = "new-copy-identity", "fresh-target"
	pending, err = restored.Send(t.Context(), "explicit new copy")
	if err != nil || pending.PendingFileMutation == nil {
		t.Fatal("new identity blocked", err)
	}
	if _, err := restored.ResolveFileMutation(t.Context(), pending.PendingFileMutation.CallID, agentloop.FileMutationApprove); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(f.workspace, "fresh-target")); err != nil || !info.IsDir() {
		t.Fatal("new copy not executed", err)
	}
}
