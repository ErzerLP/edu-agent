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
	if options.WorkspaceCopyBytes != 0 {
		limits.CopyBytes = options.WorkspaceCopyBytes
	}
	if options.WorkspaceCopyPlanBytes != 0 {
		limits.CopyPlanBytes = options.WorkspaceCopyPlanBytes
	}
	if options.WorkspaceCopyEntries != 0 {
		limits.CopyEntries = options.WorkspaceCopyEntries
	}
	if options.WorkspaceQueryMemoryBytes != 0 {
		limits.QueryMemoryBytes = options.WorkspaceQueryMemoryBytes
	}
	if options.WorkspaceQueryEntries != 0 {
		limits.QueryEntries = options.WorkspaceQueryEntries
	}
	if options.WorkspaceQueryRecords != 0 {
		limits.QueryRecords = options.WorkspaceQueryRecords
	}
	return workspace.OpenWithLimits(path, limits)
}
