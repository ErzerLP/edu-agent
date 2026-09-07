package workspace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func purgeWorkspaceFixture(t *testing.T) (*Workspace, string, string, string) {
	t.Helper()
	root := t.TempDir()
	path := ArchiveDirectory + "/selected"
	if err := os.MkdirAll(filepath.Join(root, path, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.bin", "z.bin"} {
		if err := os.WriteFile(filepath.Join(root, path, name), []byte("purge-body\x00\xff"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	version := w.Execute(t.Context(), ToolStat, `{"path":"`+path+`"}`).Value.(map[string]any)["entry_version"].(string)
	raw, _ := json.Marshal(map[string]any{"path": path, "expected_version": version})
	return w, root, path, string(raw)
}

func TestArchivePurgeWorkspacePlanAndSettlement(t *testing.T) {
	for _, mode := range []string{"completed", "cancel_after_item", "changed", "tampered", "no_observer"} {
		t.Run(mode, func(t *testing.T) {
			w, root, path, raw := purgeWorkspaceFixture(t)
			for _, bad := range []string{`null`, `{}`, strings.Replace(raw, `"path":`, `"Path":`, 1), strings.TrimSuffix(raw, "}") + `,"recursive":true}`, strings.TrimSuffix(raw, "}") + `,"path":"other"}`, strings.Replace(raw, `"path":"`+path+`"`, `"path":null`, 1)} {
				if p, r := w.PrepareMutation(t.Context(), ToolPurgeArchive, bad); p != nil || resultCode(t, r) != CodeInvalidArguments {
					t.Fatal(bad, r)
				}
			}
			if r := w.Execute(t.Context(), ToolPurgeArchive, raw); r.Publication == PublicationCompleted {
				t.Fatal("execute bypassed confirmation")
			}
			p, r := w.PrepareMutation(t.Context(), ToolPurgeArchive, raw)
			if p == nil {
				t.Fatal(r)
			}
			if !p.IsPurgeArchive() || p.FileEffect().Validate() != nil || !p.FileEffect().IsArchivePurge() || p.FullDiff() != "" || !strings.Contains(p.PurgeManifest(), path+"/z.bin") || strings.Contains(p.PurgeManifest(), "purge-body") {
				t.Fatal(p)
			}
			if r := w.CommitMutation(t.Context(), p); r.Publication != PublicationUnchanged || r.Effect != nil {
				t.Fatal("unguarded commit", r)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			attempts, settled := 0, 0
			observer := securefile.PurgeObserver{Before: func(context.Context, int, securefile.PurgeItem) error { attempts++; return nil }, After: func(ctx context.Context, _ int, _ securefile.PurgeItem, _ securefile.PurgeItemResult) error {
				if ctx.Err() != nil {
					t.Fatal("settlement inherited cancellation")
				}
				settled++
				if mode == "cancel_after_item" {
					cancel()
				}
				return nil
			}}
			switch mode {
			case "changed":
				if err := os.WriteFile(filepath.Join(root, path, "new"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "tampered":
				p.Presentation.Preview = "different"
			case "no_observer":
				observer = securefile.PurgeObserver{}
			}
			r, actual := w.CommitPurge(ctx, p, observer)
			if mode == "completed" || mode == "cancel_after_item" {
				value := r.Value.(map[string]any)
				if r.Effect == nil || !r.Effect.IsArchivePurge() || r.Reference == nil || r.Reference.ContentHash != "" || !r.Reference.InvalidateObserved || value["physical_bytes_reclaimed"] != "unknown" || attempts != settled {
					t.Fatal(r, actual)
				}
				if mode == "completed" {
					if r.Publication != PublicationCompleted || actual.Completed != 4 || value["logical_bytes_removed"] != int64(24) {
						t.Fatal(r, actual)
					}
					if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
						t.Fatal(err)
					}
				} else if r.Publication != PublicationUnknown || actual.Completed != 1 || attempts != 1 {
					t.Fatal(r, actual)
				}
			} else if r.Publication != PublicationUnchanged || r.Effect != nil || attempts != 0 {
				t.Fatal(r, actual, attempts)
			}
			if _, err := os.Stat(filepath.Join(root, ArchiveDirectory)); err != nil {
				t.Fatal("archive root removed", err)
			}
			if again, _ := w.CommitPurge(t.Context(), p, observer); again.Publication != PublicationUnchanged {
				t.Fatal("plan replayed", again)
			}
		})
	}
}
