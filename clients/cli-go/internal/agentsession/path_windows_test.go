//go:build windows

package agentsession

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"golang.org/x/sys/windows"
)

func TestAgentSessionWindowsPrivateACLAndAtomicReplace(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "agent-sessions")
	store := openTestStore(t, rootPath, &memorySecretBackend{}, Limits{})
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "private", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	marker, err := handle.MarkDirty(t.Context(), record.RecordRevision, 1, "model", false)
	if err != nil {
		t.Fatal(err)
	}
	marker.MayHaveSideEffect = true
	if _, err := handle.UpdateDirty(t.Context(), marker); err != nil {
		t.Fatal(err)
	}
	candidate := record
	candidate.Checkpoint = []byte(`{"v":2}`)
	record, err = handle.Save(t.Context(), record.RecordRevision, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if record.RecordRevision != 2 {
		t.Fatalf("record revision = %d, want 2", record.RecordRevision)
	}
	if err := securefile.CheckPrivateDirectory(rootPath); err != nil {
		t.Fatalf("session root ACL: %v", err)
	}
	paths := []string{
		profileLockName,
		sessionLockName(record.StorageID),
		indexName,
		keyName(record.StorageID),
		recordName(record.StorageID),
		indexProjectionName(record.StorageID),
		dirtyName(record.StorageID),
	}
	for _, name := range paths {
		if err := securefile.CheckPrivateFile(filepath.Join(rootPath, name)); err != nil {
			t.Fatalf("private ACL for %s: %v", name, err)
		}
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentSessionWindowsTightensBroadRootACLAndFailsClosedOnBroadRecord(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "agent-sessions")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	setAgentSessionBroadWindowsDACL(t, rootPath, true)
	if err := securefile.CheckPrivateDirectory(rootPath); !errors.Is(err, securefile.ErrPermission) {
		t.Fatalf("broad root ACL check = %v, want ErrPermission", err)
	}
	store := openTestStore(t, rootPath, &memorySecretBackend{}, Limits{})
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "tightened", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	defer store.Close()
	if err := securefile.CheckPrivateDirectory(rootPath); err != nil {
		t.Fatalf("tightened root ACL: %v", err)
	}
	recordPath := filepath.Join(rootPath, recordName(record.StorageID))
	setAgentSessionBroadWindowsDACL(t, recordPath, false)
	if _, err := handle.Load(); !errors.Is(err, securefile.ErrPermission) {
		t.Fatalf("broad record load error = %v, want fail-closed ErrPermission", err)
	}
}

func TestAgentSessionWindowsRejectsReparseRootAndParent(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(base, "linked-root")
	createAgentSessionWindowsDirectorySymlink(t, linkRoot, realRoot)
	if _, err := Open(t.Context(), Options{
		Root: linkRoot, ProfileFingerprint: strings.Repeat("a", 64), Secrets: &memorySecretBackend{},
	}); !errors.Is(err, securefile.ErrLink) {
		t.Fatalf("reparse root error = %v, want ErrLink", err)
	}
	if _, err := Open(t.Context(), Options{
		Root: filepath.Join(linkRoot, "sessions"), ProfileFingerprint: strings.Repeat("a", 64), Secrets: &memorySecretBackend{},
	}); !errors.Is(err, securefile.ErrLink) {
		t.Fatalf("reparse parent error = %v, want ErrLink", err)
	}
}

func createAgentSessionWindowsDirectorySymlink(t *testing.T, linkPath, targetPath string) {
	t.Helper()
	linkPointer, err := windows.UTF16PtrFromString(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	targetPointer, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	const allowUnprivilegedCreate = 0x2
	if err := windows.CreateSymbolicLink(linkPointer, targetPointer, windows.SYMBOLIC_LINK_FLAG_DIRECTORY|allowUnprivilegedCreate); err != nil {
		t.Fatalf("create required Windows directory reparse fixture: %s", safeAgentSessionWindowsError(err))
	}
}

func setAgentSessionBroadWindowsDACL(t *testing.T, path string, directory bool) {
	t.Helper()
	inheritance := ""
	flags := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		inheritance = "OICI"
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:(A;%s;FA;;;WD)", inheritance))
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
	handle, err := windows.CreateFile(pointer, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		t.Fatalf("open ACL fixture: %s", safeAgentSessionWindowsError(err))
	}
	err = windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	runtime.KeepAlive(descriptor)
	closeErr := windows.CloseHandle(handle)
	if err != nil {
		t.Fatalf("apply ACL fixture: %s", safeAgentSessionWindowsError(err))
	}
	if closeErr != nil {
		t.Fatalf("close ACL fixture: %s", safeAgentSessionWindowsError(closeErr))
	}
}

func safeAgentSessionWindowsError(err error) string {
	var errno windows.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("win32=%d", uint32(errno))
	}
	return fmt.Sprintf("error_type=%T", err)
}
