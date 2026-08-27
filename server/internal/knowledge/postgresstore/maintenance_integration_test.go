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
	"github.com/edu-agent/edu-agent/server/internal/learning"
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

func TestPostgreSQLKnowledgeMaintenanceEvidenceCarryoverChain(t *testing.T) {
	fixture := newKnowledgeMaintenanceCarryoverFixture(t)
	ctx := t.Context()
	base := importKnowledgeMaintenanceCarryoverBase(t, fixture.knowledge)
	evidence := seedAcceptedEvidenceForMaintenance(t, fixture.pool, base.Revision)
	learningBefore := readLearningWriteCounts(t, fixture.pool)

	first := createOpenEvidenceMaintenance(t, fixture.knowledge, base.Revision.ID,
		"66000000-0000-4000-8000-000000000001", "Topic First")
	if len(first.EvidenceImpact.References) != 1 || first.EvidenceImpact.References[0] != (knowledge.AcceptedEvidenceReference{
		EvidenceID: evidence.EvidenceID, NodeRevisionID: evidence.NodeRevisionID,
		KnowledgeRevisionID: evidence.KnowledgeRevisionID,
	}) {
		t.Fatalf("first maintenance direct evidence impact=%+v", first.EvidenceImpact)
	}
	firstDecision := knowledge.ProposalDecisionCommand{
		OperationID: "66000000-0000-4000-8000-000000000002", ProposalID: first.ID,
		Decision: "approve", Reason: "approve first carryover source", ActorDeviceID: integrationActorID,
	}
	beforeFailure := readMaintenanceAtomicCounts(t, fixture.pool)
	if _, err := fixture.pool.Exec(ctx, `
		CREATE FUNCTION reject_knowledge_head_after_carryover() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected knowledge head failure after carryover'; END $$;
		CREATE TRIGGER reject_knowledge_head_after_carryover
		BEFORE UPDATE ON knowledge_catalog
		FOR EACH ROW EXECUTE FUNCTION reject_knowledge_head_after_carryover()`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.knowledge.Decide(ctx, firstDecision); err == nil {
		t.Fatal("knowledge approval unexpectedly survived injected post-carryover failure")
	}
	if afterFailure := readMaintenanceAtomicCounts(t, fixture.pool); afterFailure != beforeFailure {
		t.Fatalf("failed knowledge approval left revision or carryover rows before=%+v after=%+v", beforeFailure, afterFailure)
	}
	stillOpen, err := fixture.knowledge.Get(ctx, first.ID)
	if err != nil || stillOpen.Status != knowledge.ProposalOpen || stillOpen.Decision != nil {
		t.Fatalf("failed knowledge approval did not remain open proposal=%+v err=%v", stillOpen, err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		DROP TRIGGER reject_knowledge_head_after_carryover ON knowledge_catalog;
		DROP FUNCTION reject_knowledge_head_after_carryover()`); err != nil {
		t.Fatal(err)
	}

	firstApplied, err := fixture.knowledge.Decide(ctx, firstDecision)
	if err != nil {
		t.Fatal(err)
	}
	if firstApplied.Status != knowledge.ProposalApplied {
		t.Fatalf("first maintenance approval=%+v", firstApplied)
	}
	assertLearningWriteCounts(t, fixture.pool, learningBefore)
	firstCarryover := loadCarryoverForKnowledgeProposal(t, fixture, firstApplied.ID)
	if firstCarryover.Status != learning.EvidenceCarryoverOpen || len(firstCarryover.Candidates) != 1 ||
		firstCarryover.SourceEvidenceID != evidence.EvidenceID ||
		firstCarryover.SourceKnowledgeRevisionID != evidence.KnowledgeRevisionID ||
		firstCarryover.SourceNodeRevisionID != evidence.NodeRevisionID ||
		firstCarryover.TargetKnowledgeRevisionID != firstApplied.AppliedRevisionID {
		t.Fatalf("first provisional carryover=%+v", firstCarryover)
	}
	assertCarryoverRows(t, fixture.pool, firstCarryover.ID, 0, 0)

	authorityBefore := readCarryoverAuthoritySnapshot(t, fixture.pool, evidence.EvidenceID)
	approveFirst := learning.EvidenceCarryoverDecisionCommand{
		OperationID: "66000000-0000-4000-8000-000000000003", ProposalID: firstCarryover.ID,
		Decision: "approve", Reason: "equivalent first revision",
	}
	firstApproved, err := fixture.learning.DecideEvidenceCarryover(ctx, integrationActorID, approveFirst)
	if err != nil {
		t.Fatal(err)
	}
	if firstApproved.Status != learning.EvidenceCarryoverApproved || firstApproved.Decision == nil ||
		len(firstApproved.Links) != 1 || firstApproved.Links[0].SourceEvidenceID != evidence.EvidenceID {
		t.Fatalf("first carryover approval=%+v", firstApproved)
	}
	assertCarryoverRows(t, fixture.pool, firstCarryover.ID, 1, 1)
	if replay, err := fixture.learning.DecideEvidenceCarryover(ctx, integrationActorID, approveFirst); err != nil ||
		!replay.Replayed || replay.Decision == nil || replay.Decision.ID != firstApproved.Decision.ID ||
		len(replay.Links) != 1 || replay.Links[0].ID != firstApproved.Links[0].ID {
		t.Fatalf("carryover replay=%+v err=%v", replay, err)
	}
	conflict := approveFirst
	conflict.Reason = "different replay payload"
	if _, err := fixture.learning.DecideEvidenceCarryover(ctx, integrationActorID, conflict); learning.ErrorCode(err) != learning.CodeOperationConflict {
		t.Fatalf("carryover changed replay error=%v code=%q", err, learning.ErrorCode(err))
	}
	if authorityAfter := readCarryoverAuthoritySnapshot(t, fixture.pool, evidence.EvidenceID); authorityAfter != authorityBefore {
		t.Fatalf("carryover approval changed evidence/mastery/review before=%+v after=%+v", authorityBefore, authorityAfter)
	}

	combined, err := fixture.learningStore.AcceptedEvidenceImpact(ctx, []string{
		evidence.NodeRevisionID, firstApproved.Links[0].TargetNodeRevisionID,
		firstApproved.Links[0].TargetNodeRevisionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSortedUniqueEvidenceImpact(t, combined)
	if combined.Count != 2 {
		t.Fatalf("direct plus approved-link impact=%+v", combined)
	}

	second := createOpenEvidenceMaintenance(t, fixture.knowledge, firstApplied.AppliedRevisionID,
		"66000000-0000-4000-8000-000000000004", "Topic Second")
	if len(second.EvidenceImpact.References) != 1 || second.EvidenceImpact.References[0] != (knowledge.AcceptedEvidenceReference{
		EvidenceID: evidence.EvidenceID, NodeRevisionID: firstApproved.Links[0].TargetNodeRevisionID,
		KnowledgeRevisionID: firstApproved.Links[0].TargetKnowledgeRevisionID,
	}) {
		t.Fatalf("second maintenance approved-link impact=%+v first_link=%+v", second.EvidenceImpact, firstApproved.Links[0])
	}
	secondApplied := approveKnowledgeMaintenance(t, fixture.knowledge, second,
		"66000000-0000-4000-8000-000000000005", "apply second revision")
	secondCarryover := loadCarryoverForKnowledgeProposal(t, fixture, secondApplied.ID)
	if secondCarryover.SourceEvidenceID != evidence.EvidenceID ||
		secondCarryover.SourceKnowledgeRevisionID != firstApproved.Links[0].TargetKnowledgeRevisionID ||
		secondCarryover.SourceNodeRevisionID != firstApproved.Links[0].TargetNodeRevisionID ||
		len(secondCarryover.Candidates) != 1 {
		t.Fatalf("second chained provisional carryover=%+v", secondCarryover)
	}
	secondApproved, err := fixture.learning.DecideEvidenceCarryover(ctx, integrationActorID, learning.EvidenceCarryoverDecisionCommand{
		OperationID: "66000000-0000-4000-8000-000000000006", ProposalID: secondCarryover.ID,
		Decision: "approve", Reason: "approved link is valid source",
	})
	if err != nil || secondApproved.Status != learning.EvidenceCarryoverApproved || len(secondApproved.Links) != 1 {
		t.Fatalf("second chained approval=%+v err=%v", secondApproved, err)
	}
	assertCarryoverRows(t, fixture.pool, secondCarryover.ID, 1, 1)
	if authorityAfter := readCarryoverAuthoritySnapshot(t, fixture.pool, evidence.EvidenceID); authorityAfter != authorityBefore {
		t.Fatalf("chained approval changed evidence/mastery/review before=%+v after=%+v", authorityBefore, authorityAfter)
	}

	learningBeforeThirdApply := readLearningWriteCounts(t, fixture.pool)
	third := createOpenEvidenceMaintenance(t, fixture.knowledge, secondApplied.AppliedRevisionID,
		"66000000-0000-4000-8000-000000000007", "Topic Third")
	thirdApplied := approveKnowledgeMaintenance(t, fixture.knowledge, third,
		"66000000-0000-4000-8000-000000000008", "apply third revision")
	assertLearningWriteCounts(t, fixture.pool, learningBeforeThirdApply)
	thirdCarryover := loadCarryoverForKnowledgeProposal(t, fixture, thirdApplied.ID)
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO learning_evidence_invalidations(id,evidence_id,reason,event_seq,created_at)
		VALUES('66000000-0000-4000-8000-000000000009',$1,'source invalidated',990001,clock_timestamp())`, evidence.EvidenceID); err != nil {
		t.Fatal(err)
	}
	thirdStale, err := fixture.learning.DecideEvidenceCarryover(ctx, integrationActorID, learning.EvidenceCarryoverDecisionCommand{
		OperationID: "66000000-0000-4000-8000-000000000010", ProposalID: thirdCarryover.ID,
		Decision: "approve", Reason: "must fail closed after invalidation",
	})
	if err != nil || thirdStale.Status != learning.EvidenceCarryoverStale || len(thirdStale.Links) != 0 {
		t.Fatalf("invalidated source carryover=%+v err=%v", thirdStale, err)
	}
	assertCarryoverRows(t, fixture.pool, thirdCarryover.ID, 1, 0)
}

func TestPostgreSQLKnowledgeMaintenanceEvidenceCarryoverStaleAndConcurrency(t *testing.T) {
	t.Run("stale head", func(t *testing.T) {
		fixture, _, applied, carryover := setupOpenEvidenceCarryover(t)
		autoApplyMaintenanceAddition(t, fixture.knowledge, applied.AppliedRevisionID,
			"67000000-0000-4000-8000-000000000001", "unrelated.md")
		result, err := fixture.learning.DecideEvidenceCarryover(t.Context(), integrationActorID, learning.EvidenceCarryoverDecisionCommand{
			OperationID: "67000000-0000-4000-8000-000000000002", ProposalID: carryover.ID,
			Decision: "approve", Reason: "head advanced",
		})
		if err != nil || result.Status != learning.EvidenceCarryoverStale || len(result.Links) != 0 {
			t.Fatalf("stale-head carryover=%+v err=%v", result, err)
		}
		assertCarryoverRows(t, fixture.pool, carryover.ID, 1, 0)
	})

	t.Run("stale owner generations", func(t *testing.T) {
		fixture, _, _, carryover := setupOpenEvidenceCarryover(t)
		if _, err := fixture.pool.Exec(t.Context(), `
			UPDATE privacy_owner_generation_gates
			SET learner_generation=learner_generation+1,updated_at=clock_timestamp()
			WHERE owner_kind IN ('knowledge','learning')`); err != nil {
			t.Fatal(err)
		}
		result, err := fixture.learning.DecideEvidenceCarryover(t.Context(), integrationActorID, learning.EvidenceCarryoverDecisionCommand{
			OperationID: "67000000-0000-4000-8000-000000000003", ProposalID: carryover.ID,
			Decision: "approve", Reason: "generation advanced",
		})
		if err != nil || result.Status != learning.EvidenceCarryoverStale || len(result.Links) != 0 {
			t.Fatalf("stale-generation carryover=%+v err=%v", result, err)
		}
		assertCarryoverRows(t, fixture.pool, carryover.ID, 1, 0)
	})

	t.Run("opposite decisions serialize", func(t *testing.T) {
		fixture, _, _, carryover := setupOpenEvidenceCarryover(t)
		commands := []learning.EvidenceCarryoverDecisionCommand{
			{OperationID: "67000000-0000-4000-8000-000000000004", ProposalID: carryover.ID, Decision: "approve", Reason: "approve concurrently"},
			{OperationID: "67000000-0000-4000-8000-000000000005", ProposalID: carryover.ID, Decision: "reject", Reason: "reject concurrently"},
		}
		start := make(chan struct{})
		outcomes := make(chan struct {
			proposal learning.EvidenceCarryoverProposal
			err      error
		}, len(commands))
		var ready sync.WaitGroup
		ready.Add(len(commands))
		for _, command := range commands {
			go func(command learning.EvidenceCarryoverDecisionCommand) {
				ready.Done()
				<-start
				proposal, err := fixture.learning.DecideEvidenceCarryover(context.Background(), integrationActorID, command)
				outcomes <- struct {
					proposal learning.EvidenceCarryoverProposal
					err      error
				}{proposal: proposal, err: err}
			}(command)
		}
		ready.Wait()
		close(start)
		var terminal learning.EvidenceCarryoverProposal
		conflicts := 0
		for range commands {
			outcome := <-outcomes
			if outcome.err == nil {
				terminal = outcome.proposal
				continue
			}
			if learning.ErrorCode(outcome.err) != learning.CodeOperationConflict {
				t.Fatalf("concurrent carryover error=%v code=%q", outcome.err, learning.ErrorCode(outcome.err))
			}
			conflicts++
		}
		if terminal.ID == "" || conflicts != 1 ||
			(terminal.Status != learning.EvidenceCarryoverApproved && terminal.Status != learning.EvidenceCarryoverRejected) {
			t.Fatalf("concurrent terminal=%+v conflicts=%d", terminal, conflicts)
		}
		wantLinks := int64(0)
		if terminal.Status == learning.EvidenceCarryoverApproved {
			wantLinks = 1
		}
		assertCarryoverRows(t, fixture.pool, carryover.ID, 1, wantLinks)
	})

	t.Run("zero candidates require rejection", func(t *testing.T) {
		fixture := newKnowledgeMaintenanceCarryoverFixture(t)
		base, err := fixture.knowledge.Import(t.Context(), knowledge.ImportCommand{
			OperationID: "67000000-0000-4000-8000-000000000006", ExpectedParentProvided: true,
			Source: "knowledge-carryover-delete-postgres", ActorDeviceID: integrationActorID,
			Documents: []knowledge.ImportDocument{
				{Path: "topic.md", Markdown: "# Topic\none two three four five six seven eight nine ten\n"},
				{Path: "z-keep.md", Markdown: "# Keep\nunrelated retained document\n"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		seedAcceptedEvidenceForMaintenance(t, fixture.pool, base.Revision)
		exported, err := fixture.knowledge.Export(t.Context(), base.Revision.ID)
		if err != nil {
			t.Fatal(err)
		}
		candidate := make([]knowledge.ImportDocument, 0, 1)
		for _, document := range maintenanceImportDocuments(exported.Documents) {
			if document.Path == "z-keep.md" {
				candidate = append(candidate, document)
			}
		}
		if len(candidate) != 1 {
			t.Fatalf("delete fixture retained documents=%+v", candidate)
		}
		proposal, err := fixture.knowledge.Create(t.Context(), knowledge.CreateProposalCommand{
			RequestID: "67000000-0000-4000-8000-000000000007", BaseRevisionID: base.Revision.ID,
			ActorDeviceID:     integrationActorID,
			Sources:           []knowledge.ProposalSource{maintenancePostgresSource("note", "agent/delete", "delete source mapping")},
			CandidateSnapshot: candidate,
		})
		if err != nil || proposal.Status != knowledge.ProposalOpen || proposal.EvidenceImpact.Count != 1 {
			t.Fatalf("delete proposal=%+v err=%v", proposal, err)
		}
		applied := approveKnowledgeMaintenance(t, fixture.knowledge, proposal,
			"67000000-0000-4000-8000-000000000008", "approve deletion")
		carryover := loadCarryoverForKnowledgeProposal(t, fixture, applied.ID)
		if len(carryover.Candidates) != 0 || carryover.Status != learning.EvidenceCarryoverOpen {
			t.Fatalf("zero-candidate carryover=%+v", carryover)
		}
		approve := learning.EvidenceCarryoverDecisionCommand{
			OperationID: "67000000-0000-4000-8000-000000000009", ProposalID: carryover.ID,
			Decision: "approve", Reason: "cannot approve deletion mapping",
		}
		if _, err := fixture.learning.DecideEvidenceCarryover(t.Context(), integrationActorID, approve); learning.ErrorCode(err) != learning.CodeEvidenceCarryoverNoCandidates {
			t.Fatalf("zero-candidate approve error=%v code=%q", err, learning.ErrorCode(err))
		}
		open, err := fixture.learning.GetEvidenceCarryover(t.Context(), carryover.ID)
		if err != nil || open.Status != learning.EvidenceCarryoverOpen || open.Decision != nil {
			t.Fatalf("zero-candidate approve closed proposal=%+v err=%v", open, err)
		}
		assertCarryoverRows(t, fixture.pool, carryover.ID, 0, 0)
		rejected, err := fixture.learning.DecideEvidenceCarryover(t.Context(), integrationActorID, learning.EvidenceCarryoverDecisionCommand{
			OperationID: "67000000-0000-4000-8000-000000000010", ProposalID: carryover.ID,
			Decision: "reject", Reason: "no valid mapping",
		})
		if err != nil || rejected.Status != learning.EvidenceCarryoverRejected || len(rejected.Links) != 0 {
			t.Fatalf("zero-candidate rejection=%+v err=%v", rejected, err)
		}
		assertCarryoverRows(t, fixture.pool, carryover.ID, 1, 0)
	})
}

type knowledgeMaintenanceCarryoverFixture struct {
	pool          *pgxpool.Pool
	knowledge     *knowledge.Service
	learning      *learning.Service
	learningStore *learningpostgres.Store
}

func newKnowledgeMaintenanceCarryoverFixture(t *testing.T) knowledgeMaintenanceCarryoverFixture {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL evidence carryover test not run")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("knowledge_carryover_test_%d", time.Now().UnixNano())
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
	if _, err := pool.Exec(ctx, `
		INSERT INTO devices(id,display_name,created_at)
		VALUES($1,'knowledge carryover actor',clock_timestamp())`, integrationActorID); err != nil {
		t.Fatal(err)
	}
	knowledgeStore := postgresstore.New(pool)
	tutoringStore := tutoringpostgres.New(pool)
	learningStore := learningpostgres.New(pool, tutoringStore, knowledgeStore)
	knowledgeService, err := knowledge.NewService(knowledgeStore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{
		MaintenanceStore: knowledgeStore, EvidenceImpactReader: learningStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	learningService, err := learning.NewService(learningStore, learningStore, maintenanceLearningResolver{}, learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return knowledgeMaintenanceCarryoverFixture{
		pool: pool, knowledge: knowledgeService, learning: learningService, learningStore: learningStore,
	}
}

type maintenanceLearningResolver struct{}

func (maintenanceLearningResolver) Resolve(_ context.Context, knowledgeRevisionID, nodeRevisionID string) (learning.KnowledgeReference, error) {
	return learning.KnowledgeReference{
		KnowledgeRevisionID: knowledgeRevisionID, NodeRevisionID: nodeRevisionID,
		NodeID: "maintenance-carryover-node", DocumentRevisionID: "maintenance-carryover-document",
		Range: learning.SourceRange{Start: 0, End: 1}, Slice: "x", SliceSHA256: learning.SHA256([]byte("x")),
	}, nil
}

func importKnowledgeMaintenanceCarryoverBase(t *testing.T, service *knowledge.Service) knowledge.ImportResult {
	t.Helper()
	result, err := service.Import(t.Context(), knowledge.ImportCommand{
		OperationID: "68000000-0000-4000-8000-000000000001", ExpectedParentProvided: true,
		Source: "knowledge-carryover-postgres", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "topic.md", Markdown: "# Topic\none two three four five six seven eight nine ten\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func createOpenEvidenceMaintenance(t *testing.T, service *knowledge.Service, baseRevisionID, requestID, title string) knowledge.Proposal {
	t.Helper()
	exported, err := service.Export(t.Context(), baseRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	candidate := maintenanceImportDocuments(exported.Documents)
	found := false
	for index := range candidate {
		if candidate[index].Path != "topic.md" {
			continue
		}
		markdown := candidate[index].Markdown
		headingStart := 0
		if !strings.HasPrefix(markdown, "# ") {
			headingStart = strings.Index(markdown, "\n# ")
			if headingStart < 0 {
				t.Fatalf("topic markdown has no level-one heading: %q", markdown)
			}
			headingStart++
		}
		headingEnd := strings.IndexByte(markdown[headingStart:], '\n')
		if headingEnd < 0 {
			headingEnd = len(markdown)
		} else {
			headingEnd += headingStart
		}
		candidate[index].Markdown = markdown[:headingStart] + "# " + title + markdown[headingEnd:]
		found = true
	}
	if !found {
		t.Fatal("topic.md missing from maintenance export")
	}
	proposal, err := service.Create(t.Context(), knowledge.CreateProposalCommand{
		RequestID: requestID, BaseRevisionID: baseRevisionID, ActorDeviceID: integrationActorID,
		Sources:           []knowledge.ProposalSource{maintenancePostgresSource("note", "agent/"+title, "source "+title)},
		CandidateSnapshot: candidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Status != knowledge.ProposalOpen || proposal.EvidenceImpact.Count != 1 || proposal.Risk.AutoApply {
		t.Fatalf("evidence maintenance proposal=%+v", proposal)
	}
	return proposal
}

func approveKnowledgeMaintenance(t *testing.T, service *knowledge.Service, proposal knowledge.Proposal, operationID, reason string) knowledge.Proposal {
	t.Helper()
	result, err := service.Decide(t.Context(), knowledge.ProposalDecisionCommand{
		OperationID: operationID, ProposalID: proposal.ID, Decision: "approve",
		Reason: reason, ActorDeviceID: integrationActorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != knowledge.ProposalApplied || result.AppliedRevisionID == "" {
		t.Fatalf("knowledge maintenance approval=%+v", result)
	}
	return result
}

func autoApplyMaintenanceAddition(t *testing.T, service *knowledge.Service, baseRevisionID, requestID, path string) knowledge.Proposal {
	t.Helper()
	exported, err := service.Export(t.Context(), baseRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	candidate := maintenanceImportDocuments(exported.Documents)
	candidate = append(candidate, knowledge.ImportDocument{Path: path, Markdown: "# Unrelated\nno evidence binding\n"})
	proposal, err := service.Create(t.Context(), knowledge.CreateProposalCommand{
		RequestID: requestID, BaseRevisionID: baseRevisionID, ActorDeviceID: integrationActorID,
		Sources:           []knowledge.ProposalSource{maintenancePostgresSource("note", "agent/"+path, "add "+path)},
		CandidateSnapshot: candidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Status != knowledge.ProposalApplied || proposal.EvidenceImpact.Count != 0 || !proposal.Risk.AutoApply {
		t.Fatalf("unrelated auto addition=%+v", proposal)
	}
	return proposal
}

func setupOpenEvidenceCarryover(t *testing.T) (knowledgeMaintenanceCarryoverFixture, maintenanceEvidenceFixture, knowledge.Proposal, learning.EvidenceCarryoverProposal) {
	t.Helper()
	fixture := newKnowledgeMaintenanceCarryoverFixture(t)
	base := importKnowledgeMaintenanceCarryoverBase(t, fixture.knowledge)
	evidence := seedAcceptedEvidenceForMaintenance(t, fixture.pool, base.Revision)
	proposal := createOpenEvidenceMaintenance(t, fixture.knowledge, base.Revision.ID,
		"69000000-0000-4000-8000-000000000001", "Carryover Source")
	applied := approveKnowledgeMaintenance(t, fixture.knowledge, proposal,
		"69000000-0000-4000-8000-000000000002", "create open carryover")
	carryover := loadCarryoverForKnowledgeProposal(t, fixture, applied.ID)
	if carryover.Status != learning.EvidenceCarryoverOpen || len(carryover.Candidates) != 1 {
		t.Fatalf("open carryover fixture=%+v", carryover)
	}
	return fixture, evidence, applied, carryover
}

func loadCarryoverForKnowledgeProposal(t *testing.T, fixture knowledgeMaintenanceCarryoverFixture, knowledgeProposalID string) learning.EvidenceCarryoverProposal {
	t.Helper()
	var proposalID string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT proposal_id::text
		FROM learning_evidence_carryover_proposals
		WHERE knowledge_proposal_id=$1`, knowledgeProposalID).Scan(&proposalID); err != nil {
		t.Fatal(err)
	}
	proposal, err := fixture.learning.GetEvidenceCarryover(t.Context(), proposalID)
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}

func assertSortedUniqueEvidenceImpact(t *testing.T, impact knowledge.AcceptedEvidenceImpact) {
	t.Helper()
	previous := ""
	for _, reference := range impact.References {
		current := reference.EvidenceID + "|" + reference.NodeRevisionID + "|" + reference.KnowledgeRevisionID
		if previous != "" && current <= previous {
			t.Fatalf("accepted evidence impact is not strictly ordered and unique: %+v", impact.References)
		}
		previous = current
	}
}

type carryoverAuthoritySnapshot struct {
	EvidenceRows       int64
	Invalidations      int64
	ProjectionEvidence int64
	ProjectionNodes    int64
	ProjectionReviews  int64
	KnowledgeRevision  string
	NodeRevision       string
	AcceptedEventSeq   int64
}

func readCarryoverAuthoritySnapshot(t *testing.T, pool *pgxpool.Pool, evidenceID string) carryoverAuthoritySnapshot {
	t.Helper()
	var value carryoverAuthoritySnapshot
	if err := pool.QueryRow(t.Context(), `
		SELECT
		  (SELECT count(*) FROM learning_evidence),
		  (SELECT count(*) FROM learning_evidence_invalidations),
		  (SELECT count(*) FROM learning_projection_evidence),
		  (SELECT count(*) FROM learning_projection_nodes),
		  (SELECT count(*) FROM learning_projection_reviews),
		  evidence.knowledge_revision_id::text,evidence.node_revision_id::text,evidence.accepted_event_seq
		FROM learning_evidence evidence WHERE evidence.id=$1`, evidenceID).Scan(
		&value.EvidenceRows, &value.Invalidations, &value.ProjectionEvidence, &value.ProjectionNodes,
		&value.ProjectionReviews, &value.KnowledgeRevision, &value.NodeRevision, &value.AcceptedEventSeq,
	); err != nil {
		t.Fatal(err)
	}
	return value
}

func assertCarryoverRows(t *testing.T, pool *pgxpool.Pool, proposalID string, wantTerminal, wantLinks int64) {
	t.Helper()
	var events, heads, decisions, operations, links, projected int64
	if err := pool.QueryRow(t.Context(), `
		SELECT
		  (SELECT count(*) FROM learning_events WHERE aggregate_type='evidence_carryover' AND aggregate_id=$1),
		  (SELECT count(*) FROM learning_aggregate_heads WHERE aggregate_type='evidence_carryover' AND aggregate_id=$1),
		  (SELECT count(*) FROM learning_evidence_carryover_decisions WHERE proposal_id=$1),
		  (SELECT count(*) FROM learning_evidence_carryover_operations WHERE proposal_id=$1),
		  (SELECT count(*) FROM learning_evidence_carryover_links WHERE proposal_id=$1),
		  (SELECT count(*) FROM learning_projection_carryovers WHERE proposal_id=$1)`, proposalID).Scan(
		&events, &heads, &decisions, &operations, &links, &projected,
	); err != nil {
		t.Fatal(err)
	}
	wantProjected := int64(0)
	if wantLinks > 0 {
		wantProjected = 1
	}
	if events != wantTerminal || heads != wantTerminal || decisions != wantTerminal || operations != wantTerminal ||
		links != wantLinks || projected != wantProjected {
		t.Fatalf("carryover rows proposal=%s events=%d heads=%d decisions=%d operations=%d links=%d projected=%d",
			proposalID, events, heads, decisions, operations, links, projected)
	}
}

type maintenanceAtomicCounts struct {
	Proposals           int64
	Decisions           int64
	Operations          int64
	Revisions           int64
	Origins             int64
	Outbox              int64
	CarryoverProposals  int64
	CarryoverCandidates int64
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
		  (SELECT count(*) FROM outbox_messages),
		  (SELECT count(*) FROM learning_evidence_carryover_proposals),
		  (SELECT count(*) FROM learning_evidence_carryover_candidates)`).Scan(
		&value.Proposals, &value.Decisions, &value.Operations, &value.Revisions, &value.Origins, &value.Outbox,
		&value.CarryoverProposals, &value.CarryoverCandidates,
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
	ProjectionReviews  int64
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
		  (SELECT count(*) FROM learning_projection_timeline),
		  (SELECT count(*) FROM learning_projection_reviews)`).Scan(
		&value.Events, &value.Evidence, &value.ProjectionEvidence, &value.ProjectionNodes,
		&value.ProjectionTimeline, &value.ProjectionReviews,
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

type maintenanceEvidenceFixture struct {
	EvidenceID          string
	KnowledgeRevisionID string
	NodeRevisionID      string
	NodeID              string
	DocumentRevisionID  string
}

func seedAcceptedEvidenceForMaintenance(t *testing.T, pool *pgxpool.Pool, revision knowledge.KnowledgeRevision) maintenanceEvidenceFixture {
	t.Helper()
	ctx := context.Background()
	var document knowledge.DocumentRevision
	for _, snapshot := range revision.Documents {
		if snapshot.Path == "topic.md" {
			document = snapshot.Revision
			break
		}
	}
	if document.ID == "" || len(document.Nodes) < 2 {
		t.Fatalf("topic.md evidence fixture is missing a target node: %+v", revision.Documents)
	}
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
	return maintenanceEvidenceFixture{
		EvidenceID: evidenceID, KnowledgeRevisionID: revision.ID, NodeRevisionID: node.ID,
		NodeID: node.NodeID, DocumentRevisionID: document.ID,
	}
}
