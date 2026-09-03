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

func enforcePrivateSessionFile(path string) error {
	return securefile.EnsurePrivateFile(path)
}
