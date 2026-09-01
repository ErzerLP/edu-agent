package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceSearchProgressIsBoundedSafeAndCancellationStopsReporting(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	for index := range 96 {
		name := filepath.Join(root, "src", "file-"+intString(index)+".txt")
		if err := os.WriteFile(name, []byte(strings.Repeat("content ", 64)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	value, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()

	var progress []Progress
	ctx := WithProgressReporter(t.Context(), func(current Progress) {
		progress = append(progress, current)
	})
	result := value.Execute(ctx, ToolSearch, `{"query":"missing","path":"src"}`)
	if code := resultCode(t, result); code != "" {
		t.Fatalf("search code=%q value=%+v", code, result.Value)
	}
	if len(progress) < 3 || len(progress) > maxSearchProgressReports {
		t.Fatalf("progress events=%d values=%+v", len(progress), progress)
	}
	last := progress[len(progress)-1]
	if last.Tool != ToolSearch || last.Path != "src" || last.ScannedFiles != 96 || last.ScannedBytes <= 0 || last.Matches != 0 {
		t.Fatalf("final progress=%+v", last)
	}
	for _, current := range progress {
		if current.Path != "src" || strings.Contains(current.Path, root) || current.ScannedFiles < 0 || current.ScannedBytes < 0 {
			t.Fatalf("unsafe progress=%+v root=%q", current, root)
		}
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancelReports := 0
	cancelled = WithProgressReporter(cancelled, func(Progress) {
		cancelReports++
		cancel()
	})
	cancelledResult := value.Execute(cancelled, ToolSearch, `{"query":"missing","path":"src"}`)
	if code := resultCode(t, cancelledResult); code != CodeCancelled || cancelReports != 1 {
		t.Fatalf("cancel code=%q reports=%d value=%+v", code, cancelReports, cancelledResult.Value)
	}
}

func TestWorkspaceReadProgressStartsBeforeIOAndUpdatesInPlace(t *testing.T) {
	root := t.TempDir()
	content := strings.Repeat("line content\n", 64)
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()

	var progress []Progress
	ctx := WithProgressReporter(t.Context(), func(current Progress) {
		progress = append(progress, current)
	})
	result := value.Execute(ctx, ToolRead, `{"path":"notes.md","offset":3,"limit":4}`)
	if code := resultCode(t, result); code != "" {
		t.Fatalf("read code=%q value=%+v", code, result.Value)
	}
	if len(progress) < 3 {
		t.Fatalf("read progress=%+v", progress)
	}
	first, inspected, final := progress[0], progress[1], progress[len(progress)-1]
	if first.Tool != ToolRead || first.Path != "notes.md" || first.StartLine != 3 || first.Bytes != 0 {
		t.Fatalf("initial progress=%+v", first)
	}
	if inspected.Path != "notes.md" || inspected.Bytes != int64(len(content)) || inspected.StartLine != 3 {
		t.Fatalf("inspected progress=%+v", inspected)
	}
	if final.Path != "notes.md" || final.StartLine != 3 || final.EndLine < 3 || final.Bytes <= 0 {
		t.Fatalf("final progress=%+v", final)
	}
}

func TestWorkspaceInitialProgressValidatesAllFiveToolPaths(t *testing.T) {
	tests := []struct {
		tool, raw, path, operation string
		start                      int
	}{
		{ToolList, `{}`, ".", "", 0},
		{ToolRead, `{"path":"notes.md","offset":7}`, "notes.md", "", 7},
		{ToolSearch, `{"query":"needle","path":"src"}`, "src", "", 0},
		{ToolWrite, `{"path":"notes.md","mode":"replace","content":"new","expected_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, "notes.md", "write_replace", 0},
		{ToolEdit, `{"path":"notes.md","expected_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","edits":[{"old_text":"old","new_text":"new"}]}`, "notes.md", "edit", 0},
	}
	for _, test := range tests {
		t.Run(test.tool, func(t *testing.T) {
			progress, ok := InitialProgress(test.tool, test.raw)
			if !ok || progress.Path != test.path || progress.Operation != test.operation || progress.StartLine != test.start {
				t.Fatalf("progress=%+v ok=%v", progress, ok)
			}
		})
	}
	for _, raw := range []string{`null`, `{"path":"../outside"}`, `{"path":"notes.md","unknown":true}`} {
		if progress, ok := InitialProgress(ToolRead, raw); ok {
			t.Fatalf("unsafe initial progress=%+v raw=%q", progress, raw)
		}
	}
}

func intString(value int) string {
	if value == 0 {
		return "0"
	}
	var buffer [24]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}
