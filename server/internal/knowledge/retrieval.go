package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	defaultDocumentShortlist = 8
	defaultFallbackChoices   = 3
	defaultMaxDepth          = 8
	defaultCandidatesLayer   = 20
	defaultMaxHits           = 10
	defaultTotalCandidates   = 200
)

type retrievalDocument struct {
	path     string
	revision DocumentRevision
	score    int
}

type queueItem struct {
	document         *retrievalDocument
	parent           NodeRevision
	depth            int
	originTraceIndex int
}

func (s *Service) Retrieve(ctx context.Context, command RetrievalCommand) (RetrievalResult, error) {
	queryTokens := retrievalTokens(command.Query)
	if len(queryTokens) == 0 {
		return RetrievalResult{}, &Error{Code: CodeInvalidRequest}
	}
	contextVersion := command.QueryContextSchemaVersion
	if contextVersion == "" {
		contextVersion = QueryContextVersion
	}
	if contextVersion != QueryContextVersion {
		return RetrievalResult{}, &Error{Code: CodeInvalidRequest}
	}
	limits, initiallyTruncated := normalizeRetrievalLimits(command.Limits)
	var revision KnowledgeRevision
	if command.KnowledgeRevisionID == nil {
		head, err := s.store.Head(ctx)
		if err != nil {
			return RetrievalResult{}, err
		}
		if head == nil {
			return RetrievalResult{}, &Error{Code: CodeNotFound}
		}
		revision = *head
	} else {
		id := strings.ToLower(strings.TrimSpace(*command.KnowledgeRevisionID))
		if !validUUID(id) {
			return RetrievalResult{}, &Error{Code: CodeInvalidRequest}
		}
		loaded, err := s.store.Revision(ctx, id)
		if err != nil {
			return RetrievalResult{}, err
		}
		revision = loaded
	}
	result := RetrievalResult{
		KnowledgeRevisionID: revision.ID, RetrieverVersion: RetrieverVersion, SelectorVersion: SelectorVersion,
		QueryContextVersion: contextVersion, SummarySnapshot: []string{}, Trace: []RetrievalTrace{}, Hits: []RetrievalHit{},
		Truncated: initiallyTruncated,
	}
	artifacts, err := s.store.ReadyNodeArtifacts(ctx, revision.ID)
	if err != nil {
		return RetrievalResult{}, err
	}
	artifactByNode, summarySnapshot, artifactFailureReason := pinSummaryArtifacts(revision, artifacts)
	result.SummarySnapshot = summarySnapshot
	documents := scoreDocuments(revision.Documents, queryTokens, s.canonicalizer)
	if len(documents) > defaultDocumentShortlist {
		documents = documents[:defaultDocumentShortlist]
	}
	for _, document := range documents {
		result.DocumentShortlist = append(result.DocumentShortlist, document.path)
	}
	allNodes := map[string]NodeRevision{}
	for i := range documents {
		for _, node := range documents[i].revision.Nodes {
			allNodes[node.ID] = node
		}
	}
	// The shortlist is the frozen traversal input. Keep every document in it so
	// roots from later documents cannot be skipped when an earlier document has
	// a positive score.
	traversalDocuments := documents
	queue := make([]queueItem, 0, len(traversalDocuments))
	for i := range traversalDocuments {
		if len(traversalDocuments[i].revision.Nodes) != 0 {
			queue = append(queue, queueItem{document: &traversalDocuments[i], parent: traversalDocuments[i].revision.Nodes[0], depth: 0, originTraceIndex: -1})
		}
	}
	totalCandidates := 0
	hitSeen := map[string]struct{}{}
	for len(queue) != 0 {
		item := queue[0]
		queue = queue[1:]
		if item.depth >= limits.MaxDepth {
			markTraceTruncated(&result, item.originTraceIndex)
			continue
		}
		children := childrenOf(item.document.revision, item.parent)
		if len(children) == 0 {
			continue
		}
		candidates := scoreNodes(item.document.revision, children, queryTokens, artifactByNode)
		layerTruncated := false
		if len(candidates) > limits.CandidatesPerLayer {
			candidates = candidates[:limits.CandidatesPerLayer]
			layerTruncated = true
			result.Truncated = true
		}
		remaining := limits.TotalCandidates - totalCandidates
		if remaining <= 0 {
			markTraceTruncated(&result, item.originTraceIndex)
			break
		}
		if len(candidates) > remaining {
			candidates = candidates[:remaining]
			layerTruncated = true
			result.Truncated = true
		}
		totalCandidates += len(candidates)
		candidateHash := CandidateSetHash(revision.ID, item.parent.ID, candidates)
		selectorRequest := SelectorRequest{
			KnowledgeRevisionID: revision.ID, Query: command.Query, QueryContextVersion: contextVersion,
			Context: command.Context, ParentNodeRevisionID: item.parent.ID, Candidates: candidates,
			SummarySnapshot:  summariesForCandidates(candidates, artifactByNode),
			CandidateSetHash: candidateHash, RemainingBudget: limits.TotalCandidates - totalCandidates,
			ArtifactFailureReason: artifactFailureReason,
		}
		decisions, degraded, reason, selectorTruncated := s.selectLayer(ctx, selectorRequest, allNodes)
		if degraded {
			result.Degraded = true
		}
		if selectorTruncated {
			layerTruncated = true
			result.Truncated = true
		}
		fullDecisions := make([]Decision, len(candidates))
		selected := make(map[string]string, len(decisions))
		for _, decision := range decisions {
			selected[decision.NodeRevisionID] = decision.Action
		}
		for i, candidate := range candidates {
			fullDecisions[i] = Decision{NodeRevisionID: candidate.NodeRevisionID, Action: selected[candidate.NodeRevisionID]}
		}
		traceIndex := len(result.Trace)
		result.Trace = append(result.Trace, RetrievalTrace{
			Index: traceIndex, Depth: item.depth, ParentNodeRevisionID: item.parent.ID,
			Candidates: candidates, Decisions: fullDecisions, CandidateSetHash: candidateHash,
			ReasonCode: reason, Degraded: degraded, Truncated: layerTruncated,
		})
		candidateByID := make(map[string]Candidate, len(candidates))
		for _, candidate := range candidates {
			candidateByID[candidate.NodeRevisionID] = candidate
		}
		for _, decision := range decisions {
			node := allNodes[decision.NodeRevisionID]
			switch decision.Action {
			case "select", "select_expand":
				if item.document.score > 0 {
					if _, exists := hitSeen[node.ID]; !exists {
						hitSeen[node.ID] = struct{}{}
						result.Hits = append(result.Hits, makeHit(*item.document, node, traceIndex, item.depth))
					}
				}
			}
			if decision.Action == "expand" || decision.Action == "select_expand" {
				if candidateByID[decision.NodeRevisionID].HasChildren {
					queue = enqueueDocumentBFS(queue, queueItem{document: item.document, parent: node, depth: item.depth + 1, originTraceIndex: traceIndex})
				}
			}
		}
		sort.Slice(result.Hits, func(i, j int) bool {
			if result.Hits[i].TraceIndex != result.Hits[j].TraceIndex {
				return result.Hits[i].TraceIndex < result.Hits[j].TraceIndex
			}
			if result.Hits[i].Depth != result.Hits[j].Depth {
				return result.Hits[i].Depth < result.Hits[j].Depth
			}
			return result.Hits[i].NodeRevisionID < result.Hits[j].NodeRevisionID
		})
		if len(result.Hits) >= limits.MaxHits {
			if len(result.Hits) > limits.MaxHits || len(queue) != 0 {
				markTraceTruncated(&result, traceIndex)
			}
			result.Hits = result.Hits[:limits.MaxHits]
			break
		}
	}
	return result, nil
}

func markTraceTruncated(result *RetrievalResult, preferredIndex int) {
	result.Truncated = true
	if preferredIndex < 0 || preferredIndex >= len(result.Trace) {
		preferredIndex = len(result.Trace) - 1
	}
	if preferredIndex >= 0 {
		result.Trace[preferredIndex].Truncated = true
	}
}

func enqueueDocumentBFS(queue []queueItem, item queueItem) []queueItem {
	return append(queue, item)
}

func (s *Service) selectLayer(ctx context.Context, request SelectorRequest, allNodes map[string]NodeRevision) ([]Decision, bool, string, bool) {
	fallback := lexicalDecisions(request.Candidates)
	if request.ArtifactFailureReason != "" {
		return fallback, true, request.ArtifactFailureReason, false
	}
	if s.selector == nil {
		return fallback, true, "selector_not_configured", false
	}
	response, err := s.selector.Select(ctx, request)
	if err != nil {
		var failure *SelectorFailure
		if errors.As(err, &failure) {
			return fallback, true, failure.Reason, failure.Truncated
		}
		return fallback, true, "selector_upstream_error", false
	}
	if response.KnowledgeRevisionID != request.KnowledgeRevisionID {
		return fallback, true, "selector_cross_revision", false
	}
	if response.CandidateSetHash != request.CandidateSetHash {
		return fallback, true, "selector_stale_response", false
	}
	if len(response.Decisions) > defaultFallbackChoices {
		return fallback, true, "selector_over_budget", true
	}
	positions := make(map[string]int, len(request.Candidates))
	for i, candidate := range request.Candidates {
		positions[candidate.NodeRevisionID] = i
	}
	lastPosition := -1
	seen := map[string]struct{}{}
	for _, decision := range response.Decisions {
		position, exists := positions[decision.NodeRevisionID]
		if !exists {
			if _, belongsToFrozenRevision := allNodes[decision.NodeRevisionID]; belongsToFrozenRevision {
				return fallback, true, "selector_wrong_parent", false
			}
			return fallback, true, "selector_unknown_candidate", false
		}
		if _, duplicate := seen[decision.NodeRevisionID]; duplicate || position <= lastPosition {
			return fallback, true, "selector_schema_error", false
		}
		seen[decision.NodeRevisionID] = struct{}{}
		lastPosition = position
		candidate := request.Candidates[position]
		switch decision.Action {
		case "select":
		case "expand", "select_expand":
			if !candidate.HasChildren {
				return fallback, true, "selector_schema_error", false
			}
		default:
			return fallback, true, "selector_schema_error", false
		}
	}
	return append([]Decision(nil), response.Decisions...), false, "", false
}

func lexicalDecisions(candidates []Candidate) []Decision {
	positive := make([]Candidate, 0, defaultFallbackChoices)
	for _, candidate := range candidates {
		if candidate.Score > 0 {
			positive = append(positive, candidate)
			if len(positive) == defaultFallbackChoices {
				break
			}
		}
	}
	if len(positive) == 0 && len(candidates) != 0 {
		positive = append(positive, candidates[0])
	}
	result := make([]Decision, 0, len(positive))
	for _, candidate := range positive {
		action := "select"
		if candidate.HasChildren {
			if candidate.LocalBodyScore > 0 {
				action = "select_expand"
			} else {
				action = "expand"
			}
		}
		result = append(result, Decision{NodeRevisionID: candidate.NodeRevisionID, Action: action})
	}
	return result
}

func CandidateSetHash(revisionID, parentNodeRevisionID string, candidates []Candidate) string {
	var builder strings.Builder
	builder.WriteString("candidate-set-v1\n")
	builder.WriteString(revisionID)
	builder.WriteByte('\n')
	builder.WriteString(parentNodeRevisionID)
	builder.WriteByte('\n')
	for _, candidate := range candidates {
		summary := candidate.SummaryArtifactID
		if summary == "" {
			summary = "-"
		}
		builder.WriteString(intString(candidate.Ordinal))
		builder.WriteByte('|')
		builder.WriteString(candidate.NodeRevisionID)
		builder.WriteByte('|')
		builder.WriteString(intString(candidate.Score))
		builder.WriteByte('|')
		builder.WriteString(candidate.TitleSHA256)
		builder.WriteByte('|')
		builder.WriteString(summary)
		builder.WriteByte('\n')
	}
	hash := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(hash[:])
}

func pinSummaryArtifacts(revision KnowledgeRevision, artifacts []NodeArtifact) (map[string]NodeArtifact, []string, string) {
	ownedNodes := make(map[string]struct{})
	for _, document := range revision.Documents {
		for _, node := range document.Revision.Nodes {
			ownedNodes[node.ID] = struct{}{}
		}
	}
	selected := make(map[string]NodeArtifact)
	for _, artifact := range artifacts {
		if _, owned := ownedNodes[artifact.NodeRevisionID]; !owned {
			return map[string]NodeArtifact{}, []string{}, "selector_cross_revision_artifact"
		}
		inputHash, err := hex.DecodeString(artifact.InputHash)
		if !validUUID(artifact.ID) || artifact.Kind != "summary" || artifact.Status != "ready" || err != nil || len(inputHash) != sha256.Size {
			return map[string]NodeArtifact{}, []string{}, "selector_stale_artifact"
		}
		current, exists := selected[artifact.NodeRevisionID]
		if !exists || artifact.CreatedAt.After(current.CreatedAt) || (artifact.CreatedAt.Equal(current.CreatedAt) && artifact.ID > current.ID) {
			selected[artifact.NodeRevisionID] = artifact
		}
	}
	nodeIDs := make([]string, 0, len(selected))
	for nodeID := range selected {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Strings(nodeIDs)
	snapshot := make([]string, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		snapshot = append(snapshot, selected[nodeID].ID)
	}
	return selected, snapshot, ""
}

func summariesForCandidates(candidates []Candidate, artifacts map[string]NodeArtifact) []NodeArtifact {
	result := make([]NodeArtifact, 0, len(candidates))
	for _, candidate := range candidates {
		if artifact, exists := artifacts[candidate.NodeRevisionID]; exists {
			result = append(result, artifact)
		}
	}
	return result
}

func scoreDocuments(input []SnapshotDocument, queryTokens []string, canonicalizer *Canonicalizer) []retrievalDocument {
	result := make([]retrievalDocument, 0, len(input))
	for _, document := range input {
		var titles strings.Builder
		for _, node := range document.Revision.Nodes {
			if node.HeadingLevel != 0 {
				titles.WriteString(node.Title)
				titles.WriteByte(' ')
			}
		}
		excerpt := canonicalUserBody(document.Revision.CanonicalMarkdown, canonicalizer)
		score := 4*scoreField(queryTokens, document.Path) + 3*scoreField(queryTokens, titles.String()) + scoreField(queryTokens, excerpt)
		result = append(result, retrievalDocument{path: document.Path, revision: document.Revision, score: score})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].score != result[j].score {
			return result[i].score > result[j].score
		}
		return result[i].revision.ID < result[j].revision.ID
	})
	return result
}

func canonicalUserBody(canonical string, canonicalizer *Canonicalizer) string {
	_, body, err := parseFrontMatter([]byte(canonical))
	if err != nil {
		return ""
	}
	clean, _, err := canonicalizer.stripIdentityMarkers([]byte(body))
	if err != nil {
		return ""
	}
	return string(clean)
}

func scoreNodes(document DocumentRevision, nodes []NodeRevision, queryTokens []string, artifacts map[string]NodeArtifact) []Candidate {
	byID := make(map[string]NodeRevision, len(document.Nodes))
	for _, node := range document.Nodes {
		byID[node.ID] = node
	}
	result := make([]Candidate, 0, len(nodes))
	for _, node := range nodes {
		var childTitles strings.Builder
		for _, childID := range node.Children {
			childTitles.WriteString(byID[childID].Title)
			childTitles.WriteByte(' ')
		}
		local := ""
		if node.LocalBodyRange.Start >= 0 && node.LocalBodyRange.End <= len(document.CanonicalMarkdown) && node.LocalBodyRange.Start <= node.LocalBodyRange.End {
			local = truncateUTF8(document.CanonicalMarkdown[node.LocalBodyRange.Start:node.LocalBodyRange.End], 2048)
		}
		localScore := scoreField(queryTokens, local)
		score := 4*scoreField(queryTokens, node.Title) + 2*scoreField(queryTokens, strings.Join(node.AncestorTitles, " ")) + scoreField(queryTokens, childTitles.String()) + localScore
		titleHash := sha256.Sum256([]byte(node.Title))
		candidate := Candidate{
			NodeRevisionID: node.ID, Score: score, Title: node.Title, TitleSHA256: hex.EncodeToString(titleHash[:]),
			HasChildren: len(node.Children) != 0, LocalBodyScore: localScore,
		}
		if artifact, exists := artifacts[node.ID]; exists {
			candidate.SummaryArtifactID = artifact.ID
		}
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Score != result[j].Score {
			return result[i].Score > result[j].Score
		}
		return result[i].NodeRevisionID < result[j].NodeRevisionID
	})
	for i := range result {
		result[i].Ordinal = i
	}
	return result
}

func childrenOf(document DocumentRevision, parent NodeRevision) []NodeRevision {
	byID := make(map[string]NodeRevision, len(document.Nodes))
	for _, node := range document.Nodes {
		byID[node.ID] = node
	}
	result := make([]NodeRevision, 0, len(parent.Children))
	for _, id := range parent.Children {
		if child, exists := byID[id]; exists {
			result = append(result, child)
		}
	}
	return result
}

func scoreField(queryTokens []string, field string) int {
	return fieldScore(queryTokens, truncateUTF8(field, 2048))
}

func fieldScore(queryTokens []string, field string) int {
	field = truncateUTF8(field, 2048)
	fieldSet := map[string]struct{}{}
	for _, token := range retrievalTokens(field) {
		fieldSet[token] = struct{}{}
	}
	matches := 0
	for _, token := range queryTokens {
		if _, exists := fieldSet[token]; exists {
			matches++
		}
	}
	if len(queryTokens) == 0 {
		return 0
	}
	return 1_000_000 * matches / len(queryTokens)
}

func makeHit(document retrievalDocument, node NodeRevision, traceIndex, depth int) RetrievalHit {
	slice := ""
	if node.SectionRange.Start >= 0 && node.SectionRange.End <= len(document.revision.CanonicalMarkdown) && node.SectionRange.Start <= node.SectionRange.End {
		slice = document.revision.CanonicalMarkdown[node.SectionRange.Start:node.SectionRange.End]
	}
	return RetrievalHit{
		DocumentID: document.revision.DocumentID, DocumentRevisionID: document.revision.ID,
		NodeID: node.NodeID, NodeRevisionID: node.ID, Path: document.path,
		HeadingRange: node.HeadingRange, LocalBodyRange: node.LocalBodyRange, SectionRange: node.SectionRange,
		CanonicalSlice: slice, SliceSHA256: sha256Hex([]byte(slice)), TraceIndex: traceIndex, Depth: depth,
		Provenance: "canonical_markdown",
	}
}

func normalizeRetrievalLimits(input RetrievalLimits) (RetrievalLimits, bool) {
	result := input
	truncated := false
	result.MaxDepth, truncated = bounded(result.MaxDepth, defaultMaxDepth, truncated)
	result.CandidatesPerLayer, truncated = bounded(result.CandidatesPerLayer, defaultCandidatesLayer, truncated)
	result.MaxHits, truncated = bounded(result.MaxHits, defaultMaxHits, truncated)
	result.TotalCandidates, truncated = bounded(result.TotalCandidates, defaultTotalCandidates, truncated)
	return result, truncated
}

func bounded(value, maximum int, truncated bool) (int, bool) {
	if value <= 0 {
		return maximum, truncated
	}
	if value > maximum {
		return maximum, true
	}
	return value, truncated
}

func truncateUTF8(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	end := maximum
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func intString(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buffer [24]byte
	position := len(buffer)
	for value > 0 {
		position--
		buffer[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		buffer[position] = '-'
	}
	return string(buffer[position:])
}
