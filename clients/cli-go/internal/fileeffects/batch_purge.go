package fileeffects

import "strings"

// Purge plans are raw-path bytewise descending, unlike the frozen v1 copy
// preorder. Every child precedes its parent; the exact root is the final item.
// Case-folded keys reject portable aliases without normalizing stored spelling.
func (v *batchPlanValidator) addPurge(index int, item BatchItem) error {
	s, t := item.Source, item.Target
	if index != len(v.paths) || !ValidPath(s.Path, false) || (s.Kind != "file" && s.Kind != "directory" && s.Kind != "link") || !strings.HasPrefix(s.Version, "entry-v1:") || !ValidVersion(s.Version) || t != (Endpoint{Path: s.Path, Kind: "absent"}) || item.Bytes < 0 || (s.Kind != "file" && item.Bytes != 0) {
		return batchError("invalid_plan")
	}
	if s.Path == v.root.Source.Path {
		if s != v.root.Source || t != v.root.Target {
			return batchError("invalid_plan")
		}
	} else if v.root.Source.Kind != "directory" || !strings.HasPrefix(s.Path, v.root.Source.Path+"/") {
		return batchError("invalid_plan")
	}
	if index > 0 && v.last <= s.Path {
		return batchError("invalid_plan")
	}
	key := batchFold(s.Path)
	if _, exists := v.paths[key]; exists {
		return batchError("invalid_plan")
	}
	v.paths[key] = item
	v.last = s.Path
	return nil
}

// Checking each immediate parent once proves the complete ancestor chain:
// all paths are strictly within the root and every edge reduces path depth.
// It also rejects file/link ancestors and differently spelled parent aliases.
// Run before any identity/storage I/O, and again on authenticated restoration.
func (v *batchPlanValidator) finalize() error {
	if v.version != 2 {
		return nil // Preserve the v1 copy acceptance set exactly.
	}
	if len(v.paths) == 0 || v.last != v.root.Source.Path {
		return batchError("invalid_plan")
	}
	for _, item := range v.paths {
		if item.Source.Path == v.root.Source.Path {
			continue
		}
		parent := item.Source.Path[:strings.LastIndexByte(item.Source.Path, '/')]
		p, exists := v.paths[batchFold(parent)]
		if !exists || p.Source.Path != parent || p.Source.Kind != "directory" {
			return batchError("invalid_plan")
		}
	}
	return nil
}
