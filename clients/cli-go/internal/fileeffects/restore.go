package fileeffects

import "strings"

// IsArchiveRestore identifies a v3 restore fact, not authorization to operate
// on an archive path. Validate and the executor's identity checks remain required.
func (e Effect) IsArchiveRestore() bool {
	return e.SchemaVersion == 3 && e.Operation == "restore_archive"
}

func validArchiveRestore(e Effect) bool {
	parts := strings.SplitN(e.Source.Path, "/", 3)
	if !e.IsArchiveRestore() || !ValidPath(e.Source.Path, false) || len(parts) != 3 || parts[0] != ArchiveDirectory || !ValidPath(e.Target.Path, false) || Protected(e.Target.Path) {
		return false
	}
	if e.Source.Kind != e.Target.Kind || !strings.HasPrefix(e.Source.Version, "entry-v1:") || !ValidVersion(e.Source.Version) || e.Target.Version != "" || e.Directories != (DirectoryChain{}) {
		return false
	}
	return e.Source.Kind == "file" && e.Scope == "entry" || e.Source.Kind == "directory" && e.Scope == "subtree"
}
