package workspace

import (
	"context"
	"errors"
	"fmt"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"strings"
)

const (
	CodeMoveCrossDevice = "move_cross_device"
	CodeMoveUnsupported = "move_unsupported"
)

// Copy and move deliberately share the exact strict three-string wire shape,
// not source semantics or publication primitives. Both use stat entry versions.
func decodeMoveArguments(raw string) (copyArguments, error) { return decodeCopyArguments(raw) }
func (w *Workspace) prepareMove(ctx context.Context, raw string) (*PreparedMutation, Result) {
	args, err := decodeMoveArguments(raw)
	if err != nil {
		return nil, mutationFailure(CodeInvalidArguments, "移动需要 source、destination、expected_version 三个字符串，版本必须来自 stat")
	}
	source, err := normalizeModelPath(args.Source, false)
	if err != nil {
		return nil, mutationFailureForError(err, "移动源路径无效")
	}
	destination, err := normalizeModelPath(args.Destination, false)
	if err != nil {
		return nil, mutationFailureForError(err, "移动目标路径无效")
	}
	if !fileeffects.ValidPath(source, false) || !fileeffects.ValidPath(destination, false) {
		return nil, mutationFailure(CodeInvalidPath, "移动路径无法安全完整显示")
	}
	plan, err := w.root.PrepareMove(ctx, source, destination, args.ExpectedVersion)
	if err != nil {
		return nil, moveFailure(ctx, source, destination, err)
	}
	// Even no-op must validate path, entry type and the supplied source version.
	// No candidate means no approval, WAL or fabricated side-effect receipt.
	if plan.Unchanged() {
		return nil, Result{Publication: PublicationUnchanged, Summary: "源与目标相同，未移动：" + source, Value: map[string]any{"operation": ToolMove, "source": source, "path": source, "destination": destination, "entry_type": string(plan.Kind()), "publication_outcome": "unchanged", "complete": true}}
	}
	preview := fmt.Sprintf("移动源：%s\n移动目标：%s\n类型：%s；源入口版本：%s\n入口字节数：%d（不读取正文，无内容大小限制）\n仅同文件系统整体移动，不覆盖、不永久删除、不创建父目录。目录内部链接保留且不遍历；入口版本不是子树快照。结果未知时保留两端事实，不自动重试或恢复重放。", source, destination, plan.Kind(), plan.Version(), plan.Size())
	p := &PreparedMutation{path: source, movePlan: plan, baseVersion: plan.Version(), previewHash: hashProjection(preview), Presentation: MutationPresentation{Tool: ToolMove, Operation: ToolMove, Path: source, DestinationPath: destination, EntryKind: string(plan.Kind()), BaseVersion: plan.Version(), PreviewKind: ToolMove, Preview: preview}}
	if len(preview) > w.limits.MutationPreviewBytes || safeResultJSONSize(moveResult(p, PublicationUnknown).Value) > w.limits.ResultBytes || safeResultJSONSize(map[string]any{"file_effect": p.FileEffect(), "operation": ToolMove, "path": source, "source": source, "destination": destination, "entry_type": string(plan.Kind()), "publication_outcome": "unknown", "error": CodeOutcomeUnknown, "code": CodeOutcomeUnknown}) > 2<<10 {
		return nil, mutationFailure(CodeInvalidPath, "路径过长，无法完整保留移动授权与双端副作用事实")
	}
	return p, Result{}
}
func (w *Workspace) commitMove(ctx context.Context, p *PreparedMutation) Result {
	v := p.Presentation
	if p.movePlan == nil || p.path != p.movePlan.Source() || v.Path != p.path || v.DestinationPath != p.movePlan.Destination() || v.Operation != ToolMove || v.EntryKind != string(p.movePlan.Kind()) || v.BaseVersion != p.movePlan.Version() || v.PreviewKind != ToolMove || v.Truncated || p.previewHash != hashProjection(v.Preview) {
		return mutationFailure(CodeInvalidArguments, "移动候选与冻结授权不一致")
	}
	release, err := w.queues.acquire(ctx, "file:"+p.movePlan.Identity())
	if err != nil {
		return moveFailure(ctx, p.path, p.movePlan.Destination(), err)
	}
	defer release()
	releaseTarget, err := w.queues.acquire(ctx, "create:"+strings.ToLower(p.movePlan.Destination()))
	if err != nil {
		return moveFailure(ctx, p.path, p.movePlan.Destination(), err)
	}
	defer releaseTarget()
	result, err := w.root.Move(ctx, p.movePlan)
	if result.Outcome == securefile.PublishUnknown || errors.Is(err, securefile.ErrOutcomeUnknown) {
		return moveResult(p, PublicationUnknown)
	}
	if err != nil {
		return moveFailure(ctx, p.path, p.movePlan.Destination(), err)
	}
	if result.Outcome != securefile.PublishCompleted {
		return moveFailure(ctx, p.path, p.movePlan.Destination(), securefile.ErrChanged)
	}
	return moveResult(p, PublicationCompleted)
}
func moveResult(p *PreparedMutation, outcome PublicationOutcome) Result {
	effect := p.FileEffect()
	value := map[string]any{"operation": ToolMove, "path": p.path, "source": p.path, "destination": p.movePlan.Destination(), "entry_type": string(p.movePlan.Kind()), "publication_outcome": string(outcome), "complete": outcome == PublicationCompleted, "file_effect": effect, "preview_kind": ToolMove, "preview": p.Presentation.Preview}
	summary := "已移动：" + p.path + " → " + p.movePlan.Destination() + "；未永久删除，新入口版本需 stat 读取"
	if outcome == PublicationUnknown {
		summary = "移动结果未知：" + p.path + " → " + p.movePlan.Destination() + "；请核查两端，不会自动重试、恢复重放或删除回滚"
		value["error"], value["code"], value["message"] = CodeOutcomeUnknown, CodeOutcomeUnknown, summary
	}
	return Result{Value: value, Summary: summary, Publication: outcome, Effect: &effect, Reference: &Reference{Path: effect.ReferencePath(), Kind: effect.ReferenceKind(), InvalidateObserved: true}}
}
func moveFailure(ctx context.Context, source, destination string, err error) Result {
	var result Result
	switch {
	case ctx.Err() != nil:
		result = mutationContextFailure(ctx.Err())
	case errors.Is(err, securefile.ErrMovePath):
		result = mutationFailure(CodeInvalidPath, "拒绝移动到自身后代、路径别名或大小写单独变更")
	case errors.Is(err, securefile.ErrCrossDevice):
		result = mutationFailure(CodeMoveCrossDevice, "跨文件系统移动不支持；不会复制后删除")
	case errors.Is(err, securefile.ErrArchiveUnsupported):
		result = mutationFailure(CodeMoveUnsupported, "平台不支持安全不覆盖移动；不会降级或重试")
	default:
		result = mutationFailureForSecureError(err, "移动未发布；没有永久删除或覆盖")
	}
	value := result.Value.(map[string]any)
	value["operation"], value["source"], value["path"], value["destination"] = ToolMove, source, source, destination
	value["suggestion"] = "用 stat 核查源、目标和父目录；版本变化需重新准备与授权。目标冲突只能选择不存在的新目标，不能覆盖或自动归档冲突项；不支持的移动交由用户处理，禁止复制后删除或自动重试。"
	return result
}
