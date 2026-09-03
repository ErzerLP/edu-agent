//go:build windows

package keybackend

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"golang.org/x/sys/windows"
)

func TestWindowsSecretBroadACLReadFailsClosedAndReplacementTightens(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	locator := Locator{Service: "edu-agent-agent-sessions-v1", Account: "windows-acl-replace"}
	defer DeleteSecret(locator)
	first := []byte("first-secret")
	if err := StoreSecret(locator, first); err != nil {
		t.Fatal(err)
	}
	path, err := secretPath(locator)
	if err != nil {
		t.Fatal(err)
	}
	if err := securefile.CheckPrivateDirectory(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := securefile.CheckPrivateFile(path); err != nil {
		t.Fatal(err)
	}
	setKeyBackendBroadWindowsDACL(t, path)
	if _, err := LoadSecret(locator, 1024); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("broad ACL load error = %v, want ErrUnavailable", err)
	}
	second := []byte("second-secret")
	if err := StoreSecret(locator, second); err != nil {
		t.Fatal(err)
	}
	if err := securefile.CheckPrivateFile(path); err != nil {
		t.Fatalf("replacement ACL: %v", err)
	}
	loaded, err := LoadSecret(locator, 1024)
	if err != nil || !bytes.Equal(loaded, second) {
		t.Fatalf("replacement round trip = %q, err=%v", loaded, err)
	}
	clear(loaded)
	if err := DeleteSecret(locator); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted DPAPI file remains: %v", err)
	}
}

func TestWindowsSecretRejectsReparseParentAndTarget(t *testing.T) {
	locator := Locator{Service: "edu-agent-agent-sessions-v1", Account: "windows-reparse"}
	t.Run("parent", func(t *testing.T) {
		localApp := t.TempDir()
		t.Setenv("LOCALAPPDATA", localApp)
		outside := t.TempDir()
		createKeyBackendWindowsSymlink(t, filepath.Join(localApp, "EduAgent"), outside, true)
		if err := StoreSecret(locator, []byte("blocked")); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("reparse parent store error = %v, want ErrUnavailable", err)
		}
	})
	t.Run("target", func(t *testing.T) {
		localApp := t.TempDir()
		t.Setenv("LOCALAPPDATA", localApp)
		path, err := secretPath(locator)
		if err != nil {
			t.Fatal(err)
		}
		if err := ensureWindowsSecretDirectory(filepath.Dir(path)); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.dpapi")
		if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		createKeyBackendWindowsSymlink(t, path, outside, false)
		if err := StoreSecret(locator, []byte("blocked")); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("reparse target store error = %v, want ErrUnavailable", err)
		}
		if _, err := LoadSecret(locator, 1024); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("reparse target load error = %v, want ErrUnavailable", err)
		}
	})
}

func createKeyBackendWindowsSymlink(t *testing.T, linkPath, targetPath string, directory bool) {
	t.Helper()
	linkPointer, err := windows.UTF16PtrFromString(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	targetPointer, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	flags := uint32(0x2)
	if directory {
		flags |= windows.SYMBOLIC_LINK_FLAG_DIRECTORY
	}
	if err := windows.CreateSymbolicLink(linkPointer, targetPointer, flags); err != nil {
		t.Fatalf("create required Windows reparse fixture: %s", safeKeyBackendWindowsError(err))
	}
}

func setKeyBackendBroadWindowsDACL(t *testing.T, path string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(pointer, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatalf("open ACL fixture: %s", safeKeyBackendWindowsError(err))
	}
	err = windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	runtime.KeepAlive(descriptor)
	closeErr := windows.CloseHandle(handle)
	if err != nil {
		t.Fatalf("apply ACL fixture: %s", safeKeyBackendWindowsError(err))
	}
	if closeErr != nil {
		t.Fatalf("close ACL fixture: %s", safeKeyBackendWindowsError(closeErr))
	}
}

func safeKeyBackendWindowsError(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("win32=%d", uint32(errno))
	}
	return fmt.Sprintf("error_type=%T", err)
}
