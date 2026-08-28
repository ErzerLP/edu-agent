package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/transport/httpapi"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	crossTransportDeviceID = "99000000-0000-4000-8000-000000000001"
	crossTransportToken    = "cross-transport-token"
)

type crossTransportIdentity struct{}

func (crossTransportIdentity) Authenticate(_ context.Context, token, _ string) (identity.Credential, error) {
	if token != crossTransportToken {
		return identity.Credential{}, identity.ErrUnauthenticated
	}
	return identity.Credential{
		Device: identity.Device{ID: crossTransportDeviceID},
		Scopes: []string{"knowledge:read", "knowledge:write", "knowledge:approve", "learning:read", "learning:write", "memory:read"},
	}, nil
}
func (crossTransportIdentity) ExchangePairingCode(context.Context, string, string) (identity.IssuedCredential, error) {
	return identity.IssuedCredential{}, errors.New("not implemented")
}
func (crossTransportIdentity) ListDevices(context.Context) ([]identity.Device, error) {
	return nil, nil
}
func (crossTransportIdentity) RevokeDevice(context.Context, string) error { return nil }

type crossTransportReadiness struct{}

func (crossTransportReadiness) Ready(context.Context) health.Report {
	return health.Report{Status: health.StatusHealthy, Components: map[string]health.Component{}}
}

type crossTransportBearer struct{ token string }

func (t crossTransportBearer) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(clone)
}

func TestPostgreSQLComposeMemoryBridgeBindsPersistentResponseCommitGate(t *testing.T) {
	pool := appIntegrationPool(t)
	stores := newApplicationStores(pool)
	bridge, err := composeMemoryBridge(pool, stores, bridgeTestConfig(t, false), memoryBridgeDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	permit, err := bridge.readPermits.Acquire(ctx, privacy.OwnerKnowledge)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		permit.Release()
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	const closeDeviceID = "99000000-0000-4000-8000-000000000010"
	const closeErasureID = "99000000-0000-4000-8000-000000000011"
	const closeOperationID = "99000000-0000-4000-8000-000000000012"
	if _, err := tx.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'response gate close',clock_timestamp())`, closeDeviceID); err != nil {
		permit.Release()
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO privacy_erasures(
			id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,
			target_learner_generation,managed_backup_scheduled_unrecoverable_after
		) VALUES($1,$2,$3,decode(repeat('ab',32),'hex'),'learner_request',$2,clock_timestamp(),2,clock_timestamp()+interval '1 hour')`, closeErasureID, closeDeviceID, closeOperationID); err != nil {
		permit.Release()
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('privacy-owner:'||'knowledge',0))`); err != nil {
		permit.Release()
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET learner_generation=learner_generation+1,
			read_open=FALSE,
			write_open=FALSE,
			active_erasure_id=$1,
			updated_at=clock_timestamp()
		WHERE owner_kind='knowledge'`, closeErasureID); err != nil {
		permit.Release()
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		permit.Release()
		t.Fatal(err)
	}
	wrote := false
	err = permit.CommitResponse(func() { wrote = true })
	permit.Release()
	if privacy.ErrorCode(err) != privacy.CodeContentRedacted || wrote {
		t.Fatalf("composed response gate did not honor remote close: err=%v wrote=%v", err, wrote)
	}
}

func TestPostgreSQLHTTPAndMCPShareKnowledgeLearningAndTutoringState(t *testing.T) {
	pool := appIntegrationPool(t)
	if _, err := pool.Exec(context.Background(), `INSERT INTO devices(id,display_name,created_at) VALUES($1,$2,now())`, crossTransportDeviceID, "cross-transport-device"); err != nil {
		t.Fatal(err)
	}
	stores := newApplicationStores(pool)
	knowledgeService, err := knowledge.NewService(stores.knowledge, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := bridgeTestConfig(t, false)
	cfg.Model = config.ModelConfig{Name: "cross-transport-test", ContextWindow: 8192}
	learningComposition, err := composeLearningWithStores(stores.learning, stores.tutoring, knowledgeService, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := composeMemoryBridge(pool, stores, cfg, memoryBridgeDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	authLimiter := httpapi.NewFixedWindowLimiter(1000, time.Minute)
	deviceLimiter := httpapi.NewFixedWindowLimiter(1000, time.Minute)
	handler, err := composeTransportHandler(httpapi.Options{
		Identity: crossTransportIdentity{}, Knowledge: knowledgeService, Learning: learningComposition.service,
		Memory: bridge.memoryService, MemoryExporter: bridge.memoryExporter, ReadPermits: bridge.readPermits,
		Readiness: crossTransportReadiness{}, Logger: logger,
		PairLimiter: httpapi.NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: authLimiter, DeviceLimiter: deviceLimiter,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	var imported knowledge.ImportResult
	postJSON(t, server.URL+"/v1/knowledge/imports", map[string]any{
		"operation_id": "91000000-0000-4000-8000-000000000001", "expected_parent_revision_id": nil,
		"source": "cross-transport-test", "documents": []map[string]any{{
			"path": "concurrency.md", "markdown": "# Concurrency\nChannels coordinate goroutines without creating a second source of truth.\n",
		}},
	}, http.StatusCreated, &imported)
	if imported.Revision.ID == "" {
		t.Fatal("HTTP knowledge import did not create a revision")
	}

	session := connectCrossTransportSDK(t, server.URL+"/mcp")
	head, err := session.ReadResource(context.Background(), &sdkmcp.ReadResourceParams{URI: "edu-agent://knowledge/head"})
	if err != nil || len(head.Contents) != 1 || !bytes.Contains([]byte(head.Contents[0].Text), []byte(imported.Revision.ID)) {
		t.Fatalf("MCP knowledge head result=%+v err=%v", head, err)
	}
	tree, err := session.ReadResource(context.Background(), &sdkmcp.ReadResourceParams{URI: "edu-agent://knowledge/revisions/" + imported.Revision.ID + "/tree"})
	if err != nil || len(tree.Contents) != 1 || !bytes.Contains([]byte(tree.Contents[0].Text), []byte("Concurrency")) {
		t.Fatalf("MCP knowledge tree result=%+v err=%v", tree, err)
	}

	var httpGoal learning.OperationResult
	postJSON(t, server.URL+"/v1/learning/goals", map[string]any{
		"operation_id": "92000000-0000-4000-8000-000000000001", "payload_schema_version": 1,
		"aggregate_type": "goal", "aggregate_id": "93000000-0000-4000-8000-000000000001",
		"expected_version": 0, "text": "Learn channel ownership", "source": "http-cross-transport",
	}, http.StatusCreated, &httpGoal)
	projection, err := session.ReadResource(context.Background(), &sdkmcp.ReadResourceParams{URI: "edu-agent://learning/projections/status"})
	if err != nil || len(projection.Contents) != 1 {
		t.Fatalf("MCP projection status result=%+v err=%v", projection, err)
	}
	var projectionStatus learning.ProjectionStatus
	if err := json.Unmarshal([]byte(projection.Contents[0].Text), &projectionStatus); err != nil {
		t.Fatal(err)
	}
	if projectionStatus.HighWater < httpGoal.LastEventSequence || projectionStatus.Metadata.AsOfEventSequence < httpGoal.LastEventSequence {
		t.Fatalf("MCP projection did not observe HTTP write: goal=%+v projection=%+v", httpGoal, projectionStatus)
	}

	mcpGoal := callOperationTool(t, session, "learning.create_goal", map[string]any{
		"operation_id": "92000000-0000-4000-8000-000000000002", "payload_schema_version": 1,
		"aggregate_type": "goal", "aggregate_id": "93000000-0000-4000-8000-000000000002",
		"expected_version": 0, "text": "Practice cancellation", "source": "mcp-cross-transport",
	})
	goalRevisionID := nestedString(t, mcpGoal, "result", "goal_revision_id")
	mcpSession := callOperationTool(t, session, "tutoring.create_session", map[string]any{
		"operation_id": "92000000-0000-4000-8000-000000000003", "payload_schema_version": 1,
		"aggregate_type": "session", "aggregate_id": "94000000-0000-4000-8000-000000000001",
		"expected_version": 0, "goal_revision_id": goalRevisionID,
	})
	mcpAction := callOperationTool(t, session, "tutoring.apply_action", map[string]any{
		"session_id":   "94000000-0000-4000-8000-000000000001",
		"operation_id": "92000000-0000-4000-8000-000000000004", "payload_schema_version": 1,
		"aggregate_type": "session", "aggregate_id": "94000000-0000-4000-8000-000000000001",
		"expected_version": int64(mcpSession["aggregate_version"].(float64)), "action": "start_diagnostic",
	})

	var httpSession learning.SessionView
	getJSON(t, server.URL+"/v1/tutoring/sessions/94000000-0000-4000-8000-000000000001", http.StatusOK, &httpSession)
	if httpSession.Session.ID != "94000000-0000-4000-8000-000000000001" || httpSession.Session.State != tutoring.StateDiagnostic {
		t.Fatalf("HTTP session did not observe MCP action: %+v", httpSession.Session)
	}
	if httpSession.Metadata.AsOfEventSequence < int64(mcpAction["last_event_seq"].(float64)) || httpSession.Session.AggregateVer != int64(mcpAction["aggregate_version"].(float64)) {
		t.Fatalf("HTTP projection/event sequence differs from MCP result: action=%v session=%+v", mcpAction, httpSession)
	}
}

func connectCrossTransportSDK(t *testing.T, endpoint string) *sdkmcp.ClientSession {
	t.Helper()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "app-cross-transport-test", Version: "1"}, &sdkmcp.ClientOptions{Capabilities: &sdkmcp.ClientCapabilities{}})
	session, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: crossTransportBearer{token: crossTransportToken}},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callOperationTool(t *testing.T, session *sdkmcp.ClientSession, name string, arguments map[string]any) map[string]any {
	t.Helper()
	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil || result.IsError {
		t.Fatalf("%s result=%+v err=%v", name, result, err)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("%s structured result=%T", name, result.StructuredContent)
	}
	return structured
}

func nestedString(t *testing.T, value map[string]any, object, field string) string {
	t.Helper()
	nested, ok := value[object].(map[string]any)
	if !ok {
		t.Fatalf("%s is %T in %v", object, value[object], value)
	}
	result, ok := nested[field].(string)
	if !ok || result == "" {
		t.Fatalf("%s.%s is %T in %v", object, field, nested[field], nested)
	}
	return result
}

func postJSON(t *testing.T, endpoint string, input any, status int, output any) {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+crossTransportToken)
	request.Header.Set("Content-Type", "application/json")
	doJSON(t, request, status, output)
}

func getJSON(t *testing.T, endpoint string, status int, output any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+crossTransportToken)
	doJSON(t, request, status, output)
}

func doJSON(t *testing.T, request *http.Request, status int, output any) {
	t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		var failure any
		_ = json.NewDecoder(response.Body).Decode(&failure)
		t.Fatalf("%s %s status=%d body=%v", request.Method, request.URL, response.StatusCode, failure)
	}
	if output != nil {
		if err := json.NewDecoder(response.Body).Decode(output); err != nil {
			t.Fatal(err)
		}
	}
}
