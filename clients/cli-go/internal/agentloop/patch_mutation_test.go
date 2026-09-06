package agentloop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type patchTestSink struct {
	durabilitySink
	order      []string
	after      func(int) error
	beforeFail int
	settled    int
}

func (s *patchTestSink) BeforeFilePublication(ctx context.Context, wal FileWriteAhead) error {
	s.order = append(s.order, "before:"+wal.ToolCallID)
	if err := s.durabilitySink.BeforeFilePublication(ctx, wal); err != nil {
		return err
	}
	if len(s.files) == s.beforeFail {
		return errors.New("WAL refused")
	}
	return nil
}
func (s *patchTestSink) AfterFilePublication(_ context.Context, id string, _ workspace.Result) error {
	s.order = append(s.order, "after:"+id)
	s.settled++
	if s.after != nil {
		return s.after(s.settled)
	}
	return nil
}
func patchTestHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestFilePatchModelApprovalAndPerItemSettlement(t *testing.T) {
	for _, mode := range []string{"approve", "decline", "cancel_pending", "changed", "changed_second", "wal_second", "settle_first", "cancel_after_first", "retention_failure", "model_failure", "yolo"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			a, b := "old-a\n", "old-b\n"
			for name, text := range map[string]string{"a.txt": a, "b.txt": b} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			newBody := "PRIVATE_NEW_BODY_" + strings.Repeat("x", 10000)
			patch := "*** Begin Patch\n*** Update File: a.txt\n@@\n-old-a\n+new-a\n*** Update File: b.txt\n@@\n-old-b\n+new-b\n*** Add File: new.txt\n+" + newBody + "\n*** End Patch"
			args, _ := json.Marshal(map[string]any{"patch": patch, "expected_hashes": map[string]string{"a.txt": patchTestHash(a), "b.txt": patchTestHash(b)}})
			w, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("patch-outer", workspace.ToolPatch, string(args))}, {Message: modelclient.Message{Role: "assistant", Content: "done"}}}}
			sink := &patchTestSink{}
			session := newDurableTestSession(t, model, &fakeServer{}, w, sink)
			defer session.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "wal_second" {
				sink.beforeFail = 2
			}
			sink.after = func(index int) error {
				if index != 1 {
					return nil
				}
				switch mode {
				case "changed_second":
					return os.WriteFile(filepath.Join(root, "b.txt"), []byte("external\n"), 0600)
				case "settle_first":
					return errors.New("settlement failed")
				case "cancel_after_first":
					cancel()
				}
				return nil
			}
			if mode == "retention_failure" {
				session.options.Artifacts = localartifact.New(localartifact.Options{MaxArtifactBytes: 16})
			}
			if mode == "yolo" {
				if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
					t.Fatal(err)
				}
			}
			result, err := session.Send(ctx, "apply the approved patch")
			if err != nil {
				t.Fatal(err)
			}
			if mode != "yolo" && mode != "retention_failure" {
				pending := result.PendingFileMutation
				if pending == nil || pending.DiffID == "" || pending.DiffBytes <= 8192 {
					t.Fatalf("missing complete diff before authorization: %+v", pending)
				}
				if len(sink.files) != 0 {
					t.Fatal("WAL/file changes before approval")
				}
				var full strings.Builder
				for offset := int64(0); ; {
					page, err := session.options.Artifacts.Read(ctx, session.options.ArtifactOwner, pending.DiffID, offset, 4096)
					if err != nil {
						t.Fatal(err)
					}
					full.Write(page.Data)
					offset = page.NextOffset
					if !page.More {
						break
					}
				}
				if !strings.Contains(full.String(), newBody) {
					t.Fatal("diff not fully retained")
				}
				if mode == "changed" {
					if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("external\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "model_failure" {
					model.err = errors.New("provider failed")
				}
				if mode == "cancel_pending" {
					_, err = session.CancelPendingFileMutation("patch-outer")
				} else {
					decision := FileMutationApprove
					if mode == "decline" {
						decision = FileMutationDecline
					}
					result, err = session.ResolveFileMutation(ctx, "patch-outer", decision)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			all := mode == "approve" || mode == "yolo" || mode == "model_failure"
			partial := mode == "changed_second" || mode == "wal_second" || mode == "settle_first" || mode == "cancel_after_first"
			wantA, wantB := a, b
			if all || partial {
				wantA = "new-a\n"
			}
			if all {
				wantB = "new-b\n"
			}
			if mode == "changed" {
				wantA = "external\n"
			}
			if mode == "changed_second" {
				wantB = "external\n"
			}
			for name, want := range map[string]string{"a.txt": wantA, "b.txt": wantB} {
				data, err := os.ReadFile(filepath.Join(root, name))
				if err != nil || string(data) != want {
					t.Fatalf("%s=%q, want=%q err=%v", name, data, want, err)
				}
			}
			data, readErr := os.ReadFile(filepath.Join(root, "new.txt"))
			if all {
				if readErr != nil || string(data) != newBody+"\n" {
					t.Fatal("last file not fully applied")
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatal("remaining item was started")
			}
			for i := 0; i+1 < len(sink.order); i += 2 {
				if strings.TrimPrefix(sink.order[i], "before:") != strings.TrimPrefix(sink.order[i+1], "after:") {
					t.Fatalf("unpaired WAL/settlement: %v", sink.order)
				}
			}
			if all && len(sink.files) != 3 {
				t.Fatalf("WAL count=%d", len(sink.files))
			}
			if partial && !strings.Contains(result.Text, "1/3") {
				t.Fatalf("partial fallback lied: %+v", result)
			}
			if all || partial || mode == "changed" {
				var receipt patchReceipt
				found := false
				for _, info := range session.options.Artifacts.List(session.options.ArtifactOwner) {
					if info.Kind != "receipt" {
						continue
					}
					page, err := session.options.Artifacts.Read(t.Context(), session.options.ArtifactOwner, info.ID, 0, 65536)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(page.Data, &receipt); err != nil {
						t.Fatal(err)
					}
					found = true
				}
				if !found || len(receipt.Items) != 3 {
					t.Fatalf("receipt missing: %+v", receipt)
				}
				if partial && (receipt.Completed != 1 || receipt.Items[2].Outcome != "not_started") {
					t.Fatalf("false receipt: %+v", receipt)
				}
				if receipt.Items[0].ExpectedVersion != patchTestHash(a) {
					t.Fatal("frozen input version lost from complete receipt")
				}
				if (all || partial) && receipt.Items[0].ResultVersion != patchTestHash("new-a\n") {
					t.Fatal("actual published version lost from complete receipt")
				}
				if mode == "settle_first" && receipt.Items[0].PersistenceError != "file_settlement_unsaved" {
					t.Fatal("settlement failure confused with publication outcome")
				}
			}
			if all || partial {
				checkpoint, err := session.ExportCheckpoint()
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := EncodeSessionCheckpoint(checkpoint)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(encoded), "PRIVATE_NEW_BODY_") || strings.Contains(string(encoded), "*** Begin Patch") {
					t.Fatal("raw patch/full diff entered stable history")
				}
			}
		})
	}
}
