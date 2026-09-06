package agentcontroller

import (
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func openWorkspaceForClient(path string, options agentloop.Options) (*workspace.Workspace, error) {
	limits := workspace.DefaultLimits()
	if options.WorkspaceReadFileBytes != 0 {
		limits.ReadFileBytes = options.WorkspaceReadFileBytes
	}
	if options.WorkspaceEditFileBytes != 0 {
		limits.EditFileBytes = options.WorkspaceEditFileBytes
	}
	if options.WorkspaceDiffBytes != 0 {
		limits.DiffBytes = options.WorkspaceDiffBytes
	}
	if options.WorkspacePatchBytes != 0 {
		limits.PatchBytes = options.WorkspacePatchBytes
	}
	return workspace.OpenWithLimits(path, limits)
}
