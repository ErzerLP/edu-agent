package nocturne

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

const exportMemoryID = "10000000-0000-4000-8000-000000000001"

type exporterReader struct {
	page memory.RecordPage
	view memory.RecordView
}

func (r exporterReader) ListRecords(context.Context, memory.PageRequest) (memory.RecordPage, error) {
	return r.page, nil
}
func (r exporterReader) Record(context.Context, string) (memory.RecordView, error) {
	return r.view, nil
}

func (*exporterRemote) EnsureParent(context.Context) error { return nil }

type exporterRemote struct {
	node    memory.RemoteNode
	err     error
	path    string
	started chan struct{}
}

func (r *exporterRemote) Health(context.Context) error { return r.err }
func (r *exporterRemote) Capabilities(context.Context) (memory.NocturneCapabilities, error) {
	return memory.NocturneCapabilities{}, r.err
}
func (r *exporterRemote) GetNode(ctx context.Context, path string) (memory.RemoteNode, error) {
	r.path = path
	if r.started != nil {
		close(r.started)
		<-ctx.Done()
		return memory.RemoteNode{}, ctx.Err()
	}
	return r.node, r.err
}
func (*exporterRemote) CreateNode(context.Context, string, string) (memory.RemoteMutation, error) {
	return memory.RemoteMutation{}, errors.New("not implemented")
}
func (*exporterRemote) UpdateNode(context.Context, string, string) (memory.RemoteMutation, error) {
	return memory.RemoteMutation{}, errors.New("not implemented")
}
func (*exporterRemote) DeletePath(context.Context, string) error {
	return errors.New("not implemented")
}
func (*exporterRemote) Search(context.Context, string) ([]memory.RemoteSearchResult, error) {
	return nil, errors.New("not implemented")
}
func (*exporterRemote) ListOrphans(context.Context) ([]memory.RemoteOrphan, error) {
	return nil, errors.New("not implemented")
}
func (*exporterRemote) OrphanDetail(context.Context, int64) (memory.RemoteOrphan, error) {
	return memory.RemoteOrphan{}, errors.New("not implemented")
}
func (*exporterRemote) PermanentDelete(context.Context, int64) (memory.RemoteDeleteResult, error) {
	return memory.RemoteDeleteResult{}, errors.New("not implemented")
}
func (*exporterRemote) References(context.Context, string) (memory.RemoteReferences, error) {
	return memory.RemoteReferences{}, errors.New("not implemented")
}
func (*exporterRemote) ClearReviewReferences(context.Context, string) error {
	return errors.New("not implemented")
}
func (*exporterRemote) Backups(context.Context) (memory.BackupInventory, error) {
	return memory.BackupInventory{}, errors.New("not implemented")
}
func (*exporterRemote) PruneBackups(context.Context, memory.BackupPruneRequest) (memory.BackupPruneResult, error) {
	return memory.BackupPruneResult{}, errors.New("not implemented")
}

func TestMemoryExporterLoadsAppliedContentLiveAndVerifiesHash(t *testing.T) {
	content := "prefers concise examples"
	reader := appliedExporterReader(content)
	remote := &exporterRemote{node: memory.RemoteNode{Content: content}}
	exporter, err := NewMemoryExporter(MemoryExporterOptions{
		Service: reader, Remote: remote, ReadPermits: privacy.NewReadPermitManager(), ParentPath: "edu-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := exporter.Export(context.Background(), memory.PageRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ContentStatus != memory.ExportContentAvailable || page.Items[0].Content != content || page.Degraded {
		t.Fatalf("unexpected live export: %+v", page)
	}
	if remote.path != "edu-agent/"+exportMemoryID {
		t.Fatalf("non-deterministic export path %q", remote.path)
	}

	remote.node.Content = "tampered"
	page, err = exporter.Export(context.Background(), memory.PageRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Items[0].ContentStatus != memory.ExportContentUnavailable || page.Items[0].Content != "" || !page.Degraded || !hasReason(page.ReasonCodes, ExportReasonContentHashMismatch) {
		t.Fatalf("hash mismatch was not fail-closed: %+v", page)
	}
}

func TestMemoryExporterRecordDetailLoadsLiveContentAndDegradesClosed(t *testing.T) {
	content := "prefers concise examples"
	reader := appliedExporterReader(content)
	remote := &exporterRemote{node: memory.RemoteNode{Content: content}}
	exporter, err := NewMemoryExporter(MemoryExporterOptions{
		Service: reader, Remote: remote, ReadPermits: privacy.NewReadPermitManager(), ParentPath: "edu-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := exporter.Detail(context.Background(), exportMemoryID)
	if err != nil || detail.ContentStatus != memory.ExportContentAvailable || detail.Content != content ||
		detail.Record.LogicalMemoryID != exportMemoryID || detail.Receipt.ID == "" {
		t.Fatalf("live detail=%+v err=%v", detail, err)
	}
	remote.err = errors.New("connection refused")
	detail, err = exporter.Detail(context.Background(), exportMemoryID)
	if err != nil || detail.ContentStatus != memory.ExportContentDegraded || detail.Content != "" {
		t.Fatalf("degraded detail=%+v err=%v", detail, err)
	}
	disabled, err := NewMemoryExporter(MemoryExporterOptions{
		Service: reader, ReadPermits: privacy.NewReadPermitManager(), ParentPath: "edu-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	detail, err = disabled.Detail(context.Background(), exportMemoryID)
	if err != nil || detail.ContentStatus != memory.ExportContentUnavailable || detail.Content != "" {
		t.Fatalf("disabled detail=%+v err=%v", detail, err)
	}
}

func TestMemoryExporterRepresentsDisabledAndDownNocturneWithoutContent(t *testing.T) {
	reader := appliedExporterReader("private body")
	permits := privacy.NewReadPermitManager()
	disabled, err := NewMemoryExporter(MemoryExporterOptions{Service: reader, ReadPermits: permits, ParentPath: "edu-agent"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := disabled.Export(context.Background(), memory.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Items[0].ContentStatus != memory.ExportContentUnavailable || page.Items[0].Content != "" ||
		page.Items[0].DeliveryStatus != memory.DeliveryApplied || page.Items[0].Receipt.ID == "" ||
		!hasReason(page.ReasonCodes, ExportReasonNocturneNotConfigured) {
		t.Fatalf("unexpected disabled export: %+v", page)
	}

	down := &exporterRemote{err: errors.New("connection refused")}
	exporter, err := NewMemoryExporter(MemoryExporterOptions{Service: reader, Remote: down, ReadPermits: permits, ParentPath: "edu-agent"})
	if err != nil {
		t.Fatal(err)
	}
	page, err = exporter.Export(context.Background(), memory.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Items[0].ContentStatus != memory.ExportContentDegraded || page.Items[0].Content != "" || !hasReason(page.ReasonCodes, ExportReasonNocturneUnavailable) {
		t.Fatalf("unexpected degraded export: %+v", page)
	}
}

func TestMemoryExporterPermitCancellationNeverPublishesRemoteContent(t *testing.T) {
	permits := privacy.NewReadPermitManager()
	remote := &exporterRemote{started: make(chan struct{})}
	exporter, err := NewMemoryExporter(MemoryExporterOptions{
		Service: appliedExporterReader("private body"), Remote: remote, ReadPermits: permits, ParentPath: "edu-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		page memory.ExportPage
		err  error
	}
	resultChannel := make(chan result, 1)
	go func() {
		page, exportErr := exporter.Export(context.Background(), memory.PageRequest{})
		resultChannel <- result{page: page, err: exportErr}
	}()
	select {
	case <-remote.started:
	case <-time.After(time.Second):
		t.Fatal("remote export did not start")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := permits.CloseAndDrain(closeCtx, 2, privacy.OwnerMemory); err != nil {
		t.Fatal(err)
	}
	resultValue := <-resultChannel
	if memory.ErrorCode(resultValue.err) != memory.CodeContentRedacted || len(resultValue.page.Items) != 0 {
		t.Fatalf("canceled export published a page: page=%+v err=%v", resultValue.page, resultValue.err)
	}
}

func TestMemoryExporterRecordDetailPermitCancellationPublishesNothing(t *testing.T) {
	permits := privacy.NewReadPermitManager()
	remote := &exporterRemote{started: make(chan struct{})}
	exporter, err := NewMemoryExporter(MemoryExporterOptions{
		Service: appliedExporterReader("private body"), Remote: remote, ReadPermits: permits, ParentPath: "edu-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		detail memory.RecordDetail
		err    error
	}
	resultChannel := make(chan result, 1)
	go func() {
		detail, detailErr := exporter.Detail(context.Background(), exportMemoryID)
		resultChannel <- result{detail: detail, err: detailErr}
	}()
	select {
	case <-remote.started:
	case <-time.After(time.Second):
		t.Fatal("remote detail did not start")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := permits.CloseAndDrain(closeCtx, 2, privacy.OwnerMemory); err != nil {
		t.Fatal(err)
	}
	resultValue := <-resultChannel
	if memory.ErrorCode(resultValue.err) != memory.CodeContentRedacted || resultValue.detail.Content != "" ||
		resultValue.detail.Record.LogicalMemoryID != "" {
		t.Fatalf("canceled detail published data: detail=%+v err=%v", resultValue.detail, resultValue.err)
	}
}

func appliedExporterReader(content string) exporterReader {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	record := memory.Record{
		LogicalMemoryID: exportMemoryID, ID: "10000000-0000-4000-8000-000000000002", Revision: 1,
		RecordGeneration: 1, LearnerGeneration: 1, CandidateID: "10000000-0000-4000-8000-000000000003",
		ExternalURI: memory.DeterministicExternalURI(exportMemoryID), ContentHash: memory.SHA256String(content),
		Status: memory.RecordApplied, DeliveryID: "10000000-0000-4000-8000-000000000004",
		ReceiptID: "10000000-0000-4000-8000-000000000005", CreatedAt: now,
	}
	record.ExternalURIDigest = memory.SHA256String(record.ExternalURI)
	generation := memory.GenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}
	return exporterReader{
		page: memory.RecordPage{Items: []memory.Record{record}, ReadGeneration: generation},
		view: memory.RecordView{
			Record: record, Delivery: memory.Delivery{PublicStatus: memory.DeliveryApplied},
			Receipt:        memory.Receipt{ID: record.ReceiptID, DeliveryID: record.DeliveryID, Status: memory.ReceiptSucceeded},
			ReadGeneration: generation,
		},
	}
}

func hasReason(reasons []string, target string) bool {
	for _, reason := range reasons {
		if reason == target {
			return true
		}
	}
	return false
}
