package command

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

func (a *App) workbenchProgress(ctx context.Context, client APIClient, req workbench.Request, p workbench.Page) (workbench.Page, error) {
	reader, ok := client.(progressClient)
	if !ok {
		return p, commandError("progress_unavailable", "目标进度查询不可用", "升级客户端与服务端", ExitUnavailable)
	}
	q := api.ProgressQuery{Global: strings.HasPrefix(req.Page, "global-"), GoalID: req.Resource, Status: "active", Cursor: req.Cursor, Limit: 20}
	if req.Resource != "" {
		q.Status = "all"
	}
	// 搜索栏用于明确选择状态，避免客户端截取全集后过滤。
	if req.Search != "" {
		q.Status = req.Search
	}
	p.Searchable = true
	p.Content = "默认展示进行中目标；搜索可填 draft / paused / completed / archived / all。\n按优先级、截止时间排序；活跃时间为估算。\n"
	if strings.Contains(req.Page, "reviews") {
		page, err := reader.ScopedReviews(ctx, q, nil)
		if err != nil {
			return p, mapAPIError(err)
		}
		p.Title = "到期复习"
		p.NextCursor = page.NextCursor
		p.Content += fmt.Sprintf("共 %d 项 · as-of=%d · 更新时间 %s\n", page.Total, page.Metadata.AsOfEventSeq, page.UpdatedAt.Format(time.RFC3339))
		for _, r := range page.Items {
			p.Content += fmt.Sprintf("任务 %s\n学习区 %s · 目标 %s\n到期 %s · 来源证据 %s\n%s\n", r.TaskID, r.SpaceName+"（"+r.LearningSpaceID+"）", r.GoalName+"（"+r.GoalID+"）", r.DueAt.Format(time.RFC3339), r.EvidenceID, r.UnavailableReason)
			if r.Startable {
				p.Entries = append(p.Entries, workbench.Entry{ID: "resume:" + r.LearningSpaceID + "/" + r.SessionID, Label: "进入原会话复习 · " + r.TaskID})
			}
		}
		if page.Total == 0 {
			p.Content += "没有符合条件的到期复习。\n"
		}
		p.Content += progressWarnings(page.Metadata)
	} else {
		page, err := reader.Progress(ctx, q)
		if err != nil {
			return p, mapAPIError(err)
		}
		p.Title = "学习进度与待办"
		p.NextCursor = page.NextCursor
		p.Content += fmt.Sprintf("共 %d 个目标 · as-of=%d · 更新时间 %s\n", page.Total, page.Metadata.AsOfEventSeq, page.UpdatedAt.Format(time.RFC3339))
		for _, g := range page.Items {
			p.Content += "\n" + progressContent(g)
			for _, s := range g.Sessions {
				if s.Resumable {
					p.Entries = append(p.Entries, workbench.Entry{ID: "resume:" + s.LearningSpaceID + "/" + s.SessionID, Label: "继续 " + g.Goal.GoalManagement().Details.Name + " · " + s.SessionID})
				}
			}
		}
		if page.Total == 0 {
			p.Content += "没有符合条件的目标。\n可到资料页查看或导入资料，到目标页创建目标；保存目标不会自动开始教学。\n"
		}
		p.Content += progressWarnings(page.Metadata)
	}
	p.Entries = append(p.Entries, workbench.Entry{ID: "page:global-overview", Label: "全局学习总览"}, workbench.Entry{ID: "page:global-reviews", Label: "全局到期复习"})
	if !q.Global {
		p.Entries = append(p.Entries, workbench.Entry{ID: "agent:" + req.Resource + "/", Label: "打开本区 AI 聊天（不自动选择目标）"})
	}
	p.Content = safeText(p.Content)
	return p, nil
}
func progressWarnings(m api.ProjectionMetadata) string {
	if m.Degraded || m.Incomplete || m.Rebuilding {
		return "\n投影延迟、重建或结果不完整：" + strings.Join(m.ReasonCodes, ", ") + "\n"
	}
	return ""
}
