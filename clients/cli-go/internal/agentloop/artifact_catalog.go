package agentloop

import (
	"context"
	"sort"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
)

// ArtifactCatalog is a read-only view captured for one Session/owner. It keeps
// immutable results and append-only batch journals in their separate stores;
// b_ offsets refer to raw log bytes, never a changing folded receipt table.
// Capturing all fields together prevents a Session switch from mixing owners.
type ArtifactCatalog struct {
	Artifacts     *localartifact.Manager
	Batches       *fileeffects.BatchManager
	Owner         string
	ArtifactError string
	BatchError    string
}

func (c ArtifactCatalog) batchAvailable() error {
	if c.BatchError != "" {
		return &localartifact.Error{Code: c.BatchError}
	}
	if c.Batches == nil {
		return &localartifact.Error{Code: "artifact_unavailable"}
	}
	return nil
}
func (c ArtifactCatalog) List(ctx context.Context) ([]localartifact.Info, error) {
	if c.ArtifactError != "" {
		return nil, &localartifact.Error{Code: c.ArtifactError}
	}
	var result []localartifact.Info
	if c.Artifacts != nil {
		result = append(result, c.Artifacts.List(c.Owner)...)
	}
	if c.BatchError != "" {
		return nil, &localartifact.Error{Code: c.BatchError}
	}
	if c.Batches != nil {
		items, err := c.Batches.List(ctx, c.Owner)
		if err != nil {
			return nil, err
		}
		result = append(result, items...)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
func (c ArtifactCatalog) Read(ctx context.Context, id string, offset int64, limit int) (localartifact.Page, error) {
	if strings.HasPrefix(id, "b_") {
		if err := c.batchAvailable(); err != nil {
			return localartifact.Page{}, err
		}
		return c.Batches.Read(ctx, c.Owner, id, offset, limit)
	}
	if c.ArtifactError != "" {
		return localartifact.Page{}, &localartifact.Error{Code: c.ArtifactError}
	}
	if c.Artifacts == nil {
		return localartifact.Page{}, &localartifact.Error{Code: "artifact_unavailable"}
	}
	return c.Artifacts.Read(ctx, c.Owner, id, offset, limit)
}
func (c ArtifactCatalog) Search(ctx context.Context, id, needle string, offset int64, limit int) (localartifact.SearchPage, error) {
	if strings.HasPrefix(id, "b_") {
		if err := c.batchAvailable(); err != nil {
			return localartifact.SearchPage{}, err
		}
		return c.Batches.Search(ctx, c.Owner, id, needle, offset, limit)
	}
	if c.ArtifactError != "" {
		return localartifact.SearchPage{}, &localartifact.Error{Code: c.ArtifactError}
	}
	if c.Artifacts == nil {
		return localartifact.SearchPage{}, &localartifact.Error{Code: "artifact_unavailable"}
	}
	return c.Artifacts.Search(ctx, c.Owner, id, needle, offset, limit)
}

func (s *Session) artifactCatalog() ArtifactCatalog {
	catalog := ArtifactCatalog{Artifacts: s.options.Artifacts, Batches: s.options.FileBatches, Owner: s.options.ArtifactOwner}
	if status, ok := s.options.Durability.(interface{ ArtifactHistoryStatus() string }); ok {
		catalog.ArtifactError = status.ArtifactHistoryStatus()
	}
	if status, ok := s.options.Durability.(interface{ FileBatchHistoryStatus() string }); ok {
		catalog.BatchError = status.FileBatchHistoryStatus()
	}
	return catalog
}
