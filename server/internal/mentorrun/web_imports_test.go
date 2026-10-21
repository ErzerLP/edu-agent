package mentorrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

func TestSourceResearchImportsLongTitles(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		for _, title := range []string{strings.Repeat("GitHub problem set ", 15), strings.Repeat("网络练习", 50)} {
			t.Run(fmt.Sprintf("concurrent=%t/unicode=%t", concurrent, utf8.RuneCountInString(title) != len(title)), func(t *testing.T) {
				f := researchCoverageFixture(t, 1, 2, func(string) []string {
					return []string{"http://example.org/rfc", "http://example.net/github"}
				}, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/html")
					if r.URL.Path == "/rfc" {
						fmt.Fprint(w, "<title>RFC</title><main>协议规范：发送端在拥塞时减小发送窗口。</main>")
					} else {
						fmt.Fprintf(w, "<title>%s</title><main>练习：初始窗口为十个报文，丢包后窗口应如何变化？请计算并解释。</main>", title)
					}
				})
				ctx := context.Background()
				job := completeSourceResearch(t, f)
				preview, err := f.service.PreviewSourceResearch(ctx, f.actor, learningspace.DefaultID, job.ID, SourceResearchPreviewRequest{OperationID: uuid.NewString(), ResultSnapshotID: job.ResultSnapshotID, ExpectedSelectionVersion: job.SelectionVersion})
				if err != nil {
					t.Fatal(err)
				}
				commands := []WebImportRequest{}
				for _, item := range preview.Items {
					if item.Kind == "document" {
						commands = append(commands, WebImportRequest{OperationID: item.OperationID, PreviewID: item.WebPreviewID, ExpectedVersion: 1, RevisionID: item.RevisionID, StorageConsent: true})
					}
				}
				if len(commands) != 2 {
					t.Fatalf("expected two document imports, got %d (status=%s)", len(commands), job.Status)
				}
				type outcome struct {
					receipt WebImportReceipt
					err     error
				}
				results := make(chan outcome, 2)
				start := make(chan struct{})
				for _, command := range commands {
					if concurrent {
						go func() {
							<-start
							r, e := f.service.ImportWebDocument(ctx, f.actor, learningspace.DefaultID, command)
							results <- outcome{r, e}
						}()
					} else {
						r, e := f.service.ImportWebDocument(ctx, f.actor, learningspace.DefaultID, command)
						results <- outcome{r, e}
					}
				}
				close(start)
				imported := map[string]WebImportReceipt{}
				for range commands {
					result := <-results
					if result.err != nil {
						t.Errorf("import failed: %v", result.err)
						continue
					}
					imported[result.receipt.OperationID] = result.receipt
					var name string
					var metadata []byte
					if err = f.pool.QueryRow(ctx, `SELECT c.name,m.metadata FROM knowledge_collections c JOIN knowledge_source_metadata m ON m.document_revision_id=$2 WHERE c.id=$1`, result.receipt.Source.CollectionID, result.receipt.Source.DocumentRevisionID).Scan(&name, &metadata); err != nil {
						t.Fatal(err)
					}
					var source struct{ Title string }
					if json.Unmarshal(metadata, &source) != nil || source.Title != "RFC" && source.Title != strings.TrimSpace(title) || name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 120 {
						t.Fatalf("collection name or original source title lost: name=%q metadata=%s", name, metadata)
					}
				}
				if t.Failed() {
					return
				}
				for _, command := range commands {
					replay, err := f.service.ImportWebDocument(ctx, f.actor, learningspace.DefaultID, command)
					if err != nil || replay != imported[command.OperationID] {
						t.Fatalf("import replay changed identity: %+v %v", replay, err)
					}
				}
				var count int
				if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_source_revisions`).Scan(&count); err != nil || count != 2 {
					t.Fatalf("expected exactly two durable sources: count=%d err=%v", count, err)
				}
			})
		}
	}
}
