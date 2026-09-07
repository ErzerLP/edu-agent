package command

import (
	"errors"
	"strconv"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type fileCopyOptions struct {
	Bytes, PlanBytes, JournalMemoryBytes int64
	Entries, JournalRecords              int
}

func parseFileCopyOptions(args []string) ([]string, fileCopyOptions, error) {
	result := fileCopyOptions{Bytes: workspace.DefaultCopyBytes, PlanBytes: workspace.DefaultCopyPlanBytes, Entries: workspace.DefaultCopyEntries, JournalMemoryBytes: localartifact.DefaultMemoryBytes, JournalRecords: localartifact.DefaultMaxRecords}
	entries, records := int64(result.Entries), int64(result.JournalRecords)
	budgets := []struct {
		name  string
		value *int64
	}{
		{"--file-copy-limit", &result.Bytes},
		{"--file-copy-plan-limit", &result.PlanBytes},
		{"--file-copy-entry-limit", &entries},
		{"--file-copy-journal-limit", &result.JournalMemoryBytes},
		{"--file-copy-max-records", &records},
	}
	for _, budget := range budgets {
		var err error
		args, *budget.value, err = parseFileByteOption(args, budget.name, *budget.value)
		if err != nil {
			return nil, result, err
		}
	}
	if result.PlanBytes > 1<<30 || result.JournalMemoryBytes > 1<<30 || entries > 1000000 || records > 8192 {
		return nil, result, errors.New("copy resource ceiling exceeded")
	}
	result.Entries, result.JournalRecords = int(entries), int(records)
	return args, result, nil
}

func (o fileCopyOptions) arguments() []string {
	return []string{
		"--file-copy-limit", strconv.FormatInt(o.Bytes, 10),
		"--file-copy-plan-limit", strconv.FormatInt(o.PlanBytes, 10),
		"--file-copy-entry-limit", strconv.Itoa(o.Entries),
		"--file-copy-journal-limit", strconv.FormatInt(o.JournalMemoryBytes, 10),
		"--file-copy-max-records", strconv.Itoa(o.JournalRecords),
	}
}
