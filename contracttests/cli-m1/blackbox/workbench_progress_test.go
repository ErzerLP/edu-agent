package blackbox

import (
	"encoding/json"
	"net/http"
	"testing"
)

// 通过真实服务、数据库与 CLI 验证概览读取，模型仅使用既有严格夹具。
func TestBlackBoxWorkbenchProgress(t *testing.T) {
	h := newHarness(t)
	h.pairBoth(h.serverURL)
	const spaceID = "00000000-0000-4000-8000-000000000001"
	check := func(label string, total int) {
		t.Helper()
		space := h.runCLI(h.primaryHome, "", "space", "show", "--id", spaceID)
		requireExit(t, space, 0, label+"学习区与能力声明")
		progress := h.runCLI(h.primaryHome, "", "progress", "--space", spaceID, "--json")
		if progress.exit != 0 {
			t.Fatalf("%s：GET /v1/learning/progress 失败：exit=%d %s", label, progress.exit, progress.stderr)
		}
		var page struct {
			Total int               `json:"total"`
			Items []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(progress.stdout, &page) != nil || page.Total != total || len(page.Items) != total {
			t.Fatalf("%s：未返回预期的真实目标数量 %d", label, total)
		}
	}
	check("新初始化无目标", 0)
	t.Run("空区主入口", func(t *testing.T) { h.workbenchPTY(t, "没有符合条件的目标") })
	goal := h.runCLI(h.primaryHome, "", "goal", "set", "尚无教学会话的目标")
	requireExit(t, goal, 0, "保存独立目标")
	goalID := h.scalarString("独立目标身份", `SELECT goal_id FROM learning_goal_revisions ORDER BY created_at DESC LIMIT 1`)
	requireExit(t, h.runCLI(h.primaryHome, "", "goal", "start", "--id", goalID), 0, "启用无会话目标")
	check("有目标无会话", 1)
	h.importFixture(h.primaryHome)
	sessionID := h.setGoal(h.primaryHome, "Understand the stable concept and its verification step")
	goalID = h.scalarString("教学目标身份", `SELECT g.goal_id FROM learning_goal_revisions g JOIN tutoring_sessions s ON s.goal_revision_id=g.id WHERE s.id=$1`, sessionID)
	requireExit(t, h.runCLI(h.primaryHome, "", "goal", "start", "--id", goalID), 0, "启用教学目标")
	check("教学尚无路线", 2)
	// 接受检索降级与已展示路线，随后 EOF 停止，不额外生成题目或证据。
	materialize := h.runCLI(h.primaryHome, "y\ny\n", "learn", "--session", sessionID)
	requireContains(t, materialize.stdout, "Current: proposed route", "生成真实路线")
	credential := h.pairCredential("", "Issue17 结构核查")
	var raw struct {
		Items []struct {
			Sessions []map[string]json.RawMessage `json:"sessions"`
			Nodes    []struct {
				Misconceptions json.RawMessage `json:"misconceptions"`
			} `json:"nodes"`
		} `json:"items"`
	}
	// 原始响应仅在内存中核对字段，不输出学习正文或凭据。
	var payload json.RawMessage
	h.authenticatedJSON(http.MethodGet, h.serverURL+"/v1/learning/progress", credential.Token, nil, http.StatusOK, &payload)
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal("真实进度响应不是合法 JSON")
	}
	found := false
	for _, item := range raw.Items {
		for _, session := range item.Sessions {
			if len(session["route_revision_id"]) > 0 && len(session["node_revision_id"]) > 0 {
				found = true
			}
		}
		for _, node := range item.Nodes {
			if string(node.Misconceptions) == "null" {
				t.Log("真实进度响应：items[].nodes[].misconceptions=null")
			}
		}
	}
	if !found {
		t.Fatal("真实教学未生成会话的路线及节点版本，不能证明覆盖故障")
	}
	t.Log("真实响应 HTTP 200：items[].sessions[] 包含 node_revision_id 和 route_revision_id")
	check("已有路线与节点", 2)
	t.Run("已有学习数据主入口", func(t *testing.T) { h.workbenchPTY(t, "路线 ") })
}
