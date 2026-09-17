package mentorrun

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

func TestPostgreSQLTaskListUsesOriginalOwnerAndDevice(t *testing.T) {
	f := fixture(t, func(http.ResponseWriter, *http.Request, int) {})
	ctx := context.Background()
	first := f.accept(t)
	// 使用原运行行模拟不同 owner 阶段，不创建任务中心状态表。
	if _, err := f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET state=state||'{"kind":"research","status":"partial","stage":"fetching","result_unknown":true}'::jsonb WHERE id=$1`, first.RunID); err != nil {
		t.Fatal(err)
	}
	f.create.OperationID = uuid.NewString()
	if _, err := f.service.Command(ctx, f.actor, learningspace.DefaultID, first.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Kind: "clear"}); err != nil {
		t.Fatal(err)
	}
	second := f.accept(t)
	if _, err := f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET state=state||'{"kind":"content_edit","status":"waiting_approval","stage":"candidate_ready"}'::jsonb WHERE id=$1`, second.RunID); err != nil {
		t.Fatal(err)
	}
	read := func(space string, q ListQuery) Page {
		t.Helper()
		var result Page
		if err := f.service.ReadList(ctx, f.actor, space, q, func(page Page) error { result = page; return nil }); err != nil {
			t.Fatal(err)
		}
		for _, item := range result.Items {
			if item.Output != "" || item.Research != nil || item.ContentEdit != nil {
				t.Fatal("列表泄漏正文")
			}
		}
		return result
	}
	page := read(learningspace.DefaultID, ListQuery{Limit: 1})
	if len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("未分页：%+v", page)
	}
	next := read(learningspace.DefaultID, ListQuery{Limit: 1, Cursor: page.NextCursor})
	if len(next.Items) != 1 || next.Items[0].RunID == page.Items[0].RunID || next.NextCursor != "" {
		t.Fatalf("重复或丢失运行：%+v", next)
	}
	filtered := read(learningspace.DefaultID, ListQuery{Limit: 20, Kind: "content_edit", Status: "waiting_approval"})
	if len(filtered.Items) != 1 || filtered.Items[0].RunID != second.RunID || filtered.Items[0].Stage != "candidate_ready" {
		t.Fatalf("未使用原状态：%+v", filtered)
	}
	if len(read(uuid.NewString(), ListQuery{Limit: 20}).Items) != 0 {
		t.Fatal("列表跨区泄漏")
	}
	other := f.actor
	other.Device.ID = uuid.NewString()
	if err := f.service.ReadList(ctx, other, learningspace.DefaultID, ListQuery{Limit: 20}, func(Page) error { t.Fatal("越权列表已发送"); return nil }); err == nil {
		t.Fatal("未拒绝伪造设备")
	}
	for _, q := range []ListQuery{{Limit: 101}, {Limit: 20, Kind: "sync"}, {Limit: 20, Cursor: "invalid"}, {Limit: 20, Status: "success"}} {
		if err := f.service.ReadList(ctx, f.actor, learningspace.DefaultID, q, func(Page) error { return nil }); !errors.Is(err, ErrInvalid) {
			t.Fatalf("错误筛选被接受：%+v %v", q, err)
		}
	}
	if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET revoked_at=now() WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReadList(ctx, f.actor, learningspace.DefaultID, ListQuery{Limit: 20}, func(Page) error { t.Fatal("撤销设备仍收到列表"); return nil }); err == nil {
		t.Fatal("撤销未生效")
	}
}
