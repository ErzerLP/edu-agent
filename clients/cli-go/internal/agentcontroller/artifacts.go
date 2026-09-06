package agentcontroller

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
)

type fileArtifactStore struct{ handle *agentsession.Handle }

func fileArtifactError(err error) error {
	if err == nil {
		return nil
	}
	code := "artifact_store_failed"
	switch {
	case errors.Is(err, agentsession.ErrStoreFull):
		code = "artifact_limit"
	case errors.Is(err, agentsession.ErrCorrupt):
		code = "artifact_corrupt"
	case errors.Is(err, agentsession.ErrVersionUnsupported):
		code = "artifact_version_unsupported"
	case errors.Is(err, agentsession.ErrNotFound), errors.Is(err, agentsession.ErrPrivacyInvalidated), errors.Is(err, agentsession.ErrKeyUnavailable):
		code = "artifact_unavailable"
	}
	return &localartifact.Error{Code: code}
}
func (s fileArtifactStore) ReadArtifact(ctx context.Context, name string) ([]byte, error) {
	data, err := s.handle.ReadArtifact(ctx, name)
	return data, fileArtifactError(err)
}
func (s fileArtifactStore) WriteArtifact(ctx context.Context, name string, data []byte) error {
	return fileArtifactError(s.handle.WriteArtifact(ctx, name, data))
}
func (s fileArtifactStore) ListArtifacts(ctx context.Context, prefix string) ([]string, error) {
	names, err := s.handle.ListArtifacts(ctx, prefix)
	return names, fileArtifactError(err)
}

// Bind only after target preflight/commit. No Store operation calls Controller
// back, and immutable artifacts have no background writers or process lease.
func (c *Controller) bindArtifactsLocked() {
	if c.artifacts == nil || !c.persistent || c.handle == nil {
		return
	}
	c.artifactErr = ""
	if err := c.artifacts.Bind(c.artifactOwner, fileArtifactStore{c.handle}); err != nil {
		c.artifactErr = "artifact_unavailable"
		var stable *localartifact.Error
		if errors.As(err, &stable) {
			c.artifactErr = stable.Code
		}
		c.appendStatusNoticeLocked("[artifact_unavailable] 完整差异/逐项结果历史无法安全加载；不会把它当空历史，也不会自动应用旧修改。")
	}
}
func (c *Controller) ArtifactHistoryStatus() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.artifactErr
}
func (c *Controller) artifactBinding() (*localartifact.Manager, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.switching || c.artifacts == nil {
		return nil, "", &localartifact.Error{Code: "artifact_unavailable"}
	}
	if c.artifactErr != "" {
		return nil, "", &localartifact.Error{Code: c.artifactErr}
	}
	return c.artifacts, c.artifactOwner, nil
}
func (c *Controller) LocalArtifacts() ([]localartifact.Info, error) {
	m, owner, err := c.artifactBinding()
	if err != nil {
		return nil, err
	}
	return m.List(owner), nil
}
func (c *Controller) ReadLocalArtifact(ctx context.Context, id string, offset int64, limit int) (localartifact.Page, error) {
	m, owner, err := c.artifactBinding()
	if err != nil {
		return localartifact.Page{}, err
	}
	return m.Read(ctx, owner, id, offset, limit)
}
func (c *Controller) SearchLocalArtifact(ctx context.Context, id, needle string, offset int64, limit int) (localartifact.SearchPage, error) {
	m, owner, err := c.artifactBinding()
	if err != nil {
		return localartifact.SearchPage{}, err
	}
	return m.Search(ctx, owner, id, needle, offset, limit)
}
