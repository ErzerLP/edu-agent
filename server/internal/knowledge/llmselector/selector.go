package llmselector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
)

type ChatClient interface {
	Chat(context.Context, llm.ChatRequest) (llm.ChatResult, error)
}

type Selector struct {
	client ChatClient
}

func New(client ChatClient) *Selector {
	return &Selector{client: client}
}

func (s *Selector) Select(ctx context.Context, request knowledge.SelectorRequest) (knowledge.SelectorResponse, error) {
	if s == nil || s.client == nil {
		return knowledge.SelectorResponse{}, &knowledge.SelectorFailure{Reason: "selector_not_configured"}
	}
	input, err := json.Marshal(struct {
		KnowledgeRevisionID  string                   `json:"knowledge_revision_id"`
		Query                string                   `json:"query"`
		QueryContextVersion  string                   `json:"query_context_schema_version"`
		Context              map[string]any           `json:"context,omitempty"`
		ParentNodeRevisionID string                   `json:"parent_node_revision_id"`
		CandidateSetHash     string                   `json:"candidate_set_hash"`
		RemainingBudget      int                      `json:"remaining_budget"`
		Candidates           []knowledge.Candidate    `json:"candidates"`
		SummarySnapshot      []knowledge.NodeArtifact `json:"summary_snapshot,omitempty"`
	}{
		KnowledgeRevisionID: request.KnowledgeRevisionID, Query: request.Query,
		QueryContextVersion: request.QueryContextVersion, Context: request.Context,
		ParentNodeRevisionID: request.ParentNodeRevisionID, CandidateSetHash: request.CandidateSetHash,
		RemainingBudget: request.RemainingBudget, Candidates: request.Candidates,
		SummarySnapshot: request.SummarySnapshot,
	})
	if err != nil {
		return knowledge.SelectorResponse{}, &knowledge.SelectorFailure{Reason: "selector_schema_error", Cause: err}
	}
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"required":["knowledge_revision_id","candidate_set_hash","decisions"],"properties":{"knowledge_revision_id":{"type":"string"},"candidate_set_hash":{"type":"string"},"decisions":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["node_revision_id","action"],"properties":{"node_revision_id":{"type":"string"},"action":{"type":"string"}}}}}}`)
	result, err := s.client.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "Select only from the ordered candidates. Return decisions in candidate order. Valid actions are select, expand, and select_expand. Return an empty decisions array to stop this branch."},
			{Role: llm.RoleUser, Content: string(input)},
		},
		Schema: schema, SchemaName: "knowledge_selector_response", UseNativeJSONSchema: false,
	})
	if err != nil {
		return knowledge.SelectorResponse{}, &knowledge.SelectorFailure{Reason: selectorReason(err), Cause: err}
	}
	decoder := json.NewDecoder(bytes.NewReader(result.JSON))
	decoder.DisallowUnknownFields()
	var wire struct {
		KnowledgeRevisionID string          `json:"knowledge_revision_id"`
		CandidateSetHash    string          `json:"candidate_set_hash"`
		Decisions           json.RawMessage `json:"decisions"`
	}
	if err := decoder.Decode(&wire); err != nil {
		return knowledge.SelectorResponse{}, &knowledge.SelectorFailure{Reason: "selector_schema_error", Cause: err}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return knowledge.SelectorResponse{}, &knowledge.SelectorFailure{Reason: "selector_schema_error", Cause: fmt.Errorf("selector returned trailing JSON")}
	}
	if len(wire.Decisions) == 0 || bytes.Equal(bytes.TrimSpace(wire.Decisions), []byte("null")) {
		return knowledge.SelectorResponse{}, &knowledge.SelectorFailure{Reason: "selector_schema_error", Cause: fmt.Errorf("selector decisions must be an array")}
	}
	decisionDecoder := json.NewDecoder(bytes.NewReader(wire.Decisions))
	decisionDecoder.DisallowUnknownFields()
	var decisions []knowledge.Decision
	if err := decisionDecoder.Decode(&decisions); err != nil {
		return knowledge.SelectorResponse{}, &knowledge.SelectorFailure{Reason: "selector_schema_error", Cause: err}
	}
	if err := decisionDecoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return knowledge.SelectorResponse{}, &knowledge.SelectorFailure{Reason: "selector_schema_error", Cause: fmt.Errorf("selector decisions contain trailing JSON")}
	}
	return knowledge.SelectorResponse{
		KnowledgeRevisionID: wire.KnowledgeRevisionID,
		CandidateSetHash:    wire.CandidateSetHash,
		Decisions:           decisions,
	}, nil
}

func selectorReason(err error) string {
	switch llm.Category(err) {
	case llm.ErrorTimeout:
		return "selector_timeout"
	case llm.ErrorInvalidResponse, llm.ErrorSchemaMismatch, llm.ErrorInvalidRequest:
		return "selector_schema_error"
	case llm.ErrorUnauthorized:
		return "selector_unauthorized"
	case llm.ErrorRateLimited:
		return "selector_rate_limited"
	case llm.ErrorUnavailable:
		return "selector_unavailable"
	default:
		return "selector_upstream_error"
	}
}
