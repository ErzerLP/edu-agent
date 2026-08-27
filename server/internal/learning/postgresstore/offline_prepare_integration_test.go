package postgresstore_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	"github.com/edu-agent/edu-agent/server/internal/transport/httpapi"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
)

func TestPostgreSQLOfflinePairingBootstrapFromFreshSchema(t *testing.T) {
	pool := learningIntegrationPool(t)
	store := postgresstore.New(pool, tutoringpostgres.New(pool), knowledgedb.New(pool))
	signer := offlineIntegrationSigner(t)
	service, err := learning.NewOfflineService(store, signer, signer.Origin(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := service.PairingBootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.ProtocolVersion != 1 || bootstrap.LearnerGeneration != "1" || bootstrap.ServerBaseURL != signer.Origin() || bootstrap.SignerManifest.SignerKeyID == "" {
		t.Fatalf("offline pairing bootstrap metadata=%+v", bootstrap)
	}
}

func TestPostgreSQLOfflineObjectivePrepareSyncStatus(t *testing.T) {
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO devices(id,display_name,created_at)
		VALUES($1,'offline-objective',clock_timestamp()),($2,'offline-other',clock_timestamp())`, learningDeviceOne, learningDeviceTwo); err != nil {
		t.Fatal(err)
	}
	insertLearningKnowledgeFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE knowledge_catalog SET head_revision_id=$1,updated_at=clock_timestamp() WHERE singleton_id=1`, learningKnowledgeRevision); err != nil {
		t.Fatal(err)
	}
	store := postgresstore.New(pool, tutoringpostgres.New(pool), knowledgedb.New(pool))
	goalRevisionID := "30000000-0000-4000-8000-000000000001"
	if _, err := store.Commit(ctx, goalCommit(t, learningDeviceOne, "20000000-0000-4000-8000-000000000001", goalRevisionID, 0, 1, 1)); err != nil {
		t.Fatal(err)
	}
	session, version := seedOfflinePrepareSession(t, store, goalRevisionID)
	signer := offlineIntegrationSigner(t)
	service, err := learning.NewOfflineService(store, signer, signer.Origin(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	handler := offlineIntegrationHTTPHandler(t, service, learningDeviceOne)
	prepareOperationID := "61000000-0000-4000-8000-000000000001"
	prepareRequest := learning.OfflinePrepareRequest{
		OperationID:             prepareOperationID,
		PayloadSchemaVersion:    1,
		ExpectedSessionVersion:  strconv.FormatInt(version, 10),
		TrustedManifestRevision: "0",
		TrustedManifestDigest:   learning.OfflineZeroDigest,
	}
	var prepared learning.OfflinePrepareResponse
	if statusCode := offlineIntegrationRequest(t, handler, http.MethodPost, "/v1/learning/offline/packs", prepareRequest, &prepared); statusCode != http.StatusCreated {
		t.Fatalf("initial offline prepare HTTP status=%d", statusCode)
	}
	if prepared.Replayed || len(prepared.ManifestChain) != 1 {
		t.Fatalf("initial offline prepare=%+v", prepared)
	}
	var pack learning.OfflinePackPayloadV1
	if err := json.Unmarshal(prepared.Pack.Payload, &pack); err != nil {
		t.Fatal(err)
	}
	if len(pack.Items) != 1 || !pack.Truncated || pack.TruncatedReason != "model_partial" || pack.Items[0].Activity.Type != learning.ActivityObjective {
		t.Fatalf("offline Objective bounded pack=%+v", pack)
	}
	var replayedPrepare learning.OfflinePrepareResponse
	statusCode := offlineIntegrationRequest(t, handler, http.MethodPost, "/v1/learning/offline/packs", prepareRequest, &replayedPrepare)
	if statusCode != http.StatusOK || !replayedPrepare.Replayed || string(replayedPrepare.Pack.Payload) != string(prepared.Pack.Payload) {
		t.Fatalf("offline prepare replay status=%d replayed=%v payload_equal=%v", statusCode, replayedPrepare.Replayed, string(replayedPrepare.Pack.Payload) == string(prepared.Pack.Payload))
	}
	var highWater int64
	if err := pool.QueryRow(ctx, `SELECT high_water FROM offline_device_sequence_heads WHERE device_id=$1`, learningDeviceOne).Scan(&highWater); err != nil || highWater != 1 {
		t.Fatalf("prepare sequence high water=%d err=%v", highWater, err)
	}

	item := pack.Items[0]
	var authorization learning.OfflineAuthorizationPayloadV1
	if err := json.Unmarshal(item.Authorization.Payload, &authorization); err != nil {
		t.Fatal(err)
	}
	answer := "ok"
	operationPayload, _ := json.Marshal(learning.OfflineAttemptPayload{
		Answer:       answer,
		AnswerSHA256: learning.SHA256([]byte(answer)),
		Help:         learning.HelpNone,
		Observations: []learning.OfflineObservation{{Kind: "activity_presented"}, {Kind: "answer_recorded"}},
	})
	wire := learning.OfflineOperationWireV1{
		OperationID:          authorization.OperationID,
		DeviceID:             learningDeviceOne,
		DeviceSequence:       authorization.DeviceSequence,
		SubmissionID:         authorization.SubmissionID,
		PayloadSchemaVersion: 1,
		AggregateType:        "offline_attempt",
		AggregateID:          authorization.SubmissionID,
		ExpectedVersion:      authorization.ExpectedVersion,
		OfflineActivityID:    authorization.OfflineActivityID,
		ActivityRevision:     authorization.ActivityRevision,
		Authorization:        item.Authorization.Payload,
		Signature:            item.Authorization.Signature,
		OccurredAt:           nil,
		OperationType:        learning.OfflineAttemptCompleted,
		Payload:              operationPayload,
	}
	operationBody, _ := json.Marshal(wire)
	syncRequest := learning.OfflineSyncRequest{
		SyncRequestID:        "62000000-0000-4000-8000-000000000001",
		PayloadSchemaVersion: 1,
		Operations:           []json.RawMessage{operationBody},
	}
	var synced learning.OfflineSyncResponse
	if statusCode := offlineIntegrationRequest(t, handler, http.MethodPost, "/v1/learning/offline/sync", syncRequest, &synced); statusCode != http.StatusOK || len(synced.Results) != 1 || synced.Results[0].ArchiveStatus != learning.OfflineArchivedSucceeded || synced.Results[0].AssessmentStatus != learning.OfflineAssessmentCompleted || synced.Results[0].EvidenceStatus != learning.OfflineEvidenceAccepted || synced.Results[0].Replayed || synced.Results[0].ReasonCodes == nil {
		t.Fatalf("offline Objective sync status=%d result=%+v", statusCode, synced)
	}
	var replayedSync learning.OfflineSyncResponse
	if statusCode := offlineIntegrationRequest(t, handler, http.MethodPost, "/v1/learning/offline/sync", syncRequest, &replayedSync); statusCode != http.StatusOK || len(replayedSync.Results) != 1 || !replayedSync.Results[0].Replayed || replayedSync.Results[0].Receipt == nil || replayedSync.Results[0].ReasonCodes == nil {
		t.Fatalf("offline Objective sync replay status=%d result=%+v", statusCode, replayedSync)
	}
	var status learning.OfflineOperationStatus
	if statusCode := offlineIntegrationRequest(t, handler, http.MethodGet, "/v1/learning/offline/operations/"+authorization.OperationID, nil, &status); statusCode != http.StatusOK || status.EvidenceStatus != learning.OfflineEvidenceAccepted || status.Receipt.AggregateVersion == "" || status.StatusTicket.Revision != "1" {
		t.Fatalf("offline Objective status HTTP=%d result=%+v", statusCode, status)
	}
	otherDeviceHandler := offlineIntegrationHTTPHandler(t, service, learningDeviceTwo)
	if statusCode := offlineIntegrationRequest(t, otherDeviceHandler, http.MethodGet, "/v1/learning/offline/operations/"+authorization.OperationID, nil, nil); statusCode != http.StatusNotFound {
		t.Fatalf("other device read offline operation status=%d", statusCode)
	}
	current, err := store.Session(ctx, session.ID)
	if err != nil || current.Session.State != tutoring.StateAwaitingResponse || current.Session.AggregateVer != version {
		t.Fatalf("offline Objective advanced tutoring Session: %+v err=%v", current, err)
	}
	var claims, attempts, evidence int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM offline_device_sequence_claims WHERE device_id=$1),
		  (SELECT count(*) FROM learning_attempts WHERE offline_submission_id=$2),
		  (SELECT count(*) FROM learning_evidence WHERE attempt_id=$2)`,
		learningDeviceOne, authorization.SubmissionID).Scan(&claims, &attempts, &evidence); err != nil {
		t.Fatal(err)
	}
	if claims != 1 || attempts != 1 || evidence != 1 {
		t.Fatalf("offline replay duplicated facts claims=%d attempts=%d evidence=%d", claims, attempts, evidence)
	}
}

type offlineIntegrationIdentity struct {
	deviceID string
}

func (f offlineIntegrationIdentity) ExchangePairingCode(context.Context, string, string) (identity.IssuedCredential, error) {
	return identity.IssuedCredential{}, nil
}

func (f offlineIntegrationIdentity) Authenticate(context.Context, string, string) (identity.Credential, error) {
	return identity.Credential{Device: identity.Device{ID: f.deviceID}, Scopes: []string{"learning:read", "learning:write"}}, nil
}

func (f offlineIntegrationIdentity) ListDevices(context.Context) ([]identity.Device, error) {
	return []identity.Device{{ID: f.deviceID}}, nil
}

func (f offlineIntegrationIdentity) RevokeDevice(context.Context, string) error { return nil }

type offlineIntegrationReadiness struct{}

func (offlineIntegrationReadiness) Ready(context.Context) health.Report {
	return health.Report{Status: health.StatusHealthy, Components: map[string]health.Component{}}
}

func offlineIntegrationHTTPHandler(t *testing.T, service *learning.OfflineService, deviceID string) http.Handler {
	t.Helper()
	handler, err := httpapi.New(httpapi.Options{
		Identity:      offlineIntegrationIdentity{deviceID: deviceID},
		Offline:       service,
		Readiness:     offlineIntegrationReadiness{},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		PairLimiter:   httpapi.NewFixedWindowLimiter(100, time.Minute),
		AuthLimiter:   httpapi.NewFixedWindowLimiter(100, time.Minute),
		DeviceLimiter: httpapi.NewFixedWindowLimiter(100, time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func offlineIntegrationRequest(t *testing.T, handler http.Handler, method, path string, input, output any) int {
	t.Helper()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Authorization", "Bearer integration-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if output != nil && response.Code >= 200 && response.Code < 300 {
		if err := json.Unmarshal(response.Body.Bytes(), output); err != nil {
			t.Fatalf("decode %s %s response: %v body=%s", method, path, err, response.Body.String())
		}
	}
	return response.Code
}

func seedOfflinePrepareSession(t *testing.T, store *postgresstore.Store, goalRevisionID string) (tutoring.Session, int64) {
	return seedOfflinePrepareSessionWithType(t, store, goalRevisionID, learning.ActivityObjective)
}

func seedOfflinePrepareSessionWithType(t *testing.T, store *postgresstore.Store, goalRevisionID string, activityType learning.ActivityType) (tutoring.Session, int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	sessionID := "63000000-0000-4000-8000-000000000001"
	routeRevisionID := "63000000-0000-4000-8000-000000000002"
	routeID := "63000000-0000-4000-8000-000000000003"
	stepID := "63000000-0000-4000-8000-000000000004"
	activityID := "63000000-0000-4000-8000-000000000005"
	commit := func(operationID string, expected int64, batch learning.CommandBatch) learning.OperationResult {
		result, err := store.Commit(ctx, learning.CommitRequest{
			DeviceID:     learningDeviceOne,
			Operation:    learning.OperationEnvelope{OperationID: operationID, PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: expected, Payload: json.RawMessage(`{}`)},
			RequestHash:  learning.SHA256([]byte(operationID)),
			Expectations: []learning.AggregateExpectation{{Type: "session", ID: sessionID, ExpectedVersion: expected}},
			Batch:        batch,
			ReceivedAt:   now.Add(time.Duration(expected) * time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	session := tutoring.Session{ID: sessionID, State: tutoring.StateGoalReady, Context: tutoring.FocusContext{GoalRevisionID: goalRevisionID}}
	result := commit("64000000-0000-4000-8000-000000000001", 0, learning.CommandBatch{Session: &session, TutoringState: string(session.State), Events: []learning.EventDraft{
		eventDraft(learning.EventLearningSessionStarted, sessionID, learning.SessionProjection{Session: session}),
		eventDraft(learning.EventTutoringStateChanged, sessionID, learning.SessionProjection{Session: session}),
	}})
	route := learning.RouteRevision{ID: routeRevisionID, RouteID: routeID, Revision: 1, GoalRevisionID: goalRevisionID, KnowledgeRevisionID: learningKnowledgeRevision, PolicyVersion: learning.RoutePolicyVersion, Steps: []learning.RouteStep{{ID: stepID, Ordinal: 0, NodeID: learningNodeID, NodeRevisionID: learningNodeRevisionID, TeachingIntent: "teach", CompletionCondition: "pass"}}, CreatedAt: now}
	session.State = tutoring.StateRouteActive
	session.Context = tutoring.FocusContext{GoalRevisionID: goalRevisionID, RouteRevisionID: routeRevisionID, RouteStepID: stepID, KnowledgeRevisionID: learningKnowledgeRevision, FocusNodeRevisionID: learningNodeRevisionID}
	result = commit("64000000-0000-4000-8000-000000000002", result.AggregateVersion, learning.CommandBatch{RouteRevision: &route, Session: &session, Authority: learning.AuthorityProvenance{RouteSteps: map[string]learning.KnowledgeOwner{stepID: {KnowledgeRevisionID: learningKnowledgeRevision, NodeID: learningNodeID, NodeRevisionID: learningNodeRevisionID, DocumentRevisionID: learningDocumentRevisionID}}}, TutoringState: string(session.State), Events: []learning.EventDraft{
		eventDraft(learning.EventRouteRevisionCreated, sessionID, route),
		eventDraft(learning.EventTutoringStateChanged, sessionID, learning.SessionProjection{Session: session}),
	}})
	reference := learning.KnowledgeReference{KnowledgeRevisionID: learningKnowledgeRevision, NodeID: learningNodeID, NodeRevisionID: learningNodeRevisionID, DocumentRevisionID: learningDocumentRevisionID, Range: learning.SourceRange{Start: 0, End: 5}, Slice: "topic", SliceSHA256: learning.SHA256([]byte("topic"))}
	rubric := learning.Rubric{Revision: "offline-r1", Items: []learning.RubricItem{{ID: "item-1", Criterion: "correct"}}}
	if activityType == learning.ActivityObjective {
		rubric.ObjectiveRule = &learning.ObjectiveRule{AcceptedAnswers: []string{"ok"}, TrimSpace: true}
	} else {
		rubric.Items[0].RequiredReferenceIDs = []string{learningNodeRevisionID}
	}
	activity := learning.Activity{ID: activityID, Revision: 1, SessionID: sessionID, GoalRevisionID: goalRevisionID, RouteRevisionID: routeRevisionID, RouteStepID: stepID, KnowledgeRevisionID: learningKnowledgeRevision, TargetNodeID: learningNodeID, TargetNodeRevisionID: learningNodeRevisionID, References: []learning.KnowledgeReference{reference}, Prompt: "topic?", Type: activityType, Rubric: rubric, Difficulty: 1, AllowedHelp: []learning.HelpLevel{learning.HelpNone}, ActivityPolicyVersion: learning.ActivityPolicyVersion, AssessmentPolicyVersion: learning.AssessmentPolicyVersion, ReviewPolicyVersion: learning.ReviewPolicyVersion, CreatedAt: now}
	session.State = tutoring.StateActivityIssued
	session.Context.ActivityID = &activity.ID
	result = commit("64000000-0000-4000-8000-000000000003", result.AggregateVersion, learning.CommandBatch{Activity: &activity, Session: &session, TutoringState: string(session.State), Events: []learning.EventDraft{
		eventDraft(learning.EventActivityIssued, sessionID, activity),
		eventDraft(learning.EventTutoringStateChanged, sessionID, learning.SessionProjection{Session: session}),
	}})
	session.State = tutoring.StateAwaitingResponse
	result = commit("64000000-0000-4000-8000-000000000004", result.AggregateVersion, learning.CommandBatch{Session: &session, TutoringState: string(session.State), Events: []learning.EventDraft{
		eventDraft(learning.EventActivityPresented, sessionID, activity),
		eventDraft(learning.EventTutoringStateChanged, sessionID, learning.SessionProjection{Session: session}),
	}})
	return session, result.AggregateVersion
}

func offlineIntegrationSigner(t *testing.T) *learning.Ed25519OfflineSigner {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(0x80 + index)
	}
	signer, err := learning.NewEd25519OfflineSigner("offline-integration-key", ed25519.NewKeyFromSeed(seed), "http://127.0.0.1:8080/", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}
