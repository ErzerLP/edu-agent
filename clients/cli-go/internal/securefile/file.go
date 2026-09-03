package securefile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrNotFound       = errors.New("secure file was not found")
	ErrNotDirectory   = errors.New("secure path is not a directory")
	ErrNotRegular     = errors.New("secure file is not a regular file")
	ErrLink           = errors.New("secure path is a link or reparse point")
	ErrPermission     = errors.New("secure path permission denied")
	ErrTooLarge       = errors.New("secure file exceeds size limit")
	ErrChanged        = errors.New("secure file changed while reading")
	ErrAlreadyExists  = errors.New("secure file already exists")
	ErrOutcomeUnknown = errors.New("secure file publication outcome is unknown")
	ErrOutsideRoot    = errors.New("secure file handle is outside its root")
)

const maxFileBytes = 32 << 20

type EntryType string

const (
	EntryFile      EntryType = "file"
	EntryDirectory EntryType = "directory"
	EntryLink      EntryType = "link"
	EntryOther     EntryType = "other"
)

type DirEntry struct {
	Name string
	Type EntryType
}

type Snapshot struct {
	Data     []byte
	Size     int64
	ModTime  time.Time
	Mode     os.FileMode
	Identity string
}

type PublishMode string

const (
	PublishCreate  PublishMode = "create"
	PublishReplace PublishMode = "replace"
)

type PublishOutcome string

const (
	PublishUnchanged PublishOutcome = "unchanged"
	PublishCompleted PublishOutcome = "completed"
	PublishUnknown   PublishOutcome = "unknown"
)

type PublishOptions struct {
	Mode          PublishMode
	Permission    os.FileMode
	ExpectedHash  string
	ExpectedLimit int64
	Private       bool
}

type PublishResult struct {
	Outcome PublishOutcome
}

type Root struct {
	file         *os.File
	path         string
	resolvedPath string
}

func (r *Root) Identity() (string, error) {
	if r == nil || r.file == nil {
		return "", errors.New("secure root is closed")
	}
	info, err := r.file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect secure root: %w", err)
	}
	identity, err := snapshotFileIdentityForPlatform(r.file, info)
	if err != nil {
		return "", fmt.Errorf("identify secure root: %w", err)
	}
	return identity, nil
}

func (r *Root) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
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
	snapshot, err := readOpenFileSnapshot(file, info, limit, private)
	if err != nil {
		return nil, err
	}
	return snapshot.Data, nil
}

func readOpenFileSnapshot(file *os.File, info os.FileInfo, limit int64, private bool) (Snapshot, error) {
	defer file.Close()
	if limit < 0 {
		return Snapshot{}, errors.New("secure file size limit is invalid")
	}
	if !info.Mode().IsRegular() {
		return Snapshot{}, ErrNotRegular
	}
	if private {
		if err := checkPrivateOpenFile(file, info); err != nil {
			return Snapshot{}, err
		}
	}
	if info.Size() > limit {
		return Snapshot{}, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("read secure file: %w", err)
	}
	if int64(len(data)) > limit {
		return Snapshot{}, ErrTooLarge
	}
	after, err := file.Stat()
	if err != nil {
		return Snapshot{}, fmt.Errorf("reinspect secure file: %w", err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(info, after) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) || int64(len(data)) != after.Size() {
		return Snapshot{}, ErrChanged
	}
	identity, err := snapshotFileIdentityForPlatform(file, after)
	if err != nil {
		return Snapshot{}, fmt.Errorf("identify secure file: %w", err)
	}
	return Snapshot{Data: data, Size: after.Size(), ModTime: after.ModTime(), Mode: after.Mode(), Identity: identity}, nil
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

func directoryComponents(relative string) ([]string, error) {
	if relative == "." {
		return nil, nil
	}
	return relativeComponents(relative)
}

// Publish atomically creates or replaces a root-confined regular file.
func (r *Root) Publish(ctx context.Context, relative string, data []byte, options PublishOptions) (PublishResult, error) {
	if r == nil || r.file == nil {
		return PublishResult{Outcome: PublishUnchanged}, errors.New("secure root is closed")
	}
	if options.Mode != PublishCreate && options.Mode != PublishReplace {
		return PublishResult{Outcome: PublishUnchanged}, errors.New("secure publish mode is invalid")
	}
	if options.Mode == PublishCreate && options.ExpectedHash != "" || options.Mode == PublishReplace && (options.ExpectedHash == "" || options.ExpectedLimit < 1) {
		return PublishResult{Outcome: PublishUnchanged}, errors.New("secure publish version precondition is invalid")
	}
	if err := ctx.Err(); err != nil {
		return PublishResult{Outcome: PublishUnchanged}, err
	}
	components, err := relativeComponents(relative)
	if err != nil {
		return PublishResult{Outcome: PublishUnchanged}, err
	}
	return publishWithinRootOptions(ctx, r, components, data, options)
}

func snapshotContentHash(data []byte) string {
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
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
			if err := CheckPrivateFile(path); err != nil {
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
	if private {
		if err := protectPrivateOpenFile(temp, false); err != nil {
			return fmt.Errorf("protect secure temporary file ACL: %w", err)
		}
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
	if private {
		if err := CheckPrivateFile(path); err != nil {
			return fmt.Errorf("verify published secure file ACL: %w", err)
		}
	}
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
