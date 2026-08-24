package securefile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var ErrNotFound = errors.New("secure file was not found")

const maxFileBytes = 32 << 20

type Root struct {
	file         *os.File
	path         string
	resolvedPath string
}

func (r *Root) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	return r.file.Close()
}

func Read(path string, private bool) ([]byte, error) {
	return ReadLimit(path, maxFileBytes, private)
}

func ReadLimit(path string, limit int64, private bool) ([]byte, error) {
	file, info, err := openReadNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open secure file without following links: %w", err)
	}
	return readOpenFile(file, info, limit, private)
}

func readOpenFile(file *os.File, info os.FileInfo, limit int64, private bool) ([]byte, error) {
	defer file.Close()
	if limit < 0 {
		return nil, errors.New("secure file size limit is invalid")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("secure file is not a regular file")
	}
	if private {
		if err := checkPrivateFile(info); err != nil {
			return nil, err
		}
	}
	if info.Size() > limit {
		return nil, errors.New("secure file exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read secure file: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, errors.New("secure file exceeds size limit")
	}
	return data, nil
}

func relativeComponents(relative string) ([]string, error) {
	if relative == "" || strings.HasPrefix(relative, "/") || strings.Contains(relative, "\\") {
		return nil, errors.New("secure relative path must use non-empty slash-separated components")
	}
	components := strings.Split(relative, "/")
	for _, component := range components {
		if component == "" || component == "." || component == ".." || strings.ContainsRune(component, 0) {
			return nil, errors.New("secure relative path contains an invalid component")
		}
	}
	return components, nil
}

func AtomicWrite(path string, data []byte, private bool) error {
	dir := filepath.Dir(path)
	if err := ensureDirectory(dir, private); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("secure file target is not a regular file")
		}
		if private {
			if err := checkPrivateFile(info); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect secure file target: %w", err)
	}

	temp, err := os.CreateTemp(dir, ".edu-agent-*")
	if err != nil {
		return fmt.Errorf("create secure temporary file: %w", err)
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
		return fmt.Errorf("protect secure temporary file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write secure temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync secure temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close secure temporary file: %w", err)
	}
	if err := replaceFile(tempPath, path); err != nil {
		return fmt.Errorf("publish secure file: %w", err)
	}
	published = true
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync secure directory: %w", err)
	}
	return nil
}

func Delete(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete secure file: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync secure directory: %w", err)
	}
	return nil
}
