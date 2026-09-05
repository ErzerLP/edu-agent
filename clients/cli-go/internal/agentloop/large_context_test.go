package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentlimits"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type largeContextModel struct {
	mu       sync.Mutex
	requests []modelclient.Request
	text     string
}

func (m *largeContextModel) Complete(_ context.Context, r modelclient.Request) (modelclient.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, r)
	if len(r.Tools) == 1 {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant"}}, nil
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: m.text}}, nil
}
func largeContextSession(t *testing.T, m Model, mode string) *Session {
	t.Helper()
	s, err := New(m, &fakeServer{}, Options{ContextWindow: 272000, MaxTokens: 128000, ContextCompaction: mode, NewUUID: func() (string, error) { return "10000000-0000-4000-8000-000000000001", nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func assertLargeContextBudget(t *testing.T, plan ContextPlan, window int) {
	t.Helper()
	if plan.EstimatedInput+plan.ReservedOutput+plan.SafetyMargin > window || plan.SafetyMargin < divideRoundUp(window*5, 100) || plan.Request.MaxTokens != plan.ReservedOutput || plan.ReservedOutput < 1 || plan.ReservedOutput > 128000 {
		t.Fatalf("invalid budget: input=%d output=%d margin=%d window=%d", plan.EstimatedInput, plan.ReservedOutput, plan.SafetyMargin, window)
	}
}

type largeContextCompleteOnly struct{ client *modelclient.Client }

func (m largeContextCompleteOnly) Complete(ctx context.Context, r modelclient.Request) (modelclient.Response, error) {
	return m.client.Complete(ctx, r)
}

func TestLargeContextDefaultRequestsAndLongResponses(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			text := strings.Repeat("answer-line-data\n", 8193)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var wire struct {
					MaxTokens int  `json:"max_tokens"`
					Stream    bool `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
					t.Error(err)
					return
				}
				if wire.MaxTokens != 128000 || wire.Stream != streaming {
					t.Errorf("wire=%+v", wire)
				}
				if !streaming {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": modelclient.Message{Role: "assistant", Content: text}, "finish_reason": "stop"}}})
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for i := 0; i < 8193; i++ {
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer-line-data\\n\"}}]}\n\n")
				}
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			client, err := modelclient.New(server.URL, "fake", "", 10*time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}
			var model Model = client
			if !streaming {
				model = largeContextCompleteOnly{client}
			}
			s := largeContextSession(t, model, ContextCompactionRecentOnly)
			var displayed strings.Builder
			ctx := WithActivityReporter(t.Context(), func(a Activity) {
				if a.Kind == ActivityTextDelta {
					displayed.WriteString(a.Delta)
				}
			})
			result, err := s.Send(ctx, "hello")
			if err != nil || result.Text != text {
				t.Fatalf("send len=%d err=%v", len(result.Text), err)
			}
			if streaming && displayed.String() != text {
				t.Fatal("activity truncated long stream")
			}
			plan, err := s.contextPlan()
			if err != nil {
				t.Fatal(err)
			}
			assertLargeContextBudget(t, plan, 272000)
		})
	}
}

func TestLargeContextCheckpointResumeAndSourceProjection(t *testing.T) {
	text := strings.Repeat("<", agentlimits.MaxAssistantTextBytes)
	model := &largeContextModel{text: text}
	s := largeContextSession(t, model, ContextCompactionAuto)
	if result, err := s.Send(t.Context(), "保留约束"); err != nil || result.Text != text {
		t.Fatalf("send=%v", err)
	}
	checkpoint, err := s.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeSessionCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= 8<<20 {
		t.Fatalf("fixture did not exercise old checkpoint bound: %d", len(data))
	}
	decoded, err := DecodeSessionCheckpoint(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Messages[len(decoded.Messages)-1].Content != text {
		t.Fatal("checkpoint lost body")
	}
	resumed := largeContextSession(t, &largeContextModel{text: "continued"}, ContextCompactionAuto)
	if err := resumed.RestoreCheckpoint(decoded); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Send(t.Context(), "继续"); err != nil {
		t.Fatal(err)
	}
	if !resumed.ContextStatus().Degraded || resumed.ContextStatus().DegradedCode != "context_history_projected" {
		t.Fatalf("projection not visible: %+v", resumed.ContextStatus())
	}
	m := resumed.model.(*largeContextModel)
	m.mu.Lock()
	requests := append([]modelclient.Request(nil), m.requests...)
	m.mu.Unlock()
	found := false
	for _, request := range requests {
		if len(request.Tools) <= 1 {
			continue
		}
		if NewTokenEstimator().EstimateRequest(request)+request.MaxTokens+13600 > 272000 {
			t.Fatal("resumed request exceeds window")
		}
		for _, message := range request.Messages {
			if !strings.Contains(message.Content, "context_history_projected") {
				continue
			}
			var projected struct {
				SourceID      string `json:"source_id"`
				SHA256        string `json:"sha256"`
				OriginalBytes int    `json:"original_bytes"`
				Degraded      bool   `json:"degraded"`
			}
			if err := json.Unmarshal([]byte(message.Content), &projected); err != nil {
				t.Fatal(err)
			}
			if !validOpaqueID(projected.SourceID, "src_") || len(projected.SHA256) != 64 || projected.OriginalBytes != len(text) || !projected.Degraded {
				t.Fatalf("bad provenance: %+v", projected)
			}
			resumed.contextRuntime.mu.Lock()
			_, exists := resumed.contextRuntime.ledger.Sources[projected.SourceID]
			resumed.contextRuntime.mu.Unlock()
			if !exists {
				t.Fatal("untraceable source")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no source projection")
	}
	// Projection is request-local, not destructive storage truncation.
	after, err := resumed.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if after.Messages[1].Content != text {
		t.Fatal("saved history was truncated")
	}
	for i := 0; i < 5; i++ {
		decoded.Messages = append(decoded.Messages, modelclient.Message{Role: "assistant", Content: text})
		decoded.MessageTurnIDs = append(decoded.MessageTurnIDs, decoded.MessageTurnIDs[0])
	}
	if _, err := EncodeSessionCheckpoint(decoded); err == nil {
		t.Fatal("checkpoint quota not enforced")
	}
	future := checkpoint
	future.SchemaVersion++
	if _, err := EncodeSessionCheckpoint(future); !errors.Is(err, ErrCheckpointVersionUnsupported) {
		t.Fatalf("future version=%v", err)
	}
}

func TestLargeContextProjectionPreservesToolsOffAndProtectedTurns(t *testing.T) {
	long := strings.Repeat("x", 1<<20)
	messages := []modelclient.Message{{Role: "system", Content: "rules"}, {Role: "user", Content: "只授权这一次文件发布"}, toolMessage("effect", "write", `{}`), {Role: "tool", ToolCallID: "effect", Content: `{"publication_outcome":"completed","path":"notes.md"}`}, {Role: "assistant", Content: long}, {Role: "user", Content: "continue"}}
	planner := ContextPlanner{ContextWindow: 272000, MaxTokens: 128000, Mode: ContextCompactionAuto, Estimator: NewTokenEstimator()}
	plan, err := planner.Plan(messages, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertLargeContextBudget(t, plan, 272000)
	if plan.ProjectedTurns != 1 || len(plan.Request.Messages) != len(messages) || plan.Request.Messages[1].Content != messages[1].Content || plan.Request.Messages[2].ToolCalls[0].ID != "effect" || plan.Request.Messages[3].Content != messages[3].Content || plan.Request.Messages[3].ToolCallID != "effect" {
		t.Fatal("projection lost user or tool facts")
	}
	planner.Mode = ContextCompactionOff
	if _, err := planner.Plan(messages, nil, nil); err == nil {
		t.Fatal("off silently clipped")
	}
	planner.Mode = ContextCompactionAuto
	planner.ProtectedGroups = map[int]bool{0: true}
	if _, err := planner.Plan(messages, nil, nil); err == nil {
		t.Fatal("protected round clipped")
	}
	if _, err := planner.Plan([]modelclient.Message{{Role: "user", Content: long}}, nil, nil); err == nil {
		t.Fatal("current uncompressible accepted")
	}
	for _, limit := range []int{1, 512, 64000, 128000} {
		p := ContextPlanner{ContextWindow: 272000, MaxTokens: limit, Estimator: NewTokenEstimator()}
		plan, err := p.Plan([]modelclient.Message{{Role: "user", Content: "hi"}}, nil, nil)
		if err != nil || plan.Request.MaxTokens != limit {
			t.Fatalf("limit=%d err=%v", limit, err)
		}
		assertLargeContextBudget(t, plan, 272000)
	}
	if err := validateModelMessage(modelclient.Message{Role: "assistant", Content: long + "x"}); err == nil {
		t.Fatal("agent accepted oversized body")
	}
}

func TestLargeContextInternalCompressionRequestsStaySmall(t *testing.T) {
	m := &largeContextModel{}
	_, _ = runObserver(t.Context(), m, NewTokenEstimator(), 272000, observerSnapshot{Sources: []SourceEntry{{ID: "source", RecallText: "hello"}}})
	_, _ = runReflector(t.Context(), m, NewTokenEstimator(), 272000, reflectorSnapshot{Observations: []Observation{{ID: "observation", Content: "hello"}}})
	if len(m.requests) != 2 {
		t.Fatalf("requests=%d", len(m.requests))
	}
	for _, r := range m.requests {
		if r.MaxTokens <= 0 || r.MaxTokens > 2048 || len(r.Tools) != 1 {
			t.Fatalf("internal output=%d", r.MaxTokens)
		}
	}
}
