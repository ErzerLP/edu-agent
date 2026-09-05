package agentsession

import "github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"

func validateLegacyV1Receipts(receipts []fileReceiptV1) error {
	for _, v := range receipts {
		if validateLegacyFileReceipt(fileReceiptV3{ToolCallID: v.ToolCallID, Operation: v.Operation, Path: v.Path, Kind: v.Kind, ContentHash: v.ContentHash, InvalidateObserved: v.InvalidateObserved, StableCode: v.StableCode, Outcome: v.Outcome}) != nil {
			return ErrCorrupt
		}
	}
	return nil
}

func upcastFileReceipt(v fileReceiptV3) FileReceipt {
	source, target := "", v.Path
	if v.Operation == "archive" {
		source, target = v.Path, v.ArchivePath
	}
	e := fileeffects.New(v.Operation, source, target, v.Kind)
	e.Target.Version = v.ContentHash // Only an actually recorded raw-byte hash.
	return FileReceipt{ToolCallID: v.ToolCallID, Effect: e, InvalidateObserved: v.InvalidateObserved, StableCode: v.StableCode, Outcome: v.Outcome}
}
func upcastFileWriteAhead(v fileWriteAheadV2) FileWriteAhead {
	r := upcastFileReceipt(fileReceiptV3{ToolCallID: v.ToolCallID, Operation: v.Operation, Path: v.Path, ArchivePath: v.ArchivePath, Kind: v.Kind, ContentHash: v.ContentHash, InvalidateObserved: v.InvalidateObserved, StableCode: v.StableCode, Outcome: v.PublicationOutcome})
	return FileWriteAhead{ToolCallID: r.ToolCallID, Effect: r.Effect, InvalidateObserved: r.InvalidateObserved, StableCode: r.StableCode, PublicationOutcome: r.Outcome}
}
