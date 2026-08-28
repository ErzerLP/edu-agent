package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/importer"
)

func (a *App) runKnowledge(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return commandError("usage", "knowledge requires import, maintenance, or notesync", "run edu-agent knowledge import <file-or-directory>, edu-agent knowledge maintenance proposals, or edu-agent knowledge notesync status", ExitInput)
	}
	if args[0] == "maintenance" {
		return a.RunKnowledgeMaintenance(ctx, args[1:])
	}
	if args[0] == "notesync" {
		return a.RunKnowledgeNotesync(ctx, args[1:])
	}
	if args[0] != "import" {
		return commandError("usage", "knowledge requires import, maintenance, or notesync", "run edu-agent knowledge import <file-or-directory>, edu-agent knowledge maintenance proposals, or edu-agent knowledge notesync status", ExitInput)
	}
	set := newFlagSet("knowledge import")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	if err := set.Parse(args[1:]); err != nil || len(set.Args()) != 1 {
		return commandError("usage", "knowledge import requires one Markdown file or directory", "run edu-agent knowledge import <file-or-directory>", ExitInput)
	}
	batch, err := importer.Load(set.Args()[0])
	if err != nil {
		return commandError("invalid_markdown_input", "the Markdown file set is invalid", "remove symlinks, invalid paths, oversized files, or non-UTF-8 content", ExitInput)
	}
	operationID, err := a.NewUUID()
	if err != nil {
		return commandError("uuid_generation_failed", "a secure operation ID could not be generated", "inspect the operating system random source", ExitInternal)
	}
	if err := validateInitialImportRequestSize(operationID, batch.Documents); err != nil {
		return err
	}
	bound, timeout, err := a.loadBinding(flags.overrides())
	if err != nil {
		return err
	}
	a.printInsecureWarning(bound.Config)
	client := a.NewClient(bound.Config.ServerURL, bound.Token, timeout)
	var expectedParent *string
	head, err := client.KnowledgeHead(ctx)
	if err != nil {
		var apiErr *api.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "not_found" {
			return mapAPIError(err)
		}
	} else {
		expectedParent = &head.RevisionID
	}
	request := api.ImportRequest{
		OperationID: operationID, ExpectedParentRevisionID: expectedParent, Source: "go-cli-m1", Documents: batch.Documents,
	}
	if err := validateImportRequestSize(request); err != nil {
		return err
	}
	result, err := client.ImportKnowledge(ctx, request)
	if err == nil {
		return printImportResult(a.Out, result)
	}
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "identity_review_required" || apiErr.IdentityReview == nil {
		return mapAPIError(err)
	}
	result, err = a.resolveIdentityReview(ctx, client, request, *apiErr.IdentityReview)
	if err != nil {
		return err
	}
	return printImportResult(a.Out, result)
}

func validateInitialImportRequestSize(operationID string, documents []api.ImportDocument) error {
	maxParent := "00000000-0000-4000-8000-000000000000"
	return validateImportRequestSize(api.ImportRequest{
		OperationID: operationID, ExpectedParentRevisionID: &maxParent, Source: "go-cli-m1", Documents: documents,
	})
}

func validateImportRequestSize(request api.ImportRequest) error {
	data, err := json.Marshal(request)
	if err != nil {
		return commandError("request_encoding_failed", "the import request could not be encoded", "inspect local Markdown metadata", ExitInternal)
	}
	if len(data) > importer.MaxRequestSize {
		return commandError("payload_too_large", "the JSON import body exceeds 16 MiB", "split the import into smaller batches", ExitInput)
	}
	return nil
}

func printImportResult(out interface{ Write([]byte) (int, error) }, result api.ImportResult) error {
	state := "created"
	if result.Replayed {
		state = "replayed"
	} else if result.Unchanged {
		state = "unchanged"
	}
	_, err := fmt.Fprintf(out, "Knowledge revision: %s\nRevision number: %d\nResult: %s\n", safeText(result.Revision.RevisionID), result.Revision.RevisionNo, safeText(state))
	return err
}

const (
	maxIdentityReviewSubmissions = 6
	maxIdentityReviewRefreshes   = 1
)

func (a *App) resolveIdentityReview(ctx context.Context, client APIClient, original api.ImportRequest, review api.IdentityReview) (api.ImportResult, error) {
	documentDecisions := make(map[string]api.DocumentResolution)
	nodeDecisions := make(map[string]api.NodeResolution)
	refreshes := 0
	for submission := 0; submission < maxIdentityReviewSubmissions; submission++ {
		documentResolutions, nodeResolutions, err := a.collectIdentityResolutions(review)
		if err != nil {
			return api.ImportResult{}, err
		}
		for _, resolution := range documentResolutions {
			documentDecisions[resolution.Locator] = resolution
		}
		for _, resolution := range nodeResolutions {
			nodeDecisions[resolution.Locator] = resolution
		}
		operationID, err := a.NewUUID()
		if err != nil {
			return api.ImportResult{}, commandError("uuid_generation_failed", "a secure resolution operation ID could not be generated", "inspect the operating system random source", ExitInternal)
		}
		resolved := original
		resolved.OperationID = operationID
		resolved.IdentityReviewBasisHash = review.BasisHash
		resolved.IdentityReviewOperationID = review.OperationID
		resolved.IdentityReviewReceipt = review.Receipt
		resolved.DocumentResolutions = sortedDocumentResolutions(documentDecisions)
		resolved.NodeResolutions = sortedNodeResolutions(nodeDecisions)
		if err := validateImportRequestSize(resolved); err != nil {
			return api.ImportResult{}, err
		}
		result, importErr := client.ImportKnowledge(ctx, resolved)
		if importErr == nil {
			return result, nil
		}
		var apiErr *api.APIError
		if !errors.As(importErr, &apiErr) {
			return api.ImportResult{}, mapAPIError(importErr)
		}
		switch apiErr.Code {
		case "identity_review_required":
			if apiErr.IdentityReview == nil || apiErr.IdentityReview.Receipt == review.Receipt {
				return api.ImportResult{}, commandError("identity_review_stalled", "identity review did not advance to a new receipt", "run the import again after inspecting the service", ExitConflict)
			}
			review = *apiErr.IdentityReview
			continue
		case "stale_identity_review":
			if refreshes >= maxIdentityReviewRefreshes {
				return api.ImportResult{}, commandError("stale_identity_review", "identity review expired twice; no old decision was reused", "run the import again after the service is stable", ExitConflict)
			}
			refreshes++
			refresh := original
			refresh.IdentityReviewBasisHash = ""
			refresh.IdentityReviewOperationID = ""
			refresh.IdentityReviewReceipt = ""
			refresh.DocumentResolutions = nil
			refresh.NodeResolutions = nil
			_, refreshErr := client.ImportKnowledge(ctx, refresh)
			var refreshAPIError *api.APIError
			if !errors.As(refreshErr, &refreshAPIError) || refreshAPIError.Code != "identity_review_required" || refreshAPIError.IdentityReview == nil {
				if refreshErr == nil {
					return api.ImportResult{}, commandError("identity_review_changed", "the stale review did not produce a new review receipt", "read the current knowledge head before trying again", ExitConflict)
				}
				return api.ImportResult{}, mapAPIError(refreshErr)
			}
			_, _ = fmt.Fprintln(a.Err, "warning[stale_identity_review]: the service issued a new review; previous decisions were discarded")
			documentDecisions = make(map[string]api.DocumentResolution)
			nodeDecisions = make(map[string]api.NodeResolution)
			review = *refreshAPIError.IdentityReview
		default:
			return api.ImportResult{}, mapAPIError(importErr)
		}
	}
	return api.ImportResult{}, commandError("identity_review_limit", "identity review exceeded the bounded multi-stage limit", "run the import again after inspecting the service review sequence", ExitConflict)
}

func sortedDocumentResolutions(values map[string]api.DocumentResolution) []api.DocumentResolution {
	locators := make([]string, 0, len(values))
	for locator := range values {
		locators = append(locators, locator)
	}
	sort.Strings(locators)
	result := make([]api.DocumentResolution, 0, len(locators))
	for _, locator := range locators {
		result = append(result, values[locator])
	}
	return result
}

func sortedNodeResolutions(values map[string]api.NodeResolution) []api.NodeResolution {
	locators := make([]string, 0, len(values))
	for locator := range values {
		locators = append(locators, locator)
	}
	sort.Strings(locators)
	result := make([]api.NodeResolution, 0, len(locators))
	for _, locator := range locators {
		result = append(result, values[locator])
	}
	return result
}

func (a *App) collectIdentityResolutions(review api.IdentityReview) ([]api.DocumentResolution, []api.NodeResolution, error) {
	_, _ = fmt.Fprintln(a.Out, "Identity review required.")
	documents := make([]api.DocumentResolution, 0, len(review.Documents))
	for _, item := range review.Documents {
		printDocumentReview(a.Out, item)
		action, err := a.Terminal.ReadLine("Document action (preserve/new): ")
		if err != nil {
			return nil, nil, commandError("identity_review_input_failed", "document action could not be read", "rerun the import and review every candidate", ExitInput)
		}
		action = strings.TrimSpace(strings.ToLower(action))
		if action != "preserve" && action != "new" {
			return nil, nil, commandError("invalid_identity_action", "document action must be preserve or new", "rerun the import and select an allowed action", ExitInput)
		}
		resolution := api.DocumentResolution{Locator: item.Locator, Action: action}
		if action == "preserve" {
			source, readErr := a.Terminal.ReadLine("Source document ID: ")
			if readErr != nil || !documentCandidateContains(item.Candidates, strings.TrimSpace(source)) {
				return nil, nil, commandError("invalid_identity_source", "preserve requires an explicit candidate document ID", "rerun the import and enter a displayed stable ID", ExitInput)
			}
			resolution.DocumentID = strings.TrimSpace(source)
		}
		reason, readErr := a.Terminal.ReadLine("Reason: ")
		if readErr != nil || strings.TrimSpace(reason) == "" {
			return nil, nil, commandError("invalid_identity_reason", "every identity decision requires a non-empty reason", "rerun the import and explain the decision", ExitInput)
		}
		resolution.Reason = strings.TrimSpace(reason)
		documents = append(documents, resolution)
	}
	nodes := make([]api.NodeResolution, 0, len(review.Nodes))
	for _, item := range review.Nodes {
		printNodeReview(a.Out, item)
		action, err := a.Terminal.ReadLine("Node action (preserve/new/rewrite/split/merge): ")
		if err != nil {
			return nil, nil, commandError("identity_review_input_failed", "node action could not be read", "rerun the import and review every candidate", ExitInput)
		}
		action = strings.TrimSpace(strings.ToLower(action))
		if !allowedNodeAction(action) {
			return nil, nil, commandError("invalid_identity_action", "node action is not allowed", "rerun the import and select a displayed action", ExitInput)
		}
		resolution := api.NodeResolution{Locator: item.Locator, Action: action}
		if action != "new" {
			sources, readErr := a.Terminal.ReadLine("Source node revision IDs (comma-separated): ")
			if readErr != nil {
				return nil, nil, commandError("identity_review_input_failed", "node sources could not be read", "rerun the import", ExitInput)
			}
			resolution.SourceNodeRevisionIDs = parseSourceIDs(sources)
			if !validNodeSources(action, resolution.SourceNodeRevisionIDs, item.Candidates) {
				return nil, nil, commandError("invalid_identity_source", "the action requires explicit displayed source node revision IDs", "rerun the import and enter candidate revision IDs", ExitInput)
			}
		}
		reason, readErr := a.Terminal.ReadLine("Reason: ")
		if readErr != nil || strings.TrimSpace(reason) == "" {
			return nil, nil, commandError("invalid_identity_reason", "every identity decision requires a non-empty reason", "rerun the import and explain the decision", ExitInput)
		}
		resolution.Reason = strings.TrimSpace(reason)
		nodes = append(nodes, resolution)
	}
	return documents, nodes, nil
}

func printDocumentReview(out interface{ Write([]byte) (int, error) }, item api.DocumentIdentityReview) {
	_, _ = fmt.Fprintf(out, "Document: %s\nLocator: %s\nReason: %s\n", safeText(item.Path), safeText(item.Locator), safeText(item.ReasonCode))
	printCandidates(out, item.Candidates)
}

func printNodeReview(out interface{ Write([]byte) (int, error) }, item api.NodeIdentityReview) {
	_, _ = fmt.Fprintf(out, "Node: %s preorder=%d\nLocator: %s\nReason: %s\n", safeText(item.Path), item.Preorder, safeText(item.Locator), safeText(item.ReasonCode))
	printCandidates(out, item.Candidates)
}

func printCandidates(out interface{ Write([]byte) (int, error) }, candidates []api.IdentityCandidate) {
	for _, candidate := range candidates {
		_, _ = fmt.Fprintf(out, "Candidate: stable_id=%s revision_id=%s reason=%s score=%d", safeText(candidate.StableID), safeText(candidate.RevisionID), safeText(candidate.ReasonCode), candidate.Score)
		if evidence := safeEvidence(candidate.Evidence); evidence != "" {
			_, _ = fmt.Fprintf(out, " evidence=%s", safeText(evidence))
		}
		_, _ = fmt.Fprintln(out)
	}
}

func safeEvidence(evidence map[string]any) string {
	allowed := map[string]bool{
		"path": true, "semantic_similarity": true, "title_ancestors_match": true,
		"explicit_marker": true, "same_path": true,
	}
	keys := make([]string, 0, len(evidence))
	for key, value := range evidence {
		if allowed[key] && primitiveEvidence(value) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, evidence[key]))
	}
	return strings.Join(parts, ",")
}

func primitiveEvidence(value any) bool {
	switch typed := value.(type) {
	case string, bool, json.Number, float64:
		return true
	case []any:
		for _, item := range typed {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func documentCandidateContains(candidates []api.IdentityCandidate, id string) bool {
	for _, candidate := range candidates {
		if candidate.StableID == id {
			return true
		}
	}
	return false
}

func allowedNodeAction(action string) bool {
	return action == "preserve" || action == "new" || action == "rewrite" || action == "split" || action == "merge"
}

func parseSourceIDs(input string) []string {
	var result []string
	seen := map[string]bool{}
	for _, part := range strings.Split(input, ",") {
		value := strings.TrimSpace(part)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func validNodeSources(action string, sources []string, candidates []api.IdentityCandidate) bool {
	if (action == "preserve" || action == "rewrite" || action == "split") && len(sources) != 1 {
		return false
	}
	if action == "merge" && len(sources) < 2 {
		return false
	}
	allowed := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		allowed[candidate.RevisionID] = true
	}
	for _, source := range sources {
		if !allowed[source] {
			return false
		}
	}
	return true
}
