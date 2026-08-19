package llmselector

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
)

type fakeChat struct {
	result  llm.ChatResult
	err     error
	request llm.ChatRequest
}

func (f *fakeChat) Chat(_ context.Context, request llm.ChatRequest) (llm.ChatResult, error) {
	f.request = request
	return f.result, f.err
}

func TestSelectorUsesStructuredChatContract(t *testing.T) {
	response := knowledge.SelectorResponse{
		KnowledgeRevisionID: "10000000-0000-4000-8000-000000000001",
		CandidateSetHash:    "candidate-hash",
		Decisions:           []knowledge.Decision{{NodeRevisionID: "20000000-0000-4000-8000-000000000001", Action: "select"}},
	}
	encoded, _ := json.Marshal(response)
	client := &fakeChat{result: llm.ChatResult{JSON: encoded}}
	selector := New(client)
	result, err := selector.Select(context.Background(), knowledge.SelectorRequest{
		KnowledgeRevisionID: response.KnowledgeRevisionID, CandidateSetHash: response.CandidateSetHash,
		Query: "channel", Candidates: []knowledge.Candidate{{NodeRevisionID: response.Decisions[0].NodeRevisionID, Title: "Channel"}},
	})
	if err != nil || result.CandidateSetHash != response.CandidateSetHash || len(result.Decisions) != 1 {
		t.Fatalf("selector response: result=%+v err=%v", result, err)
	}
	if len(client.request.Schema) == 0 || client.request.SchemaName != "knowledge_selector_response" || len(client.request.Messages) != 2 {
		t.Fatalf("structured chat request was not used: %+v", client.request)
	}
}

func TestSelectorRequiresExplicitDecisionsArray(t *testing.T) {
	base := `{"knowledge_revision_id":"10000000-0000-4000-8000-000000000001","candidate_set_hash":"hash"`
	for _, test := range []struct {
		name    string
		payload string
		wantErr bool
	}{
		{name: "missing", payload: base + `}`, wantErr: true},
		{name: "null", payload: base + `,"decisions":null}`, wantErr: true},
		{name: "empty", payload: base + `,"decisions":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, err := New(&fakeChat{result: llm.ChatResult{JSON: json.RawMessage(test.payload)}}).Select(t.Context(), knowledge.SelectorRequest{})
			var failure *knowledge.SelectorFailure
			if test.wantErr {
				if !errors.As(err, &failure) || failure.Reason != "selector_schema_error" {
					t.Fatalf("missing/null decisions error = %v", err)
				}
				return
			}
			if err != nil || response.Decisions == nil || len(response.Decisions) != 0 {
				t.Fatalf("explicit empty decisions = %+v err=%v", response, err)
			}
		})
	}
}

func TestSelectorClassifiesModelAndSchemaFailures(t *testing.T) {
	tests := []struct {
		name   string
		client *fakeChat
		reason string
	}{
		{name: "timeout", client: &fakeChat{err: &llm.Error{Category: llm.ErrorTimeout}}, reason: "selector_timeout"},
		{name: "malformed", client: &fakeChat{result: llm.ChatResult{JSON: json.RawMessage(`{"knowledge_revision_id":"x","candidate_set_hash":"h","decisions":[],"extra":true}`)}}, reason: "selector_schema_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.client).Select(context.Background(), knowledge.SelectorRequest{Query: "q"})
			var failure *knowledge.SelectorFailure
			if !errors.As(err, &failure) || failure.Reason != test.reason {
				t.Fatalf("failure = %v", err)
			}
		})
	}
}
