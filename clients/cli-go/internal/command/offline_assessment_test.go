package command

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func TestOfflineAssessmentCLIListsShowsAndDecidesWithoutBodyLeakage(t *testing.T) {
	const (
		submissionID = "83000000-0000-4000-8000-000000000001"
		operationID  = "84000000-0000-4000-8000-000000000001"
	)
	feedback := commandSessionView("Feedback", "open", "provisional", false, false)
	view := api.OfflineAssessmentView{
		Metadata: commandMetadata(), SubmissionID: submissionID, AggregateVersion: "2",
		Activity: *feedback.WorkItem.Activity, Attempt: *feedback.WorkItem.Attempt,
		Assessment: *feedback.WorkItem.Assessment, Decision: *feedback.WorkItem.AssessmentDecision,
		AllowedDecisions: []string{"override", "void"},
	}
	view.Attempt.ArchiveDisposition = "offline_succeeded"
	view.Attempt.OfflineSubmissionID = submissionID
	page := api.OfflineAssessmentPage{Metadata: view.Metadata, Items: []api.OfflineAssessmentSummary{{
		AssessmentID: view.Assessment.AssessmentID, AttemptID: view.Attempt.AttemptID,
		ActivityID: view.Activity.ActivityID, ActivityRevision: "1", SubmissionID: submissionID,
		AggregateVersion: "2", DispositionVersion: "1", Disposition: "provisional",
		Confidence: view.Assessment.Confidence, AllowedDecisions: []string{"override", "void"},
		AttemptReceivedAt: view.Attempt.ReceivedAt, AssessmentCreatedAt: view.Assessment.CreatedAt,
	}}}
	var received api.OfflineAssessmentVoidRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/learning/offline/assessments":
			writeJSONTest(w, http.StatusOK, page)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/learning/offline/assessments/"+view.Assessment.AssessmentID:
			writeJSONTest(w, http.StatusOK, view)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/learning/offline/assessments/"+view.Assessment.AssessmentID+"/decisions":
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&received); err != nil {
				t.Fatal(err)
			}
			decision := view.Decision
			decision.DecisionID = operationID
			decision.Version = 2
			decision.Disposition = "voided"
			decision.Reason = received.Reason
			decision.ReplacesDecisionID = view.Decision.DecisionID
			writeJSONTest(w, http.StatusCreated, api.OfflineAssessmentDecisionReceipt{
				OperationID: operationID, AssessmentID: view.Assessment.AssessmentID,
				AttemptID: view.Attempt.AttemptID, SubmissionID: submissionID,
				AggregateVersion: "3", FirstEventSequence: "21", LastEventSequence: "21",
				ProjectionAsOfEventSequence: "21", Decision: decision,
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{lines: []string{"invalid assessment"}})
	app.NewUUID = uuidSequence(t, operationID)
	if exit := app.Run(t.Context(), []string{"offline", "assessments", "--limit", "25"}); exit != ExitOK {
		t.Fatalf("list exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	assertOfflineAssessmentOutputSafe(t, out.String())
	out.Reset()
	if exit := app.Run(t.Context(), []string{"offline", "assessment", "show", view.Assessment.AssessmentID}); exit != ExitOK {
		t.Fatalf("show exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "Allowed decisions: override,void") {
		t.Fatalf("show output=%q", out.String())
	}
	assertOfflineAssessmentOutputSafe(t, out.String())
	out.Reset()
	if exit := app.Run(t.Context(), []string{"offline", "assessment", "void", view.Assessment.AssessmentID}); exit != ExitOK {
		t.Fatalf("void exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if received.OperationID != operationID || received.AttemptID != view.Attempt.AttemptID || received.ExpectedVersion != "2" || received.ExpectedDispositionVersion != "1" || received.Reason != "invalid assessment" {
		t.Fatalf("decision request=%+v", received)
	}
	if !strings.Contains(out.String(), "disposition=voided") || !strings.Contains(out.String(), "evidence=none") {
		t.Fatalf("decision output=%q", out.String())
	}
	assertOfflineAssessmentOutputSafe(t, out.String())
}

func TestOfflineAssessmentCLIAllowsStableOperationIDReplay(t *testing.T) {
	const (
		submissionID = "83000000-0000-4000-8000-000000000001"
		operationID  = "84000000-0000-4000-8000-000000000001"
	)
	feedback := commandSessionView("Feedback", "open", "provisional", false, false)
	view := api.OfflineAssessmentView{
		Metadata: commandMetadata(), SubmissionID: submissionID, AggregateVersion: "2",
		Activity: *feedback.WorkItem.Activity, Attempt: *feedback.WorkItem.Attempt,
		Assessment: *feedback.WorkItem.Assessment, Decision: *feedback.WorkItem.AssessmentDecision,
		AllowedDecisions: []string{"override", "void"},
	}
	view.Attempt.ArchiveDisposition = "offline_succeeded"
	view.Attempt.OfflineSubmissionID = submissionID
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSONTest(w, http.StatusOK, view)
			return
		}
		decision := view.Decision
		decision.DecisionID, decision.Version, decision.Disposition = operationID, 2, "voided"
		decision.Reason, decision.ReplacesDecisionID = "invalid assessment", view.Decision.DecisionID
		writeJSONTest(w, http.StatusOK, api.OfflineAssessmentDecisionReceipt{
			OperationID: operationID, AssessmentID: view.Assessment.AssessmentID, AttemptID: view.Attempt.AttemptID,
			SubmissionID: submissionID, Replayed: true, AggregateVersion: "3", FirstEventSequence: "21",
			LastEventSequence: "21", ProjectionAsOfEventSequence: "21", Decision: decision,
		})
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{lines: []string{"invalid assessment"}})
	if exit := app.Run(t.Context(), []string{"offline", "assessment", "void", "--operation-id", operationID, view.Assessment.AssessmentID}); exit != ExitOK || !strings.Contains(out.String(), "replayed=true") {
		t.Fatalf("replay exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
}

func TestOfflineAssessmentCLIRejectsUnicodeTextBeyondPublicLimits(t *testing.T) {
	const submissionID = "83000000-0000-4000-8000-000000000001"
	feedback := commandSessionView("Feedback", "open", "provisional", false, false)
	view := api.OfflineAssessmentView{
		Metadata: commandMetadata(), SubmissionID: submissionID, AggregateVersion: "2",
		Activity: *feedback.WorkItem.Activity, Attempt: *feedback.WorkItem.Attempt,
		Assessment: *feedback.WorkItem.Assessment, Decision: *feedback.WorkItem.AssessmentDecision,
		AllowedDecisions: []string{"override", "void"},
	}
	view.Attempt.ArchiveDisposition = "offline_succeeded"
	view.Attempt.OfflineSubmissionID = submissionID

	for _, test := range []struct {
		name  string
		kind  string
		lines []string
	}{
		{name: "reason", kind: "void", lines: []string{strings.Repeat("界", api.MaxOfflineAssessmentDecisionReasonRunes+1)}},
		{name: "misconception", kind: "override", lines: []string{"valid reason", "", strings.Repeat("误", api.MaxOfflineAssessmentMisconceptionRunes+1)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			postCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					postCalls++
					t.Fatalf("overlong CLI input reached POST: %s", r.URL.Path)
				}
				writeJSONTest(w, http.StatusOK, view)
			}))
			defer server.Close()
			configStore, credentialStore := pairedStores(server.URL, "token")
			app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{lines: test.lines})
			exit := app.Run(t.Context(), []string{"offline", "assessment", test.kind, view.Assessment.AssessmentID})
			if exit != ExitInput || postCalls != 0 {
				t.Fatalf("exit=%d postCalls=%d err=%q", exit, postCalls, errOut.String())
			}
		})
	}
}

func assertOfflineAssessmentOutputSafe(t *testing.T, output string) {
	t.Helper()
	for _, secret := range []string{"quiz answer", "State the concept.", "canonical note", strings.Repeat("a", 64), strings.Repeat("c", 64)} {
		if strings.Contains(output, secret) {
			t.Fatalf("offline assessment output leaked %q: %s", secret, output)
		}
	}
}
