//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"io"
	"os"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// Separate hooks keep directory fault injection independent of ordinary Copy.
var copyTreeMkdirUnix = unix.Mkdirat
var copyTreeOpenUnix = unix.Openat
var copyTreeRenameUnix = renameArchiveNoReplace
var copyTreeSyncUnix = unix.Fsync
var copyTreeClose = func(f *os.File) error { return f.Close() }

const copyTreeItemOverhead int64 = 1024
const copyTreeSettleTimeout = 5 * time.Second

type copyTreeDirectory struct {
	file *os.File
	id   string
}

type copyTreeState struct {
	root   *Root
	plan   *CopyTreePlan
	guards []*DirectoryChangeGuard
	dirs   map[string]copyTreeDirectory
	order  []string
}

func closeCopyTreeGuards(guards []*DirectoryChangeGuard) error {
	var err error
	for i := len(guards) - 1; i >= 0; i-- {
		err = errors.Join(err, guards[i].Close())
	}
	if err != nil {
		return errors.Join(errCopyTreeCleanup, err)
	}
	return nil
}

func prepareCopyTreeWithinRoot(ctx context.Context, r *Root, source, destination, version string, limits CopyTreeLimits) (plan *CopyTreePlan, err error) {
	plan, guards, err := scanCopyTree(ctx, r, source, destination, version, limits)
	err = errors.Join(err, closeCopyTreeGuards(guards))
	if err != nil {
		return nil, err
	}
	return plan, nil
}

// scanCopyTree is read-only. Guards cover enumeration through the last metadata
// check; callers either close them before authorization or keep them during this
// one execution. No watches survive in CopyTreePlan.
func scanCopyTree(ctx context.Context, r *Root, source, destination, version string, limits CopyTreeLimits) (plan *CopyTreePlan, guards []*DirectoryChangeGuard, err error) {
	p := &CopyTreePlan{root: r, source: source, destination: destination, version: version, limits: limits}
	rootNode, err := readCopyTreeNode(ctx, r, source, destination)
	if err != nil {
		return nil, guards, err
	}
	if rootNode.entry.Kind != EntryDirectory {
		return nil, guards, ErrNotDirectory
	}
	if rootNode.entry.Version != version {
		return nil, guards, ErrChanged
	}
	if _, err = r.InspectArchiveSource(ctx, source); err != nil {
		return nil, guards, err // Also reject a filesystem alias of the root.
	}
	parent, err := openMkdirDirectory(ctx, r, copyTreeParts(copyTreeParent(destination)))
	if err != nil {
		return nil, guards, err
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	p.destinationParentID, err = mkdirHandleIdentity(parent)
	if err != nil {
		return nil, guards, err
	}
	// Reject physical aliases of source descendants, not just textual prefixes.
	sourceDir, err := openUnixStatParent(ctx, r, copyTreeParts(source))
	if err != nil {
		return nil, guards, err
	}
	locationErr := verifyUnixArchiveLocation(r.file, parent, sourceDir)
	if err = errors.Join(locationErr, sourceDir.Close()); err != nil {
		return nil, guards, err
	}
	if err = checkCopyTreeTarget(ctx, r, destination); err != nil {
		return nil, guards, err
	}
	seen := make(map[string]bool)
	directoryIDs := make(map[string]bool)
	add := func(node copyTreeNode) error {
		cost := copyTreeItemOverhead + int64(len(node.item.Source)+len(node.item.Destination)+len(node.item.Version)+len(node.sourceParentID)+len(node.entry.Identity))
		if len(p.nodes) >= limits.Entries || cost > limits.PlanBytes-p.planBytes {
			return ErrTooLarge
		}
		if node.entry.Kind == EntryFile {
			if node.entry.Size < 0 || node.entry.Size > limits.Bytes-p.bytes {
				return ErrTooLarge
			}
			p.bytes += node.entry.Size
		} else {
			if directoryIDs[node.entry.Identity] {
				return ErrChanged // No directory cycles/bind aliases in a frozen tree.
			}
			directoryIDs[node.entry.Identity] = true
		}
		key := copyTreeFold(node.item.Destination)
		if seen[key] {
			return ErrAlreadyExists
		}
		seen[key] = true
		p.planBytes += cost
		p.nodes = append(p.nodes, node)
		return nil
	}
	if err = add(rootNode); err != nil {
		return nil, guards, err
	}
	for index := 0; index < len(p.nodes); index++ {
		node := p.nodes[index]
		if node.entry.Kind != EntryDirectory {
			continue
		}
		scan, openErr := r.OpenDirectoryScan(ctx, node.item.Source)
		if openErr != nil {
			return nil, guards, openErr
		}
		scanErr := func() (err error) {
			defer func() { err = errors.Join(err, scan.Close()) }()
			if scan.Entry() != node.entry {
				return ErrChanged
			}
			for {
				entries, _, done, nextErr := scan.Next(ctx, min(128, limits.Entries-len(p.nodes)+1))
				if nextErr != nil {
					return nextErr
				}
				for _, entry := range entries {
					if len(p.nodes) >= limits.Entries {
						return ErrTooLarge
					}
					child, childErr := readCopyTreeNode(ctx, r, node.item.Source+"/"+entry.Name, node.item.Destination+"/"+entry.Name)
					if childErr != nil {
						return childErr
					}
					if childErr = add(child); childErr != nil {
						return childErr
					}
				}
				if done {
					guard, detachErr := scan.DetachChangeGuard()
					if detachErr != nil {
						return detachErr
					}
					guards = append(guards, guard)
					return nil
				}
			}
		}()
		if scanErr != nil {
			return nil, guards, scanErr
		}
	}
	// Global bytewise order is deterministic and puts every parent before children.
	sort.Slice(p.nodes, func(i, j int) bool { return p.nodes[i].item.Source < p.nodes[j].item.Source })
	if err = verifyCopyTreeSources(ctx, r, p, guards); err != nil {
		return nil, guards, err
	}
	if err = verifyMkdirParent(ctx, r, parent, copyTreeParts(copyTreeParent(destination)), p.destinationParentID); err != nil {
		return nil, guards, err
	}
	if err = checkCopyTreeTarget(ctx, r, destination); err != nil {
		return nil, guards, err
	}
	return p, guards, nil
}

func readCopyTreeNode(ctx context.Context, r *Root, source, destination string) (node copyTreeNode, err error) {
	for _, path := range []string{source, destination} {
		parts, pathErr := r.archiveSourceComponents(ctx, path)
		if pathErr != nil {
			return node, pathErr
		}
		if len(path) > 4096 || len(parts) > 64 {
			return node, ErrTooLarge
		}
		if pathErr = r.CheckArchiveWritePath(ctx, path); pathErr != nil {
			return node, pathErr
		}
	}
	parts := copyTreeParts(source)
	parent, err := openMkdirDirectory(ctx, r, parts[:len(parts)-1])
	if err != nil {
		return node, err
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	node.sourceParentID, err = mkdirHandleIdentity(parent)
	if err != nil {
		return node, err
	}
	stat, err := unixArchiveStat(parent, parts[len(parts)-1])
	if err != nil {
		return node, err
	}
	if _, err = unixArchiveEntry(stat); err != nil {
		return node, err
	}
	node.entry = unixEntryInfo(stat)
	node.permission = os.FileMode(uint32(stat.Mode) & 0777)
	node.item = CopyTreeItem{Source: source, Destination: destination, Kind: node.entry.Kind, Version: node.entry.Version, Size: node.entry.Size}
	if err = verifyMkdirParent(ctx, r, parent, parts[:len(parts)-1], node.sourceParentID); err != nil {
		return node, err
	}
	return node, nil
}

// Check all sibling spellings, even on case-sensitive filesystems. The scan is
// paged, so a large unrelated parent never becomes part of the retained plan.
func checkCopyTreeTarget(ctx context.Context, r *Root, destination string) (err error) {
	if err = r.CheckArchiveWritePath(ctx, destination); err != nil {
		return err
	}
	// Ask the filesystem too: its aliases may include Unicode normalization or
	// mount-specific rules beyond our conservative simple-case-fold policy.
	if _, statErr := r.Stat(ctx, destination); statErr == nil {
		return ErrAlreadyExists
	} else if !errors.Is(statErr, ErrNotFound) {
		return statErr
	}
	scan, err := r.OpenDirectoryScan(ctx, copyTreeParent(destination))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, scan.Close()) }()
	parts := copyTreeParts(destination)
	name := copyTreeFold(parts[len(parts)-1])
	for {
		entries, _, done, nextErr := scan.Next(ctx, 128)
		if nextErr != nil {
			return nextErr
		}
		for _, entry := range entries {
			if copyTreeFold(entry.Name) == name {
				return ErrAlreadyExists
			}
		}
		if done {
			return nil
		}
	}
}

func checkCopyTreeGuards(ctx context.Context, guards []*DirectoryChangeGuard) error {
	for _, guard := range guards {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := guard.Check(); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func verifyCopyTreeSources(ctx context.Context, r *Root, plan *CopyTreePlan, guards []*DirectoryChangeGuard) error {
	for _, node := range plan.nodes {
		current, err := readCopyTreeNode(ctx, r, node.item.Source, node.item.Destination)
		if err != nil {
			return errors.Join(ErrChanged, err)
		}
		if current != node {
			return ErrChanged
		}
	}
	return checkCopyTreeGuards(ctx, guards)
}

func copyTreeWithinRoot(ctx context.Context, r *Root, plan *CopyTreePlan, observer CopyTreeObserver, initial CopyTreeResult) (result CopyTreeResult, err error) {
	result = initial
	state := &copyTreeState{root: r, plan: plan, dirs: make(map[string]copyTreeDirectory)}
	defer func() {
		closeErr := state.close()
		err = errors.Join(err, closeErr)
		result.CleanupIncomplete = result.CleanupIncomplete || errors.Is(err, errCopyTreeCleanup)
		for _, item := range result.Items {
			if item.Outcome == PublishCompleted {
				result.Completed++
			} else if item.Outcome == PublishUnknown {
				result.Unknown++
			}
		}
		switch {
		case err == nil && result.Completed == len(plan.nodes) && !result.CleanupIncomplete:
			result.Outcome = PublishCompleted
		case result.Completed != 0 || result.Unknown != 0 || result.CleanupIncomplete:
			result.Outcome = PublishUnknown
			err = errors.Join(ErrOutcomeUnknown, err)
		default:
			result.Outcome = PublishUnchanged
		}
	}()
	// Re-enumerate everything before Before(0); not even a private staging name
	// is created until the frozen authorization and all preflight checks agree.
	current, guards, err := scanCopyTree(ctx, r, plan.source, plan.destination, plan.version, plan.limits)
	state.guards = guards
	if err != nil {
		return result, err
	}
	if current.destinationParentID != plan.destinationParentID || current.bytes != plan.bytes || len(current.nodes) != len(plan.nodes) {
		return result, ErrChanged
	}
	for i := range plan.nodes {
		if plan.nodes[i] != current.nodes[i] {
			return result, ErrChanged
		}
	}
	parentPath := copyTreeParent(plan.destination)
	parent, err := openMkdirDirectory(ctx, r, copyTreeParts(parentPath))
	if err != nil {
		return result, err
	}
	state.dirs[parentPath] = copyTreeDirectory{file: parent, id: plan.destinationParentID}
	state.order = append(state.order, parentPath)
	if err = state.verifyParents(ctx, plan.destination); err != nil {
		return result, err
	}
	for i, node := range plan.nodes {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if err = observer.Before(ctx, i, node.item); err != nil {
			result.Items[i].Err = err
			return result, err
		}
		itemResult, incomplete := state.execute(ctx, node)
		itemResult.Attempted = true
		result.Items[i] = itemResult
		result.CleanupIncomplete = result.CleanupIncomplete || incomplete
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), copyTreeSettleTimeout)
		afterErr := observer.After(settleCtx, i, node.item, itemResult)
		afterErr = errors.Join(afterErr, settleCtx.Err())
		cancel()
		if err = errors.Join(itemResult.Err, afterErr); err != nil {
			return result, err
		}
		if itemResult.Outcome != PublishCompleted {
			return result, ErrChanged
		}
	}
	// Cancellation after publication must not erase facts. These final checks
	// and resource release must still succeed before the aggregate is completed.
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), copyTreeSettleTimeout)
	defer cancel()
	if err = verifyCopyTreeSources(finalCtx, r, plan, state.guards); err != nil {
		return result, err
	}
	for _, path := range state.order {
		owned := state.dirs[path]
		if err = verifyMkdirParent(finalCtx, r, owned.file, copyTreeParts(path), owned.id); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *copyTreeState) close() error {
	err := closeCopyTreeGuards(s.guards)
	for i := len(s.order) - 1; i >= 0; i-- {
		err = errors.Join(err, copyTreeClose(s.dirs[s.order[i]].file))
	}
	if err != nil {
		return errors.Join(errCopyTreeCleanup, err)
	}
	return nil
}

// Validate every owned ancestor against the original workspace root, not a new
// root made from a freshly created directory or a newly adopted path identity.
func (s *copyTreeState) verifyParents(ctx context.Context, destination string) error {
	anchor := copyTreeParent(s.plan.destination)
	for path := copyTreeParent(destination); ; path = copyTreeParent(path) {
		owned, ok := s.dirs[path]
		if !ok {
			return ErrChanged
		}
		if err := verifyMkdirParent(ctx, s.root, owned.file, copyTreeParts(path), owned.id); err != nil {
			return err
		}
		if path == anchor {
			return nil
		}
		if path == "." {
			return ErrChanged
		}
	}
}

func (s *copyTreeState) verifyItem(ctx context.Context, node copyTreeNode) error {
	if err := s.verifyParents(ctx, node.item.Destination); err != nil {
		return err
	}
	if err := checkCopyTreeGuards(ctx, s.guards); err != nil {
		return err
	}
	current, err := readCopyTreeNode(ctx, s.root, node.item.Source, node.item.Destination)
	if err != nil {
		return errors.Join(ErrChanged, err)
	}
	if current != node {
		return ErrChanged
	}
	return nil
}

func (s *copyTreeState) execute(ctx context.Context, node copyTreeNode) (result CopyTreeItemResult, cleanupIncomplete bool) {
	result.Outcome = PublishUnchanged
	if result.Err = s.verifyItem(ctx, node); result.Err != nil {
		return result, false
	}
	if result.Err = checkCopyTreeTarget(ctx, s.root, node.item.Destination); result.Err != nil {
		return result, false
	}
	if node.entry.Kind == EntryDirectory {
		return s.mkdir(ctx, node)
	}
	parent := s.dirs[copyTreeParent(node.item.Destination)]
	// Construct directly from frozen metadata and our pinned parent identity.
	// Preparing a new ordinary plan here could adopt an attacker's replacement.
	p := &CopyPlan{root: s.root, source: copyTreeParts(node.item.Source), destination: copyTreeParts(node.item.Destination),
		entry: ArchiveEntry{Kind: node.entry.Kind, Size: node.entry.Size, Identity: node.entry.Identity, Version: node.entry.Version},
		limit: s.plan.limits.Bytes, permission: node.permission, sourceParentID: node.sourceParentID, destinationParentID: parent.id}
	copied, err := s.root.Copy(ctx, p)
	result.Outcome, result.ContentHash, result.Err = copied.Outcome, copied.ContentHash, err
	if copied.Outcome == PublishCompleted {
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), copyTreeSettleTimeout)
		checkErr := s.verifyItem(checkCtx, node)
		cancel()
		if checkErr != nil {
			result.Outcome, result.ContentHash = PublishUnknown, ""
			result.Err = errors.Join(ErrOutcomeUnknown, result.Err, checkErr)
		} else {
			result.Bytes = node.entry.Size
		}
	}
	// Ordinary Copy deliberately cannot prove cleanup after unknown publication
	// or ownership loss. Conservatively expose that uncertainty rather than
	// claiming that no private staging entry can remain.
	return result, copied.Outcome == PublishUnknown
}

func (s *copyTreeState) mkdir(ctx context.Context, node copyTreeNode) (result CopyTreeItemResult, incomplete bool) {
	result.Outcome = PublishUnchanged
	parent := s.dirs[copyTreeParent(node.item.Destination)]
	name, err := secureTempName()
	if err != nil {
		result.Err = err
		return result, false
	}
	if result.Err = s.verifyItem(ctx, node); result.Err != nil {
		return result, false
	}
	if err = copyTreeMkdirUnix(int(parent.file.Fd()), name, 0700); err != nil {
		result.Err = archiveUnixError(err)
		if !unixArchiveRenameUnchanged(err) {
			result.Outcome = PublishUnknown
			result.Err = errors.Join(ErrOutcomeUnknown, result.Err)
			incomplete = true
		}
		return result, incomplete
	}
	// Capture the private name before open and compare with the held descriptor.
	// A public-path mkdir followed by adopting whichever entry opens is unsafe.
	stat, err := unixArchiveStat(parent.file, name)
	if err != nil {
		result.Outcome, result.Err = PublishUnknown, errors.Join(ErrOutcomeUnknown, err)
		return result, true // No pin: never blindly unlink a pathname.
	}
	fd, err := copyTreeOpenUnix(int(parent.file.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		result.Outcome, result.Err = PublishUnknown, errors.Join(ErrOutcomeUnknown, archiveUnixError(err))
		return result, true
	}
	temp := os.NewFile(uintptr(fd), name)
	owned, published, retain := false, false, false
	defer func() {
		var cleanupErr error
		if !published {
			if owned {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), copyTreeSettleTimeout)
				cleanupErr = s.verifyParents(cleanupCtx, node.item.Destination)
				if cleanupErr == nil {
					cleanupErr = verifyCopyTreeDirectoryAt(parent.file, name, temp)
				}
				if cleanupErr == nil {
					cleanupErr = unix.Unlinkat(int(parent.file.Fd()), name, unix.AT_REMOVEDIR)
				}
				if cleanupErr == nil {
					cleanupErr = copyTreeSyncUnix(int(parent.file.Fd()))
				}
				cancel()
			} else {
				cleanupErr = errCopyTreeCleanup
			}
		}
		if !retain {
			cleanupErr = errors.Join(cleanupErr, copyTreeClose(temp))
		}
		if cleanupErr != nil {
			incomplete = true
			result.Outcome = PublishUnknown
			result.Err = errors.Join(result.Err, errCopyTreeCleanup, cleanupErr)
		}
		if result.Outcome == PublishUnknown {
			result.Err = errors.Join(ErrOutcomeUnknown, result.Err)
		}
	}()
	same, err := unixArchiveSame(stat, temp)
	if err != nil || !same || unixEntryType(uint32(stat.Mode)) != EntryDirectory {
		result.Err = errors.Join(ErrChanged, err)
		return result, true
	}
	owned = true
	if result.Err = verifyCopyTreeDirectoryAt(parent.file, name, temp); result.Err != nil {
		return result, false
	}
	// Nothing in a new private directory is ours except the empty directory.
	if names, readErr := temp.Readdirnames(1); len(names) != 0 || !errors.Is(readErr, io.EOF) {
		result.Err = errors.Join(ErrChanged, readErr)
		return result, false
	}
	if result.Err = unix.Fchmod(fd, 0700); result.Err != nil {
		return result, false
	}
	id, err := mkdirHandleIdentity(temp)
	if err != nil {
		result.Err = err
		return result, false
	}
	if result.Err = copyTreeSyncUnix(fd); result.Err != nil {
		return result, false
	}
	if result.Err = s.verifyItem(ctx, node); result.Err != nil {
		return result, false
	}
	if result.Err = verifyCopyTreeDirectoryAt(parent.file, name, temp); result.Err != nil {
		return result, false
	}
	if result.Err = ctx.Err(); result.Err != nil {
		return result, false
	}
	parts := copyTreeParts(node.item.Destination)
	err = copyTreeRenameUnix(int(parent.file.Fd()), name, int(parent.file.Fd()), parts[len(parts)-1])
	if err != nil {
		result.Err = archiveUnixError(err)
		if !unixArchiveRenameUnchanged(err) {
			published = true // It may have committed: never unlink either spelling.
			result.Outcome = PublishUnknown
			incomplete = true
		}
		return result, incomplete
	}
	published, retain = true, true
	result.Outcome = PublishCompleted
	s.dirs[node.item.Destination] = copyTreeDirectory{file: temp, id: id}
	s.order = append(s.order, node.item.Destination)
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), copyTreeSettleTimeout)
	defer cancel()
	result.Err = s.verifyItem(checkCtx, node)
	if result.Err == nil {
		result.Err = verifyMkdirParent(checkCtx, s.root, temp, parts, id)
	}
	if result.Err == nil {
		result.Err = copyTreeSyncUnix(fd)
	}
	if result.Err == nil {
		result.Err = copyTreeSyncUnix(int(parent.file.Fd()))
	}
	if result.Err == nil {
		result.Err = s.verifyItem(checkCtx, node)
	}
	if result.Err == nil {
		result.Err = verifyMkdirParent(checkCtx, s.root, temp, parts, id)
	}
	if result.Err != nil {
		result.Outcome = PublishUnknown
	}
	return result, false
}

func verifyCopyTreeDirectoryAt(parent *os.File, name string, file *os.File) error {
	stat, err := unixArchiveStat(parent, name)
	if err != nil {
		return err
	}
	same, err := unixArchiveSame(stat, file)
	if err != nil {
		return err
	}
	if !same || unixEntryType(uint32(stat.Mode)) != EntryDirectory {
		return ErrChanged
	}
	return nil
}
