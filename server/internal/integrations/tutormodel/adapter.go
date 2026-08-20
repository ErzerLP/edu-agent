package tutormodel

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/learning"
)

type Adapter struct{ client *llm.Client }

func New(client *llm.Client) *Adapter {
	if client == nil {
		return nil
	}
	return &Adapter{client: client}
}

type modelError struct {
	category string
	cause    error
}

func (e *modelError) Error() string         { return fmt.Sprintf("tutor model failed: %s", e.category) }
func (e *modelError) Unwrap() error         { return e.cause }
func (e *modelError) ModelCategory() string { return e.category }

func (a *Adapter) Generate(ctx context.Context, request learning.ProposalRequest) (json.RawMessage, error) {
	schema := schemaFor(request.Type)
	if schema == nil {
		return nil, &modelError{category: "invalid_request"}
	}
	input, err := json.Marshal(request)
	if err != nil {
		return nil, &modelError{category: "invalid_request", cause: err}
	}
	result, err := a.client.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleSystem, Content: "Return only a JSON object matching the requested tutoring proposal schema. Never create canonical IDs, state, evidence, mastery, or dates."}, {Role: llm.RoleAssistant, Content: "I will return only the requested proposal JSON."}, {Role: llm.RoleUser, Content: string(input)}}, Schema: schema, SchemaName: "tutoring_proposal", UseNativeJSONSchema: true})
	if err != nil {
		return nil, &modelError{category: category(llm.Category(err)), cause: err}
	}
	return result.JSON, nil
}
func category(value llm.ErrorCategory) string {
	switch value {
	case llm.ErrorTimeout:
		return "timeout"
	case llm.ErrorRateLimited:
		return "rate_limited"
	case llm.ErrorUnavailable:
		return "unavailable"
	case llm.ErrorUpstream:
		return "upstream_error"
	case llm.ErrorInvalidResponse:
		return "malformed_json"
	case llm.ErrorSchemaMismatch:
		return "schema_mismatch"
	case llm.ErrorUnauthorized:
		return "unauthorized"
	case llm.ErrorIncompatible:
		return "incompatible"
	default:
		return "invalid_request"
	}
}
func schemaFor(kind learning.ProposalType) json.RawMessage {
	const reference = `{"type":"object","properties":{"node_revision_id":{"type":"string"},"slice_sha256":{"type":"string"},"range":{"type":"object","properties":{"start":{"type":"integer"},"end":{"type":"integer"}},"required":["start","end"],"additionalProperties":false}},"required":["node_revision_id"],"additionalProperties":false}`
	const sourceRange = `{"type":"object","properties":{"start":{"type":"integer"},"end":{"type":"integer"}},"required":["start","end"],"additionalProperties":false}`
	switch kind {
	case learning.ProposalRoute:
		return json.RawMessage(`{"type":"object","properties":{"route":{"type":"array","items":{"type":"object","properties":{"node_revision_id":{"type":"string"},"teaching_intent":{"type":"string"},"completion_condition":{"type":"string"}},"required":["node_revision_id","teaching_intent","completion_condition"],"additionalProperties":false}}},"required":["route"],"additionalProperties":false}`)
	case learning.ProposalActivity:
		rubricItem := `{"type":"object","properties":{"rubric_item_id":{"type":"string"},"criterion":{"type":"string"},"required_reference_ids":{"type":"array","items":{"type":"string"}}},"required":["rubric_item_id","criterion"],"additionalProperties":false}`
		objectiveRule := `{"type":"object","properties":{"accepted_answers":{"type":"array","items":{"type":"string"}},"case_sensitive":{"type":"boolean"},"trim_space":{"type":"boolean"}},"required":["accepted_answers","case_sensitive","trim_space"],"additionalProperties":false}`
		rubric := `{"type":"object","properties":{"rubric_revision":{"type":"string"},"items":{"type":"array","items":` + rubricItem + `},"objective_rule":` + objectiveRule + `},"required":["rubric_revision","items"],"additionalProperties":false}`
		activity := `{"type":"object","properties":{"prompt":{"type":"string"},"type":{"type":"string","enum":["objective","open"]},"rubric":` + rubric + `,"difficulty":{"type":"integer"},"allowed_help":{"type":"array","items":{"type":"string","enum":["none","hint","scaffold","answer_revealed"]}},"knowledge_references":{"type":"array","minItems":1,"items":` + reference + `}},"required":["prompt","type","rubric","difficulty","allowed_help","knowledge_references"],"additionalProperties":false}`
		return json.RawMessage(`{"type":"object","properties":{"activity":` + activity + `},"required":["activity"],"additionalProperties":false}`)
	case learning.ProposalAssessment:
		item := `{"type":"object","properties":{"rubric_item_id":{"type":"string"},"conclusion":{"type":"string","enum":["pass","partial","fail","unassessed"]},"answer_quote":{"type":"string"},"answer_range":` + sourceRange + `,"answer_quote_sha256":{"type":"string"},"knowledge_reference_id":{"type":"string"},"knowledge_quote":{"type":"string"},"knowledge_range":` + sourceRange + `,"knowledge_quote_sha256":{"type":"string"},"misconception_candidate":{"type":"string"}},"required":["rubric_item_id","conclusion","answer_quote","answer_range","answer_quote_sha256","knowledge_reference_id","knowledge_quote","knowledge_range","knowledge_quote_sha256"],"additionalProperties":false}`
		assessment := `{"type":"object","properties":{"items":{"type":"array","items":` + item + `},"rubric_complete":{"type":"boolean"},"confidence":{"type":"integer"},"risk_flags":{"type":"array","items":{"type":"string"}}},"required":["items","rubric_complete","confidence","risk_flags"],"additionalProperties":false}`
		return json.RawMessage(`{"type":"object","properties":{"assessment":` + assessment + `},"required":["assessment"],"additionalProperties":false}`)
	case learning.ProposalFreeAnswer, learning.ProposalExplanation:
		text := `{"type":"object","properties":{"text":{"type":"string"},"knowledge_references":{"type":"array","minItems":1,"items":` + reference + `}},"required":["text","knowledge_references"],"additionalProperties":false}`
		return json.RawMessage(`{"type":"object","properties":{"text":` + text + `},"required":["text"],"additionalProperties":false}`)
	default:
		return nil
	}
}
