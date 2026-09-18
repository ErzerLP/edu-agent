package mentorrun

import (
	"context"
	"encoding/json"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

func (h *executionHost) readProgress(ctx context.Context) (string, error) {
	if h.service.progress == nil {
		return "进度聚合暂不可用，不能据此判断没有复习。", nil
	}
	if _, err := h.goal(ctx); err != nil {
		return "", err
	}
	scoped, err := learningspace.WithScope(ctx, h.owned.SpaceID)
	if err != nil {
		return "", err
	}
	page, err := h.service.progress.Progress(scoped, learning.ProgressQuery{GoalID: h.owned.GoalID, Status: "all", Limit: 1})
	if err != nil {
		return "进度读取未完成，不能据此判断没有复习。", nil
	}
	if _, err = h.goal(ctx); err != nil {
		return "", err
	}
	raw, err := json.Marshal(page)
	if len(raw) > 32000 {
		return "进度记录超过本次工具输出上限，请在进度页查看原记录。", nil
	}
	return string(raw), err
}
