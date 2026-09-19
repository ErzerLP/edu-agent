package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

type studyClient interface {
	Study(context.Context, string, api.StudyQuery, json.RawMessage) (api.StudyDocument, error)
}

const studyHelp = `study 使用同一服务端学习事实，默认输出原协议 JSON。
study capabilities
study research|start|mentor --goal UUID --input 请求.json
study current --goal UUID --kind research|start_learning|mentor|content_edit
study runs [--kind research|start_learning|mentor|content_edit] [--cursor ID] [--limit N]
study run --run UUID [--wait]
study sources --run UUID
study source|source-decision --run UUID --source UUID [--input 请求.json]
study run-command --run UUID --input 请求.json
study operation --operation UUID
study session-operation --session UUID --operation UUID
study context --session UUID
study ensure --session UUID --input 请求.json
study library [--goal UUID] [--kind reading|exercise] [--cursor ID] [--limit N]
study content|history --artifact UUID [--version N] [--text]
study citation --artifact UUID --source UUID --version N
study answer --artifact UUID --input 请求.json
study changes|change-context --goal UUID [--session UUID]
study change|change-command --goal UUID --change UUID [--version N] [--input 请求.json]
所有入口支持 --space UUID、--server URL、--timeout DURATION；--input - 从 stdin 读取。
写入 JSON 与 OpenAPI 请求一致，必须自带稳定 operation_id（ensure 是幂等适配）。
research/start 使用明确公开主题、外发同意、预算及独立运行 session_id；start 还需自动采纳与模型同意。
同设备同目标同类型再次发起时，从 current 读取并复用 run.session_id；无历史时才创建运行会话 UUID。
mentor 必须指定 teaching_session_id；服务端负责提出变更，CLI 本地 Agent 不执行领域写入。
change-command 按具体 expected_revision/hash/interaction_id 审阅，不自动批准更新后的候选。
--wait 仅轮询指定 run，遇到待输入/审批/预算暂停即返回；写操作不会自动重放。
响应未知时查询原 operation；答案用 session-operation，变更用 change 核对。
运行属于原设备；其他设备读取共享正式课堂、正文、变更和进度，不迁移本地聊天。
未提交草稿仅属于原进程/标签页，不自动跨端同步。工作台目标页提供名称可读入口。`

func (a *App) runStudy(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, err := fmt.Fprintln(a.Out, studyHelp)
		return err
	}
	action := args[0]
	set := newFlagSet("study " + action)
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	var q api.StudyQuery
	set.StringVar(&q.Goal, "goal", "", "目标 ID")
	set.StringVar(&q.Session, "session", "", "教学会话 ID")
	set.StringVar(&q.Run, "run", "", "运行 ID")
	set.StringVar(&q.Artifact, "artifact", "", "正文 ID")
	set.StringVar(&q.Change, "change", "", "变更 ID")
	set.StringVar(&q.Source, "source", "", "来源或引用 ID")
	set.StringVar(&q.Operation, "operation", "", "原操作 ID")
	set.StringVar(&q.Kind, "kind", "", "类型")
	set.StringVar(&q.Status, "status", "", "状态")
	set.StringVar(&q.Cursor, "cursor", "", "分页游标")
	set.IntVar(&q.Limit, "limit", 20, "每页数量")
	set.Int64Var(&q.Version, "version", 0, "具体内容版本或变更修订")
	input := set.String("input", "", "JSON 请求文件或 -")
	asText := set.Bool("text", false, "安全文本投影")
	wait := set.Bool("wait", false, "只读轮询指定运行")
	if err := set.Parse(args[1:]); errors.Is(err, flag.ErrHelp) {
		_, e := fmt.Fprintln(a.Out, studyHelp)
		return e
	} else if err != nil || len(set.Args()) != 0 {
		return commandError("usage", "学习服务参数无效", "study help", ExitInput)
	}
	if *wait && action != "run" {
		return commandError("usage", "--wait 仅支持 run", "study help", ExitInput)
	}
	var raw json.RawMessage
	if *input != "" {
		var reader io.Reader = os.Stdin
		if *input != "-" {
			file, err := os.Open(*input)
			if err != nil {
				return commandError("invalid_study_request", "无法读取请求文件", "检查 --input 路径", ExitInput)
			}
			defer file.Close()
			reader = file
		}
		var err error
		raw, err = io.ReadAll(io.LimitReader(reader, (256<<10)+1))
		if err != nil || len(raw) > 256<<10 || !json.Valid(raw) {
			return commandError("invalid_study_request", "请求须为不超过 256 KiB 的 JSON", "study help", ExitInput)
		}
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	c, ok := online.client.(studyClient)
	if !ok {
		return commandError("learning_services_upgrade_required", "客户端未接入新学习协议", "升级客户端", ExitUnavailable)
	}
	for {
		result, err := c.Study(ctx, action, q, raw)
		if err != nil {
			if len(raw) > 0 {
				fmt.Fprintln(a.Err, "操作未确认；保留原请求和 operation_id，先查询原对象/操作，不自动重放。")
			}
			return mapAPIError(err)
		}
		if !*wait || !studyRunWaiting(result.String("status")) {
			if *asText {
				_, err = fmt.Fprintln(a.Out, studyText(action, result))
				return err
			}
			return json.NewEncoder(a.Out).Encode(result)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func studyRunWaiting(status string) bool {
	return status == "queued" || status == "running" || status == "cancelling"
}

func studyText(action string, d api.StudyDocument) string {
	if action == "content" || action == "ensure" {
		return studySafeLines(api.ContentText(d))
	}
	if action == "run" {
		return studySafeLines(fmt.Sprintf("%s · %s · %s\n%s\n%s\n", d.String("kind"), d.String("status"), d.String("stage"), d.String("reason"), d.String("output")))
	}
	raw, _ := json.MarshalIndent(d, "", "  ")
	return studySafeLines(string(raw))
}

func studySafeLines(text string) string {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = safeText(lines[i])
	}
	return strings.Join(lines, "\n")
}
