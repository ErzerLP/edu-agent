package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"io"
	"strings"
	"unicode/utf8"
)

type copyArguments struct {
	Source          string `json:"source"`
	Destination     string `json:"destination"`
	ExpectedVersion string `json:"expected_version"`
}

func decodeCopyArguments(raw string) (copyArguments, error) {
	var args copyArguments
	if !utf8.ValidString(raw) {
		return args, argumentError("copy input must be UTF-8")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return args, argumentError("copy requires an object")
	}
	fields := map[string]*string{"source": &args.Source, "destination": &args.Destination, "expected_version": &args.ExpectedVersion}
	seen := map[string]bool{}
	for d.More() {
		keyToken, err := d.Token()
		if err != nil {
			return args, argumentError("invalid copy key")
		}
		key, ok := keyToken.(string)
		if !ok || fields[key] == nil || seen[key] {
			return args, argumentError("unknown or duplicate copy field")
		}
		seen[key] = true
		var value json.RawMessage
		if d.Decode(&value) != nil || string(value) == "null" || json.Unmarshal(value, fields[key]) != nil || *fields[key] == "" {
			return args, argumentError("copy fields must be nonempty text")
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') || len(seen) != 3 {
		return args, argumentError("source, destination and expected_version are required")
	}
	if err = d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return args, argumentError("trailing copy input")
	}
	if !strings.HasPrefix(args.ExpectedVersion, "entry-v1:") || !fileeffects.ValidVersion(args.ExpectedVersion) {
		return args, argumentError("copy requires a stat entry version")
	}
	return args, nil
}
func (w *Workspace) prepareCopy(ctx context.Context, raw string) (*PreparedMutation, Result) {
	args, err := decodeCopyArguments(raw)
	if err != nil {
		return nil, mutationFailureForError(err, "复制参数无效")
	}
	source, err := normalizeModelPath(args.Source, false)
	if err != nil {
		return nil, mutationFailureForError(err, "复制源路径无效")
	}
	destination, err := normalizeModelPath(args.Destination, false)
	if err != nil {
		return nil, mutationFailureForError(err, "复制目标路径无效")
	}
	if !fileeffects.ValidPath(source, false) || !fileeffects.ValidPath(destination, false) {
		return nil, mutationFailure(CodeInvalidPath, "复制路径无法安全完整显示")
	}
	plan, err := w.root.PrepareCopy(ctx, source, destination, args.ExpectedVersion)
	if err != nil {
		return nil, copyFailure(ctx, source, destination, err)
	}
	preview := fmt.Sprintf("复制源：%s\n复制目标：%s\n源入口版本：%s\n字节数：%d（上限 32MiB）；普通权限：%04o\n流式复制普通文件（含二进制），源保持不变；目标必须不存在，父目录必须已存在。仅保留普通 rwx 权限，不复制特殊权限、ACL 或扩展属性。", source, destination, plan.Version(), plan.Size(), plan.Permission())
	p := &PreparedMutation{path: source, copyPlan: plan, baseVersion: plan.Version(), previewHash: hashProjection(preview), Presentation: MutationPresentation{Tool: ToolCopy, Operation: ToolCopy, Path: source, DestinationPath: destination, EntryKind: "file", BaseVersion: plan.Version(), PreviewKind: ToolCopy, Preview: preview}}
	// Both paths and metadata version must fit full history and confirmation.
	if len(preview) > w.limits.MutationPreviewBytes || safeResultJSONSize(copyResult(p, PublicationUnknown, "").Value) > w.limits.ResultBytes || safeResultJSONSize(map[string]any{"file_effect": p.FileEffect(), "operation": ToolCopy, "path": source, "destination": destination, "publication_outcome": "unknown", "error": CodeOutcomeUnknown, "code": CodeOutcomeUnknown}) > 2<<10 {
		return nil, mutationFailure(CodeInvalidPath, "路径过长，无法完整保留复制授权与副作用事实")
	}
	return p, Result{}
}
func (w *Workspace) commitCopy(ctx context.Context, p *PreparedMutation) Result {
	v := p.Presentation
	if p.copyPlan == nil || p.path != p.copyPlan.Source() || v.Path != p.path || v.DestinationPath != p.copyPlan.Destination() || v.Operation != ToolCopy || v.EntryKind != "file" || v.BaseVersion != p.copyPlan.Version() || v.PreviewKind != ToolCopy || v.Truncated || p.previewHash != hashProjection(v.Preview) {
		return mutationFailure(CodeInvalidArguments, "复制候选与冻结授权不一致")
	}
	release, err := w.queues.acquire(ctx, "create:"+strings.ToLower(p.copyPlan.Destination()))
	if err != nil {
		return mutationContextFailure(err)
	}
	defer release()
	result, err := w.root.Copy(ctx, p.copyPlan)
	if result.Outcome == securefile.PublishUnknown || errors.Is(err, securefile.ErrOutcomeUnknown) {
		return copyResult(p, PublicationUnknown, "")
	}
	if err != nil {
		return copyFailure(ctx, p.path, p.copyPlan.Destination(), err)
	}
	if result.Outcome != securefile.PublishCompleted {
		return copyFailure(ctx, p.path, p.copyPlan.Destination(), securefile.ErrChanged)
	}
	return copyResult(p, PublicationCompleted, result.ContentHash)
}
func copyResult(p *PreparedMutation, outcome PublicationOutcome, hash string) Result {
	effect := p.FileEffect()
	if outcome == PublicationCompleted {
		effect.Target.Version = hash
	} else {
		hash = ""
	}
	value := map[string]any{"operation": ToolCopy, "path": p.path, "source": p.path, "destination": p.copyPlan.Destination(), "entry_type": "file", "publication_outcome": string(outcome), "complete": outcome == PublicationCompleted, "file_effect": effect, "source_unchanged": true, "bytes": p.copyPlan.Size(), "preview_kind": ToolCopy, "preview": p.Presentation.Preview}
	summary := "已复制：" + p.path + " → " + p.copyPlan.Destination() + "；源未修改"
	if hash != "" {
		value["content_hash"] = hash
	}
	if outcome == PublicationUnknown {
		summary = "复制结果未知：" + p.path + " → " + p.copyPlan.Destination() + "；本操作未修改源；请核查目标及临时项，不会自动重试或恢复重放"
		value["error"], value["code"], value["message"] = CodeOutcomeUnknown, CodeOutcomeUnknown, summary
	}
	return Result{Value: value, Summary: summary, Publication: outcome, Effect: &effect, Reference: &Reference{Path: effect.Target.Path, Kind: "copy", ContentHash: hash, InvalidateObserved: outcome == PublicationUnknown}}
}
func copyFailure(ctx context.Context, source, destination string, err error) Result {
	var result Result
	if ctx.Err() != nil {
		result = mutationContextFailure(ctx.Err())
	} else {
		result = mutationFailureForSecureError(err, "复制未发布，源未修改")
	}
	value := result.Value.(map[string]any)
	value["operation"] = ToolCopy
	value["source"] = source
	value["path"] = source
	value["destination"] = destination
	value["source_unchanged"] = true
	return result
}
