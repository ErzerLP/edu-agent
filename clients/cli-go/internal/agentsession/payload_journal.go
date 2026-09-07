package agentsession

// Dirty v5 is frozen independently of the live journal. All nested effect
// fields use the already frozen C6 shape, with the C8 closed operation set.
type dirtyPayloadV5 struct {
	dirtyPayloadV1
	File *fileWriteAheadV3 `json:"file,omitempty"`
}

func upcastWriteAheadV5(v fileWriteAheadV3) (FileWriteAhead, error) {
	switch v.Effect.Operation {
	case "write_create", "write_replace", "edit", "archive", "mkdir", "copy", "move":
	default:
		return FileWriteAhead{}, ErrCorrupt
	}
	e, err := upcastEffectV6(v.Effect)
	if err != nil {
		return FileWriteAhead{}, err
	}
	r := FileWriteAhead{ToolCallID: v.ToolCallID, Effect: e, InvalidateObserved: v.InvalidateObserved, StableCode: v.StableCode, PublicationOutcome: v.PublicationOutcome}
	if validateFileWriteAhead(r) != nil {
		return FileWriteAhead{}, ErrCorrupt
	}
	return r, nil
}
