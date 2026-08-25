package command

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func TestGoalSetCreatesSessionOrConfirmsActiveSwitch(t *testing.T) {
	t.Parallel()
	for _, active := range []bool{false, true} {
		active := active
		name := "new session"
		if active {
			name = "active switch"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hasSession := active
			goal := api.GoalRevision{}
			var switchCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/current":
					if !hasSession {
						writeJSONTest(w, http.StatusNotFound, api.ErrorResponse{Error: api.ErrorBody{Code: "not_found", Message: "none", RequestID: "request-none"}})
						return
					}
					view := commandSessionView("GoalReady", "open", "", false, false)
					if goal.GoalRevisionID != "" {
						view.WorkItem.GoalRevision = &goal
						view.Session.Focus.GoalRevisionID = goal.GoalRevisionID
					}
					if active && switchCalls.Load() == 0 {
						view = commandSessionView("RouteActive", "open", "", false, false)
					}
					writeJSONTest(w, http.StatusOK, view)
				case r.Method == http.MethodPost && r.URL.Path == "/v1/learning/goals":
					var request api.LearningGoalRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					if request.Source != "go-cli-m1" || request.Text != "Study the imported topic" {
						t.Fatalf("goal request=%+v", request)
					}
					goal = api.GoalRevision{GoalRevisionID: "ab000000-0000-4000-8000-000000000001", GoalID: request.AggregateID, Revision: 1, Text: request.Text, Source: request.Source, ActorDeviceID: testDeviceID, CreatedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)}
					writeJSONTest(w, http.StatusCreated, api.GoalOperationResult{Status: "succeeded", AggregateType: "goal", AggregateID: request.AggregateID, AggregateVersion: 1, FirstEventSeq: 1, LastEventSeq: 1, ProjectionAsOfEventSeq: 1, Result: goal})
				case r.Method == http.MethodPost && r.URL.Path == "/v1/tutoring/sessions":
					var request api.TutoringSessionRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					hasSession = true
					writeJSONTest(w, http.StatusCreated, api.SessionOperationResult{
						Status: "succeeded", AggregateType: "session", AggregateID: request.AggregateID, AggregateVersion: 1,
						FirstEventSeq: 1, LastEventSeq: 1, ProjectionAsOfEventSeq: 1,
						Result: api.TutoringSession{SessionID: request.AggregateID, State: "GoalReady", AggregateVersion: 1, Focus: api.FocusContext{GoalRevisionID: request.GoalRevisionID}},
					})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions"):
					var request api.ActionSwitchGoalRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					if request.Action != "switch_goal" || request.GoalRevisionID != goal.GoalRevisionID {
						t.Fatalf("switch request=%+v", request)
					}
					switchCalls.Add(1)
					writeJSONTest(w, http.StatusCreated, commandOperationResult())
				default:
					t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			configStore, credentialStore := pairedStores(server.URL, "token")
			app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{confirmed: true})
			if exit := app.Run(t.Context(), []string{"goal", "set", "Study", "the", "imported", "topic"}); exit != ExitOK {
				t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
			}
			if active && switchCalls.Load() != 1 {
				t.Fatalf("switch calls=%d", switchCalls.Load())
			}
			if !active && switchCalls.Load() != 0 {
				t.Fatalf("unexpected switch calls=%d", switchCalls.Load())
			}
			if configStore.saveCalls != 0 || credentialStore.saveCalls != 0 {
				t.Fatalf("goal/session content was persisted locally")
			}
		})
	}
}

func TestStableLearningQueriesAndStaleCursorRestart(t *testing.T) {
	t.Parallel()
	var staleReturned atomic.Bool
	var currentOnlyCalls atomic.Int32
	route := *commandSessionView("RouteActive", "open", "", false, false).WorkItem.RouteRevision
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tutoring/sessions/current":
			writeJSONTest(w, http.StatusNotFound, api.ErrorResponse{Error: api.ErrorBody{Code: "not_found", Message: "none", RequestID: "request-none"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/learning/routes":
			if r.URL.Query().Get("current_only") != "true" {
				t.Fatalf("current_only=%q", r.URL.Query().Get("current_only"))
			}
			currentOnlyCalls.Add(1)
			if r.URL.Query().Get("cursor") == "stale" && !staleReturned.Swap(true) {
				writeJSONTest(w, http.StatusConflict, api.ErrorResponse{Error: api.ErrorBody{Code: "stale_cursor", Message: "changed", RequestID: "request-cursor"}})
				return
			}
			writeJSONTest(w, http.StatusOK, api.RoutesPage{Metadata: commandMetadata(), Items: []api.RouteProjection{{Route: route, EventSeq: 30, Current: true}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/learning/projections/status":
			writeJSONTest(w, http.StatusOK, api.ProjectionStatus{Metadata: commandMetadata(), CommittedEventHighWater: 30, Fingerprint: strings.Repeat("f", 64), ActiveGenerationID: commandMetadata().Generation})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v1/learning/nodes/"):
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
	for _, args := range [][]string{{"route", "--cursor", "stale"}, {"progress"}, {"evidence"}, {"reviews"}} {
		configStore, credentialStore := pairedStores(server.URL, "token")
		app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
		if exit := app.Run(t.Context(), args); exit != ExitOK {
			t.Fatalf("args=%v exit=%d out=%q err=%q", args, exit, out.String(), errOut.String())
		}
		if args[0] == "route" && !strings.Contains(errOut.String(), "stale_cursor") {
			t.Fatalf("stale warning missing: %q", errOut.String())
		}
	}
	if currentOnlyCalls.Load() < 3 {
		t.Fatalf("current_only calls=%d", currentOnlyCalls.Load())
	}
}
