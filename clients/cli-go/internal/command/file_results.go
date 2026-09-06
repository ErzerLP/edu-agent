package command

import (
	"errors"
	"strconv"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type fileResultOptions struct {
	DiffBytes, PatchBytes int64
	Artifacts             localartifact.Options
}

func parseFileResultOptions(args []string) ([]string, fileResultOptions, error) {
	result := fileResultOptions{DiffBytes: workspace.DefaultDiffBytes, PatchBytes: workspace.DefaultPatchBytes,
		Artifacts: localartifact.Options{MaxArtifactBytes: localartifact.DefaultMaxArtifactBytes, MemoryBytes: localartifact.DefaultMemoryBytes, MaxRecords: localartifact.DefaultMaxRecords}}
	var err error
	for _, option := range []struct {
		name   string
		target *int64
	}{{"--file-diff-limit", &result.DiffBytes}, {"--file-patch-limit", &result.PatchBytes}, {"--artifact-memory-limit", &result.Artifacts.MemoryBytes}} {
		args, *option.target, err = parseFileByteOption(args, option.name, *option.target)
		if err != nil {
			return nil, result, err
		}
	}
	var records int64
	args, records, err = parseFileByteOption(args, "--artifact-max-records", int64(result.Artifacts.MaxRecords))
	if err != nil {
		return nil, result, err
	}
	if result.DiffBytes > 1<<30 || result.Artifacts.MemoryBytes > 1<<30 || records > 8192 {
		return nil, result, errors.New("artifact resource ceiling exceeded")
	}
	result.Artifacts.MaxRecords = int(records)
	result.Artifacts.MaxArtifactBytes = result.DiffBytes
	return args, result, nil
}
func (o fileResultOptions) arguments() []string {
	return []string{"--file-diff-limit", strconv.FormatInt(o.DiffBytes, 10), "--file-patch-limit", strconv.FormatInt(o.PatchBytes, 10), "--artifact-memory-limit", strconv.FormatInt(o.Artifacts.MemoryBytes, 10), "--artifact-max-records", strconv.Itoa(o.Artifacts.MaxRecords)}
}
