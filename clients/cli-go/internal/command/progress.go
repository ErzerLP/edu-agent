package command

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

type progressClient interface {
	Progress(context.Context, api.ProgressQuery) (api.ProgressPage, error)
	ScopedReviews(context.Context, api.ProgressQuery, *time.Time) (api.ReviewsPage, error)
}

func (a *App) runScopedProgress(ctx context.Context, args []string, reviews bool) error {
	set := newFlagSet("progress")
	var flags onlineFlags
	var q api.ProgressQuery
	var asJSON bool
	var dueText string
	var all bool
	addOnlineFlags(set, &flags)
	set.BoolVar(&q.Global, "global", false, "读取全部学习区")
	set.StringVar(&q.GoalID, "goal-id", "", "目标身份")
	set.StringVar(&q.Status, "status", "active", "active/draft/paused/completed/archived/all")
	set.StringVar(&q.Order, "order", "priority", "priority/recent")
	set.StringVar(&q.Cursor, "cursor", "", "服务端分页游标")
	set.IntVar(&q.Limit, "limit", 50, "每页数量")
	set.BoolVar(&asJSON, "json", false, "输出服务端完整结果")
	set.BoolVar(&all, "all", false, "兼容参数；显示全部目标状态，分页仍使用 cursor")
	if reviews {
		set.StringVar(&dueText, "due-before", "", "到期时间上界，默认当前时间")
	}
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "进度查询参数无效", "使用 --global、--goal-id、--status、--limit、--cursor 或 --json", ExitInput)
	}
	if all {
		q.Status = "all"
	}
	if err := validatePageInput(q.Limit, q.Cursor); err != nil {
		return err
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	client, ok := online.client.(progressClient)
	if !ok {
		return commandError("progress_unavailable", "客户端不支持目标进度", "升级客户端", ExitUnavailable)
	}
	if reviews {
		var due *time.Time
		if dueText != "" {
			due, err = parseDueBefore(dueText)
			if err != nil {
				return err
			}
		}
		page, err := client.ScopedReviews(ctx, q, due)
		if err != nil {
			return mapAPIError(err)
		}
		if asJSON {
			return json.NewEncoder(a.Out).Encode(page)
		}
		printProjectionWarning(a.Err, page.Metadata)
		fmt.Fprintf(a.Out, "到期复习：%d 项 · 截止 %s · as-of=%d\n", page.Total, page.DueBefore.Format(time.RFC3339), page.Metadata.AsOfEventSeq)
		for _, r := range page.Items {
			fmt.Fprintln(a.Out, safeText(fmt.Sprintf("任务 %s · 区 %s · 目标 %s\n到期 %s · 可开始=%t %s\n原会话 %s · 来源证据 %s\n", r.TaskID, r.SpaceName+"（"+r.LearningSpaceID+"）", r.GoalName+"（"+r.GoalID+"）", r.DueAt.Format(time.RFC3339), r.Startable, r.UnavailableReason, r.SessionID, r.EvidenceID)))
		}
		if page.NextCursor != "" {
			fmt.Fprintf(a.Out, "下一页：--cursor %s\n", safeText(page.NextCursor))
		}
		return nil
	}
	page, err := client.Progress(ctx, q)
	if err != nil {
		return mapAPIError(err)
	}
	if asJSON {
		return json.NewEncoder(a.Out).Encode(page)
	}
	printProjectionWarning(a.Err, page.Metadata)
	fmt.Fprintf(a.Out, "目标：%d · as-of=%d · 更新时间 %s\n", page.Total, page.Metadata.AsOfEventSeq, page.UpdatedAt.Format(time.RFC3339))
	for _, g := range page.Items {
		fmt.Fprintln(a.Out, progressContent(g))
	}
	if page.NextCursor != "" {
		fmt.Fprintf(a.Out, "下一页：--cursor %s\n", safeText(page.NextCursor))
	}
	return nil
}

func progressContent(g api.GoalProgress) string {
	m := g.Goal.GoalManagement()
	var b strings.Builder
	fmt.Fprintf(&b, "学习区：%s（%s）\n统计为本目标有效历史；旧版本证据保留真实来源，不代表已满足当前目标。\n", safeText(g.SpaceName), g.LearningSpaceID)
	fmt.Fprintf(&b, "%s · %s · 优先级 %s\n目标 %s · 版本 %d\n有效证据 %d · 待确认 %d · 活跃时间约 %d 秒（估算）\n", safeText(m.Details.Name), m.Status, m.Details.Priority, g.Goal.GoalID, g.Goal.Revision, g.EvidenceCount, len(g.Pending), g.EstimatedSeconds)
	if m.Completion != nil {
		fmt.Fprintf(&b, "目标完成：%s（不代表路线或掌握度完成）\n", m.Completion.Kind)
	}
	if len(g.Routes) == 0 {
		b.WriteString("路线：尚未开始，百分比未知\n")
	}
	for _, r := range g.Routes {
		fmt.Fprintf(&b, "路线 %s：%d/%d；依据 %s；对应当前目标版本=%t\n", r.Route.RouteRevisionID, r.Numerator, r.Denominator, r.Basis, r.CurrentGoalRevision)
	}
	for _, n := range g.Nodes {
		fmt.Fprintf(&b, "节点 %s：掌握=%s，证据=%d，待确认=%d\n", n.Mastery.NodeRevisionID, n.Mastery.State, n.Mastery.ValidEvidenceCount, n.Mastery.PendingAssessments)
	}
	for _, pending := range g.Pending {
		fmt.Fprintf(&b, "待确认评估 %s · 节点 %s · %s\n", pending.AssessmentID, pending.NodeRevisionID, strings.Join(pending.Reasons, ", "))
	}
	for _, review := range g.Reviews {
		fmt.Fprintf(&b, "复习 %s · 到期 %s · 可开始=%t %s\n", review.TaskID, review.DueAt.Format(time.RFC3339), review.Startable, review.UnavailableReason)
	}
	if len(g.Sessions) == 0 {
		b.WriteString("尚无教学会话；可到目标页开始学习。\n")
	}
	for _, s := range g.Sessions {
		fmt.Fprintf(&b, "下一步 %s · 会话 %s · 可继续=%t\n", s.Position, s.SessionID, s.Resumable)
	}
	return safeText(b.String())
}
