package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/transport/problem"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type callbackRuntime struct {
	knowledge      KnowledgeService
	learning       LearningService
	memory         MemoryService
	memoryExporter MemoryExporter
}

func (r callbackRuntime) callTool(ctx context.Context, request *sdkmcp.CallToolRequest, descriptor Descriptor) (*sdkmcp.CallToolResult, error) {
	invocation, ok := invocationFromContext(ctx)
	if !ok || invocation.Descriptor.Name != descriptor.Name || request.Params == nil || request.Params.Name != descriptor.Name {
		return r.toolFailure(ctx, descriptor, problem.Internal()), nil
	}
	var value any
	var err error
	switch descriptor.Name {
	case "knowledge.retrieve":
		var input knowledgeRetrieveInput
		if decodeArguments(request.Params.Arguments, &input) != nil || input.KnowledgeRevisionID != nil && !canonicalUUID(*input.KnowledgeRevisionID) {
			err = &knowledge.Error{Code: knowledge.CodeInvalidRequest}
			break
		}
		value, err = r.knowledge.Retrieve(ctx, knowledge.RetrievalCommand{
			Query: input.Query, KnowledgeRevisionID: input.KnowledgeRevisionID,
			QueryContextSchemaVersion: input.QueryContextSchemaVersion,
			Context:                   input.Context, Limits: input.Limits,
		})
	case "learning.list_timeline":
		var input timelineInput
		if decodeArguments(request.Params.Arguments, &input) != nil || input.SessionID != "" && !canonicalUUID(input.SessionID) {
			err = invalidLearningInput()
			break
		}
		var page learning.CursorPageRequest
		page, err = input.learningPage()
		if err == nil {
			value, err = r.learning.Timeline(ctx, learning.TimelineQuery{Page: page, SessionID: input.SessionID})
			if pageValue, ok := value.(learning.TimelinePage); ok {
				normalizeTimeline(&pageValue)
				value = pageValue
			}
		}
	case "learning.list_routes":
		var input routesInput
		if decodeArguments(request.Params.Arguments, &input) != nil {
			err = invalidLearningInput()
			break
		}
		var page learning.CursorPageRequest
		page, err = input.learningPage()
		if err == nil {
			page.CurrentOnly = input.CurrentOnly
			value, err = r.learning.Routes(ctx, page)
			if pageValue, ok := value.(learning.RoutesPage); ok {
				normalizeRoutes(&pageValue)
				value = pageValue
			}
		}
	case "learning.list_evidence":
		var input evidenceInput
		if decodeArguments(request.Params.Arguments, &input) != nil || input.NodeRevisionID != "" && !canonicalUUID(input.NodeRevisionID) {
			err = invalidLearningInput()
			break
		}
		var page learning.CursorPageRequest
		page, err = input.learningPage()
		if err == nil {
			value, err = r.learning.Evidence(ctx, learning.EvidenceQuery{Page: page, NodeRevisionID: input.NodeRevisionID})
			if pageValue, ok := value.(learning.EvidencePage); ok {
				normalizeEvidence(&pageValue)
				value = pageValue
			}
		}
	case "learning.list_reviews":
		var input reviewsInput
		if decodeArguments(request.Params.Arguments, &input) != nil {
			err = invalidLearningInput()
			break
		}
		var page learning.CursorPageRequest
		page, err = input.learningPage()
		if err == nil {
			value, err = r.learning.Reviews(ctx, learning.ReviewQuery{Page: page, DueBefore: input.DueBefore})
			if pageValue, ok := value.(learning.ReviewsPage); ok {
				normalizeReviews(&pageValue)
				value = pageValue
			}
		}
	case "memory.list_records":
		var input pageInput
		if decodeArguments(request.Params.Arguments, &input) != nil {
			err = &memory.Error{Code: memory.CodeInvalidRequest}
			break
		}
		var page memory.PageRequest
		page, err = input.memoryPage()
		if err == nil {
			value, err = r.memory.ListRecords(ctx, page)
			if pageValue, ok := value.(memory.RecordPage); ok {
				normalizeRecordPage(&pageValue)
				value = pageValue
			}
		}
	case "learning.create_goal":
		var input createGoalInput
		if decodeArguments(request.Params.Arguments, &input) != nil {
			err = invalidLearningInput()
			break
		}
		var command learning.GoalCommand
		command, err = input.command()
		if err == nil {
			value, err = r.learning.CreateGoal(ctx, invocation.Credential.Device.ID, command)
		}
	case "tutoring.create_session":
		var input createSessionInput
		if decodeArguments(request.Params.Arguments, &input) != nil {
			err = invalidLearningInput()
			break
		}
		var command learning.SessionCommand
		command, err = input.command()
		if err == nil {
			value, err = r.learning.CreateSession(ctx, invocation.Credential.Device.ID, command)
		}
	case "tutoring.propose":
		var input proposeInput
		if decodeArguments(request.Params.Arguments, &input) != nil {
			err = invalidLearningInput()
			break
		}
		var proposal learning.ProposalRequest
		proposal, err = input.request()
		if err == nil {
			value, err = r.learning.Propose(ctx, invocation.Credential.Device.ID, proposal)
		}
	case "tutoring.apply_action":
		var input applyActionInput
		if decodeArguments(request.Params.Arguments, &input) != nil {
			err = invalidLearningInput()
			break
		}
		var command learning.ActionCommand
		command, err = input.command()
		if err == nil {
			value, err = r.learning.ApplyAction(ctx, invocation.Credential.Device.ID, input.SessionID, command)
		}
	default:
		return r.toolFailure(ctx, descriptor, problem.DescriptorNotFound()), nil
	}
	if err != nil {
		return r.toolFailure(ctx, descriptor, mapApplicationProblem(err)), nil
	}
	return r.toolSuccess(ctx, descriptor, value), nil
}

func (r callbackRuntime) readResource(ctx context.Context, request *sdkmcp.ReadResourceRequest, descriptor Descriptor) (*sdkmcp.ReadResourceResult, error) {
	invocation, ok := invocationFromContext(ctx)
	if !ok || invocation.Descriptor.Name != descriptor.Name || request.Params == nil {
		return nil, resourceError(problem.Internal(), requestIDFromContext(ctx))
	}
	uri := request.Params.URI
	var value any
	var err error
	switch descriptor.Name {
	case "knowledge.head":
		var revision *knowledge.KnowledgeRevision
		revision, err = r.knowledge.Head(ctx)
		if err == nil && revision == nil {
			err = &knowledge.Error{Code: knowledge.CodeNotFound}
		} else if err == nil {
			value = map[string]any{"revision": revision}
		}
	case "knowledge.revision_tree":
		values, matched := matchResourceTemplate(descriptor.URITemplate, uri)
		if !matched {
			err = &knowledge.Error{Code: knowledge.CodeInvalidRequest}
		} else {
			value, err = r.knowledge.Tree(ctx, values["revision_id"])
		}
	case "knowledge.revision_export":
		values, matched := matchResourceTemplate(descriptor.URITemplate, uri)
		if !matched {
			err = &knowledge.Error{Code: knowledge.CodeInvalidRequest}
		} else {
			value, err = r.knowledge.Export(ctx, values["revision_id"])
		}
	case "tutoring.current_session":
		value, err = r.learning.CurrentSession(ctx)
		if session, ok := value.(learning.SessionView); ok {
			normalizeSession(&session)
			value = session
		}
	case "tutoring.session":
		values, matched := matchResourceTemplate(descriptor.URITemplate, uri)
		if !matched {
			err = invalidLearningInput()
		} else {
			value, err = r.learning.Session(ctx, values["session_id"])
			if session, ok := value.(learning.SessionView); ok {
				normalizeSession(&session)
				value = session
			}
		}
	case "learning.node":
		values, matched := matchResourceTemplate(descriptor.URITemplate, uri)
		if !matched {
			err = invalidLearningInput()
		} else {
			value, err = r.learning.Node(ctx, values["node_revision_id"])
			if node, ok := value.(learning.NodeView); ok {
				normalizeNode(&node)
				value = node
			}
		}
	case "learning.projection_status":
		value, err = r.learning.ProjectionStatus(ctx)
		if status, ok := value.(learning.ProjectionStatus); ok {
			normalizeProjectionMetadata(&status.Metadata)
			value = status
		}
	case "memory.record":
		values, matched := matchResourceTemplate(descriptor.URITemplate, uri)
		if !matched {
			err = &memory.Error{Code: memory.CodeInvalidRequest}
		} else {
			value, err = r.memoryExporter.Detail(ctx, values["memory_id"])
		}
	case "memory.export":
		value, err = r.memoryExporter.Export(ctx, memory.PageRequest{Limit: 50})
		if page, ok := value.(memory.ExportPage); ok {
			normalizeExportPage(&page)
			value = page
		}
	default:
		err = errors.New("unknown MCP resource descriptor")
	}
	if err != nil {
		mapped := mapApplicationProblem(err)
		invocation.State.finish("error", mapped.Code)
		return nil, resourceError(mapped, invocation.RequestID)
	}
	encoded, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		mapped := problem.Internal()
		invocation.State.finish("error", mapped.Code)
		return nil, resourceError(mapped, invocation.RequestID)
	}
	if descriptor.OutputLimit > 0 && int64(len(encoded)) > descriptor.OutputLimit {
		mapped := problem.PayloadTooLarge("MCP descriptor output exceeds the configured limit")
		invocation.State.finish("error", mapped.Code)
		return nil, resourceError(mapped, invocation.RequestID)
	}
	invocation.State.finish("success", "")
	return &sdkmcp.ReadResourceResult{Contents: []*sdkmcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: string(encoded)}}}, nil
}

func (r callbackRuntime) toolSuccess(ctx context.Context, descriptor Descriptor, value any) *sdkmcp.CallToolResult {
	invocation, _ := invocationFromContext(ctx)
	encoded, err := json.Marshal(value)
	if err != nil {
		return r.toolFailure(ctx, descriptor, problem.Internal())
	}
	if descriptor.OutputLimit > 0 && int64(len(encoded)) > descriptor.OutputLimit {
		return r.toolFailure(ctx, descriptor, problem.PayloadTooLarge("MCP descriptor output exceeds the configured limit"))
	}
	var structured any
	if json.Unmarshal(encoded, &structured) != nil {
		return r.toolFailure(ctx, descriptor, problem.Internal())
	}
	invocation.State.finish("success", "")
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(encoded)}}, StructuredContent: structured,
	}
}

func (r callbackRuntime) toolFailure(ctx context.Context, _ Descriptor, mapped problem.Problem) *sdkmcp.CallToolResult {
	requestID := requestIDFromContext(ctx)
	if invocation, ok := invocationFromContext(ctx); ok {
		invocation.State.finish("error", mapped.Code)
		requestID = invocation.RequestID
	}
	envelope := mapped.Envelope(requestID)
	encoded, _ := json.Marshal(envelope)
	return &sdkmcp.CallToolResult{
		Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: string(encoded)}},
		StructuredContent: envelope, IsError: true,
	}
}

func requestIDFromContext(ctx context.Context) string {
	if invocation, ok := invocationFromContext(ctx); ok {
		return invocation.RequestID
	}
	return ""
}

func resourceError(mapped problem.Problem, requestID string) error {
	encoded, _ := json.Marshal(mapped.Envelope(requestID))
	var code int64 = jsonrpc.CodeInvalidParams
	if mapped.Status >= http.StatusInternalServerError {
		code = jsonrpc.CodeInternalError
	}
	return &jsonrpc.Error{Code: code, Message: mapped.Message, Data: json.RawMessage(encoded)}
}

func mapApplicationProblem(err error) problem.Problem {
	if knowledge.ErrorCode(err) != "" {
		return problem.Knowledge(err)
	}
	if learning.ErrorCode(err) != "" {
		return problem.Learning(err)
	}
	if memory.ErrorCode(err) != "" {
		return problem.Memory(err)
	}
	return problem.Internal()
}

func normalizeProjectionMetadata(metadata *learning.ProjectionMetadata) {
	if metadata.ReasonCodes == nil {
		metadata.ReasonCodes = []string{}
	}
}

func normalizeSession(value *learning.SessionView) { normalizeProjectionMetadata(&value.Metadata) }
func normalizeTimeline(value *learning.TimelinePage) {
	normalizeProjectionMetadata(&value.Metadata)
	if value.Items == nil {
		value.Items = []learning.TimelineItem{}
	}
}
func normalizeRoutes(value *learning.RoutesPage) {
	normalizeProjectionMetadata(&value.Metadata)
	if value.Items == nil {
		value.Items = []learning.RouteProjection{}
	}
}
func normalizeEvidence(value *learning.EvidencePage) {
	normalizeProjectionMetadata(&value.Metadata)
	if value.Items == nil {
		value.Items = []learning.AcceptedEvidence{}
	}
}
func normalizeReviews(value *learning.ReviewsPage) {
	normalizeProjectionMetadata(&value.Metadata)
	if value.Items == nil {
		value.Items = []learning.ReviewSchedule{}
	}
}
func normalizeNode(value *learning.NodeView) {
	normalizeProjectionMetadata(&value.Metadata)
	if value.Evidence == nil {
		value.Evidence = []learning.AcceptedEvidence{}
	}
	if value.Node.Misconceptions == nil {
		value.Node.Misconceptions = []learning.MisconceptionHypothesis{}
	}
	if value.Node.Mastery.Kinds == nil {
		value.Node.Mastery.Kinds = map[learning.EvidenceKind]int{}
	}
	if value.Node.Mastery.Outcomes == nil {
		value.Node.Mastery.Outcomes = map[learning.Outcome]int{}
	}
	if value.Node.Mastery.Help == nil {
		value.Node.Mastery.Help = map[learning.HelpLevel]int{}
	}
	if value.Node.Mastery.UncertaintyReasons == nil {
		value.Node.Mastery.UncertaintyReasons = []string{}
	}
}
func normalizeRecordPage(value *memory.RecordPage) {
	if value.Items == nil {
		value.Items = []memory.Record{}
	}
}
func normalizeExportPage(value *memory.ExportPage) {
	if value.Items == nil {
		value.Items = []memory.ExportItem{}
	}
	if value.ReasonCodes == nil {
		value.ReasonCodes = []string{}
	}
}
