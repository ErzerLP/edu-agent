//go:build !windows

package command

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func TestImportWizardPTYScanPreviewConfirm(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	for _, paste := range []bool{false, true} {
		t.Run(map[bool]string{false: "Markdown", true: "粘贴原文"}[paste], func(t *testing.T) {
			name := "go.md"
			if paste {
				name = "粘贴文本.md"
			}
			collection := "00000000-0000-4000-8000-000000000002"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/learning-spaces/capabilities":
					json.NewEncoder(w).Encode(api.LearningSpaceCapabilities{Version: 1, DefaultSpaceID: api.DefaultLearningSpaceID, LegacyScope: "fixed_default", Modules: map[string]string{"knowledge": "collections_v1", "learning": "default_only", "tutoring": "default_only", "memory": "default_only"}})
				case "/v1/knowledge/collections":
					json.NewEncoder(w).Encode(map[string]any{"items": []api.KnowledgeCollection{{ID: collection, Name: "Go 教材", Source: "测试", Version: 1}}})
				case "/v1/knowledge/revisions/head":
					w.WriteHeader(404)
					json.NewEncoder(w).Encode(api.ErrorResponse{Error: api.ErrorBody{Code: "not_found", Message: "empty", RequestID: "test"}})
				case "/v1/knowledge/imports/previews":
					var request api.ImportRequest
					if json.NewDecoder(r.Body).Decode(&request) != nil {
						w.WriteHeader(400)
						return
					}
					json.NewEncoder(w).Encode(api.ImportPreview{Status: "ready", Receipt: "fixture-receipt", Summary: api.ImportSummary{OperationID: request.OperationID, SpaceID: api.DefaultLearningSpaceID, CollectionID: collection, ActorDeviceID: testDeviceID, Added: 1, DocumentIDs: []string{testDocID}}, Diff: []api.KnowledgeMaintenanceDocumentDiff{}})
				case "/v1/knowledge/imports/confirm":
					var request api.ConfirmImportRequest
					if json.NewDecoder(r.Body).Decode(&request) != nil {
						w.WriteHeader(400)
						return
					}
					if len(request.Request.Documents) != 1 || request.Request.Documents[0].Path != name || request.Receipt != "fixture-receipt" {
						w.WriteHeader(400)
						return
					}
					json.NewEncoder(w).Encode(api.ImportResult{Revision: testRevision(), Summary: &api.ImportSummary{OperationID: request.Request.OperationID, SpaceID: api.DefaultLearningSpaceID, CollectionID: collection, ActorDeviceID: testDeviceID, Added: 1, DocumentIDs: []string{testDocID}}})
				default:
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			primary, terminal, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer primary.Close()
			defer terminal.Close()
			if err = pty.Setsize(primary, &pty.Winsize{Rows: 32, Cols: 120}); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(t.TempDir(), "go.md")
			if err = os.WriteFile(file, []byte("# Go\nchannel\n"), 0600); err != nil {
				t.Fatal(err)
			}
			app, _, _ := newTestApp(nil, nil, nil)
			app.teachingInput, app.teachingOutput = terminal, terminal
			app.learningSpace = api.DefaultLearningSpaceID
			app.learningSpaceName = "Go 后端"
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- app.runImportWizard(ctx, api.NewClient(server.URL, "test", time.Second, nil), collection, file)
			}()
			chunks := make(chan string, 64)
			go func() {
				buffer := make([]byte, 4096)
				for {
					n, err := primary.Read(buffer)
					if n > 0 {
						select {
						case chunks <- string(buffer[:n]):
						case <-ctx.Done():
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
			output := ""
			wait := func(text string) {
				t.Helper()
				for !strings.Contains(output, text) {
					select {
					case chunk := <-chunks:
						output += chunk
					case err := <-done:
						t.Fatalf("向导提前退出：%v，输出 %q", err, output)
					case <-ctx.Done():
						t.Fatalf("未显示 %q，输出 %q", text, output)
					}
				}
			}
			wait("来源路径")
			if paste {
				io.WriteString(primary, "\x1bOS")
				wait("Ctrl+S 加入清单")
				io.WriteString(primary, "\x1b[200~# 原文\n```\n不改写\x1b[201~\x13")
			} else {
				io.WriteString(primary, "\r")
			}
			wait("Enter 服务端检查")
			io.WriteString(primary, "\r")
			wait("Ctrl+S 确认")
			io.WriteString(primary, "\x13")
			wait("已提交版本")
			io.WriteString(primary, "\x1b")
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("Esc 未退出向导")
			}
			if !strings.Contains(output, "\x1b[?1049h") || app.importDrafts[api.DefaultLearningSpaceID].result == nil {
				t.Fatal("全屏或结果未保留")
			}
		})
	}
}
