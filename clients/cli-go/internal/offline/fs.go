package offline

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxManagedFile = ObjectHeaderSize + MaxSealedObject

// Keep the platform-shared managed-file primitives visibly rooted in this
// compilation unit; Store invokes each of them on both Unix and Windows.
var (
	_ = maxManagedFile
	_ = readManaged
	_ = atomicWriteManaged
	_ = deleteManaged
	_ = listManaged
)

func Exists(root string) (bool, error) {
	if err := validateExistingOrMissingRoot(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	info, err := lstatManaged(root, "profile.key")
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || isReparsePoint(filepath.Join(root, "profile.key"), info) {
		return false, ErrUnsafePath
	}
	return true, nil
}

func validateRelative(relative string) ([]string, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") || strings.ContainsRune(relative, 0) {
		return nil, ErrUnsafePath
	}
	parts := strings.Split(relative, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.Contains(part, ":") {
			return nil, ErrUnsafePath
		}
	}
	return parts, nil
}

func validateExistingOrMissingRoot(root string) error {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return ErrUnsafePath
	}
	volume := filepath.VolumeName(absolute)
	remainder := strings.TrimPrefix(absolute, volume)
	current := volume + string(filepath.Separator)
	for _, component := range strings.FieldsFunc(remainder, func(r rune) bool { return r == '/' || r == '\\' }) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		if err != nil {
			return fmt.Errorf("inspect offline root component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || isReparsePoint(current, info) {
			return ErrUnsafePath
		}
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrUnsafePath
	}
	return nil
}

func ensureRoot(root string) error {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return ErrUnsafePath
	}
	volume := filepath.VolumeName(absolute)
	remainder := strings.TrimPrefix(absolute, volume)
	current := volume + string(filepath.Separator)
	components := strings.FieldsFunc(remainder, func(r rune) bool { return r == '/' || r == '\\' })
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("create offline root: %w", err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return fmt.Errorf("inspect offline root: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || isReparsePoint(current, info) {
			return ErrUnsafePath
		}
		if index == len(components)-1 {
			if err := enforcePrivateDirectory(current, info); err != nil {
				return err
			}
		}
	}
	return nil
}

func managedPath(root, relative string, createParents bool) (string, error) {
	parts, err := validateRelative(relative)
	if err != nil {
		return "", err
	}
	if err := validateExistingOrMissingRoot(root); err != nil {
		return "", err
	}
	current := root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && createParents {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("create offline object directory: %w", err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || isReparsePoint(current, info) {
			return "", ErrUnsafePath
		}
		if err := enforcePrivateDirectory(current, info); err != nil {
			return "", err
		}
	}
	return filepath.Join(current, parts[len(parts)-1]), nil
}

func lstatManaged(root, relative string) (os.FileInfo, error) {
	path, err := managedPath(root, relative, false)
	if err != nil {
		return nil, err
	}
	return os.Lstat(path)
}

func readManaged(root, relative string, limit int64) ([]byte, error) {
	path, err := managedPath(root, relative, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	file, info, err := openReadNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open offline object: %w", err)
	}
	defer file.Close()
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || isReparsePoint(path, info) {
		return nil, ErrUnsafePath
	}
	if err := enforcePrivateFile(path, info); err != nil {
		return nil, err
	}
	if limit < 0 || info.Size() > limit {
		return nil, fmt.Errorf("%w: managed file exceeds limit", ErrCorruptStore)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read offline object: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: managed file exceeds limit", ErrCorruptStore)
	}
	return data, nil
}

func atomicWriteManaged(root, relative string, data []byte, replace bool) error {
	path, err := managedPath(root, relative, true)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !replace {
			return os.ErrExist
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || isReparsePoint(path, info) {
			return ErrUnsafePath
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory := filepath.Dir(path)
	temp, err := os.CreateTemp(directory, ".edu-offline-*")
	if err != nil {
		return fmt.Errorf("create offline temporary file: %w", err)
	}
	tempPath := temp.Name()
	published := false
	defer func() {
		_ = temp.Close()
		if !published {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("protect offline temporary file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write offline temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("flush offline temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close offline temporary file: %w", err)
	}
	if _, err := managedPath(root, relative, false); err != nil {
		return err
	}
	if !replace {
		if _, err := os.Lstat(path); err == nil {
			return os.ErrExist
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := replaceFile(tempPath, path, replace); err != nil {
		return fmt.Errorf("publish offline file: %w", err)
	}
	published = true
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("flush offline directory: %w", err)
	}
	return nil
}

func deleteManaged(root, relative string) error {
	path, err := managedPath(root, relative, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || isReparsePoint(path, info) {
		return ErrUnsafePath
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("delete offline file: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

func listManaged(root, relativeDirectory string) ([]os.DirEntry, error) {
	path, err := managedPath(root, relativeDirectory+"/placeholder", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	directory := filepath.Dir(path)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return entries, nil
}
