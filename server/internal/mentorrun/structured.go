package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
)

type citationFailure struct{ reason string }

func (*citationFailure) Error() string { return errCitation.Error() }
func (*citationFailure) Unwrap() error { return errCitation }
func citationReason(err error) string {
	var failure *citationFailure
	if errors.As(err, &failure) {
		if failure.reason == "schema_mismatch" {
			return "schema_mismatch"
		}
		return research.ValidationReason(&research.ValidationError{Reason: failure.reason})
	}
	return "invalid_source_citation"
}

const citationSchema = `{"type":"object","properties":{"source_id":{"type":"string"},"revision_id":{"type":"string"},"fragment_id":{"type":"string"},"quote":{"type":"string"}},"required":["source_id","revision_id","fragment_id","quote"],"additionalProperties":false}`

var synthesisSchema = json.RawMessage(`{"type":"object","properties":{"points":{"type":"array","maxItems":12,"items":{"type":"object","properties":{"text":{"type":"string"},"citations":{"type":"array","minItems":1,"maxItems":8,"items":` + citationSchema + `}},"required":["text","citations"],"additionalProperties":false}},"gaps":{"type":"array","maxItems":12,"items":{"type":"string"}},"examples":{"type":"array","maxItems":6,"items":{"type":"string"}}},"required":["points","gaps","examples"],"additionalProperties":false}`)
var preparedSchema = json.RawMessage(`{"type":"object","properties":{"concept_key":{"type":"string"},"name":{"type":"string"},"prompt":{"type":"string"},"criterion":{"type":"string"},"citations":{"type":"array","minItems":1,"maxItems":8,"items":` + citationSchema + `}},"required":["concept_key","name","prompt","criterion","citations"],"additionalProperties":false}`)

func preparedSchemaFor(sources []research.Source) json.RawMessage {
	return quoteConstrainedSchema(preparedSchema, sources, false)
}

func synthesisSchemaFor(sources []research.Source) json.RawMessage {
	return quoteConstrainedSchema(synthesisSchema, sources, true)
}

func quoteConstrainedSchema(base json.RawMessage, sources []research.Source, points bool) json.RawMessage {
	var schema map[string]any
	_ = json.Unmarshal(base, &schema)
	quotes := []string{}
	for _, candidate := range research.CopyableCitations(sources) {
		quotes = append(quotes, candidate.Citation.Quote)
	}
	if len(quotes) > 0 {
		properties := schema["properties"].(map[string]any)
		if points {
			properties = properties["points"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
		}
		quote := properties["citations"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["quote"].(map[string]any)
		quote["enum"] = quotes
	}
	raw, _ := json.Marshal(schema)
	return raw
}

// Server-only structured generation reuses the configured mentor connection and
// its wire API. Every attempt crosses the existing HTTP authorization and lease gate.
func (h *executionHost) generateCited(ctx context.Context, request modelclient.Request, schema json.RawMessage, stage string, validate func([]byte) error) error {
	client, _, native, err := h.service.settings.CitedMentorClient(func(connection settings.Connection, next http.RoundTripper) http.RoundTripper {
		return budgetTransport{host: h, next: next, stage: stage, connection: connection}
	})
	if err != nil || client == nil {
		return ErrModel
	}
	chat := llm.ChatRequest{Schema: schema, SchemaName: stage, UseNativeJSONSchema: native}
	for _, message := range request.Messages {
		chat.Messages = append(chat.Messages, llm.Message{Role: llm.Role(message.Role), Content: message.Content})
	}
	chat.Messages[0].Content += " 直接复制 citation_support 中相关 candidate 的 citation 对象，不计算范围或 hash。只需简短完成当前任务。资料不足如实说明，候选并不证明结论。"
	if stage == "checking_synthesis" && h.body.StartLearning != nil {
		chat.Messages[0].Content += " 为第一项小活动只整理一个最相关的基础要点，最多引用一个候选；gaps 只列最必要的覆盖缺口，examples 返回 []。不做推导、证明、计算或全面综述。"
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err = h.checkpoint(ctx, stage); err != nil {
			return err
		}
		estimateRequest := request
		estimateRequest.Messages = nil
		for _, message := range chat.Messages {
			estimateRequest.Messages = append(estimateRequest.Messages, modelclient.Message{Role: string(message.Role), Content: message.Content})
		}
		if err = (callModel{h}).begin(ctx, estimateRequest); err != nil {
			return err
		}
		if native {
			h.reserve += len(schema)
		}
		if h.reserve > h.limits.ContextTokens {
			return ErrLimit
		}
		started := time.Now()
		response, callErr := client.Chat(ctx, chat)
		if h.transportErr != nil {
			return h.transportErr
		}
		category := "success"
		if callErr != nil {
			category = string(llm.Category(callErr))
		}
		slog.InfoContext(ctx, "mentor_model_completed", "run_id", h.owned.RunID, "model_stage", stage, "attempt", attempt+1, "elapsed_ms", time.Since(started).Milliseconds(), "output_budget", h.limits.OutputTokens, "input_tokens", response.Usage.InputTokens, "output_tokens", response.Usage.OutputTokens, "reasoning_tokens", response.Usage.ReasoningTokens, "total_tokens", response.Usage.TotalTokens, "error_category", category)
		if callErr == nil || llm.Category(callErr) != llm.ErrorTimeout && llm.Category(callErr) != llm.ErrorUnavailable {
			if err = h.service.mutate(ctx, &h.owned, "model_received", func(item *row) error {
				item.callStarted = false
				if response.Usage.TotalTokens > 0 && response.Usage.TotalTokens <= h.reserve {
					item.CostUnknown = false
				}
				item.Stage = stage
				return nil
			}); err != nil {
				return err
			}
		}
		if callErr == nil {
			err = validate(response.JSON)
		} else {
			err = callErr
		}
		if err == nil {
			return nil
		}
		reason := "schema_mismatch"
		if errors.Is(err, errCitation) {
			reason = citationReason(err)
		} else if llm.Category(err) != llm.ErrorSchemaMismatch && llm.Category(err) != llm.ErrorInvalidResponse {
			return err
		}
		slog.WarnContext(ctx, "mentor_generation_rejected", "run_id", h.owned.RunID, "model_stage", stage, "attempt", attempt+1, "error_category", "invalid_source_citation", "rejection_reason", reason)
		if attempt == 1 {
			return err
		}
		feedback, _ := json.Marshal(map[string]string{"rejection_reason": reason, "instruction": "前次生成未通过完整校验。修正该结构或引用问题；重新输出完整 JSON，复制给定候选，不能扩大来源范围。"})
		chat.Messages = append(chat.Messages, llm.Message{Role: llm.RoleUser, Content: string(feedback)})
	}
	return err
}
