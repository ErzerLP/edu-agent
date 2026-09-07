package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func (p *PreparedMutation) IsCopyTree() bool { return p != nil && p.copyTreePlan != nil }
func (p *PreparedMutation) CopyTreeItems() []securefile.CopyTreeItem {
	if !p.IsCopyTree() {
		return nil
	}
	return p.copyTreePlan.Items()
}
func (p *PreparedMutation) CopyManifest() string {
	if p == nil {
		return ""
	}
	return p.copyManifest
}

type copyManifestItem struct {
	Source          string `json:"source"`
	Destination     string `json:"destination"`
	Kind            string `json:"kind"`
	ExpectedVersion string `json:"expected_version"`
	Bytes           int64  `json:"bytes"`
}

func (w *Workspace) prepareCopyTree(ctx context.Context, source, destination, version string) (*PreparedMutation, Result) {
	plan, err := w.root.PrepareCopyTree(ctx, source, destination, version, securefile.CopyTreeLimits{Bytes: w.limits.CopyBytes, Entries: w.limits.CopyEntries, PlanBytes: w.limits.CopyPlanBytes})
	if err != nil {
		return nil, copyFailure(ctx, source, destination, err)
	}
	items := plan.Items()
	manifest := make([]copyManifestItem, len(items))
	files, directories := 0, 0
	for i, item := range items {
		if !fileeffects.ValidPath(item.Source, false) || !fileeffects.ValidPath(item.Destination, false) {
			return nil, mutationFailure(CodeInvalidPath, "复制清单含无法安全完整展示的路径")
		}
		entry := copyManifestItem{Source: item.Source, Destination: item.Destination, Kind: string(item.Kind), ExpectedVersion: item.Version}
		if item.Kind == securefile.EntryFile {
			files++
			entry.Bytes = item.Size
		} else {
			directories++
		}
		manifest[i] = entry
	}
	body, err := json.Marshal(struct {
		Status string             `json:"status"`
		Items  []copyManifestItem `json:"items"`
	}{Status: "plan_only_not_executed", Items: manifest})
	if err != nil || int64(len(body)) > w.limits.CopyPlanBytes {
		return nil, mutationFailure(CodeFileTooLarge, "完整复制清单超过 --file-copy-plan-limit；未创建目标")
	}
	preview := fmt.Sprintf("递归复制源：%s\n复制目标：%s\n源入口版本：%s\n冻结范围：%d 个普通文件，%d 个目录（含根）；%d 字节（本次预算 %d 字节）。\n目标根必须不存在，不覆盖、不合并；新目录0700，文件保留普通rwx。完整清单另行保留，可用F6查看。一次批准整个计划；失败/取消立即停止余项，已完成项保留，不是跨文件事务。", source, destination, plan.Version(), files, directories, plan.Bytes(), w.limits.CopyBytes)
	presentation := MutationPresentation{Tool: ToolCopy, Operation: ToolCopy, Path: source, DestinationPath: destination, EntryKind: "directory", BaseVersion: plan.Version(), PreviewKind: ToolCopy, Preview: preview}
	p := &PreparedMutation{path: source, copyTreePlan: plan, copyTreePresentation: presentation, copyManifest: string(body), baseVersion: plan.Version(), previewHash: hashProjection(preview), Presentation: presentation}
	// The complete manifest lives separately; the single root fact must still
	// fit the existing history/dirty safety contract without truncating paths.
	rootFact := map[string]any{"file_effect": p.FileEffect(), "operation": ToolCopy, "path": source, "destination": destination, "publication_outcome": "unknown", "error": CodeOutcomeUnknown, "code": CodeOutcomeUnknown}
	// Leave room for immutable-plan and batch IDs, retention watermarks and
	// per-item counts. These metadata must not evict the complete root fact.
	const receiptMetadataReserve = 1024
	if len(preview) > w.limits.MutationPreviewBytes || safeResultJSONSize(rootFact)+receiptMetadataReserve > 2<<10 {
		return nil, mutationFailure(CodeInvalidPath, "根路径过长，无法完整保留复制授权与副作用事实")
	}
	return p, Result{}
}

// CommitCopyTree is the journal-observed path. Ordinary CommitMutation cannot
// publish this token. Root is single-use as well, and no callback can change
// the frozen manifest or destination identities.
func (w *Workspace) CommitCopyTree(ctx context.Context, p *PreparedMutation, observer securefile.CopyTreeObserver) (Result, securefile.CopyTreeResult) {
	empty := securefile.CopyTreeResult{Outcome: securefile.PublishUnchanged}
	if w == nil || w.root == nil {
		return mutationFailure(CodeWorkspaceUnavailable, "工作区不可用"), empty
	}
	if !p.IsCopyTree() {
		return mutationFailure(CodeInvalidArguments, "候选不是目录复制计划"), empty
	}
	p.commitMu.Lock()
	used := p.committed
	p.committed = true
	p.commitMu.Unlock()
	if used || p.Presentation != p.copyTreePresentation || p.path != p.copyTreePlan.Source() || p.previewHash != hashProjection(p.Presentation.Preview) {
		return mutationFailure(CodeInvalidArguments, "目录复制候选已经处理或与冻结授权不一致"), empty
	}
	release, err := w.queues.acquire(ctx, "create:"+strings.ToLower(p.copyTreePlan.Destination()))
	if err != nil {
		return mutationContextFailure(err), empty
	}
	defer release()
	actual, err := w.root.CopyTree(ctx, p.copyTreePlan, observer)
	if actual.Outcome == securefile.PublishUnchanged {
		if err == nil {
			err = securefile.ErrChanged
		}
		return copyFailure(ctx, p.path, p.copyTreePlan.Destination(), err), actual
	}
	return copyTreeResult(p, actual), actual
}

func copyTreeResult(p *PreparedMutation, actual securefile.CopyTreeResult) Result {
	outcome := PublicationUnknown
	state := "partial"
	if actual.Outcome == securefile.PublishCompleted {
		outcome, state = PublicationCompleted, "completed"
	} else if actual.Unknown != 0 || actual.CleanupIncomplete || actual.Completed == len(actual.Items) {
		state = "unknown"
	}
	attempted, unchanged := 0, 0
	var copied int64
	for _, item := range actual.Items {
		if !item.Attempted {
			continue
		}
		attempted++
		if item.Outcome == securefile.PublishUnchanged {
			unchanged++
		}
		if item.Outcome == securefile.PublishCompleted {
			copied += item.Bytes
		}
	}
	effect := p.FileEffect()
	summary := fmt.Sprintf("目录复制 %s：%s → %s；已完成%d/%d项，未尝试%d项；本操作未修改源", state, p.path, effect.Target.Path, actual.Completed, len(actual.Items), len(actual.Items)-attempted)
	value := map[string]any{"operation": ToolCopy, "path": p.path, "source": p.path, "destination": effect.Target.Path, "entry_type": "directory", "publication_outcome": string(outcome), "copy_state": state, "complete": outcome == PublicationCompleted, "source_unchanged": true, "file_effect": effect, "item_count": len(actual.Items), "attempted": attempted, "completed": actual.Completed, "unchanged": unchanged, "unknown": actual.Unknown, "not_started": len(actual.Items) - attempted, "bytes_copied": copied, "cleanup_incomplete": actual.CleanupIncomplete}
	if outcome == PublicationUnknown {
		value["error"], value["code"], value["message"] = CodeOutcomeUnknown, CodeOutcomeUnknown, summary+"；已完成项不回滚，后续不会自动重放，请查看逐项日志"
	}
	return Result{Value: value, Summary: summary, Publication: outcome, Effect: &effect, Reference: &Reference{Path: effect.Target.Path, Kind: "copy", InvalidateObserved: true}}
}
