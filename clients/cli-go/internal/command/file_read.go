package command

import (
	"errors"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

// This client-process budget is deliberately separate from file mutation,
// arguments, tool output, and persisted Session settings.
func parseFileReadOptions(args []string) ([]string, int64, error) {
	limit := workspace.DefaultReadFileBytes
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
		if name != "--file-read-limit" {
			remaining = append(remaining, arg)
			continue
		}
		if seen {
			return nil, limit, errors.New("duplicate file read budget")
		}
		seen = true
		if !hasValue {
			if i+1 >= len(args) {
				return nil, limit, errors.New("missing file read budget")
			}
			i++
			value = args[i]
		}
		n, err := strconv.ParseInt(value, 10, strconv.IntSize)
		if err != nil || n < 1 || n >= int64(^uint(0)>>1) {
			return nil, limit, errors.New("file read budget must be a positive bounded byte count")
		}
		limit = n
	}
	return remaining, limit, nil
}
