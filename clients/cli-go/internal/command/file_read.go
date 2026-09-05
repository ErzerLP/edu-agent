package command

import (
	"errors"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

// Client-process file budgets are separate from arguments, tool output and
// persisted Session settings; neither one changes structured path permissions.
func parseFileReadOptions(args []string) ([]string, int64, error) {
	return parseFileByteOption(args, "--file-read-limit", workspace.DefaultReadFileBytes)
}

func parseFileEditOptions(args []string) ([]string, int64, error) {
	return parseFileByteOption(args, "--file-edit-limit", workspace.DefaultEditFileBytes)
}

func parseFileByteOption(args []string, flagName string, limit int64) ([]string, int64, error) {
	remaining := make([]string, 0, len(args))
	seen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			remaining = append(remaining, args[i:]...)
			break
		}
		if arg == "--workspace" && i+1 < len(args) {
			remaining = append(remaining, arg, args[i+1])
			i++
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if name != flagName {
			remaining = append(remaining, arg)
			continue
		}
		if seen {
			return nil, limit, errors.New("duplicate file byte budget")
		}
		seen = true
		if !hasValue {
			if i+1 >= len(args) {
				return nil, limit, errors.New("missing file byte budget")
			}
			i++
			value = args[i]
		}
		n, err := strconv.ParseInt(value, 10, strconv.IntSize)
		if err != nil || n < 1 || n >= int64(^uint(0)>>1) {
			return nil, limit, errors.New("file budget must be a positive bounded byte count")
		}
		limit = n
	}
	return remaining, limit, nil
}
