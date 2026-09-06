package command

import (
	"errors"
	"strconv"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type fileQueryOptions struct {
	MemoryBytes      int64
	Entries, Records int
}

func parseFileQueryOptions(args []string) ([]string, fileQueryOptions, error) {
	result := fileQueryOptions{MemoryBytes: workspace.DefaultQueryMemoryBytes, Entries: workspace.DefaultQueryEntries, Records: workspace.DefaultQueryRecords}
	var err error
	args, result.MemoryBytes, err = parseFileByteOption(args, "--file-query-memory-limit", result.MemoryBytes)
	if err != nil {
		return nil, result, err
	}
	var entries, records int64
	args, entries, err = parseFileByteOption(args, "--file-query-entry-limit", int64(result.Entries))
	if err != nil {
		return nil, result, err
	}
	args, records, err = parseFileByteOption(args, "--file-query-max-records", int64(result.Records))
	if err != nil {
		return nil, result, err
	}
	if result.MemoryBytes > 1<<30 || entries > 1000000 || records > 64 {
		return nil, result, errors.New("query resource ceiling exceeded")
	}
	result.Entries, result.Records = int(entries), int(records)
	return args, result, nil
}

func (o fileQueryOptions) arguments() []string {
	return []string{"--file-query-memory-limit", strconv.FormatInt(o.MemoryBytes, 10), "--file-query-entry-limit", strconv.Itoa(o.Entries), "--file-query-max-records", strconv.Itoa(o.Records)}
}
