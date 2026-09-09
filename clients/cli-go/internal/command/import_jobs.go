package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/importer"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func jobManifest(documents []api.ImportDocument) []api.ImportJobItem {
	items := make([]api.ImportJobItem, len(documents))
	for i, d := range documents {
		sum := sha256.Sum256([]byte(d.Markdown))
		items[i] = api.ImportJobItem{Path: d.Path, Bytes: len(d.Markdown), Digest: hex.EncodeToString(sum[:])}
	}
	return items
}
func uploadImportJob(ctx context.Context, client *api.Client, j api.ImportJob, docs []api.ImportDocument) (api.ImportJob, error) {
	manifest := jobManifest(docs)
	byPath := map[string]int{}
	for i, item := range manifest {
		byPath[item.Path] = i
	}
	// 先核对全部原清单，再传输；磁盘变化不能被当作原确认内容。
	for _, b := range j.Batches {
		index, ok := byPath[b.Item.Path]
		if !ok || manifest[index] != b.Item {
			return j, commandError("import_job_source_changed", "本地来源缺失或摘要变化："+b.Item.Path, "恢复原文件，或为变更文件创建新任务并重新预览", ExitConflict)
		}
	}
	for i, b := range j.Batches {
		if b.Status != "pending" && b.Status != "missing" {
			continue
		}
		d := docs[byPath[b.Item.Path]]
		var err error
		j, err = client.RunImportJob(ctx, api.ImportJobCommand{ID: j.ID, Action: "upload", Batch: i, Document: &d})
		if err != nil {
			return j, err
		}
	}
	return j, nil
}
func (a *App) runImportJobs(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" {
		_, err := fmt.Fprintln(a.Out, "knowledge import jobs new --collection UUID [--id UUID] [--include 规则] [--exclude 规则] 路径\nknowledge import jobs list|browse --collection UUID\nknowledge import jobs show|preview|continue|cancel|clean --collection UUID --id UUID\nknowledge import jobs resume --collection UUID --id UUID 路径\nknowledge import jobs confirm --collection UUID --id UUID --plan-version 版本\nknowledge import jobs batch --collection UUID --id UUID --batch 序号\nknowledge import jobs resolve --collection UUID --id UUID --request 决定.json\n支持 --space。每批一个文件；confirm只授权，continue推进一个批次并先核对原操作。new输出任务ID后自动分段上传；响应未知可用同一ID重试。resume校验原清单所有文件，不保存客户端正文。序号从0开始。")
		return err
	}
	set := newFlagSet("knowledge import jobs")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	collection := set.String("collection", "", "资料集合")
	id := set.String("id", "", "原任务ID")
	plan := set.Int64("plan-version", 0, "完整预览版本")
	batch := set.Int("batch", 0, "批次序号")
	cursor := set.String("cursor", "", "列表游标")
	all := set.Bool("all", false, "按已确认计划逐批继续，未知结果立即停止")
	requestFile := set.String("request", "", "身份决定JSON")
	include := set.String("include", "", "包含规则")
	exclude := set.String("exclude", "", "排除规则")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	if !spaceIDPattern.MatchString(*collection) {
		return commandError("usage", "请指定资料集合", "knowledge library list", ExitInput)
	}
	bound, timeout, err := a.loadBinding(flags.overrides())
	if err != nil {
		return err
	}
	client, ok := a.scopedClient(bound.Config.ServerURL, bound.Token, timeout).(*api.Client)
	if !ok {
		return commandError("unsupported", "客户端不支持导入任务", "更新客户端", ExitUnavailable)
	}
	client = client.WithCollection(*collection)
	action := args[0]
	var result any
	if action == "browse" {
		return a.runImportJobsUI(ctx, client, *collection)
	}
	if action == "list" {
		result, err = client.ImportJobs(ctx, *cursor)
	} else if action == "show" {
		result, err = client.ImportJob(ctx, *id)
	} else if action == "batch" {
		result, err = client.ImportJobBatch(ctx, *id, *batch)
	} else if action == "new" || action == "resume" {
		if len(set.Args()) != 1 {
			return commandError("usage", "需要一个明确的来源路径", "knowledge import jobs help", ExitInput)
		}
		report := importer.Scan(ctx, importer.ScanOptions{Path: set.Args()[0], Include: importPatterns(*include), Exclude: importPatterns(*exclude), Job: true})
		if report.Stopped != "" {
			return commandError("invalid_input", report.Stopped, "修正来源后重试", ExitInput)
		}
		for _, item := range report.Items {
			if item.Status == "error" {
				return commandError("invalid_input", item.Path+": "+item.Reason, "修正或明确排除该文件", ExitInput)
			}
		}
		docs := report.Documents()
		var j api.ImportJob
		if action == "new" {
			if *id == "" {
				*id, err = a.NewUUID()
				if err != nil {
					return err
				}
			}
			_, _ = fmt.Fprintf(a.Err, "导入任务ID：%s（请保留以核对或恢复）\n", *id)
			j, err = client.RunImportJob(ctx, api.ImportJobCommand{ID: *id, Action: "create", Items: jobManifest(docs)})
		} else {
			j, err = client.ImportJob(ctx, *id)
		}
		if err == nil {
			j, err = uploadImportJob(ctx, client, j, docs)
		}
		result = j
	} else {
		c := api.ImportJobCommand{ID: *id, Action: action, PlanVersion: *plan}
		if action == "resolve" {
			root, e := securefile.OpenRoot(filepath.Dir(*requestFile))
			if e != nil {
				return e
			}
			defer root.Close()
			raw, e := root.ReadLimit(filepath.Base(*requestFile), 1<<20, false)
			if e != nil {
				return e
			}
			if e = json.Unmarshal(raw, &c); e != nil {
				return e
			}
			c.ID = *id
			c.Action = "resolve"
		}
		result, err = client.RunImportJob(ctx, c)
	}
	if err != nil {
		return mapAPIError(err)
	}
	if action == "continue" && *all {
		for j := result.(api.ImportJob); j.Approved && j.Status == "partial"; {
			j, err = client.RunImportJob(ctx, api.ImportJobCommand{ID: j.ID, Action: "continue"})
			if err != nil {
				return mapAPIError(err)
			}
			result = j
		}
	}
	return json.NewEncoder(a.Out).Encode(result)
}
