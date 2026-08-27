package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/transport/httpapi"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type compositionKnowledge struct {
	head        *knowledge.KnowledgeRevision
	importActor string
}

func (s *compositionKnowledge) Head(context.Context) (*knowledge.KnowledgeRevision, error) {
	return s.head, nil
}
func (s *compositionKnowledge) Import(_ context.Context, command knowledge.ImportCommand) (knowledge.ImportResult, error) {
	s.importActor = command.ActorDeviceID
	s.head = &knowledge.KnowledgeRevision{ID: "81000000-0000-4000-8000-000000000001", RevisionNo: 1, Source: command.Source, CreatedByDeviceID: command.ActorDeviceID}
	return knowledge.ImportResult{Revision: *s.head}, nil
}
func (s *compositionKnowledge) Tree(context.Context, string) (knowledge.TreeResult, error) {
	return knowledge.TreeResult{Revision: *s.head}, nil
}
func (s *compositionKnowledge) Export(context.Context, string) (knowledge.ExportResult, error) {
	return knowledge.ExportResult{RevisionID: s.head.ID, Documents: []knowledge.ExportDocument{}}, nil
}
func (s *compositionKnowledge) Retrieve(context.Context, knowledge.RetrievalCommand) (knowledge.RetrievalResult, error) {
	return knowledge.RetrievalResult{KnowledgeRevisionID: s.head.ID, Hits: []knowledge.RetrievalHit{}}, nil
}

type compositionLearning struct {
	highWater int64
	actor     string
}

func (s *compositionLearning) operation(actor, aggregateType, aggregateID string) learning.OperationResult {
	s.actor = actor
	s.highWater++
	return learning.OperationResult{
		Status: "committed", AggregateType: aggregateType, AggregateID: aggregateID,
		AggregateVersion: 1, FirstEventSequence: s.highWater, LastEventSequence: s.highWater,
		ProjectionAsOf: s.highWater, Result: json.RawMessage(`{"goal_revision_id":"82000000-0000-4000-8000-000000000001"}`),
	}
}
func (s *compositionLearning) CreateGoal(_ context.Context, actor string, command learning.GoalCommand) (learning.OperationResult, error) {
	return s.operation(actor, "goal", command.Operation.AggregateID), nil
}
func (s *compositionLearning) CreateSession(_ context.Context, actor string, command learning.SessionCommand) (learning.OperationResult, error) {
	return s.operation(actor, "session", command.Operation.AggregateID), nil
}
func (s *compositionLearning) Propose(_ context.Context, actor string, _ learning.ProposalRequest) (learning.ProposalArtifact, error) {
	s.actor = actor
	return learning.ProposalArtifact{}, nil
}
func (s *compositionLearning) ApplyAction(_ context.Context, actor, sessionID string, _ learning.ActionCommand) (learning.OperationResult, error) {
	return s.operation(actor, "session", sessionID), nil
}
func (s *compositionLearning) Decide(context.Context, string, string, learning.AssessmentDecisionCommand) (learning.OperationResult, error) {
	return learning.OperationResult{}, nil
}
func (s *compositionLearning) CurrentSession(context.Context) (learning.SessionView, error) {
	return learning.SessionView{}, nil
}
func (s *compositionLearning) Session(context.Context, string) (learning.SessionView, error) {
	return learning.SessionView{}, nil
}
func (s *compositionLearning) Timeline(context.Context, learning.TimelineQuery) (learning.TimelinePage, error) {
	return learning.TimelinePage{Metadata: s.metadata(), Items: []learning.TimelineItem{}}, nil
}
func (s *compositionLearning) Routes(context.Context, learning.CursorPageRequest) (learning.RoutesPage, error) {
	return learning.RoutesPage{Metadata: s.metadata(), Items: []learning.RouteProjection{}}, nil
}
func (s *compositionLearning) Node(context.Context, string) (learning.NodeView, error) {
	return learning.NodeView{Metadata: s.metadata()}, nil
}
func (s *compositionLearning) Evidence(context.Context, learning.EvidenceQuery) (learning.EvidencePage, error) {
	return learning.EvidencePage{Metadata: s.metadata(), Items: []learning.AcceptedEvidence{}}, nil
}
func (s *compositionLearning) Reviews(context.Context, learning.ReviewQuery) (learning.ReviewsPage, error) {
	return learning.ReviewsPage{Metadata: s.metadata(), Items: []learning.ReviewSchedule{}}, nil
}
func (s *compositionLearning) ProjectionStatus(context.Context) (learning.ProjectionStatus, error) {
	return learning.ProjectionStatus{Metadata: s.metadata(), HighWater: s.highWater}, nil
}
func (s *compositionLearning) metadata() learning.ProjectionMetadata {
	return learning.ProjectionMetadata{AsOfEventSequence: s.highWater, ReasonCodes: []string{}}
}

type compositionMemory struct{}

func (compositionMemory) CreateCandidate(context.Context, memory.DevicePrincipal, memory.CreateCandidateCommand) (memory.OperationResult, error) {
	return memory.OperationResult{}, nil
}
func (compositionMemory) CreateCorrectionCandidate(context.Context, memory.DevicePrincipal, memory.CreateCorrectionCandidateCommand) (memory.OperationResult, error) {
	return memory.OperationResult{}, nil
}
func (compositionMemory) DecideCandidate(context.Context, memory.DevicePrincipal, memory.DecideCandidateCommand) (memory.OperationResult, error) {
	return memory.OperationResult{}, nil
}
func (compositionMemory) DeleteRecord(context.Context, memory.DevicePrincipal, memory.DeleteRecordCommand) (memory.OperationResult, error) {
	return memory.OperationResult{}, nil
}
func (compositionMemory) ReplayDelivery(context.Context, memory.DevicePrincipal, memory.ReplayDeliveryCommand) (memory.OperationResult, error) {
	return memory.OperationResult{}, nil
}
func (compositionMemory) Candidate(context.Context, string) (memory.CandidateView, error) {
	return memory.CandidateView{}, nil
}
func (compositionMemory) ListCandidates(context.Context, memory.PageRequest) (memory.CandidatePage, error) {
	return memory.CandidatePage{}, nil
}
func (compositionMemory) ListRecords(context.Context, memory.PageRequest) (memory.RecordPage, error) {
	return memory.RecordPage{Items: []memory.Record{}}, nil
}
func (compositionMemory) Record(context.Context, string) (memory.RecordView, error) {
	return memory.RecordView{}, nil
}
func (compositionMemory) Detail(context.Context, string) (memory.RecordDetail, error) {
	return memory.RecordDetail{}, nil
}
func (compositionMemory) Export(context.Context, memory.PageRequest) (memory.ExportPage, error) {
	return memory.ExportPage{Items: []memory.ExportItem{}, ReasonCodes: []string{}}, nil
}

func TestComposeTransportHandlerSharesStateAcrossHTTPAndMCP(t *testing.T) {
	knowledgeService := &compositionKnowledge{}
	learningService := &compositionLearning{}
	memoryService := compositionMemory{}
	permits := privacy.NewReadPermitManager()
	authLimiter := httpapi.NewFixedWindowLimiter(100, time.Minute)
	deviceLimiter := httpapi.NewFixedWindowLimiter(100, time.Minute)
	handler, err := composeTransportHandler(httpapi.Options{
		Identity: crossTransportIdentity{}, Knowledge: knowledgeService, Learning: learningService,
		Memory: memoryService, MemoryExporter: memoryService, ReadPermits: permits,
		Readiness: crossTransportReadiness{}, Logger: slog.Default(),
		PairLimiter: httpapi.NewFixedWindowLimiter(100, time.Minute), AuthLimiter: authLimiter, DeviceLimiter: deviceLimiter,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	var imported knowledge.ImportResult
	postJSON(t, server.URL+"/v1/knowledge/imports", map[string]any{
		"operation_id": "83000000-0000-4000-8000-000000000001", "expected_parent_revision_id": nil,
		"source": "composition", "documents": []map[string]any{{"path": "shared.md", "markdown": "# Shared\nstate\n"}},
	}, http.StatusCreated, &imported)
	if knowledgeService.importActor != crossTransportDeviceID {
		t.Fatalf("HTTP import actor=%q", knowledgeService.importActor)
	}
	session := connectCrossTransportSDK(t, server.URL+"/mcp")
	resource, err := session.ReadResource(context.Background(), &sdkmcp.ReadResourceParams{URI: "edu-agent://knowledge/head"})
	if err != nil || len(resource.Contents) != 1 || !strings.Contains(resource.Contents[0].Text, imported.Revision.ID) {
		t.Fatalf("MCP did not read HTTP-updated knowledge: result=%+v err=%v", resource, err)
	}

	var httpGoal learning.OperationResult
	postJSON(t, server.URL+"/v1/learning/goals", map[string]any{
		"operation_id": "83000000-0000-4000-8000-000000000002", "payload_schema_version": 1,
		"aggregate_type": "goal", "aggregate_id": "84000000-0000-4000-8000-000000000001",
		"expected_version": 0, "text": "HTTP goal", "source": "composition",
	}, http.StatusCreated, &httpGoal)
	projection, err := session.ReadResource(context.Background(), &sdkmcp.ReadResourceParams{URI: "edu-agent://learning/projections/status"})
	if err != nil || !strings.Contains(projection.Contents[0].Text, `"committed_event_high_water":1`) {
		t.Fatalf("MCP did not read HTTP-updated projection: result=%+v err=%v", projection, err)
	}

	callOperationTool(t, session, "learning.create_goal", map[string]any{
		"operation_id": "83000000-0000-4000-8000-000000000003", "payload_schema_version": 1,
		"aggregate_type": "goal", "aggregate_id": "84000000-0000-4000-8000-000000000002",
		"expected_version": 0, "text": "MCP goal", "source": "composition",
	})
	var httpProjection learning.ProjectionStatus
	getJSON(t, server.URL+"/v1/learning/projections/status", http.StatusOK, &httpProjection)
	if httpProjection.HighWater != 2 || httpProjection.Metadata.AsOfEventSequence != 2 || learningService.actor != crossTransportDeviceID {
		t.Fatalf("HTTP did not read MCP-updated projection: projection=%+v actor=%q", httpProjection, learningService.actor)
	}
}
