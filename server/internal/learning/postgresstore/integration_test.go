package postgresstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxpostgresstore "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	learningDeviceOne          = "90000000-0000-4000-8000-000000000001"
	learningDeviceTwo          = "90000000-0000-4000-8000-000000000002"
	learningGoalID             = "10000000-0000-4000-8000-000000000001"
	learningKnowledgeRevision  = "40000000-0000-4000-8000-000000000002"
	learningDocumentID         = "41000000-0000-4000-8000-000000000001"
	learningDocumentRevisionID = "41000000-0000-4000-8000-000000000002"
	learningNodeID             = "41000000-0000-4000-8000-000000000003"
	learningNodeRevisionID     = "41000000-0000-4000-8000-000000000004"
)

func TestPostgreSQLLearningPublicLoadersRespectPersistentReadGateAfterRestart(t *testing.T) {
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'learning-loader-gate',now())`, learningDeviceOne); err != nil {
		t.Fatal(err)
	}
	store := postgresstore.New(pool, tutoringpostgres.New(pool))
	goalID := "11000000-0000-4000-8000-000000000001"
	revisionID := "31000000-0000-4000-8000-000000000001"
	request := goalCommitFor(t, learningDeviceOne, "21000000-0000-4000-8000-000000000001", goalID, revisionID, 0, 1, 1)
	if _, err := store.Commit(ctx, request); err != nil {
		t.Fatal(err)
	}
	if goal, err := store.LoadGoalRevision(ctx, revisionID); err != nil || goal.Text == "" {
		t.Fatalf("pre-barrier goal=%+v err=%v", goal, err)
	}

	erasureID := "71000000-0000-4000-8000-000000000001"
	var generation int64
	if err := pool.QueryRow(ctx, `SELECT learner_generation+1 FROM privacy_owner_generation_gates WHERE owner_kind='learning'`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `INSERT INTO privacy_erasures(id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,target_learner_generation,managed_backup_scheduled_unrecoverable_after) VALUES($1,$2,$3,decode(repeat('ab',32),'hex'),'learner_request',$2,clock_timestamp(),$4,clock_timestamp()+interval '1 day')`, erasureID, learningDeviceOne, "72000000-0000-4000-8000-000000000001", generation); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO privacy_erasure_heads(erasure_id,status,summary_version,stable_reason,updated_at) VALUES($1,'barrier_committed',1,'loader_gate_restart',clock_timestamp())`, erasureID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO privacy_redaction_barriers(erasure_id,learner_generation,redacted_through_event_seq,policy_version,reason_code,event_id,committed_at) VALUES($1,$2,0,$3,'learner_request',$4,clock_timestamp())`, erasureID, generation, privacy.PolicyVersion, "73000000-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE privacy_owner_generation_gates SET learner_generation=$2,read_open=FALSE,write_open=FALSE,active_erasure_id=$1,updated_at=clock_timestamp() WHERE owner_kind='learning'`, erasureID, generation); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	restarted := postgresstore.New(pool, tutoringpostgres.New(pool))
	loaders := map[string]func() error{
		"aggregate_version": func() error { _, err := restarted.LoadAggregateVersion(ctx, "goal", goalID); return err },
		"goal_revision":     func() error { _, err := restarted.LoadGoalRevision(ctx, revisionID); return err },
		"route_revision":    func() error { _, err := restarted.LoadRouteRevision(ctx, revisionID); return err },
		"activity":          func() error { _, err := restarted.LoadActivity(ctx, revisionID); return err },
		"attempt":           func() error { _, err := restarted.LoadAttempt(ctx, revisionID); return err },
		"assessment": func() error {
			_, _, err := restarted.LoadAssessment(ctx, revisionID)
			return err
		},
		"proposal":             func() error { _, err := restarted.LoadProposal(ctx, revisionID); return err },
		"free_question":        func() error { _, err := restarted.LoadFreeQuestion(ctx, revisionID); return err },
		"free_answer":          func() error { _, err := restarted.LoadFreeAnswer(ctx, revisionID); return err },
		"valid_evidence":       func() error { _, err := restarted.LoadValidEvidence(ctx, revisionID); return err },
		"misconceptions":       func() error { _, err := restarted.LoadMisconceptions(ctx, revisionID); return err },
		"latest_free_question": func() error { _, err := restarted.LatestFreeQuestion(ctx, revisionID); return err },
	}
	for name, load := range loaders {
		if err := load(); privacy.ErrorCode(err) != privacy.CodeContentRedacted {
			t.Fatalf("%s gate err=%v code=%q", name, err, privacy.ErrorCode(err))
		}
	}
}

func TestPostgreSQLLearningCoreDurabilityAndRebuild(t *testing.T) {
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	store := postgresstore.New(pool, tutoringpostgres.New(pool))
	if err := migrations.Check(ctx, pool); err != nil {
		t.Fatal(err)
	}
	emptyFingerprint, err := learning.ProjectionFingerprint(learning.EmptyProjection("integration-empty-generation"))
	if err != nil {
		t.Fatal(err)
	}
	emptyRebuilt, err := store.Rebuild(ctx)
	if err != nil {
		t.Fatalf("rebuild empty migrated projection: %v", err)
	}
	if emptyRebuilt.HighWater != 0 || emptyRebuilt.Metadata.AsOfEventSequence != 0 || emptyRebuilt.Metadata.Incomplete || emptyRebuilt.Metadata.Degraded || emptyRebuilt.Metadata.Rebuilding || emptyRebuilt.RebuildingGenerationID != nil || emptyRebuilt.Fingerprint != emptyFingerprint {
		t.Fatalf("empty migrated projection rebuild=%+v expected_fingerprint=%s", emptyRebuilt, emptyFingerprint)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'learning-one',now()),($2,'learning-two',now())`, learningDeviceOne, learningDeviceTwo); err != nil {
		t.Fatal(err)
	}
	insertLearningKnowledgeFixture(t, pool)

	first := goalCommit(t, learningDeviceOne, "20000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000001", 0, 1, 1)
	result, err := store.Commit(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if result.FirstEventSequence != 1 || result.LastEventSequence != 1 || result.AggregateVersion != 1 || result.ProjectionAsOf != 1 {
		t.Fatalf("first commit = %+v", result)
	}
	var ordinals []int
	rows, err := pool.Query(ctx, `SELECT operation_ordinal FROM learning_events WHERE operation_id=$1 ORDER BY event_seq`, first.Operation.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ordinal int
		if err := rows.Scan(&ordinal); err != nil {
			t.Fatal(err)
		}
		ordinals = append(ordinals, ordinal)
	}
	rows.Close()
	if len(ordinals) != 1 || ordinals[0] != 0 {
		t.Fatalf("event ordinals = %v", ordinals)
	}

	replay, err := store.Commit(ctx, first)
	if err != nil || !replay.Replayed || replay.FirstEventSequence != result.FirstEventSequence || replay.LastEventSequence != result.LastEventSequence {
		t.Fatalf("inbox replay = %+v err=%v", replay, err)
	}
	changed := first
	changed.RequestHash = learning.SHA256([]byte("changed"))
	if _, err := store.Commit(ctx, changed); learning.ErrorCode(err) != learning.CodeIdempotencyConflict {
		t.Fatalf("inbox conflict = %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	outcomes := make(chan error, 2)
	for index, device := range []string{learningDeviceOne, learningDeviceTwo} {
		go func(index int, device string) {
			defer wg.Done()
			<-start
			request := goalCommit(t, device, fmt.Sprintf("20000000-0000-4000-8000-%012d", 10+index), fmt.Sprintf("30000000-0000-4000-8000-%012d", 10+index), 1, 2, 1, "30000000-0000-4000-8000-000000000001")
			_, err := store.Commit(ctx, request)
			outcomes <- err
		}(index, device)
	}
	close(start)
	wg.Wait()
	close(outcomes)
	successes, conflicts := 0, 0
	for err := range outcomes {
		if err == nil {
			successes++
		} else if learning.ErrorCode(err) == learning.CodeVersionConflict {
			conflicts++
		} else {
			t.Fatalf("race error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("expected-version race successes=%d conflicts=%d", successes, conflicts)
	}

	assertIndependentAggregateClock(t, store, pool)
	sessionID, sessionVersion := commitLearningAuthorityFixture(t, store)
	assertProjectionKnowledgeAndStats(t, store, pool, sessionID)
	assertKnowledgeOwnerCompositeFK(t, store, pool, sessionID, sessionVersion)
	assertSessionAuthoritySnapshot(t, store, sessionID, sessionVersion)
	assertAssessmentProvenanceNullFence(t, pool)
	assertRejectedOperationArchive(t, store, sessionID)
	assertUncommittedCheckpointInvisible(t, pool, store)
	assertOutboxConflictRollback(t, store, pool, sessionID, sessionVersion)
	assertSharedTransactionRollsBackTutoringAndOutbox(t, store, pool, sessionID, sessionVersion)

	var clockBefore, eventsBefore, inboxBefore, revisionsBefore int64
	if err := pool.QueryRow(ctx, `SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1`).Scan(&clockBefore); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_events`).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_inbox`).Scan(&inboxBefore); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_goal_revisions`).Scan(&revisionsBefore); err != nil {
		t.Fatal(err)
	}
	faultOperation := "20000000-0000-4000-8000-000000000099"
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_learning_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.operation_id='`+faultOperation+`'::uuid THEN RAISE EXCEPTION 'injected learning failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_learning_event BEFORE INSERT ON learning_events FOR EACH ROW EXECUTE FUNCTION reject_learning_event()`); err != nil {
		t.Fatal(err)
	}
	var currentGoalRevisionID string
	if err := pool.QueryRow(ctx, `SELECT id FROM learning_goal_revisions WHERE goal_id=$1 AND revision=2`, learningGoalID).Scan(&currentGoalRevisionID); err != nil {
		t.Fatal(err)
	}
	fault := goalCommit(t, learningDeviceOne, faultOperation, "30000000-0000-4000-8000-000000000099", 2, 3, 1, currentGoalRevisionID)
	if _, err := store.Commit(ctx, fault); err == nil {
		t.Fatal("fault injection unexpectedly committed")
	}
	assertCount(t, pool, `SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1`, clockBefore)
	assertCount(t, pool, `SELECT count(*) FROM learning_events`, eventsBefore)
	assertCount(t, pool, `SELECT count(*) FROM learning_inbox`, inboxBefore)
	assertCount(t, pool, `SELECT count(*) FROM learning_goal_revisions`, revisionsBefore)
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_learning_event ON learning_events; DROP FUNCTION reject_learning_event()`); err != nil {
		t.Fatal(err)
	}

	before, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Metadata.AsOfEventSequence != before.HighWater || before.HighWater != clockBefore {
		t.Fatalf("checkpoint/high-water mismatch: %+v", before)
	}
	assertPayloadMutationBlocked(t, pool, store, before.ActiveGenerationID)
	rebuilt, err := store.Rebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ActiveGenerationID == before.ActiveGenerationID || rebuilt.Fingerprint != before.Fingerprint || rebuilt.Metadata.AsOfEventSequence != rebuilt.HighWater {
		t.Fatalf("full replay differs: before=%+v rebuilt=%+v", before, rebuilt)
	}
	rebuilt = assertStaleRebuildTakeover(t, store, pool, rebuilt)
	assertFocusProjection(t, store, sessionID, sessionVersion)
	rebuilt = assertConcurrentRebuildTail(t, store, pool)

	activeBeforeFailure := rebuilt.ActiveGenerationID
	sessionBeforeFailure, err := store.Session(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_projection_rebuild() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected projection failure'; END $$; CREATE TRIGGER reject_projection_rebuild BEFORE INSERT ON learning_projection_timeline FOR EACH ROW EXECUTE FUNCTION reject_projection_rebuild()`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Rebuild(ctx); err == nil {
		t.Fatal("projection fault unexpectedly succeeded")
	}
	statusAfterFailure, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if statusAfterFailure.ActiveGenerationID != activeBeforeFailure || statusAfterFailure.RebuildingGenerationID != nil {
		t.Fatalf("failed rebuild changed generation: %+v", statusAfterFailure)
	}
	if !statusAfterFailure.Metadata.Incomplete || !statusAfterFailure.Metadata.Degraded || !contains(statusAfterFailure.Metadata.ReasonCodes, "rebuild_failed") {
		t.Fatalf("failed rebuild did not degrade active status: %+v", statusAfterFailure)
	}
	activeSession, err := store.Session(ctx, sessionID)
	if err != nil || activeSession.Metadata.GenerationID != activeBeforeFailure || activeSession.Session.ID != sessionID || !reflect.DeepEqual(activeSession.Estimate, sessionBeforeFailure.Estimate) {
		t.Fatalf("failed rebuild made active generation stats unreadable: before=%+v session=%+v err=%v", sessionBeforeFailure.Estimate, activeSession, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_projection_rebuild ON learning_projection_timeline; DROP FUNCTION reject_projection_rebuild()`); err != nil {
		t.Fatal(err)
	}

	finalStatus, err := store.Rebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if finalStatus.Fingerprint != rebuilt.Fingerprint || finalStatus.HighWater != rebuilt.HighWater {
		t.Fatalf("incremental/full replay fingerprint mismatch: rebuilt=%+v final=%+v", rebuilt, finalStatus)
	}

	assertExpiredProposalWorkerFenced(t, store, pool)
	proposalRequest := learning.ProposalRequest{RequestID: "40000000-0000-4000-8000-000000000001", Type: learning.ProposalRoute, AggregateType: "goal", AggregateID: learningGoalID, AggregateVersion: 2, KnowledgeRevisionID: learningKnowledgeRevision, NodeRevisionIDs: []string{learningNodeRevisionID}, Input: json.RawMessage(`{"goal":"integration"}`)}
	proposalHash, err := learning.HashJSON(proposalRequest)
	if err != nil {
		t.Fatal(err)
	}
	proposalNow := time.Now().UTC().Truncate(time.Microsecond)
	claim, err := store.ClaimProposal(ctx, learningDeviceOne, proposalRequest, proposalHash, proposalNow)
	if err != nil || claim.State != "claimed" || claim.LeaseToken == "" {
		t.Fatalf("proposal claim = %+v err=%v", claim, err)
	}
	busy, err := store.ClaimProposal(ctx, learningDeviceOne, proposalRequest, proposalHash, proposalNow)
	if err != nil || busy.State != "busy" {
		t.Fatalf("proposal busy replay = %+v err=%v", busy, err)
	}
	artifact := learning.ProposalArtifact{ID: "40000000-0000-4000-8000-000000000003", SchemaVersion: learning.ProposalSchemaVersion, InputHash: proposalHash, Type: proposalRequest.Type, AggregateType: proposalRequest.AggregateType, AggregateID: proposalRequest.AggregateID, AggregateVersion: proposalRequest.AggregateVersion, KnowledgeRevisionID: proposalRequest.KnowledgeRevisionID, Route: []learning.RouteProposalStep{{NodeRevisionID: learningNodeRevisionID, TeachingIntent: "teach", CompletionCondition: "pass"}}, ModelID: "strict-fake", ModelParameters: map[string]any{"temperature": 0}, PromptRevision: learning.TutorPromptRevision, AttemptCategories: []string{"success"}, CreatedAt: proposalNow}
	if err := store.CompleteProposal(ctx, learningDeviceOne, claim.LeaseToken, artifact, proposalNow); err != nil {
		t.Fatal(err)
	}
	ready, err := store.ClaimProposal(ctx, learningDeviceOne, proposalRequest, proposalHash, proposalNow)
	if err != nil || ready.State != "ready" || ready.Artifact == nil || ready.Artifact.ID != artifact.ID {
		t.Fatalf("proposal ready replay = %+v err=%v", ready, err)
	}
	if _, err := store.ClaimProposal(ctx, learningDeviceOne, proposalRequest, learning.SHA256([]byte("different")), proposalNow); learning.ErrorCode(err) != learning.CodeIdempotencyConflict {
		t.Fatalf("proposal idempotency conflict = %v", err)
	}
	assertUnknownSchemaFailsGeneration(t, pool, store)
}

func TestPostgreSQLPersistedFocusInvalidationThroughService(t *testing.T) {
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	store := postgresstore.New(pool, tutoringpostgres.New(pool))
	if _, err := store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'focus-invalidation',now())`, learningDeviceOne); err != nil {
		t.Fatal(err)
	}

	goalID := "60000000-0000-4000-8000-000000000001"
	goalRevisionID := "60000000-0000-4000-8000-000000000002"
	if _, err := store.Commit(ctx, goalCommitFor(t, learningDeviceOne, "60000000-0000-4000-8000-000000000003", goalID, goalRevisionID, 0, 1, 1)); err != nil {
		t.Fatal(err)
	}
	service, err := learning.NewService(store, store, integrationKnowledgeResolver{}, learning.ServiceOptions{Now: func() time.Time {
		return time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	}})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		state  tutoring.State
		action tutoring.Action
	}{
		{name: "end_activity", state: tutoring.StateActivityIssued, action: tutoring.ActionEndActivity},
		{name: "switch_goal", state: tutoring.StateRouteActive, action: tutoring.ActionSwitchGoal},
		{name: "complete_session", state: tutoring.StateRouteActive, action: tutoring.ActionCompleteSession},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			sessionID := fmt.Sprintf("61000000-0000-4000-8000-%012d", index+1)
			frameID := fmt.Sprintf("62000000-0000-4000-8000-%012d", index+1)
			seedPersistedFocusSession(t, store, sessionID, frameID, goalRevisionID, test.state, fmt.Sprintf("63000000-0000-4000-8000-%012d", index+1))

			before, err := store.LoadSessionAuthority(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if before.Session.ActiveFrame == nil || before.Session.FocusFrameInvalidated {
				t.Fatalf("seeded authority=%+v", before)
			}
			beforeEvents := sessionEventCount(t, pool, sessionID)
			beforeVersion := sessionHeadVersion(t, pool, sessionID)
			if beforeVersion != before.Session.AggregateVer {
				t.Fatalf("seeded head=%d authority=%d", beforeVersion, before.Session.AggregateVer)
			}

			action := persistedFocusAction(fmt.Sprintf("64000000-0000-4000-8000-%012d", index+1), sessionID, before.Session.AggregateVer, test.action, goalRevisionID)
			if _, err := service.ApplyAction(ctx, learningDeviceOne, sessionID, action); err != nil {
				t.Fatalf("%s: %v", test.action, err)
			}
			afterInvalidation, err := store.LoadSessionAuthority(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if afterInvalidation.Session.ActiveFrame != nil || !afterInvalidation.Session.FocusFrameInvalidated {
				t.Fatalf("reloaded invalidation authority=%+v", afterInvalidation)
			}
			actionEvents := sessionEventCount(t, pool, sessionID)
			actionVersion := sessionHeadVersion(t, pool, sessionID)
			if actionEvents <= beforeEvents || actionVersion <= beforeVersion || actionVersion != afterInvalidation.Session.AggregateVer {
				t.Fatalf("action did not advance authority: events %d->%d version %d->%d authority=%+v", beforeEvents, actionEvents, beforeVersion, actionVersion, afterInvalidation)
			}

			resume := persistedFocusAction(fmt.Sprintf("65000000-0000-4000-8000-%012d", index+1), sessionID, afterInvalidation.Session.AggregateVer, tutoring.ActionResumeFocus, "")
			if _, err := service.ApplyAction(ctx, learningDeviceOne, sessionID, resume); learning.ErrorCode(err) != learning.CodeFocusFrameInvalidated {
				t.Fatalf("resume error=%v code=%q", err, learning.ErrorCode(err))
			}
			if got := sessionEventCount(t, pool, sessionID); got != actionEvents {
				t.Fatalf("rejected resume changed event count %d->%d", actionEvents, got)
			}
			if got := sessionHeadVersion(t, pool, sessionID); got != actionVersion {
				t.Fatalf("rejected resume changed version %d->%d", actionVersion, got)
			}
			if got := inboxOperationCount(t, pool, learningDeviceOne, resume.Operation.OperationID); got != 1 {
				t.Fatalf("resume rejection inbox count=%d want=1", got)
			}

			if _, err := service.ApplyAction(ctx, learningDeviceOne, sessionID, resume); learning.ErrorCode(err) != learning.CodeFocusFrameInvalidated {
				t.Fatalf("replayed resume error=%v code=%q", err, learning.ErrorCode(err))
			}
			if got := sessionEventCount(t, pool, sessionID); got != actionEvents {
				t.Fatalf("replayed resume changed event count %d->%d", actionEvents, got)
			}
			if got := sessionHeadVersion(t, pool, sessionID); got != actionVersion {
				t.Fatalf("replayed resume changed version %d->%d", actionVersion, got)
			}
			if got := inboxOperationCount(t, pool, learningDeviceOne, resume.Operation.OperationID); got != 1 {
				t.Fatalf("replayed resume inbox count=%d want=1", got)
			}
		})
	}
}

type integrationKnowledgeResolver struct{}

func (integrationKnowledgeResolver) Resolve(_ context.Context, knowledgeRevisionID, nodeRevisionID string) (learning.KnowledgeReference, error) {
	return learning.KnowledgeReference{KnowledgeRevisionID: knowledgeRevisionID, NodeID: "integration-node", NodeRevisionID: nodeRevisionID, DocumentRevisionID: "integration-document", Range: learning.SourceRange{Start: 0, End: 1}, Slice: "x", SliceSHA256: learning.SHA256([]byte("x"))}, nil
}

func seedPersistedFocusSession(t *testing.T, store *postgresstore.Store, sessionID, frameID, goalRevisionID string, state tutoring.State, operationID string) {
	t.Helper()
	now := time.Date(2026, 8, 20, 12, 20, 0, 0, time.UTC)
	contextValue := tutoring.FocusContext{GoalRevisionID: goalRevisionID}
	frame := tutoring.FocusFrame{ID: frameID, SessionID: sessionID, SavedState: tutoring.StateRouteActive, Context: contextValue, SavedAggregateVersion: 1}
	session := tutoring.Session{ID: sessionID, State: state, Context: contextValue, ActiveFrame: &frame}
	snapshot := learning.SessionProjection{Session: session}
	request := learning.CommitRequest{
		DeviceID:     learningDeviceOne,
		Operation:    learning.OperationEnvelope{OperationID: operationID, PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: 0, Payload: json.RawMessage(`{"command":"seed_focus"}`)},
		RequestHash:  learning.SHA256([]byte(operationID)),
		Expectations: []learning.AggregateExpectation{{Type: "session", ID: sessionID, ExpectedVersion: 0}},
		Batch: learning.CommandBatch{
			Session: &session, FocusFrame: &frame, ResultSession: true, TutoringState: string(state),
			Events: []learning.EventDraft{
				eventDraft(learning.EventFocusSuspended, sessionID, snapshot),
				eventDraft(learning.EventTutoringStateChanged, sessionID, snapshot),
			},
		},
		ReceivedAt: now,
	}
	if _, err := store.Commit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

func persistedFocusAction(operationID, sessionID string, expectedVersion int64, action tutoring.Action, goalRevisionID string) learning.ActionCommand {
	return learning.ActionCommand{
		Operation: learning.OperationEnvelope{OperationID: operationID, PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: expectedVersion, Payload: json.RawMessage(`{"command":"focus_action"}`)},
		Action:    action, GoalRevisionID: goalRevisionID,
	}
}

func sessionEventCount(t *testing.T, pool *pgxpool.Pool, sessionID string) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_events WHERE aggregate_type='session' AND aggregate_id=$1`, sessionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func sessionHeadVersion(t *testing.T, pool *pgxpool.Pool, sessionID string) int64 {
	t.Helper()
	var version int64
	if err := pool.QueryRow(context.Background(), `SELECT aggregate_version FROM learning_aggregate_heads WHERE aggregate_type='session' AND aggregate_id=$1`, sessionID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func inboxOperationCount(t *testing.T, pool *pgxpool.Pool, deviceID, operationID string) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_inbox WHERE device_id=$1 AND operation_id=$2`, deviceID, operationID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertProjectionKnowledgeAndStats(t *testing.T, store *postgresstore.Store, pool *pgxpool.Pool, sessionID string) {
	t.Helper()
	ctx := context.Background()
	status, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Metadata.KnowledgeRevisionID != learningKnowledgeRevision {
		t.Fatalf("projection metadata knowledge revision=%q want=%q", status.Metadata.KnowledgeRevisionID, learningKnowledgeRevision)
	}
	var storedKnowledgeRevision string
	var stat learning.ActiveTimeEstimate
	if err := pool.QueryRow(ctx, `SELECT g.knowledge_revision_id::text,st.item FROM learning_projection_head h JOIN learning_projection_generations g ON g.id=h.active_generation_id JOIN learning_projection_stats st ON st.generation_id=g.id AND st.session_id=$1 WHERE h.singleton_id=1`, sessionID).Scan(&storedKnowledgeRevision, &stat); err != nil {
		t.Fatal(err)
	}
	view, err := store.Session(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.CurrentSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if storedKnowledgeRevision != learningKnowledgeRevision || view.Metadata.KnowledgeRevisionID != learningKnowledgeRevision || current.Metadata.KnowledgeRevisionID != learningKnowledgeRevision {
		t.Fatalf("knowledge revision round-trip stored=%q session=%q current=%q", storedKnowledgeRevision, view.Metadata.KnowledgeRevisionID, current.Metadata.KnowledgeRevisionID)
	}
	if !stat.Estimated || stat.AlgorithmVersion != learning.ActiveTimePolicyVersion || stat.SampleCount != 1 || !reflect.DeepEqual(view.Estimate, stat) || !reflect.DeepEqual(current.Estimate, stat) {
		t.Fatalf("generation stats stored=%+v session=%+v current=%+v", stat, view.Estimate, current.Estimate)
	}
}

func assertKnowledgeOwnerCompositeFK(t *testing.T, store *postgresstore.Store, pool *pgxpool.Pool, sessionID string, version int64) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	stepID := "58000000-0000-4000-8000-000000000003"
	route := learning.RouteRevision{
		ID: "58000000-0000-4000-8000-000000000001", RouteID: "58000000-0000-4000-8000-000000000002", Revision: 1,
		GoalRevisionID: "30000000-0000-4000-8000-000000000001", KnowledgeRevisionID: learningKnowledgeRevision,
		PolicyVersion: learning.RoutePolicyVersion, CreatedAt: now,
		Steps: []learning.RouteStep{{ID: stepID, NodeID: learningNodeID, NodeRevisionID: learningNodeRevisionID, TeachingIntent: "teach", CompletionCondition: "pass"}},
	}
	request := learning.CommitRequest{
		DeviceID:     learningDeviceOne,
		Operation:    learning.OperationEnvelope{OperationID: "58000000-0000-4000-8000-000000000004", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: version, Payload: json.RawMessage(`{}`)},
		RequestHash:  learning.SHA256([]byte("invalid knowledge owner")),
		Expectations: []learning.AggregateExpectation{{Type: "session", ID: sessionID, ExpectedVersion: version}},
		Batch: learning.CommandBatch{
			RouteRevision: &route,
			Authority: learning.AuthorityProvenance{RouteSteps: map[string]learning.KnowledgeOwner{stepID: {
				KnowledgeRevisionID: learningKnowledgeRevision, NodeID: learningNodeID, NodeRevisionID: learningNodeRevisionID,
				DocumentRevisionID: "58000000-0000-4000-8000-000000000005",
			}}},
			Events: []learning.EventDraft{eventDraft(learning.EventRouteRevisionCreated, sessionID, route)},
		},
		ReceivedAt: now,
	}
	_, err := store.Commit(context.Background(), request)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || (pgErr.ConstraintName != "learning_route_step_node_owner" && pgErr.ConstraintName != "learning_route_step_snapshot_owner") {
		t.Fatalf("invalid resolver tuple bypassed composite FK: err=%v constraint=%q", err, pgErrConstraint(pgErr))
	}
	var routeCount int64
	if queryErr := pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_route_revisions WHERE id=$1`, route.ID).Scan(&routeCount); queryErr != nil || routeCount != 0 {
		t.Fatalf("failed owner tuple left route revision count=%d err=%v", routeCount, queryErr)
	}
	authority, loadErr := store.LoadSessionAuthority(context.Background(), sessionID)
	if loadErr != nil || authority.Session.AggregateVer != version {
		t.Fatalf("failed owner tuple advanced authority=%+v err=%v", authority, loadErr)
	}
}

func assertSessionAuthoritySnapshot(t *testing.T, store *postgresstore.Store, sessionID string, version int64) {
	t.Helper()
	authority, err := store.LoadSessionAuthority(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if authority.Session.AggregateVer != version || authority.Session.ActiveFrame == nil || authority.Session.ActiveFrame.CreatedEventSequence <= 0 || authority.Session.ActiveFrame.CreatedEventSequence > authority.AsOfEventSequence {
		t.Fatalf("session/frame/high-water not from one authority snapshot: %+v", authority)
	}
}

func assertAssessmentProvenanceNullFence(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	var assessmentID string
	if err := pool.QueryRow(ctx, `SELECT id FROM learning_assessments ORDER BY created_at,id LIMIT 1`).Scan(&assessmentID); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO learning_assessment_items(
			assessment_id,ordinal,rubric_item_id,conclusion,
			answer_start,answer_end,answer_quote,answer_quote_hash,
			knowledge_revision_id,knowledge_node_revision_id,knowledge_node_id,
			knowledge_document_revision_id,knowledge_start,knowledge_end,knowledge_quote,knowledge_quote_hash)
		VALUES($1,99,'partial-null-owner','pass',0,2,'ok',decode(repeat('11',32),'hex'),
			$2,$3,NULL,$4,0,5,'topic',decode(repeat('22',32),'hex'))`,
		assessmentID, learningKnowledgeRevision, learningNodeRevisionID, learningDocumentRevisionID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || (pgErr.ConstraintName != "learning_assessment_item_provenance_shape" && pgErr.ConstraintName != "learning_assessment_item_node_owner") {
		t.Fatalf("assessment partial-NULL ownership err=%v constraint=%q", err, pgErrConstraint(pgErr))
	}
}

func pgErrConstraint(err *pgconn.PgError) string {
	if err == nil {
		return ""
	}
	return err.ConstraintName
}

func assertExpiredProposalWorkerFenced(t *testing.T, store *postgresstore.Store, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := learning.ProposalRequest{
		RequestID: "57000000-0000-4000-8000-000000000001", Type: learning.ProposalRoute,
		AggregateType: "goal", AggregateID: learningGoalID, AggregateVersion: 2,
		KnowledgeRevisionID: learningKnowledgeRevision, NodeRevisionIDs: []string{learningNodeRevisionID},
		Input: json.RawMessage(`{"goal":"expired-worker"}`),
	}
	hash, err := learning.HashJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	oldClaim, err := store.ClaimProposal(ctx, learningDeviceOne, request, hash, now)
	if err != nil || oldClaim.State != "claimed" {
		t.Fatalf("old proposal claim=%+v err=%v", oldClaim, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tutoring_proposal_requests SET lease_expires_at=now()-interval '1 second' WHERE device_id=$1 AND request_id=$2`, learningDeviceOne, request.RequestID); err != nil {
		t.Fatal(err)
	}
	if err := store.FailProposal(ctx, learningDeviceOne, oldClaim.LeaseToken, []string{"expired"}, "expired", now); learning.ErrorCode(err) != learning.CodeStaleProposal {
		t.Fatalf("expired worker failed proposal err=%v", err)
	}
	currentClaim, err := store.ClaimProposal(ctx, learningDeviceOne, request, hash, time.Now().UTC())
	if err != nil || currentClaim.State != "claimed" || currentClaim.LeaseToken == oldClaim.LeaseToken {
		t.Fatalf("proposal reclaim=%+v err=%v", currentClaim, err)
	}
	artifact := learning.ProposalArtifact{
		ID: "57000000-0000-4000-8000-000000000002", SchemaVersion: learning.ProposalSchemaVersion,
		InputHash: hash, Type: request.Type, AggregateType: request.AggregateType, AggregateID: request.AggregateID,
		AggregateVersion: request.AggregateVersion, KnowledgeRevisionID: request.KnowledgeRevisionID,
		Route:   []learning.RouteProposalStep{{NodeRevisionID: learningNodeRevisionID, TeachingIntent: "teach", CompletionCondition: "pass"}},
		ModelID: "strict-fake", ModelParameters: map[string]any{"temperature": 0}, PromptRevision: learning.TutorPromptRevision,
		AttemptCategories: []string{"success"}, CreatedAt: now,
	}
	if err := store.CompleteProposal(ctx, learningDeviceOne, oldClaim.LeaseToken, artifact, now); learning.ErrorCode(err) != learning.CodeStaleProposal {
		t.Fatalf("reclaimed old worker completed proposal err=%v", err)
	}
	if err := store.FailProposal(ctx, learningDeviceOne, oldClaim.LeaseToken, []string{"stale"}, "stale", now); learning.ErrorCode(err) != learning.CodeStaleProposal {
		t.Fatalf("reclaimed old worker failed proposal err=%v", err)
	}
	if err := store.CompleteProposal(ctx, learningDeviceOne, currentClaim.LeaseToken, artifact, now); err != nil {
		t.Fatalf("current proposal worker complete err=%v", err)
	}
	if err := store.FailProposal(ctx, learningDeviceOne, oldClaim.LeaseToken, []string{"stale"}, "stale", now); learning.ErrorCode(err) != learning.CodeStaleProposal {
		t.Fatalf("old worker overwrote ready proposal err=%v", err)
	}
	var status string
	var artifactCount int
	if err := pool.QueryRow(ctx, `SELECT status FROM tutoring_proposal_requests WHERE device_id=$1 AND request_id=$2`, learningDeviceOne, request.RequestID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tutoring_proposal_artifacts WHERE id=$1`, artifact.ID).Scan(&artifactCount); err != nil {
		t.Fatal(err)
	}
	if status != "ready" || artifactCount != 1 {
		t.Fatalf("proposal reclaim final status=%s artifact_count=%d", status, artifactCount)
	}
}

func assertStaleRebuildTakeover(t *testing.T, store *postgresstore.Store, pool *pgxpool.Pool, before learning.ProjectionStatus) learning.ProjectionStatus {
	t.Helper()
	ctx := context.Background()
	staleGeneration := "58000000-0000-4000-8000-000000000001"
	staleToken := "58000000-0000-4000-8000-000000000002"
	if _, err := pool.Exec(ctx, `
		INSERT INTO learning_projection_generations(
			id,projection_version,reducer_version,assessment_policy_version,review_policy_version,
			status,target_high_water,checkpoint_event_seq,created_at)
		SELECT $1,projection_version,reducer_version,assessment_policy_version,review_policy_version,
			'building',$2,0,now()
		FROM learning_projection_generations WHERE id=$3`, staleGeneration, before.HighWater, before.ActiveGenerationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE learning_projection_head SET rebuilding_generation_id=$1,rebuild_lease_token=$2,rebuild_lease_expires_at=now()+interval '1 minute',updated_at=now() WHERE singleton_id=1`, staleGeneration, staleToken); err != nil {
		t.Fatal(err)
	}
	_, err := store.Rebuild(ctx)
	var inProgress *learning.Error
	if !errors.As(err, &inProgress) || inProgress.Code != learning.CodeProjectionUnavailable || inProgress.Reason != "rebuild_in_progress" {
		t.Fatalf("live rebuild marker was taken over err=%v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE learning_projection_head SET rebuild_lease_expires_at=now()-interval '1 second' WHERE singleton_id=1 AND rebuilding_generation_id=$1`, staleGeneration); err != nil {
		t.Fatal(err)
	}
	after, err := store.Rebuild(ctx)
	if err != nil {
		t.Fatalf("stale rebuild marker takeover err=%v", err)
	}
	if after.ActiveGenerationID == before.ActiveGenerationID || after.ActiveGenerationID == staleGeneration || after.RebuildingGenerationID != nil || after.Fingerprint != before.Fingerprint || after.HighWater != before.HighWater {
		t.Fatalf("stale rebuild takeover status before=%+v after=%+v", before, after)
	}
	var staleStatus string
	var staleReasons []string
	if err := pool.QueryRow(ctx, `SELECT status,reason_codes FROM learning_projection_generations WHERE id=$1`, staleGeneration).Scan(&staleStatus, &staleReasons); err != nil {
		t.Fatal(err)
	}
	if staleStatus != "failed" || !contains(staleReasons, "rebuild_lease_expired") {
		t.Fatalf("stale generation status=%s reasons=%v", staleStatus, staleReasons)
	}
	return after
}

func assertPayloadMutationBlocked(t *testing.T, pool *pgxpool.Pool, store *postgresstore.Store, activeGeneration string) {
	t.Helper()
	ctx := context.Background()
	var payloadID string
	if err := pool.QueryRow(ctx, `SELECT id FROM learning_event_payloads ORDER BY created_at,id LIMIT 1`).Scan(&payloadID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE learning_event_payloads SET payload='{"tampered":true}'::jsonb WHERE id=$1`, payloadID); err == nil || !strings.Contains(err.Error(), "learning history is append-only") {
		t.Fatalf("append-only payload mutation was not rejected: %v", err)
	}
	status, err := store.ProjectionStatus(ctx)
	if err != nil || status.ActiveGenerationID != activeGeneration || status.RebuildingGenerationID != nil {
		t.Fatalf("rejected payload mutation changed projection state: %+v err=%v", status, err)
	}
}

func insertLearningKnowledgeFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO knowledge_revisions(id,revision_no,manifest_hash,source,created_by_device_id,created_at,canonicalizer_version,parser_version,indexer_version,identity_policy_version) VALUES($1,1,decode(repeat('11',32),'hex'),'learning fixture',$2,now(),'c1','p1','i1','id1')`, []any{learningKnowledgeRevision, learningDeviceOne}},
		{`INSERT INTO knowledge_documents(id,created_at) VALUES($1,now())`, []any{learningDocumentID}},
		{`INSERT INTO knowledge_nodes(id,document_id,created_at) VALUES($1,$2,now())`, []any{learningNodeID, learningDocumentID}},
		{`INSERT INTO knowledge_document_revisions(id,document_id,canonical_hash,semantic_hash,root_node_id,parser_version,created_at) VALUES($1,$2,decode(repeat('22',32),'hex'),decode(repeat('33',32),'hex'),$3,'p1',now())`, []any{learningDocumentRevisionID, learningDocumentID, learningNodeID}},
		{`INSERT INTO knowledge_document_payloads(document_revision_id,canonical_markdown) VALUES($1,'topic')`, []any{learningDocumentRevisionID}},
		{`INSERT INTO knowledge_snapshot_documents(knowledge_revision_id,canonical_path,folded_path,document_id,document_revision_id) VALUES($1,'topic.md','topic.md',$2,$3)`, []any{learningKnowledgeRevision, learningDocumentID, learningDocumentRevisionID}},
		{`INSERT INTO knowledge_node_revisions(id,node_id,document_id,document_revision_id,parent_node_revision_id,sibling_index,heading_level,title,ancestor_titles,heading_start,heading_end,heading_start_line,heading_end_line,local_body_start,local_body_end,local_body_start_line,local_body_end_line,section_start,section_end,section_start_line,section_end_line,semantic_local_body_hash,indexer_version) VALUES($1,$2,$3,$4,NULL,0,0,'topic','[]'::jsonb,0,0,1,1,0,5,1,1,0,5,1,1,decode(repeat('44',32),'hex'),'i1')`, []any{learningNodeRevisionID, learningNodeID, learningDocumentID, learningDocumentRevisionID}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertIndependentAggregateClock(t *testing.T, store *postgresstore.Store, pool *pgxpool.Pool) {
	t.Helper()
	requests := make([]learning.CommitRequest, 0, 2)
	for index, device := range []string{learningDeviceOne, learningDeviceTwo} {
		sessionID := fmt.Sprintf("10000000-0000-4000-8000-%012d", 201+index)
		operationID := fmt.Sprintf("20000000-0000-4000-8000-%012d", 201+index)
		session := tutoring.Session{ID: sessionID, State: tutoring.StateGoalReady}
		events := []learning.EventDraft{
			eventDraft(learning.EventLearningSessionStarted, sessionID, learning.SessionProjection{Session: session}),
			eventDraft(learning.EventTutoringStateChanged, sessionID, learning.SessionProjection{Session: session}),
		}
		requests = append(requests, learning.CommitRequest{
			DeviceID:    device,
			Operation:   learning.OperationEnvelope{OperationID: operationID, PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: 0, Payload: json.RawMessage(`{}`)},
			RequestHash: learning.SHA256([]byte(device + operationID)), Expectations: []learning.AggregateExpectation{{Type: "session", ID: sessionID, ExpectedVersion: 0}},
			Batch: learning.CommandBatch{Session: &session, Events: events, TutoringState: string(session.State)}, ReceivedAt: time.Now().UTC(),
		})
	}
	start := make(chan struct{})
	results := make(chan error, len(requests))
	for _, request := range requests {
		request := request
		go func() {
			<-start
			_, err := store.Commit(context.Background(), request)
			results <- err
		}()
	}
	close(start)
	for range requests {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	type span struct{ first, last, count int64 }
	spans := make([]span, len(requests))
	for index, request := range requests {
		if err := pool.QueryRow(context.Background(), `SELECT min(event_seq),max(event_seq),count(*) FROM learning_events WHERE operation_id=$1`, request.Operation.OperationID).Scan(&spans[index].first, &spans[index].last, &spans[index].count); err != nil {
			t.Fatal(err)
		}
		if spans[index].count != 2 || spans[index].last-spans[index].first != 1 {
			t.Fatalf("operation event block was interleaved: %+v", spans[index])
		}
	}
	if !(spans[0].last < spans[1].first || spans[1].last < spans[0].first) {
		t.Fatalf("independent aggregate event blocks overlap: %+v", spans)
	}
}

func commitLearningAuthorityFixture(t *testing.T, store *postgresstore.Store) (string, int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	sessionID := "50000000-0000-4000-8000-000000000001"
	goalRevisionID := "30000000-0000-4000-8000-000000000001"
	routeRevisionID := "50000000-0000-4000-8000-000000000002"
	routeID := "50000000-0000-4000-8000-000000000003"
	stepID := "50000000-0000-4000-8000-000000000004"
	activityID := "50000000-0000-4000-8000-000000000005"
	attemptID := "50000000-0000-4000-8000-000000000006"
	assessmentID := "50000000-0000-4000-8000-000000000007"
	decisionOneID := "50000000-0000-4000-8000-000000000008"
	decisionTwoID := "50000000-0000-4000-8000-000000000009"
	evidenceID := "50000000-0000-4000-8000-000000000010"

	commit := func(operationID string, expected int64, batch learning.CommandBatch) learning.OperationResult {
		result, err := store.Commit(ctx, learning.CommitRequest{
			DeviceID:    learningDeviceOne,
			Operation:   learning.OperationEnvelope{OperationID: operationID, PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: expected, Payload: json.RawMessage(`{}`)},
			RequestHash: learning.SHA256([]byte(operationID)), Expectations: []learning.AggregateExpectation{{Type: "session", ID: sessionID, ExpectedVersion: expected}}, Batch: batch, ReceivedAt: now.Add(time.Duration(expected) * time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	session := tutoring.Session{ID: sessionID, State: tutoring.StateGoalReady, Context: tutoring.FocusContext{GoalRevisionID: goalRevisionID}}
	result := commit("51000000-0000-4000-8000-000000000001", 0, learning.CommandBatch{Session: &session, TutoringState: string(session.State), Events: []learning.EventDraft{
		eventDraft(learning.EventLearningSessionStarted, sessionID, learning.SessionProjection{Session: session}), eventDraft(learning.EventTutoringStateChanged, sessionID, learning.SessionProjection{Session: session}),
	}})

	route := learning.RouteRevision{ID: routeRevisionID, RouteID: routeID, Revision: 1, GoalRevisionID: goalRevisionID, KnowledgeRevisionID: learningKnowledgeRevision, PolicyVersion: learning.RoutePolicyVersion, Steps: []learning.RouteStep{{ID: stepID, Ordinal: 0, NodeID: learningNodeID, NodeRevisionID: learningNodeRevisionID, TeachingIntent: "teach", CompletionCondition: "pass"}}, CreatedAt: now}
	session.State = tutoring.StateRouteActive
	session.Context = tutoring.FocusContext{GoalRevisionID: goalRevisionID, RouteRevisionID: routeRevisionID, RouteStepID: stepID, KnowledgeRevisionID: learningKnowledgeRevision, FocusNodeRevisionID: learningNodeRevisionID}
	result = commit("51000000-0000-4000-8000-000000000002", result.AggregateVersion, learning.CommandBatch{RouteRevision: &route, Session: &session, Authority: learning.AuthorityProvenance{RouteSteps: map[string]learning.KnowledgeOwner{stepID: {KnowledgeRevisionID: learningKnowledgeRevision, NodeID: learningNodeID, NodeRevisionID: learningNodeRevisionID, DocumentRevisionID: learningDocumentRevisionID}}}, TutoringState: string(session.State), Events: []learning.EventDraft{eventDraft(learning.EventRouteRevisionCreated, sessionID, route), eventDraft(learning.EventTutoringStateChanged, sessionID, learning.SessionProjection{Session: session})}})

	reference := learning.KnowledgeReference{KnowledgeRevisionID: learningKnowledgeRevision, NodeID: learningNodeID, NodeRevisionID: learningNodeRevisionID, DocumentRevisionID: learningDocumentRevisionID, Range: learning.SourceRange{Start: 0, End: 5}, Slice: "topic", SliceSHA256: learning.SHA256([]byte("topic"))}
	activity := learning.Activity{ID: activityID, Revision: 1, SessionID: sessionID, GoalRevisionID: goalRevisionID, RouteRevisionID: routeRevisionID, RouteStepID: stepID, KnowledgeRevisionID: learningKnowledgeRevision, TargetNodeID: learningNodeID, TargetNodeRevisionID: learningNodeRevisionID, References: []learning.KnowledgeReference{reference}, Prompt: "topic?", Type: learning.ActivityObjective, Rubric: learning.Rubric{Revision: "r1", Items: []learning.RubricItem{{ID: "item-1", Criterion: "correct"}}, ObjectiveRule: &learning.ObjectiveRule{AcceptedAnswers: []string{"ok"}, TrimSpace: true}}, Difficulty: 1, AllowedHelp: []learning.HelpLevel{learning.HelpNone}, ActivityPolicyVersion: learning.ActivityPolicyVersion, AssessmentPolicyVersion: learning.AssessmentPolicyVersion, ReviewPolicyVersion: learning.ReviewPolicyVersion, CreatedAt: now}
	session.State = tutoring.StateActivityIssued
	session.Context.ActivityID = &activity.ID
	result = commit("51000000-0000-4000-8000-000000000003", result.AggregateVersion, learning.CommandBatch{Activity: &activity, Session: &session, TutoringState: string(session.State), Events: []learning.EventDraft{eventDraft(learning.EventActivityIssued, sessionID, activity), eventDraft(learning.EventTutoringStateChanged, sessionID, learning.SessionProjection{Session: session})}})

	attempt := learning.Attempt{ID: attemptID, SessionID: sessionID, ActivityID: activityID, ActivityRevision: 1, AnswerPayloadID: "50000000-0000-4000-8000-000000000011", Answer: "ok", AnswerSHA256: learning.SHA256([]byte("ok")), Help: learning.HelpNone, ActorDeviceID: learningDeviceOne, ReceivedAt: now}
	session.State = tutoring.StateEvaluating
	session.Context.AttemptID = &attempt.ID
	result = commit("51000000-0000-4000-8000-000000000004", result.AggregateVersion, learning.CommandBatch{Attempt: &attempt, Session: &session, TutoringState: string(session.State), Events: []learning.EventDraft{eventDraft(learning.EventAttemptSubmitted, sessionID, attempt), eventDraft(learning.EventTutoringStateChanged, sessionID, learning.SessionProjection{Session: session})}})

	item := learning.AssessmentItem{RubricItemID: "item-1", Conclusion: learning.ConclusionPass, AnswerQuote: "ok", AnswerRange: learning.SourceRange{Start: 0, End: 2}, AnswerQuoteSHA256: learning.SHA256([]byte("ok")), KnowledgeReferenceID: learningNodeRevisionID, KnowledgeQuote: "topic", KnowledgeRange: learning.SourceRange{Start: 0, End: 5}, KnowledgeQuoteSHA256: learning.SHA256([]byte("topic"))}
	assessment := learning.AssessmentArtifact{ID: assessmentID, SessionID: sessionID, AttemptID: attemptID, ActivityID: activityID, ActivityRevision: 1, Items: []learning.AssessmentItem{item}, RubricComplete: true, Confidence: 500, RiskFlags: []learning.RiskFlag{learning.RiskInsufficientAnswerEvidence}, ModelID: "fixture", ModelParameters: map[string]any{}, PromptRevision: "p1", ProposalInputHash: learning.SHA256([]byte("proposal")), Attempts: 1, AttemptCategories: []string{"success"}, CreatedAt: now}
	decisionOne := learning.AssessmentDecision{ID: decisionOneID, AssessmentID: assessmentID, Version: 1, Disposition: learning.DispositionProvisional, Items: assessment.Items, ActorDeviceID: learningDeviceOne, CreatedAt: now}
	session.State = tutoring.StateFeedback
	result = commit("51000000-0000-4000-8000-000000000005", result.AggregateVersion, learning.CommandBatch{Assessment: &assessment, Authority: learning.AuthorityProvenance{AssessmentItems: []learning.KnowledgeOwner{{KnowledgeRevisionID: learningKnowledgeRevision, NodeID: learningNodeID, NodeRevisionID: learningNodeRevisionID, DocumentRevisionID: learningDocumentRevisionID}}}, Decisions: []learning.AssessmentDecision{decisionOne}, Session: &session, Disposition: learning.DispositionProvisional, TutoringState: string(session.State), Events: []learning.EventDraft{
		eventDraft(learning.EventAssessmentRecorded, sessionID, assessment), eventDraft(learning.EventAssessmentMarkedProvisional, sessionID, learning.AssessmentProjectionEvent{AssessmentID: assessmentID, NodeRevisionID: learningNodeRevisionID, Reasons: []string{"low_confidence"}, Decision: decisionOne}), eventDraft(learning.EventTutoringStateChanged, sessionID, learning.SessionProjection{Session: session}),
	}})

	decisionTwo := learning.AssessmentDecision{ID: decisionTwoID, AssessmentID: assessmentID, Version: 2, Disposition: learning.DispositionAccepted, Items: assessment.Items, ActorDeviceID: learningDeviceOne, CreatedAt: now, ReplacesDecisionID: &decisionOneID, ProducedEvidenceID: &evidenceID}
	evidence := learning.AcceptedEvidence{ID: evidenceID, DispositionDecisionID: decisionTwoID, AssessmentID: assessmentID, AttemptID: attemptID, ActivityID: activityID, ActivityRevision: 1, GoalRevisionID: goalRevisionID, RouteRevisionID: routeRevisionID, KnowledgeRevisionID: learningKnowledgeRevision, NodeRevisionID: learningNodeRevisionID, RubricRevision: "r1", Kind: learning.EvidencePracticeRecall, ActivityType: learning.ActivityObjective, Outcome: learning.OutcomePass, Help: learning.HelpNone, ReceivedAt: now, AcceptancePolicyVersion: learning.AssessmentPolicyVersion, ReducerPolicyVersion: learning.MasteryReducerVersion, ReviewPolicyVersion: learning.ReviewPolicyVersion}
	result = commit("51000000-0000-4000-8000-000000000006", result.AggregateVersion, learning.CommandBatch{Decisions: []learning.AssessmentDecision{decisionTwo}, Evidence: []learning.AcceptedEvidence{evidence}, Authority: learning.AuthorityProvenance{Evidence: map[string]learning.EvidenceOwner{evidenceID: {SessionID: sessionID, KnowledgeOwner: learning.KnowledgeOwner{KnowledgeRevisionID: learningKnowledgeRevision, NodeID: learningNodeID, NodeRevisionID: learningNodeRevisionID, DocumentRevisionID: learningDocumentRevisionID}}}}, Session: &session, Disposition: learning.DispositionAccepted, TutoringState: string(session.State), Events: []learning.EventDraft{eventDraft(learning.EventAssessmentAccepted, sessionID, learning.AssessmentProjectionEvent{AssessmentID: assessmentID, NodeRevisionID: learningNodeRevisionID, Decision: decisionTwo}), eventDraft(learning.EventEvidenceAccepted, sessionID, evidence)}})
	loaded, err := store.LoadSession(ctx, sessionID)
	if err != nil || loaded.AggregateVer != result.AggregateVersion {
		t.Fatalf("decision session version loaded=%+v result=%+v err=%v", loaded, result, err)
	}
	evidencePage, err := store.EvidenceList(ctx, learning.EvidenceQuery{NodeRevisionID: learningNodeRevisionID})
	if err != nil || len(evidencePage.Items) != 1 || evidencePage.Items[0].ID != evidenceID {
		t.Fatalf("filtered accepted evidence projection=%+v err=%v", evidencePage, err)
	}
	allEvidence, err := store.EvidenceList(ctx, learning.EvidenceQuery{})
	if err != nil || len(allEvidence.Items) != 1 || allEvidence.Items[0].ID != evidenceID {
		t.Fatalf("unfiltered accepted evidence projection=%+v err=%v", allEvidence, err)
	}
	node, err := store.Node(ctx, learningNodeRevisionID)
	if err != nil || node.Node.Mastery.PendingAssessments != 0 {
		t.Fatalf("provisional assessment was not independently resolved: %+v err=%v", node, err)
	}

	frame := &tutoring.FocusFrame{ID: "50000000-0000-4000-8000-000000000012", SessionID: sessionID, SavedState: tutoring.StateRouteActive, Context: session.Context, SavedAggregateVersion: result.AggregateVersion}
	session.State = tutoring.StateFreeQuestion
	session.ActiveFrame = frame
	result = commit("51000000-0000-4000-8000-000000000007", result.AggregateVersion, learning.CommandBatch{Session: &session, FocusFrame: frame, TutoringState: string(session.State), Events: []learning.EventDraft{eventDraft(learning.EventFocusSuspended, sessionID, learning.SessionProjection{Session: session}), eventDraft(learning.EventTutoringStateChanged, sessionID, learning.SessionProjection{Session: session})}})
	return sessionID, result.AggregateVersion
}

func assertRejectedOperationArchive(t *testing.T, store *postgresstore.Store, sessionID string) {
	t.Helper()
	lookup := learning.OperationLookup{DeviceID: learningDeviceOne, OperationID: "52000000-0000-4000-8000-000000000001", RequestHash: learning.SHA256([]byte("rejected"))}
	authority, authorityErr := store.LoadSessionAuthority(context.Background(), sessionID)
	if authorityErr != nil {
		t.Fatal(authorityErr)
	}
	result, err := store.ArchiveRejection(context.Background(), learning.OperationRejection{Lookup: lookup, AggregateType: "session", AggregateID: sessionID, Expectations: []learning.AggregateExpectation{{Type: "session", ID: sessionID, ExpectedVersion: authority.Session.AggregateVer}}, Error: learning.Error{Code: learning.CodeInvalidTransition, Reason: "fixture"}, CompletedAt: time.Now().UTC()})
	if learning.ErrorCode(err) != learning.CodeInvalidTransition || !result.Archived {
		t.Fatalf("archive rejection result=%+v err=%v", result, err)
	}
	replay, replayErr, found := store.LookupOperation(context.Background(), lookup)
	if !found || learning.ErrorCode(replayErr) != learning.CodeInvalidTransition || !replay.Replayed {
		t.Fatalf("rejected replay=%+v found=%v err=%v", replay, found, replayErr)
	}
	lookup.RequestHash = learning.SHA256([]byte("different"))
	if _, conflict, found := store.LookupOperation(context.Background(), lookup); !found || learning.ErrorCode(conflict) != learning.CodeIdempotencyConflict {
		t.Fatalf("rejection hash conflict found=%v err=%v", found, conflict)
	}
}

func assertUncommittedCheckpointInvisible(t *testing.T, pool *pgxpool.Pool, store *postgresstore.Store) {
	t.Helper()
	ctx := context.Background()
	before, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var sequence int64
	if err := tx.QueryRow(ctx, `SELECT current_event_seq+1 FROM learning_event_clock WHERE singleton_id=1 FOR UPDATE`).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{}`)
	hash := learning.SHA256(payload)
	if _, err := tx.Exec(ctx, `INSERT INTO learning_aggregate_heads(aggregate_type,aggregate_id,aggregate_version,last_event_seq,updated_at) VALUES('goal',$1,1,$2,now())`, "53000000-0000-4000-8000-000000000001", sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO learning_event_payloads(id,payload,payload_hash,created_at) VALUES($1,$2,decode($3,'hex'),now())`, "53000000-0000-4000-8000-000000000002", payload, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO learning_events(event_seq,id,event_type,event_schema_version,aggregate_type,aggregate_id,aggregate_version,device_id,operation_id,operation_ordinal,received_at,payload_id,payload_hash) VALUES($1,$2,'GoalRevisionCreated',1,'goal',$3,1,$4,$5,0,now(),$6,decode($7,'hex'))`, sequence, "53000000-0000-4000-8000-000000000003", "53000000-0000-4000-8000-000000000001", learningDeviceOne, "53000000-0000-4000-8000-000000000004", "53000000-0000-4000-8000-000000000002", hash); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE learning_event_clock SET current_event_seq=$1,updated_at=now() WHERE singleton_id=1`, sequence); err != nil {
		t.Fatal(err)
	}
	during, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if during.HighWater != before.HighWater || during.Metadata.AsOfEventSequence != before.Metadata.AsOfEventSequence {
		t.Fatalf("checkpoint crossed uncommitted event: before=%+v during=%+v", before, during)
	}
}

func assertOutboxConflictRollback(t *testing.T, store *postgresstore.Store, pool *pgxpool.Pool, sessionID string, version int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	message, err := outbox.NewMessage(outbox.NewMessageInput{
		BusinessType: "learning.fixture", AggregateID: sessionID, IdempotencyKey: "learning-outbox-conflict",
		Revision: version, Generation: 1, Payload: json.RawMessage(`{"value":1}`),
		AuditMetadata: json.RawMessage(`{"actor":"learning-test"}`), MaxAttempts: 3,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	outboxStore := outboxpostgresstore.New(pool)
	inserted, err := outboxStore.Enqueue(ctx, message)
	if err != nil || !inserted {
		t.Fatalf("seed outbox message inserted=%v err=%v", inserted, err)
	}
	inserted, err = outboxStore.Enqueue(ctx, message)
	if err != nil || inserted {
		t.Fatalf("identical outbox replay inserted=%v err=%v", inserted, err)
	}
	var clockBefore, eventsBefore, outboxBefore int64
	if err := pool.QueryRow(ctx, `SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1`).Scan(&clockBefore); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_events`).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_messages`).Scan(&outboxBefore); err != nil {
		t.Fatal(err)
	}
	conflict := message
	conflict.Payload = json.RawMessage(`{"value":2}`)
	session, err := store.LoadSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	request := learning.CommitRequest{
		DeviceID: learningDeviceOne,
		Operation: learning.OperationEnvelope{
			OperationID: "54000000-0000-4000-8000-000000000002", PayloadSchemaVersion: 1,
			AggregateType: "session", AggregateID: sessionID, ExpectedVersion: version, Payload: json.RawMessage(`{}`),
		},
		RequestHash:  learning.SHA256([]byte("outbox conflict rollback")),
		Expectations: []learning.AggregateExpectation{{Type: "session", ID: sessionID, ExpectedVersion: version}},
		Batch: learning.CommandBatch{
			Session: &session, Outbox: []outbox.Message{conflict},
			Events: []learning.EventDraft{eventDraft(learning.EventTutoringStateChanged, sessionID, learning.SessionProjection{Session: session})},
		},
		ReceivedAt: now,
	}
	if _, err := store.Commit(ctx, request); !errors.Is(err, outboxpostgresstore.ErrIdempotencyConflict) {
		t.Fatalf("learning outbox conflict err=%v", err)
	}
	assertCount(t, pool, `SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1`, clockBefore)
	assertCount(t, pool, `SELECT count(*) FROM learning_events`, eventsBefore)
	assertCount(t, pool, `SELECT count(*) FROM outbox_messages`, outboxBefore)
	loaded, err := store.LoadSession(ctx, sessionID)
	if err != nil || loaded.AggregateVer != version {
		t.Fatalf("outbox conflict changed session=%+v err=%v", loaded, err)
	}
	var originalPayload bool
	if err := pool.QueryRow(ctx, `SELECT payload='{"value":1}'::jsonb FROM outbox_messages WHERE idempotency_key=$1`, message.IdempotencyKey).Scan(&originalPayload); err != nil || !originalPayload {
		t.Fatalf("outbox conflict changed original payload=%v err=%v", originalPayload, err)
	}
}

func assertSharedTransactionRollsBackTutoringAndOutbox(t *testing.T, store *postgresstore.Store, pool *pgxpool.Pool, sessionID string, version int64) {
	t.Helper()
	ctx := context.Background()
	var before int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_messages`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	message, err := outbox.NewMessage(outbox.NewMessageInput{BusinessType: "learning.fixture", AggregateID: sessionID, IdempotencyKey: "learning-outbox-rollback", Payload: json.RawMessage(`{}`), AuditMetadata: json.RawMessage(`{}`), MaxAttempts: 3}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_learning_projection_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected projection write failure'; END $$; CREATE TRIGGER reject_learning_projection_write BEFORE INSERT ON learning_projection_timeline FOR EACH ROW EXECUTE FUNCTION reject_learning_projection_write()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_learning_projection_write ON learning_projection_timeline; DROP FUNCTION IF EXISTS reject_learning_projection_write()`)
	})
	session, err := store.LoadSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	originalState, originalAttachedQuiz := session.State, session.AttachedQuiz
	session.State = tutoring.StateCompleted
	session.AttachedQuiz = !session.AttachedQuiz
	request := learning.CommitRequest{DeviceID: learningDeviceOne, Operation: learning.OperationEnvelope{OperationID: "54000000-0000-4000-8000-000000000001", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: version, Payload: json.RawMessage(`{}`)}, RequestHash: learning.SHA256([]byte("outbox rollback")), Expectations: []learning.AggregateExpectation{{Type: "session", ID: sessionID, ExpectedVersion: version}}, Batch: learning.CommandBatch{Session: &session, Outbox: []outbox.Message{message}, Events: []learning.EventDraft{eventDraft(learning.EventTutoringStateChanged, sessionID, learning.SessionProjection{Session: session})}}, ReceivedAt: time.Now().UTC()}
	if _, err := store.Commit(ctx, request); err == nil {
		t.Fatal("projection fault unexpectedly committed outbox command")
	}
	assertCount(t, pool, `SELECT count(*) FROM outbox_messages`, before)
	loaded, err := store.LoadSession(ctx, sessionID)
	if err != nil || loaded.AggregateVer != version || loaded.State != originalState || loaded.AttachedQuiz != originalAttachedQuiz {
		t.Fatalf("shared transaction rollback changed tutoring session: before_state=%s before_quiz=%v loaded=%+v err=%v", originalState, originalAttachedQuiz, loaded, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_learning_projection_write ON learning_projection_timeline; DROP FUNCTION reject_learning_projection_write()`); err != nil {
		t.Fatal(err)
	}
}

func assertFocusProjection(t *testing.T, store *postgresstore.Store, sessionID string, version int64) {
	t.Helper()
	view, err := store.Session(context.Background(), sessionID)
	if err != nil || view.Session.AggregateVer != version || view.Session.ActiveFrame == nil || view.Session.ActiveFrame.Context.FocusNodeRevisionID != learningNodeRevisionID || view.Metadata.AsOfEventSequence == 0 {
		t.Fatalf("focus projection=%+v err=%v", view, err)
	}
}

func assertConcurrentRebuildTail(t *testing.T, store *postgresstore.Store, pool *pgxpool.Pool) learning.ProjectionStatus {
	t.Helper()
	ctx := context.Background()
	blocker, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `LOCK TABLE learning_events IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	rebuildResult := make(chan error, 1)
	go func() { _, err := store.Rebuild(ctx); rebuildResult <- err }()
	for attempts := 0; ; attempts++ {
		status, err := store.ProjectionStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if status.Metadata.Rebuilding {
			break
		}
		if attempts >= 100 {
			t.Fatal("rebuild marker was not observable")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var initialTarget int64
	if err := pool.QueryRow(ctx, `SELECT g.target_high_water FROM learning_projection_head h JOIN learning_projection_generations g ON g.id=h.rebuilding_generation_id WHERE h.singleton_id=1`).Scan(&initialTarget); err != nil {
		t.Fatal(err)
	}
	request := goalCommitFor(t, learningDeviceTwo, "55000000-0000-4000-8000-000000000001", "55000000-0000-4000-8000-000000000002", "55000000-0000-4000-8000-000000000003", 0, 1, 1)
	type commitOutcome struct {
		result learning.OperationResult
		err    error
	}
	commitResult := make(chan commitOutcome, 1)
	go func() {
		result, err := store.Commit(ctx, request)
		commitResult <- commitOutcome{result: result, err: err}
	}()
	for attempts := 0; ; attempts++ {
		var clock int64
		err := pool.QueryRow(ctx, `SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1 FOR UPDATE NOWAIT`).Scan(&clock)
		var lockErr *pgconn.PgError
		if errors.As(err, &lockErr) && lockErr.Code == "55P03" {
			break
		}
		if err != nil {
			t.Fatalf("probe tail commit event-clock lock: %v", err)
		}
		if attempts >= 100 {
			t.Fatal("tail commit did not acquire the event clock before rebuild release")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-rebuildResult; err != nil {
		t.Fatal(err)
	}
	outcome := <-commitResult
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome.result.FirstEventSequence <= initialTarget {
		t.Fatalf("tail commit sequence=%d initial rebuild target=%d", outcome.result.FirstEventSequence, initialTarget)
	}
	status, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Metadata.Rebuilding || status.Metadata.AsOfEventSequence != status.HighWater {
		t.Fatalf("concurrent rebuild did not catch committed tail: %+v", status)
	}
	return status
}

func assertUnknownSchemaFailsGeneration(t *testing.T, pool *pgxpool.Pool, store *postgresstore.Store) {
	t.Helper()
	ctx := context.Background()
	before, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var sequence int64
	if err := tx.QueryRow(ctx, `SELECT current_event_seq+1 FROM learning_event_clock WHERE singleton_id=1 FOR UPDATE`).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"kind":"future"}`)
	hash := learning.SHA256(payload)
	if _, err := tx.Exec(ctx, `INSERT INTO learning_aggregate_heads(aggregate_type,aggregate_id,aggregate_version,last_event_seq,updated_at) VALUES('goal',$1,1,$2,now())`, "56000000-0000-4000-8000-000000000001", sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO learning_event_payloads(id,payload,payload_hash,created_at) VALUES($1,$2,decode($3,'hex'),now())`, "56000000-0000-4000-8000-000000000002", payload, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO learning_events(event_seq,id,event_type,event_schema_version,aggregate_type,aggregate_id,aggregate_version,device_id,operation_id,operation_ordinal,received_at,payload_id,payload_hash) VALUES($1,$2,'ExposureRecorded',99,'goal',$3,1,$4,$5,0,now(),$6,decode($7,'hex'))`, sequence, "56000000-0000-4000-8000-000000000003", "56000000-0000-4000-8000-000000000001", learningDeviceOne, "56000000-0000-4000-8000-000000000004", "56000000-0000-4000-8000-000000000002", hash); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE learning_event_clock SET current_event_seq=$1,updated_at=now() WHERE singleton_id=1`, sequence); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Rebuild(ctx); learning.ErrorCode(err) != learning.CodeUnsupportedEventSchema {
		t.Fatalf("unknown schema rebuild error=%v", err)
	}
	after, err := store.ProjectionStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.ActiveGenerationID != before.ActiveGenerationID || after.RebuildingGenerationID != nil || !after.Metadata.Incomplete || !contains(after.Metadata.ReasonCodes, learning.CodeUnsupportedEventSchema) {
		t.Fatalf("unknown schema generation state=%+v", after)
	}
	var failed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM learning_projection_generations WHERE status='failed' AND $1=ANY(reason_codes)`, learning.CodeUnsupportedEventSchema).Scan(&failed); err != nil || failed == 0 {
		t.Fatalf("unknown schema failed generation count=%d err=%v", failed, err)
	}
}

func eventDraft(kind learning.EventType, sessionID string, payload any) learning.EventDraft {
	encoded, _ := json.Marshal(payload)
	return learning.EventDraft{Type: kind, AggregateType: "session", AggregateID: sessionID, Payload: encoded}
}

func contains(values []string, value string) bool {
	for _, current := range values {
		if current == value {
			return true
		}
	}
	return false
}

func goalCommit(t *testing.T, deviceID, operationID, revisionID string, expectedVersion, revision int64, eventCount int, previousRevisionID ...string) learning.CommitRequest {
	return goalCommitFor(t, deviceID, operationID, learningGoalID, revisionID, expectedVersion, revision, eventCount, previousRevisionID...)
}

func goalCommitFor(t *testing.T, deviceID, operationID, goalID, revisionID string, expectedVersion, revision int64, eventCount int, previousRevisionID ...string) learning.CommitRequest {
	t.Helper()
	now := time.Date(2026, 8, 20, 12, 0, int(expectedVersion), 0, time.UTC)
	goal := learning.GoalRevision{ID: revisionID, GoalID: goalID, Revision: revision, Text: fmt.Sprintf("goal-%d", revision), Source: "integration", ActorDeviceID: deviceID, CreatedAt: now}
	if len(previousRevisionID) > 0 {
		goal.PreviousRevisionID = &previousRevisionID[0]
	}
	payload, err := json.Marshal(goal)
	if err != nil {
		t.Fatal(err)
	}
	events := make([]learning.EventDraft, eventCount)
	for index := range events {
		events[index] = learning.EventDraft{Type: learning.EventGoalRevisionCreated, AggregateType: "goal", AggregateID: goalID, Payload: payload}
	}
	return learning.CommitRequest{DeviceID: deviceID, Operation: learning.OperationEnvelope{OperationID: operationID, PayloadSchemaVersion: 1, AggregateType: "goal", AggregateID: goalID, ExpectedVersion: expectedVersion, Payload: json.RawMessage(`{"command":"goal"}`)}, RequestHash: learning.SHA256([]byte(deviceID + operationID)), Expectations: []learning.AggregateExpectation{{Type: "goal", ID: goalID, ExpectedVersion: expectedVersion}}, Batch: learning.CommandBatch{GoalRevision: &goal, Events: events, TypedResult: payload}, ReceivedAt: now}
}

func learningIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL learning integration suite not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("learning_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
func assertCount(t *testing.T, pool *pgxpool.Pool, query string, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(context.Background(), query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("query %q = %d, want %d", query, got, want)
	}
}
