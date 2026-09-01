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
