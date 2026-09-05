package workspace

import (
	"runtime"
	"strings"
)

// Metadata evidence deliberately uses a different identity/digest than file
// content. A file publication (including an uncertain one) must still expire
// metadata for that entry and parents it may have created or modified. A newer
// read is conservatively treated the same way; it cannot prove old metadata.
func metadataReferenceSupersedes(current, previous *Reference) bool {
	if current == nil || previous == nil {
		return false
	}
	path, prior := current.Path, previous.Path
	if runtime.GOOS == "windows" {
		path, prior = strings.ToLower(path), strings.ToLower(prior)
	}
	if current.Kind == "file" && previous.Kind == "entry_metadata" {
		return prior == path || prior == "." || strings.HasPrefix(path, prior+"/")
	}
	// A stat observation alone cannot prove an old content snapshot still
	// applies, even if its metadata version happens to be unchanged.
	return current.Kind == "entry_metadata" && previous.Kind == "file" && path == prior
}
