package fileeffects

import "strings"

// IsArchivePurge identifies a permanent-cleanup fact, never permission to delete.
func (e Effect) IsArchivePurge() bool {
	return e.SchemaVersion == 4 && e.Operation == "purge_archive"
}

func validArchivePurge(e Effect) bool {
	if !e.IsArchivePurge() || !ValidPath(e.Source.Path, false) || !strings.HasPrefix(e.Source.Path, ArchiveDirectory+"/") || e.Target.Path != e.Source.Path || e.Target.Kind != "absent" || e.Target.Version != "" || e.Directories != (DirectoryChain{}) || !strings.HasPrefix(e.Source.Version, "entry-v1:") || !ValidVersion(e.Source.Version) {
		return false
	}
	return e.Source.Kind == "directory" && e.Scope == "subtree" || (e.Source.Kind == "file" || e.Source.Kind == "link") && e.Scope == "entry"
}
