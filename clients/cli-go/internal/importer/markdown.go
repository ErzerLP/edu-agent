package importer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	xcases "golang.org/x/text/cases"
	xnorm "golang.org/x/text/unicode/norm"
)

const (
	MaxDocuments    = 1000
	MaxDocumentSize = 4 << 20
	MaxRequestSize  = 16 << 20
	MaxPathRunes    = 512
	MaxPathBytes    = 1024
)

type Batch struct {
	Documents []api.ImportDocument
}

func Load(path string) (Batch, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Batch{}, fmt.Errorf("inspect import path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Batch{}, errors.New("import path must not be a symlink")
	}
	var documents []api.ImportDocument
	if info.Mode().IsRegular() {
		if filepath.Ext(path) != ".md" {
			return Batch{}, errors.New("import file must use the .md extension")
		}
		root, err := securefile.OpenRoot(filepath.Dir(path))
		if err != nil {
			return Batch{}, fmt.Errorf("open import root: %w", err)
		}
		defer root.Close()
		document, err := readDocumentFromRoot(filepath.Base(path), root, filepath.Base(path), nil)
		if err != nil {
			return Batch{}, err
		}
		documents = append(documents, document)
	} else if info.IsDir() {
		root, err := securefile.OpenRoot(path)
		if err != nil {
			return Batch{}, fmt.Errorf("open import root: %w", err)
		}
		defer root.Close()
		err = filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			entryInfo, err := entry.Info()
			if err != nil {
				return err
			}
			if entryInfo.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink is not allowed: %s", entry.Name())
			}
			if entry.IsDir() {
				return nil
			}
			if filepath.Ext(entry.Name()) != ".md" {
				return nil
			}
			if !entryInfo.Mode().IsRegular() {
				return fmt.Errorf("Markdown path is not a regular file: %s", entry.Name())
			}
			relative, err := filepath.Rel(path, current)
			if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return errors.New("import path escapes its root")
			}
			canonicalPath := filepath.ToSlash(relative)
			document, err := readDocumentFromRoot(canonicalPath, root, canonicalPath, nil)
			if err != nil {
				return err
			}
			documents = append(documents, document)
			if len(documents) > MaxDocuments {
				return fmt.Errorf("import contains more than %d Markdown documents", MaxDocuments)
			}
			return nil
		})
		if err != nil {
			return Batch{}, err
		}
	} else {
		return Batch{}, errors.New("import path must be a regular Markdown file or directory")
	}
	if len(documents) == 0 {
		return Batch{}, errors.New("import directory contains no Markdown documents")
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].Path < documents[j].Path })
	seen := make(map[string]string, len(documents))
	fold := xcases.Fold()
	for _, document := range documents {
		key := fold.String(document.Path)
		if previous, ok := seen[key]; ok {
			return Batch{}, fmt.Errorf("canonical path conflict: %s and %s", previous, document.Path)
		}
		seen[key] = document.Path
	}
	return Batch{Documents: documents}, nil
}

func readDocument(canonicalPath, localPath string) (api.ImportDocument, error) {
	return readDocumentAfterInspect(canonicalPath, localPath, nil)
}

func readDocumentAfterInspect(canonicalPath, localPath string, beforeOpen func()) (api.ImportDocument, error) {
	root, err := securefile.OpenRoot(filepath.Dir(localPath))
	if err != nil {
		return api.ImportDocument{}, fmt.Errorf("open import root: %w", err)
	}
	defer root.Close()
	return readDocumentFromRoot(canonicalPath, root, filepath.Base(localPath), beforeOpen)
}

func readDocumentFromRoot(canonicalPath string, root *securefile.Root, relativePath string, beforeOpen func()) (api.ImportDocument, error) {
	if err := validatePath(canonicalPath); err != nil {
		return api.ImportDocument{}, fmt.Errorf("invalid Markdown path %q: %w", canonicalPath, err)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	data, err := root.ReadLimit(relativePath, MaxDocumentSize, false)
	if err != nil {
		return api.ImportDocument{}, fmt.Errorf("read Markdown document %s: %w", canonicalPath, err)
	}
	if !utf8.Valid(data) {
		return api.ImportDocument{}, fmt.Errorf("Markdown document is not valid UTF-8: %s", canonicalPath)
	}
	return api.ImportDocument{Path: canonicalPath, Markdown: string(data)}, nil
}

func validatePath(path string) error {
	if path == "" || strings.Contains(path, "\\") || strings.HasPrefix(path, "/") || !xnorm.NFC.IsNormalString(path) {
		return errors.New("path must be a relative NFC slash path")
	}
	if len(path) > MaxPathBytes || utf8.RuneCountInString(path) > MaxPathRunes {
		return errors.New("path exceeds the public server limit")
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("path contains an empty or dot segment")
		}
	}
	for _, value := range path {
		if value == 0 || unicode.IsControl(value) {
			return errors.New("path contains a control character")
		}
	}
	return nil
}
