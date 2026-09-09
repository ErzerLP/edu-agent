package knowledge

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

const DefaultCollectionID = "00000000-0000-4000-8000-000000000002"
const CollectionHeader = "X-Knowledge-Collection-ID"

func legacyKnowledgeScope(ctx context.Context) error {
	if CollectionID(ctx) != DefaultCollectionID {
		return &learningspace.Error{Code: "learning_space_module_unavailable"}
	}
	return learningspace.RequireLegacyModule(ctx)
}

type collectionKey struct{}

func HasCollection(ctx context.Context) bool { _, ok := ctx.Value(collectionKey{}).(string); return ok }

func WithCollection(ctx context.Context, id string) (context.Context, error) {
	if !learningspace.ValidID(id) {
		return nil, &Error{Code: CodeInvalidRequest}
	}
	return context.WithValue(ctx, collectionKey{}, id), nil
}

func CollectionID(ctx context.Context) string {
	if id, ok := ctx.Value(collectionKey{}).(string); ok {
		return id
	}
	return DefaultCollectionID
}

type Collection struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Source         string  `json:"source"`
	Shared         bool    `json:"shared"`
	HeadRevisionID *string `json:"head_revision_id"`
	Version        int64   `json:"version"`
}

type ScopeEntry struct {
	CollectionID string `json:"collection_id"`
	RevisionID   string `json:"revision_id"`
	DocumentID   string `json:"document_id,omitempty"`
	NodeID       string `json:"node_id,omitempty"`
}

type ScopeSnapshot struct {
	Updates []ScopeEntry `json:"updates,omitempty"`
	ID      string       `json:"id"`
	SpaceID string       `json:"space_id"`
	Entries []ScopeEntry `json:"entries"`
}

type CollectionCommand struct {
	ID              string `json:"id"`
	Action          string `json:"action"`
	Name            string `json:"name"`
	Source          string `json:"source"`
	Shared          bool   `json:"shared"`
	ExpectedVersion int64  `json:"expected_version"`
}

type ScopeStore interface {
	Collections(context.Context, bool) ([]Collection, error)
	ChangeCollection(context.Context, CollectionCommand) (Collection, error)
	FreezeScope(context.Context, ScopeSnapshot) (ScopeSnapshot, error)
	ReadScope(context.Context, string) (ScopeSnapshot, error)
}

func (s *Service) SupportsKnowledgeScopes() bool { _, ok := s.store.(ScopeStore); return ok }

func (s *Service) scopeStore() (ScopeStore, error) {
	store, ok := s.store.(ScopeStore)
	if !ok {
		return nil, &learningspace.Error{Code: "learning_space_module_unavailable"}
	}
	return store, nil
}
func (s *Service) Collections(ctx context.Context, shared bool) ([]Collection, error) {
	store, err := s.scopeStore()
	if err != nil {
		return nil, err
	}
	return store.Collections(ctx, shared)
}
func (s *Service) ChangeCollection(ctx context.Context, c CollectionCommand) (Collection, error) {
	store, err := s.scopeStore()
	if err != nil {
		return Collection{}, err
	}
	return store.ChangeCollection(ctx, c)
}
func (s *Service) FreezeScope(ctx context.Context, c ScopeSnapshot) (ScopeSnapshot, error) {
	store, err := s.scopeStore()
	if err != nil {
		return ScopeSnapshot{}, err
	}
	return store.FreezeScope(ctx, c)
}
func (s *Service) ReadScope(ctx context.Context, id string) (ScopeSnapshot, error) {
	store, err := s.scopeStore()
	if err != nil {
		return ScopeSnapshot{}, err
	}
	return store.ReadScope(ctx, id)
}

// 未实现范围端口的旧适配器必须拒绝非默认区，不能把全库结果当作区内结果。
func (s *Service) checkScopeAdapter(ctx context.Context) error {
	if _, ok := s.store.(ScopeStore); ok {
		return nil
	}
	if CollectionID(ctx) != DefaultCollectionID {
		return &Error{Code: CodeNotFound}
	}
	return learningspace.RequireLegacyModule(ctx)
}
