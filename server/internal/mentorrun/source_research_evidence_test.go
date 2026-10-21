package mentorrun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestSourceResearchRejectsNonBodyEvidence(t *testing.T) {
	for _, scope := range []string{"title_or_contents", "insufficient"} {
		t.Run(scope, func(t *testing.T) {
			f := researchCoverageFixture(t, 1, 1, func(string) []string { return []string{"http://example.org/problemset"} }, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, "<title>TCP exercises</title><main>Table of contents CHAPTER 3: TCP congestion window evolution. CHAPTER 4: IP routing.</main>")
			})
			next := f.modelReply
			f.modelReply = func(w http.ResponseWriter, r *http.Request, call int) bool {
				raw, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(strings.NewReader(string(raw)))
				var req modelclient.Request
				if err := json.Unmarshal(raw, &req); err != nil {
					t.Error(err)
					return true
				}
				if !strings.Contains(req.Messages[0].Content, "source_research_screen") {
					return next(w, r, call)
				}
				// Even an inconsistent include=true cannot promote an explicitly
				// non-body scope into a recommendation or report evidence.
				out, _ := json.Marshal(map[string]any{"include": true, "evidence_scope": scope, "reason": "列有TCP拥塞窗口练习", "quality_notes": "仅见目录，未读到题干或解答", "candidate": 1})
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": modelclient.Message{Role: "assistant", Content: string(out)}, "finish_reason": "stop"}}})
				return true
			}
			job := completeSourceResearch(t, f)
			page, err := f.service.SourceResearchCandidates(context.Background(), f.actor, learningspace.DefaultID, job.ID, "", 20)
			if err != nil || job.Status != "completed" || len(page.Items) != 0 {
				t.Fatalf("non-body evidence was not safely excluded: status=%s candidates=%+v err=%v", job.Status, page.Items, err)
			}
			report, err := f.service.SourceResearchReport(context.Background(), f.actor, learningspace.DefaultID, job.ID, job.ReportRevisionID, 0, 5)
			if err != nil || len(report.Sections) != 1 || len(report.Sections[0].Citations) != 0 || !strings.Contains(strings.Join(report.Sections[0].Gaps, " "), "正文") || strings.Contains(report.Sections[0].Body, "解答自查") {
				t.Fatalf("report recommended unread exercises or lost the gap: %+v %v", report, err)
			}
		})
	}
}

func TestSourceResearchReportRequiresBodyScope(t *testing.T) {
	for _, scope := range []string{"", "title_or_contents", "body"} {
		t.Run("scope="+scope, func(t *testing.T) {
			f := researchCoverageFixture(t, 1, 1, func(string) []string { return nil }, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				fmt.Fprint(w, "TCP练习：初始拥塞窗口为十个报文段，一次丢包后减半，求下一轮窗口。")
			})
			next := f.modelReply
			f.modelReply = func(w http.ResponseWriter, r *http.Request, call int) bool {
				raw, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(strings.NewReader(string(raw)))
				var req modelclient.Request
				if err := json.Unmarshal(raw, &req); err != nil {
					t.Error(err)
					return true
				}
				if strings.Contains(req.Messages[0].Content, "source_research_report") {
					var input struct {
						Evidence []struct {
							Scope string `json:"evidence_scope"`
							Notes string `json:"quality_notes"`
						} `json:"evidence"`
					}
					if json.Unmarshal([]byte(req.Messages[1].Content), &input) != nil || scope != "body" || len(input.Evidence) != 1 || input.Evidence[0].Scope != "body" || !strings.Contains(input.Evidence[0].Notes, "未核验解答") {
						t.Errorf("report received unsupported evidence or lost usage limits: %s", req.Messages[1].Content)
					}
				}
				return next(w, r, call)
			}
			ctx := context.Background()
			if _, err := f.service.CreateSourceResearch(ctx, f.actor, learningspace.DefaultID, SourceResearchCreate{OperationID: uuid.NewString(), Request: "TCP拥塞窗口练习"}); err != nil {
				t.Fatal(err)
			}
			owned, err := f.service.claim(ctx)
			if err != nil {
				t.Fatal(err)
			}
			h := &executionHost{service: f.service, owned: *owned, body: owned.body, limits: f.settings.View().Limits}
			direction := SourceResearchDirection{ID: "direction-1", Title: "TCP练习"}
			h.body.SourceResearch.Directions = []SourceResearchDirection{direction}
			r, err := h.registerWebResource(ctx, "http://example.org/exercise", "TCP练习", "", "search")
			if err != nil {
				t.Fatal(err)
			}
			value, err := h.sourceResearchTool(ctx, "read_web", map[string]string{"resource_id": r.ID})
			if err != nil {
				t.Fatal(err)
			}
			d := value.(research.WebDocument)
			_, support, actual, err := h.sourceResearchEvidence(ctx, []string{d.ID})
			if err != nil {
				t.Fatal(err)
			}
			d = actual[d.ID]
			proof, err := d.Cite(support[0].FragmentID, support[0].Quote, h.body.Web.Supplied[d.ID])
			if err != nil {
				t.Fatal(err)
			}
			id := uuid.NewString()
			if err = f.service.mutateTx(ctx, &h.owned, "", func(tx pgx.Tx, item *row) error {
				raw, err := f.service.sealWeb("source_research_candidates", id, item.Generation, map[string]any{"id": id, "document_id": d.ID, "document_revision_id": d.RevisionID, "title": "TCP练习", "url": d.URL, "support": proof, "evidence_scope": scope, "quality_notes": "已读完整题干，未核验解答"})
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `INSERT INTO learning_source_research_candidates(id,job_id,round_id,privacy_generation,document_id,rank,ciphertext) VALUES($1,$2,$3,$4,$5,0,$6)`, id, item.SourceResearchJobID, item.SourceResearchRoundID, item.Generation, d.ID, raw)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err = h.reportSourceResearch(ctx, direction, []string{d.ID}); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			var sectionID string
			if err = f.pool.QueryRow(ctx, `SELECT id::text,ciphertext FROM learning_source_research_sections WHERE job_id=$1`, owned.SourceResearchJobID).Scan(&sectionID, &raw); err != nil {
				t.Fatal(err)
			}
			var section SourceResearchReportSection
			if err = f.service.openWeb("source_research_sections", sectionID, owned.Generation, raw, &section); err != nil {
				t.Fatal(err)
			}
			want := 0
			if scope == "body" {
				want = 1
			}
			if len(section.Citations) != want {
				t.Fatalf("report cited unverified body scope: %+v", section)
			}
		})
	}
}
