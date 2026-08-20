package knowledge

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type ImportOperationRecord struct {
	RequestHash string
	Result      ImportResult
}

type PreparedCommit struct {
	OperationID              string
	RequestHash              string
	ExpectedParentRevisionID *string
	Revision                 KnowledgeRevision
	Unchanged                bool
	Lineages                 []Lineage
}

type CatalogReader interface {
	Head(context.Context) (*KnowledgeRevision, error)
	Revision(context.Context, string) (KnowledgeRevision, error)
	DocumentIdentityExists(context.Context, string) (bool, error)
	NodeIdentityOwner(context.Context, string) (string, bool, error)
	ReadyNodeArtifacts(context.Context, string) ([]NodeArtifact, error)
}

type ImportCommitter interface {
	LookupImportOperation(context.Context, string) (ImportOperationRecord, bool, error)
	CommitImport(context.Context, PreparedCommit) (ImportResult, error)
}

type CatalogStore interface {
	CatalogReader
	ImportCommitter
}

type ServiceOptions struct {
	Selector Selector
	NewUUID  func() string
	Now      func() time.Time
}

var identityReviewKey = func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(fmt.Sprintf("generate identity review key: %v", err))
	}
	return key
}()

type identityReviewRecord struct {
	basis     string
	operation string
	receipt   string
}

type Service struct {
	store         CatalogStore
	canonicalizer *Canonicalizer
	selector      Selector
	newUUID       func() string
	now           func() time.Time
	reviewMu      sync.RWMutex
	reviews       map[string]identityReviewRecord
}

func NewService(store CatalogStore, canonicalizer *Canonicalizer, options ServiceOptions) (*Service, error) {
	if store == nil || canonicalizer == nil {
		return nil, fmt.Errorf("knowledge store and canonicalizer are required")
	}
	if options.NewUUID == nil {
		options.NewUUID = uuid.NewString
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Service{store: store, canonicalizer: canonicalizer, selector: options.Selector, newUUID: options.NewUUID, now: options.Now, reviews: make(map[string]identityReviewRecord)}, nil
}

type preparedDocument struct {
	path       string
	inspected  InspectedDocument
	documentID string
	rootNodeID string
	nodeIDs    []string
	old        *SnapshotDocument
}

type pendingLineage struct {
	action            string
	reason            string
	sourceRevisionIDs []string
	targetPreorder    int
	documentIndex     int
}

func (s *Service) Import(ctx context.Context, command ImportCommand) (ImportResult, error) {
	source := strings.TrimSpace(command.Source)
	if !command.ExpectedParentProvided || !validUUID(strings.ToLower(command.OperationID)) || len(command.Documents) == 0 || len(command.Documents) > MaxImportDocuments || !utf8.ValidString(source) || utf8.RuneCountInString(source) < 1 || utf8.RuneCountInString(source) > MaxSourceRunes {
		return ImportResult{}, &Error{Code: CodeInvalidRequest}
	}
	command.Source = source
	command.OperationID = strings.ToLower(command.OperationID)
	command.IdentityReviewOperationID = strings.ToLower(strings.TrimSpace(command.IdentityReviewOperationID))
	command.IdentityReviewReceipt = strings.ToLower(strings.TrimSpace(command.IdentityReviewReceipt))
	if command.ExpectedParentRevisionID != nil {
		value := strings.ToLower(strings.TrimSpace(*command.ExpectedParentRevisionID))
		if !validUUID(value) {
			return ImportResult{}, &Error{Code: CodeInvalidRequest}
		}
		command.ExpectedParentRevisionID = &value
	}
	prepared, err := s.prepareDocuments(command.Documents)
	if err != nil {
		return ImportResult{}, err
	}
	requestHash := hashImportRequest(command, prepared)
	if operation, exists, err := s.store.LookupImportOperation(ctx, command.OperationID); err != nil {
		return ImportResult{}, fmt.Errorf("lookup knowledge import operation: %w", err)
	} else if exists {
		if operation.RequestHash != requestHash {
			return ImportResult{}, &Error{Code: CodeIdempotencyConflict}
		}
		result := operation.Result
		result.Replayed = true
		return result, nil
	}

	head, err := s.store.Head(ctx)
	if err != nil {
		return ImportResult{}, fmt.Errorf("read knowledge head: %w", err)
	}
	currentID := revisionID(head)
	if !sameOptionalID(command.ExpectedParentRevisionID, currentID) {
		return ImportResult{}, &Error{Code: CodeRevisionConflict, CurrentRevisionID: currentID, CurrentRevisionKnown: true}
	}
	var parent *KnowledgeRevision
	if command.ExpectedParentRevisionID != nil {
		loaded, err := s.store.Revision(ctx, *command.ExpectedParentRevisionID)
		if err != nil {
			return ImportResult{}, err
		}
		parent = &loaded
	}
	basis := reviewBasisHash(command.ExpectedParentRevisionID, prepared)
	hasResolutions := len(command.DocumentResolutions) != 0 || len(command.NodeResolutions) != 0
	if hasResolutions {
		if command.IdentityReviewBasisHash != basis || !validUUID(command.IdentityReviewOperationID) || command.IdentityReviewOperationID == command.OperationID || command.IdentityReviewReceipt != identityReviewReceipt(basis, command.IdentityReviewOperationID) || !s.validIssuedReview(basis, command.IdentityReviewOperationID, command.IdentityReviewReceipt) {
			return ImportResult{}, &Error{Code: CodeStaleIdentityReview}
		}
	} else if command.IdentityReviewBasisHash != "" && command.IdentityReviewBasisHash != basis {
		return ImportResult{}, &Error{Code: CodeStaleIdentityReview}
	}
	documentResolutions, err := indexDocumentResolutions(command.DocumentResolutions)
	if err != nil {
		return ImportResult{}, err
	}
	nodeResolutions, err := indexNodeResolutions(command.NodeResolutions)
	if err != nil {
		return ImportResult{}, err
	}
	if reviews, err := s.resolveDocuments(ctx, prepared, parent, basis, documentResolutions); err != nil {
		return ImportResult{}, err
	} else if len(reviews) != 0 {
		review := newIdentityReview(basis, command.OperationID, reviews, nil)
		s.rememberIssuedReview(review)
		return ImportResult{}, &Error{Code: CodeIdentityReviewRequired, Review: review}
	}
	if len(documentResolutions) != 0 {
		return ImportResult{}, &Error{Code: CodeInvalidRequest}
	}
	lineageDrafts, nodeReviews, err := s.resolveNodes(ctx, prepared, basis, nodeResolutions)
	if err != nil {
		return ImportResult{}, err
	}
	if len(nodeReviews) != 0 {
		review := newIdentityReview(basis, command.OperationID, nil, nodeReviews)
		s.rememberIssuedReview(review)
		return ImportResult{}, &Error{Code: CodeIdentityReviewRequired, Review: review}
	}
	if len(nodeResolutions) != 0 {
		return ImportResult{}, &Error{Code: CodeInvalidRequest}
	}

	built := make([]DocumentRevision, len(prepared))
	incomingDocumentIDs := make(map[string]struct{}, len(prepared))
	for i := range prepared {
		document, err := s.canonicalizer.Materialize(prepared[i].inspected, prepared[i].documentID, prepared[i].rootNodeID, prepared[i].nodeIDs)
		if err != nil {
			return ImportResult{}, err
		}
		built[i] = document
		incomingDocumentIDs[document.DocumentID] = struct{}{}
	}

	snapshot := make(map[string]SnapshotDocument)
	if parent != nil {
		for _, document := range parent.Documents {
			if _, moving := incomingDocumentIDs[document.Revision.DocumentID]; !moving {
				snapshot[document.Path] = document
			}
		}
	}
	for i, document := range built {
		if existing, occupied := snapshot[prepared[i].path]; occupied && existing.Revision.DocumentID != document.DocumentID {
			return ImportResult{}, &Error{Code: CodePathOccupied}
		}
		snapshot[prepared[i].path] = SnapshotDocument{Path: prepared[i].path, Revision: document}
	}
	foldedPaths := make(map[string]string, len(snapshot))
	for documentPath := range snapshot {
		folded := foldedPath(documentPath)
		if existingPath, duplicate := foldedPaths[folded]; duplicate && existingPath != documentPath {
			return ImportResult{}, &Error{Code: CodePathOccupied}
		}
		foldedPaths[folded] = documentPath
	}
	documents := make([]SnapshotDocument, 0, len(snapshot))
	for _, document := range snapshot {
		documents = append(documents, document)
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].Path < documents[j].Path })
	manifestHash := hashManifest(documents)
	if parent != nil && manifestHash == parent.ManifestHash {
		preparedCommit := PreparedCommit{
			OperationID: command.OperationID, RequestHash: requestHash,
			ExpectedParentRevisionID: command.ExpectedParentRevisionID, Revision: *parent, Unchanged: true,
		}
		return s.store.CommitImport(ctx, preparedCommit)
	}
	revisionID := s.newUUID()
	if !validUUID(revisionID) {
		return ImportResult{}, fmt.Errorf("UUID generator returned invalid knowledge revision ID")
	}
	revisionNo := int64(1)
	if parent != nil {
		revisionNo = parent.RevisionNo + 1
	}
	revision := KnowledgeRevision{
		ID: revisionID, RevisionNo: revisionNo, ParentRevisionID: cloneString(command.ExpectedParentRevisionID),
		ManifestHash: manifestHash, Source: strings.TrimSpace(command.Source), CreatedByDeviceID: command.ActorDeviceID,
		CreatedAt: s.now().UTC().Truncate(time.Microsecond), CanonicalizerVersion: CanonicalizerVersion, ParserVersion: ParserVersion,
		IndexerVersion: IndexerVersion, IdentityPolicyVersion: IdentityPolicyVersion, Documents: documents,
	}
	lineages, err := s.materializeLineages(lineageDrafts, built, revision)
	if err != nil {
		return ImportResult{}, err
	}
	revision.Lineages = lineages
	return s.store.CommitImport(ctx, PreparedCommit{
		OperationID: command.OperationID, RequestHash: requestHash,
		ExpectedParentRevisionID: command.ExpectedParentRevisionID, Revision: revision, Lineages: lineages,
	})
}

func (s *Service) rememberIssuedReview(review *IdentityReview) {
	s.reviewMu.Lock()
	defer s.reviewMu.Unlock()
	if s.reviews == nil {
		s.reviews = make(map[string]identityReviewRecord)
	}
	s.reviews[review.Receipt] = identityReviewRecord{basis: review.BasisHash, operation: review.OperationID, receipt: review.Receipt}
}

func (s *Service) validIssuedReview(basis, operationID, receipt string) bool {
	s.reviewMu.RLock()
	defer s.reviewMu.RUnlock()
	review, ok := s.reviews[receipt]
	return ok && review.basis == basis && review.operation == operationID && review.receipt == receipt
}

func identityReviewReceipt(basis, operationID string) string {
	mac := hmac.New(sha256.New, identityReviewKey)
	_, _ = mac.Write([]byte("identity-review-receipt-v2\n" + basis + "\n" + operationID + "\n"))
	return hex.EncodeToString(mac.Sum(nil))
}

func newIdentityReview(basis, operationID string, documents []DocumentIdentityReview, nodes []NodeIdentityReview) *IdentityReview {
	if documents == nil {
		documents = []DocumentIdentityReview{}
	}
	if nodes == nil {
		nodes = []NodeIdentityReview{}
	}
	return &IdentityReview{
		BasisHash: basis, OperationID: operationID, Receipt: identityReviewReceipt(basis, operationID),
		Documents: documents, Nodes: nodes,
	}
}

func (s *Service) materializeLineages(drafts []pendingLineage, built []DocumentRevision, revision KnowledgeRevision) ([]Lineage, error) {
	type group struct {
		action  string
		reason  string
		sources []string
		targets []string
	}
	groups := make([]group, 0, len(drafts))
	splitGroups := map[string]int{}
	for _, draft := range drafts {
		if draft.documentIndex < 0 || draft.documentIndex >= len(built) || draft.targetPreorder < 0 || draft.targetPreorder+1 >= len(built[draft.documentIndex].Nodes) {
			return nil, &Error{Code: CodeInvalidRequest}
		}
		target := built[draft.documentIndex].Nodes[draft.targetPreorder+1].ID
		if draft.action == "split" {
			key := strings.Join(append([]string{draft.action, draft.reason}, draft.sourceRevisionIDs...), "\x1f")
			if index, exists := splitGroups[key]; exists {
				groups[index].targets = append(groups[index].targets, target)
				continue
			}
			splitGroups[key] = len(groups)
		}
		groups = append(groups, group{action: draft.action, reason: draft.reason, sources: append([]string(nil), draft.sourceRevisionIDs...), targets: []string{target}})
	}
	sourceOwners := make(map[string]int)
	for groupIndex, item := range groups {
		groupSources := make(map[string]struct{}, len(item.sources))
		for _, source := range item.sources {
			if _, duplicate := groupSources[source]; duplicate {
				return nil, &Error{Code: CodeInvalidRequest}
			}
			groupSources[source] = struct{}{}
			if owner, exists := sourceOwners[source]; exists && owner != groupIndex {
				return nil, &Error{Code: CodeInvalidRequest}
			}
			sourceOwners[source] = groupIndex
		}
	}
	lineages := make([]Lineage, 0, len(groups))
	for _, item := range groups {
		if item.action == "split" && (len(item.sources) != 1 || len(item.targets) < 2) {
			return nil, &Error{Code: CodeInvalidRequest}
		}
		if item.action == "merge" && (len(item.sources) < 2 || len(item.targets) != 1) {
			return nil, &Error{Code: CodeInvalidRequest}
		}
		if item.action == "rewrite" && (len(item.sources) != 1 || len(item.targets) != 1) {
			return nil, &Error{Code: CodeInvalidRequest}
		}
		lineageID := s.newUUID()
		if !validUUID(lineageID) {
			return nil, fmt.Errorf("UUID generator returned invalid lineage ID")
		}
		members := make([]LineageMember, 0, len(item.sources)+len(item.targets))
		for _, source := range item.sources {
			members = append(members, LineageMember{Role: "source", NodeRevisionID: source})
		}
		for _, target := range item.targets {
			members = append(members, LineageMember{Role: "target", NodeRevisionID: target})
		}
		lineages = append(lineages, Lineage{
			ID: lineageID, KnowledgeRevisionID: revision.ID, Action: item.action,
			ActorDeviceID: revision.CreatedByDeviceID, Reason: item.reason,
			PolicyVersion: IdentityPolicyVersion, CreatedAt: revision.CreatedAt, Members: members,
		})
	}
	return lineages, nil
}

func (s *Service) prepareDocuments(input []ImportDocument) ([]preparedDocument, error) {
	prepared := make([]preparedDocument, 0, len(input))
	paths := map[string]struct{}{}
	for _, item := range input {
		documentPath, err := NormalizePath(item.Path)
		if err != nil {
			return nil, err
		}
		folded := foldedPath(documentPath)
		if _, duplicate := paths[folded]; duplicate {
			return nil, &Error{Code: CodeInvalidPath}
		}
		paths[folded] = struct{}{}
		inspected, err := s.canonicalizer.Inspect(item.Markdown)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, preparedDocument{path: documentPath, inspected: inspected})
	}
	sort.Slice(prepared, func(i, j int) bool { return prepared[i].path < prepared[j].path })
	totalNodes := 0
	for _, document := range prepared {
		totalNodes += len(document.inspected.DraftNodes) + 1
	}
	if totalNodes > MaxImportNodes {
		return nil, &Error{Code: CodePayloadTooLarge}
	}
	return prepared, nil
}

func (s *Service) resolveDocuments(ctx context.Context, documents []preparedDocument, parent *KnowledgeRevision, basis string, resolutions map[string]DocumentResolution) ([]DocumentIdentityReview, error) {
	oldByID := map[string]SnapshotDocument{}
	oldByHash := map[string][]SnapshotDocument{}
	oldByPath := map[string]SnapshotDocument{}
	if parent != nil {
		for _, document := range parent.Documents {
			oldByID[document.Revision.DocumentID] = document
			oldByHash[document.Revision.SemanticHash] = append(oldByHash[document.Revision.SemanticHash], document)
			oldByPath[document.Path] = document
		}
	}
	newHashCount := map[string]int{}
	for _, document := range documents {
		newHashCount[document.inspected.SemanticHash]++
	}
	assigned := map[string]string{}
	var reviews []DocumentIdentityReview
	for i := range documents {
		document := &documents[i]
		locator := documentLocator(basis, document.path)
		resolution, hasResolution := resolutions[locator]
		var selected string
		if explicit := document.inspected.ExplicitDocumentID; explicit != "" {
			if old, exists := oldByID[explicit]; exists {
				selected = explicit
				document.old = &old
			} else {
				exists, err := s.store.DocumentIdentityExists(ctx, explicit)
				if err != nil {
					return nil, err
				}
				if exists {
					return nil, &Error{Code: CodeDuplicateDocumentIdentity}
				}
				selected = explicit
			}
		} else if exact := oldByHash[document.inspected.SemanticHash]; len(exact) == 1 && newHashCount[document.inspected.SemanticHash] == 1 {
			selected = exact[0].Revision.DocumentID
			old := exact[0]
			document.old = &old
		} else if hasResolution {
			switch resolution.Action {
			case "preserve":
				old, exists := oldByID[resolution.DocumentID]
				candidates := documentCandidates(*document, oldByPath, parent, s.canonicalizer)
				if !exists || !candidateContainsStableID(candidates, resolution.DocumentID) || strings.TrimSpace(resolution.Reason) == "" {
					return nil, &Error{Code: CodeInvalidRequest}
				}
				selected = old.Revision.DocumentID
				document.old = &old
			case "new":
				if strings.TrimSpace(resolution.Reason) == "" {
					return nil, &Error{Code: CodeInvalidRequest}
				}
				selected = s.newUUID()
			default:
				return nil, &Error{Code: CodeInvalidRequest}
			}
			delete(resolutions, locator)
		} else {
			candidates := documentCandidates(*document, oldByPath, parent, s.canonicalizer)
			if len(candidates) != 0 {
				reviews = append(reviews, DocumentIdentityReview{Path: document.path, Locator: locator, ReasonCode: "document_match_ambiguous", Candidates: candidates})
				continue
			}
			selected = s.newUUID()
		}
		if !validUUID(selected) {
			return nil, fmt.Errorf("UUID generator returned invalid document ID")
		}
		if previous, duplicate := assigned[selected]; duplicate {
			_ = previous
			return nil, &Error{Code: CodeDuplicateDocumentIdentity}
		}
		assigned[selected] = document.path
		document.documentID = selected
	}
	sort.Slice(reviews, func(i, j int) bool { return reviews[i].Path < reviews[j].Path })
	return reviews, nil
}

func documentCandidates(document preparedDocument, oldByPath map[string]SnapshotDocument, parent *KnowledgeRevision, canonicalizer *Canonicalizer) []IdentityCandidate {
	candidates := map[string]IdentityCandidate{}
	if old, exists := oldByPath[document.path]; exists {
		candidates[old.Revision.DocumentID] = IdentityCandidate{
			StableID: old.Revision.DocumentID, RevisionID: old.Revision.ID, ReasonCode: "same_path",
			Evidence: map[string]any{"path": document.path},
		}
	}
	if parent != nil {
		newTokens := identityTokens(document.inspected.Body)
		for _, old := range parent.Documents {
			oldTokens := identityTokens(canonicalUserBody(old.Revision.CanonicalMarkdown, canonicalizer))
			score := similarity(oldTokens, newTokens, old.Revision.SemanticHash, document.inspected.SemanticHash)
			if score <= 0 {
				continue
			}
			candidate := candidates[old.Revision.DocumentID]
			candidate.StableID = old.Revision.DocumentID
			candidate.RevisionID = old.Revision.ID
			candidate.ReasonCode = "document_similarity"
			candidate.Score = score
			candidate.Evidence = map[string]any{"semantic_similarity": score}
			candidates[old.Revision.DocumentID] = candidate
		}
	}
	result := make([]IdentityCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].StableID < result[j].StableID })
	return result
}

func (s *Service) resolveNodes(ctx context.Context, documents []preparedDocument, basis string, resolutions map[string]NodeResolution) ([]pendingLineage, []NodeIdentityReview, error) {
	globallyAssigned := map[string]string{}
	var lineages []pendingLineage
	var reviews []NodeIdentityReview
	for documentIndex := range documents {
		document := &documents[documentIndex]
		oldNodes := map[string]NodeRevision{}
		oldHash := map[string][]NodeRevision{}
		newHashCount := map[string]int{}
		if document.old != nil {
			for i, node := range document.old.Revision.Nodes {
				if i == 0 {
					continue
				}
				oldNodes[node.NodeID] = node
				oldHash[node.SemanticLocalBodyHash] = append(oldHash[node.SemanticLocalBodyHash], node)
			}
		}
		for _, node := range document.inspected.DraftNodes {
			newHashCount[node.SemanticLocalBodyHash]++
		}
		rootID, err := s.resolveRootIdentity(ctx, *document)
		if err != nil {
			return nil, nil, err
		}
		document.rootNodeID = rootID
		if owner, duplicate := globallyAssigned[rootID]; duplicate && owner != document.documentID {
			return nil, nil, &Error{Code: CodeInvalidIdentityMarker}
		}
		globallyAssigned[rootID] = document.documentID
		document.nodeIDs = make([]string, len(document.inspected.DraftNodes))
		for nodeIndex, draft := range document.inspected.DraftNodes {
			locator := nodeLocator(basis, document.path, nodeIndex)
			resolution, hasResolution := resolutions[locator]
			var selected string
			var needsReview bool
			var reviewReason string
			if draft.ExplicitNodeID != "" {
				if old, exists := oldNodes[draft.ExplicitNodeID]; exists {
					oldTokens := nodeTokens(document.old.Revision, old, s.canonicalizer)
					score := similarity(oldTokens, draft.VisibleTokens, old.SemanticLocalBodyHash, draft.SemanticLocalBodyHash)
					bothEmpty := len(oldTokens) == 0 && len(draft.VisibleTokens) == 0
					unchanged := old.SemanticLocalBodyHash == draft.SemanticLocalBodyHash
					if unchanged || bothEmpty || (len(oldTokens) >= 8 && len(draft.VisibleTokens) >= 8 && score >= 500_000) {
						selected = old.NodeID
					} else {
						needsReview, reviewReason = true, "marked_node_changed"
					}
				} else {
					owner, exists, err := s.store.NodeIdentityOwner(ctx, draft.ExplicitNodeID)
					if err != nil {
						return nil, nil, err
					}
					if exists && owner != document.documentID {
						return nil, nil, &Error{Code: CodeInvalidIdentityMarker}
					}
					if exists {
						return nil, nil, &Error{Code: CodeInvalidIdentityMarker}
					}
					selected = draft.ExplicitNodeID
				}
			} else if exact := oldHash[draft.SemanticLocalBodyHash]; len(exact) == 1 && newHashCount[draft.SemanticLocalBodyHash] == 1 && len(draft.VisibleTokens) != 0 && len(nodeTokens(document.old.Revision, exact[0], s.canonicalizer)) != 0 {
				selected = exact[0].NodeID
			} else if hasResolution {
				selected, lineages, err = s.applyNodeResolution(*document, documentIndex, nodeIndex, resolution, oldNodes, lineages)
				if err != nil {
					return nil, nil, err
				}
				delete(resolutions, locator)
			} else {
				candidates := reviewNodeCandidates(*document, draft, oldNodes, s.canonicalizer)
				if needsReview || len(candidates) != 0 {
					if reviewReason == "" {
						reviewReason = "node_match_ambiguous"
					}
					reviews = append(reviews, NodeIdentityReview{Path: document.path, Locator: locator, Preorder: nodeIndex, ReasonCode: reviewReason, Candidates: candidates})
					continue
				}
				selected = s.newUUID()
			}
			if needsReview {
				if hasResolution {
					selected, lineages, err = s.applyNodeResolution(*document, documentIndex, nodeIndex, resolution, oldNodes, lineages)
					if err != nil {
						return nil, nil, err
					}
					delete(resolutions, locator)
				} else {
					candidates := reviewNodeCandidates(*document, draft, oldNodes, s.canonicalizer)
					reviews = append(reviews, NodeIdentityReview{
						Path: document.path, Locator: locator, Preorder: nodeIndex,
						ReasonCode: reviewReason, Candidates: candidates,
					})
					continue
				}
			}
			if !validUUID(selected) {
				return nil, nil, fmt.Errorf("UUID generator returned invalid node ID")
			}
			if owner, duplicate := globallyAssigned[selected]; duplicate {
				_ = owner
				return nil, nil, &Error{Code: CodeInvalidIdentityMarker}
			}
			globallyAssigned[selected] = document.documentID
			document.nodeIDs[nodeIndex] = selected
		}
	}
	sort.Slice(reviews, func(i, j int) bool {
		if reviews[i].Path != reviews[j].Path {
			return reviews[i].Path < reviews[j].Path
		}
		return reviews[i].Preorder < reviews[j].Preorder
	})
	return lineages, reviews, nil
}

func (s *Service) resolveRootIdentity(ctx context.Context, document preparedDocument) (string, error) {
	explicit := document.inspected.ExplicitRootNodeID
	if document.old != nil {
		oldRoot := document.old.Revision.RootNodeID
		if explicit == "" || explicit == oldRoot {
			return oldRoot, nil
		}
		return "", &Error{Code: CodeInvalidIdentityMarker}
	}
	if explicit != "" {
		_, exists, err := s.store.NodeIdentityOwner(ctx, explicit)
		if err != nil {
			return "", err
		}
		if exists {
			return "", &Error{Code: CodeInvalidIdentityMarker}
		}
		return explicit, nil
	}
	return s.newUUID(), nil
}

func (s *Service) applyNodeResolution(document preparedDocument, documentIndex, nodeIndex int, resolution NodeResolution, oldNodes map[string]NodeRevision, lineages []pendingLineage) (string, []pendingLineage, error) {
	if strings.TrimSpace(resolution.Reason) == "" {
		return "", lineages, &Error{Code: CodeInvalidRequest}
	}
	allowedSources := map[string]struct{}{}
	for _, candidate := range reviewNodeCandidates(document, document.inspected.DraftNodes[nodeIndex], oldNodes, s.canonicalizer) {
		allowedSources[candidate.RevisionID] = struct{}{}
	}
	sources := make([]NodeRevision, 0, len(resolution.SourceNodeRevisionIDs))
	seen := map[string]struct{}{}
	for _, sourceID := range resolution.SourceNodeRevisionIDs {
		if _, duplicate := seen[sourceID]; duplicate {
			return "", lineages, &Error{Code: CodeInvalidRequest}
		}
		seen[sourceID] = struct{}{}
		var matched *NodeRevision
		for _, old := range oldNodes {
			if old.ID == sourceID {
				copy := old
				matched = &copy
				break
			}
		}
		if matched == nil {
			return "", lineages, &Error{Code: CodeInvalidRequest}
		}
		if _, allowed := allowedSources[sourceID]; !allowed {
			return "", lineages, &Error{Code: CodeInvalidRequest}
		}
		sources = append(sources, *matched)
	}
	switch resolution.Action {
	case "preserve":
		if len(sources) != 1 {
			return "", lineages, &Error{Code: CodeInvalidRequest}
		}
		return sources[0].NodeID, lineages, nil
	case "new":
		if len(sources) != 0 {
			return "", lineages, &Error{Code: CodeInvalidRequest}
		}
		return s.newUUID(), lineages, nil
	case "rewrite", "split":
		if len(sources) != 1 {
			return "", lineages, &Error{Code: CodeInvalidRequest}
		}
	case "merge":
		if len(sources) < 2 {
			return "", lineages, &Error{Code: CodeInvalidRequest}
		}
	default:
		return "", lineages, &Error{Code: CodeInvalidRequest}
	}
	sourceIDs := make([]string, len(sources))
	for i, source := range sources {
		sourceIDs[i] = source.ID
	}
	sort.Strings(sourceIDs)
	lineages = append(lineages, pendingLineage{
		action: resolution.Action, reason: strings.TrimSpace(resolution.Reason), sourceRevisionIDs: sourceIDs,
		targetPreorder: nodeIndex, documentIndex: documentIndex,
	})
	return s.newUUID(), lineages, nil
}

func reviewNodeCandidates(document preparedDocument, draft DraftNode, oldNodes map[string]NodeRevision, canonicalizer *Canonicalizer) []IdentityCandidate {
	result := nodeCandidates(document, draft, oldNodes, canonicalizer)
	if draft.ExplicitNodeID != "" {
		if old, exists := oldNodes[draft.ExplicitNodeID]; exists && !candidateContainsStableID(result, old.NodeID) {
			oldTokens := nodeTokens(document.old.Revision, old, canonicalizer)
			result = append(result, IdentityCandidate{
				StableID: old.NodeID, RevisionID: old.ID, ReasonCode: "explicit_marker_changed",
				Score:    similarity(oldTokens, draft.VisibleTokens, old.SemanticLocalBodyHash, draft.SemanticLocalBodyHash),
				Evidence: map[string]any{"explicit_marker": true},
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].StableID < result[j].StableID })
	return result
}

func candidateContainsStableID(candidates []IdentityCandidate, stableID string) bool {
	for _, candidate := range candidates {
		if candidate.StableID == stableID {
			return true
		}
	}
	return false
}

func nodeCandidates(document preparedDocument, draft DraftNode, oldNodes map[string]NodeRevision, canonicalizer *Canonicalizer) []IdentityCandidate {
	var result []IdentityCandidate
	for _, old := range oldNodes {
		oldTokens := nodeTokens(document.old.Revision, old, canonicalizer)
		score := similarity(oldTokens, draft.VisibleTokens, old.SemanticLocalBodyHash, draft.SemanticLocalBodyHash)
		titleMatch := old.Title == draft.Title && strings.Join(old.AncestorTitles, "\x1f") == strings.Join(draft.AncestorTitles, "\x1f")
		if score == 0 && !titleMatch {
			continue
		}
		reason := "node_similarity"
		if titleMatch {
			reason = "same_title_ancestors"
		}
		result = append(result, IdentityCandidate{
			StableID: old.NodeID, RevisionID: old.ID, ReasonCode: reason, Score: score,
			Evidence: map[string]any{"semantic_similarity": score, "title_ancestors_match": titleMatch},
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].StableID < result[j].StableID })
	return result
}

func nodeTokens(document DocumentRevision, node NodeRevision, canonicalizer *Canonicalizer) []string {
	start, end := node.LocalBodyRange.Start, node.LocalBodyRange.End
	if start < 0 || end < start || end > len(document.CanonicalMarkdown) {
		return nil
	}
	return identityTokens(string(visibleMarkdown([]byte(document.CanonicalMarkdown[start:end]), canonicalizer.markdown)))
}

func (s *Service) Head(ctx context.Context) (*KnowledgeRevision, error) {
	return s.store.Head(ctx)
}

func (s *Service) Tree(ctx context.Context, revisionID string) (TreeResult, error) {
	if !validUUID(strings.ToLower(revisionID)) {
		return TreeResult{}, &Error{Code: CodeInvalidRequest}
	}
	revision, err := s.store.Revision(ctx, strings.ToLower(revisionID))
	if err != nil {
		return TreeResult{}, err
	}
	return TreeResult{Revision: revision}, nil
}

func (s *Service) Export(ctx context.Context, revisionID string) (ExportResult, error) {
	tree, err := s.Tree(ctx, revisionID)
	if err != nil {
		return ExportResult{}, err
	}
	result := ExportResult{RevisionID: tree.Revision.ID, Documents: make([]ExportDocument, 0, len(tree.Revision.Documents))}
	for _, document := range tree.Revision.Documents {
		markdown, err := ExportMarkdown(document.Revision.CanonicalMarkdown, tree.Revision.ID)
		if err != nil {
			return ExportResult{}, err
		}
		result.Documents = append(result.Documents, ExportDocument{Path: document.Path, Markdown: markdown})
	}
	return result, nil
}

func indexDocumentResolutions(input []DocumentResolution) (map[string]DocumentResolution, error) {
	result := make(map[string]DocumentResolution, len(input))
	for _, resolution := range input {
		if resolution.Locator == "" {
			return nil, &Error{Code: CodeInvalidRequest}
		}
		if _, duplicate := result[resolution.Locator]; duplicate {
			return nil, &Error{Code: CodeInvalidRequest}
		}
		result[resolution.Locator] = resolution
	}
	return result, nil
}

func indexNodeResolutions(input []NodeResolution) (map[string]NodeResolution, error) {
	result := make(map[string]NodeResolution, len(input))
	for _, resolution := range input {
		if resolution.Locator == "" {
			return nil, &Error{Code: CodeInvalidRequest}
		}
		if _, duplicate := result[resolution.Locator]; duplicate {
			return nil, &Error{Code: CodeInvalidRequest}
		}
		result[resolution.Locator] = resolution
	}
	return result, nil
}

func hashImportRequest(command ImportCommand, documents []preparedDocument) string {
	type hashDocument struct {
		Path        string `json:"path"`
		Fingerprint string `json:"fingerprint"`
	}
	value := struct {
		ExpectedParent  *string              `json:"expected_parent_revision_id"`
		Source          string               `json:"source"`
		Documents       []hashDocument       `json:"documents"`
		Basis           string               `json:"identity_review_basis_hash,omitempty"`
		ReviewOperation string               `json:"identity_review_operation_id,omitempty"`
		ReviewReceipt   string               `json:"identity_review_receipt,omitempty"`
		Document        []DocumentResolution `json:"document_resolutions,omitempty"`
		Node            []NodeResolution     `json:"node_resolutions,omitempty"`
	}{
		ExpectedParent: command.ExpectedParentRevisionID, Source: strings.TrimSpace(command.Source),
		Basis:           command.IdentityReviewBasisHash,
		ReviewOperation: command.IdentityReviewOperationID,
		ReviewReceipt:   command.IdentityReviewReceipt,
		Document:        append([]DocumentResolution(nil), command.DocumentResolutions...),
		Node:            append([]NodeResolution(nil), command.NodeResolutions...),
	}
	for _, document := range documents {
		value.Documents = append(value.Documents, hashDocument{Path: document.path, Fingerprint: reviewDocumentFingerprint(document.inspected)})
	}
	sort.Slice(value.Document, func(i, j int) bool { return value.Document[i].Locator < value.Document[j].Locator })
	sort.Slice(value.Node, func(i, j int) bool { return value.Node[i].Locator < value.Node[j].Locator })
	encoded, _ := json.Marshal(value)
	return sha256Hex(encoded)
}

func hashManifest(documents []SnapshotDocument) string {
	var builder strings.Builder
	builder.WriteString("knowledge-manifest-v1\n")
	for _, document := range documents {
		builder.WriteString(document.Path)
		builder.WriteByte('|')
		builder.WriteString(document.Revision.DocumentID)
		builder.WriteByte('|')
		builder.WriteString(document.Revision.ID)
		builder.WriteByte('\n')
	}
	return sha256Hex([]byte(builder.String()))
}

func revisionID(revision *KnowledgeRevision) *string {
	if revision == nil {
		return nil
	}
	value := revision.ID
	return &value
}

func sameOptionalID(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
