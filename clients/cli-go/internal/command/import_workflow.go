package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/importer"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func (a *App) runImportWorkflow(ctx context.Context, args []string) error {
	if args[0] == "help" {
		_, err := fmt.Fprintln(a.Out, "knowledge import wizard [--collection ID] [路径]\nknowledge import scan [--include '*.md,*.txt'] [--exclude 'private/**'] 路径\nknowledge import preview --collection ID --request 请求.json\nknowledge import confirm --collection ID --request 确认.json\nknowledge import operation --collection ID --id 操作ID\npreview 请求沿用 ImportRequest，必须提供 operation_id、expected_parent_revision_id（空集合为 null）、source、documents。输出 ready 时返回 receipt；review 时按候选填写身份决定并以新 operation 重新预览。confirm 请求为 {request: 原预览请求, receipt: 回执}，有效期15分钟。确认未知时先查询原 operation；未查到不证明未提交，只能重试同一确认请求。所有命令支持 --space。非 TTY 不启动向导。")
		return err
	}
	set := newFlagSet("knowledge import " + args[0])
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	collection := set.String("collection", "", "资料集合")
	requestFile := set.String("request", "", "UTF-8 JSON 请求文件")
	id := set.String("id", "", "原 operation ID")
	include := set.String("include", "", "相对路径规则，逗号分隔")
	exclude := set.String("exclude", "", "排除规则，逗号分隔")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	if args[0] == "scan" {
		if len(set.Args()) != 1 {
			return commandError("usage", "扫描需要一个明确路径", "knowledge import help", ExitInput)
		}
		report := importer.Scan(ctx, importer.ScanOptions{Path: set.Args()[0], Include: importPatterns(*include), Exclude: importPatterns(*exclude)})
		return json.NewEncoder(a.Out).Encode(report)
	}
	if args[0] == "wizard" && !a.interactiveTerminalAvailable() {
		return commandError("not_a_terminal", "导入向导需要交互终端", "使用 knowledge import scan/preview/confirm", ExitInput)
	}
	bound, timeout, err := a.loadBinding(flags.overrides())
	if err != nil {
		return err
	}
	client, ok := a.scopedClient(bound.Config.ServerURL, bound.Token, timeout).(*api.Client)
	if !ok {
		return commandError("unsupported", "客户端不支持导入预览", "更新客户端", ExitUnavailable)
	}
	if args[0] == "wizard" {
		initial := ""
		if len(set.Args()) > 0 {
			initial = set.Args()[0]
		}
		return a.runImportWizard(ctx, client, *collection, initial)
	}
	if *collection == "" || !spaceIDPattern.MatchString(*collection) {
		return commandError("usage", "请明确选择资料集合", "knowledge library list", ExitInput)
	}
	client = client.WithCollection(*collection)
	var result any
	if args[0] == "operation" {
		result, err = client.ImportOperation(ctx, *id)
	} else {
		root, openErr := securefile.OpenRoot(filepath.Dir(*requestFile))
		if openErr != nil {
			return openErr
		}
		defer root.Close()
		raw, readErr := root.ReadLimit(filepath.Base(*requestFile), importer.MaxRequestSize, false)
		if readErr != nil {
			return readErr
		}
		if args[0] == "preview" {
			var request api.ImportRequest
			if err := decodeImportInput(raw, &request, false); err != nil {
				return err
			}
			result, err = client.PreviewImport(ctx, request)
		} else {
			var request api.ConfirmImportRequest
			if err := decodeImportInput(raw, &request, true); err != nil {
				return err
			}
			result, err = client.ConfirmImport(ctx, request)
		}
	}
	if err != nil {
		var apiErr *api.APIError
		if args[0] == "operation" && errors.As(err, &apiErr) && apiErr.Code == "not_found" {
			return commandError("import_operation_unconfirmed", "尚未取得原操作的成功回执", "未查到不证明提交失败；稍后核对或重试原确认请求", ExitConflict)
		}
		return mapAPIError(err)
	}
	return json.NewEncoder(a.Out).Encode(result)
}

func decodeImportInput(raw []byte, target any, confirm bool) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return commandError("usage", "请求只能包含一个 JSON 对象", "knowledge import help", ExitInput)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if confirm {
		if err := json.Unmarshal(fields["request"], &fields); err != nil {
			return err
		}
	}
	for _, key := range []string{"operation_id", "expected_parent_revision_id", "source", "documents"} {
		if _, ok := fields[key]; !ok {
			return commandError("usage", "请求缺少 "+key, "空集合的父版本必须显式为 null", ExitInput)
		}
	}
	return nil
}

func importPatterns(value string) []string {
	var result []string
	for _, p := range strings.Split(value, ",") {
		if p = strings.TrimSpace(p); p != "" {
			result = append(result, p)
		}
	}
	return result
}
