package command

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func TestProposalMismatchProducesNoContentOrAction(t *testing.T) {
	t.Parallel()
	var actionCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tutoring/sessions/current":
			writeJSONTest(w, http.StatusOK, commandSessionView("Diagnostic", "open", "", false, false))
		case "/v1/knowledge/revisions/head":
			writeJSONTest(w, http.StatusOK, api.HeadResponse{Revision: testRevision()})
		case "/v1/knowledge/retrievals":
			writeJSONTest(w, http.StatusOK, commandRetrieval(false, false))
		case "/v1/tutoring/proposals":
			var request api.TutoringProposalRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			proposal := commandProposal(request, "route", "open")
			proposal.FrozenRequest.AggregateVersion++
			writeJSONTest(w, http.StatusCreated, proposal)
		default:
			if strings.HasSuffix(r.URL.Path, "/actions") {
				actionCalls.Add(1)
			}
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitInternal || out.Len() != 0 || actionCalls.Load() != 0 || !strings.Contains(errOut.String(), "protocol_error") {
		t.Fatalf("exit=%d out=%q actions=%d err=%q", exit, out.String(), actionCalls.Load(), errOut.String())
	}
}

func TestSubmitAttemptRequiresAllowedActionBeforeInputOrHTTP(t *testing.T) {
	t.Parallel()
	var actionCalls atomic.Int32
	view := commandSessionView("AwaitingResponse", "open", "", false, false)
	view.WorkItem.AllowedActions = []string{"ask_free_question", "end_activity"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/current" {
			writeJSONTest(w, http.StatusOK, view)
			return
		}
		actionCalls.Add(1)
		t.Fatalf("unexpected HTTP request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	term := &fakeTerminal{lines: []string{"answer", "hint"}}
	app, _, errOut := newTestApp(configStore, credentialStore, term)
	if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitConflict || actionCalls.Load() != 0 || !strings.Contains(errOut.String(), "submit_attempt") {
		t.Fatalf("exit=%d actions=%d err=%q", exit, actionCalls.Load(), errOut.String())
	}
	if len(term.lines) != 1 || term.lines[0] != "hint" {
		t.Fatalf("help input was unexpectedly consumed: remaining=%v", term.lines)
	}
}

func TestAssessmentOverrideUsesCurrentDecisionDefaults(t *testing.T) {
	t.Parallel()
	artifact := commandAssessment()
	artifact.Items[0].Conclusion = "fail"
	artifact.Items[0].MisconceptionCandidate = "artifact candidate"
	currentItem := artifact.Items[0]
	currentItem.Conclusion = "partial"
	currentItem.MisconceptionCandidate = "current candidate"
	decision := api.AssessmentDecision{Items: []api.AssessmentItem{currentItem}}
	tests := []struct {
		name      string
		lines     []string
		current   string
		want      string
		wantError bool
	}{
		{name: "blank uses current decision", lines: []string{"", ""}, current: "partial", want: "partial"},
		{name: "explicit pass", lines: []string{"pass", "updated"}, current: "partial", want: "pass"},
		{name: "unassessed forbidden", lines: []string{"unassessed"}, current: "partial", wantError: true},
		{name: "blank cannot preserve unassessed", lines: []string{""}, current: "unassessed", wantError: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			decisionCopy := decision
			decisionCopy.Items = append([]api.AssessmentItem(nil), decision.Items...)
			decisionCopy.Items[0].Conclusion = test.current
			app := &App{Terminal: &fakeTerminal{lines: append([]string(nil), test.lines...)}, Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
			items, err := app.collectAssessmentOverride(artifact, decisionCopy)
			if test.wantError {
				if err == nil {
					t.Fatal("expected override validation error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0].Conclusion != test.want || items[0].AnswerQuoteSHA256 != artifact.Items[0].AnswerQuoteSHA256 || items[0].KnowledgeQuoteSHA256 != artifact.Items[0].KnowledgeQuoteSHA256 {
				t.Fatalf("items=%+v", items)
			}
			if test.name == "blank uses current decision" && items[0].MisconceptionCandidate != "current candidate" {
				t.Fatalf("candidate=%q", items[0].MisconceptionCandidate)
			}
		})
	}
}

func TestLearnHelpCommandsAreContextLegal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state  string
		want   []string
		forbid []string
	}{
		{state: "AwaitingResponse", want: []string{":answer", ":ask", ":end", ":quit"}, forbid: []string{":quiz", ":resume", ":assessment", ":complete"}},
		{state: "FreeAnswer", want: []string{":ask", ":quiz", ":resume", ":quit"}, forbid: []string{":answer", ":assessment", ":end", ":complete"}},
		{state: "Feedback", want: []string{":assessment", ":quit"}, forbid: []string{":answer", ":ask", ":quiz", ":resume", ":end", ":complete"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.state, func(t *testing.T) {
			view := commandSessionView(test.state, "open", "provisional", false, false)
			commands := learnHelpCommands(view)
			for _, command := range test.want {
				if !slices.Contains(commands, command) {
					t.Errorf("commands %v do not contain %s", commands, command)
				}
			}
			for _, command := range test.forbid {
				if slices.Contains(commands, command) {
					t.Errorf("commands %v unexpectedly contain %s", commands, command)
				}
			}
		})
	}
}

func TestLearnRouteActivePaginatesDueReviewForCurrentNode(t *testing.T) {
	t.Parallel()
	metadata := commandMetadata()
	view := commandSessionView("RouteActive", "open", "", false, false)
	view.WorkItem.AllowedActions = []string{"present_review"}
	var reviewCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/learning/reviews" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
		if r.URL.Query().Get("limit") != "50" || r.URL.Query().Get("due_before") == "" {
			t.Fatalf("query=%q", r.URL.RawQuery)
		}
		switch cursor := r.URL.Query().Get("cursor"); cursor {
		case "":
			reviewCalls.Add(1)
			writeJSONTest(w, http.StatusOK, api.ReviewsPage{Metadata: metadata, Items: reviewSchedules(defaultPageLimit), NextCursor: "review-page-2"})
		case "review-page-2":
			reviewCalls.Add(1)
			writeJSONTest(w, http.StatusOK, api.ReviewsPage{Metadata: metadata, Items: []api.ReviewSchedule{{NodeRevisionID: commandNodeRevision, Step: 1, DueAt: time.Now().UTC(), Intervals: []int64{86400}, PolicyVersion: "review-v1"}}})
		default:
			t.Fatalf("unexpected cursor %q", cursor)
		}
	}))
	defer server.Close()
	terminal := &fakeTerminal{lines: []string{"", ":quit"}}
	app := &App{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, Terminal: terminal}
	fresh, quit, err := app.learnRouteActive(t.Context(), api.NewClient(server.URL, "token", time.Second, nil), view)
	if err != nil || !quit || fresh.Session.SessionID != view.Session.SessionID || reviewCalls.Load() != 2 || terminal.confirmCalls != 1 {
		t.Fatalf("quit=%t session=%s reviews=%d confirms=%d err=%v", quit, fresh.Session.SessionID, reviewCalls.Load(), terminal.confirmCalls, err)
	}
}

func TestLearnRouteActiveRestartsDueReviewsAfterStaleCursor(t *testing.T) {
	t.Parallel()
	oldMetadata := commandMetadata()
	newMetadata := oldMetadata
	newMetadata.AsOfEventSeq++
	newMetadata.Generation = "dd000000-0000-4000-8000-000000000006"
	view := commandSessionView("RouteActive", "open", "", false, false)
	view.WorkItem.AllowedActions = []string{"present_review"}
	freshView := view
	freshView.Metadata = newMetadata
	var reviewCalls, currentCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tutoring/sessions/current":
			currentCalls.Add(1)
			writeJSONTest(w, http.StatusOK, freshView)
		case "/v1/learning/reviews":
			call := reviewCalls.Add(1)
			cursor := r.URL.Query().Get("cursor")
			switch {
			case call == 1 && cursor == "":
				writeJSONTest(w, http.StatusOK, api.ReviewsPage{Metadata: oldMetadata, Items: reviewSchedules(defaultPageLimit), NextCursor: "stale-review"})
			case call == 2 && cursor == "stale-review":
				writeJSONTest(w, http.StatusConflict, api.ErrorResponse{Error: api.ErrorBody{Code: "stale_cursor", Message: "changed", RequestID: "request-stale-review"}})
			case call == 3 && cursor == "":
				writeJSONTest(w, http.StatusOK, api.ReviewsPage{Metadata: newMetadata, Items: []api.ReviewSchedule{{NodeRevisionID: commandNodeRevision, Step: 1, DueAt: time.Now().UTC(), Intervals: []int64{86400}, PolicyVersion: "review-v1"}}})
			default:
				t.Fatalf("unexpected review call=%d cursor=%q", call, cursor)
			}
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	terminal := &fakeTerminal{lines: []string{"", ":quit"}}
	errOut := &bytes.Buffer{}
	app := &App{Out: &bytes.Buffer{}, Err: errOut, Terminal: terminal}
	_, quit, err := app.learnRouteActive(t.Context(), api.NewClient(server.URL, "token", time.Second, nil), view)
	if err != nil || !quit || reviewCalls.Load() != 3 || currentCalls.Load() != 1 || terminal.confirmCalls != 1 || !strings.Contains(errOut.String(), "stale_cursor") {
		t.Fatalf("quit=%t reviews=%d current=%d confirms=%d err=%v warnings=%q", quit, reviewCalls.Load(), currentCalls.Load(), terminal.confirmCalls, err, errOut.String())
	}
}

func TestProgressAllPaginatesEvidenceAndReviews(t *testing.T) {
	t.Parallel()
	metadata := commandMetadata()
	var evidenceCalls, reviewCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/learning/projections/status":
			writeJSONTest(w, http.StatusOK, api.ProjectionStatus{Metadata: metadata, CommittedEventHighWater: metadata.AsOfEventSeq, Fingerprint: strings.Repeat("f", 64), ActiveGenerationID: metadata.Generation})
		case "/v1/tutoring/sessions/current":
			writeJSONTest(w, http.StatusNotFound, api.ErrorResponse{Error: api.ErrorBody{Code: "not_found", Message: "none", RequestID: "request-no-session"}})
		case "/v1/learning/routes":
			writeJSONTest(w, http.StatusOK, api.RoutesPage{Metadata: metadata, Items: []api.RouteProjection{}})
		case "/v1/learning/evidence":
			evidenceCalls.Add(1)
			if r.URL.Query().Get("cursor") == "" {
				writeJSONTest(w, http.StatusOK, api.EvidencePage{Metadata: metadata, Items: make([]api.AcceptedEvidence, defaultPageLimit), NextCursor: "evidence-page-2"})
			} else if r.URL.Query().Get("cursor") == "evidence-page-2" {
				writeJSONTest(w, http.StatusOK, api.EvidencePage{Metadata: metadata, Items: []api.AcceptedEvidence{{EvidenceID: "99000000-0000-4000-8000-000000000001"}}})
			} else {
				t.Fatalf("unexpected evidence cursor %q", r.URL.Query().Get("cursor"))
			}
		case "/v1/learning/reviews":
			reviewCalls.Add(1)
			if r.URL.Query().Get("cursor") == "" {
				writeJSONTest(w, http.StatusOK, api.ReviewsPage{Metadata: metadata, Items: reviewSchedules(defaultPageLimit), NextCursor: "review-page-2"})
			} else if r.URL.Query().Get("cursor") == "review-page-2" {
				writeJSONTest(w, http.StatusOK, api.ReviewsPage{Metadata: metadata, Items: []api.ReviewSchedule{{NodeRevisionID: commandNodeRevision, Intervals: []int64{}}}})
			} else {
				t.Fatalf("unexpected review cursor %q", r.URL.Query().Get("cursor"))
			}
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"progress", "--all"}); exit != ExitOK || evidenceCalls.Load() != 2 || reviewCalls.Load() != 2 || !strings.Contains(out.String(), "Evidence: 51 bounded items") || !strings.Contains(out.String(), "Reviews: 51 bounded items") || strings.Contains(errOut.String(), "warning[truncated]") {
		t.Fatalf("exit=%d evidence=%d reviews=%d out=%q err=%q", exit, evidenceCalls.Load(), reviewCalls.Load(), out.String(), errOut.String())
	}
}

func TestProgressAllEvidenceStaleCursorRestartsWholeSnapshot(t *testing.T) {
	oldMetadata := commandMetadata()
	newMetadata := oldMetadata
	newMetadata.AsOfEventSeq++
	newMetadata.Generation = "dd000000-0000-4000-8000-000000000007"
	var statusCalls, routeCalls, evidenceCalls, reviewCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/learning/projections/status":
			metadata := oldMetadata
			if statusCalls.Add(1) > 1 {
				metadata = newMetadata
			}
			writeJSONTest(w, http.StatusOK, api.ProjectionStatus{Metadata: metadata, CommittedEventHighWater: metadata.AsOfEventSeq, Fingerprint: strings.Repeat("f", 64), ActiveGenerationID: metadata.Generation})
		case "/v1/tutoring/sessions/current":
			writeJSONTest(w, http.StatusNotFound, api.ErrorResponse{Error: api.ErrorBody{Code: "not_found", Message: "none", RequestID: "request-no-session"}})
		case "/v1/learning/routes":
			metadata := oldMetadata
			if routeCalls.Add(1) > 1 {
				metadata = newMetadata
			}
			writeJSONTest(w, http.StatusOK, api.RoutesPage{Metadata: metadata, Items: []api.RouteProjection{}})
		case "/v1/learning/evidence":
			call := evidenceCalls.Add(1)
			switch {
			case call == 1 && r.URL.Query().Get("cursor") == "":
				writeJSONTest(w, http.StatusOK, api.EvidencePage{Metadata: oldMetadata, Items: make([]api.AcceptedEvidence, defaultPageLimit), NextCursor: "stale-evidence"})
			case call == 2 && r.URL.Query().Get("cursor") == "stale-evidence":
				writeJSONTest(w, http.StatusConflict, api.ErrorResponse{Error: api.ErrorBody{Code: "stale_cursor", Message: "changed", RequestID: "request-stale-evidence"}})
			case call == 3 && r.URL.Query().Get("cursor") == "":
				writeJSONTest(w, http.StatusOK, api.EvidencePage{Metadata: newMetadata, Items: []api.AcceptedEvidence{{EvidenceID: "99000000-0000-4000-8000-000000000002"}}})
			default:
				t.Fatalf("unexpected evidence call=%d cursor=%q", call, r.URL.Query().Get("cursor"))
			}
		case "/v1/learning/reviews":
			reviewCalls.Add(1)
			writeJSONTest(w, http.StatusOK, api.ReviewsPage{Metadata: newMetadata, Items: []api.ReviewSchedule{{NodeRevisionID: commandNodeRevision, Intervals: []int64{}}}})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"progress", "--all"}); exit != ExitOK || statusCalls.Load() != 2 || routeCalls.Load() != 2 || evidenceCalls.Load() != 3 || reviewCalls.Load() != 1 || !strings.Contains(out.String(), "Evidence: 1 bounded items") || !strings.Contains(errOut.String(), "stale_cursor") {
		t.Fatalf("exit=%d status=%d routes=%d evidence=%d reviews=%d out=%q err=%q", exit, statusCalls.Load(), routeCalls.Load(), evidenceCalls.Load(), reviewCalls.Load(), out.String(), errOut.String())
	}
}

func TestLearnRouteActiveFailsClosedWhenDueReviewExceedsPageBudget(t *testing.T) {
	t.Parallel()
	metadata := commandMetadata()
	view := commandSessionView("RouteActive", "open", "", false, false)
	view.WorkItem.AllowedActions = []string{"present_review", "issue_activity"}
	var reviewCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/learning/reviews" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
		reviewCalls.Add(1)
		writeJSONTest(w, http.StatusOK, api.ReviewsPage{Metadata: metadata, Items: reviewSchedules(defaultPageLimit), NextCursor: "more-reviews"})
	}))
	defer server.Close()
	terminal := &fakeTerminal{lines: []string{""}}
	app := &App{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, Terminal: terminal}
	_, _, err := app.learnRouteActive(t.Context(), api.NewClient(server.URL, "token", time.Second, nil), view)
	if err == nil || !strings.Contains(err.Error(), "review_lookup_truncated") || reviewCalls.Load() != maxProgressPages || terminal.confirmCalls != 0 {
		t.Fatalf("reviews=%d confirms=%d err=%v", reviewCalls.Load(), terminal.confirmCalls, err)
	}
}

func TestProgressAllMarksEvidencePageBudgetTruncated(t *testing.T) {
	t.Parallel()
	metadata := commandMetadata()
	var evidenceCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/learning/projections/status":
			writeJSONTest(w, http.StatusOK, api.ProjectionStatus{Metadata: metadata, CommittedEventHighWater: metadata.AsOfEventSeq, Fingerprint: strings.Repeat("f", 64), ActiveGenerationID: metadata.Generation})
		case "/v1/tutoring/sessions/current":
			writeJSONTest(w, http.StatusNotFound, api.ErrorResponse{Error: api.ErrorBody{Code: "not_found", Message: "none", RequestID: "request-no-session"}})
		case "/v1/learning/routes":
			writeJSONTest(w, http.StatusOK, api.RoutesPage{Metadata: metadata, Items: []api.RouteProjection{}})
		case "/v1/learning/evidence":
			evidenceCalls.Add(1)
			writeJSONTest(w, http.StatusOK, api.EvidencePage{Metadata: metadata, Items: make([]api.AcceptedEvidence, defaultPageLimit), NextCursor: "more-evidence"})
		case "/v1/learning/reviews":
			writeJSONTest(w, http.StatusOK, api.ReviewsPage{Metadata: metadata, Items: []api.ReviewSchedule{}})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"progress", "--all"}); exit != ExitOK || evidenceCalls.Load() != maxProgressPages || !strings.Contains(out.String(), "Evidence: 500 bounded items") || !strings.Contains(errOut.String(), "warning[truncated]") {
		t.Fatalf("exit=%d evidence=%d out=%q err=%q", exit, evidenceCalls.Load(), out.String(), errOut.String())
	}
}

func reviewSchedules(count int) []api.ReviewSchedule {
	items := make([]api.ReviewSchedule, count)
	for index := range items {
		items[index].Intervals = []int64{}
	}
	return items
}

func TestProgressAllRestartsWithoutCombiningStaleGeneration(t *testing.T) {
	t.Parallel()
	oldNode := "55000000-0000-4000-8000-000000000091"
	newNode := "55000000-0000-4000-8000-000000000092"
	oldMetadata := commandMetadata()
	newMetadata := oldMetadata
	newMetadata.AsOfEventSeq = 31
	newMetadata.Generation = "dd000000-0000-4000-8000-000000000002"
	oldRoute := *commandSessionView("RouteActive", "open", "", false, false).WorkItem.RouteRevision
	oldRoute.RouteRevisionID = "33000000-0000-4000-8000-000000000091"
	oldRoute.RouteID = "33000000-0000-4000-8000-000000000092"
	oldRoute.Steps[0].NodeRevisionID = oldNode
	newRoute := oldRoute
	newRoute.RouteRevisionID = "33000000-0000-4000-8000-000000000093"
	newRoute.RouteID = "33000000-0000-4000-8000-000000000094"
	newRoute.Steps = append([]api.RouteStep(nil), oldRoute.Steps...)
	newRoute.Steps[0].NodeRevisionID = newNode
	newView := commandSessionView("RouteActive", "open", "", false, false)
	newView.Metadata = newMetadata
	newView.WorkItem.RouteRevision = &newRoute
	var routeCalls, currentCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tutoring/sessions/current":
			currentCalls.Add(1)
			writeJSONTest(w, http.StatusOK, newView)
		case "/v1/learning/routes":
			if r.URL.Query().Get("current_only") != "true" {
				t.Fatalf("current_only=%q", r.URL.Query().Get("current_only"))
			}
			call := routeCalls.Add(1)
			if r.URL.Query().Get("cursor") == "stale" {
				writeJSONTest(w, http.StatusConflict, api.ErrorResponse{Error: api.ErrorBody{Code: "stale_cursor", Message: "changed", RequestID: "request-stale"}})
				return
			}
			if call == 1 {
				writeJSONTest(w, http.StatusOK, api.RoutesPage{Metadata: oldMetadata, Items: []api.RouteProjection{{Route: oldRoute, EventSeq: 30, Current: true}}, NextCursor: "stale"})
				return
			}
			writeJSONTest(w, http.StatusOK, api.RoutesPage{Metadata: newMetadata, Items: []api.RouteProjection{{Route: newRoute, EventSeq: 31, Current: true}}})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	view := commandSessionView("RouteActive", "open", "", false, false)
	errOut := &bytes.Buffer{}
	app := &App{Err: errOut}
	client := api.NewClient(server.URL, "token", time.Second, nil)
	base, truncated, err := app.progressNodeIDs(t.Context(), client, view, true, false)
	if err != nil || truncated || routeCalls.Load() != 0 || len(base) != 1 || base[0] != commandNodeRevision {
		t.Fatalf("base=%v truncated=%t calls=%d err=%v", base, truncated, routeCalls.Load(), err)
	}
	ids, truncated, err := app.progressNodeIDs(t.Context(), client, view, true, true)
	if err != nil || truncated || routeCalls.Load() != 3 || currentCalls.Load() != 1 || len(ids) != 1 || ids[0] != newNode || slices.Contains(ids, commandNodeRevision) || slices.Contains(ids, oldNode) || !strings.Contains(errOut.String(), "stale_cursor") {
		t.Fatalf("ids=%v truncated=%t routes=%d current=%d err=%v warnings=%q", ids, truncated, routeCalls.Load(), currentCalls.Load(), err, errOut.String())
	}
}

func TestProgressAllStaleCursorDiscardsDeletedGeneration(t *testing.T) {
	oldNode := "55000000-0000-4000-8000-000000000095"
	newNode := "55000000-0000-4000-8000-000000000096"
	oldMetadata := commandMetadata()
	newMetadata := oldMetadata
	newMetadata.AsOfEventSeq = 31
	newMetadata.Generation = "dd000000-0000-4000-8000-000000000005"
	oldRoute := *commandSessionView("RouteActive", "open", "", false, false).WorkItem.RouteRevision
	oldRoute.RouteRevisionID = "33000000-0000-4000-8000-000000000099"
	oldRoute.RouteID = "33000000-0000-4000-8000-000000000100"
	oldRoute.Steps[0].NodeRevisionID = oldNode
	newRoute := oldRoute
	newRoute.RouteRevisionID = "33000000-0000-4000-8000-000000000101"
	newRoute.RouteID = "33000000-0000-4000-8000-000000000102"
	newRoute.Steps = append([]api.RouteStep(nil), oldRoute.Steps...)
	newRoute.Steps[0].NodeRevisionID = newNode
	oldView := commandSessionView("RouteActive", "open", "", false, false)
	oldView.Metadata = oldMetadata
	oldView.WorkItem.RouteRevision = &oldRoute
	newView := commandSessionView("RouteActive", "open", "", false, false)
	newView.Metadata = newMetadata
	newView.WorkItem.RouteRevision = &newRoute
	newNodeView := commandNodeView()
	newNodeView.Metadata = newMetadata
	newNodeView.Node.Mastery.NodeRevisionID = newNode
	var statusCalls, currentCalls, routeCalls, oldNodeCalls, newNodeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/learning/projections/status":
			metadata := oldMetadata
			if statusCalls.Add(1) > 1 {
				metadata = newMetadata
			}
			writeJSONTest(w, http.StatusOK, api.ProjectionStatus{Metadata: metadata, CommittedEventHighWater: metadata.AsOfEventSeq, Fingerprint: strings.Repeat("f", 64), ActiveGenerationID: metadata.Generation})
		case "/v1/tutoring/sessions/current":
			if currentCalls.Add(1) == 1 {
				writeJSONTest(w, http.StatusOK, oldView)
			} else {
				writeJSONTest(w, http.StatusOK, newView)
			}
		case "/v1/learning/routes":
			if r.URL.Query().Get("current_only") != "true" {
				t.Fatalf("current_only=%q", r.URL.Query().Get("current_only"))
			}
			call := routeCalls.Add(1)
			if r.URL.Query().Get("cursor") == "stale" {
				writeJSONTest(w, http.StatusConflict, api.ErrorResponse{Error: api.ErrorBody{Code: "stale_cursor", Message: "changed", RequestID: "request-stale"}})
				return
			}
			if call == 1 {
				writeJSONTest(w, http.StatusOK, api.RoutesPage{Metadata: oldMetadata, Items: []api.RouteProjection{{Route: oldRoute, EventSeq: 30, Current: true}}, NextCursor: "stale"})
			} else {
				writeJSONTest(w, http.StatusOK, api.RoutesPage{Metadata: newMetadata, Items: []api.RouteProjection{{Route: newRoute, EventSeq: 31, Current: true}}})
			}
		case "/v1/learning/evidence":
			writeJSONTest(w, http.StatusOK, api.EvidencePage{Metadata: newMetadata, Items: []api.AcceptedEvidence{}})
		case "/v1/learning/reviews":
			writeJSONTest(w, http.StatusOK, api.ReviewsPage{Metadata: newMetadata, Items: []api.ReviewSchedule{}})
		default:
			if strings.Contains(r.URL.Path, "/v1/learning/nodes/") {
				switch {
				case strings.Contains(r.URL.Path, oldNode), strings.Contains(r.URL.Path, commandNodeRevision):
					oldNodeCalls.Add(1)
					writeJSONTest(w, http.StatusNotFound, api.ErrorResponse{Error: api.ErrorBody{Code: "not_found", Message: "deleted", RequestID: "request-deleted"}})
				case strings.Contains(r.URL.Path, newNode):
					newNodeCalls.Add(1)
					writeJSONTest(w, http.StatusOK, newNodeView)
				default:
					t.Fatalf("unexpected node %s", r.URL.Path)
				}
				return
			}
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"progress", "--all"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if statusCalls.Load() != 2 || currentCalls.Load() != 2 || routeCalls.Load() != 3 || oldNodeCalls.Load() != 0 || newNodeCalls.Load() != 1 {
		t.Fatalf("status=%d current=%d routes=%d old_nodes=%d new_nodes=%d", statusCalls.Load(), currentCalls.Load(), routeCalls.Load(), oldNodeCalls.Load(), newNodeCalls.Load())
	}
	if !strings.Contains(out.String(), newNode) || strings.Contains(out.String(), oldNode) || strings.Contains(out.String(), commandNodeRevision) || !strings.Contains(errOut.String(), "stale_cursor") {
		t.Fatalf("out=%q err=%q", out.String(), errOut.String())
	}
}

func TestProgressAllRetriesCompleteSnapshotOnGenerationMismatch(t *testing.T) {
	oldNode := "55000000-0000-4000-8000-000000000093"
	newNode := "55000000-0000-4000-8000-000000000094"
	oldMetadata := commandMetadata()
	newMetadata := oldMetadata
	newMetadata.AsOfEventSeq = 31
	newMetadata.Generation = "dd000000-0000-4000-8000-000000000003"
	oldRoute := *commandSessionView("RouteActive", "open", "", false, false).WorkItem.RouteRevision
	oldRoute.RouteRevisionID = "33000000-0000-4000-8000-000000000095"
	oldRoute.RouteID = "33000000-0000-4000-8000-000000000096"
	oldRoute.Steps[0].NodeRevisionID = oldNode
	newRoute := oldRoute
	newRoute.RouteRevisionID = "33000000-0000-4000-8000-000000000097"
	newRoute.RouteID = "33000000-0000-4000-8000-000000000098"
	newRoute.Steps = append([]api.RouteStep(nil), oldRoute.Steps...)
	newRoute.Steps[0].NodeRevisionID = newNode
	oldView := commandSessionView("RouteActive", "open", "", false, false)
	oldView.Metadata = oldMetadata
	oldView.WorkItem.RouteRevision = &oldRoute
	newView := commandSessionView("RouteActive", "open", "", false, false)
	newView.Metadata = newMetadata
	newView.WorkItem.RouteRevision = &newRoute
	oldNodeView := commandNodeView()
	oldNodeView.Metadata = oldMetadata
	oldNodeView.Node.Mastery.NodeRevisionID = oldNode
	newNodeView := commandNodeView()
	newNodeView.Metadata = newMetadata
	newNodeView.Node.Mastery.NodeRevisionID = newNode
	var statusCalls, currentCalls, routeCalls, nodeCalls, evidenceCalls, reviewCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/learning/projections/status":
			metadata := oldMetadata
			if statusCalls.Add(1) > 1 {
				metadata = newMetadata
			}
			writeJSONTest(w, http.StatusOK, api.ProjectionStatus{Metadata: metadata, CommittedEventHighWater: metadata.AsOfEventSeq, Fingerprint: strings.Repeat("f", 64), ActiveGenerationID: metadata.Generation})
		case "/v1/tutoring/sessions/current":
			if currentCalls.Add(1) == 1 {
				writeJSONTest(w, http.StatusOK, oldView)
			} else {
				writeJSONTest(w, http.StatusOK, newView)
			}
		case "/v1/learning/routes":
			if r.URL.Query().Get("current_only") != "true" {
				t.Fatalf("current_only=%q", r.URL.Query().Get("current_only"))
			}
			if routeCalls.Add(1) == 1 {
				writeJSONTest(w, http.StatusOK, api.RoutesPage{Metadata: oldMetadata, Items: []api.RouteProjection{{Route: oldRoute, EventSeq: 30, Current: true}}})
			} else {
				writeJSONTest(w, http.StatusOK, api.RoutesPage{Metadata: newMetadata, Items: []api.RouteProjection{{Route: newRoute, EventSeq: 31, Current: true}}})
			}
		case "/v1/learning/evidence":
			evidenceCalls.Add(1)
			writeJSONTest(w, http.StatusOK, api.EvidencePage{Metadata: newMetadata, Items: []api.AcceptedEvidence{}})
		case "/v1/learning/reviews":
			reviewCalls.Add(1)
			writeJSONTest(w, http.StatusOK, api.ReviewsPage{Metadata: newMetadata, Items: []api.ReviewSchedule{}})
		default:
			if strings.Contains(r.URL.Path, "/v1/learning/nodes/") {
				nodeCalls.Add(1)
				if strings.Contains(r.URL.Path, oldNode) {
					writeJSONTest(w, http.StatusOK, oldNodeView)
					return
				}
				if strings.Contains(r.URL.Path, newNode) {
					writeJSONTest(w, http.StatusOK, newNodeView)
					return
				}
			}
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"progress", "--all"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if statusCalls.Load() != 2 || currentCalls.Load() != 2 || routeCalls.Load() != 2 || nodeCalls.Load() != 2 || evidenceCalls.Load() != 2 || reviewCalls.Load() != 1 {
		t.Fatalf("status=%d current=%d routes=%d nodes=%d evidence=%d reviews=%d", statusCalls.Load(), currentCalls.Load(), routeCalls.Load(), nodeCalls.Load(), evidenceCalls.Load(), reviewCalls.Load())
	}
	if !strings.Contains(out.String(), newNode) || strings.Contains(out.String(), oldNode) || !strings.Contains(errOut.String(), "progress_snapshot") {
		t.Fatalf("out=%q err=%q", out.String(), errOut.String())
	}
}

func TestProgressRejectsUnstableCompleteSnapshot(t *testing.T) {
	metadata := commandMetadata()
	mismatched := metadata
	mismatched.Generation = "dd000000-0000-4000-8000-000000000004"
	view := commandSessionView("RouteActive", "open", "", false, false)
	var statusCalls, currentCalls, routeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/learning/projections/status":
			statusCalls.Add(1)
			writeJSONTest(w, http.StatusOK, api.ProjectionStatus{Metadata: metadata, CommittedEventHighWater: metadata.AsOfEventSeq, Fingerprint: strings.Repeat("f", 64), ActiveGenerationID: metadata.Generation})
		case "/v1/tutoring/sessions/current":
			currentCalls.Add(1)
			writeJSONTest(w, http.StatusOK, view)
		case "/v1/learning/routes":
			routeCalls.Add(1)
			writeJSONTest(w, http.StatusOK, api.RoutesPage{Metadata: mismatched, Items: []api.RouteProjection{}})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"progress", "--all"}); exit != ExitConflict || out.Len() != 0 || statusCalls.Load() != 2 || currentCalls.Load() != 2 || routeCalls.Load() != 2 || !strings.Contains(errOut.String(), "unstable_progress_snapshot") {
		t.Fatalf("exit=%d out=%q err=%q status=%d current=%d routes=%d", exit, out.String(), errOut.String(), statusCalls.Load(), currentCalls.Load(), routeCalls.Load())
	}
}

func TestStandaloneAssessmentAndDerivedQueriesWarnMetadata(t *testing.T) {
	t.Parallel()
	t.Run("assessment show", func(t *testing.T) {
		view := commandSessionView("Feedback", "open", "provisional", false, false)
		view.Metadata.Degraded = true
		view.Metadata.ReasonCodes = []string{"assessment_projection_lag"}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSONTest(w, http.StatusOK, view) }))
		defer server.Close()
		configStore, credentialStore := pairedStores(server.URL, "token")
		app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
		if exit := app.Run(t.Context(), []string{"assessment", "show"}); exit != ExitOK || !strings.Contains(errOut.String(), "assessment_projection_lag") || !strings.Contains(errOut.String(), "generation=") {
			t.Fatalf("exit=%d warnings=%q", exit, errOut.String())
		}
	})
	t.Run("derived evidence and reviews", func(t *testing.T) {
		evidenceMetadata := commandMetadata()
		evidenceMetadata.Incomplete = true
		evidenceMetadata.ReasonCodes = []string{"evidence_projection_lag"}
		reviewMetadata := commandMetadata()
		reviewMetadata.Degraded = true
		reviewMetadata.ReasonCodes = []string{"review_projection_lag"}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/v1/learning/nodes/"):
				writeJSONTest(w, http.StatusOK, commandNodeView())
			case r.URL.Path == "/v1/learning/evidence":
				writeJSONTest(w, http.StatusOK, api.EvidencePage{Metadata: evidenceMetadata, Items: []api.AcceptedEvidence{}})
			case r.URL.Path == "/v1/learning/reviews":
				writeJSONTest(w, http.StatusOK, api.ReviewsPage{Metadata: reviewMetadata, Items: []api.ReviewSchedule{}})
			default:
				t.Fatalf("unexpected %s", r.URL.Path)
			}
		}))
		defer server.Close()
		errOut := &bytes.Buffer{}
		app := &App{Out: &bytes.Buffer{}, Err: errOut}
		if err := app.printDecisionProjection(t.Context(), api.NewClient(server.URL, "token", time.Second, nil), commandSessionView("Feedback", "open", "provisional", false, false)); err != nil || !strings.Contains(errOut.String(), "evidence_projection_lag") || !strings.Contains(errOut.String(), "review_projection_lag") {
			t.Fatalf("error=%v warnings=%q", err, errOut.String())
		}
	})
}
