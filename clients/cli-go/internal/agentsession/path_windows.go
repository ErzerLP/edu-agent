//go:build windows

package agentsession

import (
	"os"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"golang.org/x/sys/windows"
)

func sessionPathIsReparse(path string, _ os.FileInfo) bool {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return true
	}
	attributes, err := windows.GetFileAttributes(pointer)
	return err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func enforcePrivateSessionDirectory(path string, _ os.FileInfo) error {
	return securefile.EnsurePrivateDirectory(path)
}

// validatePrivateSessionLock checks an already-open lock file without trying to
// rewrite its owner or DACL while LockFileEx is holding the file. New lock files
// inherit the protected current-user-only ACL from the Session root; existing
// files fail closed if that invariant does not hold.
func validatePrivateSessionLock(path string) error {
	return securefile.CheckPrivateFile(path)
}
