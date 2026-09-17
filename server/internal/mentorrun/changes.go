package mentorrun

import (
	"context"
	"encoding/json"
	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var changeTools = []modelclient.Tool{
	{Type: "function", Function: modelclient.ToolDefinition{Name: "read_learning_context", Description: "读取绑定教学会话的路线、题目、授权来源、目标标准与正式学习证据；必须先读再提出变更。", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}},
	{Type: "function", Function: modelclient.ToolDefinition{Name: "propose_learning_change", Description: "提出当前会话的教学变更。仅解释可直接追加；路线会安全排队或等待审批；目标标准始终等待具体确认。steps 为后续建议，前置索引只能指向较早步骤。goal 需沿用读取的完整字段，只改用户要求的范围/标准。", Parameters: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","enum":["explanation","route","goal"]},"trigger":{"type":"string","enum":["user_request","activity_feedback","authorized_source","goal_constraint"]},"reason":{"type":"string"},"evidence_ids":{"type":"array","items":{"type":"string"}},"context_id":{"type":"string"},"explanation":{"type":"string"},"steps":{"type":"array","maxItems":30,"items":{"type":"object","properties":{"node_revision_id":{"type":"string"},"name":{"type":"string"},"criterion":{"type":"string"},"prompt":{"type":"string"},"difficulty":{"type":"integer","minimum":1,"maximum":5},"prerequisites":{"type":"array","items":{"type":"integer"}}},"required":["node_revision_id","name","criterion","prompt","difficulty","prerequisites"],"additionalProperties":false}},"goal":{"type":"object","properties":{"name":{"type":"string"},"expected_outcome":{"type":"string"},"scope":{"type":"string"},"exclusions":{"type":"string"},"self_assessment":{"type":"string"},"purpose":{"type":"string"},"completion_criteria":{"type":"string"},"priority":{"type":"string"},"scope_snapshot_id":{"type":"string"},"timezone":{"type":"string"},"deadline":{"type":["string","null"]},"weekly_minutes":{"type":["integer","null"]}},"additionalProperties":false}},"required":["kind","trigger","reason","evidence_ids","context_id","explanation","steps"],"additionalProperties":false}`)}},
}

func (h *executionHost) changeTool(ctx context.Context, call modelclient.ToolCall) (string, error) {
	if h.owned.TeachingSessionID == "" || !h.service.changes.Available() {
		return "", ErrInvalid
	}
	scoped, err := learningspace.WithScope(ctx, h.owned.SpaceID)
	if err != nil {
		return "", err
	}
	actor := identity.Credential{Device: identity.Device{ID: h.owned.device}, TokenID: h.owned.token}
	if call.Function.Name == "read_learning_context" {
		var args struct{}
		if agentcore.DecodeArguments(call.Function.Arguments, &args) != nil {
			return "", ErrInvalid
		}
		snap, e := h.service.changes.Snapshot(scoped, actor, h.owned.GoalID, h.owned.TeachingSessionID)
		if e != nil {
			return "", e
		}
		h.body.ChangeBase = &snap.Base
		raw, e := json.Marshal(snap)
		return string(raw), e
	}
	var candidate learningchange.Candidate
	if agentcore.DecodeArguments(call.Function.Arguments, &candidate) != nil || h.body.ChangeBase == nil {
		return "", ErrInvalid
	}
	operation := uuid.NewSHA1(uuid.MustParse(h.owned.RunID), []byte("change:"+call.ID)).String()
	var change learningchange.Change
	err = h.service.mutateTx(scoped, &h.owned, "learning_change", func(tx pgx.Tx, item *row) error {
		var e error
		change, e = h.service.changes.ChangeTx(scoped, tx, actor, h.owned.GoalID, h.owned.TeachingSessionID, operation, learningchange.Command{OperationID: operation, Action: "propose", Base: h.body.ChangeBase, Candidate: &candidate})
		if e == nil {
			item.body = h.body
			item.callStarted = false
		}
		return e
	})
	if err != nil {
		return "", err
	}
	// 模型只拿到真实状态与候选身份，不把批准和已应用混为一谈。
	raw, err := json.Marshal(change)
	return string(raw), err
}
