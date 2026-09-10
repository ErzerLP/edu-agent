package command

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

func goalFields(g api.GoalRevision) []workbench.Field {
	d := g.GoalManagement().Details
	deadline, minutes := "", ""
	if d.Deadline != nil {
		deadline = d.Deadline.Format(time.RFC3339)
	}
	if d.WeeklyMinutes != nil {
		minutes = strconv.Itoa(*d.WeeklyMinutes)
	}
	return []workbench.Field{
		{ID: "name", Label: "目标名称", Value: d.Name}, {ID: "text", Label: "学习意图（多行）", Value: g.Text},
		{ID: "outcome", Label: "预期结果", Value: d.ExpectedOutcome}, {ID: "scope", Label: "范围", Value: d.Scope},
		{ID: "exclusions", Label: "排除项", Value: d.Exclusions}, {ID: "self-assessment", Label: "自述基础（非学习证据）", Value: d.SelfAssessment},
		{ID: "purpose", Label: "用途", Value: d.Purpose}, {ID: "criteria", Label: "完成标准", Value: d.CompletionCriteria},
		{ID: "priority", Label: "优先级 low/normal/high", Value: d.Priority, Choices: []string{"low", "normal", "high"}},
		{ID: "timezone", Label: "IANA 时区（可空）", Value: d.Timezone}, {ID: "deadline", Label: "截止时间 RFC3339（可空）", Value: deadline}, {ID: "weekly-minutes", Label: "每周分钟数（可空）", Value: minutes},
	}
}

func goalContent(g api.GoalRevision) string {
	m := g.GoalManagement()
	text := fmt.Sprintf("%s · %s · 版本 %d\n", m.Details.Name, goalStatus(m.Status), g.Revision)
	for _, f := range goalFields(g) {
		text += f.Label + "：" + f.Value + "\n"
	}
	text += "资料范围：" + m.Details.ScopeSnapshotID + "\n学习标准验证：未验证；自述与手动完成不代表掌握度。"
	if m.Details.ScopeSnapshotID == "" {
		text += "\n尚未选择资料，可保存草稿；开始教学前可从资料页冻结范围并绑定。"
	}
	if m.Completion != nil {
		text += "\n手动完成依据：" + m.Completion.Reason
	}
	if m.RouteAdjustmentNeeded {
		text += "\n范围或标准变化，路线可能需要调整；旧题目仍引用原修订。"
	}
	return text
}

func (a *App) workbenchGoals(ctx context.Context, client APIClient, req workbench.Request, p workbench.Page) (workbench.Page, error) {
	c, ok := client.(goalClient)
	if !ok {
		return p, fmt.Errorf("客户端不支持目标管理")
	}
	p.Title = "区内 · 目标管理"
	if req.Page == "goals" {
		page, err := c.Goals(ctx, req.Search, "", req.Cursor, 30)
		if err != nil {
			return p, mapAPIError(err)
		}
		p.Searchable, p.NextCursor = true, page.NextCursor
		p.Entries = append(p.Entries, workbench.Entry{ID: "page:new-goal", Label: "新建目标草稿"})
		for _, g := range page.Items {
			label := fmt.Sprintf("%s · %s · v%d", g.GoalManagement().Details.Name, goalStatus(g.GoalManagement().Status), g.Revision)
			p.Content += label + "\n"
			p.Entries = append(p.Entries, workbench.Entry{ID: "page:goal/" + g.GoalID, Label: label})
		}
		if len(page.Items) == 0 {
			p.Content = "没有匹配的目标。可新建草稿，不会自动开始教学。"
		}
		return p, nil
	}
	if req.Page == "history" {
		page, err := c.GoalRevisions(ctx, req.Resource, req.Cursor, 20)
		if err != nil {
			return p, mapAPIError(err)
		}
		p.Title, p.NextCursor = "目标 · 历史修订", page.NextCursor
		for _, g := range page.Items {
			p.Content += goalContent(g) + "\n\n"
		}
		return p, nil
	}
	g := api.GoalRevision{Management: &api.GoalManagement{Status: "draft", Details: api.GoalDetails{Priority: "normal"}}}
	if req.Page == "goal" {
		p.Entries = append(p.Entries, workbench.Entry{ID: "agent:" + g.GoalID + "/", Label: "打开此目标的 AI 聊天（与教学续学独立）"})
		var err error
		g, err = c.Goal(ctx, req.Resource)
		if err != nil {
			return p, mapAPIError(err)
		}
	} else {
		g.Management.Details.ScopeSnapshotID = req.Resource
		if req.Resource == "" {
			g.Management.Details.ScopeSnapshotID = req.Scope
		}
	}
	if req.Action != "" {
		if req.Version != g.Revision {
			return p, commandError("version_conflict", "目标已被其他客户端修改；未覆盖", "刷新后检查差异", ExitConflict)
		}
		if req.Action == "new-session" {
			view, err := a.startGoalSessionWithIDs(ctx, client, g, req.Entity, req.Operation)
			if err != nil {
				return p, err
			}
			p.Redirect = "session/" + view.Session.SessionID
			return p, nil
		}
		if g.GoalID == "" {
			g.GoalID = req.Entity
		}
		r := goalRequest(g, req.Operation)
		switch req.Action {
		case "save":
			d := g.GoalManagement().Details
			for key, value := range req.Values {
				if err := setGoalField(&r.Text, &d, key, strings.TrimSpace(value)); err != nil {
					return p, err
				}
			}
			if r.Text == "" {
				r.Text = d.Name
			}
			r.Details = &d
		case "bind-scope":
			if req.Scope == "" {
				return p, fmt.Errorf("请先在资料页选择并冻结范围")
			}
			d := g.GoalManagement().Details
			d.ScopeSnapshotID = req.Scope
			r.Details = &d
		case "start", "pause", "resume", "complete", "archive", "restore":
			r.Action = req.Action
			r.CompletionReason = req.Text
		default:
			return p, fmt.Errorf("不支持的目标操作")
		}
		result, err := a.saveGoal(ctx, c, r)
		if err != nil {
			return p, err
		}
		p.Redirect = "goal/" + result.Result.GoalID
		return p, nil
	}
	p.Content, p.Version = goalContent(g), g.Revision
	p.Actions = []workbench.Action{{ID: "save", Label: "编辑并保存结构化目标", Fields: goalFields(g), DraftKey: fmt.Sprintf("/%s/%d", g.GoalID, g.Revision)}}
	if req.Page == "goal" {
		p.Entries = append(p.Entries, workbench.Entry{ID: "page:history/" + g.GoalID, Label: "查看历史修订"}, workbench.Entry{ID: "page:sessions/" + g.GoalID, Label: "选择此目标的教学会话"})
		p.Entries = append(p.Entries, workbench.Entry{ID: "page:goal-progress/" + g.GoalID, Label: "查看目标进度"}, workbench.Entry{ID: "page:reviews/" + g.GoalID, Label: "查看目标到期复习"})
		p.Actions = append(p.Actions, workbench.Action{ID: "bind-scope", Label: "绑定本进程已选择的资料范围", Confirmation: "绑定已冻结的资料范围？不会改写旧题目。"})
		status := g.GoalManagement().Status
		if status == "draft" || status == "active" {
			p.Actions = append(p.Actions, workbench.Action{ID: "new-session", Label: "明确开始新的独立教学会话"})
		}
		for _, op := range []struct{ id, label string }{{"start", "开始目标"}, {"pause", "暂停目标"}, {"resume", "恢复目标"}, {"complete", "手动完成目标"}, {"archive", "归档目标"}, {"restore", "恢复归档目标"}} {
			act := workbench.Action{ID: op.id, Label: op.label, Confirmation: "仅改变目标状态，不结束或改写教学历史。确认？"}
			if op.id == "complete" {
				act.Input = "手动完成依据（不产生学习证据）"
			}
			p.Actions = append(p.Actions, act)
		}
	}
	return p, nil
}

func (a *App) workbenchSessions(ctx context.Context, client APIClient, req workbench.Request, p workbench.Page) (workbench.Page, error) {
	c, ok := client.(teachingSessionClient)
	if !ok {
		return p, fmt.Errorf("客户端不支持显式教学会话")
	}
	page, err := c.Sessions(ctx, req.Resource, "", req.Cursor, 50)
	if err != nil {
		return p, mapAPIError(err)
	}
	p.Title, p.NextCursor = "区内 · 教学会话选择（与 AI 聊天历史分开）", page.NextCursor
	for _, item := range page.Items {
		label := item.Name + " · " + item.State + " · " + item.Position
		if !item.Resumable {
			label += "（历史）"
		}
		p.Content += label + "\n"
		p.Entries = append(p.Entries, workbench.Entry{ID: "page:session/" + item.SessionID, Label: label})
	}
	if len(page.Items) == 0 {
		p.Content = "没有教学会话。请到目标页明确开始新的教学。"
	}
	p.Entries = append(p.Entries, workbench.Entry{ID: "page:goals", Label: "返回目标列表"})
	return p, nil
}
