package securefile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
)

const CopyMaxBytes int64 = 32 << 20

// CopyPlan freezes metadata and both parent identities, never file contents.
// It is root-bound and single-use. It creates no temporary file or directory.
type CopyPlan struct {
	root                                *Root
	source, destination                 []string
	entry                               ArchiveEntry
	permission                          os.FileMode
	sourceParentID, destinationParentID string
	mu                                  sync.Mutex
	consumed                            bool
}

func (p *CopyPlan) Source() string          { return strings.Join(p.source, "/") }
func (p *CopyPlan) Destination() string     { return strings.Join(p.destination, "/") }
func (p *CopyPlan) Version() string         { return p.entry.Version }
func (p *CopyPlan) Size() int64             { return p.entry.Size }
func (p *CopyPlan) Permission() os.FileMode { return p.permission }

type CopyResult struct {
	Outcome     PublishOutcome
	ContentHash string // Actual streamed bytes; empty unless completed.
}
type copyState struct {
	root                                    *Root
	sourceParent, destinationParent, source *os.File
	handles                                 []*os.File
	entry                                   ArchiveEntry
	permission                              os.FileMode
}

// Hooks are separate from old publication operations for focused fault tests.
var copyRead = func(f *os.File, b []byte) (int, error) { return f.Read(b) }
var copyWrite = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
var copySync = func(f *os.File) error { return f.Sync() }
var copyClose = func(f *os.File) error { return f.Close() }

func (r *Root) PrepareCopy(ctx context.Context, source, destination, expectedVersion string) (plan *CopyPlan, err error) {
	if !validArchiveVersion(expectedVersion) || strings.ToLower(expectedVersion) != expectedVersion {
		return nil, ErrChanged
	}
	src, err := r.archiveSourceComponents(ctx, source)
	if err != nil {
		return nil, err
	}
	dst, err := r.archiveSourceComponents(ctx, destination)
	if err != nil {
		return nil, err
	}
	if len(src) > 64 || len(dst) > 64 || len(source) > 4096 || len(destination) > 4096 {
		return nil, ErrTooLarge
	}
	for _, path := range []string{source, destination} {
		if err := r.CheckArchiveWritePath(ctx, path); err != nil {
			return nil, err
		}
	}
	p := &CopyPlan{root: r, source: src, destination: dst}
	state, err := openCopyState(ctx, r, p)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, state.close())
		if err != nil {
			plan = nil
		}
	}()
	if state.entry.Version != expectedVersion {
		return nil, ErrChanged
	}
	if state.entry.Kind != EntryFile {
		return nil, ErrNotRegular
	}
	if state.entry.Size < 0 || state.entry.Size > CopyMaxBytes {
		return nil, ErrTooLarge
	}
	p.entry, p.permission = state.entry, state.permission.Perm()
	p.sourceParentID, err = mkdirHandleIdentity(state.sourceParent)
	if err != nil {
		return nil, err
	}
	p.destinationParentID, err = mkdirHandleIdentity(state.destinationParent)
	if err != nil {
		return nil, err
	}
	if err = state.verify(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}
func (s *copyState) close() error {
	var err error
	for i := len(s.handles) - 1; i >= 0; i-- {
		err = errors.Join(err, copyClose(s.handles[i]))
	}
	return err
}
func (r *Root) Copy(ctx context.Context, p *CopyPlan) (result CopyResult, err error) {
	result.Outcome = PublishUnchanged
	if r == nil || r.file == nil || p == nil || p.root != r || p.sourceParentID == "" || p.destinationParentID == "" {
		return result, ErrChanged
	}
	p.mu.Lock()
	used := p.consumed
	p.consumed = true
	p.mu.Unlock()
	if used {
		return result, ErrChanged
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	for _, path := range []string{p.Source(), p.Destination()} {
		if err = r.CheckArchiveWritePath(ctx, path); err != nil {
			return result, err
		}
	}
	state, err := openCopyState(ctx, r, p)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := state.close(); closeErr != nil {
			err = errors.Join(err, closeErr)
			if result.Outcome != PublishUnchanged {
				result.Outcome = PublishUnknown
			}
		}
		if result.Outcome == PublishUnknown {
			result.ContentHash = ""
			err = errors.Join(ErrOutcomeUnknown, err)
		}
	}()
	if err = state.verify(ctx, p); err != nil {
		return result, err
	}
	temp, err := state.createTemp()
	if err != nil {
		if errors.Is(err, ErrOutcomeUnknown) {
			result.Outcome = PublishUnknown
		}
		return result, err
	}
	cleanup := true
	defer func() {
		var closeErr error
		if cleanup {
			closeErr = state.cleanupTemp(p, temp)
		} else {
			closeErr = copyClose(temp)
		}
		if closeErr != nil {
			result.Outcome = PublishUnknown
			err = errors.Join(err, closeErr)
		}
	}()
	hash, err := streamCopy(ctx, state.source, temp, p.entry.Size)
	if err != nil {
		return result, err
	}
	if err = copySetPermission(temp, p.permission); err != nil {
		return result, err
	}
	if err = copySync(temp); err != nil {
		return result, err
	}
	if err = state.verify(ctx, p); err != nil {
		return result, err
	}
	if err = state.verifyTemp(temp); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	result.Outcome, err = state.publishTemp(temp, p.destination[len(p.destination)-1])
	// An uncertain rename might already have published: never delete its handle.
	if result.Outcome != PublishUnchanged {
		cleanup = false
	}
	if err != nil {
		return result, err
	}
	// Late cancellation cannot erase a published result. Recheck locations without
	// the cancelled context; failures after publication are facts, not rollback.
	if err = state.verifyPublished(context.WithoutCancel(ctx), p); err != nil {
		result.Outcome = PublishUnknown
		return result, err
	}
	if err = state.verifyPublication(temp, p.destination[len(p.destination)-1]); err != nil {
		result.Outcome = PublishUnknown
		return result, err
	}
	if err = state.syncParent(); err != nil {
		result.Outcome = PublishUnknown
		return result, err
	}
	result.ContentHash = hash
	return result, nil
}
func streamCopy(ctx context.Context, source, target *os.File, size int64) (string, error) {
	if size < 0 || size > CopyMaxBytes {
		return "", ErrTooLarge
	}
	buffer := make([]byte, 32<<10)
	hash := sha256.New()
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// Read at most the frozen length + 1, detecting growth without allocating it.
		want := min(int64(len(buffer)), size-total+1)
		n, readErr := copyRead(source, buffer[:int(want)])
		if int64(n) > size-total {
			return "", ErrChanged
		}
		if n > 0 {
			for off := 0; off < n; {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				written, err := copyWrite(target, buffer[off:n])
				if err != nil {
					return "", err
				}
				if written == 0 {
					return "", io.ErrShortWrite
				}
				off += written
			}
			_, _ = hash.Write(buffer[:n])
			total += int64(n)
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return "", readErr
			}
			break
		}
		if n == 0 {
			return "", io.ErrNoProgress
		}
	}
	if total != size {
		return "", ErrChanged
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), ctx.Err()
}
