//go:build windows

package keybackend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"golang.org/x/sys/windows"
)

const nativeSecretTimeout = 5 * time.Second

func availableSecret(Locator) error {
	if _, err := exec.LookPath("powershell.exe"); err != nil || strings.TrimSpace(os.Getenv("LOCALAPPDATA")) == "" {
		return ErrUnavailable
	}
	return nil
}

func secretSlot(locator Locator) string {
	if locator.Service == ServiceOfflineV1 && legacySlot(locator.Account) {
		return locator.Account
	}
	sum := sha256.Sum256([]byte(locator.Service + "\x00" + locator.Account))
	return hex.EncodeToString(sum[:])
}

func legacySlot(value string) bool {
	if !strings.HasPrefix(value, "profile-") || len(value) > 96 {
		return false
	}
	return validLocatorPart(value, 96)
}

func secretPath(locator Locator) (string, error) {
	root := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if root == "" {
		return "", ErrUnavailable
	}
	if locator.Service == ServiceOfflineV1 {
		if !legacySlot(locator.Account) {
			return "", ErrUnavailable
		}
		return filepath.Join(root, "EduAgent", "offline-keys", locator.Account+".txt"), nil
	}
	return filepath.Join(root, "EduAgent", "session-secrets", secretSlot(locator)+".dpapi"), nil
}

func runPowerShell(script, stdin string, env ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nativeSecretTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	command.Env = append(os.Environ(), env...)
	command.Stdin = strings.NewReader(stdin)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func loadSecret(locator Locator) (string, error) {
	if err := availableSecret(locator); err != nil {
		return "", err
	}
	path, err := secretPath(locator)
	if err != nil {
		return "", err
	}
	if err := validateWindowsSecretDirectory(filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", ErrUnavailable
	}
	stored, err := readWindowsSecretFile(path, maxSecretBytes*3)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", ErrUnavailable
	}
	defer clear(stored)
	var script string
	if locator.Service == ServiceOfflineV1 {
		script = `$ErrorActionPreference='Stop'; $cipher=[Console]::In.ReadToEnd().Trim(); $secure=ConvertTo-SecureString $cipher; $ptr=[Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure); try{[Console]::Out.Write([Runtime.InteropServices.Marshal]::PtrToStringBSTR($ptr))}finally{[Runtime.InteropServices.Marshal]::ZeroFreeBSTR($ptr)}`
	} else {
		script = `$ErrorActionPreference='Stop'; $b=[Convert]::FromBase64String([Console]::In.ReadToEnd().Trim()); $d=[Security.Cryptography.ProtectedData]::Unprotect($b,$null,[Security.Cryptography.DataProtectionScope]::CurrentUser); [Console]::Out.Write([Text.Encoding]::UTF8.GetString($d))`
	}
	input := string(stored)
	if locator.Service != ServiceOfflineV1 {
		input = strings.TrimSpace(input)
	}
	value, err := runPowerShell(script, input)
	if err != nil || value == "" {
		return "", ErrUnavailable
	}
	return value, nil
}

func storeSecret(locator Locator, encoded string) error {
	if err := availableSecret(locator); err != nil {
		return err
	}
	path, err := secretPath(locator)
	if err != nil {
		return err
	}
	if err := ensureWindowsSecretDirectory(filepath.Dir(path)); err != nil {
		return ErrUnavailable
	}
	if _, err := windowsSecretTargetSafe(path); err != nil {
		return ErrUnavailable
	}
	var protect string
	if locator.Service == ServiceOfflineV1 {
		protect = `$ErrorActionPreference='Stop'; $secret=[Console]::In.ReadToEnd().Trim(); $secure=ConvertTo-SecureString $secret -AsPlainText -Force; [Console]::Out.Write((ConvertFrom-SecureString $secure))`
	} else {
		protect = `$ErrorActionPreference='Stop'; $d=[Text.Encoding]::UTF8.GetBytes([Console]::In.ReadToEnd().Trim()); $b=[Security.Cryptography.ProtectedData]::Protect($d,$null,[Security.Cryptography.DataProtectionScope]::CurrentUser); [Console]::Out.Write([Convert]::ToBase64String($b))`
	}
	protected, err := runPowerShell(protect, encoded)
	if err != nil || protected == "" {
		return ErrUnavailable
	}
	defer func() { protected = "" }()
	if err := atomicWindowsSecretWrite(path, []byte(protected)); err != nil {
		return ErrUnavailable
	}
	return nil
}

func deleteSecret(locator Locator) error {
	path, err := secretPath(locator)
	if err != nil {
		return err
	}
	if err := validateWindowsSecretDirectory(filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return ErrUnavailable
	}
	exists, err := windowsSecretTargetSafe(path)
	if err != nil {
		return ErrUnavailable
	}
	if exists {
		if err := securefile.EnsurePrivateFile(path); err != nil {
			return ErrUnavailable
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrUnavailable
	}
	return nil
}

func readWindowsSecretFile(path string, limit int64) ([]byte, error) {
	value, err := securefile.ReadLimit(path, limit, true)
	if errors.Is(err, securefile.ErrNotFound) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	return value, nil
}

func ensureWindowsSecretDirectory(path string) error {
	return walkWindowsSecretDirectory(path, true)
}

func validateWindowsSecretDirectory(path string) error {
	return walkWindowsSecretDirectory(path, false)
}

func walkWindowsSecretDirectory(path string, create bool) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	current := volume + string(filepath.Separator)
	components := strings.FieldsFunc(strings.TrimPrefix(absolute, volume), func(value rune) bool { return value == '/' || value == '\\' })
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && create {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || windowsPathIsReparse(current) {
			return securefile.ErrLink
		}
		if index == len(components)-1 {
			if create {
				if err := securefile.EnsurePrivateDirectory(current); err != nil {
					return err
				}
			} else if err := securefile.CheckPrivateDirectory(current); err != nil {
				return err
			}
		}
	}
	return nil
}

func windowsPathIsReparse(path string) bool {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return true
	}
	attributes, err := windows.GetFileAttributes(pointer)
	return err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func windowsSecretTargetSafe(path string) (bool, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes, err := windows.GetFileAttributes(pointer)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if attributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		return true, ErrUnavailable
	}
	return true, nil
}

func atomicWindowsSecretWrite(path string, value []byte) error {
	directory := filepath.Dir(path)
	temp, err := os.CreateTemp(directory, ".edu-agent-secret-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	published := false
	defer func() {
		_ = temp.Close()
		if !published {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := temp.Write(value); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := securefile.EnsurePrivateFile(tempPath); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tempPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if _, err := windowsSecretTargetSafe(path); err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return err
	}
	published = true
	if err := securefile.CheckPrivateFile(path); err != nil {
		return err
	}
	return nil
}
