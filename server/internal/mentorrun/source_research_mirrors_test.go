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

// Abbreviated RFC header, summary, contents and prose preserve the structure
// of both real snapshots; neither the site's title nor wrapper is the work.
const sourceResearchRFC1122 = `Network Working Group Internet Engineering Task Force
Request for Comments: 1122 R. Braden, Editor October 1989
Requirements for Internet Hosts -- Communication Layers
Status of This Memo
This RFC is an official specification for the Internet community. It
incorporates by reference, amends, corrects, and supplements the primary
protocol standards documents relating to hosts. Distribution of this
document is unlimited.
Summary
This is one RFC of a pair that defines and discusses the requirements for
Internet host software. This RFC covers the communications protocol layers:
link layer, IP layer, and transport layer; its companion RFC-1123 covers the
application and support protocols.
Table of Contents
1 . INTRODUCTION ................ 5
1.1 The Internet Architecture ... 6
1.1.1 Internet Hosts ............ 6
Internet Engineering Task Force [Page 1] RFC1122 INTRODUCTION October 1989
1.1.2 Architectural Assumptions
The Internet is a network of networks. Each host is directly connected to
some particular network. Two hosts on the same network communicate using
the same protocols that they use with hosts on distant networks.
Gateways forward IP datagrams independently. Redundant paths provide
robust service when an intervening gateway fails. Hosts implement the
state required for end-to-end flow control and reliability in transport
protocols and applications. Routing complexity belongs in gateways so
host software can remain independent of changes in routing architecture.
The system tolerates variation in bandwidth, delay, packet loss,
reordering, and maximum packet size across different networks.`

const sourceResearchRFC1123 = `Network Working Group Internet Engineering Task Force
Request for Comments: 1123 R. Braden, Editor October 1989
Requirements for Internet Hosts -- Application and Support
Status of This Memo
This RFC is an official specification for the Internet community.
Summary
This companion specification addresses application and support protocols.
Its discussion of network host requirements complements communication
layers without repeating their protocol rules.
Table of Contents
2. GENERAL APPLICATION REQUIREMENTS
3. REMOTE LOGIN
4. FILE TRANSFER
5. ELECTRONIC MAIL
6. SUPPORT SERVICES
Application software interprets user input and communicates with remote
services through transport connections. Remote login negotiates terminal
options and handles interactive input. File transfer separates control
commands from data connections, defines representations, and reports
failures to the client. Mail senders queue messages, retry temporary
delivery failures, and preserve envelope addresses. Domain name clients
cache answers for their permitted lifetime and distinguish authoritative
answers from referrals. These application requirements describe behavior
that is independent from IP datagram forwarding and gateway routing.`

func TestSourceResearchContentMirrorsKeepOneRepresentative(t *testing.T) {
	// The fixture routes HTTP to its local page server; host/path and body
	// structure are those of the real HTTPS pages.
	const tracker = "http://datatracker.ietf.org/doc/html/rfc1122"
	const editor = "http://www.rfc-editor.org/rfc/rfc1122.html"
	for _, urls := range [][]string{{tracker, editor}, {editor, tracker}} {
		t.Run(urls[0], func(t *testing.T) {
			f := researchCoverageFixture(t, 1, 2, func(string) []string { return urls }, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				if r.Host == "datatracker.ietf.org" {
					// Theme controls and anchors change the snapshot and fragment boundaries.
					fmt.Fprintf(w, "<title>RFC 1122 - Requirements for Internet Hosts - Communication Layers</title><main>Light Dark Auto <pre>%s</pre></main>", strings.ReplaceAll(sourceResearchRFC1122, "RFC-1123", "<a>RFC</a>-1123"))
				} else {
					fmt.Fprintf(w, "<pre>%s</pre>", sourceResearchRFC1122)
				}
			})
			observeSourceResearchReport(t, f, 1)
			job := completeSourceResearch(t, f)
			if job.Status != "completed" || job.CandidateCount != 1 || job.Progress.ReadSucceeded != 2 {
				t.Fatalf("mirrors consumed independent candidate slots: %+v", job)
			}
			candidates, err := f.service.SourceResearchCandidates(context.Background(), f.actor, learningspace.DefaultID, job.ID, "", 20)
			if err != nil || len(candidates.Items) != 1 {
				t.Fatalf("mirror candidates: %+v %v", candidates, err)
			}
			c := candidates.Items[0]
			if c.URL != tracker || c.Support == nil || c.Support.DocumentID != c.DocumentID || c.Support.RevisionID != c.RevisionID {
				t.Fatalf("readable representative or its verified citation was lost: %+v", c)
			}
			report, err := f.service.SourceResearchReport(context.Background(), f.actor, learningspace.DefaultID, job.ID, job.ReportRevisionID, 0, 5)
			if err != nil || len(report.Sections) != 1 || len(report.Sections[0].Citations) != 1 || report.Sections[0].Citations[0].Web.DocumentID != c.DocumentID {
				t.Fatalf("report treated mirrors as separate works: %+v %v", report, err)
			}
		})
	}
}

func TestSourceResearchRelatedWorksRemainIndependent(t *testing.T) {
	f := researchCoverageFixture(t, 1, 2, func(string) []string {
		return []string{"http://standards.example.org/rfc1122", "http://documents.example.net/rfc1123"}
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		text := sourceResearchRFC1122
		if strings.HasSuffix(r.URL.Path, "1123") {
			text = sourceResearchRFC1123
		}
		fmt.Fprintf(w, "<title>Internet host requirements</title><main>%s</main>", text)
	})
	observeSourceResearchReport(t, f, 2)
	job := completeSourceResearch(t, f)
	if job.Status != "completed" || job.CandidateCount != 2 {
		t.Fatalf("related specifications were merged: status=%s candidates=%d", job.Status, job.CandidateCount)
	}
	candidates, err := f.service.SourceResearchCandidates(context.Background(), f.actor, learningspace.DefaultID, job.ID, "", 20)
	if err != nil || len(candidates.Items) != 2 {
		t.Fatalf("independent candidates: %+v %v", candidates, err)
	}
}

func TestSourceResearchMirrorComparisonBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, a, b string
		want       bool
	}{
		{"formatting and Unicode", sourceResearchRFC1122, "Theme controls " + strings.NewReplacer("RFC", "ＲＦＣ", "Internet", "INTERNET", " ", " \n ").Replace(sourceResearchRFC1122), true},
		{"shared title and topic", sourceResearchRFC1122, sourceResearchRFC1123, false},
		{"different numbered specification", sourceResearchRFC1122, strings.ReplaceAll(sourceResearchRFC1122, "1122", "1123"), false},
		{"excerpt is not the whole work", sourceResearchRFC1122, sourceResearchRFC1122[:600], false},
		{"short shared boilerplate", "Light Dark Auto Internet protocol reference", "Internet protocol reference", false},
		{"empty comparison keys", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := research.WebDocument{Title: "Internet host requirements", Text: tc.a}
			b := research.WebDocument{Title: a.Title, Text: tc.b}
			if got := sourceResearchSameWork(a, b); got != tc.want {
				t.Fatalf("same work=%v, want %v", got, tc.want)
			}
		})
	}
	partial := research.WebDocument{URL: "https://mirror.org/doc", Title: "Readable title", Text: sourceResearchRFC1122, Coverage: "partial_text_limit"}
	complete := research.WebDocument{URL: "https://original.org/doc", Title: "https://original.org/doc", Text: sourceResearchRFC1122, Coverage: "complete_text"}
	if !sourceResearchPreferDocument(complete, partial) || sourceResearchPreferDocument(partial, complete) {
		t.Fatal("a better title displaced the complete text")
	}
}

func TestSourceResearchReportedRepresentativeStaysStable(t *testing.T) {
	f := researchCoverageFixture(t, 2, 2, func(query string) []string {
		if query == "direction0" {
			return []string{"http://mirror.org/rfc1122"}
		}
		return []string{"http://original.net/rfc1122"}
	}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.Host == "mirror.org" {
			fmt.Fprintf(w, "<pre>%s</pre>", sourceResearchRFC1122)
		} else {
			fmt.Fprintf(w, "<title>RFC 1122</title><main>Light Dark Auto %s</main>", sourceResearchRFC1122)
		}
	})
	observeSourceResearchReport(t, f, 1)
	job := completeSourceResearch(t, f)
	candidates, err := f.service.SourceResearchCandidates(context.Background(), f.actor, learningspace.DefaultID, job.ID, "", 20)
	if err != nil || len(candidates.Items) != 1 {
		t.Fatalf("candidates: %+v %v", candidates, err)
	}
	report, err := f.service.SourceResearchReport(context.Background(), f.actor, learningspace.DefaultID, job.ID, job.ReportRevisionID, 0, 5)
	if err != nil || len(report.Sections) != 2 {
		t.Fatalf("report: %+v %v", report, err)
	}
	for _, section := range report.Sections {
		if len(section.Citations) != 1 || section.Citations[0].Web.DocumentID != candidates.Items[0].DocumentID {
			t.Fatalf("later mirror changed the representative already cited by a saved report: %+v", section)
		}
	}
}

func TestSourceResearchReportDeduplicatesBeforeEvidenceLimit(t *testing.T) {
	// Simulate candidates saved by the old selector: the two highest-ranked
	// rows are mirrors, followed by an independent specification.
	f := researchCoverageFixture(t, 1, 3, func(string) []string { return nil }, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		body, title := sourceResearchRFC1122, "RFC 1122"
		if r.URL.Path == "/1123" {
			body, title = sourceResearchRFC1123, "RFC 1123"
		}
		if r.Host == "mirror.org" {
			fmt.Fprintf(w, "<pre>Light Dark Auto %s</pre>", body)
		} else {
			fmt.Fprintf(w, "<title>%s</title><main>%s</main>", title, body)
		}
	})
	observeSourceResearchReport(t, f, 2)
	next := f.modelReply
	f.modelReply = func(w http.ResponseWriter, r *http.Request, call int) bool {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return true
		}
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		var req modelclient.Request
		if err = json.Unmarshal(raw, &req); err != nil {
			t.Error(err)
			return true
		}
		if strings.Contains(req.Messages[0].Content, "source_research_report") && !strings.Contains(req.Messages[1].Content, "RFC 1123") {
			t.Error("mirror rows displaced independent evidence before the report limit")
		}
		return next(w, r, call)
	}
	ctx := context.Background()
	if _, err := f.service.CreateSourceResearch(ctx, f.actor, learningspace.DefaultID, SourceResearchCreate{OperationID: uuid.NewString(), Request: "Internet host requirements"}); err != nil {
		t.Fatal(err)
	}
	owned, err := f.service.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := &executionHost{service: f.service, owned: *owned, body: owned.body, limits: f.settings.View().Limits}
	direction := SourceResearchDirection{ID: "direction-1", Title: "Internet host requirements"}
	h.body.SourceResearch.Directions = []SourceResearchDirection{direction}
	ids := []string{}
	for rank, url := range []string{"http://mirror.org/1122", "http://original.net/1122", "http://other.net/1123"} {
		r, err := h.registerWebResource(ctx, url, url, "", "search")
		if err != nil {
			t.Fatal(err)
		}
		value, err := h.sourceResearchTool(ctx, "read_web", map[string]string{"resource_id": r.ID})
		if err != nil {
			t.Fatal(err)
		}
		d := value.(research.WebDocument)
		ids = append(ids, d.ID)
		_, support, actual, err := h.sourceResearchEvidence(ctx, []string{d.ID})
		if err != nil {
			t.Fatal(err)
		}
		d = actual[d.ID]
		proof, err := d.Cite(support[0].FragmentID, support[0].Quote, h.body.Web.Supplied[d.ID])
		if err != nil {
			t.Fatal(err)
		}
		c := sourceResearchCandidateRecord{SourceResearchCandidate: SourceResearchCandidate{ID: uuid.NewSHA1(uuid.MustParse(owned.SourceResearchJobID), []byte(d.RevisionID)).String(), DocumentID: d.ID, RevisionID: d.RevisionID, Title: d.Title, URL: d.URL, Support: &proof, Selected: true}}
		if err = f.service.mutateTx(ctx, &h.owned, "", func(tx pgx.Tx, item *row) error {
			raw, err := f.service.sealWeb("source_research_candidates", c.ID, item.Generation, c)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO learning_source_research_candidates(id,job_id,round_id,privacy_generation,document_id,rank,ciphertext) VALUES($1,$2,$3,$4,$5,$6,$7)`, c.ID, item.SourceResearchJobID, item.SourceResearchRoundID, item.Generation, d.ID, rank, raw)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err = h.reportSourceResearch(ctx, direction, ids); err != nil {
		t.Fatal(err)
	}
	if h.body.SourceResearch.SectionCount != 1 {
		t.Fatal("deduplicated report was not saved")
	}
}

// Observe the actual report request while retaining the fixture's ordinary
// model response and the production citation checks and persistence.
func observeSourceResearchReport(t *testing.T, f *runtimeFixture, want int) {
	t.Helper()
	previous := f.modelReply
	f.modelReply = func(w http.ResponseWriter, r *http.Request, call int) bool {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return true
		}
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		var req modelclient.Request
		if err = json.Unmarshal(raw, &req); err != nil {
			t.Error(err)
			return true
		}
		if strings.Contains(req.Messages[0].Content, "source_research_report") {
			var input struct {
				Evidence    []json.RawMessage `json:"evidence"`
				SourceCount int               `json:"source_count"`
			}
			if err = json.Unmarshal([]byte(req.Messages[1].Content), &input); err != nil {
				t.Error(err)
			}
			if len(input.Evidence) != want || input.SourceCount != want {
				t.Errorf("report received %d pieces of evidence and source_count=%d, want %d independent works", len(input.Evidence), input.SourceCount, want)
			}
		}
		return previous != nil && previous(w, r, call)
	}
}
