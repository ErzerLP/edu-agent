package blackbox

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"runtime"
	"testing"
)

type managedGoal struct {
	ID         string `json:"goal_id"`
	Revision   int64  `json:"revision"`
	Text       string `json:"text"`
	Management struct {
		Status               string `json:"status"`
		CriteriaVerification string `json:"criteria_verification"`
		Details              struct {
			Name          string `json:"name"`
			Scope         string `json:"scope"`
			Snapshot      string `json:"scope_snapshot_id"`
			Timezone      string `json:"timezone"`
			Deadline      string `json:"deadline"`
			WeeklyMinutes int    `json:"weekly_minutes"`
		} `json:"details"`
	} `json:"management"`
}

func TestBlackBoxGoalsWithoutModelPersistenceAndIndependentLifecycle(t *testing.T) {
	h := newHarnessWithOptions(t, harnessOptions{withoutModel: true})
	h.primaryHome = h.newCLIHome("goals-user")
	h.pair(h.primaryHome, h.serverURL, "目标管理验收")
	run := func(args ...string) commandResult {
		t.Helper()
		r := h.runCLI(h.primaryHome, "", args...)
		requireExit(t, r, 0, "目标管理命令")
		return r
	}
	decode := func(r commandResult) managedGoal {
		t.Helper()
		var g managedGoal
		if err := json.Unmarshal(r.stdout, &g); err != nil || g.ID == "" {
			t.Fatalf("目标响应不完整：%s %v", r.stdout, err)
		}
		return g
	}
	spaceResult := run("space", "create", "--name", "Go 后端")
	var space struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(spaceResult.stdout, &space); err != nil || space.ID == "" {
		t.Fatal("学习区创建失败")
	}
	goalA, goalB, operation := randomUUID(t), randomUUID(t), randomUUID(t)
	create := []string{"--space", space.ID, "goal", "create", "--id", goalA, "--operation-id", operation, "--json", "掌握并发编程"}
	a := decode(run(create...))
	if a.Management.Status != "draft" || a.Management.Details.Snapshot != "" || a.Management.Details.Deadline != "" {
		t.Fatalf("空资料草稿带有虚构约束：%+v", a)
	}
	if replay := decode(run(create...)); replay != a {
		t.Fatal("跨进程重试未返回原目标")
	}
	if n := h.scalarInt("未创建教学会话", `SELECT count(*) FROM tutoring_sessions`); n != 0 {
		t.Fatal("草稿隐式创建了教学会话")
	}
	decode(run("--space", space.ID, "goal", "start", "--id", goalA, "--json"))
	before := decode(run("--space", space.ID, "goal", "show", "--id", goalA, "--json"))
	decode(run("--space", space.ID, "goal", "create", "--id", goalB, "--name", "掌握并发编程", "--json", "准备后端面试"))
	decode(run("--space", space.ID, "goal", "start", "--id", goalB, "--json"))
	if after := decode(run("--space", space.ID, "goal", "show", "--id", goalA, "--json")); after != before {
		t.Fatal("创建 B 改变了 A")
	}
	var page struct {
		Items  []managedGoal `json:"items"`
		Cursor string        `json:"next_cursor"`
	}
	first := run("--space", space.ID, "goal", "list", "--search", "掌握并发", "--status", "active", "--limit", "1", "--json")
	if err := json.Unmarshal(first.stdout, &page); err != nil || len(page.Items) != 1 || page.Cursor == "" {
		t.Fatal("重名目标搜索、筛选或分页失效")
	}
	firstID := page.Items[0].ID
	second := run("--space", space.ID, "goal", "list", "--search", "掌握并发", "--status", "active", "--limit", "1", "--cursor", page.Cursor, "--json")
	if err := json.Unmarshal(second.stdout, &page); err != nil || len(page.Items) != 1 || page.Items[0].ID == firstID {
		t.Fatal("分页重复目标")
	}
	collection := randomUUID(t)
	run("--space", space.ID, "knowledge", "library", "create", "--id", collection, "--name", "Go 教材", "--source", "本地教材")
	_, file, _, _ := runtime.Caller(0)
	run("--space", space.ID, "knowledge", "import", "--collection", collection, filepath.Join(filepath.Dir(file), "testdata", "topic.md"))
	revision := h.scalarString("集合版本", `SELECT head_revision_id FROM knowledge_collections WHERE id=$1`, collection)
	snapshot := randomUUID(t)
	entries, _ := json.Marshal([]map[string]string{{"collection_id": collection, "revision_id": revision}})
	run("--space", space.ID, "knowledge", "library", "freeze", "--id", snapshot, "--entries", string(entries))
	edited := decode(run("--space", space.ID, "goal", "edit", "--id", goalA, "--scope", "Channel\nContext", "--self-assessment", "自述熟悉 goroutine", "--materials", snapshot, "--timezone", "Asia/Shanghai", "--deadline", "2026-12-31T18:00:00+08:00", "--weekly-minutes", "180", "--json"))
	if edited.Management.Details.Snapshot != snapshot || edited.Management.Details.Scope != "Channel\nContext" || edited.Management.Details.WeeklyMinutes != 180 {
		t.Fatal("结构化修订丢失")
	}
	stale := h.runCLI(h.primaryHome, "", "--space", space.ID, "goal", "edit", "--id", goalA, "--expected-version", "2", "--name", "不得覆盖")
	if stale.exit != 4 || stableErrorCode(stale.stderr) != "version_conflict" {
		t.Fatalf("旧版本未明确拒绝：%d %s", stale.exit, stale.stderr)
	}
	for _, action := range []string{"pause", "archive", "restore", "resume"} {
		run("--space", space.ID, "goal", action, "--id", goalA)
	}
	completed := decode(run("--space", space.ID, "goal", "complete", "--id", goalB, "--reason", "用户自行结束本轮准备", "--json"))
	if completed.Management.Status != "completed" || completed.Management.CriteriaVerification != "unverified" {
		t.Fatal("手动完成冒充了学习标准验证")
	}
	if n := h.scalarInt("不得生成学习证据", `SELECT count(*) FROM learning_evidence`); n != 0 {
		t.Fatal("自述或手动完成生成了证据")
	}
	session := h.setGoal(h.primaryHome, "保持已有教学会话")
	sessionBefore := h.scalarString("原会话", `SELECT to_jsonb(s)::text FROM tutoring_sessions s WHERE id=$1`, session)
	run("goal", "set", "旧入口也只保存目标")
	if sessionAfter := h.scalarString("会话不变", `SELECT to_jsonb(s)::text FROM tutoring_sessions s WHERE id=$1`, session); sessionAfter != sessionBefore {
		t.Fatal("旧 goal set 修改了已有会话")
	}
	saved := decode(run("--space", space.ID, "goal", "show", "--id", goalA, "--json"))
	h.serverProcess.stop(t)
	h.serverProcess = startProcess(t, "edu-agentd-restarted", serverBin, []string{"serve"}, h.serverEnv, openProcessLog(t, "edu-agentd-restarted"))
	waitHTTPStatus(t, h.serverURL+"/livez", http.StatusOK, nil)
	if reloaded := decode(run("--space", space.ID, "goal", "show", "--id", goalA, "--json")); reloaded != saved {
		t.Fatal("服务重启丢失目标详情")
	}
	history := run("--space", space.ID, "goal", "history", "--id", goalA, "--json")
	page.Cursor = ""
	if err := json.Unmarshal(history.stdout, &page); err != nil || len(page.Items) != 7 || page.Items[0].Management.Details.Snapshot != "" {
		t.Fatalf("原目标版本被覆盖：%s", history.stdout)
	}
	h.assertHomeFilesExclude(h.primaryHome, "Channel", "自述熟悉 goroutine", "用户自行结束本轮准备")
}
