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

type purgeArguments struct {
	Path            string `json:"path"`
	ExpectedVersion string `json:"expected_version"`
}

func decodePurgeArguments(raw string) (purgeArguments, error) {
	var args purgeArguments
	if !utf8.ValidString(raw) {
		return args, argumentError("purge input must be UTF-8")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return args, argumentError("purge requires an object")
	}
	fields := map[string]*string{"path": &args.Path, "expected_version": &args.ExpectedVersion}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || fields[key] == nil || seen[key] {
			return args, argumentError("unknown or duplicate purge field")
		}
		seen[key] = true
		var value json.RawMessage
		if d.Decode(&value) != nil || string(value) == "null" || json.Unmarshal(value, fields[key]) != nil || *fields[key] == "" {
			return args, argumentError("purge fields require nonempty text")
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') || len(seen) != 2 {
		return args, argumentError("path and expected_version required")
	}
	if err = d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return args, argumentError("trailing purge input")
	}
	if !strings.HasPrefix(args.ExpectedVersion, "entry-v1:") || !fileeffects.ValidVersion(args.ExpectedVersion) {
		return args, argumentError("purge requires a stat entry version")
	}
	return args, nil
}

func (p *PreparedMutation) IsPurgeArchive() bool { return p != nil && p.purgePlan != nil }
func (p *PreparedMutation) PurgeItems() []securefile.PurgeItem {
	if !p.IsPurgeArchive() {
		return nil
	}
	return p.purgePlan.Items()
}
func (p *PreparedMutation) PurgeManifest() string {
	if p == nil {
		return ""
	}
	return p.purgeManifest
}

type purgeManifestItem struct {
	Path            string `json:"path"`
	Kind            string `json:"kind"`
	ExpectedVersion string `json:"expected_version"`
	Bytes           int64  `json:"logical_file_bytes"`
}

func (w *Workspace) preparePurge(ctx context.Context, raw string) (*PreparedMutation, Result) {
	args, err := decodePurgeArguments(raw)
	if err != nil {
		return nil, mutationFailure(CodeInvalidArguments, "清理需要确切path及当前stat的expected_version")
	}
	path, err := normalizeModelPath(args.Path, false)
	if err != nil {
		return nil, mutationFailureForError(err, "归档清理路径无效")
	}
	if !fileeffects.ValidPath(path, false) {
		return nil, mutationFailure(CodeInvalidPath, "清理路径无法安全完整显示")
	}
	plan, err := w.root.PreparePurge(ctx, path, args.ExpectedVersion, securefile.PurgeLimits{Entries: w.limits.CopyEntries, PlanBytes: w.limits.CopyPlanBytes})
	if err != nil {
		return nil, purgeFailure(ctx, path, err)
	}
	items := plan.Items()
	manifest := make([]purgeManifestItem, len(items))
	files, dirs, links := 0, 0, 0
	for i, item := range items {
		if !fileeffects.ValidPath(item.Path, false) {
			return nil, mutationFailure(CodeInvalidPath, "清理计划含无法安全完整显示的入口")
		}
		entry := purgeManifestItem{Path: item.Path, Kind: string(item.Kind), ExpectedVersion: item.Version}
		switch item.Kind {
		case securefile.EntryFile:
			files++
			entry.Bytes = item.Size
		case securefile.EntryDirectory:
			dirs++
		case securefile.EntryLink:
			links++
		}
		manifest[i] = entry
	}
	body, err := json.Marshal(struct {
		Status    string              `json:"status"`
		Operation string              `json:"operation"`
		Items     []purgeManifestItem `json:"items"`
	}{Status: "plan_only_not_executed", Operation: ToolPurgeArchive, Items: manifest})
	if err != nil || int64(len(body)) > w.limits.CopyPlanBytes {
		return nil, mutationFailure(CodeFileTooLarge, "完整清理计划超过共享--file-copy-plan-limit；未删除入口")
	}
	preview := fmt.Sprintf("永久清理范围：%s\n当前入口版本：%s\n冻结后序计划：%d个文件、%d个目录、%d个链接；逻辑文件字节%d（不是实际释放空间）。\n批准后永久删除，不可撤销；YOLO不代替本次明确确认。完整计划可F6查看，无末页审批门槛。链接只删除自身，不访问目标；不扩展到新增内容，不清理未选父容器。失败/取消停止余项，已删除项不回滚；物理释放量未知。", path, plan.Version(), files, dirs, links, plan.Bytes())
	view := MutationPresentation{Tool: ToolPurgeArchive, Operation: ToolPurgeArchive, Path: path, EntryKind: string(plan.Kind()), BaseVersion: plan.Version(), PreviewKind: ToolPurgeArchive, Preview: preview}
	p := &PreparedMutation{path: path, purgePlan: plan, purgePresentation: view, purgeManifest: string(body), baseVersion: plan.Version(), previewHash: hashProjection(preview), Presentation: view}
	fact := map[string]any{"file_effect": p.FileEffect(), "operation": ToolPurgeArchive, "path": path, "publication_outcome": "unknown", "code": CodeOutcomeUnknown, "error": CodeOutcomeUnknown}
	if len(preview) > w.limits.MutationPreviewBytes || safeResultJSONSize(fact)+1024 > 2<<10 {
		return nil, mutationFailure(CodeInvalidPath, "路径过长，无法完整保存清理授权与实际结果")
	}
	return p, Result{}
}

// CommitPurge is the observer-required path; ordinary CommitMutation refuses it.
func (w *Workspace) CommitPurge(ctx context.Context, p *PreparedMutation, observer securefile.PurgeObserver) (Result, securefile.PurgeResult) {
	empty := securefile.PurgeResult{Outcome: securefile.PublishUnchanged}
	if w == nil || w.root == nil {
		return mutationFailure(CodeWorkspaceUnavailable, "工作区不可用"), empty
	}
	if !p.IsPurgeArchive() {
		return mutationFailure(CodeInvalidArguments, "候选不是归档清理计划"), empty
	}
	p.commitMu.Lock()
	used := p.committed
	p.committed = true
	p.commitMu.Unlock()
	if used || p.Presentation != p.purgePresentation || p.path != p.purgePlan.Path() || p.previewHash != hashProjection(p.Presentation.Preview) {
		return mutationFailure(CodeInvalidArguments, "清理候选已处理或与冻结授权不一致"), empty
	}
	release, err := w.queues.acquire(ctx, "file:"+p.purgePlan.Identity())
	if err != nil {
		return purgeFailure(ctx, p.path, err), empty
	}
	defer release()
	actual, err := w.root.Purge(ctx, p.purgePlan, observer)
	result := purgeResult(p, actual)
	if err != nil {
		failure := purgeFailure(ctx, p.path, err)
		fv := failure.Value.(map[string]any)
		value := result.Value.(map[string]any)
		value["execution_error"] = fv["code"]
		if actual.Outcome == securefile.PublishUnchanged {
			value["code"], value["error"], value["message"] = fv["code"], fv["error"], failure.Summary
			result.Summary = failure.Summary
		}
	}
	return result, actual
}

func purgeResult(p *PreparedMutation, actual securefile.PurgeResult) Result {
	outcome := PublicationOutcome(actual.Outcome)
	state := string(outcome)
	attempted, unchanged := 0, 0
	var logical int64
	for _, item := range actual.Items {
		if !item.Attempted {
			continue
		}
		attempted++
		if item.Outcome == securefile.PublishUnchanged {
			unchanged++
		}
		logical += item.Bytes
	}
	if outcome == PublicationUnknown && actual.Unknown == 0 && !actual.CleanupIncomplete && actual.Completed < len(actual.Items) {
		state = "partial"
	}
	effect := p.FileEffect()
	summary := fmt.Sprintf("永久归档清理 %s：%s；完成%d/%d项，未处理%d项；删除逻辑文件字节%d，实际释放空间未知；不可撤销，不自动重放", state, p.path, actual.Completed, len(actual.Items), len(actual.Items)-attempted, logical)
	value := map[string]any{"operation": ToolPurgeArchive, "path": p.path, "entry_type": string(p.purgePlan.Kind()), "publication_outcome": string(outcome), "purge_state": state, "complete": outcome == PublicationCompleted, "file_effect": effect, "item_count": len(actual.Items), "attempted": attempted, "completed": actual.Completed, "unchanged": unchanged, "unknown": actual.Unknown, "not_started": len(actual.Items) - attempted, "logical_bytes_removed": logical, "physical_bytes_reclaimed": "unknown", "cleanup_incomplete": actual.CleanupIncomplete, "replay": false}
	if outcome == PublicationUnknown {
		value["error"], value["code"], value["message"] = CodeOutcomeUnknown, CodeOutcomeUnknown, summary
	}
	result := Result{Value: value, Summary: summary, Publication: outcome}
	if outcome == PublicationCompleted || outcome == PublicationUnknown {
		result.Effect = &effect
		result.Reference = &Reference{Path: effect.ReferencePath(), Kind: effect.ReferenceKind(), InvalidateObserved: true}
	}
	return result
}

func purgeFailure(ctx context.Context, path string, err error) Result {
	var result Result
	switch {
	case ctx.Err() != nil:
		result = mutationContextFailure(ctx.Err())
	case errors.Is(err, securefile.ErrCrossDevice):
		result = mutationFailure("purge_cross_mount", "拒绝跨挂载点清理归档")
	case errors.Is(err, securefile.ErrArchiveUnsupported):
		result = mutationFailure("purge_unsupported", "平台无法证明安全清理边界；不降级Shell或宽松删除")
	default:
		result = mutationFailureForSecureError(err, "归档清理未开始或停止；请核对逐项实际结果")
	}
	value := result.Value.(map[string]any)
	value["operation"], value["path"], value["physical_bytes_reclaimed"], value["replay"] = ToolPurgeArchive, path, "unknown", false
	return result
}
