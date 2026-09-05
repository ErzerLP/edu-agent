package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

type mkdirArguments struct {
	Path    string `json:"path"`
	Parents bool   `json:"parents"`
}

// Mkdir is a strict new contract: duplicate/case-alias keys and null booleans
// cannot silently change the frozen plan.
func decodeMkdirArguments(raw string) (mkdirArguments, error) {
	var args mkdirArguments
	if !utf8.ValidString(raw) {
		return args, argumentError("mkdir input must be UTF-8")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return args, argumentError("mkdir requires an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return args, argumentError("invalid mkdir key")
		}
		key, ok := keyToken.(string)
		if !ok || seen[key] || (key != "path" && key != "parents") {
			return args, argumentError("unknown or duplicate mkdir field")
		}
		seen[key] = true
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return args, argumentError("invalid mkdir value")
		}
		switch key {
		case "path":
			if string(value) == "null" || json.Unmarshal(value, &args.Path) != nil {
				return args, argumentError("mkdir path must be text")
			}
		case "parents":
			if string(value) != "true" && string(value) != "false" {
				return args, argumentError("mkdir parents must be boolean")
			}
			args.Parents = string(value) == "true"
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || !seen["path"] {
		return args, argumentError("mkdir path is required")
	}
	if err = decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return args, argumentError("trailing mkdir input")
	}
	return args, nil
}

// FileEffect is a value copy of the frozen plan; UI callers cannot modify it.
func (p *PreparedMutation) FileEffect() fileeffects.Effect {
	kind := p.Presentation.EntryKind
	if kind == "" {
		kind = "file"
	}
	operation := p.Presentation.Operation
	target := p.path
	if target == "" {
		target = p.Presentation.Path
	} // test/adapter presentations
	source := ""
	if operation == ToolArchive {
		source = target
		target = p.archivePath
		if target == "" {
			target = p.Presentation.ArchivePath
		}
	}
	e := fileeffects.New(operation, source, target, kind)
	if p.movePlan != nil {
		e = fileeffects.New(ToolMove, p.movePlan.Source(), p.movePlan.Destination(), string(p.movePlan.Kind()))
		e.Source.Version = p.movePlan.Version()
	}
	if p.copyPlan != nil {
		e = fileeffects.New(ToolCopy, p.copyPlan.Source(), p.copyPlan.Destination(), "file")
		e.Source.Version = p.copyPlan.Version()
	}
	if p.archiveEntry != nil {
		e.Source.Version = p.archiveEntry.Version
	}
	if p.mkdirPlan != nil {
		e.Directories = fileeffects.DirectoryChain{Anchor: p.mkdirPlan.Anchor(), Count: p.mkdirPlan.Count()}
	}
	return e
}
func (w *Workspace) prepareMkdir(ctx context.Context, raw string) (*PreparedMutation, Result) {
	args, decodeErr := decodeMkdirArguments(raw)
	if decodeErr != nil {
		return nil, mutationFailureForError(decodeErr, "目录创建参数无效")
	}
	path, err := normalizeModelPath(args.Path, false)
	if err != nil {
		return nil, mutationFailureForError(err, "目录创建路径无效")
	}
	plan, err := w.root.PrepareMkdir(ctx, path, args.Parents)
	if err != nil {
		return nil, mkdirFailure(ctx, path, err)
	}
	if plan.Count() == 0 {
		return nil, Result{Publication: PublicationUnchanged, Summary: "目录已存在，未创建：" + path, Value: map[string]any{"operation": ToolMkdir, "path": path, "entry_type": "directory", "publication_outcome": "unchanged", "complete": true, "created_count": 0}}
	}
	preview := fmt.Sprintf("创建目标：%s\n已有父锚点：%s\n创建范围：从锚点之后的第一个缺失目录到目标，按路径顺序共 %d 层。\n仅创建这些目录；中途失败保留已创建项，不删除回滚。", path, plan.Anchor(), plan.Count())
	p := &PreparedMutation{Presentation: MutationPresentation{Tool: ToolMkdir, Operation: ToolMkdir, Path: path, EntryKind: "directory", PreviewKind: ToolMkdir, Preview: preview}, path: path, mkdirPlan: plan, previewHash: hashProjection(preview)}
	// Reserve the worst-case result (unknown), including the compact plan. Never
	// authorize a truncated path and never grow global budgets to fit a chain.
	if len(preview) > w.limits.MutationPreviewBytes || safeResultJSONSize(mkdirResult(p, PublicationUnknown, plan.Count()).Value) > w.limits.ResultBytes || safeResultJSONSize(map[string]any{"file_effect": p.FileEffect(), "operation": ToolMkdir, "path": path, "publication_outcome": "unknown", "error": CodeOutcomeUnknown, "code": CodeOutcomeUnknown}) > 2<<10 {
		return nil, mutationFailure(CodeInvalidPath, "路径过长，无法完整展示冻结创建范围")
	}
	return p, Result{}
}
func (w *Workspace) commitMkdir(ctx context.Context, p *PreparedMutation) Result {
	if p.mkdirPlan == nil || p.path != p.mkdirPlan.Path() || p.Presentation.Path != p.path || p.Presentation.Operation != ToolMkdir || p.Presentation.EntryKind != "directory" || p.Presentation.PreviewKind != ToolMkdir || p.Presentation.Truncated || p.previewHash != hashProjection(p.Presentation.Preview) {
		return mutationFailure(CodeInvalidArguments, "目录候选与冻结授权不一致")
	}
	release, err := w.queues.acquire(ctx, "mkdir:"+strings.ToLower(p.path))
	if err != nil {
		return mutationContextFailure(err)
	}
	defer release()
	result, err := w.root.Mkdir(ctx, p.mkdirPlan)
	if result.Outcome == securefile.PublishUnknown || errors.Is(err, securefile.ErrOutcomeUnknown) {
		return mkdirResult(p, PublicationUnknown, result.Created)
	}
	if err != nil {
		return mkdirFailure(ctx, p.path, err)
	}
	if result.Outcome != securefile.PublishCompleted {
		return mkdirFailure(ctx, p.path, securefile.ErrChanged)
	}
	return mkdirResult(p, PublicationCompleted, result.Created)
}
func mkdirResult(p *PreparedMutation, outcome PublicationOutcome, created int) Result {
	effect := p.FileEffect()
	effect.Directories.Created = created
	value := map[string]any{"path": p.path, "operation": ToolMkdir, "entry_type": "directory", "publication_outcome": string(outcome), "complete": outcome == PublicationCompleted, "file_effect": effect, "created_count": created, "preview_kind": ToolMkdir, "preview": p.Presentation.Preview}
	summary := fmt.Sprintf("已创建 %d 个目录：%s", created, p.path)
	if outcome == PublicationUnknown {
		summary = fmt.Sprintf("目录创建结果未知；已知创建 %d/%d 层，保留实际目录，请核查路径和元数据；不会自动重试或删除回滚", created, p.mkdirPlan.Count())
		value["error"] = CodeOutcomeUnknown
		value["code"] = CodeOutcomeUnknown
		value["message"] = summary
		value["suggestion"] = "按 file_effect 的父锚点、目标和 created 前缀核查实际目录；其余计划路径可能创建，不读取目录 content_hash"
	}
	return Result{Value: value, Summary: summary, Publication: outcome, Effect: &effect, Reference: &Reference{Path: p.path, Kind: "mkdir", InvalidateObserved: true}}
}
func mkdirFailure(ctx context.Context, path string, err error) Result {
	if ctx.Err() != nil {
		return mutationContextFailure(ctx.Err())
	}
	result := mutationFailureForSecureError(err, "目录创建未完成，未确认创建任何目录")
	value := result.Value.(map[string]any)
	value["path"] = path
	value["operation"] = ToolMkdir
	value["suggestion"] = "核查路径、父目录身份和入口元数据；冲突后必须重新准备与授权，不自动接受其他进程创建项"
	return result
}
