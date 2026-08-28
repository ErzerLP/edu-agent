package nocturne

import (
	"context"
	"errors"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

const (
	ExportReasonNocturneNotConfigured = "nocturne_not_configured"
	ExportReasonNocturneUnavailable   = "nocturne_unavailable"
	ExportReasonRecordChanged         = "record_changed_during_export"
	ExportReasonContentHashMismatch   = "content_hash_mismatch"
)

// MemoryExportReader is deliberately the public memory service surface. The
// exporter never reads persistence tables or retains remote content.
type MemoryExportReader interface {
	ListRecords(context.Context, memory.PageRequest) (memory.RecordPage, error)
	Record(context.Context, string) (memory.RecordView, error)
}

type MemoryExporterOptions struct {
	Service     MemoryExportReader
	Remote      memory.NocturneRemote
	ReadPermits *privacy.ReadPermitManager
	ParentPath  string
}

// MemoryExporter loads current metadata from the memory service and fetches
// applied content live from Nocturne. A nil remote is the supported disabled
// mode and is represented in the DTO instead of failing the HTTP request.
type MemoryExporter struct {
	service     MemoryExportReader
	remote      memory.NocturneRemote
	readPermits *privacy.ReadPermitManager
	parentPath  string
}

func NewMemoryExporter(options MemoryExporterOptions) (*MemoryExporter, error) {
	if options.Service == nil || options.ReadPermits == nil || strings.Trim(options.ParentPath, "/") != options.ParentPath || options.ParentPath == "" {
		return nil, errors.New("memory exporter requires a service, read permits, and fixed parent path")
	}
	return &MemoryExporter{
		service: options.Service, remote: options.Remote, readPermits: options.ReadPermits,
		parentPath: options.ParentPath,
	}, nil
}

func (e *MemoryExporter) Detail(ctx context.Context, logicalMemoryID string) (memory.RecordDetail, error) {
	permit, err := e.readPermits.Acquire(ctx, privacy.OwnerMemory)
	if err != nil {
		return memory.RecordDetail{}, exportReadError(err)
	}
	defer permit.Release()

	view, err := e.service.Record(permit.Context(), logicalMemoryID)
	if err != nil {
		return memory.RecordDetail{}, err
	}
	if cause := context.Cause(permit.Context()); cause != nil {
		return memory.RecordDetail{}, exportReadError(cause)
	}
	detail := memory.RecordDetail{
		Record: view.Record, Delivery: view.Delivery, Receipt: view.Receipt,
		ReadGeneration: view.ReadGeneration, ContentStatus: memory.ExportContentUnavailable,
	}
	if view.Record.Status != memory.RecordApplied || view.Delivery.PublicStatus != memory.DeliveryApplied || e.remote == nil {
		return detail, nil
	}
	node, remoteErr := e.remote.GetNode(permit.Context(), e.parentPath+"/"+view.Record.LogicalMemoryID)
	if cause := context.Cause(permit.Context()); cause != nil {
		return memory.RecordDetail{}, exportReadError(cause)
	}
	if remoteErr != nil {
		detail.ContentStatus = memory.ExportContentDegraded
		return detail, nil
	}
	if memory.SHA256String(node.Content) != view.Record.ContentHash {
		return detail, nil
	}
	if cause := context.Cause(permit.Context()); cause != nil {
		return memory.RecordDetail{}, exportReadError(cause)
	}
	detail.ContentStatus = memory.ExportContentAvailable
	detail.Content = node.Content
	return detail, nil
}

func (e *MemoryExporter) Export(ctx context.Context, request memory.PageRequest) (memory.ExportPage, error) {
	permit, err := e.readPermits.Acquire(ctx, privacy.OwnerMemory)
	if err != nil {
		return memory.ExportPage{}, exportReadError(err)
	}
	defer permit.Release()

	records, err := e.service.ListRecords(permit.Context(), request)
	if err != nil {
		return memory.ExportPage{}, err
	}
	if cause := context.Cause(permit.Context()); cause != nil {
		return memory.ExportPage{}, exportReadError(cause)
	}
	result := memory.ExportPage{
		Items: recordsToExportItems(records.Items), NextCursor: records.NextCursor,
		ReadGeneration: records.ReadGeneration, ReasonCodes: []string{},
	}
	if e.remote == nil {
		result.Degraded = true
		result.ReasonCodes = append(result.ReasonCodes, ExportReasonNocturneNotConfigured)
	}

	for index, listed := range records.Items {
		view, recordErr := e.service.Record(permit.Context(), listed.LogicalMemoryID)
		if recordErr != nil {
			if context.Cause(permit.Context()) != nil || memory.ErrorCode(recordErr) == memory.CodeContentRedacted {
				return memory.ExportPage{}, exportReadError(firstExportCause(permit.Context(), recordErr))
			}
			result.Items[index].ContentStatus = memory.ExportContentDegraded
			result.Degraded = true
			appendExportReason(&result, ExportReasonRecordChanged)
			continue
		}
		if cause := context.Cause(permit.Context()); cause != nil {
			return memory.ExportPage{}, exportReadError(cause)
		}
		result.Items[index].DeliveryStatus = view.Delivery.PublicStatus
		result.Items[index].Receipt = view.Receipt
		if !sameExportRecord(listed, view.Record) || view.ReadGeneration != records.ReadGeneration {
			result.Items[index].ContentStatus = memory.ExportContentUnavailable
			result.Degraded = true
			appendExportReason(&result, ExportReasonRecordChanged)
			continue
		}
		if view.Record.Status != memory.RecordApplied || view.Delivery.PublicStatus != memory.DeliveryApplied || e.remote == nil {
			result.Items[index].ContentStatus = memory.ExportContentUnavailable
			continue
		}

		node, remoteErr := e.remote.GetNode(permit.Context(), e.parentPath+"/"+view.Record.LogicalMemoryID)
		if cause := context.Cause(permit.Context()); cause != nil {
			return memory.ExportPage{}, exportReadError(cause)
		}
		if remoteErr != nil {
			result.Items[index].ContentStatus = memory.ExportContentDegraded
			result.Degraded = true
			appendExportReason(&result, ExportReasonNocturneUnavailable)
			continue
		}
		if memory.SHA256String(node.Content) != view.Record.ContentHash {
			result.Items[index].ContentStatus = memory.ExportContentUnavailable
			result.Degraded = true
			appendExportReason(&result, ExportReasonContentHashMismatch)
			continue
		}
		if cause := context.Cause(permit.Context()); cause != nil {
			return memory.ExportPage{}, exportReadError(cause)
		}
		result.Items[index].ContentStatus = memory.ExportContentAvailable
		result.Items[index].Content = node.Content
	}
	return result, nil
}

func recordsToExportItems(records []memory.Record) []memory.ExportItem {
	items := make([]memory.ExportItem, len(records))
	for index, record := range records {
		items[index] = memory.ExportItem{Record: record, DeliveryStatus: memory.DeliveryQueued, ContentStatus: memory.ExportContentUnavailable}
	}
	return items
}

func sameExportRecord(listed, current memory.Record) bool {
	return listed.LogicalMemoryID == current.LogicalMemoryID && listed.ID == current.ID &&
		listed.Revision == current.Revision && listed.RecordGeneration == current.RecordGeneration &&
		listed.LearnerGeneration == current.LearnerGeneration && listed.Status == current.Status &&
		listed.DeliveryID == current.DeliveryID && listed.ReceiptID == current.ReceiptID &&
		listed.ExternalURI == current.ExternalURI && listed.ContentHash == current.ContentHash
}

func appendExportReason(page *memory.ExportPage, reason string) {
	for _, existing := range page.ReasonCodes {
		if existing == reason {
			return
		}
	}
	page.ReasonCodes = append(page.ReasonCodes, reason)
}

func firstExportCause(ctx context.Context, fallback error) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return fallback
}

func exportReadError(err error) error {
	if memory.ErrorCode(err) == memory.CodeContentRedacted {
		return err
	}
	return &memory.Error{Code: memory.CodeContentRedacted, Reason: "memory_export_read_gate_closed", Cause: err}
}

var _ interface {
	Detail(context.Context, string) (memory.RecordDetail, error)
	Export(context.Context, memory.PageRequest) (memory.ExportPage, error)
} = (*MemoryExporter)(nil)
