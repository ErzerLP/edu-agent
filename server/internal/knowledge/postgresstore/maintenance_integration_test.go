package postgresstore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	learningpostgres "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	tutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgreSQLKnowledgeMaintenanceProposalLifecycle(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL knowledge maintenance test not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("knowledge_maintenance_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO devices(id,display_name,created_at)
		VALUES($1,'knowledge maintenance actor',clock_timestamp())`, integrationActorID); err != nil {
		t.Fatal(err)
	}
	knowledgeStore := postgresstore.New(pool, postgresstore.WithNotesyncPublication())
	tutoringStore := tutoringpostgres.New(pool)
	learningStore := learningpostgres.New(pool, tutoringStore, knowledgeStore)
	service, err := knowledge.NewService(knowledgeStore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{
		MaintenanceStore: knowledgeStore, EvidenceImpactReader: learningStore,
	})
	if err != nil {
		t.Fatal(err)
	}

	base, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "61000000-0000-4000-8000-000000000001", ExpectedParentProvided: true,
		Source: "knowledge-maintenance-postgres", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "topic.md", Markdown: "# Topic\none two three four five six seven eight nine ten\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	seedAcceptedEvidenceForMaintenance(t, pool, base.Revision)
	learningBefore := readLearningWriteCounts(t, pool)
	exportedBase, err := service.Export(ctx, base.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	autoCommand := knowledge.CreateProposalCommand{
		RequestID: "62000000-0000-4000-8000-000000000001", BaseRevisionID: base.Revision.ID,
		ActorDeviceID: integrationActorID,
		Sources:       []knowledge.ProposalSource{maintenancePostgresSource("note", "agent/auto", "secret-auto-source")},
		CandidateSnapshot: []knowledge.ImportDocument{
			{Path: "topic.md", Markdown: exportedBase.Documents[0].Markdown},
			{Path: "added.md", Markdown: "# Added\nsecret-candidate-body\n"},
		},
	}
	auto, err := service.Create(ctx, autoCommand)
	if err != nil {
		t.Fatal(err)
	}
	if auto.Status != knowledge.ProposalApplied || !auto.Risk.AutoApply || auto.AppliedRevisionID == "" || auto.Origin == nil {
		t.Fatalf("unexpected PostgreSQL auto proposal: %+v", auto)
	}
	replay, err := service.Create(ctx, autoCommand)
	if err != nil || !replay.Replayed || replay.ID != auto.ID {
		t.Fatalf("PostgreSQL proposal replay=%+v err=%v", replay, err)
	}
	assertLearningWriteCounts(t, pool, learningBefore)

	currentExport, err := service.Export(ctx, auto.AppliedRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	makeOpen := func(requestID, title, locator string) knowledge.Proposal {
		candidate := maintenanceImportDocuments(currentExport.Documents)
		for index := range candidate {
			if candidate[index].Path == "topic.md" {
				candidate[index].Markdown = strings.Replace(candidate[index].Markdown, "# Topic", "# "+title, 1)
			}
		}
		proposal, err := service.Create(ctx, knowledge.CreateProposalCommand{
			RequestID: requestID, BaseRevisionID: auto.AppliedRevisionID, ActorDeviceID: integrationActorID,
			Sources:           []knowledge.ProposalSource{maintenancePostgresSource("note", locator, "secret-"+title)},
			CandidateSnapshot: candidate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if proposal.Status != knowledge.ProposalOpen || proposal.EvidenceImpact.Count != 1 || proposal.Risk.AutoApply {
			t.Fatalf("proposal did not persist accepted evidence impact: %+v", proposal)
		}
		return proposal
	}
	first := makeOpen("62000000-0000-4000-8000-000000000011", "First", "agent/first")
	second := makeOpen("62000000-0000-4000-8000-000000000012", "Second", "agent/second")

	start := make(chan struct{})
	type decisionOutcome struct {
		proposal knowledge.Proposal
		err      error
	}
	outcomes := make(chan decisionOutcome, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for index, proposal := range []knowledge.Proposal{first, second} {
		go func(index int, proposal knowledge.Proposal) {
			ready.Done()
			<-start
			result, err := service.Decide(ctx, knowledge.ProposalDecisionCommand{
				OperationID: fmt.Sprintf("63000000-0000-4000-8000-%012d", 11+index), ProposalID: proposal.ID,
				Decision: "approve", Reason: "concurrent approval", ActorDeviceID: integrationActorID,
			})
			outcomes <- decisionOutcome{proposal: result, err: err}
		}(index, proposal)
	}
	ready.Wait()
	close(start)
	var applied, stale knowledge.Proposal
	for range 2 {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		switch outcome.proposal.Status {
		case knowledge.ProposalApplied:
			applied = outcome.proposal
		case knowledge.ProposalStale:
			stale = outcome.proposal
		default:
			t.Fatalf("unexpected concurrent proposal outcome: %+v", outcome.proposal)
		}
	}
	if applied.ID == "" || stale.ID == "" || stale.CurrentRevisionID != applied.AppliedRevisionID {
		t.Fatalf("concurrent proposal results applied=%+v stale=%+v", applied, stale)
	}
	var sameParentRevisions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_revisions WHERE parent_revision_id=$1`, auto.AppliedRevisionID).Scan(&sameParentRevisions); err != nil || sameParentRevisions != 1 {
		t.Fatalf("same-base revisions=%d err=%v", sameParentRevisions, err)
	}
	assertLearningWriteCounts(t, pool, learningBefore)

	rollback, err := service.CreateRollback(ctx, knowledge.CreateRollbackCommand{
		RequestID: "62000000-0000-4000-8000-000000000021", BaseRevisionID: applied.AppliedRevisionID,
		TargetRevisionID: base.Revision.ID, ActorDeviceID: integrationActorID,
		Sources: []knowledge.ProposalSource{maintenancePostgresSource("note", "human/rollback", "secret-rollback-reason")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rollback.Status != knowledge.ProposalOpen || rollback.Risk.AutoApply || !rollback.LineageImpact.Rollback {
		t.Fatalf("rollback proposal=%+v", rollback)
	}
	rolledBack, err := service.Decide(ctx, knowledge.ProposalDecisionCommand{
		OperationID: "63000000-0000-4000-8000-000000000021", ProposalID: rollback.ID,
		Decision: "approve", Reason: "restore ancestor snapshot", ActorDeviceID: integrationActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	rollbackRevision, err := service.Tree(ctx, rolledBack.AppliedRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	if rollbackRevision.Revision.ParentRevisionID == nil || *rollbackRevision.Revision.ParentRevisionID != applied.AppliedRevisionID ||
		rollbackRevision.Revision.ManifestHash != base.Revision.ManifestHash || rollbackRevision.Revision.ID == base.Revision.ID ||
		rollbackRevision.Revision.Origin == nil || rollbackRevision.Revision.Origin.ProposalID != rollback.ID ||
		rollbackRevision.Revision.Origin.RollbackTargetRevisionID == nil || *rollbackRevision.Revision.Origin.RollbackTargetRevisionID != base.Revision.ID {
		t.Fatalf("rollback revision is not appended ancestor content with origin: %+v", rollbackRevision.Revision)
	}
	if _, err := service.Tree(ctx, applied.AppliedRevisionID); err != nil {
		t.Fatalf("intervening revision was not preserved: %v", err)
	}
	assertLearningWriteCounts(t, pool, learningBefore)

	faultBase := rolledBack.AppliedRevisionID
	faultExport, err := service.Export(ctx, faultBase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION reject_knowledge_maintenance_origin() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected knowledge maintenance origin failure'; END $$;
		CREATE TRIGGER reject_knowledge_maintenance_origin
		BEFORE INSERT ON knowledge_revision_origins
		FOR EACH ROW EXECUTE FUNCTION reject_knowledge_maintenance_origin()`); err != nil {
		t.Fatal(err)
	}
	beforeFault := readMaintenanceAtomicCounts(t, pool)
	faultCandidate := maintenanceImportDocuments(faultExport.Documents)
	faultCandidate = append(faultCandidate, knowledge.ImportDocument{Path: "fault.md", Markdown: "# Fault\nsecret-fault-candidate\n"})
	if _, err := service.Create(ctx, knowledge.CreateProposalCommand{
		RequestID: "62000000-0000-4000-8000-000000000031", BaseRevisionID: faultBase,
		ActorDeviceID:     integrationActorID,
		Sources:           []knowledge.ProposalSource{maintenancePostgresSource("note", "agent/fault", "secret-fault-source")},
		CandidateSnapshot: faultCandidate,
	}); err == nil {
		t.Fatal("injected auto-apply failure unexpectedly succeeded")
	}
	afterFault := readMaintenanceAtomicCounts(t, pool)
	if beforeFault != afterFault {
		t.Fatalf("failed auto apply left partial state before=%+v after=%+v", beforeFault, afterFault)
	}
	var headAfterFault string
	if err := pool.QueryRow(ctx, `SELECT head_revision_id::text FROM knowledge_catalog WHERE singleton_id=1`).Scan(&headAfterFault); err != nil || headAfterFault != faultBase {
		t.Fatalf("failed auto apply changed head=%s err=%v", headAfterFault, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_knowledge_maintenance_origin ON knowledge_revision_origins`); err != nil {
		t.Fatal(err)
	}

	failedApproval := makeOpenOnHead(t, service, faultBase, faultExport.Documents, "62000000-0000-4000-8000-000000000032")
	if _, err := pool.Exec(ctx, `CREATE TRIGGER reject_knowledge_maintenance_origin BEFORE INSERT ON knowledge_revision_origins FOR EACH ROW EXECUTE FUNCTION reject_knowledge_maintenance_origin()`); err != nil {
		t.Fatal(err)
	}
	beforeApprovalFault := readMaintenanceAtomicCounts(t, pool)
	if _, err := service.Decide(ctx, knowledge.ProposalDecisionCommand{
		OperationID: "63000000-0000-4000-8000-000000000032", ProposalID: failedApproval.ID,
		Decision: "approve", Reason: "secret approval reason", ActorDeviceID: integrationActorID,
	}); err == nil {
		t.Fatal("injected approved apply failure unexpectedly succeeded")
	}
	afterApprovalFault := readMaintenanceAtomicCounts(t, pool)
	if beforeApprovalFault != afterApprovalFault {
		t.Fatalf("failed approval left partial state before=%+v after=%+v", beforeApprovalFault, afterApprovalFault)
	}
	stillOpen, err := service.Get(ctx, failedApproval.ID)
	if err != nil || stillOpen.Status != knowledge.ProposalOpen || stillOpen.Decision != nil {
		t.Fatalf("failed approval did not leave proposal open: %+v err=%v", stillOpen, err)
	}
	firstPage, err := service.List(ctx, knowledge.ProposalListCommand{Status: "all", Limit: 2})
	if err != nil || len(firstPage.Items) != 2 || firstPage.NextCursor == "" {
		t.Fatalf("first proposal page=%+v err=%v", firstPage, err)
	}
	secondPage, err := service.List(ctx, knowledge.ProposalListCommand{Status: "all", Limit: 100, Cursor: firstPage.NextCursor})
	if err != nil || len(secondPage.Items) == 0 {
		t.Fatalf("second proposal page=%+v err=%v", secondPage, err)
	}
	for _, left := range firstPage.Items {
		for _, right := range secondPage.Items {
			if left.ID == right.ID {
				t.Fatalf("proposal cursor repeated %s", left.ID)
			}
		}
	}
	openPage, err := service.List(ctx, knowledge.ProposalListCommand{Status: "open", Limit: 100})
	if err != nil || len(openPage.Items) != 1 || openPage.Items[0].ID != failedApproval.ID {
		t.Fatalf("open proposal page=%+v err=%v", openPage, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_knowledge_maintenance_origin ON knowledge_revision_origins; DROP FUNCTION reject_knowledge_maintenance_origin()`); err != nil {
		t.Fatal(err)
	}
	assertLearningWriteCounts(t, pool, learningBefore)

	redaction := privacy.LocalRedactionRequest{
		ErasureID: "64000000-0000-4000-8000-000000000001", Store: privacy.StoreKnowledgeContent,
		ReceiptID: "64000000-0000-4000-8000-000000000002", LearnerGeneration: 2,
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasures(
		  id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,
		  target_learner_generation,managed_backup_scheduled_unrecoverable_after)
		VALUES($1,$2,$3,decode(repeat('64',32),'hex'),'learner_request',$2,
		       clock_timestamp(),$4,clock_timestamp()+interval '1 day')`,
		redaction.ErasureID, integrationActorID, "64000000-0000-4000-8000-000000000003", redaction.LearnerGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION privacy_begin_owner_scrub(
		  requested_erasure_id UUID,requested_target_generation BIGINT,
		  requested_owner_kind TEXT,requested_receipt_id UUID
		) RETURNS UUID LANGUAGE sql AS $$ SELECT requested_receipt_id $$;
		CREATE OR REPLACE FUNCTION privacy_owner_scrub_permitted(requested_owner_kind TEXT)
		RETURNS BOOLEAN LANGUAGE sql STABLE AS $$ SELECT TRUE $$`); err != nil {
		t.Fatal(err)
	}
	if err := knowledgeStore.RedactTx(ctx, redaction); err != nil {
		t.Fatal(err)
	}
	residual, err := knowledgeStore.VerifyRedacted(ctx, redaction)
	if err != nil || residual != 0 {
		t.Fatalf("knowledge maintenance privacy residual=%d err=%v", residual, err)
	}
	redacted, err := service.Get(ctx, failedApproval.ID)
	if err != nil || redacted.Status != knowledge.ProposalRedacted || !redacted.Redacted || len(redacted.CandidateSnapshot) != 0 || len(redacted.Sources) != 0 || len(redacted.Diff) != 0 || redacted.BasisHash != "" {
		t.Fatalf("redacted proposal leaked payload: %+v err=%v", redacted, err)
	}
	var leaked bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM knowledge_maintenance_proposals
		  WHERE record::text LIKE '%secret-%' OR prepared_commit::text LIKE '%secret-%'
		  UNION ALL
		  SELECT 1 FROM knowledge_maintenance_decisions WHERE reason LIKE '%secret%'
		)`).Scan(&leaked); err != nil || leaked {
		t.Fatalf("knowledge maintenance privacy payload leak=%v err=%v", leaked, err)
	}
}

type maintenanceAtomicCounts struct {
	Proposals  int64
	Decisions  int64
	Operations int64
	Revisions  int64
	Origins    int64
	Outbox     int64
}

func readMaintenanceAtomicCounts(t *testing.T, pool *pgxpool.Pool) maintenanceAtomicCounts {
	t.Helper()
	var value maintenanceAtomicCounts
	if err := pool.QueryRow(context.Background(), `
		SELECT
		  (SELECT count(*) FROM knowledge_maintenance_proposals),
		  (SELECT count(*) FROM knowledge_maintenance_decisions),
		  (SELECT count(*) FROM knowledge_maintenance_operations),
		  (SELECT count(*) FROM knowledge_revisions),
		  (SELECT count(*) FROM knowledge_revision_origins),
		  (SELECT count(*) FROM outbox_messages)`).Scan(
		&value.Proposals, &value.Decisions, &value.Operations, &value.Revisions, &value.Origins, &value.Outbox,
	); err != nil {
		t.Fatal(err)
	}
	return value
}

type learningWriteCounts struct {
	Events             int64
	Evidence           int64
	ProjectionEvidence int64
	ProjectionNodes    int64
	ProjectionTimeline int64
}

func readLearningWriteCounts(t *testing.T, pool *pgxpool.Pool) learningWriteCounts {
	t.Helper()
	var value learningWriteCounts
	if err := pool.QueryRow(context.Background(), `
		SELECT
		  (SELECT count(*) FROM learning_events),
		  (SELECT count(*) FROM learning_evidence),
		  (SELECT count(*) FROM learning_projection_evidence),
		  (SELECT count(*) FROM learning_projection_nodes),
		  (SELECT count(*) FROM learning_projection_timeline)`).Scan(
		&value.Events, &value.Evidence, &value.ProjectionEvidence, &value.ProjectionNodes, &value.ProjectionTimeline,
	); err != nil {
		t.Fatal(err)
	}
	return value
}

func assertLearningWriteCounts(t *testing.T, pool *pgxpool.Pool, expected learningWriteCounts) {
	t.Helper()
	if actual := readLearningWriteCounts(t, pool); actual != expected {
		t.Fatalf("knowledge maintenance wrote learning state expected=%+v actual=%+v", expected, actual)
	}
}

func maintenancePostgresSource(kind, locator, excerpt string) knowledge.ProposalSource {
	hash := sha256.Sum256([]byte(excerpt))
	return knowledge.ProposalSource{Kind: kind, Locator: locator, Excerpt: excerpt, SHA256: hex.EncodeToString(hash[:])}
}

func maintenanceImportDocuments(exported []knowledge.ExportDocument) []knowledge.ImportDocument {
	result := make([]knowledge.ImportDocument, len(exported))
	for index := range exported {
		result[index] = knowledge.ImportDocument{Path: exported[index].Path, Markdown: exported[index].Markdown}
	}
	return result
}

func makeOpenOnHead(t *testing.T, service *knowledge.Service, headID string, exported []knowledge.ExportDocument, requestID string) knowledge.Proposal {
	t.Helper()
	candidate := make([]knowledge.ImportDocument, len(exported))
	for index := range exported {
		candidate[index] = knowledge.ImportDocument{Path: exported[index].Path, Markdown: exported[index].Markdown}
		if candidate[index].Path == "topic.md" {
			candidate[index].Markdown = strings.Replace(candidate[index].Markdown, "# Topic", "# Approval Fault", 1)
		}
	}
	proposal, err := service.Create(t.Context(), knowledge.CreateProposalCommand{
		RequestID: requestID, BaseRevisionID: headID, ActorDeviceID: integrationActorID,
		Sources:           []knowledge.ProposalSource{maintenancePostgresSource("note", "agent/approval-fault", "secret-approval-source")},
		CandidateSnapshot: candidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Status != knowledge.ProposalOpen {
		t.Fatalf("approved-failure fixture was not open: %+v", proposal)
	}
	return proposal
}

func seedAcceptedEvidenceForMaintenance(t *testing.T, pool *pgxpool.Pool, revision knowledge.KnowledgeRevision) {
	t.Helper()
	ctx := context.Background()
	document := revision.Documents[0].Revision
	node := document.Nodes[1]
	const (
		goalRevisionID  = "65000000-0000-4000-8000-000000000001"
		goalID          = "65000000-0000-4000-8000-000000000002"
		routeRevisionID = "65000000-0000-4000-8000-000000000003"
		routeID         = "65000000-0000-4000-8000-000000000004"
		stepID          = "65000000-0000-4000-8000-000000000005"
		sessionID       = "65000000-0000-4000-8000-000000000006"
		activityID      = "65000000-0000-4000-8000-000000000007"
		payloadID       = "65000000-0000-4000-8000-000000000008"
		attemptID       = "65000000-0000-4000-8000-000000000009"
		assessmentID    = "65000000-0000-4000-8000-000000000010"
		decisionID      = "65000000-0000-4000-8000-000000000011"
		evidenceID      = "65000000-0000-4000-8000-000000000012"
	)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO learning_goal_revisions(id,goal_id,revision,goal_text,source,actor_device_id,created_at)
		VALUES($1,$2,1,'maintenance evidence goal','fixture',$3,clock_timestamp())`, goalRevisionID, goalID, integrationActorID)
	exec(`INSERT INTO learning_route_revisions(id,route_id,revision,goal_revision_id,knowledge_revision_id,route_policy_version,created_at)
		VALUES($1,$2,1,$3,$4,'route-policy-v1',clock_timestamp())`, routeRevisionID, routeID, goalRevisionID, revision.ID)
	exec(`INSERT INTO learning_route_steps(id,route_revision_id,ordinal,knowledge_revision_id,node_id,node_revision_id,document_revision_id,teaching_intent,completion_condition)
		VALUES($1,$2,0,$3,$4,$5,$6,'teach','pass')`, stepID, routeRevisionID, revision.ID, node.NodeID, node.ID, document.ID)
	exec(`INSERT INTO tutoring_sessions(id,aggregate_version,state,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,focus_node_revision_id,started_at,updated_at)
		VALUES($1,1,'Completed',$2,$3,$4,$5,$6,clock_timestamp(),clock_timestamp())`, sessionID, goalRevisionID, routeRevisionID, stepID, revision.ID, node.ID)
	exec(`INSERT INTO learning_activities(
		  id,revision,session_id,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,
		  target_node_id,target_node_revision_id,prompt,activity_type,rubric_revision,rubric,difficulty,
		  allowed_help,activity_policy_version,assessment_policy_version,review_policy_version,created_at)
		VALUES($1,1,$2,$3,$4,$5,$6,$7,$8,'question','objective','r1','{}'::jsonb,1,
		       ARRAY['none'],'activity-policy-v1','assessment-acceptance-v1','fixed-interval-v1',clock_timestamp())`,
		activityID, sessionID, goalRevisionID, routeRevisionID, stepID, revision.ID, node.NodeID, node.ID)
	exec(`INSERT INTO learning_attempt_payloads(id,answer_text,payload_hash,created_at)
		VALUES($1,'answer',decode(repeat('13',32),'hex'),clock_timestamp())`, payloadID)
	exec(`INSERT INTO learning_attempts(id,session_id,activity_id,activity_revision,answer_payload_id,help_level,actor_device_id,received_at,payload_hash)
		VALUES($1,$2,$3,1,$4,'none',$5,clock_timestamp(),decode(repeat('14',32),'hex'))`, attemptID, sessionID, activityID, payloadID, integrationActorID)
	exec(`INSERT INTO learning_activity_evidence_claims(activity_id,activity_revision,winning_attempt_id,claim_source,claimed_event_seq,claimed_at)
		VALUES($1,1,$2,'online',900001,clock_timestamp())`, activityID, attemptID)
	exec(`INSERT INTO learning_assessments(
		  id,session_id,attempt_id,activity_id,activity_revision,rubric_complete,confidence,risk_flags,
		  trusted_model_id,model_parameters,prompt_revision,proposal_input_hash,model_attempts,attempt_categories,created_at)
		VALUES($1,$2,$3,$4,1,TRUE,1000,ARRAY[]::text[],'fixture','{}'::jsonb,'p1',decode(repeat('15',32),'hex'),1,ARRAY[]::text[],clock_timestamp())`,
		assessmentID, sessionID, attemptID, activityID)
	exec(`INSERT INTO learning_assessment_decisions(id,assessment_id,version,disposition,conclusions,actor_device_id,created_at)
		VALUES($1,$2,1,'accepted','[]'::jsonb,$3,clock_timestamp())`, decisionID, assessmentID, integrationActorID)
	exec(`INSERT INTO learning_evidence(
		  id,decision_id,assessment_id,session_id,attempt_id,activity_id,activity_revision,goal_revision_id,
		  route_revision_id,knowledge_revision_id,node_revision_id,node_id,document_revision_id,rubric_revision,
		  evidence_kind,activity_type,outcome,help_level,received_at,accepted_event_seq,acceptance_policy_version,
		  reducer_policy_version,review_policy_version,misconception_candidates,rubric_outcomes)
		VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$9,$10,$11,$12,'r1','practice_recall','objective',
		       'pass','none',clock_timestamp(),900001,'assessment-acceptance-v1','mastery-reducer-v1',
		       'fixed-interval-v1','[]'::jsonb,'[]'::jsonb)`,
		evidenceID, decisionID, assessmentID, sessionID, attemptID, activityID, goalRevisionID,
		routeRevisionID, revision.ID, node.ID, node.NodeID, document.ID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
