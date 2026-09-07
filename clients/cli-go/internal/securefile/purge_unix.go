//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Purge-specific fault boundaries never grant archive permission to other APIs.
var (
	purgeOpenUnix      = unix.Openat
	purgeUnlinkUnix    = unix.Unlinkat
	purgeSyncUnix      = unix.Fsync
	purgeClose         = func(f *os.File) error { return f.Close() }
	purgeMountUnix     = purgeNativeMount
	purgeWatchUnix     = newDirectoryChangeGuard
	purgePostcheckUnix = verifyPurgeDeleted
)

const purgeMetadataOverhead int64 = 1024
const purgeSettleTimeout = 5 * time.Second

type purgeDirectory struct {
	path, id string
	file     *os.File
}

type purgePath struct {
	root *Root
	plan *PurgePlan
	dirs []purgeDirectory // workspace through immediate parent, in that order
	file *os.File         // Entry itself, opened without following links or reading data.
	name string
	node purgeNode
}

type purgeScan struct {
	plan       *PurgePlan
	guards     []*DirectoryChangeGuard
	identities map[string]string
}

func purgeCloseFile(file *os.File) error {
	if file == nil {
		return nil
	}
	if err := purgeClose(file); err != nil {
		return errors.Join(errPurgeCleanup, err)
	}
	return nil
}

func (s *purgePath) close() error {
	file := s.file
	s.file = nil
	err := purgeCloseFile(file)
	for i := len(s.dirs) - 1; i >= 0; i-- {
		file = s.dirs[i].file
		s.dirs[i].file = nil
		err = errors.Join(err, purgeCloseFile(file))
	}
	s.dirs = nil
	return err
}

func (s *purgeScan) close() error {
	var err error
	for i := len(s.guards) - 1; i >= 0; i-- {
		err = errors.Join(err, s.guards[i].Close())
	}
	s.guards = nil
	if err != nil {
		return errors.Join(errPurgeCleanup, err)
	}
	return nil
}

func purgeFDInfo(file *os.File) (EntryInfo, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return EntryInfo{}, archiveUnixError(err)
	}
	return unixEntryInfo(stat), nil
}

func purgeCheckMount(file *os.File, want string) error {
	mount, err := purgeMountUnix(file)
	if err != nil {
		return err
	}
	if mount == "" {
		return ErrArchiveUnsupported
	}
	if mount != want {
		return ErrCrossDevice
	}
	return nil
}

func (s *purgeScan) directory(path string, file *os.File, info EntryInfo) error {
	if id, ok := s.plan.directories[path]; ok {
		if id != info.Identity {
			return ErrChanged
		}
		return nil
	}
	if _, exists := s.identities[info.Identity]; exists {
		return ErrArchiveProtected
	}
	cost := purgeMetadataOverhead + int64(len(path)+len(info.Identity))
	if cost > s.plan.limits.PlanBytes-s.plan.planBytes {
		return ErrTooLarge
	}
	guard, err := purgeWatchUnix(file)
	if err != nil {
		return err
	}
	s.guards = append(s.guards, guard)
	s.identities[info.Identity] = path
	s.plan.directories[path] = info.Identity
	s.plan.planBytes += cost
	return nil
}

func (p *PurgePlan) checkDirectory(path string, file *os.File, scan *purgeScan) (string, error) {
	info, err := purgeFDInfo(file)
	if err != nil {
		return "", err
	}
	if info.Kind != EntryDirectory {
		return "", ErrNotDirectory
	}
	if err := purgeCheckMount(file, p.mount); err != nil {
		return "", err
	}
	if scan != nil {
		if err := scan.directory(path, file, info); err != nil {
			return "", err
		}
	} else if p.directories[path] != info.Identity {
		return "", ErrChanged
	}
	return info.Identity, nil
}

// Every chain is anchored to the original workspace descriptor. The canonical
// archive and every subsequent directory have a frozen named identity. Keeping
// at most one bounded-depth chain avoids retaining a descriptor per file.
func openPurgePath(ctx context.Context, r *Root, p *PurgePlan, path string, scan *purgeScan) (_ *purgePath, err error) {
	parts, err := purgeComponents(path)
	if err != nil {
		return nil, err
	}
	s := &purgePath{root: r, plan: p, name: parts[len(parts)-1]}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.close())
		}
	}()
	fd, err := purgeOpenUnix(int(r.file.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, archiveUnixError(err)
	}
	parent := os.NewFile(uintptr(fd), ".")
	s.dirs = append(s.dirs, purgeDirectory{path: ".", file: parent})
	id, err := p.checkDirectory(".", parent, scan)
	if err != nil {
		return nil, err
	}
	s.dirs[0].id = id
	for i, name := range parts[:len(parts)-1] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		stat, err := unixArchiveStat(parent, name)
		if err != nil {
			return nil, err
		}
		info := unixEntryInfo(stat)
		if info.Kind == EntryLink {
			return nil, ErrLink
		}
		if info.Kind != EntryDirectory {
			return nil, ErrNotDirectory
		}
		fd, err := purgeOpenUnix(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, archiveUnixError(err)
		}
		parent = os.NewFile(uintptr(fd), name)
		dirPath := strings.Join(parts[:i+1], "/")
		s.dirs = append(s.dirs, purgeDirectory{path: dirPath, file: parent})
		opened, err := purgeFDInfo(parent)
		if err != nil {
			return nil, err
		}
		if opened != info {
			return nil, ErrChanged
		}
		id, err := p.checkDirectory(dirPath, parent, scan)
		if err != nil {
			return nil, err
		}
		s.dirs[len(s.dirs)-1].id = id
	}
	stat, err := unixArchiveStat(parent, s.name)
	if err != nil {
		return nil, err
	}
	info := unixEntryInfo(stat)
	if info.Kind != EntryFile && info.Kind != EntryDirectory && info.Kind != EntryLink {
		return nil, ErrNotRegular
	}
	fd, err = purgeOpenUnix(int(parent.Fd()), s.name, purgeEntryOpenFlags(info.Kind), 0)
	if err != nil {
		return nil, archiveUnixError(err)
	}
	s.file = os.NewFile(uintptr(fd), s.name)
	if err := purgeCheckMount(s.file, p.mount); err != nil {
		return nil, err
	}
	if info.Kind == EntryDirectory {
		if _, err := p.checkDirectory(path, s.file, scan); err != nil {
			return nil, err
		}
	}
	s.node = purgeNode{item: PurgeItem{Path: path, Kind: info.Kind, Version: info.Version, Size: info.Size}, entry: info, parentID: s.dirs[len(s.dirs)-1].id}
	if err := s.verify(ctx, s.node, false); err != nil {
		return nil, err
	}
	return s, nil
}

// Verify both the name from each held parent and the child's real '..'. This
// catches a held ancestor moved outside the archive or to a different spelling;
// all objects, including '..', must also retain the frozen mount identity.
func (s *purgePath) verifyParents(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rootInfo, err := purgeFDInfo(s.root.file)
	if err != nil {
		return err
	}
	if rootInfo.Identity != s.plan.directories["."] || rootInfo.Kind != EntryDirectory {
		return ErrChanged
	}
	if err := purgeCheckMount(s.root.file, s.plan.mount); err != nil {
		return err
	}
	for i, dir := range s.dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := purgeFDInfo(dir.file)
		if err != nil {
			return err
		}
		if info.Identity != dir.id || info.Kind != EntryDirectory || s.plan.directories[dir.path] != dir.id {
			return ErrChanged
		}
		if err := purgeCheckMount(dir.file, s.plan.mount); err != nil {
			return err
		}
		if i == 0 {
			continue
		}
		parent := s.dirs[i-1]
		name := dir.path[strings.LastIndexByte(dir.path, '/')+1:]
		stat, err := unixArchiveStat(parent.file, name)
		if err != nil {
			return errors.Join(ErrChanged, err)
		}
		named := unixEntryInfo(stat)
		if named.Kind != EntryDirectory || named.Identity != dir.id {
			return ErrChanged
		}
		if err := purgeVerifyNamed(parent.file, name, info, s.plan.mount, true); err != nil {
			return err
		}
		if err := purgeVerifyParent(dir.file, parent.file, parent.id, s.plan.mount); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func purgeVerifyParent(file, parent *os.File, parentID, mount string) (err error) {
	fd, err := purgeOpenUnix(int(file.Fd()), "..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return archiveUnixError(err)
	}
	up := os.NewFile(uintptr(fd), "..")
	defer func() { err = errors.Join(err, purgeCloseFile(up)) }()
	info, err := purgeFDInfo(up)
	if err != nil {
		return err
	}
	if info.Kind != EntryDirectory || info.Identity != parentID {
		return ErrOutsideRoot
	}
	if err := purgeCheckMount(up, mount); err != nil {
		return err
	}
	// Retain the parent's descriptor through the identity check.
	other, err := purgeFDInfo(parent)
	if err != nil {
		return err
	}
	if other.Identity != parentID {
		return ErrChanged
	}
	return nil
}

func (s *purgePath) verify(ctx context.Context, node purgeNode, directoryAfterChildren bool) error {
	if err := s.verifyParents(ctx); err != nil {
		return err
	}
	parent := s.dirs[len(s.dirs)-1]
	if parent.id != node.parentID {
		return ErrChanged
	}
	if err := purgeCheckMount(s.file, s.plan.mount); err != nil {
		return err
	}
	opened, err := purgeFDInfo(s.file)
	if err != nil {
		return err
	}
	matches := func(info EntryInfo) bool {
		if directoryAfterChildren && node.entry.Kind == EntryDirectory {
			return info.Identity == node.entry.Identity && info.Kind == EntryDirectory
		}
		return info == node.entry
	}
	if !matches(opened) {
		return ErrChanged
	}
	if node.entry.Kind == EntryDirectory {
		if err := purgeVerifyParent(s.file, parent.file, parent.id, s.plan.mount); err != nil {
			return err
		}
	}
	stat, err := unixArchiveStat(parent.file, s.name)
	if err != nil {
		return errors.Join(ErrChanged, err)
	}
	if !matches(unixEntryInfo(stat)) {
		return ErrChanged
	}
	if err := purgeVerifyNamed(parent.file, s.name, node.entry, s.plan.mount, directoryAfterChildren); err != nil {
		return err
	}
	return ctx.Err()
}

// A same-inode bind mount can leave stat identity unchanged. Reopen the named
// object too, proving its native mount rather than only the older held handle's.
func purgeVerifyNamed(parent *os.File, name string, expected EntryInfo, mount string, relaxDirectory bool) (err error) {
	fd, err := purgeOpenUnix(int(parent.Fd()), name, purgeEntryOpenFlags(expected.Kind), 0)
	if err != nil {
		return archiveUnixError(err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { err = errors.Join(err, purgeCloseFile(file)) }()
	if err := purgeCheckMount(file, mount); err != nil {
		return err
	}
	info, err := purgeFDInfo(file)
	if err != nil {
		return err
	}
	if relaxDirectory && expected.Kind == EntryDirectory {
		if info.Identity != expected.Identity || info.Kind != EntryDirectory {
			return ErrChanged
		}
	} else if info != expected {
		return ErrChanged
	}
	return nil
}

func (s *purgeScan) readNode(ctx context.Context, path string) (node purgeNode, err error) {
	opened, err := openPurgePath(ctx, s.plan.root, s.plan, path, s)
	if err != nil {
		return node, err
	}
	node = opened.node
	return node, opened.close()
}

func (s *purgeScan) add(node purgeNode) error {
	p := s.plan
	cost := purgeMetadataOverhead + int64(len(node.item.Path)+len(node.item.Version)+len(node.entry.Identity)+len(node.parentID))
	if len(p.nodes) >= p.limits.Entries || cost > p.limits.PlanBytes-p.planBytes {
		return ErrTooLarge
	}
	if node.entry.Kind == EntryFile {
		if node.entry.Size < 0 || node.entry.Size > math.MaxInt64-p.bytes {
			return ErrTooLarge
		}
		p.bytes += node.entry.Size
	}
	p.planBytes += cost
	p.nodes = append(p.nodes, node)
	return nil
}

func (s *purgeScan) enumerate(ctx context.Context, node purgeNode) (err error) {
	opened, err := openPurgePath(ctx, s.plan.root, s.plan, node.item.Path, s)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, opened.close()) }()
	if opened.node != node {
		return ErrChanged
	}
	for {
		if err := opened.verify(ctx, node, false); err != nil {
			return err
		}
		names, readErr := opened.file.Readdirnames(min(128, s.plan.limits.Entries-len(s.plan.nodes)+1))
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, name := range names {
			if len(s.plan.nodes) >= s.plan.limits.Entries {
				return ErrTooLarge
			}
			child, err := s.readNode(ctx, node.item.Path+"/"+name)
			if err != nil {
				return err
			}
			if err := s.add(child); err != nil {
				return err
			}
		}
		if err := opened.verify(ctx, node, false); err != nil {
			return err
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

// All native guards span enumeration and the final full metadata check. This
// detects same-tick directory mutations which mtime/ctime alone cannot prove.
func scanPurge(ctx context.Context, r *Root, path, version string, limits PurgeLimits) (plan *PurgePlan, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mount, err := purgeMountUnix(r.file)
	if err != nil {
		return nil, err
	}
	if mount == "" {
		return nil, ErrArchiveUnsupported
	}
	p := &PurgePlan{root: r, path: path, version: version, limits: limits, mount: mount, directories: make(map[string]string)}
	scan := &purgeScan{plan: p, identities: make(map[string]string)}
	defer func() {
		err = errors.Join(err, scan.close())
		if err != nil {
			plan = nil
		}
	}()
	node, err := scan.readNode(ctx, path)
	if err != nil {
		return nil, err
	}
	if node.entry.Version != version {
		return nil, ErrChanged
	}
	p.identity, p.kind = node.entry.Identity, node.entry.Kind
	if err := scan.add(node); err != nil {
		return nil, err
	}
	for i := 0; i < len(p.nodes); i++ {
		if p.nodes[i].entry.Kind == EntryDirectory {
			if err := scan.enumerate(ctx, p.nodes[i]); err != nil {
				return nil, err
			}
		}
	}
	// Reverse bytewise path order is deterministic postorder: each descendant
	// has its parent's path as a strict prefix, so all descendants come first.
	sort.Slice(p.nodes, func(i, j int) bool { return p.nodes[i].item.Path > p.nodes[j].item.Path })
	for i, node := range p.nodes {
		if i > 0 && p.nodes[i-1].item.Path == node.item.Path {
			return nil, ErrChanged
		}
		current, err := scan.readNode(ctx, node.item.Path)
		if err != nil {
			return nil, errors.Join(ErrChanged, err)
		}
		if current != node {
			return nil, ErrChanged
		}
	}
	for _, guard := range scan.guards {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := guard.Check(); err != nil {
			return nil, err
		}
	}
	return p, ctx.Err()
}

func preparePurgeWithinRoot(ctx context.Context, r *Root, path, version string, limits PurgeLimits) (*PurgePlan, error) {
	p, err := scanPurge(ctx, r, path, version, limits)
	if err != nil {
		return nil, err
	}
	p.use = &purgeUse{}
	return p, nil
}

func samePurgePlan(a, b *PurgePlan) bool {
	if a.mount != b.mount || a.bytes != b.bytes || a.kind != b.kind || a.identity != b.identity || len(a.nodes) != len(b.nodes) || len(a.directories) != len(b.directories) {
		return false
	}
	for i := range a.nodes {
		if a.nodes[i] != b.nodes[i] {
			return false
		}
	}
	for path, id := range a.directories {
		if b.directories[path] != id {
			return false
		}
	}
	return true
}

func purgeWithinRoot(ctx context.Context, r *Root, p *PurgePlan, observer PurgeObserver, initial PurgeResult) (result PurgeResult, err error) {
	result = initial
	defer func() {
		result.CleanupIncomplete = result.CleanupIncomplete || errors.Is(err, errPurgeCleanup)
		for _, item := range result.Items {
			switch item.Outcome {
			case PublishCompleted:
				result.Completed++
			case PublishUnknown:
				result.Unknown++
			}
		}
		switch {
		case err == nil && result.Completed == len(p.nodes) && !result.CleanupIncomplete:
			result.Outcome = PublishCompleted
		case result.Completed > 0 || result.Unknown > 0 || result.CleanupIncomplete:
			result.Outcome = PublishUnknown
			err = errors.Join(ErrOutcomeUnknown, err)
		default:
			result.Outcome = PublishUnchanged
		}
	}()
	// This complete second enumeration, resource acquisition and resource close
	// must succeed before the first intent. Nothing is held across authorization.
	current, err := scanPurge(ctx, r, p.path, p.version, p.limits)
	if err != nil {
		return result, err
	}
	if !samePurgePlan(p, current) {
		return result, ErrChanged
	}
	for i, node := range p.nodes {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := observer.Before(ctx, i, node.item); err != nil {
			return result, err
		}
		item := executePurgeItem(ctx, r, p, node)
		item.Attempted = true
		result.Items[i] = item
		result.CleanupIncomplete = result.CleanupIncomplete || errors.Is(item.Err, errPurgeCleanup)
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), purgeSettleTimeout)
		afterErr := observer.After(settleCtx, i, node.item, item)
		afterErr = errors.Join(afterErr, settleCtx.Err())
		cancel()
		if err := errors.Join(item.Err, afterErr); err != nil {
			return result, err
		}
		if item.Outcome != PublishCompleted {
			return result, ErrChanged
		}
	}
	return result, nil
}

func executePurgeItem(ctx context.Context, r *Root, p *PurgePlan, node purgeNode) (result PurgeItemResult) {
	result.Outcome = PublishUnchanged
	opened, err := openPurgePath(ctx, r, p, node.item.Path, nil)
	if err != nil {
		result.Err = err
		return result
	}
	defer func() {
		if err := opened.close(); err != nil {
			result.Err = errors.Join(result.Err, err)
			if result.Outcome != PublishUnchanged {
				result.Outcome = PublishUnknown
			}
		}
		if result.Outcome == PublishUnknown {
			result.Bytes = 0
			result.Err = errors.Join(ErrOutcomeUnknown, result.Err)
		}
	}()
	// Directory metadata was necessarily changed by our successful child
	// unlinks. Only identity/type are relaxed; rmdir itself must prove emptiness.
	if result.Err = opened.verify(ctx, node, true); result.Err != nil {
		return result
	}
	parent := opened.dirs[len(opened.dirs)-1].file
	flags := 0
	if node.entry.Kind == EntryDirectory {
		flags = unix.AT_REMOVEDIR
	}
	// The final cancellation check is adjacent to the irreversible primitive.
	// There is still a non-cooperative cross-process name-replacement window.
	if result.Err = ctx.Err(); result.Err != nil {
		return result
	}
	if err := purgeUnlinkUnix(int(parent.Fd()), opened.name, flags); err != nil {
		result.Err = archiveUnixError(err)
		if !unixArchiveRenameUnchanged(err) {
			result.Outcome = PublishUnknown
		}
		return result
	}
	result.Outcome = PublishCompleted
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), purgeSettleTimeout)
	defer cancel()
	if result.Err = purgePostcheckUnix(checkCtx, opened); result.Err == nil {
		result.Err = purgeSyncUnix(int(parent.Fd()))
	}
	if result.Err == nil {
		result.Err = purgePostcheckUnix(checkCtx, opened)
	}
	if result.Err != nil {
		result.Outcome = PublishUnknown
		return result
	}
	if node.entry.Kind == EntryFile {
		result.Bytes = node.entry.Size
	}
	return result
}

func verifyPurgeDeleted(ctx context.Context, opened *purgePath) error {
	if err := opened.verifyParents(ctx); err != nil {
		return err
	}
	_, err := unixArchiveStat(opened.dirs[len(opened.dirs)-1].file, opened.name)
	if !errors.Is(err, ErrNotFound) {
		return errors.Join(ErrChanged, err)
	}
	return ctx.Err()
}
