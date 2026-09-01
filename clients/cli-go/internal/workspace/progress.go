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
	Tool             string
	Path             string
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
		_ = recover()
	}()
	reporter(progress)
}

func safeProgress(progress Progress) bool {
	if progress.Tool != ToolList && progress.Tool != ToolRead && progress.Tool != ToolSearch {
		return false
	}
	normalized, err := normalizeModelPath(progress.Path, true)
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
	return len(s.matches)
}
