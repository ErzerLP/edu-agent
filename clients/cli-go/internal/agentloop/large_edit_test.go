package agentloop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestLargeFileEditModelAuthorizationAndSettlement(t *testing.T) {
	for _, mode := range []string{"approve", "decline", "cancel_pending", "wal_failure", "changed", "model_failure", "yolo"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "large.txt")
			original := "\ufeffMARK_A\r\n" + strings.Repeat("unchanged-padding\r\n", 70000) + "MARK_B\r\n"
			want := strings.Replace(strings.Replace(original, "MARK_A", "UPDATED_A", 1), "MARK_B", "UPDATED_B", 1)
			if err := os.WriteFile(path, []byte(original), 0640); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(original))
			hash := "sha256:" + hex.EncodeToString(digest[:])
			raw, _ := json.Marshal(map[string]any{"path": "large.txt", "expected_hash": hash, "edits": []map[string]string{{"old_text": "MARK_A", "new_text": "UPDATED_A"}, {"old_text": "MARK_B", "new_text": "UPDATED_B"}}})
			w, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("large-edit", workspace.ToolEdit, string(raw))}, {Message: modelclient.Message{Role: "assistant", Content: "已编辑"}}}}
			sink := &durabilitySink{}
			if mode == "wal_failure" {
				sink.fileErr = errors.New("WAL unavailable")
			}
			session := newDurableTestSession(t, model, &fakeServer{}, w, sink)
			defer session.Close()
			if mode == "yolo" {
				if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
					t.Fatal(err)
				}
			}
			var activities []Activity
			ctx := WithActivityReporter(t.Context(), func(a Activity) { activities = append(activities, a) })
			result, err := session.Send(ctx, "修改大文件的两处标记")
			if err != nil {
				t.Fatal(err)
			}
			if mode != "yolo" {
				if result.PendingFileMutation == nil || result.PendingFileMutation.BaseVersion != hash {
					t.Fatalf("missing approval/version: %+v", result)
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != original {
					t.Fatal("changed before approval")
				}
				if mode == "changed" {
					original += "external change\r\n"
					if err := os.WriteFile(path, []byte(original), 0640); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "model_failure" {
					model.err = errors.New("model unavailable after publication")
				}
				if mode == "cancel_pending" {
					result, err = session.CancelPendingFileMutation("large-edit")
				} else {
					decision := FileMutationApprove
					if mode == "decline" {
						decision = FileMutationDecline
					}
					result, err = session.ResolveFileMutation(ctx, "large-edit", decision)
				}
				if mode == "wal_failure" {
					if err == nil {
						t.Fatal("WAL failure bypassed")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			} else if result.PendingFileMutation != nil {
				t.Fatal("YOLO unexpectedly asked for approval")
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			changed := mode == "approve" || mode == "model_failure" || mode == "yolo"
			if changed {
				if string(data) != want || len(sink.files) != 1 || sink.files[0].Effect.Operation != "edit" {
					t.Fatal("large edit or WAL did not match approved plan")
				}
				visible := false
				for _, a := range activities {
					if a.Event.ID == "large-edit" && a.Event.Status == EventSucceeded {
						visible = true
					}
				}
				if !visible {
					t.Fatal("client-visible completion missing")
				}
				checkpoint, err := session.ExportCheckpoint()
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := EncodeSessionCheckpoint(checkpoint)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(encoded), strings.Repeat("unchanged-padding\\r\\n", 5000)) {
					t.Fatal("full source/candidate entered checkpoint")
				}
			} else if string(data) != original {
				t.Fatalf("%s changed file", mode)
			}
		})
	}
}
