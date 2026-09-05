package workspace

import (
	"context"
	"unicode"
	"unicode/utf8"
)

const maxSearchProgressReports = 64

// Progress is bounded, presentation-safe workspace progress. It contains only
// normalized relative paths and aggregate counters; it never contains tool
// arguments, hashes, file contents, or operating-system errors.
type Progress struct {
	DestinationPath  string
	Tool             string
	Path             string
	Operation        string
	Returned         int
	StartLine        int
	EndLine          int
	Bytes            int64
	ScannedFiles     int
	ScannedBytes     int64
	Matches          int
	TruncationReason string
	NextOffset       int
	NextByteOffset   int
	HasContinuation  bool
}

type progressReporter func(Progress)
type progressReporterContextKey struct{}

// InitialProgress returns a bounded, validated presentation for a workspace
// call before filesystem work begins. Invalid or unsafe arguments produce no
// detail rather than exposing raw model input.
func InitialProgress(toolName, rawArguments string) (Progress, bool) {
	progress := Progress{Tool: toolName}
	var path string
	var err error
	switch toolName {
	case ToolFind:
		args, decodeErr := decodeFindArguments(rawArguments)
		if decodeErr != nil {
			return Progress{}, false
		}
		if args.Path == "" {
			args.Path = "."
		}
		path, err = normalizeModelPath(args.Path, true)
	case ToolStat:
		var args statArguments
		if decodeArguments(rawArguments, &args) != nil {
			return Progress{}, false
		}
		path, err = normalizeModelPath(args.Path, true)
	case ToolList:
		var args listArguments
		if decodeArguments(rawArguments, &args) != nil {
			return Progress{}, false
		}
		if args.Path == "" {
			args.Path = "."
		}
		path, err = normalizeModelPath(args.Path, true)
	case ToolRead:
		var args readArguments
		if decodeArguments(rawArguments, &args) != nil {
			return Progress{}, false
		}
		path, err = normalizeModelPath(args.Path, false)
		if args.Offset == 0 {
			args.Offset = 1
		}
		progress.StartLine = args.Offset
	case ToolSearch:
		args, decodeErr := decodeSearchArguments(rawArguments)
		if decodeErr != nil {
			return Progress{}, false
		}
		if args.Path == "" {
			args.Path = "."
		}
		path, err = normalizeModelPath(args.Path, true)
	case ToolWrite:
		var args writeArguments
		if decodeArguments(rawArguments, &args) != nil {
			return Progress{}, false
		}
		path, err = normalizeModelPath(args.Path, false)
		progress.Operation = "write_" + args.Mode
	case ToolCopy, ToolMove:
		args, decodeErr := decodeCopyArguments(rawArguments)
		if decodeErr != nil {
			return Progress{}, false
		}
		path, err = normalizeModelPath(args.Source, false)
		if err != nil {
			return Progress{}, false
		}
		progress.DestinationPath, err = normalizeModelPath(args.Destination, false)
		progress.Operation = toolName
	case ToolMkdir:
		args, decodeErr := decodeMkdirArguments(rawArguments)
		if decodeErr != nil {
			return Progress{}, false
		}
		path, err = normalizeModelPath(args.Path, false)
		progress.Operation = ToolMkdir
	case ToolArchive:
		var args archiveArguments
		if decodeArguments(rawArguments, &args) != nil {
			return Progress{}, false
		}
		path, err = normalizeModelPath(args.Path, false)
		progress.Operation = ToolArchive
	case ToolEdit:
		var args editArguments
		if decodeArguments(rawArguments, &args) != nil {
			return Progress{}, false
		}
		path, err = normalizeModelPath(args.Path, false)
		progress.Operation = "edit"
	default:
		return Progress{}, false
	}
	if err != nil {
		return Progress{}, false
	}
	progress.Path = path
	if !safeProgress(progress) {
		return Progress{}, false
	}
	return progress, true
}

// WithProgressReporter attaches a presentation-only progress sink to one
// workspace operation. Reporter panics are isolated from workspace execution.
func WithProgressReporter(ctx context.Context, reporter func(Progress)) context.Context {
	if ctx == nil || reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, progressReporterContextKey{}, progressReporter(reporter))
}

func publishProgress(ctx context.Context, progress Progress) {
	if ctx == nil || ctx.Err() != nil || !safeProgress(progress) {
		return
	}
	reporter, ok := ctx.Value(progressReporterContextKey{}).(progressReporter)
	if !ok || reporter == nil {
		return
	}
	defer func() {
		recover()
	}()
	reporter(progress)
}

func safeProgress(progress Progress) bool {
	if !IsReadTool(progress.Tool) && !IsMutationTool(progress.Tool) {
		return false
	}
	if progress.Tool == ToolCopy || progress.Tool == ToolMove {
		destination, err := normalizeModelPath(progress.DestinationPath, false)
		if err != nil || destination != progress.DestinationPath {
			return false
		}
		for _, r := range destination {
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
				return false
			}
		}
	}
	allowRoot := progress.Tool == ToolFind || progress.Tool == ToolStat || progress.Tool == ToolList || progress.Tool == ToolSearch
	if progress.Operation != "" && progress.Operation != "write_create" && progress.Operation != "write_replace" && progress.Operation != "edit" && progress.Operation != ToolArchive && progress.Operation != ToolMkdir && progress.Operation != ToolCopy && progress.Operation != ToolMove {
		return false
	}
	normalized, err := normalizeModelPath(progress.Path, allowRoot)
	if err != nil || normalized != progress.Path || progress.Path == "" || len(progress.Path) > 4096 || !utf8.ValidString(progress.Path) {
		return false
	}
	for _, current := range progress.Path {
		if unicode.IsControl(current) || unicode.In(current, unicode.Cf) {
			return false
		}
	}
	for _, current := range progress.TruncationReason {
		if current >= 'a' && current <= 'z' || current >= '0' && current <= '9' || current == '_' || current == '-' {
			continue
		}
		return false
	}
	return len(progress.TruncationReason) <= 64 && progress.Returned >= 0 && progress.StartLine >= 0 && progress.EndLine >= 0 && progress.Bytes >= 0 &&
		progress.ScannedFiles >= 0 && progress.ScannedBytes >= 0 && progress.Matches >= 0 && progress.NextOffset >= 0 && progress.NextByteOffset >= 0
}

type searchProgressEmitter struct {
	ctx         context.Context
	scope       string
	reports     int
	lastFiles   int
	lastBytes   int64
	lastMatches int
}

func newSearchProgressEmitter(ctx context.Context, scope string) *searchProgressEmitter {
	return &searchProgressEmitter{ctx: ctx, scope: scope}
}

func (e *searchProgressEmitter) initial() {
	if e == nil {
		return
	}
	e.publish(searchState{}, false, true)
}

func (e *searchProgressEmitter) maybe(state searchState) {
	if e == nil || e.ctx.Err() != nil || e.reports >= maxSearchProgressReports-1 {
		return
	}
	firstScannedFile := e.lastFiles == 0 && state.scannedFiles > 0
	if !firstScannedFile && state.scannedFiles-e.lastFiles < 32 &&
		state.scannedBytes-e.lastBytes < 256<<10 && state.matchesCount()-e.lastMatches < 16 {
		return
	}
	e.publish(state, false, false)
}

func (e *searchProgressEmitter) final(state searchState) {
	if e == nil || e.ctx.Err() != nil {
		return
	}
	e.publish(state, true, true)
}

func (e *searchProgressEmitter) publish(state searchState, final, force bool) {
	if e == nil || e.ctx.Err() != nil || !force && e.reports >= maxSearchProgressReports-1 {
		return
	}
	progress := Progress{
		Tool: ToolSearch, Path: e.scope, Returned: state.matchesCount(),
		ScannedFiles: state.scannedFiles, ScannedBytes: state.scannedBytes, Matches: state.matchesCount(),
	}
	if final && !state.complete {
		progress.TruncationReason = state.reason
	}
	publishProgress(e.ctx, progress)
	if e.ctx.Err() != nil {
		return
	}
	e.reports++
	e.lastFiles = state.scannedFiles
	e.lastBytes = state.scannedBytes
	e.lastMatches = state.matchesCount()
}

func (s searchState) matchesCount() int {
	switch s.output {
	case "files":
		return len(s.files)
	case "count":
		return s.matchedLines
	default:
		return len(s.matches)
	}
}
