package command

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

type planningClient interface {
	PlanningList(context.Context, string) ([]api.PlanningDraft, error)
	Planning(context.Context, string, string) (api.PlanningDraft, error)
	ChangePlanning(context.Context, string, string, api.PlanningCommand) (api.PlanningDraft, error)
}

func (a *App) runPlanning(ctx context.Context, args []string) error {
	set := newFlagSet("goal plan")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	goal := set.String("id", "", "目标 ID")
	plan := set.String("plan", "", "已有草稿 ID")
	session := set.String("session", "", "适用会话 ID")
	asJSON := set.Bool("json", false, "读取草稿 JSON")
	if err := set.Parse(args); err != nil || *goal == "" || len(set.Args()) != 0 {
		return commandError("usage", "使用 goal plan --id 目标ID [--plan 草稿ID] [--session 会话ID] [--json]", "goal help", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	client, ok := online.client.(planningClient)
	if !ok {
		return commandError("unsupported", "客户端不支持规划", "更新客户端", ExitInput)
	}
	if *asJSON {
		if *plan == "" {
			items, e := client.PlanningList(ctx, *goal)
			if e != nil {
				return mapAPIError(e)
			}
			return json.NewEncoder(a.Out).Encode(items)
		}
		d, e := client.Planning(ctx, *goal, *plan)
		if e != nil {
			return mapAPIError(e)
		}
		return json.NewEncoder(a.Out).Encode(d)
	}
	return a.browsePlanning(ctx, client, *goal, *plan, *session)
}
func (a *App) savePlanning(ctx context.Context, client planningClient, goal, id string, c api.PlanningCommand) (api.PlanningDraft, error) {
	var err error
	if c.OperationID == "" {
		c.OperationID, err = a.operationID()
		if err != nil {
			return api.PlanningDraft{}, err
		}
	}
	for {
		d, e := client.ChangePlanning(ctx, goal, id, c)
		if e == nil {
			return d, nil
		}
		fmt.Fprintln(a.Err, mapAPIError(e))
		fmt.Fprintln(a.Err, "草稿与操作身份：", id, c.OperationID)
		answer, readErr := a.Terminal.ReadLine("r 使用同一请求查询/重试真实结果；其他键返回（不代表已回滚） > ")
		if readErr != nil {
			return d, readErr
		}
		if answer != "r" {
			return d, mapAPIError(e)
		}
	}
}
func (a *App) browsePlanning(ctx context.Context, client planningClient, goal, id, session string) error {
	if id == "" {
		items, err := client.PlanningList(ctx, goal)
		if err != nil {
			return mapAPIError(err)
		}
		for i, d := range items {
			fmt.Fprintf(a.Out, "%d. %s · 草稿版本 %d · %s\n", i+1, safeText(d.Goal.GoalManagement().Details.Name), d.Version, safeText(d.State))
		}
		choice, err := a.Terminal.ReadLine("序号恢复；n 创建独立规划草稿；q 返回 > ")
		if err != nil {
			return err
		}
		if choice == "q" {
			return nil
		}
		if choice == "n" {
			if session == "" {
				if list, ok := client.(teachingSessionClient); ok {
					cursor := ""
					for {
						page, e := list.Sessions(ctx, goal, "resumable", cursor, 20)
						if e != nil {
							return mapAPIError(e)
						}
						for i, item := range page.Items {
							fmt.Fprintf(a.Out, "%d. %s · %s · %s\n", i+1, safeText(item.Name), safeText(item.State), safeText(item.Position))
						}
						choice, e := a.Terminal.ReadLine("可选绑定已有教学：序号；p 下一页；Enter 不绑定（仍可采用到新会话） > ")
						if e != nil {
							return e
						}
						if choice == "p" && page.NextCursor != "" {
							cursor = page.NextCursor
							continue
						}
						if choice != "" {
							i, e := strconv.Atoi(choice)
							if e != nil || i < 1 || i > len(page.Items) {
								return fmt.Errorf("教学序号无效")
							}
							session = page.Items[i-1].SessionID
						}
						break
					}
				}
			}
			id, err = a.operationID()
			if err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "草稿 ID：", id)
			if _, err = a.savePlanning(ctx, client, goal, id, api.PlanningCommand{Action: "create", SessionID: session}); err != nil {
				return err
			}
		} else {
			i, e := strconv.Atoi(choice)
			if e != nil || i < 1 || i > len(items) {
				return fmt.Errorf("无效序号")
			}
			id = items[i-1].ID
		}
	}
	for {
		d, err := client.Planning(ctx, goal, id)
		if err != nil {
			return mapAPIError(err)
		}
		a.showPlanning(d)
		if d.State == "applied" {
			if d.AppliedSessionID != "" {
				a.selectTeachingSession(d.AppliedSessionID)
				fmt.Fprintln(a.Out, "已采用。继续学习：edu-agent learn --session", d.AppliedSessionID)
			} else {
				fmt.Fprintln(a.Out, "目标建议已确认保存；未创建或修改教学会话。")
			}
			return nil
		}
		action, err := a.Terminal.ReadLine("g 可选 AI；u 将建议复制到草稿；e 编辑目标建议；a 添加步骤；t 编辑步骤；d 删除；m 重排；v 出处；f 资料范围；q 保存并退出；c 确认 > ")
		if err != nil {
			return err
		}
		if action == "q" {
			return nil
		}
		content := d.Content
		c := api.PlanningCommand{ExpectedVersion: d.Version, Action: "edit"}
		switch action {
		case "g":
			c.Action = "generate"
		case "u":
			if d.Suggestion == nil {
				continue
			}
			content = *d.Suggestion
		case "e":
			key, e := a.Terminal.ReadLine("字段 name/outcome/self-assessment/purpose/scope/exclusions/criteria/timezone/weekly-minutes；留空跳过澄清 > ")
			if e != nil {
				return e
			}
			if key == "" {
				continue
			}
			value, e := a.readPlanningText("输入内容，单独一行 . 结束；留空可跳过")
			if e != nil {
				return e
			}
			text := d.Goal.Text
			if e = setGoalField(&text, &content.Details, key, value); e != nil {
				fmt.Fprintln(a.Err, e)
				continue
			}
		case "a", "t":
			index := len(content.Steps)
			if action == "t" {
				index, err = a.planningIndex(len(content.Steps), "要编辑的步骤序号 > ")
				if err != nil {
					fmt.Fprintln(a.Err, err)
					continue
				}
			}
			st, e := a.editPlanningStep(d)
			if e != nil {
				fmt.Fprintln(a.Err, e)
				continue
			}
			if action == "a" {
				content.Steps = append(content.Steps, st)
			} else {
				content.Steps[index] = st
			}
		case "d", "m":
			from, e := a.planningIndex(len(content.Steps), "步骤序号 > ")
			if e != nil {
				fmt.Fprintln(a.Err, e)
				continue
			}
			to := -1
			if action == "m" {
				to, e = a.planningIndex(len(content.Steps), "移至序号 > ")
				if e != nil {
					fmt.Fprintln(a.Err, e)
					continue
				}
			}
			content.Steps = reorderPlanningSteps(content.Steps, from, to)
		case "f":
			concrete, ok := client.(*api.Client)
			if !ok {
				return fmt.Errorf("资料选择不可用")
			}
			scope, e := a.selectGoalMaterials(ctx, concrete)
			if e != nil {
				fmt.Fprintln(a.Err, e)
				continue
			}
			content.Details.ScopeSnapshotID = scope
		case "v":
			for i, source := range d.Sources {
				fmt.Fprintf(a.Out, "%d. %s\n", i+1, safeText(source.Name))
			}
			i, e := a.planningIndex(len(d.Sources), "资料序号 > ")
			if e != nil {
				continue
			}
			source := d.Sources[i]
			fmt.Fprintf(a.Out, "%s\n文档版本：%s · 字节范围 %d..%d · SHA256 %s\n%s\n", safeText(source.Name), source.Reference.DocumentRevisionID, source.Reference.Range.Start, source.Reference.Range.End, source.Reference.SliceSHA256, goalMultiline(source.Reference.Slice))
			continue
		case "c":
			c.Action = "confirm"
			choice, e := a.Terminal.ReadLine("采用到：1 仅修订目标；2 新会话；3 原会话后续步骤（不修改旧题） > ")
			if e != nil {
				return e
			}
			c.Target = map[string]string{"1": "goal", "2": "new_session", "3": "current_session"}[choice]
			if c.Target == "" {
				continue
			}
			c.UpdateGoal, e = a.Terminal.Confirm("同时将当前草稿中的目标建议写入正式目标？")
			if e != nil {
				return e
			}
			confirmed, e := a.Terminal.Confirm("已查看目标原文、步骤和资料，确认采用以上选择？")
			if e != nil {
				return e
			}
			if !confirmed {
				continue
			}
		default:
			continue
		}
		if c.Action == "edit" {
			c.Content = &content
		}
		if _, err = a.savePlanning(ctx, client, goal, id, c); err != nil {
			fmt.Fprintln(a.Err, err)
		}
	}
}
func (a *App) readPlanningText(prompt string) (string, error) {
	fmt.Fprintln(a.Out, prompt)
	lines := []string{}
	for {
		v, e := a.Terminal.ReadLine(" > ")
		if e != nil {
			return "", e
		}
		if v == "." {
			return strings.Join(lines, "\n"), nil
		}
		lines = append(lines, v)
	}
}
func (a *App) planningIndex(n int, prompt string) (int, error) {
	v, e := a.Terminal.ReadLine(prompt)
	if e != nil {
		return 0, e
	}
	i, e := strconv.Atoi(v)
	if e != nil || i < 1 || i > n {
		return 0, fmt.Errorf("序号超出范围")
	}
	return i - 1, nil
}
func (a *App) editPlanningStep(d api.PlanningDraft) (api.PlanningStep, error) {
	var st api.PlanningStep
	for _, f := range []struct {
		name  string
		value *string
	}{{"阶段/步骤名称", &st.Name}, {"学习内容", &st.Content}, {"安排理由", &st.Reason}, {"练习方向", &st.Exercise}, {"完成依据", &st.Completion}} {
		v, e := a.Terminal.ReadLine(f.name + " > ")
		if e != nil {
			return st, e
		}
		*f.value = v
	}
	v, e := a.Terminal.ReadLine("预计分钟 > ")
	if e != nil {
		return st, e
	}
	st.Minutes, e = strconv.Atoi(v)
	if e != nil {
		return st, e
	}
	v, e = a.Terminal.ReadLine("前置步骤序号，逗号分隔；留空无前置 > ")
	if e != nil {
		return st, e
	}
	st.Prerequisites = []int{}
	if strings.TrimSpace(v) != "" {
		for _, part := range strings.Split(v, ",") {
			i, e := strconv.Atoi(strings.TrimSpace(part))
			if e != nil {
				return st, e
			}
			st.Prerequisites = append(st.Prerequisites, i-1)
		}
	}
	for i, src := range d.Sources {
		fmt.Fprintf(a.Out, "%d. %s\n", i+1, safeText(src.Name))
	}
	v, e = a.Terminal.ReadLine("资料序号；留空保存为未验证大纲 > ")
	if e != nil {
		return st, e
	}
	if v != "" {
		i, e := strconv.Atoi(v)
		if e != nil || i < 1 || i > len(d.Sources) {
			return st, fmt.Errorf("资料序号无效")
		}
		st.NodeRevisionID = d.Sources[i-1].Reference.NodeRevisionID
	}
	return st, nil
}
func reorderPlanningSteps(steps []api.PlanningStep, from, to int) []api.PlanningStep {
	order := []int{}
	for i := range steps {
		if i != from {
			order = append(order, i)
		}
	}
	if to >= 0 {
		order = append(order, 0)
		copy(order[to+1:], order[to:])
		order[to] = from
	}
	mapped := map[int]int{}
	for i, old := range order {
		mapped[old] = i
	}
	result := []api.PlanningStep{}
	for _, old := range order {
		st := steps[old]
		st.Prerequisites = []int{}
		for _, p := range steps[old].Prerequisites {
			if n, ok := mapped[p]; ok {
				st.Prerequisites = append(st.Prerequisites, n)
			}
		}
		result = append(result, st)
	}
	return result
}
func (a *App) showPlanning(d api.PlanningDraft) {
	fmt.Fprintf(a.Out, "\n学习规划草稿 · 版本 %d\n用户原文：%s\n自述基础（非掌握度）：%s\n%s\n", d.Version, goalMultiline(d.Goal.Text), goalMultiline(d.Content.Details.SelfAssessment), safeText(d.ModelSource))
	fmt.Fprintln(a.Out, "覆盖分析仅基于冻结范围内最多 100 个章节、48KB 正文；未列出不代表资料不存在。")
	for _, reason := range d.StaleReasons {
		fmt.Fprintln(a.Out, "已过期：", safeText(reason))
	}
	if d.LastError != "" {
		fmt.Fprintln(a.Out, safeText(d.LastError))
	}
	show := func(content api.PlanningContent) {
		budget, deadline := "未设定", "未设定"
		if content.Details.WeeklyMinutes != nil {
			budget = strconv.Itoa(*content.Details.WeeklyMinutes) + " 分钟/周"
		}
		if content.Details.Deadline != nil {
			deadline = content.Details.Deadline.Format("2006-01-02T15:04:05Z07:00")
		}
		fmt.Fprintf(a.Out, "名称：%s\n自述基础（非学习证据）：%s\n排除项：%s\n优先级：%s\n每周负荷：%s\n截止时间：%s · 时区：%s\n", safeText(content.Details.Name), goalMultiline(content.Details.SelfAssessment), goalMultiline(content.Details.Exclusions), safeText(content.Details.Priority), budget, deadline, safeText(content.Details.Timezone))
		fmt.Fprintf(a.Out, "预期结果：%s\n范围：%s\n用途：%s\n完成标准：%s\n", goalMultiline(content.Details.ExpectedOutcome), goalMultiline(content.Details.Scope), goalMultiline(content.Details.Purpose), goalMultiline(content.Details.CompletionCriteria))
		for _, q := range content.Questions {
			fmt.Fprintf(a.Out, "可选澄清（%s）：%s；已有回答：%s\n", safeText(q.Field), safeText(q.Question), safeText(q.Answer))
		}
		for i, st := range content.Steps {
			source := "无可靠引用，仅建议大纲"
			for _, src := range d.Sources {
				if src.Reference.NodeRevisionID == st.NodeRevisionID {
					source = src.Name
				}
			}
			prereqs := []string{}
			for _, p := range st.Prerequisites {
				if p >= 0 && p < len(content.Steps) {
					prereqs = append(prereqs, content.Steps[p].Name)
				}
			}
			fmt.Fprintf(a.Out, "%d. %s · %d 分钟\n内容：%s\n理由：%s\n练习：%s\n完成依据：%s\n前置：%s\n资料：%s\n", i+1, safeText(st.Name), st.Minutes, goalMultiline(st.Content), goalMultiline(st.Reason), goalMultiline(st.Exercise), goalMultiline(st.Completion), safeText(strings.Join(prereqs, "、")), safeText(source))
		}
		for _, gap := range content.Gaps {
			fmt.Fprintln(a.Out, "待核对缺口：", safeText(gap))
		}
	}
	fmt.Fprintln(a.Out, "当前可编辑草稿（未确认不改变正式目标）：")
	show(d.Content)
	if d.Suggestion != nil {
		fmt.Fprintln(a.Out, "AI 建议（尚未写入草稿或正式目标）：")
		show(*d.Suggestion)
	}
}
