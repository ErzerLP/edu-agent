package workspace

import (
	"errors"
	"fmt"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

const (
	CodeInvalidArguments     = "invalid_arguments"
	CodeInvalidPath          = "invalid_path"
	CodePathOutsideWorkspace = "path_outside_workspace"
	CodeNotFound             = "not_found"
	CodeNotDirectory         = "not_directory"
	CodeUnsupportedType      = "unsupported_type"
	CodeLinkNotAllowed       = "link_not_allowed"
	CodePermissionDenied     = "permission_denied"
	CodeInvalidUTF8          = "invalid_utf8"
	CodeBinaryFile           = "binary_file"
	CodeFileTooLarge         = "file_too_large"
	CodeRegexInvalid         = "regex_invalid"
	CodeContentChanged       = "content_changed"
	CodeAlreadyExists        = "already_exists"
	CodeReplacementMissing   = "replacement_missing"
	CodeReplacementNotUnique = "replacement_not_unique"
	CodeReplacementOverlap   = "replacement_overlap"
	CodeNoChanges            = "no_changes"
	CodeAuthorizationDenied  = "authorization_denied"
	CodeOutcomeUnknown       = "outcome_unknown"
	CodeCancelled            = "cancelled"
	CodeTimeout              = "timeout"
	CodeWorkspaceUnavailable = "workspace_unavailable"
	CodeInternalError        = "internal_error"
)

type operationError struct {
	code string
	err  error
}

func (e *operationError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *operationError) Unwrap() error { return e.err }

func operationFailure(code, message string) error {
	return &operationError{code: code, err: errors.New(message)}
}

func failureResult(code, summary string) Result {
	if code == "" {
		code = CodeInternalError
	}
	message := truncateUTF8Bytes(summary, 240)
	if message == "" {
		message = "工作区操作未完成"
	}
	return Result{
		Value: map[string]any{
			"error": code, "code": code, "complete": false,
			"message": message, "suggestion": failureSuggestion(code),
		},
		Summary: summary,
	}
}

func failureSuggestion(code string) string {
	switch code {
	case CodeReplacementMissing:
		return "重新读取文件，并使用当前内容中存在的精确 old_text 重试"
	case CodeReplacementNotUnique:
		return "扩大 old_text 上下文使其只匹配一次，然后携带最新 expected_hash 重试"
	case CodeReplacementOverlap:
		return "合并或拆分替换，确保同一原始快照中的编辑区域互不重叠"
	case CodeContentChanged:
		return "重新读取目标以取得最新 content_hash，再基于新内容准备操作"
	case CodeAuthorizationDenied:
		return "保留原文件；如仍需修改，请重新发起并等待用户明确授权"
	case CodeCancelled:
		return "操作已停止；确认仍有需要后再重新发起"
	case CodeTimeout:
		return "缩小 path、读取范围或搜索范围后重试"
	case CodeInternalError:
		return "不要猜测文件状态；重新读取目标，若仍失败请向用户报告"
	case CodeInvalidArguments, CodeInvalidPath, CodePathOutsideWorkspace:
		return "修正参数并使用工作区内的相对斜杠路径后重试"
	case CodeNotFound:
		return "检查相对路径，或先用 list 查找目标"
	case CodeAlreadyExists:
		return "先读取现有文件；需要替换时携带最新 expected_hash 使用 replace"
	case CodeWorkspaceUnavailable:
		return "继续普通对话，或由用户用有效的 --workspace 重新启动 Agent"
	case CodeRegexInvalid:
		return "简化正则表达式，或改用 literal 搜索"
	case CodePermissionDenied, CodeLinkNotAllowed, CodeUnsupportedType, CodeNotDirectory:
		return "选择工作区内可直接访问的普通文件或目录，不要跟随链接"
	case CodeInvalidUTF8, CodeBinaryFile, CodeFileTooLarge:
		return "选择安全上限内的 UTF-8 文本文件"
	case CodeOutcomeUnknown:
		return "文件状态不确定；必须重新读取目标后再决定下一步"
	case CodeNoChanges:
		return "无需写入；如预期有变化，请重新读取并检查候选内容"
	default:
		return "检查稳定错误码和参数后再决定是否重试"
	}
}

func resultForError(err error, fallbackSummary string) Result {
	var operation *operationError
	if errors.As(err, &operation) {
		return failureResult(operation.code, fallbackSummary)
	}
	return failureResult(codeForSecureError(err), fallbackSummary)
}

func codeForSecureError(err error) string {
	switch {
	case errors.Is(err, securefile.ErrNotFound):
		return CodeNotFound
	case errors.Is(err, securefile.ErrNotDirectory):
		return CodeNotDirectory
	case errors.Is(err, securefile.ErrNotRegular):
		return CodeUnsupportedType
	case errors.Is(err, securefile.ErrLink):
		return CodeLinkNotAllowed
	case errors.Is(err, securefile.ErrPermission):
		return CodePermissionDenied
	case errors.Is(err, securefile.ErrTooLarge):
		return CodeFileTooLarge
	case errors.Is(err, securefile.ErrChanged):
		return CodeContentChanged
	case errors.Is(err, securefile.ErrAlreadyExists):
		return CodeAlreadyExists
	case errors.Is(err, securefile.ErrOutcomeUnknown):
		return CodeOutcomeUnknown
	case errors.Is(err, securefile.ErrOutsideRoot):
		return CodePathOutsideWorkspace
	default:
		return CodeInternalError
	}
}

func argumentError(message string) error {
	return operationFailure(CodeInvalidArguments, fmt.Sprintf("invalid workspace tool arguments: %s", message))
}
