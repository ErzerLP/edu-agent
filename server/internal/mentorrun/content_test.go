package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

func editFixture(t *testing.T) (*runtimeFixture, learningcontent.Revision) {
	t.Helper()
	f := startFixture(t, "Go 并发")
	r := f.accept(t)
	f.work(t)
	started := f.snapshot(t, r.RunID).StartLearning.Result
	if started == nil {
		t.Fatal("前置未发布活动")
	}
	content := f.service.starter.Content
	content.ConfigureReferences(f.service.starter.Knowledge)
	value, err := content.Get(context.Background(), f.actor, started.ArtifactID, 1)
	if err != nil {
		t.Fatal(err)
	}
	b := value.Body.Blocks[0]
	f.create = Create{OperationID: uuid.NewString(), SessionID: uuid.NewString(), ExpectedVersion: 1, Prompt: "请换个例子", Save: true, RequestBudget: 2, TokenBudget: 50000, ContentEdit: &learningcontent.EditRequest{Action: "example", Selection: learningcontent.Selection{SpaceID: value.SpaceID, GoalID: value.GoalID, SessionID: value.SessionID, ArtifactID: value.ArtifactID, Version: value.Version, BlockID: b.ID, Start: 0, End: len(b.Text), Hash: learningcontent.TextHash(b.Text)}}}
	return f, value
}

func TestPostgreSQLContentCollaborationModelVersionsAndLibrary(t *testing.T) {
	f, original := editFixture(t)
	ctx := context.Background()
	content := f.service.starter.Content
	var activityBefore []byte
	if err := f.pool.QueryRow(ctx, `SELECT to_jsonb(a) FROM learning_activities a WHERE id=$1`, original.ActivityID).Scan(&activityBefore); err != nil {
		t.Fatal(err)
	}
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		var request modelclient.Request
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("模型输入无法解析")
			return
		}
		var input struct {
			Selection learningcontent.Selection `json:"selection"`
			Selected  string                    `json:"selected_text"`
		}
		if len(request.Messages) != 2 || json.Unmarshal([]byte(request.Messages[1].Content), &input) != nil || input.Selection != f.create.ContentEdit.Selection || input.Selected != original.Body.Blocks[0].Text {
			t.Error("模型未收到原版精确定位")
		}
		stream(w, modelclient.Message{Content: `{"text":"把通道想象为传递纸条的队列。","reference_ids":[]}`})
	}
	accepted := f.accept(t)
	f.work(t)
	result := f.snapshot(t, accepted.RunID)
	if result.Status != "succeeded" || result.ContentEdit == nil || result.ContentEdit.Result == nil || result.ContentEdit.Result.Version != 2 {
		t.Fatalf("局部内容未提交：%+v", result)
	}
	updated, err := content.Get(ctx, f.actor, original.ArtifactID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Body.Blocks[1].Text != "把通道想象为传递纸条的队列。" || !reflect.DeepEqual(updated.Body.Blocks[0], original.Body.Blocks[0]) || !reflect.DeepEqual(updated.Body.Blocks[2:], original.Body.Blocks[1:]) {
		t.Fatal("局部更新破坏未选块")
	}
	var activityAfter []byte
	if err = f.pool.QueryRow(ctx, `SELECT to_jsonb(a) FROM learning_activities a WHERE id=$1`, original.ActivityID).Scan(&activityAfter); err != nil || !bytes.Equal(activityBefore, activityAfter) {
		t.Fatal("原题、评分或活动引用被更改", err)
	}
	if replay, err := f.service.Create(ctx, f.actor, original.SpaceID, f.goal, f.create); err != nil || replay.RunID != accepted.RunID {
		t.Fatal("受理未幂等", err)
	}
	bad := f.create
	bad.OperationID = uuid.NewString()
	if _, err = f.service.Create(ctx, f.actor, original.SpaceID, f.goal, bad); !errors.Is(err, learningcontent.ErrConflict) {
		t.Fatal("过期选区被受理", err)
	}
	page, err := content.Library(ctx, f.actor, learningcontent.LibraryQuery{GoalID: f.goal, Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ArtifactID != original.ArtifactID || page.Items[0].Favorite {
		t.Fatal("未收藏内容没有自动收录", page, err)
	}
	one := int64(1)
	if _, err = content.Preference(ctx, f.actor, original.ArtifactID, &learningcontent.Preference{Favorite: true, PinnedVersion: &one}); err != nil {
		t.Fatal(err)
	}
	page, err = content.Library(ctx, f.actor, learningcontent.LibraryQuery{Favorite: true, Limit: 10, NodeID: original.Body.References[0].NodeID})
	if err != nil || len(page.Items) != 1 || *page.Items[0].PinnedVersion != 1 {
		t.Fatal("收藏/知识点/固定版本不一致", page, err)
	}
	for _, format := range []string{"markdown", "json"} {
		if value, err := content.Export(ctx, f.actor, original.ArtifactID, 2, format); err != nil || value.Text == "" {
			t.Fatal("导出失败", err)
		}
	}
	if citation, err := content.Citation(ctx, f.actor, original.ArtifactID, 2, original.Body.References[0].NodeRevisionID); err != nil || citation.Reference.Slice != original.Body.References[0].Slice {
		t.Fatal("原始来源无法定位", err)
	}
	if _, err = content.Citation(ctx, f.actor, original.ArtifactID, 2, uuid.NewString()); !errors.Is(err, learningcontent.ErrNotFound) {
		t.Fatal("伪造引用被接受", err)
	}
	command := learningcontent.Restore{OperationID: uuid.NewString(), ExpectedVersion: 2, Version: 1, Reason: "恢复基线说明"}
	restored, err := content.Restore(ctx, f.actor, original.ArtifactID, command)
	if err != nil || restored.Version != 3 || !reflect.DeepEqual(restored.Body.Blocks, original.Body.Blocks) {
		t.Fatal("未生成补偿版", err)
	}
	if replay, err := content.Restore(ctx, f.actor, original.ArtifactID, command); err != nil || replay.Version != 3 {
		t.Fatal("恢复不幂等", err)
	}
	// 来源撤销后，旧版、导出和已结束运行都不可恢复受限正文。
	if _, err = f.pool.Exec(ctx, `DELETE FROM knowledge_collection_links WHERE space_id=$1`, original.SpaceID); err != nil {
		t.Fatal(err)
	}
	if _, err = content.Get(ctx, f.actor, original.ArtifactID, 1); !errors.Is(err, learningcontent.ErrForbidden) {
		t.Fatal("来源失效后历史泄漏", err)
	}
	if _, err = content.Export(ctx, f.actor, original.ArtifactID, 2, "json"); !errors.Is(err, learningcontent.ErrForbidden) {
		t.Fatal("来源失效后导出泄漏", err)
	}
	if err = f.service.ReadSnapshot(ctx, f.actor, original.SpaceID, accepted.RunID, func(Snapshot) error { t.Error("已发送受限正文"); return nil }); !errors.Is(err, learningcontent.ErrForbidden) {
		t.Fatal("运行恢复绕过来源权限", err)
	}
}

func TestPostgreSQLContentRejectsPartialBudgetAndConcurrentEdit(t *testing.T) {
	for _, scenario := range []string{"invalid", "budget", "conflict", "stop", "commit_failure"} {
		t.Run(scenario, func(t *testing.T) {
			f, original := editFixture(t)
			ctx := context.Background()
			if scenario == "commit_failure" {
				if _, err := f.pool.Exec(ctx, `CREATE FUNCTION reject_content_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.version>1 THEN RAISE EXCEPTION '验收写入故障'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_content_commit BEFORE INSERT ON learning_content_revisions FOR EACH ROW EXECUTE FUNCTION reject_content_commit()`); err != nil {
					t.Fatal(err)
				}
			}
			f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
				if scenario == "conflict" {
					_, err := f.service.starter.Content.Restore(ctx, f.actor, original.ArtifactID, learningcontent.Restore{OperationID: uuid.NewString(), ExpectedVersion: 1, Version: 1, Reason: "并发恢复"})
					if err != nil {
						t.Error(err)
					}
				}
				if scenario == "stop" {
					id, err := f.service.Current(ctx, f.actor, learningspace.DefaultID, f.goal, "content_edit")
					if err != nil {
						t.Error(err)
						return
					}
					s := f.snapshot(t, id)
					if _, err = f.service.Command(ctx, f.actor, learningspace.DefaultID, id, Command{OperationID: uuid.NewString(), ExpectedVersion: s.Version, Kind: "stop"}); err != nil {
						t.Error(err)
					}
				}
				text := `{"text":"候选正文","reference_ids":[]}`
				if scenario == "invalid" {
					text = `{"text":"真实的部分输出`
				}
				stream(w, modelclient.Message{Content: text})
			}
			if scenario == "budget" {
				f.create.TokenBudget = 1
			}
			r := f.accept(t)
			f.work(t)
			snapshot := f.snapshot(t, r.RunID)
			if snapshot.Status == "succeeded" || snapshot.ContentEdit.Result != nil {
				t.Fatal("未完成候选被保存", snapshot)
			}
			if scenario == "invalid" && snapshot.Output == "" {
				t.Fatal("丢失真实部分输出")
			}
			if scenario == "budget" && snapshot.Status != "paused_budget" {
				t.Fatal("预算状态不真实", snapshot)
			}
			if scenario == "conflict" && snapshot.Reason != "selection_expired" {
				t.Fatal("没有说明并发冲突", snapshot)
			}
			var count int
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_content_revisions WHERE artifact_id=$1`, original.ArtifactID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 1
			if scenario == "conflict" {
				want = 2
			}
			if count != want {
				t.Fatal(fmt.Sprintf("半成品进入正式版本：%d", count))
			}
		})
	}
}

func TestPostgreSQLContentConcurrentPatchAndDerivedReuse(t *testing.T) {
	f, original := editFixture(t)
	ctx := context.Background()
	content := f.service.starter.Content
	request := *f.create.ContentEdit
	apply := func(operation string) (learningcontent.Revision, error) {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			return learningcontent.Revision{}, err
		}
		defer tx.Rollback(context.Background())
		r, err := content.PatchTx(ctx, tx, f.actor, operation, request, learningcontent.Candidate{Text: "并发例子"}, "用户请求示例", "fixture")
		if err != nil {
			return r, err
		}
		return r, tx.Commit(ctx)
	}
	operations := []string{uuid.NewString(), uuid.NewString()}
	type outcome struct {
		index    int
		revision learningcontent.Revision
		err      error
	}
	results := make(chan outcome, 2)
	for i, operation := range operations {
		go func() { r, err := apply(operation); results <- outcome{i, r, err} }()
	}
	winner := -1
	conflicts := 0
	for range 2 {
		r := <-results
		if r.err == nil {
			winner = r.index
		} else if errors.Is(r.err, learningcontent.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(r.err)
		}
	}
	if winner < 0 || conflicts != 1 {
		t.Fatal("并发加工没有唯一胜者", winner, conflicts)
	}
	if replay, err := apply(operations[winner]); err != nil || replay.Version != 2 {
		t.Fatal("局部命令未幂等", err)
	}
	current, err := content.Get(ctx, f.actor, original.ArtifactID, 0)
	if err != nil {
		t.Fatal(err)
	}
	command := learningcontent.Reuse{OperationID: uuid.NewString(), ExpectedVersion: 2, SourceArtifactID: original.ArtifactID, SourceVersion: 2, SourceBlockID: current.Body.Blocks[1].ID, Reason: "将说明再次整理到正文"}
	derived, err := content.Reuse(ctx, f.actor, original.ArtifactID, command)
	if err != nil || derived.Version != 3 || !reflect.DeepEqual(derived.Body.References, original.Body.References) || len(derived.Body.Lineage) < 2 {
		t.Fatal("重复使用丢失原始来源链或伪造外部来源", err)
	}
	if replay, err := content.Reuse(ctx, f.actor, original.ArtifactID, command); err != nil || replay.Version != 3 {
		t.Fatal("引用操作未幂等", err)
	}
	other, _ := learningspace.WithScope(ctx, uuid.NewString())
	if _, err = content.Reuse(other, f.actor, original.ArtifactID, command); !errors.Is(err, learningcontent.ErrNotFound) {
		t.Fatal("跨区引用被接受", err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,'knowledge:read') WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err = content.Get(ctx, f.actor, original.ArtifactID, 1); !errors.Is(err, learningcontent.ErrForbidden) {
		t.Fatal("撤销来源读取权限后仍可读历史", err)
	}
}
