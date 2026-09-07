package agentsession

import (
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
)

// Record v6 and dirty v7 accepted only effect v1. Reuse the frozen nested
// shapes, never the live record, marker, effect or local-intent DTOs. The new
// top-level versions change semantics, not fields or encrypted containers.
type recordPayloadV6 struct {
	recordPayloadV1
	FileReceipts []fileReceiptV4 `json:"file_receipts,omitempty"`
}

type dirtyPayloadV7 struct {
	dirtyPayloadV6
	LocalEffects []localEffectIntentV7 `json:"local_effects,omitempty"`
}

type localEffectIntentV7 struct {
	ToolCallID string `json:"tool_call_id"`
	Operation  string `json:"operation"`
	TaskID     string `json:"task_id,omitempty"`
}

func upcastReceiptV6(v fileReceiptV4) (FileReceipt, error) {
	e, err := upcastEffectV6(v.Effect)
	if err != nil {
		return FileReceipt{}, err
	}
	r := FileReceipt{ToolCallID: v.ToolCallID, Effect: e, InvalidateObserved: v.InvalidateObserved, StableCode: v.StableCode, Outcome: v.Outcome}
	if validateFileReceipt(r) != nil {
		return FileReceipt{}, ErrCorrupt
	}
	return r, nil
}

func upcastDirtyV7(v dirtyPayloadV7) (DirtyMarker, error) {
	if v.SchemaVersion != 7 {
		return DirtyMarker{}, ErrCorrupt
	}
	m, err := upcastDirtyFileFactsV6(v.dirtyPayloadV6)
	if err != nil {
		return DirtyMarker{}, err
	}
	m.LocalEffects, err = upcastLocalEffectsV7(v.LocalEffects)
	if err != nil || validateDirtyMarker(m) != nil {
		return DirtyMarker{}, ErrCorrupt
	}
	return m, nil
}

// validateFileEffectV1 freezes the original effect-v1 semantic contract for
// every legacy upcaster. Do not replace this with live Effect.Validate: later
// versions/operations must not legalize previously invalid persisted facts.
// Earlier payloads further restrict the operation set in their own upcasters.
func validateFileEffectV1(e fileEffectV4) error {
	if e.SchemaVersion != 1 || !fileeffects.ValidPath(e.Target.Path, false) || !fileeffects.ValidVersion(e.Target.Version) || (e.Target.Kind != "file" && e.Target.Kind != "directory") {
		return ErrCorrupt
	}
	if e.Scope != "entry" && e.Scope != "subtree" {
		return ErrCorrupt
	}
	switch e.Operation {
	case "move":
		if !fileeffects.ValidPath(e.Source.Path, false) || fileeffects.Protected(e.Source.Path) || fileeffects.Protected(e.Target.Path) || strings.EqualFold(e.Source.Path, e.Target.Path) || e.Source.Kind != e.Target.Kind || !strings.HasPrefix(e.Source.Version, "entry-v1:") || !fileeffects.ValidVersion(e.Source.Version) || e.Target.Version != "" || e.Directories != (directoryChainV4{}) {
			return ErrCorrupt
		}
		if e.Target.Kind == "directory" && (e.Scope != "subtree" || strings.HasPrefix(e.Target.Path, e.Source.Path+"/")) || e.Target.Kind == "file" && e.Scope != "entry" {
			return ErrCorrupt
		}
	case "copy":
		if !fileeffects.ValidPath(e.Source.Path, false) || fileeffects.Protected(e.Source.Path) || fileeffects.Protected(e.Target.Path) || e.Source.Path == e.Target.Path || e.Source.Kind != "file" || e.Target.Kind != "file" || e.Scope != "entry" || !strings.HasPrefix(e.Source.Version, "entry-v1:") || !fileeffects.ValidVersion(e.Source.Version) || strings.HasPrefix(e.Target.Version, "entry-v1:") || e.Directories != (directoryChainV4{}) {
			return ErrCorrupt
		}
	case "mkdir":
		if e.Source != (fileEndpointV4{}) || e.Target.Kind != "directory" || e.Scope != "subtree" || fileeffects.Protected(e.Target.Path) || e.Target.Version != "" {
			return ErrCorrupt
		}
		d := e.Directories
		if !fileeffects.ValidPath(d.Anchor, true) || d.Count < 1 || d.Count > 64 || d.Created < 0 || d.Created > d.Count {
			return ErrCorrupt
		}
		parts := strings.Split(e.Target.Path, "/")
		depth := len(parts) - d.Count
		if depth < 0 {
			return ErrCorrupt
		}
		anchor := "."
		if depth > 0 {
			anchor = strings.Join(parts[:depth], "/")
		}
		if anchor != d.Anchor {
			return ErrCorrupt
		}
	case "archive":
		if !fileeffects.ValidPath(e.Source.Path, false) || fileeffects.Protected(e.Source.Path) || e.Source.Kind != e.Target.Kind || !fileeffects.ValidVersion(e.Source.Version) || strings.HasPrefix(e.Source.Version, "sha256:") || e.Target.Version != "" || e.Directories != (directoryChainV4{}) {
			return ErrCorrupt
		}
		parts := strings.SplitN(e.Target.Path, "/", 3)
		if len(parts) != 3 || parts[0] != fileeffects.ArchiveDirectory || parts[2] != e.Source.Path {
			return ErrCorrupt
		}
		stamp, id, ok := strings.Cut(parts[1], "-")
		if !ok || stamp == "" || id == "" {
			return ErrCorrupt
		}
		for _, r := range parts[1] {
			if r != '-' && r != '_' && r != '.' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
				return ErrCorrupt
			}
		}
		if e.Target.Kind == "directory" && e.Scope != "subtree" || e.Target.Kind == "file" && e.Scope != "entry" {
			return ErrCorrupt
		}
	case "write_create", "write_replace", "edit":
		if e.Source != (fileEndpointV4{}) || e.Target.Kind != "file" || e.Scope != "entry" || e.Directories != (directoryChainV4{}) || strings.HasPrefix(e.Target.Version, "entry-v1:") {
			return ErrCorrupt
		}
	default:
		return ErrCorrupt
	}
	return nil
}
