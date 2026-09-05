package command

import (
	"errors"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

// Resource flags apply to this client process (including resumed Sessions), not
// to shell command permissions or execution duration. Strip only known flags;
// the existing command parser remains authoritative for everything else.
func parseLocalExecutionOptions(args []string) ([]string, localexec.Options, error) {
	options := localexec.Options{
		MaxTasks: localexec.DefaultMaxTasks, MaxConcurrent: localexec.DefaultMaxConcurrent,
		OutputBytesPerTask: localexec.DefaultOutputBytesPerTask, OutputBytesTotal: localexec.DefaultOutputBytesTotal,
	}
	values := map[string]*int{
		"--task-max-records":        &options.MaxTasks,
		"--task-max-running":        &options.MaxConcurrent,
		"--task-output-limit":       &options.OutputBytesPerTask,
		"--task-total-output-limit": &options.OutputBytesTotal,
	}
	remaining := make([]string, 0, len(args))
	seen := make(map[string]bool, len(values))
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
		target, known := values[name]
		if !known {
			remaining = append(remaining, arg)
			continue
		}
		if seen[name] {
			return nil, options, errors.New("duplicate local task resource flag")
		}
		seen[name] = true
		if !hasValue {
			if i+1 >= len(args) {
				return nil, options, errors.New("missing local task resource value")
			}
			i++
			value = args[i]
		}
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return nil, options, errors.New("local task resources must be positive integers (output sizes in bytes)")
		}
		*target = n
	}
	if options.MaxConcurrent > options.MaxTasks {
		return nil, options, errors.New("concurrent task limit exceeds task record limit")
	}
	return remaining, options, nil
}
