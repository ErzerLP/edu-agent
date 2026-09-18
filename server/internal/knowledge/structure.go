package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// 结构协议独立于旧 CLI 的严格 DTO；旧概念和评估继续引用原修订。
type ConceptSource struct {
	CollectionID string `json:"collection_id"`
	RevisionID   string `json:"revision_id"`
	DocumentID   string `json:"document_id"`
	NodeID       string `json:"node_id"`
	Quote        string `json:"quote"`
}
type ConceptClaim struct {
	Text       string `json:"text"`
	Conditions string `json:"conditions"`
	Sources    []int  `json:"sources"`
	Gap        string `json:"gap"`
}
type ConceptRelation struct {
	TargetID  string `json:"target_id"`
	Kind      string `json:"kind"`
	Suggested bool   `json:"suggested"`
	Sources   []int  `json:"sources"`
}
type ConceptContent struct {
	Description  string            `json:"description"`
	SourceStatus string            `json:"source_status"`
	Suggested    bool              `json:"suggested"`
	Sources      []ConceptSource   `json:"sources"`
	Claims       []ConceptClaim    `json:"claims"`
	Relations    []ConceptRelation `json:"relations"`
	ReplacedBy   []string          `json:"replaced_by"`
}
type StructureNode struct {
	ConceptRevision
	GoalID        string         `json:"goal_id"`
	Content       ConceptContent `json:"content"`
	LearningState string         `json:"learning_state"`
}
type StructureQuery struct {
	GoalID string
	RootID string
	Search string
	Cursor string
	Limit  int
}
type StructureEdge struct {
	SourceID string `json:"source_id"`
	ConceptRelation
}
type StructurePage struct {
	Version    int64           `json:"version"`
	Generation int64           `json:"generation"`
	Items      []StructureNode `json:"items"`
	Edges      []StructureEdge `json:"edges"`
	NextCursor string          `json:"next_cursor"`
	Partial    bool            `json:"partial"`
	Notice     string          `json:"notice"`
}
type StructureEdit struct {
	ConceptID string         `json:"concept_id"`
	GoalID    string         `json:"goal_id"`
	Name      string         `json:"name"`
	Content   ConceptContent `json:"content"`
}
type StructureCommand struct {
	OperationID   string          `json:"operation_id"`
	BaseVersion   int64           `json:"base_version"`
	Generation    int64           `json:"generation"`
	Kind          string          `json:"kind"`
	Reason        string          `json:"reason"`
	Edits         []StructureEdit `json:"edits"`
	Compensates   string          `json:"compensates,omitempty"`
	ActorDeviceID string          `json:"-"`
}
type StructureImpact struct {
	GoalIDs     []string `json:"goal_ids"`
	Contexts    int64    `json:"contexts"`
	Activities  int64    `json:"activities"`
	Contents    int64    `json:"contents"`
	Evidence    int64    `json:"evidence"`
	Fingerprint string   `json:"fingerprint"`
}
type StructureProposal struct {
	ID             string          `json:"id"`
	OperationID    string          `json:"operation_id"`
	Status         string          `json:"status"`
	Kind           string          `json:"kind"`
	Reason         string          `json:"reason"`
	BaseVersion    int64           `json:"base_version"`
	Generation     int64           `json:"generation"`
	Hash           string          `json:"hash"`
	Before         []StructureNode `json:"before"`
	After          []StructureNode `json:"after"`
	Impact         StructureImpact `json:"impact"`
	CurrentImpact  StructureImpact `json:"current_impact"`
	Compensates    string          `json:"compensates"`
	AppliedVersion int64           `json:"applied_version"`
	CreatedAt      time.Time       `json:"created_at"`
	DecisionReason string          `json:"decision_reason"`
	Replayed       bool            `json:"replayed"`
}
type StructureDecision struct {
	OperationID   string `json:"operation_id"`
	Hash          string `json:"hash"`
	Decision      string `json:"decision"`
	Reason        string `json:"reason"`
	ActorDeviceID string `json:"-"`
}
type StructureProposalPage struct {
	Items      []StructureProposal `json:"items"`
	NextCursor string              `json:"next_cursor"`
}
type StructureStore interface {
	ReadStructure(context.Context, StructureQuery) (StructurePage, error)
	ReadConcept(context.Context, string, string) (StructureNode, error)
	CreateStructureProposal(context.Context, StructureCommand) (StructureProposal, error)
	ReadStructureProposal(context.Context, string) (StructureProposal, error)
	ListStructureProposals(context.Context, string, int) (StructureProposalPage, error)
	DecideStructureProposal(context.Context, string, StructureDecision) (StructureProposal, error)
}

func StructureDigest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func structureText(v string, max int, required bool) bool {
	return utf8.ValidString(v) && len(v) <= max && !strings.ContainsRune(v, 0) && (!required || strings.TrimSpace(v) != "")
}
func (c StructureCommand) Validate() error {
	invalid := &Error{Code: CodeInvalidRequest}
	if !validUUID(c.OperationID) || !validUUID(c.ActorDeviceID) || c.BaseVersion < 0 || c.Generation < 1 || !structureText(c.Reason, 2000, true) {
		return invalid
	}
	if c.Kind == "compensate" {
		if !validUUID(c.Compensates) || len(c.Edits) != 0 {
			return invalid
		}
		return nil
	}
	if c.Compensates != "" || len(c.Edits) < 1 || len(c.Edits) > 20 {
		return invalid
	}
	if c.Kind != "edit" && c.Kind != "merge" && c.Kind != "split" {
		return invalid
	}
	seen := map[string]bool{}
	for _, e := range c.Edits {
		if !validUUID(e.ConceptID) || seen[e.ConceptID] || (e.GoalID != "" && !validUUID(e.GoalID)) || !structureText(e.Name, 300, true) {
			return invalid
		}
		seen[e.ConceptID] = true
		v := e.Content
		if !structureText(v.Description, 8000, false) || len(v.Sources) > 16 || len(v.Claims) > 16 || len(v.Relations) > 40 || len(v.ReplacedBy) > 20 {
			return invalid
		}
		switch v.SourceStatus {
		case "candidate", "included", "unverified", "conflict", "superseded":
		default:
			return invalid
		}
		if (v.SourceStatus == "superseded") != (len(v.ReplacedBy) > 0) {
			return invalid
		}
		for _, source := range v.Sources {
			if !validUUID(source.CollectionID) || !validUUID(source.RevisionID) || !validUUID(source.DocumentID) || !validUUID(source.NodeID) || !structureText(source.Quote, 2000, true) {
				return invalid
			}
		}
		indexes := func(ids []int) bool {
			if len(ids) > 16 {
				return false
			}
			for _, n := range ids {
				if n < 0 || n >= len(v.Sources) {
					return false
				}
			}
			return true
		}
		for _, claim := range v.Claims {
			if !structureText(claim.Text, 2000, true) || !structureText(claim.Conditions, 2000, false) || !structureText(claim.Gap, 2000, false) || !indexes(claim.Sources) || len(claim.Sources) == 0 && claim.Gap == "" {
				return invalid
			}
		}
		if v.SourceStatus == "conflict" && len(v.Claims) < 2 {
			return invalid
		}
		for _, edge := range v.Relations {
			if !validUUID(edge.TargetID) || edge.TargetID == e.ConceptID || !indexes(edge.Sources) {
				return invalid
			}
			switch edge.Kind {
			case "prerequisite", "related", "contrast", "part_of":
			default:
				return invalid
			}
		}
		for _, id := range v.ReplacedBy {
			if !validUUID(id) || id == e.ConceptID {
				return invalid
			}
		}
	}
	return nil
}

func (s *Service) SupportsStructure() bool { _, ok := s.store.(StructureStore); return ok }
func (s *Service) structureStore() (StructureStore, error) {
	v, ok := s.store.(StructureStore)
	if !ok {
		return nil, &Error{Code: CodeNotFound}
	}
	return v, nil
}
func (s *Service) ReadStructure(ctx context.Context, q StructureQuery) (StructurePage, error) {
	if q.Limit == 0 {
		q.Limit = 30
	}
	if q.Limit < 1 || q.Limit > 100 || !structureText(q.Search, 200, false) || q.GoalID != "" && !validUUID(q.GoalID) || q.RootID != "" && !validUUID(q.RootID) || len(q.Cursor) > 2000 {
		return StructurePage{}, &Error{Code: CodeInvalidRequest}
	}
	st, err := s.structureStore()
	if err != nil {
		return StructurePage{}, err
	}
	return st.ReadStructure(ctx, q)
}
func (s *Service) ReadConcept(ctx context.Context, id, revision string) (StructureNode, error) {
	if !validUUID(id) || revision != "" && !validUUID(revision) {
		return StructureNode{}, &Error{Code: CodeInvalidRequest}
	}
	st, err := s.structureStore()
	if err != nil {
		return StructureNode{}, err
	}
	return st.ReadConcept(ctx, id, revision)
}
func (s *Service) CreateStructureProposal(ctx context.Context, c StructureCommand) (StructureProposal, error) {
	if err := c.Validate(); err != nil {
		return StructureProposal{}, err
	}
	st, err := s.structureStore()
	if err != nil {
		return StructureProposal{}, err
	}
	return st.CreateStructureProposal(ctx, c)
}
func (s *Service) ReadStructureProposal(ctx context.Context, id string) (StructureProposal, error) {
	if !validUUID(id) {
		return StructureProposal{}, &Error{Code: CodeInvalidRequest}
	}
	st, err := s.structureStore()
	if err != nil {
		return StructureProposal{}, err
	}
	return st.ReadStructureProposal(ctx, id)
}
func (s *Service) ListStructureProposals(ctx context.Context, cursor string, limit int) (StructureProposalPage, error) {
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 || cursor != "" && !validUUID(cursor) {
		return StructureProposalPage{}, &Error{Code: CodeInvalidRequest}
	}
	st, err := s.structureStore()
	if err != nil {
		return StructureProposalPage{}, err
	}
	return st.ListStructureProposals(ctx, cursor, limit)
}
func (s *Service) DecideStructureProposal(ctx context.Context, id string, c StructureDecision) (StructureProposal, error) {
	if !validUUID(id) || !validUUID(c.OperationID) || !validUUID(c.ActorDeviceID) || len(c.Hash) != 64 || !structureText(c.Reason, 2000, true) || (c.Decision != "approve" && c.Decision != "reject") {
		return StructureProposal{}, &Error{Code: CodeInvalidRequest}
	}
	st, err := s.structureStore()
	if err != nil {
		return StructureProposal{}, err
	}
	return st.DecideStructureProposal(ctx, id, c)
}
