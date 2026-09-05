package agentsession

import "github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"

// record-v5/dirty-v4 use the frozen V4 nested shape, with the closed C7
// operation set (including copy, never move). No live effect/endpoint DTO is
// embedded in legacy decoding. Future authenticated versions fail closed.
type recordPayloadV5 struct {
	recordPayloadV1
	FileReceipts []fileReceiptV4 `json:"file_receipts,omitempty"`
}
type dirtyPayloadV4 struct {
	dirtyPayloadV1
	File *fileWriteAheadV3 `json:"file,omitempty"`
}

func upcastEffectV5(v fileEffectV4) (fileeffects.Effect, error) {
	switch v.Operation {
	case "write_create", "write_replace", "edit", "archive", "mkdir", "copy":
	default:
		return fileeffects.Effect{}, ErrCorrupt
	}
	e := fileeffects.Effect{SchemaVersion: v.SchemaVersion, Operation: v.Operation, Source: fileeffects.Endpoint(v.Source), Target: fileeffects.Endpoint(v.Target), Scope: v.Scope, Directories: fileeffects.DirectoryChain(v.Directories)}
	if e.Validate() != nil {
		return fileeffects.Effect{}, ErrCorrupt
	}
	return e, nil
}
func upcastReceiptV5(v fileReceiptV4) (FileReceipt, error) {
	e, err := upcastEffectV5(v.Effect)
	if err != nil {
		return FileReceipt{}, err
	}
	r := FileReceipt{ToolCallID: v.ToolCallID, Effect: e, InvalidateObserved: v.InvalidateObserved, StableCode: v.StableCode, Outcome: v.Outcome}
	if validateFileReceipt(r) != nil {
		return FileReceipt{}, ErrCorrupt
	}
	return r, nil
}
func upcastWriteAheadV4(v fileWriteAheadV3) (FileWriteAhead, error) {
	e, err := upcastEffectV5(v.Effect)
	if err != nil {
		return FileWriteAhead{}, err
	}
	r := FileWriteAhead{ToolCallID: v.ToolCallID, Effect: e, InvalidateObserved: v.InvalidateObserved, StableCode: v.StableCode, PublicationOutcome: v.PublicationOutcome}
	if validateFileWriteAhead(r) != nil {
		return FileWriteAhead{}, ErrCorrupt
	}
	return r, nil
}
