package agentcontroller

import "github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"

func openWorkspaceForClient(path string, readBytes, editBytes int64) (*workspace.Workspace, error) {
	limits := workspace.DefaultLimits()
	if readBytes != 0 {
		limits.ReadFileBytes = readBytes
	}
	if editBytes != 0 {
		limits.EditFileBytes = editBytes
	}
	return workspace.OpenWithLimits(path, limits)
}
