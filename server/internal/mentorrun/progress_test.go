package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

type progressReader func(context.Context, learning.ProgressQuery) (learning.ProgressPage, error)

func (read progressReader) Progress(ctx context.Context, q learning.ProgressQuery) (learning.ProgressPage, error) {
	return read(ctx, q)
}

func TestPostgreSQLMentorProgressBindsScopeAndRechecksGoal(t *testing.T) {
	f := fixture(t, func(http.ResponseWriter, *http.Request, int) { t.Error("只读进度不应调用模型") })
	f.accept(t)
	owned, err := f.service.claim(t.Context())
	if err != nil || owned == nil {
		t.Fatalf("未取得原运行：%v", err)
	}
	host := executionHost{service: f.service, owned: *owned}
	page := learning.ProgressPage{Items: []learning.GoalProgress{}, DataCleared: true}
	f.service.ConfigureProgress(progressReader(func(ctx context.Context, q learning.ProgressQuery) (learning.ProgressPage, error) {
		if learningspace.Scope(ctx) != owned.SpaceID || q.GoalID != owned.GoalID || q.Global || q.Status != "all" || q.Limit != 1 {
			t.Fatalf("工具扩大了绑定范围：%+v", q)
		}
		return page, nil
	}))
	result, err := host.readProgress(t.Context())
	want, _ := json.Marshal(page)
	if err != nil || result != string(want) {
		t.Fatalf("工具未直接返回正式聚合：%s %v", result, err)
	}
	_, err = host.Execute(t.Context(), []modelclient.ToolCall{{ID: "篡改范围", Function: modelclient.ToolFunction{Name: "read_learning_progress", Arguments: `{"goal_id":"其他目标"}`}}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("接受了模型指定的其他目标：%v", err)
	}
	f.service.ConfigureProgress(progressReader(func(ctx context.Context, _ learning.ProgressQuery) (learning.ProgressPage, error) {
		_, err := f.pool.Exec(ctx, `UPDATE learning_aggregate_heads SET aggregate_version=aggregate_version+1 WHERE aggregate_type='goal' AND aggregate_id=$1`, owned.GoalID)
		return page, err
	}))
	if result, err = host.readProgress(t.Context()); err == nil || result != "" {
		t.Fatalf("目标变化后仍返回旧结果：%s %v", result, err)
	}
}
