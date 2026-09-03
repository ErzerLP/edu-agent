//go:build windows

package securefile

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// EnsurePrivateDirectory replaces an existing directory owner and DACL with the
// current user and a protected current-user-only DACL whose single ACE is
// inheritable by children.
func EnsurePrivateDirectory(path string) error {
	return ensurePrivateWindowsPath(path, true)
}

// EnsurePrivateFile replaces an existing regular-file owner and DACL with the
// current user and a protected current-user-only DACL.
func EnsurePrivateFile(path string) error {
	return ensurePrivateWindowsPath(path, false)
}

// CheckPrivateDirectory verifies a current-user owner and protected
// current-user-only directory DACL.
func CheckPrivateDirectory(path string) error {
	return checkPrivateWindowsPath(path, true)
}

// CheckPrivateFile verifies a current-user owner and protected
// current-user-only regular-file DACL.
func CheckPrivateFile(path string) error {
	return checkPrivateWindowsPath(path, false)
}

func ensurePrivateWindowsPath(path string, directory bool) error {
	handle, err := openWindowsSecurityPath(path, directory, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return ensurePrivateWindowsHandle(handle, directory)
}

func checkPrivateWindowsPath(path string, directory bool) error {
	handle, err := openWindowsSecurityPath(path, directory, windows.READ_CONTROL)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return checkPrivateWindowsHandle(handle, directory)
}

func openWindowsSecurityPath(path string, directory bool, access uint32) (windows.Handle, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	flags := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := windows.CreateFile(pointer, access|windows.FILE_READ_ATTRIBUTES, windowsAllShare, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return windows.InvalidHandle, classifyWindowsOpenError(err)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, ErrLink
	}
	isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if directory && !isDirectory {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, ErrNotDirectory
	}
	if !directory && isDirectory {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, ErrNotRegular
	}
	return handle, nil
}

func ensurePrivateWindowsHandle(handle windows.Handle, directory bool) error {
	descriptor, owner, dacl, err := privateWindowsSecurityDescriptor(directory)
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
		owner,
		nil,
		nil,
		nil,
	); err != nil {
		return fmt.Errorf("%w: set private Windows owner: %v", ErrPermission, err)
	}
	if err := windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return fmt.Errorf("%w: set private Windows DACL: %v", ErrPermission, err)
	}
	runtime.KeepAlive(descriptor)
	return checkPrivateWindowsHandle(handle, directory)
}

func checkPrivateWindowsHandle(handle windows.Handle, directory bool) error {
	actual, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("%w: read Windows DACL: %v", ErrPermission, err)
	}
	control, _, err := actual.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("%w: Windows DACL is not protected", ErrPermission)
	}
	actualOwner, ownerDefaulted, err := actual.Owner()
	if err != nil || actualOwner == nil || ownerDefaulted {
		return fmt.Errorf("%w: Windows owner is unavailable or defaulted", ErrPermission)
	}
	actualDACL, daclDefaulted, err := actual.DACL()
	if err != nil || actualDACL == nil || daclDefaulted {
		return fmt.Errorf("%w: Windows DACL is unavailable, empty, or defaulted", ErrPermission)
	}
	desired, desiredOwner, desiredDACL, err := privateWindowsSecurityDescriptor(directory)
	if err != nil {
		return err
	}
	defer runtime.KeepAlive(desired)
	if !actualOwner.Equals(desiredOwner) || !windowsACLsEqual(actualDACL, desiredDACL) {
		return fmt.Errorf("%w: Windows DACL is not current-user-only", ErrPermission)
	}
	return nil
}

func privateWindowsSecurityDescriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, *windows.SID, *windows.ACL, error) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, nil, err
	}
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;%s;FA;;;%s)", tokenUser.User.Sid.String(), inheritance, tokenUser.User.Sid.String()))
	if err != nil {
		return nil, nil, nil, err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return nil, nil, nil, err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return nil, nil, nil, err
	}
	if dacl == nil {
		return nil, nil, nil, fmt.Errorf("build private Windows DACL: empty DACL")
	}
	return descriptor, owner, dacl, nil
}

func windowsACLsEqual(left, right *windows.ACL) bool {
	if left == nil || right == nil || left.AceCount != right.AceCount {
		return false
	}
	for index := uint32(0); index < uint32(left.AceCount); index++ {
		var leftACE *windows.ACCESS_ALLOWED_ACE
		var rightACE *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(left, index, &leftACE) != nil || windows.GetAce(right, index, &rightACE) != nil || leftACE == nil || rightACE == nil {
			return false
		}
		if leftACE.Header != rightACE.Header || leftACE.Mask != rightACE.Mask || leftACE.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false
		}
		leftSID := (*windows.SID)(unsafe.Pointer(&leftACE.SidStart))
		rightSID := (*windows.SID)(unsafe.Pointer(&rightACE.SidStart))
		if !leftSID.Equals(rightSID) {
			return false
		}
	}
	return true
}

func checkPrivateOpenFile(file *os.File, _ os.FileInfo) error {
	return checkPrivateWindowsHandle(windows.Handle(file.Fd()), false)
}

func protectPrivateOpenFile(file *os.File, directory bool) error {
	return ensurePrivateWindowsHandle(windows.Handle(file.Fd()), directory)
}
