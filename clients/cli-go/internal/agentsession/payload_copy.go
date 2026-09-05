package agentsession

import "github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"

// record-v4/dirty-v3 had a closed operation set. Freeze both shape and allowed
// operations so a new copy effect cannot be smuggled into an older payload.
// Their containers remain v1; future authenticated payloads are not corrupt.
type fileEndpointV4 struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Version string `json:"version,omitempty"`
}
type directoryChainV4 struct {
	Anchor  string `json:"anchor,omitempty"`
	Count   int    `json:"count"`
	Created int    `json:"created"`
}
type fileEffectV4 struct {
	SchemaVersion int              `json:"schema_version"`
	Operation     string           `json:"operation"`
	Source        fileEndpointV4   `json:"source"`
	Target        fileEndpointV4   `json:"target"`
	Scope         string           `json:"scope"`
	Directories   directoryChainV4 `json:"directories"`
}
type fileReceiptV4 struct {
	ToolCallID         string       `json:"tool_call_id"`
	Effect             fileEffectV4 `json:"effect"`
	InvalidateObserved bool         `json:"invalidate_observed"`
	StableCode         string       `json:"stable_code"`
	Outcome            string       `json:"publication_outcome"`
}
type fileWriteAheadV3 struct {
	ToolCallID         string       `json:"tool_call_id"`
	Effect             fileEffectV4 `json:"effect"`
	InvalidateObserved bool         `json:"invalidate_observed"`
	StableCode         string       `json:"stable_code"`
	PublicationOutcome string       `json:"publication_outcome"`
}
type recordPayloadV4 struct {
	recordPayloadV1
	FileReceipts []fileReceiptV4 `json:"file_receipts,omitempty"`
}
type dirtyPayloadV3 struct {
	dirtyPayloadV1
	File *fileWriteAheadV3 `json:"file,omitempty"`
}

func upcastEffectV4(v fileEffectV4) (fileeffects.Effect, error) {
	switch v.Operation {
	case "write_create", "write_replace", "edit", "archive", "mkdir":
	default:
		return fileeffects.Effect{}, ErrCorrupt
	}
	e := fileeffects.Effect{SchemaVersion: v.SchemaVersion, Operation: v.Operation, Source: fileeffects.Endpoint(v.Source), Target: fileeffects.Endpoint(v.Target), Scope: v.Scope, Directories: fileeffects.DirectoryChain(v.Directories)}
	if e.Validate() != nil {
		return fileeffects.Effect{}, ErrCorrupt
	}
	return e, nil
}
func upcastReceiptV4(v fileReceiptV4) (FileReceipt, error) {
	e, err := upcastEffectV4(v.Effect)
	if err != nil {
		return FileReceipt{}, err
	}
	r := FileReceipt{ToolCallID: v.ToolCallID, Effect: e, InvalidateObserved: v.InvalidateObserved, StableCode: v.StableCode, Outcome: v.Outcome}
	if validateFileReceipt(r) != nil {
		return FileReceipt{}, ErrCorrupt
	}
	return r, nil
}
func upcastWriteAheadV3(v fileWriteAheadV3) (FileWriteAhead, error) {
	e, err := upcastEffectV4(v.Effect)
	if err != nil {
		return FileWriteAhead{}, err
	}
	r := FileWriteAhead{ToolCallID: v.ToolCallID, Effect: e, InvalidateObserved: v.InvalidateObserved, StableCode: v.StableCode, PublicationOutcome: v.PublicationOutcome}
	if validateFileWriteAhead(r) != nil {
		return FileWriteAhead{}, ErrCorrupt
	}
	return r, nil
}
