package command

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

type goalClient interface {
	CreateGoal(context.Context, api.LearningGoalRequest) (api.GoalOperationResult, error)
	ReviseGoal(context.Context, api.LearningGoalRequest) (api.GoalOperationResult, error)
	Goal(context.Context, string) (api.GoalRevision, error)
	Goals(context.Context, string, string, string, int) (api.GoalPage, error)
	GoalRevisions(context.Context, string, string, int) (api.GoalPage, error)
}

func goalStatus(s string) string {
	if name, ok := map[string]string{"draft": "草稿", "active": "进行中", "paused": "已暂停", "completed": "已完成（手动）", "archived": "已归档"}[s]; ok {
		return name
	}
	return s
}
func goalMultiline(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = safeText(lines[i])
	}
	return strings.Join(lines, "\n  ")
}
func (a *App) goalDetail(g api.GoalRevision) {
	m := g.GoalManagement()
	space := g.SpaceID
	if space == "" {
		space = api.DefaultLearningSpaceID
	}
	_, _ = fmt.Fprintf(a.Out, "%s · %s · 版本 %d\n目标 ID：%s\n修订 ID：%s\n学习区：%s\n学习意图：%s\n", safeText(m.Details.Name), goalStatus(m.Status), g.Revision, g.GoalID, g.GoalRevisionID, space, goalMultiline(g.Text))
	fields := []struct{ k, v string }{{"预期结果", m.Details.ExpectedOutcome}, {"范围", m.Details.Scope}, {"排除项", m.Details.Exclusions}, {"自述基础（非学习证据）", m.Details.SelfAssessment}, {"用途", m.Details.Purpose}, {"完成标准", m.Details.CompletionCriteria}, {"优先级", m.Details.Priority}, {"资料范围版本", m.Details.ScopeSnapshotID}, {"时区", m.Details.Timezone}}
	for _, f := range fields {
		_, _ = fmt.Fprintf(a.Out, "%s：%s\n", f.k, goalMultiline(f.v))
	}
	if m.Details.ScopeSnapshotID == "" {
		_, _ = fmt.Fprintln(a.Out, "资料缺口：尚未选择资料，可继续保存草稿。")
	}
	if m.Details.Deadline == nil {
		_, _ = fmt.Fprintln(a.Out, "截止时间：未设定")
	} else {
		_, _ = fmt.Fprintln(a.Out, "截止时间：", m.Details.Deadline.Format(time.RFC3339))
	}
	if m.Details.WeeklyMinutes == nil {
		_, _ = fmt.Fprintln(a.Out, "每周投入：未设定")
	} else {
		_, _ = fmt.Fprintf(a.Out, "每周投入：%d 分钟\n", *m.Details.WeeklyMinutes)
	}
	_, _ = fmt.Fprintln(a.Out, "学习标准验证：未验证；自述与手动完成不代表掌握度。")
	if m.Completion != nil {
		_, _ = fmt.Fprintf(a.Out, "完成依据：%s\n操作设备：%s，时间：%s\n", safeText(m.Completion.Reason), m.Completion.ActorDeviceID, m.Completion.At.Format(time.RFC3339))
	}
	if len(m.ChangedFields) > 0 {
		_, _ = fmt.Fprintln(a.Out, "本次变化：", strings.Join(m.ChangedFields, ", "))
	}
	if m.RouteAdjustmentNeeded {
		_, _ = fmt.Fprintln(a.Out, "范围或标准已修改，后续路线可能需要调整；旧题目仍引用原目标版本。")
	}
}
func (a *App) goalList(page api.GoalPage) {
	if len(page.Items) == 0 {
		_, _ = fmt.Fprintln(a.Out, "没有符合条件的目标。")
	}
	for i, g := range page.Items {
		m := g.GoalManagement()
		_, _ = fmt.Fprintf(a.Out, "%d. %s · %s · %s · 版本 %d\n", i+1, safeText(m.Details.Name), goalStatus(m.Status), g.CreatedAt.Format("2006-01-02 15:04:05"), g.Revision)
	}
	if page.NextCursor != "" {
		_, _ = fmt.Fprintln(a.Out, "下一页游标：", page.NextCursor)
	}
}

func (a *App) runGoalManagement(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(a.Out,"goal plan --id 目标ID [--plan 草稿ID] [--session 原会话ID] [--json]：可选 AI 完善、出处、手动编辑与确认采用。生成不修改正式目标；模型来自服务端教学配置。")
		_, err := fmt.Fprintln(a.Out, "goal set <text>：只保存一句话目标\ngoal create [--id UUID] [--operation-id UUID] [结构化参数] <学习意图>\ngoal list [--search 文本] [--status draft|active|paused|completed|archived] [--limit 1..100] [--cursor 游标] [--json]\ngoal show|history --id UUID [--json]\ngoal edit --id UUID [--expected-version N] [结构化参数]（无参数进入多行编辑）\ngoal start|pause|resume|complete|archive|restore --id UUID [--expected-version N] [--reason 完成依据]\ngoal browse：交互列表、详情、资料选择与状态操作\n结构化参数：--name、--text、--outcome、--scope、--exclusions、--self-assessment、--purpose、--criteria、--priority low|normal|high、--materials 冻结范围ID、--timezone IANA时区、--deadline RFC3339、--weekly-minutes 分钟。空值清除可选约束。\n所有管理写操作支持 --operation-id；跨进程重试创建须同时复用 --id 和 --operation-id。使用 --space 指定学习区；归属创建后固定。新增、编辑与状态操作均不切换教学会话。")
		return err
	}
	action := args[0]
	set := newFlagSet("goal " + action)
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	id := set.String("id", "", "目标 ID")
	op := set.String("operation-id", "", "重试操作 ID")
	version := set.Int64("expected-version", 0, "预期版本")
	search := set.String("search", "", "名称或意图搜索")
	status := set.String("status", "", "状态筛选")
	cursor := set.String("cursor", "", "分页游标")
	limit := set.Int("limit", 50, "页大小")
	asJSON := set.Bool("json", false, "JSON 输出")
	reason := set.String("reason", "", "手动完成依据")
	values := map[string]*string{}
	for _, key := range []string{"text", "name", "outcome", "scope", "exclusions", "self-assessment", "purpose", "criteria", "priority", "materials", "timezone", "deadline", "weekly-minutes"} {
		values[key] = set.String(key, "", "目标字段 "+key)
	}
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	switch action {
	case "create", "edit", "list", "show", "history", "browse", "start", "pause", "resume", "complete", "archive", "restore":
	default:
		return commandError("usage", "未知目标操作", "使用 goal help", ExitInput)
	}
	if action != "create" && len(set.Args()) > 0 {
		return commandError("usage", "多余的位置参数", "使用 goal help", ExitInput)
	}
	var argumentErr error
	versionSet := false
	set.Visit(func(f *flag.Flag) {
		if _, content := values[f.Name]; content && action != "create" && action != "edit" {
			argumentErr = commandError("usage", "内容修订与状态操作必须分开", "使用 goal edit 修改结构化信息", ExitInput)
		}
		if f.Name == "reason" && action != "complete" {
			argumentErr = commandError("usage", "--reason 仅适用于手动完成", "使用 goal complete --reason 完成依据", ExitInput)
		}
		if f.Name == "expected-version" {
			versionSet = true
		}
	})
	if argumentErr != nil {
		return argumentErr
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	client, ok := online.client.(goalClient)
	if !ok {
		return commandError("goal_management_unsupported", "客户端不支持目标管理", "更新客户端与服务端", ExitUnavailable)
	}
	if capabilities, ok := online.client.(spaceClient); ok {
		caps, err := capabilities.LearningSpacesCapabilities(ctx)
		if err != nil {
			return mapAPIError(err)
		}
		if caps.Modules["learning"] != "goals_v1" {
			return commandError("goal_management_unsupported", "服务端尚未提供独立目标管理", "更新服务端；goal set 仍可保存一句话目标", ExitUnavailable)
		}
	}
	if action == "browse" {
		return a.browseGoals(ctx, client)
	}
	if action == "list" || action == "history" {
		var page api.GoalPage
		if action == "list" {
			page, err = client.Goals(ctx, *search, *status, *cursor, *limit)
		} else {
			page, err = client.GoalRevisions(ctx, *id, *cursor, *limit)
		}
		if err != nil {
			return mapAPIError(err)
		}
		if *asJSON {
			return json.NewEncoder(a.Out).Encode(page)
		}
		a.goalList(page)
		return nil
	}
	var g api.GoalRevision
	if action != "create" {
		g, err = client.Goal(ctx, *id)
		if err != nil {
			return mapAPIError(err)
		}
	}
	if action == "show" {
		if *asJSON {
			return json.NewEncoder(a.Out).Encode(g)
		}
		a.goalDetail(g)
		return nil
	}
	if action == "create" {
		g.Text = strings.TrimSpace(strings.Join(set.Args(), " "))
		g.GoalID = *id
		if g.GoalID == "" {
			g.GoalID, err = a.operationID()
			if err != nil {
				return err
			}
		}
	}
	if versionSet && *version != g.Revision {
		return commandError("version_conflict", "目标已被修订", "读取详情并核对版本", ExitConflict)
	}
	d := g.GoalManagement().Details
	changed := false
	set.Visit(func(f *flag.Flag) {
		if value, ok := values[f.Name]; ok {
			changed = true
			if err == nil {
				err = setGoalField(&g.Text, &d, f.Name, *value)
			}
		}
	})
	if err != nil {
		return commandError("invalid_goal", err.Error(), "使用 goal help", ExitInput)
	}
	if action == "create" && *values["name"] == "" {
		name := []rune(g.Text)
		if len(name) > 120 {
			name = name[:120]
		}
		d.Name = string(name)
	}
	if action == "edit" && !changed {
		return a.editGoal(ctx, client, g)
	}
	request := goalRequest(g, *op)
	if action == "create" || action == "edit" {
		if changed {
			request.Details = &d
		}
	} else {
		request.Action = action
		request.CompletionReason = *reason
	}
	if action == "create" && request.Text == "" {
		request.Text = d.Name
	}
	if request.OperationID == "" {
		request.OperationID, err = a.operationID()
		if err != nil {
			return err
		}
	}
	result, err := a.saveGoal(ctx, client, request)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(a.Out).Encode(result.Result)
	}
	a.goalDetail(result.Result)
	return nil
}

func goalRequest(g api.GoalRevision, operation string) api.LearningGoalRequest {
	return api.LearningGoalRequest{OperationID: operation, PayloadSchemaVersion: 1, AggregateType: "goal", AggregateID: g.GoalID, ExpectedVersion: g.Revision, PreviousRevisionID: g.GoalRevisionID, Text: g.Text, Source: "go-cli-m1"}
}
func (a *App) saveGoal(ctx context.Context, client goalClient, r api.LearningGoalRequest) (api.GoalOperationResult, error) {
	var result api.GoalOperationResult
	var err error
	if r.ExpectedVersion == 0 {
		result, err = client.CreateGoal(ctx, r)
	} else {
		result, err = client.ReviseGoal(ctx, r)
	}
	if err != nil {
		return result, mapAPIError(err)
	}
	return result, nil
}
func setGoalField(text *string, d *api.GoalDetails, key, value string) error {
	switch key {
	case "text":
		*text = value
	case "name":
		d.Name = value
	case "outcome":
		d.ExpectedOutcome = value
	case "scope":
		d.Scope = value
	case "exclusions":
		d.Exclusions = value
	case "self-assessment":
		d.SelfAssessment = value
	case "purpose":
		d.Purpose = value
	case "criteria":
		d.CompletionCriteria = value
	case "priority":
		d.Priority = value
	case "materials":
		d.ScopeSnapshotID = value
	case "timezone":
		d.Timezone = value
	case "deadline":
		if value == "" {
			d.Deadline = nil
			return nil
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return fmt.Errorf("截止时间必须包含日期、时间和明确偏移")
		}
		d.Deadline = &parsed
	case "weekly-minutes":
		if value == "" {
			d.WeeklyMinutes = nil
			return nil
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 10080 {
			return fmt.Errorf("每周投入必须为 1 至 10080 分钟")
		}
		d.WeeklyMinutes = &parsed
	default:
		return fmt.Errorf("未知字段：%s", key)
	}
	return nil
}

func (a *App) browseGoals(ctx context.Context, client goalClient) error {
	if !a.interactiveTerminalAvailable() {
		return commandError("not_a_terminal", "目标浏览需要终端", "使用 goal list 或 show", ExitInput)
	}
	search, status, cursor := "", "", ""
	for {
		page, err := client.Goals(ctx, search, status, cursor, 20)
		if err != nil {
			_, _ = fmt.Fprintln(a.Err, mapAPIError(err))
			retry, e := a.Terminal.ReadLine("读取失败：Enter 重试，q 返回 > ")
			if e != nil {
				return e
			}
			if retry == "q" {
				return nil
			}
			continue
		}
		a.goalList(page)
		input, err := a.Terminal.ReadLine("序号查看；n 新建；s 搜索；f 筛选；p 下一页；b 首页；q 返回 > ")
		if err != nil {
			return err
		}
		switch input {
		case "q":
			return nil
		case "s":
			search, err = a.Terminal.ReadLine("搜索 > ")
			cursor = ""
		case "f":
			status, err = a.Terminal.ReadLine("draft/active/paused/completed/archived，留空全部 > ")
			cursor = ""
		case "p":
			if page.NextCursor != "" {
				cursor = page.NextCursor
			}
		case "b":
			cursor = ""
		case "n":
			text, e := a.Terminal.ReadLine("一句话学习意图 > ")
			if e != nil {
				return e
			}
			id, e := a.operationID()
			if e != nil {
				return e
			}
			g := api.GoalRevision{GoalID: id, Text: text}
			err = a.editGoal(ctx, client, g)
		default:
			i, e := strconv.Atoi(input)
			if e != nil || i < 1 || i > len(page.Items) {
				continue
			}
			err = a.browseGoalDetail(ctx, client, page.Items[i-1].GoalID)
		}
		if err != nil {
			_, _ = fmt.Fprintln(a.Err, err)
		}
	}
}

func (a *App) browseGoalDetail(ctx context.Context, client goalClient, id string) error {
	for {
		g, err := client.Goal(ctx, id)
		if err != nil {
			return mapAPIError(err)
		}
		a.goalDetail(g)
		input, err := a.Terminal.ReadLine("e 编辑/选择资料；p 帮助完善/规划；v 查看冻结资料；h 版本历史；start/pause/resume/complete/archive/restore 状态操作；q 返回 > ")
		if err != nil {
			return err
		}
		if input == "q" {
			return nil
		}
		if input == "e" {
			if err = a.editGoal(ctx, client, g); err != nil {
				return err
			}
			continue
		}
		if input == "p" {
			p, ok := client.(planningClient)
			if !ok {
				return fmt.Errorf("规划接口不可用")
			}
			if err = a.browsePlanning(ctx, p, id, "", ""); err != nil {
				return err
			}
			continue
		}
		if input == "v" {
			if err = a.showGoalMaterials(ctx, client, g.GoalManagement().Details.ScopeSnapshotID); err != nil {
				_, _ = fmt.Fprintln(a.Err, err)
			}
			continue
		}
		if input == "h" {
			cursor := ""
			for {
				page, e := client.GoalRevisions(ctx, id, cursor, 20)
				if e != nil {
					return mapAPIError(e)
				}
				for _, rev := range page.Items {
					a.goalDetail(rev)
				}
				if page.NextCursor == "" {
					break
				}
				next, e := a.Terminal.ReadLine("n 下一页，其他键返回 > ")
				if e != nil {
					return e
				}
				if next != "n" {
					break
				}
				cursor = page.NextCursor
			}
			continue
		}
		switch input {
		case "start", "pause", "resume", "complete", "archive", "restore":
		default:
			continue
		}
		op, err := a.operationID()
		if err != nil {
			return err
		}
		r := goalRequest(g, op)
		r.Action = input
		if input == "complete" {
			r.CompletionReason, err = a.Terminal.ReadLine("手动完成依据（不会产生学习证据） > ")
			if err != nil {
				return err
			}
		}
		for {
			_, err = a.saveGoal(ctx, client, r)
			if err == nil {
				break
			}
			_, _ = fmt.Fprintln(a.Err, err)
			retry, e := a.Terminal.ReadLine("操作失败：r 原请求重试，其他键返回详情 > ")
			if e != nil {
				return e
			}
			if retry != "r" {
				break
			}
		}
	}
}

func (a *App) editGoal(ctx context.Context, client goalClient, g api.GoalRevision) error {
	if !a.interactiveTerminalAvailable() {
		return commandError("not_a_terminal", "多行编辑需要终端", "使用 goal edit 的结构化参数", ExitInput)
	}
	if g.Revision == 0 {
		g.Management = &api.GoalManagement{Details: api.GoalDetails{Name: g.Text, Priority: "normal"}, Status: "draft", CriteriaVerification: "unverified"}
	}
	d := g.GoalManagement().Details
	if len([]rune(d.Name)) > 120 {
		d.Name = string([]rune(d.Name)[:120])
	}
	r := goalRequest(g, "")
	r.Details = &d
	for {
		preview := g
		preview.Text = r.Text
		management := g.GoalManagement()
		management.Details = d
		preview.Management = &management
		a.goalDetail(preview)
		if a.importedScope != "" {
			_, _ = fmt.Fprintln(a.Out, "i 加入本次导入资料（保存前可检查或取消）")
		}
		_, _ = fmt.Fprintln(a.Out, "字段：text/name/outcome/scope/exclusions/self-assessment/purpose/criteria/priority/timezone/deadline/weekly-minutes；m 选择资料；x 清除资料；s 保存；r 读取远端版本；q 取消")
		key, err := a.Terminal.ReadLine("编辑 > ")
		if err != nil {
			return err
		}
		switch key {
		case "i":
			if a.importedScope != "" {
				d.ScopeSnapshotID = a.importedScope
				r.OperationID = ""
			}
		case "q":
			return nil
		case "s":
			if r.OperationID == "" {
				r.OperationID, err = a.operationID()
				if err != nil {
					return err
				}
			}
			result, e := a.saveGoal(ctx, client, r)
			if e == nil {
				a.goalDetail(result.Result)
				return nil
			}
			_, _ = fmt.Fprintln(a.Err, e)
			_, _ = fmt.Fprintln(a.Out, "保存失败，当前输入已保留；s 重试同一请求，r 核对远端版本。")
		case "r":
			if g.Revision == 0 {
				continue
			}
			remote, e := client.Goal(ctx, g.GoalID)
			if e != nil {
				_, _ = fmt.Fprintln(a.Err, mapAPIError(e))
				continue
			}
			a.goalDetail(remote)
			confirmed, e := a.Terminal.Confirm("已展示远端内容。保留当前输入，以此版本作为下一次保存依据？")
			if e != nil {
				return e
			}
			if confirmed {
				g = remote
				r.ExpectedVersion = remote.Revision
				r.PreviousRevisionID = remote.GoalRevisionID
				r.OperationID = ""
			}
		case "m":
			real, ok := client.(*api.Client)
			if !ok {
				_, _ = fmt.Fprintln(a.Err, "当前客户端无法选择资料")
				continue
			}
			id, e := a.selectGoalMaterials(ctx, real)
			if e != nil {
				_, _ = fmt.Fprintln(a.Err, e)
				continue
			}
			if id != "" {
				d.ScopeSnapshotID = id
				r.OperationID = ""
			}
		case "x":
			d.ScopeSnapshotID = ""
			r.OperationID = ""
		default:
			var value string
			if key == "text" || key == "outcome" || key == "scope" || key == "exclusions" || key == "self-assessment" || key == "purpose" || key == "criteria" {
				_, _ = fmt.Fprintln(a.Out, "多行输入，单独一行 . 结束；仅输入 . 清空字段。")
				lines := []string{}
				size := 0
				for {
					line, e := a.Terminal.ReadLine("")
					if e != nil {
						return e
					}
					if line == "." {
						break
					}
					size += len([]rune(line)) + 1
					if size > 4001 {
						err = fmt.Errorf("字段超过 4000 字")
						continue
					}
					lines = append(lines, line)
				}
				value = strings.Join(lines, "\n")
			} else {
				value, err = a.Terminal.ReadLine("新值（留空清除） > ")
			}
			if err != nil {
				_, _ = fmt.Fprintln(a.Err, err)
				continue
			}
			if err = setGoalField(&r.Text, &d, key, value); err != nil {
				_, _ = fmt.Fprintln(a.Err, err)
				continue
			}
			r.OperationID = ""
		}
	}
}

func (a *App) showGoalMaterials(ctx context.Context, client goalClient, id string) error {
	if id == "" {
		_, _ = fmt.Fprintln(a.Out, "资料缺口：尚未选择资料，可继续保存草稿。")
		return nil
	}
	real, ok := client.(*api.Client)
	if !ok {
		return commandError("goal_management_unsupported", "当前客户端无法预览资料", "更新客户端", ExitUnavailable)
	}
	for _, kind := range []string{"", "export"} {
		view, err := real.KnowledgeLibraryView(ctx, id, kind, true)
		if err != nil {
			return mapAPIError(err)
		}
		// 使用 JSON 转义外部正文，避免终端控制字符；冻结版本与更新提示均可查看。
		encoded, err := json.MarshalIndent(view, "", "  ")
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(a.Out, string(encoded))
	}
	_, _ = fmt.Fprintln(a.Out, "资料更新不会自动替换此冻结范围；需要调整时请重新选择资料并保存目标修订。")
	return nil
}

func (a *App) selectGoalMaterials(ctx context.Context, client *api.Client) (string, error) {
	items, err := client.KnowledgeCollections(ctx, false)
	if err != nil {
		return "", mapAPIError(err)
	}
	entries := []api.KnowledgeScopeEntry{}
	for {
		if len(items) == 0 {
			_, _ = fmt.Fprintln(a.Out, "本区没有资料，可先保存空资料草稿。")
			return "", nil
		}
		for i, c := range items {
			_, _ = fmt.Fprintf(a.Out, "%d. %s · %s\n", i+1, safeText(c.Name), safeText(c.Source))
		}
		input, e := a.Terminal.ReadLine("集合序号添加文档/章节；s 冻结已选范围；q 取消 > ")
		if e != nil {
			return "", e
		}
		if input == "q" {
			return "", nil
		}
		if input == "s" {
			if len(entries) == 0 {
				continue
			}
			id, e := a.operationID()
			if e != nil {
				return "", e
			}
			for {
				snapshot, e := client.FreezeKnowledgeScope(ctx, api.KnowledgeScopeSnapshot{ID: id, Entries: entries})
				if e == nil {
					return snapshot.ID, nil
				}
				_, _ = fmt.Fprintln(a.Err, mapAPIError(e))
				retry, e := a.Terminal.ReadLine("冻结失败，选项已保留；r 重试，其他键返回选择 > ")
				if e != nil {
					return "", e
				}
				if retry != "r" {
					break
				}
			}
			continue
		}
		i, e := strconv.Atoi(input)
		if e != nil || i < 1 || i > len(items) {
			continue
		}
		c := items[i-1]
		if c.HeadRevisionID == nil {
			_, _ = fmt.Fprintln(a.Out, "此集合尚无正文。")
			continue
		}
		entry, e := a.selectKnowledgeScope(ctx, client, c)
		if e != nil {
			_, _ = fmt.Fprintln(a.Err, e)
			continue
		}
		duplicate := false
		for _, old := range entries {
			if old == entry {
				duplicate = true
			}
		}
		if !duplicate {
			entries = append(entries, entry)
		}
		_, _ = fmt.Fprintf(a.Out, "已选择 %d 个范围条目\n", len(entries))
	}
}
