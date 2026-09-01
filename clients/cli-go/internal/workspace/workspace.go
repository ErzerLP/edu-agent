package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func Open(path string) (*Workspace, error) {
	return OpenWithLimits(path, DefaultLimits())
}

func OpenWithLimits(path string, limits Limits) (*Workspace, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" || strings.IndexByte(path, 0) >= 0 {
		return nil, operationFailure(CodeWorkspaceUnavailable, "workspace root is unavailable")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, operationFailure(CodeWorkspaceUnavailable, "workspace root is unavailable")
	}
	absolute = filepath.Clean(absolute)
	root, err := securefile.OpenRoot(absolute)
	if err != nil {
		return nil, operationFailure(CodeWorkspaceUnavailable, "workspace root is unavailable")
	}
	return &Workspace{
		root: root, limits: limits,
		status: Status{Available: true, Label: safeLabel(absolute)},
	}, nil
}

func validateLimits(limits Limits) error {
	if limits.ListEntries < 1 || limits.DirectoryScanEntries < limits.ListEntries || limits.ResultBytes < 1024 ||
		limits.ReadLines < 1 || limits.FileBytes < 1 || limits.SearchMatches < 1 || limits.SearchFiles < 1 ||
		limits.SearchBytes < limits.FileBytes || limits.SearchDepth < 1 || limits.SearchPreviewBytes < 32 || limits.SearchEntries < limits.SearchFiles ||
		limits.MutationPreviewBytes < 1024 || limits.EditReplacements < 1 || limits.EditReplacements > 32 {
		return errors.New("workspace limits are invalid")
	}
	return nil
}

func (w *Workspace) Status() Status {
	if w == nil || w.root == nil {
		return Status{Code: CodeWorkspaceUnavailable}
	}
	return w.status
}

func (w *Workspace) Close() error {
	if w == nil || w.root == nil {
		return nil
	}
	err := w.root.Close()
	w.root = nil
	return err
}

func (w *Workspace) Execute(ctx context.Context, toolName, rawArguments string) Result {
	if w == nil || w.root == nil {
		return failureResult(CodeWorkspaceUnavailable, "工作区不可用")
	}
	if err := ctx.Err(); err != nil {
		return contextFailure(err)
	}
	switch toolName {
	case ToolList:
		return w.executeList(ctx, rawArguments)
	case ToolRead:
		return w.executeRead(ctx, rawArguments)
	case ToolSearch:
		return w.executeSearch(ctx, rawArguments)
	default:
		return failureResult(CodeInvalidArguments, "未知工作区工具")
	}
}

func decodeArguments(raw string, target any) error {
	if !utf8.ValidString(raw) {
		return argumentError("arguments must be valid UTF-8")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return argumentError("arguments must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return argumentError("arguments do not match the tool contract")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return argumentError("arguments contain trailing JSON")
	}
	return nil
}

func contextFailure(err error) Result {
	if errors.Is(err, context.DeadlineExceeded) {
		return failureResult(CodeTimeout, "工作区工具已超时")
	}
	return failureResult(CodeCancelled, "工作区工具已取消")
}

func safeResultJSONSize(value any) int {
	data, err := json.Marshal(value)
	if err != nil {
		return 1 << 30
	}
	return len(data)
}

func hashProjection(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func isDirectoryError(err error) bool {
	return errors.Is(err, securefile.ErrNotDirectory)
}

func inspectNonDirectory(root *securefile.Root, relative string, limit int64) error {
	_, err := root.ReadSnapshot(relative, limit, false)
	return err
}

func isPermissionError(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, securefile.ErrPermission)
}
