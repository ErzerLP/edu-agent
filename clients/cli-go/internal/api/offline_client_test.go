package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOfflineDevicePurgeUsesAuthenticatedClosedHTTPContract(t *testing.T) {
	erasureID := "10000000-0000-4000-8000-000000000010"
	deviceID := "10000000-0000-4000-8000-000000000011"
	challenge := strings.Repeat("A", 43)
	var received OfflinePurgeAckRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer offline-token" {
			t.Errorf("authorization header was not bound")
		}
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/privacy/erasures/"+erasureID+"/offline-device-purge":
			_, _ = writer.Write([]byte(`{"erasure_id":"` + erasureID + `","device_id":"` + deviceID + `","old_generation":7,"current_generation":8,"challenge_revision":1,"challenge":"` + challenge + `","issued_at":"2030-01-01T00:00:00Z","status":"pending"}`))
		case request.Method == http.MethodPost && request.URL.Path == "/v1/privacy/erasures/"+erasureID+"/offline-device-purge/ack":
			decoder := json.NewDecoder(request.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&received); err != nil {
				t.Errorf("decode acknowledgment: %v", err)
			}
			_, _ = writer.Write([]byte(`{"erasure_id":"` + erasureID + `","device_id":"` + deviceID + `","source_generation":7,"current_generation":8,"challenge_revision":1,"status":"succeeded","updated_at":"2030-01-01T00:00:01Z","stable_reason":"device_acknowledged"}`))
		default:
			t.Errorf("request=%s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "offline-token", time.Second, nil)
	task, err := client.OfflineDevicePurgeTask(context.Background(), erasureID)
	if err != nil || task == nil {
		t.Fatalf("purge task=%+v err=%v", task, err)
	}
	managedAbsent := true
	request := OfflinePurgeAckRequest{ChallengeRevision: 1, Challenge: challenge, Outcome: "succeeded", ManagedObjectsAbsent: &managedAbsent}
	response, err := client.AckOfflineDevicePurge(context.Background(), *task, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "succeeded" || received.ManagedObjectsAbsent == nil || !*received.ManagedObjectsAbsent || received.Challenge != challenge {
		t.Fatalf("response=%+v received=%+v", response, received)
	}
}

func TestOfflineAssessmentClientUsesStrictIndependentContract(t *testing.T) {
	const (
		assessmentID = "80000000-0000-4000-8000-000000000010"
		attemptID    = "70000000-0000-4000-8000-000000000010"
		submissionID = "71000000-0000-4000-8000-000000000010"
		operationID  = "90000000-0000-4000-8000-000000000010"
	)
	view := offlineAssessmentClientView(assessmentID, attemptID, submissionID)
	page := OfflineAssessmentPage{Metadata: view.Metadata, Items: []OfflineAssessmentSummary{{
		AssessmentID: assessmentID, AttemptID: attemptID, ActivityID: view.Activity.ActivityID,
		ActivityRevision: "1", SubmissionID: submissionID, AggregateVersion: "2", DispositionVersion: "1",
		Disposition: "provisional", Confidence: view.Assessment.Confidence, Confirmable: false,
		AllowedDecisions: []string{"override", "void"}, AttemptReceivedAt: view.Attempt.ReceivedAt, AssessmentCreatedAt: view.Assessment.CreatedAt,
	}}}
	var received OfflineAssessmentVoidRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer offline-token" {
			t.Errorf("authorization header was not bound")
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/learning/offline/assessments":
			if request.URL.Query().Get("status") != "provisional" || request.URL.Query().Get("limit") != "25" || request.URL.Query().Get("cursor") != "cursor-1" {
				t.Errorf("assessment query=%s", request.URL.RawQuery)
			}
			writeAPIJSON(t, writer, http.StatusOK, page)
		case request.Method == http.MethodGet && request.URL.Path == "/v1/learning/offline/assessments/"+assessmentID:
			writeAPIJSON(t, writer, http.StatusOK, view)
		case request.Method == http.MethodPost && request.URL.Path == "/v1/learning/offline/assessments/"+assessmentID+"/decisions":
			decoder := json.NewDecoder(request.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&received); err != nil {
				t.Errorf("decode offline assessment decision: %v", err)
			}
			decision := view.Decision
			decision.DecisionID = operationID
			decision.Version = 2
			decision.Disposition = "voided"
			decision.Reason = "invalid assessment"
			decision.ReplacesDecisionID = view.Decision.DecisionID
			writeAPIJSON(t, writer, http.StatusCreated, OfflineAssessmentDecisionReceipt{
				OperationID: operationID, AssessmentID: assessmentID, AttemptID: attemptID, SubmissionID: submissionID,
				AggregateVersion: "3", FirstEventSequence: "21", LastEventSequence: "21", ProjectionAsOfEventSequence: "21", Decision: decision,
			})
		default:
			t.Errorf("request=%s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, "offline-token", time.Second, nil)
	listed, err := client.OfflineAssessments(context.Background(), "cursor-1", 25, "provisional")
	if err != nil || len(listed.Items) != 1 || listed.Items[0].AssessmentID != assessmentID {
		t.Fatalf("assessment page=%+v err=%v", listed, err)
	}
	shown, err := client.OfflineAssessment(context.Background(), assessmentID)
	if err != nil || shown.SubmissionID != submissionID || shown.Attempt.OfflineSubmissionID != submissionID {
		t.Fatalf("assessment view=%+v err=%v", shown, err)
	}
	request := OfflineAssessmentVoidRequest{OfflineAssessmentDecisionBase: OfflineAssessmentDecisionBase{
		OperationID: operationID, PayloadSchemaVersion: 1, AttemptID: attemptID,
		ExpectedVersion: "2", Kind: "void", ExpectedDispositionVersion: "1",
	}, Reason: "invalid assessment"}
	receipt, err := client.DecideOfflineAssessment(context.Background(), assessmentID, request)
	if err != nil || receipt.Decision.Disposition != "voided" || received.AttemptID != attemptID || received.ExpectedVersion != "2" {
		t.Fatalf("decision receipt=%+v received=%+v err=%v", receipt, received, err)
	}
}

func TestOfflineAssessmentDecisionRequestUsesUnicodeCodePointLimits(t *testing.T) {
	const assessmentID = "80000000-0000-4000-8000-000000000010"
	base := OfflineAssessmentDecisionBase{
		OperationID: "90000000-0000-4000-8000-000000000010", PayloadSchemaVersion: 1,
		AttemptID: "70000000-0000-4000-8000-000000000010", ExpectedVersion: "2",
		ExpectedDispositionVersion: "1",
	}
	voidBase := base
	voidBase.Kind = "void"
	if err := validateOfflineAssessmentDecisionRequestContract(assessmentID, OfflineAssessmentVoidRequest{
		OfflineAssessmentDecisionBase: voidBase,
		Reason:                        strings.Repeat("界", MaxOfflineAssessmentDecisionReasonRunes),
	}); err != nil {
		t.Fatalf("boundary reason rejected: %v", err)
	}
	if err := validateOfflineAssessmentDecisionRequestContract(assessmentID, OfflineAssessmentVoidRequest{
		OfflineAssessmentDecisionBase: voidBase,
		Reason:                        strings.Repeat("界", MaxOfflineAssessmentDecisionReasonRunes+1),
	}); err == nil {
		t.Fatal("overlong reason accepted")
	}

	overrideBase := base
	overrideBase.Kind = "override"
	boundary := OfflineAssessmentOverrideRequest{
		OfflineAssessmentDecisionBase: overrideBase,
		Reason:                        strings.Repeat("由", MaxOfflineAssessmentDecisionReasonRunes),
		Items: []OfflineAssessmentOverrideItem{{
			RubricItemID: strings.Repeat("项", MaxOfflineAssessmentRubricItemIDRunes), Conclusion: "partial",
			MisconceptionCandidate: strings.Repeat("误", MaxOfflineAssessmentMisconceptionRunes),
		}},
	}
	if err := validateOfflineAssessmentDecisionRequestContract(assessmentID, boundary); err != nil {
		t.Fatalf("boundary override rejected: %v", err)
	}
	overlongRubric := boundary
	overlongRubric.Items = append([]OfflineAssessmentOverrideItem(nil), boundary.Items...)
	overlongRubric.Items[0].RubricItemID = strings.Repeat("项", MaxOfflineAssessmentRubricItemIDRunes+1)
	if err := validateOfflineAssessmentDecisionRequestContract(assessmentID, overlongRubric); err == nil {
		t.Fatal("overlong rubric item ID accepted")
	}
	overlongCandidate := boundary
	overlongCandidate.Items = append([]OfflineAssessmentOverrideItem(nil), boundary.Items...)
	overlongCandidate.Items[0].MisconceptionCandidate = strings.Repeat("误", MaxOfflineAssessmentMisconceptionRunes+1)
	if err := validateOfflineAssessmentDecisionRequestContract(assessmentID, overlongCandidate); err == nil {
		t.Fatal("overlong misconception accepted")
	}
}

func TestOfflineAssessmentClientRejectsUnknownSuccessFields(t *testing.T) {
	assessmentID := "80000000-0000-4000-8000-000000000010"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"assessment_id":"` + assessmentID + `","answer":"leak"}`))
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "offline-token", time.Second, nil).OfflineAssessment(context.Background(), assessmentID)
	var protocolError *ProtocolError
	if !errors.As(err, &protocolError) || protocolError.Category != "malformed_success_response" {
		t.Fatalf("expected closed response rejection, got %v", err)
	}
}

func offlineAssessmentClientView(assessmentID, attemptID, submissionID string) OfflineAssessmentView {
	feedback := learningSessionView("Feedback")
	activity := *feedback.WorkItem.Activity
	attempt := *feedback.WorkItem.Attempt
	assessment := *feedback.WorkItem.Assessment
	decision := *feedback.WorkItem.AssessmentDecision
	attempt.AttemptID = attemptID
	attempt.ArchiveDisposition = "offline_succeeded"
	attempt.OfflineSubmissionID = submissionID
	assessment.AssessmentID = assessmentID
	assessment.AttemptID = attemptID
	decision.AssessmentID = assessmentID
	decision.Items = assessment.Items
	return OfflineAssessmentView{
		Metadata: feedback.Metadata, SubmissionID: submissionID, AggregateVersion: "2",
		Activity: activity, Attempt: attempt, Assessment: assessment, Decision: decision,
		AllowedDecisions: []string{"override", "void"},
	}
}

func TestOfflinePrepareUsesAuthenticatedClosedHTTPContract(t *testing.T) {
	var received OfflinePrepareRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/learning/offline/packs" {
			t.Errorf("request=%s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer offline-token" {
			t.Errorf("authorization header was not bound")
		}
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"unknown":true}`))
	}))
	defer server.Close()

	digest := base64.RawURLEncoding.EncodeToString(make([]byte, sha256.Size))
	count := 1
	request := OfflinePrepareRequest{
		OperationID:             "10000000-0000-4000-8000-000000000001",
		PayloadSchemaVersion:    1,
		ExpectedSessionVersion:  "1",
		TrustedManifestRevision: "1",
		TrustedManifestDigest:   digest,
		RequestedCount:          &count,
	}
	_, status, err := NewClient(server.URL, "offline-token", time.Second, nil).PrepareOffline(context.Background(), request)
	if status != http.StatusCreated {
		t.Fatalf("status=%d", status)
	}
	var protocolError *ProtocolError
	if !errors.As(err, &protocolError) || !strings.Contains(protocolError.Category, "malformed_offline_prepare_response") {
		t.Fatalf("expected closed offline response rejection, got %v", err)
	}
	if received.OperationID != request.OperationID || received.TrustedManifestDigest != request.TrustedManifestDigest || received.RequestedCount == nil || *received.RequestedCount != count {
		t.Fatalf("received request=%+v", received)
	}
}
