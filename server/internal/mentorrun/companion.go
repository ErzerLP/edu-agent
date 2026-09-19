package mentorrun

import (
	"context"
	"encoding/json"
	"time"

	core "github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/packages/agentcore/companion"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/identity"
)

func (s *Service) ConfigureCompanion(b *companion.Broker) { s.companion = b }

// CheckCompanion 重用原身份、对话归属和隐私门禁，不把 Cookie 转成 OS 权限。
func (s *Service) CheckCompanion(ctx context.Context, actor identity.Credential, g *companion.Grant) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, false)
	if err != nil {
		return err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, true); err != nil {
		return err
	}
	if g == nil {
		return nil
	}
	if !validID(g.Space) || !validID(g.Conversation) || generation != g.Generation {
		return ErrInactive
	}
	c, err := conversationRow(ctx, tx, actor, g.Space, g.Conversation, generation, false)
	if err != nil {
		return err
	}
	if c.deleted {
		return ErrNotFound
	}
	if err = s.describeConversation(ctx, tx, &c); err != nil {
		return err
	}
	if !c.Writable || g.Model && c.Destination != g.Destination {
		return ErrInactive
	}
	return nil
}

var companionTool = modelclient.Tool{Type: "function", Function: modelclient.ToolDefinition{Name: "local_operation", Description: "仅调用已明确配对并授权的本机 companion。tool 可为 list/read/stat/prepare_write/prepare_edit/shell/task；arguments 是对应 CLI 工具 JSON 对象。文件修改只生成预览，用户必须在本地连接面板确认，模型不能 commit。Shell 使用显示设备的原生 OS 权限；task 支持 list/status/read/wait/stop/input/close_input/interrupt/eof/resize。未知结果只能核对原操作，不得换 ID 重跑。", Parameters: json.RawMessage(`{"type":"object","properties":{"tool":{"type":"string","enum":["list","read","stat","prepare_write","prepare_edit","shell","task"]},"arguments":{"type":"object"}},"required":["tool","arguments"],"additionalProperties":false}`)}}

func (h *executionHost) localDestination() string {
	v := h.service.settings.View().EffectiveMentor
	return destination(v.Provider, v.Endpoint)
}

func (h *executionHost) localOperation(ctx context.Context, call modelclient.ToolCall) (string, error) {
	var args struct {
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if core.DecodeArguments(call.Function.Arguments, &args) != nil {
		return "", ErrInvalid
	}
	b := h.service.companion
	destination := h.localDestination()
	id := b.ModelConnection(h.owned.token, h.owned.device, h.owned.ConversationID, destination)
	if id == "" {
		return `{"error":"companion_not_authorized"}`, nil
	}
	if _, err := h.goal(ctx); err != nil {
		return "", err
	}
	op := companion.Operation{ID: h.owned.RunID + ":" + call.ID, Run: h.owned.RunID, Tool: args.Tool, Arguments: args.Arguments}
	r, err := b.ModelSubmit(id, h.owned.token, h.owned.device, h.owned.ConversationID, destination, op)
	if err != nil {
		return `{"error":"companion_not_authorized_or_busy"}`, nil
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for r.State == "queued" || r.State == "dispatched" {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			r.State = "unknown"
		case <-ticker.C:
			if _, err := h.goal(ctx); err != nil {
				return "", err
			}
			r, err = b.ModelReceipt(id, op.ID)
			if err != nil {
				return `{"error":"companion_disconnected","outcome":"unknown"}`, nil
			}
		}
	}
	data, _ := json.Marshal(r)
	if len(data) > 8000 {
		data, _ = json.Marshal(map[string]string{"operation_id": op.ID, "outcome": "请在本地连接面板查询完整回执，结果超过模型预算"})
	}
	return string(data), nil
}

// 原始命令、env、input 不进入持久 checkpoint；恢复后不具备可重放参数。
func redactLocalCalls(message modelclient.Message) modelclient.Message {
	message.ToolCalls = append([]modelclient.ToolCall(nil), message.ToolCalls...)
	for i := range message.ToolCalls {
		if message.ToolCalls[i].Function.Name == "local_operation" {
			message.ToolCalls[i].Function.Arguments = `{"redacted":true}`
		}
	}
	return message
}
