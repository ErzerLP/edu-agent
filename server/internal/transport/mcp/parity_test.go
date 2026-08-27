package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/transport/httpapi"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type parityReadiness struct{}

func (parityReadiness) Ready(context.Context) health.Report {
	return health.Report{Status: health.StatusHealthy}
}

func TestHTTPAndMCPShareLearningConflictCodeDetailAndServiceInstance(t *testing.T) {
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	identityService := &testIdentity{credential: identity.Credential{
		Device: identity.Device{ID: testDeviceID}, Scopes: []string{"learning:read", "learning:write"},
	}}
	learningService := &testLearning{err: &learning.Error{
		Code: learning.CodeVersionConflict, AggregateType: "goal",
		AggregateID:     "31000000-0000-4000-8000-000000000001",
		ExpectedVersion: 1, CurrentVersion: 2, AsOfEventSequence: 9,
	}}
	knowledgeService := &testKnowledge{}
	memoryService := &testMemory{}
	permits := privacy.NewReadPermitManager()
	authLimiter := httpapi.NewFixedWindowLimiter(100, time.Minute)
	deviceLimiter := httpapi.NewFixedWindowLimiter(100, time.Minute)
	mcpHandler, err := New(Options{
		Identity: identityService, Knowledge: knowledgeService, Learning: learningService,
		Memory: memoryService, MemoryExporter: memoryService, ReadPermits: permits,
		Logger: logger, AuthLimiter: authLimiter, DeviceLimiter: deviceLimiter,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.New(httpapi.Options{
		Identity: identityService, Learning: learningService, ReadPermits: permits,
		Readiness: parityReadiness{}, MCP: mcpHandler, Logger: logger,
		PairLimiter: httpapi.NewFixedWindowLimiter(100, time.Minute),
		AuthLimiter: authLimiter, DeviceLimiter: deviceLimiter,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	arguments := map[string]any{
		"operation_id": "32000000-0000-4000-8000-000000000001", "payload_schema_version": 1,
		"aggregate_type": "goal", "aggregate_id": "31000000-0000-4000-8000-000000000001",
		"expected_version": 1, "text": "Conflict", "source": "parity-test",
	}
	session := connectSDKClient(t, server.URL+"/mcp", testToken)
	mcpResult, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "learning.create_goal", Arguments: arguments})
	if err != nil || !mcpResult.IsError {
		t.Fatalf("MCP conflict result=%+v err=%v", mcpResult, err)
	}
	mcpEnvelope, ok := mcpResult.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("MCP structured error=%T", mcpResult.StructuredContent)
	}

	body, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/learning/goals", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var httpEnvelope map[string]any
	if err := json.NewDecoder(response.Body).Decode(&httpEnvelope); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("HTTP conflict status=%d body=%v", response.StatusCode, httpEnvelope)
	}

	assertParityError(t, httpEnvelope, mcpEnvelope)
	learningService.mu.Lock()
	calls := learningService.calls
	learningService.mu.Unlock()
	if calls != 2 {
		t.Fatalf("shared learning service calls=%d, want one HTTP and one MCP invocation", calls)
	}
}

func assertParityError(t *testing.T, httpEnvelope, mcpEnvelope map[string]any) {
	t.Helper()
	httpError := httpEnvelope["error"].(map[string]any)
	mcpError := mcpEnvelope["error"].(map[string]any)
	if httpError["code"] != learning.CodeVersionConflict || mcpError["code"] != httpError["code"] || mcpError["message"] != httpError["message"] {
		t.Fatalf("error parity HTTP=%v MCP=%v", httpError, mcpError)
	}
	httpConflict := httpEnvelope["conflict"].(map[string]any)
	mcpConflict := mcpEnvelope["conflict"].(map[string]any)
	for _, field := range []string{"aggregate_type", "aggregate_id", "expected_version", "current_version", "as_of_event_seq"} {
		if mcpConflict[field] != httpConflict[field] {
			t.Fatalf("conflict %s parity HTTP=%v MCP=%v", field, httpConflict[field], mcpConflict[field])
		}
	}
}
