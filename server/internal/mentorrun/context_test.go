package mentorrun

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/settings"
)

// This catches loading cumulative history before the model window is applied.
func TestPostgreSQLBoundedConversationBeyondCheckpoint(t *testing.T) {
	var last modelclient.Request
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if err := json.NewDecoder(r.Body).Decode(&last); err != nil {
			t.Error(err)
		}
		stream(w, modelclient.Message{Role: "assistant", Content: "继续回答"})
	})
	id := newHistory(t, f, true, "")
	ids := []string{}
	for i := 0; i < 3; i++ {
		ids = append(ids, sendHistory(t, f, id, "旧问题").RunID)
		f.work(t)
	}
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = gates(ctx, tx, true); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("较早的完整原文。", 6000)
	for _, run := range ids {
		raw, e := sealHistory(f.service.aead, historyAAD(id, 1, run), Body{TopicProvenance: true, Messages: []modelclient.Message{{Role: "user", Content: text}, {Role: "assistant", Content: "旧回答"}}, Output: "旧回答"})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, `UPDATE learning_tutor_turns SET ciphertext=$2 WHERE run_id=$1`, run, raw); e != nil {
			t.Fatal(e)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for after, count := int64(0), 0; ; {
		p := historyPage(t, f, id, after, 1)
		if len(p.Items) != 1 || p.Items[0].Messages[0].Content != text {
			t.Fatal("原文分页丢失")
		}
		count++
		if p.NextCursor == 0 {
			if count != 3 {
				t.Fatal(count)
			}
			break
		}
		after = p.NextCursor
	}
	request := historyRequest(t, f, id, "继续说明")
	receipt, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, runtimeGoal(""), request)
	if err != nil {
		t.Fatalf("长历史必须继续受理：%v", err)
	}
	f.work(t)
	var snapshot Snapshot
	if err = f.service.ReadSnapshot(ctx, f.actor, learningspace.DefaultID, receipt.RunID, func(s Snapshot) error { snapshot = s; return nil }); err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "succeeded" {
		t.Fatalf("长历史未继续：%+v", snapshot.Meta)
	}
	_, limits, _, _ := f.settings.MentorClient()
	if agentcore.NewTokenEstimator().EstimateRequest(last)+limits.OutputTokens+max(256, (limits.ContextTokens+19)/20) > limits.ContextTokens {
		t.Fatal("发出超窗请求")
	}
	if !strings.Contains(snapshot.Output, "较早内容未纳入") {
		t.Fatal("缺少降级提示", snapshot.Output)
	}
	p := historyPage(t, f, id, 3, 1)
	if len(p.Items) != 1 || len(p.Items[0].Messages) != 2 || p.Items[0].Messages[0].Content != "继续说明" {
		t.Fatal("本轮保存边界错误", p)
	}
}

func TestBoundedContextKeepsToolGroups(t *testing.T) {
	h := executionHost{limits: settings.Limits{ContextTokens: 4096, OutputTokens: 256}}
	messages := []modelclient.Message{{Role: "user", Content: "当前问题必须保留"}}
	for i := 0; i < 30; i++ {
		id := strings.Repeat("x", i+1)
		messages = append(messages, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: id, Type: "function", Function: modelclient.ToolFunction{Name: "read_goal", Arguments: `{}`}}}}, modelclient.Message{Role: "tool", ToolCallID: id, Content: strings.Repeat("大工具结果", 3000)})
	}
	plan, err := h.assembleContext(context.Background(), modelclient.Request{MaxTokens: 256, Messages: []modelclient.Message{{Role: "system", Content: "系统"}}}, nil, messages)
	if err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{}
	found := false
	for _, m := range plan.Request.Messages {
		if m.Content == "当前问题必须保留" {
			found = true
		}
		for _, c := range m.ToolCalls {
			pending[c.ID] = true
		}
		if m.Role == "tool" {
			if !pending[m.ToolCallID] {
				t.Fatal("孤立工具结果")
			}
			delete(pending, m.ToolCallID)
		}
	}
	if len(pending) != 0 || !found {
		t.Fatal("工具组或当前问题丢失", pending, found)
	}
	if plan.EstimatedInput+plan.ReservedOutput+256 > 4096 {
		t.Fatal("窗口超限")
	}
}

func TestBoundedContextPrioritizesCurrentToolResult(t *testing.T) {
	h := executionHost{limits: settings.Limits{ContextTokens: 16000, OutputTokens: 256}}
	result := strings.Repeat("工具事实", 1000) + "末尾的关键事实"
	messages := []modelclient.Message{{Role: "user", Content: "当前问题"}, {Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "current", Type: "function", Function: modelclient.ToolFunction{Name: "read_goal", Arguments: `{}`}}}}, {Role: "tool", ToolCallID: "current", Content: result}}
	plan, err := h.assembleContext(context.Background(), modelclient.Request{MaxTokens: 256, Messages: []modelclient.Message{{Role: "system", Content: "系统"}}}, nil, messages)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Request.Messages[len(plan.Request.Messages)-1]; got.Role != "tool" || got.Content != result {
		t.Fatal("可放下的当前工具结果被提前裁剪")
	}
}

func TestToolCheckpointRetainsDisplayWithoutCumulativeResults(t *testing.T) {
	body := Body{Context: &ConversationContext{}, Messages: []modelclient.Message{{Role: "user", Content: "原始问题"}}}
	for i := 0; i < 400; i++ {
		id := strings.Repeat("c", i+1)
		body.appendMessage(modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: id, Type: "function", Function: modelclient.ToolFunction{Name: "read_goal", Arguments: `{}`}}}})
		body.appendMessage(modelclient.Message{Role: "tool", ToolCallID: id, Content: strings.Repeat("工具正文", 1000)})
		body.compactToolCheckpoint()
		if checkpointJSONSize(body) > MaxBody {
			t.Fatal("旧工具结果耗尽 checkpoint")
		}
	}
	body.appendMessage(modelclient.Message{Role: "assistant", Content: "最后回答"})
	body.Output = "最后回答"
	original := turnBody(row{body: body})
	if original.Messages[0].Content != "原始问题" || original.Messages[len(original.Messages)-1].Content != "最后回答" {
		t.Fatal("展示原文被模型投影替换")
	}
}

func TestPostgreSQLOversizeInputSummaryFailureContinues(t *testing.T) {
	summaries := 0
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		var request modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if agentcore.NewTokenEstimator().EstimateRequest(request)+request.MaxTokens+256 > 4096 {
			t.Error("发送超窗输入")
		}
		if strings.Contains(request.Messages[0].Content, "当前输入片段") {
			summaries++
			http.Error(w, "deterministic summary failure", http.StatusBadRequest)
			return
		}
		stream(w, modelclient.Message{Role: "assistant", Content: "依据当前片段继续"})
	})
	var update settings.Update
	if err := json.Unmarshal([]byte(`{"expected_revision":1,"limits":{"context_tokens":4096}}`), &update); err != nil {
		t.Fatal(err)
	}
	update.ExpectedRevision = f.settings.View().Revision
	if _, err := f.settings.Update(update); err != nil {
		t.Fatal(err)
	}
	id := newHistory(t, f, true, "")
	receipt := sendHistory(t, f, id, strings.Repeat("问", 5000))
	f.work(t)
	snapshot := f.snapshot(t, receipt.RunID)
	if snapshot.Status != "succeeded" || !strings.Contains(snapshot.Output, "当前输入正文未完整纳入") || summaries != 1 {
		t.Fatal("临时整理失败没有明确继续", snapshot, summaries)
	}
	var accounted int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_model_calls c JOIN learning_model_tasks t ON t.id=c.task_id WHERE t.origin_id=$1 AND c.stage='input_projection'`, receipt.RunID).Scan(&accounted); err != nil || accounted != 1 {
		t.Fatal("临时整理未入台账", accounted, err)
	}
	if historyPage(t, f, id, 0, 1).Items[0].Messages[0].Content != strings.Repeat("问", 5000) {
		t.Fatal("超大原始输入被投影改写")
	}
}
