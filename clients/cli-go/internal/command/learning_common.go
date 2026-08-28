package command

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
)

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
	maxProgressPages = 10
	maxProgressNodes = 200
)

type onlineSession struct {
	client APIClient
	config config.Config
}

func (a *App) openOnline(flags onlineFlags) (onlineSession, error) {
	bound, timeout, err := a.loadBinding(flags.overrides())
	if err != nil {
		return onlineSession{}, err
	}
	a.printInsecureWarning(bound.Config)
	return onlineSession{client: a.NewClient(bound.Config.ServerURL, bound.Token, timeout), config: bound.Config}, nil
}

func (a *App) operationID() (string, error) {
	value, err := a.NewUUID()
	if err != nil {
		return "", commandError("uuid_generation_failed", "a secure operation ID could not be generated", "inspect the operating system random source", ExitInternal)
	}
	return value, nil
}

func (a *App) currentSession(ctx context.Context, client APIClient) (api.SessionView, bool, error) {
	view, err := client.CurrentSession(ctx)
	if err == nil {
		return view, true, nil
	}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "not_found" {
		return api.SessionView{}, false, nil
	}
	return api.SessionView{}, false, mapAPIError(err)
}

func refetchSession(ctx context.Context, client APIClient, sessionID string) (api.SessionView, error) {
	view, err := client.CurrentSession(ctx)
	if err == nil && view.Session.SessionID == sessionID {
		return view, nil
	}
	if err != nil {
		var apiErr *api.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "not_found" {
			return api.SessionView{}, err
		}
	}
	view, err = client.Session(ctx, sessionID)
	if err != nil {
		return api.SessionView{}, err
	}
	if view.Session.SessionID != sessionID {
		return api.SessionView{}, &api.ProtocolError{Category: "session_refetch_mismatch"}
	}
	return view, nil
}

func sessionOperation(view api.SessionView, operationID string) api.SessionOperation {
	return api.SessionOperation{
		OperationID: operationID, PayloadSchemaVersion: 1, AggregateType: "session",
		AggregateID: view.Session.SessionID, ExpectedVersion: view.Session.AggregateVersion,
	}
}

func allowed(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (a *App) applyAndRefetch(ctx context.Context, client APIClient, view api.SessionView, request api.TutoringAction) (api.SessionView, bool, error) {
	_, err := client.ApplySessionAction(ctx, view.Session.SessionID, request)
	if err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "version_conflict" {
			fresh, fetchErr := refetchSession(ctx, client, view.Session.SessionID)
			if fetchErr != nil {
				return api.SessionView{}, true, mapAPIError(fetchErr)
			}
			_, _ = fmt.Fprintln(a.Err, "warning[version_conflict]: authoritative session refreshed; the previous input was not replayed")
			return fresh, true, nil
		}
		return api.SessionView{}, false, mapAPIError(err)
	}
	fresh, err := refetchSession(ctx, client, view.Session.SessionID)
	if err != nil {
		return api.SessionView{}, false, mapAPIError(err)
	}
	return fresh, false, nil
}

func (a *App) noFieldAction(ctx context.Context, client APIClient, view api.SessionView, action string) (api.SessionView, bool, error) {
	if view.WorkItem == nil || !allowed(view.WorkItem.AllowedActions, action) {
		return view, false, commandError("invalid_state", "the action is not allowed by the current work item", "refresh the session and use a displayed allowed action", ExitConflict)
	}
	operationID, err := a.operationID()
	if err != nil {
		return view, false, err
	}
	return a.applyAndRefetch(ctx, client, view, api.ActionNoFieldsRequest{SessionOperation: sessionOperation(view, operationID), Action: action})
}

func (a *App) proposalAction(ctx context.Context, client APIClient, view api.SessionView, action, proposalID string) (api.SessionView, bool, error) {
	if view.WorkItem == nil || !allowed(view.WorkItem.AllowedActions, action) {
		return view, false, commandError("invalid_state", "the proposal action is not allowed by the current work item", "refresh the session and use a displayed allowed action", ExitConflict)
	}
	operationID, err := a.operationID()
	if err != nil {
		return view, false, err
	}
	return a.applyAndRefetch(ctx, client, view, api.ActionProposalRequest{SessionOperation: sessionOperation(view, operationID), Action: action, ProposalID: proposalID})
}

func (a *App) createProposalAndRefetch(ctx context.Context, client APIClient, view api.SessionView, request api.TutoringProposalRequest) (api.TutoringProposal, api.SessionView, bool, error) {
	proposal, err := client.CreateProposal(ctx, request)
	if err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "stale_proposal" {
			fresh, fetchErr := client.CurrentSession(ctx)
			if fetchErr != nil {
				return api.TutoringProposal{}, api.SessionView{}, false, mapAPIError(fetchErr)
			}
			_, _ = fmt.Fprintln(a.Err, "warning[stale_proposal]: authoritative session refreshed; request a new proposal from the new work item")
			return api.TutoringProposal{}, fresh, true, nil
		}
		return api.TutoringProposal{}, view, false, mapAPIError(err)
	}
	fresh, err := client.CurrentSession(ctx)
	if err != nil {
		return api.TutoringProposal{}, api.SessionView{}, false, mapAPIError(err)
	}
	if fresh.Session.SessionID != view.Session.SessionID || fresh.Session.AggregateVersion != view.Session.AggregateVersion || fresh.Session.State != view.Session.State {
		_, _ = fmt.Fprintln(a.Err, "warning[stale_proposal]: session changed while the proposal was frozen; the proposal was discarded")
		return api.TutoringProposal{}, fresh, true, nil
	}
	return proposal, fresh, false, nil
}

func (a *App) retrieveForWorkItem(ctx context.Context, client APIClient, view api.SessionView, query, knowledgeRevisionID string) (api.KnowledgeRetrievalResult, error) {
	if strings.TrimSpace(query) == "" || knowledgeRevisionID == "" {
		return api.KnowledgeRetrievalResult{}, commandError("invalid_state", "retrieval requires an authoritative query and knowledge revision", "refresh the work item", ExitConflict)
	}
	contextValue := map[string]any{
		"session_id":        view.Session.SessionID,
		"aggregate_version": view.Session.AggregateVersion,
		"tutoring_state":    view.Session.State,
	}
	if view.WorkItem != nil && view.WorkItem.GoalRevision != nil {
		contextValue["goal_revision_id"] = view.WorkItem.GoalRevision.GoalRevisionID
	}
	result, err := client.RetrieveKnowledge(ctx, api.KnowledgeRetrievalRequest{
		Query: query, KnowledgeRevisionID: knowledgeRevisionID,
		QueryContextSchemaVersion: "query-context-v1", Context: contextValue,
		Limits: &api.KnowledgeQueryLimits{MaxDepth: 4, CandidatesPerLayer: 12, MaxHits: 10, TotalCandidates: 100},
	})
	if err != nil {
		return api.KnowledgeRetrievalResult{}, mapAPIError(err)
	}
	if len(result.Hits) == 0 {
		return api.KnowledgeRetrievalResult{}, commandError("knowledge_not_found", "retrieval returned no canonical knowledge hit", "import relevant Markdown or refine the goal/question", ExitConflict)
	}
	if result.Degraded || result.Truncated {
		reasons := retrievalReasons(result)
		_, _ = fmt.Fprintf(a.Err, "warning[retrieval_degraded]: degraded=%t truncated=%t reasons=%s\n", result.Degraded, result.Truncated, safeText(strings.Join(reasons, ",")))
		confirmed, confirmErr := a.Terminal.Confirm("Continue with the displayed retrieval limits?")
		if confirmErr != nil || !confirmed {
			return api.KnowledgeRetrievalResult{}, commandError("retrieval_declined", "the degraded or truncated retrieval was not accepted", "retry later or refine the input", ExitInput)
		}
	}
	return result, nil
}

func retrievalReasons(result api.KnowledgeRetrievalResult) []string {
	seen := map[string]bool{}
	var reasons []string
	for _, trace := range result.Trace {
		if trace.ReasonCode != "" && !seen[trace.ReasonCode] {
			seen[trace.ReasonCode] = true
			reasons = append(reasons, trace.ReasonCode)
		}
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "bounded_retrieval")
	}
	sort.Strings(reasons)
	return reasons
}

func proposalContextFromRetrieval(item api.SessionWorkItem, result api.KnowledgeRetrievalResult) map[string]any {
	hits := make([]api.ProposalContextReference, 0, len(result.Hits))
	for _, hit := range result.Hits {
		hits = append(hits, api.ProposalContextReference{
			KnowledgeRevisionID: result.KnowledgeRevisionID,
			DocumentRevisionID:  hit.DocumentRevisionID,
			NodeID:              hit.NodeID,
			NodeRevisionID:      hit.NodeRevisionID,
			Range:               api.LearningSourceRange{Start: hit.SectionRange.Start, End: hit.SectionRange.End},
			Slice:               hit.CanonicalSlice,
			SliceSHA256:         hit.SliceSHA256,
		})
	}
	return map[string]any{
		"schema_version": api.ProposalContextSchemaVersion,
		"work_item":      item,
		"retrieval": api.ProposalContextRetrieval{
			KnowledgeRevisionID: result.KnowledgeRevisionID,
			Hits:                hits,
		},
	}
}

func proposalContextFromReferences(item api.SessionWorkItem, knowledgeRevisionID string, references []api.KnowledgeReference) map[string]any {
	hits := make([]api.ProposalContextReference, 0, len(references))
	for _, reference := range references {
		hits = append(hits, api.ProposalContextReference{
			KnowledgeRevisionID: reference.KnowledgeRevisionID,
			DocumentRevisionID:  reference.DocumentRevisionID,
			NodeID:              reference.NodeID,
			NodeRevisionID:      reference.NodeRevisionID,
			Range:               reference.Range,
			Slice:               reference.Slice,
			SliceSHA256:         reference.SliceSHA256,
		})
	}
	return map[string]any{
		"schema_version": api.ProposalContextSchemaVersion,
		"work_item":      item,
		"retrieval": api.ProposalContextRetrieval{
			KnowledgeRevisionID: knowledgeRevisionID,
			Hits:                hits,
		},
	}
}

func proposalRequest(view api.SessionView, proposalType string, retrieval api.KnowledgeRetrievalResult, requestID string) (api.TutoringProposalRequest, error) {
	if view.WorkItem == nil {
		return api.TutoringProposalRequest{}, commandError("invalid_state", "the proposal requires a current work item", "refresh the session", ExitConflict)
	}
	item := view.WorkItem
	request := api.TutoringProposalRequest{
		RequestID: requestID, ProposalType: proposalType, AggregateType: "session",
		AggregateID: view.Session.SessionID, AggregateVersion: view.Session.AggregateVersion,
		RouteRevisionID: view.Session.Focus.RouteRevisionID,
		TutoringState:   view.Session.State, KnowledgeRevisionID: retrieval.KnowledgeRevisionID,
		Input: proposalContextFromRetrieval(*item, retrieval),
	}
	if item.GoalRevision != nil {
		request.GoalRevisionID = item.GoalRevision.GoalRevisionID
	}
	if item.RouteRevision != nil {
		request.RouteRevisionID = item.RouteRevision.RouteRevisionID
	}
	request.RouteStepID = view.Session.Focus.RouteStepID
	request.FocusNodeRevisionID = view.Session.Focus.FocusNodeRevisionID
	if item.Activity != nil {
		request.ActivityID = item.Activity.ActivityID
	}
	if item.Attempt != nil {
		request.AttemptID = item.Attempt.AttemptID
	}
	if item.FreeQuestion != nil {
		request.FreeQuestionID = item.FreeQuestion.FreeQuestionID
		request.FocusFrameID = item.FreeQuestion.FocusFrameID
	}
	if item.FreeAnswer != nil {
		request.FreeAnswerID = item.FreeAnswer.FreeAnswerID
	}
	seen := map[string]bool{}
	for _, hit := range retrieval.Hits {
		if !seen[hit.NodeRevisionID] {
			seen[hit.NodeRevisionID] = true
			request.NodeRevisionIDs = append(request.NodeRevisionIDs, hit.NodeRevisionID)
		}
	}
	if len(request.NodeRevisionIDs) == 0 {
		return api.TutoringProposalRequest{}, commandError("knowledge_not_found", "proposal context has no authoritative node", "refresh retrieval", ExitConflict)
	}
	return request, nil
}

func assessmentProposalRequest(view api.SessionView, requestID string) (api.TutoringProposalRequest, error) {
	if view.WorkItem == nil || view.WorkItem.Activity == nil || view.WorkItem.Attempt == nil {
		return api.TutoringProposalRequest{}, commandError("invalid_state", "assessment proposal requires the current activity and attempt", "refresh the session", ExitConflict)
	}
	item, activity := view.WorkItem, view.WorkItem.Activity
	nodeIDs := make([]string, 0, len(activity.KnowledgeReferences))
	seen := map[string]bool{}
	for _, reference := range activity.KnowledgeReferences {
		if !seen[reference.NodeRevisionID] {
			seen[reference.NodeRevisionID] = true
			nodeIDs = append(nodeIDs, reference.NodeRevisionID)
		}
	}
	request := api.TutoringProposalRequest{
		RequestID: requestID, ProposalType: "assessment", AggregateType: "session",
		AggregateID: view.Session.SessionID, AggregateVersion: view.Session.AggregateVersion,
		GoalRevisionID: activity.GoalRevisionID, RouteRevisionID: activity.RouteRevisionID,
		RouteStepID: activity.RouteStepID, FocusNodeRevisionID: activity.TargetNodeRevisionID,
		ActivityID: activity.ActivityID, AttemptID: item.Attempt.AttemptID,
		TutoringState: view.Session.State, KnowledgeRevisionID: activity.KnowledgeRevisionID,
		NodeRevisionIDs: nodeIDs, Input: proposalContextFromReferences(*item, activity.KnowledgeRevisionID, activity.KnowledgeReferences),
	}
	if item.FreeQuestion != nil {
		request.FreeQuestionID = item.FreeQuestion.FreeQuestionID
		request.FocusFrameID = item.FreeQuestion.FocusFrameID
	}
	if item.FreeAnswer != nil {
		request.FreeAnswerID = item.FreeAnswer.FreeAnswerID
	}
	return request, nil
}

func printProjectionWarning(out interface{ Write([]byte) (int, error) }, metadata api.ProjectionMetadata) {
	if !metadata.Rebuilding && !metadata.Degraded && !metadata.Incomplete && len(metadata.ReasonCodes) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "warning[projection_state]: as_of=%d projection=%s generation=%s knowledge_revision=%s rebuilding=%t degraded=%t incomplete=%t reasons=%s\n",
		metadata.AsOfEventSeq, safeText(metadata.ProjectionVersion), safeText(metadata.Generation), safeText(metadata.KnowledgeRevisionID), metadata.Rebuilding, metadata.Degraded, metadata.Incomplete, safeText(strings.Join(metadata.ReasonCodes, ",")))
}

func validatePageInput(limit int, cursor string) error {
	if limit < 1 || limit > maxPageLimit {
		return commandError("invalid_limit", "limit must be between 1 and 200", "choose a bounded page size", ExitInput)
	}
	if strings.ContainsAny(cursor, "\r\n\x00") {
		return commandError("invalid_cursor", "cursor contains invalid control data", "use the opaque cursor exactly as returned", ExitInput)
	}
	return nil
}

func parseDueBefore(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, commandError("invalid_time", "due-before must use RFC3339", "use a value such as 2026-08-24T00:00:00Z", ExitInput)
	}
	return &parsed, nil
}
