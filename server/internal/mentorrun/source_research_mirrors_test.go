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

// Front matter from the actual cs162 SIGCOMM '88 and LBL November '88 PDFs.
// The revised version adds Karels to the byline and explicitly identifies
// itself as a revision, so author-set equality alone would miss this pair.
const sourceResearchJacobsonConference = `Congestion Avoidance and Control
V. Jacobson
(Originally Published in: Proc. SIGCOMM '88, Vol18 No. 4, August 1988)
ACM SIGCOMM -157- Computer Communication Review

Congestion Avoidance and Control
Van Jacobson*
University of California
Lawrence Berkeley Laboratory
Berkeley, CA 94720
van@helios.ee.lbl.gov
In October of '86, the Internet had the first of what became a series of congestion collapses.`

const sourceResearchJacobsonRevision = `Congestion Avoidance and Control∗
Van Jacobson†
Lawrence Berkeley Laboratory
Michael J. Karels‡
University of California at Berkeley
November, 1988
Introduction
Computer networks have experienced an explosive growth over the past few years.
∗Note: This is a very slightly revised version of a paper originally presented at SIGCOMM ’88 [12]. If you
wish to reference this work, please cite [12].`

func TestSourceResearchPaperVersionIdentity(t *testing.T) {
	vegas := "TCP Vegas: New Techniques for Congestion\nDetection and Avoidance\nLawrence S. Brakmo\nSean W. O’Malley\nLarry L. Peterson\nTR 94 04\nAbstract\nVegas is a new implementation of TCP."
	cubic := "CUBIC: A New TCP-Friendly High-Speed TCP Variant\nSangtae Ha\nInjong Rhee\nLisbong Xu\n2008\nAbstract\nCUBIC uses a cubic window growth function."
	common := sourceResearchRFC1123
	for _, tc := range []struct {
		name, a, b string
		want       bool
	}{
		{"Jacobson conference and explicit revision", sourceResearchJacobsonConference, sourceResearchJacobsonRevision, true},
		{"normalized title and initial", "Congestion Avoidance and Control\nV. Jacobson\nAugust 1988\nAbstract\nOriginal text.", "CONGESTION AVOIDANCE AND CONTROL∗\nVan Jacobson†\nNovember 1988\nAbstract\nRewritten text.", true},
		{"authors on one line or separate lines", "An Adaptive Window Protocol\nAlice Smith, Bob Jones\n1994\nAbstract\nOriginal text.", "An Adaptive Window Protocol\nBob Jones\nAlice Smith\n1994\nAbstract\nRewritten text.", true},
		{"different coauthor in inline byline", "An Adaptive Window Protocol\nAlice Smith, Bob Jones\n1994\nAbstract\n" + common, "An Adaptive Window Protocol\nAlice Smith, Dana Brown\n1994\nAbstract\n" + common, false},
		{"different full first names", "Congestion Avoidance and Control\nVan Jacobson\n1988\nAbstract\n" + common, "Congestion Avoidance and Control\nVera Jacobson\n1988\nAbstract\n" + common, false},
		{"different authors with similar body", "Congestion Avoidance and Control\nVan Jacobson\n1988\nAbstract\n" + common, "Congestion Avoidance and Control\nSangtae Ha\n1988\nAbstract\n" + common, false},
		{"different years with similar body", "Congestion Avoidance and Control\nVan Jacobson\n1988\nAbstract\n" + common, "Congestion Avoidance and Control\nVan Jacobson\n2008\nAbstract\n" + common, false},
		{"additional author without revision proof", sourceResearchJacobsonConference, strings.ReplaceAll(sourceResearchJacobsonRevision, "very slightly revised version", "independent investigation"), false},
		{"revision with unrelated author", sourceResearchJacobsonConference, strings.NewReplacer("Van Jacobson", "Dana Smith", "Michael J. Karels", "Alex Brown").Replace(sourceResearchJacobsonRevision), false},
		{"revision with only secondary author in common", sourceResearchJacobsonConference, strings.NewReplacer("Van Jacobson", "Dana Smith", "Michael J. Karels", "Van Jacobson").Replace(sourceResearchJacobsonRevision), false},
		{"Vegas and CUBIC", vegas, cubic, false},
		{"Jacobson and Vegas", sourceResearchJacobsonRevision, vegas, false},
		{"title only is insufficient", "Congestion Avoidance and Control", sourceResearchJacobsonRevision, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := research.WebDocument{URL: "https://example.org/a.pdf", Title: "https://example.org/a.pdf", Text: tc.a, MediaType: "application/pdf"}
			b := research.WebDocument{URL: "https://example.net/b.pdf", Title: "https://example.net/b.pdf", Text: tc.b, MediaType: "application/pdf"}
			if got := sourceResearchSameWork(a, b); got != tc.want || sourceResearchSameWork(b, a) != tc.want {
				t.Fatalf("same work=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestSourceResearchPaperVersionsKeepPreferredCopy(t *testing.T) {
	const conference = "http://cs162.org/jacobson"
	const revision = "http://ee.lbl.gov/congavoid"
	for _, urls := range [][]string{{conference, revision}, {revision, conference}} {
		t.Run(urls[0], func(t *testing.T) {
			f := researchCoverageFixture(t, 1, 2, func(string) []string { return urls }, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				text := sourceResearchJacobsonConference
				if r.Host == "ee.lbl.gov" {
					text = sourceResearchJacobsonRevision
				}
				fmt.Fprint(w, text)
			})
			observeSourceResearchReport(t, f, 1)
			job := completeSourceResearch(t, f)
			candidates, err := f.service.SourceResearchCandidates(context.Background(), f.actor, learningspace.DefaultID, job.ID, "", 20)
			if err != nil || len(candidates.Items) != 1 {
				t.Fatalf("versions consumed two slots: %+v %v", candidates, err)
			}
			c := candidates.Items[0]
			if c.URL != revision || !strings.Contains(c.Reason, "另有版本：") || !strings.Contains(c.Reason, conference) || c.Support == nil || c.Support.DocumentID != c.DocumentID {
				t.Fatalf("preferred copy, alternate version or citation lost: %+v", c)
			}
		})
	}
}

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
		{"substantial contained excerpt", sourceResearchRFC1122, sourceResearchRFC1122[:600], true},
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

func TestSourceResearchPartialHTMLAndTextMirrors(t *testing.T) {
	const htmlURL = "http://mirror.org/rfc1122.html"
	const textURL = "http://original.net/rfc1122.txt"
	// TXT exhausts its byte budget on indentation; HTML whitespace extraction
	// reaches later sections. Both snapshots hit the same production text limit.
	body := sourceResearchRFC1122 + strings.Repeat(" \n   ", 4000) + strings.Repeat(sourceResearchRFC1123[strings.Index(sourceResearchRFC1123, "Application software"):], 30)
	for _, urls := range [][]string{{htmlURL, textURL}, {textURL, htmlURL}} {
		t.Run(urls[0], func(t *testing.T) {
			f := researchCoverageFixture(t, 1, 2, func(string) []string { return urls }, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, ".txt") {
					w.Header().Set("Content-Type", "text/plain")
					fmt.Fprint(w, body)
				} else {
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprintf(w, "<main>%s</main>", body)
				}
			})
			observeSourceResearchReport(t, f, 1)
			job := completeSourceResearch(t, f)
			if job.Status != "completed" || job.CandidateCount != 1 || job.Progress.ReadSucceeded != 2 {
				t.Fatalf("partial mirrors consumed separate candidate slots: %+v", job)
			}
			candidates, err := f.service.SourceResearchCandidates(context.Background(), f.actor, learningspace.DefaultID, job.ID, "", 20)
			if err != nil || len(candidates.Items) != 1 || candidates.Items[0].URL != htmlURL || candidates.Items[0].Coverage != "partial_text_limit" {
				t.Fatalf("partial mirror representative: %+v %v", candidates, err)
			}
		})
	}
}

func TestSourceResearchMirrorContainment(t *testing.T) {
	// No formal IDs or titles: these cases must exercise content containment.
	prose := sourceResearchRFC1122[strings.Index(sourceResearchRFC1122, "The Internet is"):]
	for _, tc := range []struct {
		name, a, b string
		want       bool
	}{
		{"contained partial text", prose, prose + sourceResearchRFC1123, true},
		{"substantial overlap", prose, prose[30:] + sourceResearchRFC1123, true},
		{"insufficient overlap", prose, prose[250:] + sourceResearchRFC1123, false},
		{"too little distinct text", prose[:200], prose, false},
		{"repeated boilerplate", strings.Repeat("Internet protocol ", 100), prose + strings.Repeat("Internet protocol ", 100), false},
		{"similar topic without identifiers", prose, sourceResearchRFC1123[strings.Index(sourceResearchRFC1123, "Application software"):], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := research.WebDocument{Text: tc.a, Coverage: "partial_text_limit"}
			b := research.WebDocument{Text: tc.b, Coverage: "partial_text_limit"}
			for _, pair := range [][2]research.WebDocument{{a, b}, {b, a}} {
				if got := sourceResearchSameWork(pair[0], pair[1]); got != tc.want {
					t.Fatalf("same work=%v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestSourceResearchMirrorFormalIdentity(t *testing.T) {
	const title = "Reliable transport protocols"
	for _, tc := range []struct {
		name, a, b, titleA, titleB string
		want                       bool
	}{
		{"same RFC", "Request for Comments: 1180", "Network Working Group\nRequest for Comments: 1180", title, "Reliable Transport Protocols | Archive", true},
		{"RFC heading", "RFC 1180: A TCP/IP Tutorial", "Request for Comments: 1180", "RFC 1180: TCP/IP Tutorial | RFC Editor", "A TCP/IP Tutorial", true},
		{"Unicode metadata", "Request for Comments: １１８０", "Request for Comments: 1180", title, title, true},
		{"same DOI", "DOI: 10.1000/ABC.123", "https://doi.org/10.1000/abc.123", title, title, true},
		{"same arXiv", "arXiv:2301.01234v2", "arXiv:2301.01234", title, title, true},
		{"arXiv PDF URL", "https://arxiv.org/pdf/2301.01234v2.pdf", "arXiv:2301.01234", title, title, true},
		{"early modern arXiv", "arXiv:0706.0001", "arXiv:0706.0001v2", title, title, true},
		{"old arXiv", "arXiv:hep-th/9901001v1", "arXiv:hep-th/9901001", title, title, true},
		{"same ISBN 13", "ISBN-13: 978-0-306-40615-7", "ISBN: 9780306406157", title, title, true},
		{"same ISBN 10", "ISBN-10: 0-8044-2957-X", "ISBN: 080442957X", title, title, true},
		{"ISBN 10 and 13", "ISBN: 0-306-40615-2", "ISBN: 9780306406157", title, title, true},
		{"different titles", "Request for Comments: 1180", "Request for Comments: 1180", title, "A survey of database query planners", false},
		{"missing titles", "Request for Comments: 1180", "Request for Comments: 1180", "", "", false},
		{"URL titles", "Request for Comments: 1180", "Request for Comments: 1180", "https://mirror.org/rfc1180", "https://original.net/rfc1180", false},
		{"different RFC", "Request for Comments: 1180", "Request for Comments: 1122", title, title, false},
		{"different DOI", "DOI: 10.1000/abc", "DOI: 10.1000/def", title, title, false},
		{"different arXiv", "arXiv:2301.01234", "arXiv:2301.01235", title, title, false},
		{"different ISBN", "ISBN: 9780306406157", "ISBN: 9780804429573", title, title, false},
		{"reference is not RFC metadata", "See RFC 1180 for an overview", "See RFC 1180 for an overview", title, title, false},
		{"unlabelled DOI", "Related work: 10.1000/abc", "Related work: 10.1000/abc", title, title, false},
		{"late identifier", strings.Repeat("x", 2048) + " DOI: 10.1000/abc", "DOI: 10.1000/abc", title, title, false},
		{"bibliography identifier", "References\nDOI: 10.1000/abc", "DOI: 10.1000/abc", title, title, false},
		{"malformed RFC", "Request for Comments: 1180draft", "Request for Comments: 1180draft", title, title, false},
		{"malformed DOI", "DOI: 10.12/abc", "DOI: 10.12/abc", title, title, false},
		{"empty DOI suffix", "DOI: 10.1000/", "DOI: 10.1000/", title, title, false},
		{"punctuation DOI suffix", "DOI: 10.1000/.", "DOI: 10.1000/.", title, title, false},
		{"malformed arXiv month", "arXiv:2313.01234", "arXiv:2313.01234", title, title, false},
		{"malformed arXiv suffix", "arXiv:2301.012345", "arXiv:2301.012345", title, title, false},
		{"short modern arXiv sequence", "arXiv:2301.0123", "arXiv:2301.0123", title, title, false},
		{"long early arXiv sequence", "arXiv:0706.00001", "arXiv:0706.00001", title, title, false},
		{"arXiv before new scheme", "arXiv:0703.0001", "arXiv:0703.0001", title, title, false},
		{"arXiv after old scheme", "arXiv:hep-th/0801001", "arXiv:hep-th/0801001", title, title, false},
		{"zero arXiv sequence", "arXiv:2301.00000", "arXiv:2301.00000", title, title, false},
		{"invalid arXiv version", "arXiv:2301.01234v0", "arXiv:2301.01234v0", title, title, false},
		{"invalid ISBN 13 checksum", "ISBN: 9780306406158", "ISBN: 9780306406158", title, title, false},
		{"invalid ISBN 10 checksum", "ISBN: 0306406153", "ISBN: 0306406153", title, title, false},
		{"invalid ISBN prefix", "ISBN: 9770306406158", "ISBN: 9770306406158", title, title, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Deliberately disjoint short bodies: positive identity matches cannot
			// be supplied by the shingle fallback or identical snapshot hashes.
			a := research.WebDocument{Text: tc.a + "\nFirst extracted passage.", Title: tc.titleA}
			b := research.WebDocument{Text: tc.b + "\nSecond bounded snapshot.", Title: tc.titleB}
			for _, pair := range [][2]research.WebDocument{{a, b}, {b, a}} {
				if got := sourceResearchSameWork(pair[0], pair[1]); got != tc.want {
					t.Fatalf("same work=%v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestSourceResearchMirrorIdentifierConflictsVetoContent(t *testing.T) {
	for _, headers := range [][2]string{
		{"Request for Comments: 1180", "Request for Comments: 1122"},
		{"DOI: 10.1000/abc", "DOI: 10.1000/def"},
		{"arXiv:2301.01234", "arXiv:2301.01235"},
		{"ISBN: 9780306406157", "ISBN: 9780804429573"},
		{"Request for Comments: 1180\nDOI: 10.1000/abc", "Request for Comments: 1180\nDOI: 10.1000/def"},
	} {
		t.Run(headers[0], func(t *testing.T) {
			a := research.WebDocument{Text: headers[0] + "\n" + sourceResearchRFC1122, Title: "Internet host requirements"}
			b := research.WebDocument{Text: headers[1] + "\n" + sourceResearchRFC1122, Title: a.Title}
			if sourceResearchSameWork(a, b) || sourceResearchSameWork(b, a) {
				t.Fatal("conflicting formal identifiers were overridden by matching title/body")
			}
		})
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
		c := sourceResearchCandidateRecord{SourceResearchCandidate: SourceResearchCandidate{ID: uuid.NewSHA1(uuid.MustParse(owned.SourceResearchJobID), []byte(d.RevisionID)).String(), DocumentID: d.ID, RevisionID: d.RevisionID, Title: d.Title, URL: d.URL, Support: &proof, Selected: true, EvidenceScope: "body"}}
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
