package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

const (
	CodeRestoreCrossDevice = "restore_cross_device"
	CodeRestoreUnsupported = "restore_unsupported"
)

func (w *Workspace) prepareRestore(ctx context.Context, raw string) (*PreparedMutation, Result) {
	args, err := decodeCopyArguments(raw)
	if err != nil {
		return nil, mutationFailure(CodeInvalidArguments, "恢复需要明确的 source、destination 和来自当前 stat 的 expected_version")
	}
	source, err := normalizeModelPath(args.Source, false)
	if err != nil {
		return nil, mutationFailureForError(err, "归档恢复源路径无效")
	}
	destination, err := normalizeModelPath(args.Destination, false)
	if err != nil {
		return nil, mutationFailureForError(err, "恢复目标路径无效")
	}
	if !fileeffects.ValidPath(source, false) || !fileeffects.ValidPath(destination, false) {
		return nil, mutationFailure(CodeInvalidPath, "恢复路径无法安全完整显示")
	}
	plan, err := w.root.PrepareRestore(ctx, source, destination, args.ExpectedVersion)
	if err != nil {
		return nil, restoreFailure(ctx, source, destination, err)
	}
	preview := fmt.Sprintf("归档恢复源：%s\n恢复目标：%s\n类型：%s；当前源入口版本：%s\n入口字节数：%d（不读取正文，无内容大小限制）\n仅同文件系统不覆盖移动，不合并、不创建父目录、不永久删除或清理空容器。目标显式指定，不根据旧归档布局猜测原位置。目录内部链接保留且不遍历；入口版本不是内容或子树快照。结果未知时检查两端，不自动重试或恢复重放。", source, destination, plan.Kind(), plan.Version(), plan.Size())
	p := &PreparedMutation{path: source, restorePlan: plan, baseVersion: plan.Version(), previewHash: hashProjection(preview), Presentation: MutationPresentation{Tool: ToolRestoreArchive, Operation: ToolRestoreArchive, Path: source, DestinationPath: destination, EntryKind: string(plan.Kind()), BaseVersion: plan.Version(), PreviewKind: ToolRestoreArchive, Preview: preview}}
	history := map[string]any{"file_effect": p.FileEffect(), "operation": ToolRestoreArchive, "path": source, "source": source, "destination": destination, "entry_type": string(plan.Kind()), "publication_outcome": "unknown", "error": CodeOutcomeUnknown, "code": CodeOutcomeUnknown}
	if len(preview) > w.limits.MutationPreviewBytes || safeResultJSONSize(restoreResult(p, PublicationUnknown).Value) > w.limits.ResultBytes || safeResultJSONSize(history) > 2<<10 {
		return nil, mutationFailure(CodeInvalidPath, "路径过长，无法完整保留恢复授权与双端副作用事实")
	}
	return p, Result{}
}

func (w *Workspace) commitRestore(ctx context.Context, p *PreparedMutation) Result {
	v := p.Presentation
	if p.restorePlan == nil || p.path != p.restorePlan.Source() || v.Tool != ToolRestoreArchive || v.Path != p.path || v.DestinationPath != p.restorePlan.Destination() || v.Operation != ToolRestoreArchive || v.EntryKind != string(p.restorePlan.Kind()) || v.BaseVersion != p.restorePlan.Version() || v.PreviewKind != ToolRestoreArchive || v.Truncated || p.previewHash != hashProjection(v.Preview) {
		return mutationFailure(CodeInvalidArguments, "恢复候选与冻结授权不一致")
	}
	release, err := w.queues.acquire(ctx, "file:"+p.restorePlan.Identity())
	if err != nil {
		return restoreFailure(ctx, p.path, p.restorePlan.Destination(), err)
	}
	defer release()
	releaseTarget, err := w.queues.acquire(ctx, "create:"+strings.ToLower(p.restorePlan.Destination()))
	if err != nil {
		return restoreFailure(ctx, p.path, p.restorePlan.Destination(), err)
	}
	defer releaseTarget()
	result, err := w.root.Restore(ctx, p.restorePlan)
	if result.Outcome == securefile.PublishUnknown || errors.Is(err, securefile.ErrOutcomeUnknown) {
		return restoreResult(p, PublicationUnknown)
	}
	if err != nil {
		return restoreFailure(ctx, p.path, p.restorePlan.Destination(), err)
	}
	if result.Outcome != securefile.PublishCompleted {
		return restoreFailure(ctx, p.path, p.restorePlan.Destination(), securefile.ErrChanged)
	}
	return restoreResult(p, PublicationCompleted)
}

func restoreResult(p *PreparedMutation, outcome PublicationOutcome) Result {
	effect := p.FileEffect()
	value := map[string]any{"operation": ToolRestoreArchive, "path": p.path, "source": p.path, "destination": p.restorePlan.Destination(), "entry_type": string(p.restorePlan.Kind()), "publication_outcome": string(outcome), "complete": outcome == PublicationCompleted, "file_effect": effect, "preview_kind": ToolRestoreArchive, "preview": p.Presentation.Preview}
	summary := "已从归档恢复：" + p.path + " → " + p.restorePlan.Destination() + "；请用 stat 读取目标的新入口版本，空归档容器未清理"
	if outcome == PublicationUnknown {
		summary = "归档恢复结果未知：" + p.path + " → " + p.restorePlan.Destination() + "；请检查两端，不会自动重试、回滚或清理"
		value["error"], value["code"], value["message"] = CodeOutcomeUnknown, CodeOutcomeUnknown, summary
	}
	return Result{Value: value, Summary: summary, Publication: outcome, Effect: &effect, Reference: &Reference{Path: effect.ReferencePath(), Kind: effect.ReferenceKind(), InvalidateObserved: true}}
}

func restoreFailure(ctx context.Context, source, destination string, err error) Result {
	var result Result
	switch {
	case ctx.Err() != nil:
		result = mutationContextFailure(ctx.Err())
	case errors.Is(err, securefile.ErrCrossDevice):
		result = mutationFailure(CodeRestoreCrossDevice, "不支持跨文件系统恢复；不会复制后删除归档")
	case errors.Is(err, securefile.ErrArchiveUnsupported):
		result = mutationFailure(CodeRestoreUnsupported, "平台不支持安全不覆盖恢复；不会降级或重试")
	case errors.Is(err, securefile.ErrMovePath):
		result = mutationFailure(CodeInvalidPath, "拒绝归档恢复路径别名或源目录后代目标")
	default:
		result = mutationFailureForSecureError(err, "归档恢复未发布；没有覆盖、删除或清理两端")
	}
	value := result.Value.(map[string]any)
	value["operation"], value["source"], value["path"], value["destination"] = ToolRestoreArchive, source, source, destination
	value["suggestion"] = "用 list/find/read/stat 定位确切归档入口并核对当前版本；必须显式选择不存在的非归档目标，父目录须已存在。冲突或版本变化需重新准备和授权，不从旧布局猜测原位置，不自动覆盖、复制删除、重试或清理。"
	return result
}
