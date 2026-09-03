//go:build windows

package securefile

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateACLHelpersTightenAndRejectReparse(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	setWindowsEveryoneDACL(t, rootPath, true)
	if err := CheckPrivateDirectory(rootPath); !errors.Is(err, ErrPermission) {
		t.Fatalf("broad directory ACL check error = %v, want ErrPermission", err)
	}
	if err := EnsurePrivateDirectory(rootPath); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDirectory(rootPath); err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(rootPath, "secret.bin")
	if err := os.WriteFile(filePath, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	setWindowsEveryoneDACL(t, filePath, false)
	if err := CheckPrivateFile(filePath); !errors.Is(err, ErrPermission) {
		t.Fatalf("broad file ACL check error = %v, want ErrPermission", err)
	}
	if err := EnsurePrivateFile(filePath); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateFile(filePath); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(rootPath, "linked.bin")
	createWindowsFileSymlinkFixture(t, linkPath, outside)
	if err := CheckPrivateFile(linkPath); !errors.Is(err, ErrLink) {
		t.Fatalf("reparse file check error = %v, want ErrLink", err)
	}
	if err := EnsurePrivateFile(linkPath); !errors.Is(err, ErrLink) {
		t.Fatalf("reparse file ensure error = %v, want ErrLink", err)
	}
}

func TestWindowsHandleRelativeCreateReplaceAndCleanup(t *testing.T) {
	rootPath := t.TempDir()
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	created, err := root.Publish(context.Background(), "nested/deeper/file.txt", []byte("first\r\n"), PublishOptions{
		Mode:       PublishCreate,
		Permission: 0o644,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Outcome != PublishCompleted {
		t.Fatalf("create outcome = %q, want %q", created.Outcome, PublishCompleted)
	}

	snapshot, err := root.ReadSnapshot("nested/deeper/file.txt", 1024, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(snapshot.Data), "first\r\n"; got != want {
		t.Fatalf("created content = %q, want %q", got, want)
	}
	if !strings.HasPrefix(snapshot.Identity, "windows:") {
		t.Fatalf("snapshot identity = %q, want Windows volume/file identity", snapshot.Identity)
	}

	replaced, err := root.Publish(context.Background(), "nested/deeper/file.txt", []byte("second\r\n"), PublishOptions{
		Mode:          PublishReplace,
		Permission:    snapshot.Mode,
		ExpectedHash:  snapshotContentHash(snapshot.Data),
		ExpectedLimit: 1024,
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if replaced.Outcome != PublishCompleted {
		t.Fatalf("replace outcome = %q, want %q", replaced.Outcome, PublishCompleted)
	}

	unchanged, err := root.Publish(context.Background(), "nested/deeper/file.txt", []byte("must not replace"), PublishOptions{
		Mode:       PublishCreate,
		Permission: 0o644,
	})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("create existing error = %v, want ErrAlreadyExists", err)
	}
	if unchanged.Outcome != PublishUnchanged {
		t.Fatalf("create existing outcome = %q, want %q", unchanged.Outcome, PublishUnchanged)
	}

	unchanged, err = root.Publish(context.Background(), "nested/deeper/file.txt", []byte("must not conflict-replace"), PublishOptions{
		Mode:          PublishReplace,
		Permission:    0o644,
		ExpectedHash:  snapshotContentHash([]byte("stale")),
		ExpectedLimit: 1024,
	})
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("stale replace error = %v, want ErrChanged", err)
	}
	if unchanged.Outcome != PublishUnchanged {
		t.Fatalf("stale replace outcome = %q, want %q", unchanged.Outcome, PublishUnchanged)
	}
	assertNoWindowsTemporaryFiles(t, filepath.Join(rootPath, "nested", "deeper"))

	final, err := os.ReadFile(filepath.Join(rootPath, "nested", "deeper", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(final), "second\r\n"; got != want {
		t.Fatalf("final content = %q, want %q", got, want)
	}
}

func TestWindowsRejectsReparseAndInvalidPaths(t *testing.T) {
	rootPath := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	t.Run("symlink-reparse", func(t *testing.T) {
		link := filepath.Join(rootPath, "linked")
		createWindowsDirectorySymlinkFixture(t, link, outside)
		assertWindowsLinkRejected(t, root, "linked")
		assertWindowsListMarksLink(t, root, "linked")
	})

	t.Run("junction", func(t *testing.T) {
		link := filepath.Join(rootPath, "junction")
		createWindowsJunctionFixture(t, link, outside)
		assertWindowsLinkRejected(t, root, "junction")
		assertWindowsListMarksLink(t, root, "junction")
	})

	if err := os.WriteFile(filepath.Join(rootPath, "plain.txt"), []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := []string{
		"../escape.txt",
		"C:/escape.txt",
		`\\?\C:\escape.txt`,
		"plain.txt:stream",
		"NUL",
		"COM1.txt",
	}
	for _, relative := range invalid {
		t.Run("invalid-"+strings.NewReplacer("/", "_", `\`, "_", ":", "_").Replace(relative), func(t *testing.T) {
			if _, err := root.ReadSnapshot(relative, 1024, false); err == nil {
				t.Fatalf("ReadSnapshot(%q) unexpectedly succeeded", relative)
			}
			result, err := root.Publish(context.Background(), relative, []byte("blocked"), PublishOptions{
				Mode:       PublishCreate,
				Permission: 0o600,
			})
			if err == nil {
				t.Fatalf("Publish(%q) unexpectedly succeeded", relative)
			}
			if result.Outcome != PublishUnchanged {
				t.Fatalf("Publish(%q) outcome = %q, want %q", relative, result.Outcome, PublishUnchanged)
			}
		})
	}
}

func TestWindowsReplacePreservesProtectedDACL(t *testing.T) {
	rootPath := t.TempDir()
	targetPath := filepath.Join(rootPath, "acl.txt")
	if err := os.WriteFile(targetPath, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeDACL := setAndReadProtectedWindowsDACL(t, targetPath)

	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	snapshot, err := root.ReadSnapshot("acl.txt", 1024, false)
	if err != nil {
		t.Fatal(err)
	}
	result, err := root.Publish(context.Background(), "acl.txt", []byte("after"), PublishOptions{
		Mode:          PublishReplace,
		Permission:    snapshot.Mode,
		ExpectedHash:  snapshotContentHash(snapshot.Data),
		ExpectedLimit: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != PublishCompleted {
		t.Fatalf("outcome = %q, want %q", result.Outcome, PublishCompleted)
	}
	afterDACL := readWindowsDACLSignature(t, targetPath)
	if afterDACL != beforeDACL {
		t.Fatalf("replacement DACL changed:\nbefore %s\nafter  %s", beforeDACL, afterDACL)
	}
}

func TestWindowsRootAndParentHandlesPinNamespace(t *testing.T) {
	outer := t.TempDir()
	rootPath := filepath.Join(outer, "workspace")
	if err := os.MkdirAll(filepath.Join(rootPath, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "parent", "file.txt"), []byte("pinned"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := os.Rename(rootPath, filepath.Join(outer, "moved-workspace")); err == nil {
		t.Fatal("workspace root was renamed while its retained handle denied delete sharing")
	}
	parent, err := openDirectoryWithinRoot(root, []string{"parent"})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	info, err := parent.Stat()
	if err != nil {
		t.Fatal(err)
	}
	identityBefore, err := snapshotFileIdentityForPlatform(parent, info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootPath, "parent"), filepath.Join(rootPath, "moved-parent")); err == nil {
		t.Fatal("opened parent was renamed while its retained handle denied delete sharing")
	}
	info, err = parent.Stat()
	if err != nil {
		t.Fatal(err)
	}
	identityAfter, err := snapshotFileIdentityForPlatform(parent, info)
	if err != nil {
		t.Fatal(err)
	}
	if identityAfter != identityBefore {
		t.Fatalf("parent handle identity changed: before %q, after %q", identityBefore, identityAfter)
	}
	if _, err := root.ReadSnapshot("parent/file.txt", 1024, false); err != nil {
		t.Fatalf("read through pinned handles: %v", err)
	}
}

func TestWindowsHardlinkAliasesShareFileIdentity(t *testing.T) {
	rootPath := t.TempDir()
	firstPath := filepath.Join(rootPath, "first.txt")
	secondPath := filepath.Join(rootPath, "second.txt")
	if err := os.WriteFile(firstPath, []byte("same file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createWindowsHardlinkFixture(secondPath, firstPath); err != nil {
		failWindowsFixture(t, "hardlink", "create", err)
	}
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	first, err := root.ReadSnapshot("first.txt", 1024, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := root.ReadSnapshot("second.txt", 1024, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Identity == "" || second.Identity == "" || first.Identity != second.Identity {
		t.Fatalf("hardlink identities differ: first %q, second %q", first.Identity, second.Identity)
	}
}

const windowsSymbolicLinkFlagAllowUnprivilegedCreate = 0x2

func createWindowsDirectorySymlinkFixture(t *testing.T, linkPath, targetPath string) {
	t.Helper()
	createWindowsSymlinkFixture(t, linkPath, targetPath, windows.SYMBOLIC_LINK_FLAG_DIRECTORY|windowsSymbolicLinkFlagAllowUnprivilegedCreate)
}

func createWindowsFileSymlinkFixture(t *testing.T, linkPath, targetPath string) {
	t.Helper()
	createWindowsSymlinkFixture(t, linkPath, targetPath, windowsSymbolicLinkFlagAllowUnprivilegedCreate)
}

func createWindowsSymlinkFixture(t *testing.T, linkPath, targetPath string, flags uint32) {
	t.Helper()
	linkPointer, err := windows.UTF16PtrFromString(linkPath)
	if err != nil {
		failWindowsFixture(t, "directory-symlink", "encode-link", err)
	}
	targetPointer, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		failWindowsFixture(t, "directory-symlink", "encode-target", err)
	}
	if err := windows.CreateSymbolicLink(linkPointer, targetPointer, flags); err != nil {
		failWindowsFixture(t, "symlink", "create-native", err)
	}
}

func createWindowsJunctionFixture(t *testing.T, junctionPath, targetPath string) {
	t.Helper()
	if err := os.Mkdir(junctionPath, 0o700); err != nil {
		failWindowsFixture(t, "junction", "create-directory", err)
	}
	junctionPointer, err := windows.UTF16PtrFromString(junctionPath)
	if err != nil {
		failWindowsFixture(t, "junction", "encode-directory", err)
	}
	handle, err := windows.CreateFile(
		junctionPointer,
		windows.GENERIC_WRITE,
		windowsAllShare,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		failWindowsFixture(t, "junction", "open-reparse-directory", err)
	}
	buffer, err := windowsMountPointReparseBuffer(targetPath)
	if err != nil {
		_ = windows.CloseHandle(handle)
		failWindowsFixture(t, "junction", "build-reparse-buffer", err)
	}
	var bytesReturned uint32
	setErr := windows.DeviceIoControl(
		handle,
		windows.FSCTL_SET_REPARSE_POINT,
		&buffer[0],
		uint32(len(buffer)),
		nil,
		0,
		&bytesReturned,
		nil,
	)
	closeErr := windows.CloseHandle(handle)
	if setErr != nil {
		failWindowsFixture(t, "junction", "set-mount-point", setErr)
	}
	if closeErr != nil {
		failWindowsFixture(t, "junction", "close-directory", closeErr)
	}
}

func windowsMountPointReparseBuffer(targetPath string) ([]byte, error) {
	cleanTarget := filepath.Clean(targetPath)
	substituteName, err := windows.UTF16FromString(`\??\` + cleanTarget)
	if err != nil {
		return nil, err
	}
	printName, err := windows.UTF16FromString(cleanTarget)
	if err != nil {
		return nil, err
	}

	substituteNameBytes := len(substituteName) * 2
	printNameBytes := len(printName) * 2
	pathBytes := substituteNameBytes + printNameBytes
	reparseDataLength := 8 + pathBytes
	if reparseDataLength > int(^uint16(0)) {
		return nil, errors.New("mount-point reparse buffer exceeds Windows limit")
	}
	buffer := make([]byte, 8+reparseDataLength)
	binary.LittleEndian.PutUint32(buffer[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buffer[4:6], uint16(reparseDataLength))
	binary.LittleEndian.PutUint16(buffer[8:10], 0)
	binary.LittleEndian.PutUint16(buffer[10:12], uint16(substituteNameBytes-2))
	binary.LittleEndian.PutUint16(buffer[12:14], uint16(substituteNameBytes))
	binary.LittleEndian.PutUint16(buffer[14:16], uint16(printNameBytes-2))

	offset := 16
	for _, value := range substituteName {
		binary.LittleEndian.PutUint16(buffer[offset:offset+2], value)
		offset += 2
	}
	for _, value := range printName {
		binary.LittleEndian.PutUint16(buffer[offset:offset+2], value)
		offset += 2
	}
	return buffer, nil
}

func createWindowsHardlinkFixture(linkPath, targetPath string) error {
	linkPointer, err := windows.UTF16PtrFromString(linkPath)
	if err != nil {
		return err
	}
	targetPointer, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		return err
	}
	return windows.CreateHardLink(linkPointer, targetPointer, 0)
}

func failWindowsFixture(t *testing.T, fixture, step string, err error) {
	t.Helper()
	t.Fatalf("%s fixture failed at %s (%s)", fixture, step, safeWindowsFixtureError(err))
}

func safeWindowsFixtureError(err error) string {
	var status windows.NTStatus
	if errors.As(err, &status) {
		return fmt.Sprintf("ntstatus=0x%08x", uint32(status))
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("win32=%d", uint32(errno))
	}
	return fmt.Sprintf("error_type=%T", err)
}

func assertWindowsLinkRejected(t *testing.T, root *Root, relative string) {
	t.Helper()
	if _, err := root.ReadSnapshot(relative+"/secret.txt", 1024, false); !errors.Is(err, ErrLink) {
		t.Fatalf("read through %s error = %v, want ErrLink", relative, err)
	}
	result, err := root.Publish(context.Background(), relative+"/created.txt", []byte("blocked"), PublishOptions{
		Mode:       PublishCreate,
		Permission: 0o600,
	})
	if !errors.Is(err, ErrLink) {
		t.Fatalf("publish through %s error = %v, want ErrLink", relative, err)
	}
	if result.Outcome != PublishUnchanged {
		t.Fatalf("publish through %s outcome = %q, want %q", relative, result.Outcome, PublishUnchanged)
	}
}

func assertWindowsListMarksLink(t *testing.T, root *Root, name string) {
	t.Helper()
	entries, _, _, err := root.ReadDir(".", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name == name {
			if entry.Type != EntryLink {
				t.Fatalf("entry %q type = %q, want %q", name, entry.Type, EntryLink)
			}
			return
		}
	}
	t.Fatalf("link entry %q was not listed", name)
}

func assertNoWindowsTemporaryFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".edu-agent-") {
			t.Fatalf("temporary file was not cleaned up: %s", entry.Name())
		}
	}
}

func setWindowsEveryoneDACL(t *testing.T, path string, directory bool) {
	t.Helper()
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	securityDescriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:(A;%s;FA;;;WD)", inheritance))
	if err != nil {
		failWindowsFixture(t, "broad-dacl", "build-security-descriptor", err)
	}
	dacl, _, err := securityDescriptor.DACL()
	if err != nil {
		failWindowsFixture(t, "broad-dacl", "read-descriptor-dacl", err)
	}
	handle, err := openWindowsTestSecurityHandle(path, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		failWindowsFixture(t, "broad-dacl", "open-target", err)
	}
	err = windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
	runtime.KeepAlive(securityDescriptor)
	closeErr := windows.CloseHandle(handle)
	if err != nil {
		failWindowsFixture(t, "broad-dacl", "apply", err)
	}
	if closeErr != nil {
		failWindowsFixture(t, "broad-dacl", "close-target", closeErr)
	}
}

func setAndReadProtectedWindowsDACL(t *testing.T, path string) string {
	t.Helper()
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		failWindowsFixture(t, "protected-dacl", "read-current-user", err)
	}
	securityDescriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;;FA;;;%s)", tokenUser.User.Sid.String()))
	if err != nil {
		failWindowsFixture(t, "protected-dacl", "build-security-descriptor", err)
	}
	dacl, _, err := securityDescriptor.DACL()
	if err != nil {
		failWindowsFixture(t, "protected-dacl", "read-descriptor-dacl", err)
	}
	handle, err := openWindowsTestSecurityHandle(path, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		failWindowsFixture(t, "protected-dacl", "open-target", err)
	}
	err = windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
	runtime.KeepAlive(securityDescriptor)
	closeErr := windows.CloseHandle(handle)
	if err != nil {
		failWindowsFixture(t, "protected-dacl", "apply", err)
	}
	if closeErr != nil {
		failWindowsFixture(t, "protected-dacl", "close-target", closeErr)
	}
	return readWindowsDACLSignature(t, path)
}

func readWindowsDACLSignature(t *testing.T, path string) string {
	t.Helper()
	handle, err := openWindowsTestSecurityHandle(path, windows.READ_CONTROL)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	securityDescriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := securityDescriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("protected=%t;%s", control&windows.SE_DACL_PROTECTED != 0, securityDescriptor.String())
}

func openWindowsTestSecurityHandle(path string, access uint32) (windows.Handle, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(
		pathPointer,
		access,
		windowsAllShare,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
}
