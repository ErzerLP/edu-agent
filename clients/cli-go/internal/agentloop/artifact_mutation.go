package agentloop

import (
	"context"
	"crypto/rand"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func withArtifactDefaults(options Options) Options {
	if options.Artifacts == nil && options.Workspace != nil {
		options.Artifacts = localartifact.New(options.ArtifactOptions)
	}
	if options.Artifacts != nil && options.ArtifactOwner == "" {
		options.ArtifactOwner = "unsaved-" + rand.Text()
	}
	return options
}

// Retain the exact frozen diff before asking for authorization. Retention
// failure is a preparation failure, never permission to publish without it.
func (s *Session) retainMutationArtifact(ctx context.Context, callID string, prepared *workspace.PreparedMutation) (localartifact.Info, error) {
	if prepared.FullDiff() == "" {
		return localartifact.Info{}, nil
	}
	if status, ok := s.options.Durability.(interface{ ArtifactHistoryStatus() string }); ok {
		if code := status.ArtifactHistoryStatus(); code != "" {
			return localartifact.Info{}, &localartifact.Error{Code: code}
		}
	}
	if s.options.Artifacts == nil {
		return localartifact.Info{}, &localartifact.Error{Code: "artifact_unavailable"}
	}
	s.appendMu.Lock()
	_, exists := s.mutationArtifacts[callID]
	s.appendMu.Unlock()
	if exists {
		// A call ID cannot bind a newly prepared candidate to an older diff.
		return localartifact.Info{}, &localartifact.Error{Code: "artifact_invalid_arguments"}
	}
	info, err := s.options.Artifacts.Put(ctx, s.options.ArtifactOwner, "diff", []byte(prepared.FullDiff()))
	if err != nil {
		return localartifact.Info{}, err
	}
	s.appendMu.Lock()
	if s.mutationArtifacts == nil {
		s.mutationArtifacts = make(map[string]localartifact.Info)
	}
	s.mutationArtifacts[callID] = info
	s.appendMu.Unlock()
	return info, nil
}

func (s *Session) attachMutationArtifact(callID string, result workspace.Result) workspace.Result {
	s.appendMu.Lock()
	info, ok := s.mutationArtifacts[callID]
	s.appendMu.Unlock()
	if !ok {
		return result
	}
	value := make(map[string]any)
	if original, ok := result.Value.(map[string]any); ok {
		for key, item := range original {
			value[key] = item
		}
	}
	value["diff_id"], value["diff_bytes"], value["diff_saved"], value["diff_hash"] = info.ID, info.Bytes, info.Saved, info.Hash
	result.Value = value
	return result
}
