package command

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

const (
	commandSessionID     = "11000000-0000-4000-8000-000000000001"
	commandGoalRevision  = "22000000-0000-4000-8000-000000000001"
	commandRouteRevision = "33000000-0000-4000-8000-000000000001"
	commandKnowledgeID   = "44000000-0000-4000-8000-000000000001"
	commandStepID        = "55000000-0000-4000-8000-000000000001"
	commandNodeID        = "55000000-0000-4000-8000-000000000002"
	commandNodeRevision  = "55000000-0000-4000-8000-000000000003"
	commandActivityID    = "66000000-0000-4000-8000-000000000001"
	commandAttemptID     = "77000000-0000-4000-8000-000000000001"
	commandAssessmentID  = "88000000-0000-4000-8000-000000000001"
	commandQuestionID    = "aa000000-0000-4000-8000-000000000001"
	commandAnswerID      = "cc000000-0000-4000-8000-000000000001"
)

func TestLearnMultilineHelpAndNoLocalPersistence(t *testing.T) {
	t.Parallel()
	state := "AwaitingResponse"
	var submitted api.ActionAttemptRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/current":
			if state == "Completed" {
				writeJSONTest(w, http.StatusNotFound, api.ErrorResponse{Error: api.ErrorBody{Code: "not_found", Message: "no active session", RequestID: "request-current-completed"}})
				return
			}
			writeJSONTest(w, http.StatusOK, commandSessionView(state, "open", "", false, false))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/"+commandSessionID:
			if state != "Completed" {
				t.Fatalf("unexpected by-ID read before completion, state=%s", state)
			}
			writeJSONTest(w, http.StatusOK, commandSessionView("Completed", "open", "", false, false))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions"):
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Fatal(err)
			}
			if submitted.Action != "submit_attempt" {
				t.Fatalf("action=%s", submitted.Action)
			}
			state = "Completed"
			writeJSONTest(w, http.StatusCreated, commandOperationResult())
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{lines: []string{":answer", "first line", "second line", ".", "hint"}})
	if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if submitted.Answer != "first line\nsecond line" || submitted.Help != "hint" {
		t.Fatalf("submitted=%+v", submitted)
	}
	if configStore.saveCalls != 0 || credentialStore.saveCalls != 0 || strings.Contains(fmt.Sprintf("%+v %+v", configStore.value, credentialStore.record), submitted.Answer) {
		t.Fatalf("learning content reached local persistence: config=%+v credential=%+v", configStore, credentialStore)
	}
}

func TestLearnClearDoesNotRedrawActivity(t *testing.T) {
	t.Parallel()
	state := "AwaitingResponse"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/current":
			writeJSONTest(w, http.StatusOK, commandSessionView(state, "open", "", false, false))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions"):
			state = "Completed"
			writeJSONTest(w, http.StatusCreated, commandOperationResult())
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	term := &fakeTerminal{lines: []string{":clear", "answer", ""}}
	app, out, errOut := newTestApp(configStore, credentialStore, term)
	if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if term.clearCalls != 1 || strings.Count(out.String(), "Question:") != 1 {
		t.Fatalf("clear_calls=%d out=%q", term.clearCalls, out.String())
	}
}

func TestLearnAskBeforeAutomaticActivityProgression(t *testing.T) {
	t.Parallel()
	for _, initial := range []string{"RouteActive", "ActivityIssued", "AwaitingResponse"} {
		initial := initial
		t.Run(initial, func(t *testing.T) {
			t.Parallel()
			state := initial
			var asked atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/current":
					writeJSONTest(w, http.StatusOK, commandSessionView(state, "open", "", false, false))
				case r.Method == http.MethodPost && r.URL.Path == "/v1/knowledge/retrievals":
					writeJSONTest(w, http.StatusOK, commandRetrieval(false, false))
				case r.Method == http.MethodPost && r.URL.Path == "/v1/tutoring/proposals":
					var request api.TutoringProposalRequest
					_ = json.NewDecoder(r.Body).Decode(&request)
					writeJSONTest(w, http.StatusCreated, commandProposal(request, "free_answer", "open"))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions"):
					var discriminator struct {
						Action string `json:"action"`
					}
					_ = json.NewDecoder(r.Body).Decode(&discriminator)
					switch discriminator.Action {
					case "ask_free_question":
						asked.Add(1)
						state = "FreeQuestion"
					case "record_free_answer":
						state = "FreeAnswer"
					default:
						t.Fatalf("unexpected action %s", discriminator.Action)
					}
					writeJSONTest(w, http.StatusCreated, commandOperationResult())
				default:
					t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			configStore, credentialStore := pairedStores(server.URL, "token")
			app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{lines: []string{":ask follow up", ":quit"}})
			if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitOK {
				t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
			}
			if asked.Load() != 1 {
				t.Fatalf("asked=%d", asked.Load())
			}
		})
	}
}

func TestLearnVersionConflictRefreshDoesNotReplayAnswer(t *testing.T) {
	t.Parallel()
	var actionCalls atomic.Int32
	var currentCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/current":
			if currentCalls.Add(1) == 1 {
				writeJSONTest(w, http.StatusOK, commandSessionView("AwaitingResponse", "open", "", false, false))
				return
			}
			other := commandSessionView("Completed", "open", "", false, false)
			other.Session.SessionID = "11000000-0000-4000-8000-000000000099"
			writeJSONTest(w, http.StatusOK, other)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/"+commandSessionID:
			writeJSONTest(w, http.StatusOK, commandSessionView("Completed", "open", "", false, false))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions"):
			actionCalls.Add(1)
			writeJSONTest(w, http.StatusConflict, api.ErrorResponse{
				Error:    api.ErrorBody{Code: "version_conflict", Message: "changed", RequestID: "request-version"},
				Conflict: &api.LearningConflict{AggregateType: "session", AggregateID: commandSessionID, ExpectedVersion: 8, CurrentVersion: 9, AsOfEventSeq: 30},
			})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{lines: []string{"answer once", "", ":quit"}})
	if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if actionCalls.Load() != 1 || !strings.Contains(errOut.String(), "previous input was not replayed") {
		t.Fatalf("calls=%d err=%q", actionCalls.Load(), errOut.String())
	}
}

func TestLearnObjectiveAndOpenAssessmentPaths(t *testing.T) {
	t.Parallel()
	for _, activityType := range []string{"objective", "open"} {
		activityType := activityType
		t.Run(activityType, func(t *testing.T) {
			t.Parallel()
			state := "Evaluating"
			var proposalCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/current":
					writeJSONTest(w, http.StatusOK, commandSessionView(state, activityType, "accepted", false, false))
				case r.Method == http.MethodPost && r.URL.Path == "/v1/tutoring/proposals":
					proposalCalls.Add(1)
					var request api.TutoringProposalRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					if request.ProposalType != "assessment" {
						t.Fatalf("proposal type=%s", request.ProposalType)
					}
					writeJSONTest(w, http.StatusCreated, commandProposal(request, "assessment", activityType))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions"):
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					_, hasProposal := body["proposal_id"]
					if hasProposal != (activityType == "open") {
						t.Fatalf("type=%s has_proposal=%t body=%v", activityType, hasProposal, body)
					}
					state = "Feedback"
					writeJSONTest(w, http.StatusCreated, commandOperationResult())
				default:
					t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			configStore, credentialStore := pairedStores(server.URL, "token")
			app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{lines: []string{":quit"}})
			if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitOK {
				t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
			}
			want := int32(0)
			if activityType == "open" {
				want = 1
			}
			if proposalCalls.Load() != want {
				t.Fatalf("proposal calls=%d want=%d", proposalCalls.Load(), want)
			}
		})
	}
}

func TestLearnProvisionalQuitDoesNotSendForbiddenAction(t *testing.T) {
	t.Parallel()
	var mutations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/current" {
			writeJSONTest(w, http.StatusOK, commandSessionView("Feedback", "open", "provisional", true, false))
			return
		}
		mutations.Add(1)
		t.Fatalf("provisional quit sent %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{lines: []string{":quit"}})
	if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if mutations.Load() != 0 || !strings.Contains(out.String(), "provisional") || !strings.Contains(out.String(), "confirm") {
		t.Fatalf("mutations=%d out=%q", mutations.Load(), out.String())
	}
}

func TestAssessmentConfirmOverrideAndVoid(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"confirm", "override", "void"} {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			disposition := "provisional"
			var receivedKind string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/current":
					writeJSONTest(w, http.StatusOK, commandSessionView("Feedback", "open", disposition, true, false))
				case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/decisions"):
					var discriminator struct {
						Kind string `json:"kind"`
					}
					data, _ := io.ReadAll(r.Body)
					if err := json.Unmarshal(data, &discriminator); err != nil {
						t.Fatal(err)
					}
					receivedKind = discriminator.Kind
					if kind == "override" {
						var request api.AssessmentOverrideRequest
						if err := json.Unmarshal(data, &request); err != nil {
							t.Fatal(err)
						}
						original := commandAssessment().Items[0]
						if len(request.Items) != 1 || request.Items[0].AnswerQuoteSHA256 != original.AnswerQuoteSHA256 || request.Items[0].KnowledgeQuoteSHA256 != original.KnowledgeQuoteSHA256 || request.Items[0].Conclusion != "partial" {
							t.Fatalf("override changed immutable evidence: %+v", request.Items)
						}
					}
					switch kind {
					case "confirm":
						disposition = "accepted"
					case "override":
						disposition = "overridden"
					case "void":
						disposition = "voided"
					}
					writeJSONTest(w, http.StatusCreated, commandDecisionOperationResult(disposition))
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/nodes/"):
					writeJSONTest(w, http.StatusOK, commandNodeView())
				case r.Method == http.MethodGet && r.URL.Path == "/v1/learning/evidence":
					writeJSONTest(w, http.StatusOK, api.EvidencePage{Metadata: commandMetadata(), Items: []api.AcceptedEvidence{}})
				case r.Method == http.MethodGet && r.URL.Path == "/v1/learning/reviews":
					writeJSONTest(w, http.StatusOK, api.ReviewsPage{Metadata: commandMetadata(), Items: []api.ReviewSchedule{}})
				default:
					t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			lines := []string{}
			if kind == "override" {
				lines = []string{"manual correction", "partial", ""}
			} else if kind == "void" {
				lines = []string{"insufficient basis"}
			}
			configStore, credentialStore := pairedStores(server.URL, "token")
			app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{lines: lines})
			if exit := app.Run(t.Context(), []string{"assessment", kind}); exit != ExitOK {
				t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
			}
			if receivedKind != kind || !strings.Contains(out.String(), disposition) {
				t.Fatalf("kind=%s received=%s disposition=%s out=%q", kind, receivedKind, disposition, out.String())
			}
		})
	}
}

func TestLearnFreeQuestionCreatesAnswerThenQuits(t *testing.T) {
	t.Parallel()
	state := "FreeQuestion"
	var proposalType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/current":
			writeJSONTest(w, http.StatusOK, commandSessionView(state, "open", "", false, false))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/knowledge/retrievals":
			writeJSONTest(w, http.StatusOK, commandRetrieval(false, false))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tutoring/proposals":
			var request api.TutoringProposalRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			proposalType = request.ProposalType
			writeJSONTest(w, http.StatusCreated, commandProposal(request, "free_answer", "open"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions"):
			state = "FreeAnswer"
			writeJSONTest(w, http.StatusCreated, commandOperationResult())
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{lines: []string{":quit"}})
	if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if proposalType != "free_answer" || !strings.Contains(out.String(), "not scored") {
		t.Fatalf("proposal=%s out=%q", proposalType, out.String())
	}
}

func TestLearnAttachedQuizRequiresExplicitResumeAfterFeedback(t *testing.T) {
	t.Parallel()
	state := "FreeAnswer"
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/current":
			attached := state == "ActivityIssued" || state == "AwaitingResponse" || state == "Evaluating" || state == "Feedback"
			disposition := ""
			if state == "Feedback" {
				disposition = "accepted"
			}
			writeJSONTest(w, http.StatusOK, commandSessionView(state, "objective", disposition, false, attached))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/knowledge/retrievals":
			writeJSONTest(w, http.StatusOK, commandRetrieval(false, false))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tutoring/proposals":
			var request api.TutoringProposalRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			writeJSONTest(w, http.StatusCreated, commandProposal(request, "activity", "objective"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions"):
			var discriminator struct {
				Action string `json:"action"`
			}
			_ = json.NewDecoder(r.Body).Decode(&discriminator)
			actions = append(actions, discriminator.Action)
			switch discriminator.Action {
			case "convert_free_answer_to_quiz":
				state = "ActivityIssued"
			case "present_activity":
				state = "AwaitingResponse"
			case "submit_attempt":
				state = "Evaluating"
			case "record_assessment":
				state = "Feedback"
			case "acknowledge_feedback":
				state = "FreeAnswer"
			case "resume_focus":
				state = "Completed"
			default:
				t.Fatalf("unexpected action %s", discriminator.Action)
			}
			writeJSONTest(w, http.StatusCreated, commandOperationResult())
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{lines: []string{":quiz", "", "quiz answer", "", "", ""}})
	if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q actions=%v", exit, out.String(), errOut.String(), actions)
	}
	want := []string{"convert_free_answer_to_quiz", "present_activity", "submit_attempt", "record_assessment", "acknowledge_feedback", "resume_focus"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("actions=%v want=%v", actions, want)
	}
}

func TestAssessmentProposalRequestIncludesAttachedFocusContext(t *testing.T) {
	view := commandSessionView("Evaluating", "open", "", false, true)
	question := api.FreeQuestion{
		FreeQuestionID: commandQuestionID, SessionID: commandSessionID,
		FocusFrameID: "bb000000-0000-4000-8000-000000000001", SessionAggregateVersion: 8,
		Text: "How does it connect?", KnowledgeRevisionID: commandKnowledgeID,
		References: []api.FrozenReference{}, ActorDeviceID: testDeviceID, ReceivedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
	}
	answer := api.FreeAnswer{
		FreeAnswerID: commandAnswerID, SessionID: commandSessionID,
		FocusFrameID: question.FocusFrameID, FreeQuestionID: question.FreeQuestionID,
		Text: "It connects through the canonical note.", KnowledgeRevisionID: commandKnowledgeID,
		References: []api.FrozenReference{}, SourceProposalID: "cc000000-0000-4000-8000-000000000002", ReceivedAt: question.ReceivedAt,
	}
	view.WorkItem.FreeQuestion = &question
	view.WorkItem.FreeAnswer = &answer

	request, err := assessmentProposalRequest(view, "e0000000-0000-4000-8000-000000000010")
	if err != nil {
		t.Fatal(err)
	}
	if request.FreeQuestionID != question.FreeQuestionID || request.FreeAnswerID != answer.FreeAnswerID || request.FocusFrameID != question.FocusFrameID {
		t.Fatalf("attached assessment context was not frozen")
	}
}

func TestLearnDegradedRedactedAndModelUnavailable(t *testing.T) {
	t.Parallel()
	t.Run("degraded retrieval declined", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/tutoring/sessions/current":
				writeJSONTest(w, http.StatusOK, commandSessionView("Diagnostic", "open", "", false, false))
			case "/v1/knowledge/revisions/head":
				writeJSONTest(w, http.StatusOK, api.HeadResponse{Revision: testRevision()})
			case "/v1/knowledge/retrievals":
				writeJSONTest(w, http.StatusOK, commandRetrieval(true, true))
			default:
				t.Fatalf("unexpected %s", r.URL.Path)
			}
		}))
		defer server.Close()
		configStore, credentialStore := pairedStores(server.URL, "token")
		app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{confirmed: false})
		if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitInput || !strings.Contains(errOut.String(), "retrieval_degraded") {
			t.Fatalf("exit=%d err=%q", exit, errOut.String())
		}
	})
	t.Run("stale proposal refreshes without action", func(t *testing.T) {
		var sessionReads atomic.Int32
		var actions atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/tutoring/sessions/current":
				if sessionReads.Add(1) == 1 {
					writeJSONTest(w, http.StatusOK, commandSessionView("Diagnostic", "open", "", false, false))
				} else {
					writeJSONTest(w, http.StatusOK, commandSessionView("Completed", "open", "", false, false))
				}
			case "/v1/knowledge/revisions/head":
				writeJSONTest(w, http.StatusOK, api.HeadResponse{Revision: testRevision()})
			case "/v1/knowledge/retrievals":
				writeJSONTest(w, http.StatusOK, commandRetrieval(false, false))
			case "/v1/tutoring/proposals":
				writeJSONTest(w, http.StatusConflict, api.ErrorResponse{Error: api.ErrorBody{Code: "stale_proposal", Message: "changed", RequestID: "request-stale"}})
			default:
				actions.Add(1)
				t.Fatalf("unexpected %s", r.URL.Path)
			}
		}))
		defer server.Close()
		configStore, credentialStore := pairedStores(server.URL, "token")
		app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
		if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitOK || actions.Load() != 0 || !strings.Contains(errOut.String(), "stale_proposal") || !strings.Contains(out.String(), "completed") {
			t.Fatalf("exit=%d actions=%d out=%q err=%q", exit, actions.Load(), out.String(), errOut.String())
		}
	})
	t.Run("model unavailable preserves state", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/tutoring/sessions/current":
				writeJSONTest(w, http.StatusOK, commandSessionView("Diagnostic", "open", "", false, false))
			case "/v1/knowledge/revisions/head":
				writeJSONTest(w, http.StatusOK, api.HeadResponse{Revision: testRevision()})
			case "/v1/knowledge/retrievals":
				writeJSONTest(w, http.StatusOK, commandRetrieval(false, false))
			case "/v1/tutoring/proposals":
				writeJSONTest(w, http.StatusServiceUnavailable, api.ErrorResponse{Error: api.ErrorBody{Code: "model_unavailable", Message: "offline", RequestID: "request-model"}})
			default:
				t.Fatalf("unexpected %s", r.URL.Path)
			}
		}))
		defer server.Close()
		configStore, credentialStore := pairedStores(server.URL, "token")
		app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
		if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitUnavailable || !strings.Contains(errOut.String(), "model") || strings.Contains(out.String(), "applied") {
			t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
		}
	})
	t.Run("content redacted", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSONTest(w, http.StatusServiceUnavailable, api.ErrorResponse{Error: api.ErrorBody{Code: "content_redacted", Message: "redacted", RequestID: "request-redacted"}})
		}))
		defer server.Close()
		configStore, credentialStore := pairedStores(server.URL, "token")
		app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
		if exit := app.Run(t.Context(), []string{"learn"}); exit != ExitUnavailable || !strings.Contains(errOut.String(), "content_redacted") || out.Len() != 0 {
			t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
		}
	})
}

func commandSessionView(state, activityType, disposition string, confirmable, attached bool) api.SessionView {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	goal := api.GoalRevision{GoalRevisionID: commandGoalRevision, GoalID: "22000000-0000-4000-8000-000000000002", Revision: 1, Text: "Learn from imported notes", Source: "go-cli-m1", ActorDeviceID: testDeviceID, CreatedAt: now}
	route := api.RouteRevision{
		RouteRevisionID: commandRouteRevision, RouteID: "33000000-0000-4000-8000-000000000002", Revision: 1,
		GoalRevisionID: commandGoalRevision, KnowledgeRevisionID: commandKnowledgeID, RoutePolicyVersion: "route-v1",
		SourceProposalID: "33000000-0000-4000-8000-000000000003", CreatedAt: now,
		Steps: []api.RouteStep{{RouteStepID: commandStepID, Ordinal: 0, NodeID: commandNodeID, NodeRevisionID: commandNodeRevision, TeachingIntent: "Explain one concept", CompletionCondition: "Answer once"}},
	}
	reference := api.KnowledgeReference{KnowledgeRevisionID: commandKnowledgeID, NodeID: commandNodeID, NodeRevisionID: commandNodeRevision, DocumentRevisionID: "44000000-0000-4000-8000-000000000002", Range: api.LearningSourceRange{Start: 0, End: 14}, Slice: "canonical note", SliceSHA256: strings.Repeat("a", 64)}
	rubric := api.Rubric{RubricRevision: "rubric-v1", Items: []api.RubricItem{{RubricItemID: "criterion-1", Criterion: "States the concept"}}}
	if activityType == "objective" {
		rubric.ObjectiveRule = &api.ObjectiveRule{AcceptedAnswers: []string{"quiz answer"}, TrimSpace: true}
	}
	activity := api.Activity{
		ActivityID: commandActivityID, Revision: 1, SessionID: commandSessionID, GoalRevisionID: commandGoalRevision,
		RouteRevisionID: commandRouteRevision, RouteStepID: commandStepID, KnowledgeRevisionID: commandKnowledgeID,
		TargetNodeID: commandNodeID, TargetNodeRevisionID: commandNodeRevision, KnowledgeReferences: []api.KnowledgeReference{reference},
		Prompt: "State the concept.", Type: activityType, Rubric: rubric, Difficulty: 2, AllowedHelp: []string{"none", "hint"},
		ActivityPolicyVersion: "activity-v1", AssessmentPolicyVersion: "assessment-v1", ReviewPolicyVersion: "review-v1", CreatedAt: now,
	}
	if attached {
		activity.AttachedFreeQuestionID, activity.AttachedFreeAnswerID = commandQuestionID, commandAnswerID
	}
	attempt := api.Attempt{AttemptID: commandAttemptID, SessionID: commandSessionID, ActivityID: commandActivityID, ActivityRevision: 1, AnswerPayloadID: "77000000-0000-4000-8000-000000000002", Answer: "quiz answer", AnswerSHA256: strings.Repeat("b", 64), Help: "none", ActorDeviceID: testDeviceID, ReceivedAt: now}
	assessment := commandAssessment()
	decision := api.AssessmentDecision{DecisionID: "99000000-0000-4000-8000-000000000001", AssessmentID: commandAssessmentID, Version: 1, Disposition: disposition, Items: assessment.Items, ActorDeviceID: testDeviceID, CreatedAt: now}
	if disposition == "accepted" {
		decision.ProducedEvidenceID = "ee000000-0000-4000-8000-000000000001"
	}
	question := api.FreeQuestion{FreeQuestionID: commandQuestionID, SessionID: commandSessionID, FocusFrameID: "bb000000-0000-4000-8000-000000000001", SessionAggregateVersion: 8, Text: "How does it connect?", KnowledgeRevisionID: commandKnowledgeID, References: []api.FrozenReference{}, ActorDeviceID: testDeviceID, ReceivedAt: now}
	answer := api.FreeAnswer{FreeAnswerID: commandAnswerID, SessionID: commandSessionID, FocusFrameID: question.FocusFrameID, FreeQuestionID: commandQuestionID, Text: "It connects through the canonical note.", KnowledgeRevisionID: commandKnowledgeID, References: []api.FrozenReference{}, SourceProposalID: "cc000000-0000-4000-8000-000000000002", ReceivedAt: now}
	item := &api.SessionWorkItem{AllowedActions: []string{}, AllowedAssessmentDecisions: []string{}, GoalRevision: &goal}
	focus := api.FocusContext{GoalRevisionID: commandGoalRevision}
	switch state {
	case "GoalReady":
		item.AllowedActions = []string{"start_diagnostic", "switch_goal"}
	case "Diagnostic":
		item.AllowedActions = []string{"apply_route", "switch_goal"}
	case "RouteActive":
		item.RouteRevision = &route
		item.AllowedActions = []string{"issue_activity", "record_exposure", "ask_free_question", "complete_session", "switch_goal"}
		focus = commandFocus(false)
	case "ActivityIssued", "AwaitingResponse":
		item.RouteRevision, item.Activity = &route, &activity
		item.AllowedActions = []string{"present_activity", "ask_free_question", "end_activity", "switch_goal"}
		if state == "AwaitingResponse" {
			item.AllowedActions = []string{"submit_attempt", "ask_free_question", "end_activity", "switch_goal"}
		}
		focus = commandFocus(false)
		focus.ActivityID = commandActivityID
	case "Evaluating":
		item.RouteRevision, item.Activity, item.Attempt = &route, &activity, &attempt
		item.AllowedActions = []string{"record_assessment", "end_activity", "switch_goal"}
		focus = commandFocus(true)
	case "Feedback":
		item.RouteRevision, item.Activity, item.Attempt, item.Assessment, item.AssessmentDecision = &route, &activity, &attempt, &assessment, &decision
		if disposition == "provisional" {
			item.AllowedActions = []string{}
			item.AllowedAssessmentDecisions = []string{"override", "void"}
			if confirmable {
				item.AllowedAssessmentDecisions = []string{"confirm", "override", "void"}
			}
		} else {
			item.AllowedActions = []string{"acknowledge_feedback", "end_activity", "switch_goal"}
			item.AllowedAssessmentDecisions = []string{"override", "void"}
		}
		focus = commandFocus(true)
	case "FreeQuestion":
		item.FreeQuestion = &question
		item.AllowedActions = []string{"record_free_answer", "resume_focus", "switch_goal"}
		focus = commandFocus(false)
	case "FreeAnswer":
		item.FreeQuestion, item.FreeAnswer = &question, &answer
		item.AllowedActions = []string{"ask_free_question", "convert_free_answer_to_quiz", "resume_focus", "switch_goal"}
		focus = commandFocus(false)
	case "Completed":
		item = nil
	}
	return api.SessionView{
		Metadata:            commandMetadata(),
		Session:             api.TutoringSession{SessionID: commandSessionID, State: state, AggregateVersion: 8, Focus: focus, AttachedQuiz: attached, CompletedRoute: state == "Completed"},
		EstimatedActiveTime: api.ActiveTimeEstimate{DurationSeconds: 240, Estimated: true, AlgorithmVersion: "active-time-v1", SampleCount: 4},
		WorkItem:            item,
	}
}

func commandFocus(withAttempt bool) api.FocusContext {
	focus := api.FocusContext{GoalRevisionID: commandGoalRevision, RouteRevisionID: commandRouteRevision, RouteStepID: commandStepID, KnowledgeRevisionID: commandKnowledgeID, FocusNodeRevisionID: commandNodeRevision, ActivityID: commandActivityID}
	if withAttempt {
		focus.AttemptID = commandAttemptID
	}
	return focus
}

func commandAssessment() api.AssessmentArtifact {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	item := api.AssessmentItem{RubricItemID: "criterion-1", Conclusion: "pass", AnswerQuote: "quiz answer", AnswerRange: api.LearningSourceRange{Start: 0, End: 11}, AnswerQuoteSHA256: strings.Repeat("c", 64), KnowledgeReferenceID: commandNodeRevision, KnowledgeQuote: "canonical note", KnowledgeRange: api.LearningSourceRange{Start: 0, End: 14}, KnowledgeQuoteSHA256: strings.Repeat("d", 64)}
	return api.AssessmentArtifact{AssessmentID: commandAssessmentID, SessionID: commandSessionID, AttemptID: commandAttemptID, ActivityID: commandActivityID, ActivityRevision: 1, Items: []api.AssessmentItem{item}, RubricComplete: true, Confidence: 900, RiskFlags: []string{}, ModelID: "fake-model", ModelParameters: map[string]any{}, PromptRevision: "assessment-v1", ProposalInputHash: strings.Repeat("e", 64), Attempts: 1, AttemptCategories: []string{"initial"}, CreatedAt: now}
}

func commandMetadata() api.ProjectionMetadata {
	return api.ProjectionMetadata{AsOfEventSeq: 30, ProjectionVersion: "projection-v1", MasteryReducerVersion: "mastery-v1", AssessmentPolicyVersion: "assessment-v1", ReviewPolicyVersion: "review-v1", KnowledgeRevisionID: commandKnowledgeID, Generation: "dd000000-0000-4000-8000-000000000001", ReasonCodes: []string{}}
}

func commandRetrieval(degraded, truncated bool) api.KnowledgeRetrievalResult {
	return api.KnowledgeRetrievalResult{
		KnowledgeRevisionID: commandKnowledgeID, RetrieverVersion: "retriever-v1", SelectorVersion: "selector-v1", QueryContextSchemaVersion: "query-context-v1",
		SummarySnapshot: []string{}, DocumentShortlist: []string{}, Trace: []api.RetrievalTrace{}, Degraded: degraded, Truncated: truncated,
		Hits: []api.RetrievalHit{{DocumentID: "44000000-0000-4000-8000-000000000003", DocumentRevisionID: "44000000-0000-4000-8000-000000000002", NodeID: commandNodeID, NodeRevisionID: commandNodeRevision, Path: "note.md", HeadingRange: commandSourceRange(), LocalBodyRange: commandSourceRange(), SectionRange: commandSourceRange(), CanonicalSlice: "canonical note", SliceSHA256: strings.Repeat("a", 64), Provenance: "canonical_markdown"}},
	}
}

func commandSourceRange() api.SourceRange {
	return api.SourceRange{Start: 0, End: 14, StartLine: 1, EndLine: 1}
}

func commandProposal(request api.TutoringProposalRequest, proposalType, activityType string) api.TutoringProposal {
	proposal := api.TutoringProposal{
		ProposalID: "fa000000-0000-4000-8000-000000000001", SchemaVersion: 1, InputHash: commandProposalHash(request),
		ProposalType: request.ProposalType, AggregateType: request.AggregateType, AggregateID: request.AggregateID, AggregateVersion: request.AggregateVersion,
		GoalRevisionID: request.GoalRevisionID, RouteRevisionID: request.RouteRevisionID, ActivityID: request.ActivityID, AttemptID: request.AttemptID,
		KnowledgeRevisionID: request.KnowledgeRevisionID, FrozenRequest: request, ModelID: "fake-model", ModelParameters: map[string]any{},
		PromptRevision: "prompt-v1", AttemptCategories: []string{"initial"}, CreatedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
	}
	switch proposalType {
	case "assessment":
		assessment := commandAssessment()
		assessment.ProposalInputHash = proposal.InputHash
		proposal.Assessment = &assessment
	case "activity":
		view := commandSessionView("ActivityIssued", activityType, "", false, true)
		activity := view.WorkItem.Activity
		proposal.Activity = &api.ActivityProposal{Prompt: activity.Prompt, Type: activity.Type, Rubric: activity.Rubric, Difficulty: activity.Difficulty, AllowedHelp: activity.AllowedHelp, KnowledgeReferences: activity.KnowledgeReferences}
	case "free_answer":
		proposal.Text = &api.TextProposal{Text: "It connects through the canonical note.", KnowledgeReferences: []api.KnowledgeReference{{KnowledgeRevisionID: commandKnowledgeID, NodeID: commandNodeID, NodeRevisionID: commandNodeRevision, DocumentRevisionID: "44000000-0000-4000-8000-000000000002", Range: api.LearningSourceRange{Start: 0, End: 14}, Slice: "canonical note", SliceSHA256: strings.Repeat("a", 64)}}}
	}
	return proposal
}

func commandOperationResult() api.SessionOperationResult {
	return sessionOperationResult(commandSessionID, 9, "GoalReady")
}

func sessionOperationResult(sessionID string, version int64, state string) api.SessionOperationResult {
	return api.SessionOperationResult{
		Status: "succeeded", AggregateType: "session", AggregateID: sessionID, AggregateVersion: version,
		FirstEventSeq: 30, LastEventSeq: 30, ProjectionAsOfEventSeq: 30,
		Result: api.TutoringSession{SessionID: sessionID, State: state, AggregateVersion: version, Focus: api.FocusContext{}, AttachedQuiz: false, CompletedRoute: state == "Completed"},
	}
}

func commandDecisionOperationResult(disposition string) api.AssessmentDecisionOperationResult {
	artifact := commandAssessment()
	decision := api.AssessmentDecision{
		DecisionID: "99000000-0000-4000-8000-000000000002", AssessmentID: commandAssessmentID, Version: 2,
		Disposition: disposition, Items: artifact.Items, ActorDeviceID: testDeviceID, CreatedAt: artifact.CreatedAt,
		ReplacesDecisionID: "99000000-0000-4000-8000-000000000001",
	}
	return api.AssessmentDecisionOperationResult{
		Status: "succeeded", AggregateType: "session", AggregateID: commandSessionID, AggregateVersion: 9,
		FirstEventSeq: 30, LastEventSeq: 30, ProjectionAsOfEventSeq: 30, EvidenceDisposition: disposition, Result: decision,
	}
}

func commandProposalHash(request api.TutoringProposalRequest) string {
	encoded, _ := json.Marshal(request)
	var normalized any
	_ = json.Unmarshal(encoded, &normalized)
	encoded, _ = json.Marshal(normalized)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func commandNodeView() api.NodeView {
	return api.NodeView{
		Metadata: commandMetadata(),
		Node: api.NodeReduction{
			Mastery:        api.MasteryProjection{NodeRevisionID: commandNodeRevision, State: "learning", BaselineState: "unseen", Kinds: api.KindCounts{}, Outcomes: api.OutcomeCounts{}, Help: api.HelpCounts{}, UncertaintyReasons: []string{}, ReducerVersion: "mastery-v1"},
			Misconceptions: []api.MisconceptionHypothesis{},
		},
		Evidence: []api.AcceptedEvidence{},
	}
}
