package operations

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

var candidatePathspecs = []string{
	"server",
	"clients",
	"contracttests",
	"scripts",
	"deploy",
	"docs/comet/changes/operations-hardening/brief.md",
	"docs/comet/changes/operations-hardening/specs/operations-hardening/spec.md",
	"docs/design",
	"Makefile",
	"go.mod",
	"go.sum",
	"go.work",
	"go.work.sum",
	".go-version",
	".tool-versions",
}

func CandidateFingerprint(root, candidateID string, excludedPaths ...string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	excluded := make([]string, 0, len(excludedPaths))
	for _, path := range excludedPaths {
		absolute, absErr := filepath.Abs(path)
		if absErr != nil {
			return "", absErr
		}
		excluded = append(excluded, filepath.Clean(absolute))
	}

	head := ""
	paths := []string{}
	if command := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree"); command.Run() == nil {
		output, outputErr := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
		if outputErr != nil {
			return "", fmt.Errorf("read candidate HEAD: %w", outputErr)
		}
		head = strings.TrimSpace(string(output))
		args := []string{"-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--"}
		args = append(args, candidatePathspecs...)
		listed, listErr := exec.Command("git", args...).Output()
		if listErr != nil {
			return "", fmt.Errorf("list candidate inputs: %w", listErr)
		}
		for _, path := range strings.Split(string(listed), "\x00") {
			if path != "" {
				paths = append(paths, filepath.ToSlash(path))
			}
		}
	} else {
		if strings.TrimSpace(candidateID) == "" {
			return "", errors.New("CANDIDATE_ID is required without Git metadata")
		}
		head = "archive:" + candidateID
		paths, err = walkCandidatePaths(root)
		if err != nil {
			return "", err
		}
	}
	sort.Strings(paths)

	hash := sha256.New()
	writeHashPart(hash, "candidate-fingerprint-v1")
	writeHashPart(hash, head)
	for _, path := range paths {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if pathExcluded(absolute, excluded) {
			continue
		}
		writeHashPart(hash, path)
		info, statErr := os.Lstat(absolute)
		if errors.Is(statErr, os.ErrNotExist) {
			writeHashPart(hash, "missing")
			continue
		}
		if statErr != nil {
			return "", fmt.Errorf("stat candidate input %s: %w", path, statErr)
		}
		writeHashPart(hash, fmt.Sprintf("mode:%o", info.Mode()&0o111))
		switch {
		case info.Mode().IsRegular():
			file, openErr := os.Open(absolute)
			if openErr != nil {
				return "", openErr
			}
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil {
				return "", copyErr
			}
			if closeErr != nil {
				return "", closeErr
			}
			writeHashPart(hash, "end-file")
		case info.Mode()&os.ModeSymlink != 0:
			target, readErr := os.Readlink(absolute)
			if readErr != nil {
				return "", readErr
			}
			writeHashPart(hash, "symlink:"+target)
		default:
			return "", fmt.Errorf("unsupported candidate input type: %s", path)
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func walkCandidatePaths(root string) ([]string, error) {
	var paths []string
	for _, pathspec := range candidatePathspecs {
		absolute := filepath.Join(root, filepath.FromSlash(pathspec))
		info, err := os.Lstat(absolute)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			paths = append(paths, filepath.ToSlash(pathspec))
			continue
		}
		err = filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			paths = append(paths, filepath.ToSlash(relative))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func HashTree(path string) (string, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	var paths []string
	err = filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, relErr := filepath.Rel(root, current)
		if relErr != nil {
			return relErr
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	hash := sha256.New()
	writeHashPart(hash, "tree-v1")
	for _, relative := range paths {
		writeHashPart(hash, relative)
		file, openErr := os.Open(filepath.Join(root, filepath.FromSlash(relative)))
		if openErr != nil {
			return "", openErr
		}
		if _, copyErr := io.Copy(hash, file); copyErr != nil {
			_ = file.Close()
			return "", copyErr
		}
		if closeErr := file.Close(); closeErr != nil {
			return "", closeErr
		}
		writeHashPart(hash, "end-file")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeHashPart(writer io.Writer, value string) {
	_, _ = io.WriteString(writer, fmt.Sprintf("%d:", len(value)))
	_, _ = io.WriteString(writer, value)
}

func pathExcluded(path string, excluded []string) bool {
	path = filepath.Clean(path)
	for _, value := range excluded {
		if path == value || strings.HasPrefix(path, value+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func ReadSelectedTests(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var selected []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
			return nil, errors.New("selected test file must contain package<TAB>test lines")
		}
		selected = append(selected, fields[0]+"::"+fields[1])
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	selected = sortedUnique(selected)
	if len(selected) == 0 {
		return nil, errors.New("selected test file is empty")
	}
	return selected, nil
}
