package agentcontroller

import "github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"

func openWorkspaceForClient(path string, readBytes int64) (*workspace.Workspace, error) {
	limits := workspace.DefaultLimits()
	if readBytes != 0 {
		limits.ReadFileBytes = readBytes
	}
	return workspace.OpenWithLimits(path, limits)
}
