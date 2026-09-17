package command

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func (a *App) runChanges(ctx context.Context, args []string) error {
	set := newFlagSet("goal changes")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	goal := set.String("id", "", "目标 ID")
	id := set.String("change", "", "变更 ID，省略时读取时间线")
	revision := set.Int64("revision", 0, "不可变候选修订，0 读取当前状态")
	asJSON := set.Bool("json", false, "输出完整差异和状态")
	if err := set.Parse(args); err != nil || *goal == "" || len(set.Args()) != 0 {
		return commandError("usage", "使用 goal changes --id 目标ID [--change 变更ID] [--revision N] [--json]", "goal help", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	client, ok := online.client.(interface {
		LearningChanges(context.Context, string, string, int64) ([]api.LearningChange, error)
	})
	if !ok {
		return commandError("unsupported", "客户端未提供变更查询", "更新客户端", ExitInput)
	}
	items, err := client.LearningChanges(ctx, *goal, *id, *revision)
	if err != nil {
		return mapAPIError(err)
	}
	if *asJSON {
		return json.NewEncoder(a.Out).Encode(items)
	}
	status := map[string]string{"proposed": "已提出", "waiting_approval": "等待审阅", "approved": "已批准，尚未生效", "queued_for_boundary": "排队，本题处理后应用", "applied": "已生效", "stale": "已失效，差异只读", "rejected": "已拒绝", "cancelled": "已取消", "needs_sources": "需要补充来源"}
	if len(items) == 0 {
		fmt.Fprintln(a.Out, "尚无教学变更")
	}
	for _, v := range items {
		label := status[v.Status]
		if label == "" {
			label = v.Status
		}
		fmt.Fprintf(a.Out, "%s · 修订 %d · %s\n会话：%s\n影响：%s\n状态说明：%s\n", safeText(v.ID), v.Revision, safeText(label), safeText(v.SessionID), safeText(v.Impact), safeText(v.Reason))
	}
	return nil
}
