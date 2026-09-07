package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func TestRecursiveCopyRequiresObservedCommitAndFrozenPresentation(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		t.Run(fmt.Sprintf("tamper=%t", tamper), func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "source"), 0700); err != nil {
				t.Fatal(err)
			}
			w, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			stat := w.Execute(t.Context(), ToolStat, `{"path":"source"}`)
			raw, _ := json.Marshal(copyArguments{Source: "source", Destination: "target", ExpectedVersion: stat.Value.(map[string]any)["entry_version"].(string)})
			p, failure := w.PrepareMutation(t.Context(), ToolCopy, string(raw))
			if p == nil || !p.IsCopyTree() || p.CopyManifest() == "" {
				t.Fatal("missing frozen plan", failure)
			}
			var result Result
			if tamper {
				p.Presentation.DestinationPath = "another"
				result, _ = w.CommitCopyTree(t.Context(), p, securefile.CopyTreeObserver{})
			} else {
				result = w.CommitMutation(t.Context(), p)
			}
			if result.Publication != PublicationUnchanged {
				t.Fatal("unsafe direct publication", result)
			}
			if _, err := os.Stat(filepath.Join(root, "target")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unobserved copy changed target", err)
			}
		})
	}
}

func TestRecursiveCopyProductionFileExceedsLegacyLimit(t *testing.T) {
	root := t.TempDir()
	data := bytes.Repeat([]byte{0, 0xff, 0x42, 0x17}, (33<<20)/4)
	if err := os.WriteFile(filepath.Join(root, "source.bin"), data, 0640); err != nil {
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	stat := w.Execute(t.Context(), ToolStat, `{"path":"source.bin"}`)
	raw, _ := json.Marshal(copyArguments{Source: "source.bin", Destination: "target.bin", ExpectedVersion: stat.Value.(map[string]any)["entry_version"].(string)})
	p, failure := w.PrepareMutation(t.Context(), ToolCopy, string(raw))
	if p == nil {
		t.Fatal("production kept old 32MiB ceiling", failure)
	}
	result := w.CommitMutation(t.Context(), p)
	if result.Publication != PublicationCompleted || result.Reference == nil || result.Reference.ContentHash != fmt.Sprintf("sha256:%x", sha256.Sum256(data)) {
		t.Fatal("wrong publication/hash", result)
	}
	got, err := os.ReadFile(filepath.Join(root, "target.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("copy lost bytes", err)
	}
}
