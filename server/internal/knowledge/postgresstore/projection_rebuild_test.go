package postgresstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	knowledgeRebuildBaseRevisionID   = "81000000-0000-4000-8000-000000000001"
	knowledgeRebuildNodeID           = "81000000-0000-4000-8000-000000000002"
	knowledgeRebuildFinalRevisionID  = "81000000-0000-4000-8000-000000000003"
	knowledgeRebuildLineageID        = "81000000-0000-4000-8000-000000000004"
	knowledgeRebuildProposalID       = "81000000-0000-4000-8000-000000000005"
	knowledgeRebuildDecisionID       = "81000000-0000-4000-8000-000000000006"
	knowledgeRebuildOldDetailsRevID  = "0dc537a7-58e5-5e14-90f1-9406fccfc5c8"
	knowledgeRebuildSourceSHA256     = "5a907e7e792509c59ebf105f3867e47dfbbdc45e02ad1a862d26466a2477defe"
	knowledgeRebuildGuideDocumentID  = "82000000-0000-4000-8000-000000000001"
	knowledgeRebuildGuideRootID      = "82000000-0000-4000-8000-000000000002"
	knowledgeRebuildIntroNodeID      = "82000000-0000-4000-8000-000000000003"
	knowledgeRebuildDetailsNodeID    = "82000000-0000-4000-8000-000000000004"
	knowledgeRebuildStableDocumentID = "83000000-0000-4000-8000-000000000001"
	knowledgeRebuildStableRootID     = "83000000-0000-4000-8000-000000000002"
	knowledgeRebuildStableNodeID     = "83000000-0000-4000-8000-000000000003"
	knowledgeRebuildDeleteDocumentID = "84000000-0000-4000-8000-000000000001"
	knowledgeRebuildDeleteRootID     = "84000000-0000-4000-8000-000000000002"
	knowledgeRebuildDeleteNodeID     = "84000000-0000-4000-8000-000000000003"
	knowledgeRebuildAddedDocumentID  = "85000000-0000-4000-8000-000000000001"
	knowledgeRebuildAddedRootID      = "85000000-0000-4000-8000-000000000002"
	knowledgeRebuildAddedNodeID      = "85000000-0000-4000-8000-000000000003"
)

var knowledgeRebuildNow = time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

type knowledgeRebuildEvidenceReader struct{}

func (knowledgeRebuildEvidenceReader) AcceptedEvidenceImpact(context.Context, []string) (knowledge.AcceptedEvidenceImpact, error) {
	return knowledge.AcceptedEvidenceImpact{Generation: 1, References: []knowledge.AcceptedEvidenceReference{}}, nil
}

type knowledgeUUIDSequence struct {
	values []string
	next   int
}

func (s *knowledgeUUIDSequence) New() string {
	if s.next >= len(s.values) {
		panic("knowledge rebuild fixture exhausted deterministic UUIDs")
	}
	value := s.values[s.next]
	s.next++
	return value
}

type canonicalKnowledgeNode struct {
	Revision     knowledge.NodeRevision `json:"revision"`
	HeadingSlice string                 `json:"heading_slice"`
	LocalSlice   string                 `json:"local_body_slice"`
	SectionSlice string                 `json:"section_slice"`
}

type canonicalKnowledgeDocument struct {
	Path              string                   `json:"path"`
	ID                string                   `json:"document_revision_id"`
	DocumentID        string                   `json:"document_id"`
	RootNodeID        string                   `json:"root_node_id"`
	CanonicalHash     string                   `json:"canonical_hash"`
	SemanticHash      string                   `json:"semantic_hash"`
	CanonicalMarkdown string                   `json:"canonical_markdown"`
	Nodes             []canonicalKnowledgeNode `json:"nodes"`
}

type canonicalKnowledgeSnapshot struct {
	ID                    string                       `json:"revision_id"`
	RevisionNo            int64                        `json:"revision_no"`
	ParentRevisionID      *string                      `json:"parent_revision_id"`
	ManifestHash          string                       `json:"manifest_hash"`
	Source                string                       `json:"source"`
	CreatedByDeviceID     string                       `json:"created_by_device_id"`
	CreatedAt             time.Time                    `json:"created_at"`
	CanonicalizerVersion  string                       `json:"canonicalizer_version"`
	ParserVersion         string                       `json:"parser_version"`
	IndexerVersion        string                       `json:"indexer_version"`
	IdentityPolicyVersion string                       `json:"identity_policy_version"`
	Documents             []canonicalKnowledgeDocument `json:"documents"`
	Lineages              []knowledge.Lineage          `json:"lineages"`
	Origin                *knowledge.RevisionOrigin    `json:"origin"`
}

type knowledgeCorpusResult struct {
	Snapshot       canonicalKnowledgeSnapshot
	BaseSnapshot   canonicalKnowledgeSnapshot
	Generation     int64
	FinalRevision  string
	DeletedDocID   string
	UnchangedDocID string
}

type knowledgeDocumentOrder string

const (
	knowledgeOrderForward knowledgeDocumentOrder = "forward"
	knowledgeOrderReverse knowledgeDocumentOrder = "reverse"
	knowledgeOrderRotated knowledgeDocumentOrder = "rotated"
)

func TestPostgreSQLKnowledgeFullTreeRebuildParityAndFailClosedCorpus(t *testing.T) {
	golden := knowledgeRebuildGoldenSnapshot(t)
	orders := []knowledgeDocumentOrder{knowledgeOrderForward, knowledgeOrderReverse, knowledgeOrderRotated}
	var baseline *knowledgeCorpusResult
	for _, order := range orders {
		t.Run(string(order), func(t *testing.T) {
			result := runKnowledgeRebuildCorpus(t, order)
			assertKnowledgeSnapshotEqual(t, result.Snapshot, golden, string(order)+" fresh rebuild", "static full-tree golden")
			if result.Generation != 1 {
				t.Fatalf("knowledge generation=%d want=1", result.Generation)
			}
			if hasKnowledgeDocument(result.Snapshot, result.DeletedDocID) {
				t.Fatalf("deleted document %s survived final snapshot", result.DeletedDocID)
			}
			if !hasKnowledgeDocument(result.BaseSnapshot, result.DeletedDocID) {
				t.Fatalf("deleted document %s disappeared from immutable base revision", result.DeletedDocID)
			}
			if !hasKnowledgeDocument(result.Snapshot, result.UnchangedDocID) {
				t.Fatalf("unchanged document %s missing from final snapshot", result.UnchangedDocID)
			}
			if baseline == nil {
				baseline = &result
				return
			}
			assertKnowledgeSnapshotEqual(t, result.Snapshot, baseline.Snapshot, string(order)+" fresh rebuild", "forward fresh rebuild")
		})
	}
}

func runKnowledgeRebuildCorpus(t *testing.T, order knowledgeDocumentOrder) knowledgeCorpusResult {
	t.Helper()
	pool := knowledgeRebuildPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'knowledge-rebuild-oracle',$2)`, integrationActorID, knowledgeRebuildNow); err != nil {
		t.Fatal(err)
	}
	ids := &knowledgeUUIDSequence{values: []string{
		knowledgeRebuildBaseRevisionID,
		knowledgeRebuildNodeID,
		knowledgeRebuildFinalRevisionID,
		knowledgeRebuildLineageID,
		knowledgeRebuildProposalID,
		knowledgeRebuildDecisionID,
	}}
	store := postgresstore.New(pool)
	service, err := knowledge.NewService(store, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{
		MaintenanceStore: store, EvidenceImpactReader: knowledgeRebuildEvidenceReader{},
		NewUUID: ids.New, Now: func() time.Time { return knowledgeRebuildNow },
	})
	if err != nil {
		t.Fatal(err)
	}

	baseDocuments := []knowledge.ImportDocument{
		{Path: "guide.md", Markdown: knowledgeRebuildBaseGuide()},
		{Path: "stable.md", Markdown: knowledgeRebuildStable()},
		{Path: "obsolete.md", Markdown: knowledgeRebuildDeleted()},
	}
	orderKnowledgeDocuments(baseDocuments, order)
	baseCommand := knowledge.ImportCommand{
		OperationID: "86000000-0000-4000-8000-000000000001", ExpectedParentProvided: true,
		Source: "knowledge-rebuild-base", ActorDeviceID: integrationActorID, Documents: baseDocuments,
	}
	base, err := service.Import(ctx, baseCommand)
	if err != nil {
		t.Fatalf("base import: %v", err)
	}
	for replayIndex := 0; replayIndex < 2; replayIndex++ {
		replay, replayErr := service.Import(ctx, baseCommand)
		if replayErr != nil || !replay.Replayed || replay.Revision.ID != base.Revision.ID {
			t.Fatalf("base replay %d result=%+v err=%v", replayIndex+1, replay, replayErr)
		}
	}
	baseSnapshot := canonicalKnowledgeSnapshotFromRevision(t, base.Revision)

	candidate := []knowledge.ImportDocument{
		{Path: "moved/guide.md", Markdown: knowledgeRebuildCandidateGuide(knowledgeRebuildDetailsNodeID)},
		{Path: "stable.md", Markdown: knowledgeRebuildStable()},
		{Path: "added.md", Markdown: knowledgeRebuildAdded()},
	}
	orderKnowledgeDocuments(candidate, order)
	previewOperationID := "86000000-0000-4000-8000-000000000002"
	parent := base.Revision.ID
	_, previewErr := service.Import(ctx, knowledge.ImportCommand{
		OperationID: previewOperationID, ExpectedParentRevisionID: &parent, ExpectedParentProvided: true,
		Source: "knowledge_maintenance", ActorDeviceID: integrationActorID, Documents: candidate,
	})
	var preview *knowledge.IdentityReview
	if domainErr, ok := previewErr.(*knowledge.Error); ok && domainErr.Code == knowledge.CodeIdentityReviewRequired {
		preview = domainErr.Review
	}
	if preview == nil || len(preview.Nodes) != 1 {
		t.Fatalf("expected one explicit rewrite review, got err=%v review=%+v", previewErr, preview)
	}
	createCommand := knowledge.CreateProposalCommand{
		RequestID: "86000000-0000-4000-8000-000000000003", BaseRevisionID: base.Revision.ID,
		ActorDeviceID: integrationActorID, CandidateSnapshot: candidate,
		Sources:                 []knowledge.ProposalSource{knowledgeRebuildSource("operations-hardening/full-tree")},
		IdentityReviewBasisHash: preview.BasisHash, IdentityReviewOperationID: preview.OperationID,
		IdentityReviewReceipt: preview.Receipt,
		NodeResolutions: []knowledge.NodeResolution{{
			Locator: preview.Nodes[0].Locator, Action: "rewrite",
			SourceNodeRevisionIDs: []string{knowledgeRebuildOldDetailsRevID}, Reason: "replace details while preserving explicit lineage",
		}},
	}
	proposal, err := service.Create(ctx, createCommand)
	if err != nil {
		t.Fatalf("create reviewed proposal: %v", err)
	}
	if proposal.ID != knowledgeRebuildProposalID || proposal.Status != knowledge.ProposalOpen || proposal.Risk.AutoApply {
		t.Fatalf("unexpected rebuild proposal: %+v", proposal)
	}
	for replayIndex := 0; replayIndex < 2; replayIndex++ {
		replay, replayErr := service.Create(ctx, createCommand)
		if replayErr != nil || !replay.Replayed || replay.ID != proposal.ID {
			t.Fatalf("proposal replay %d result=%+v err=%v", replayIndex+1, replay, replayErr)
		}
	}
	applied, err := service.Decide(ctx, knowledge.ProposalDecisionCommand{
		OperationID: "86000000-0000-4000-8000-000000000004", ProposalID: proposal.ID,
		Decision: "approve", Reason: "approve deterministic rebuild corpus", ActorDeviceID: integrationActorID,
	})
	if err != nil {
		t.Fatalf("approve reviewed proposal: %v", err)
	}
	if applied.Status != knowledge.ProposalApplied || applied.AppliedRevisionID != knowledgeRebuildFinalRevisionID {
		t.Fatalf("proposal did not apply expected revision: %+v", applied)
	}
	for replayIndex := 0; replayIndex < 2; replayIndex++ {
		replay, replayErr := service.Decide(ctx, knowledge.ProposalDecisionCommand{
			OperationID: "86000000-0000-4000-8000-000000000004", ProposalID: proposal.ID,
			Decision: "approve", Reason: "approve deterministic rebuild corpus", ActorDeviceID: integrationActorID,
		})
		if replayErr != nil || !replay.Replayed || replay.ID != proposal.ID || replay.AppliedRevisionID != applied.AppliedRevisionID {
			t.Fatalf("decision replay %d result=%+v err=%v", replayIndex+1, replay, replayErr)
		}
	}

	finalTree, err := service.Tree(ctx, applied.AppliedRevisionID)
	if err != nil {
		t.Fatalf("load final tree: %v", err)
	}
	actual := canonicalKnowledgeSnapshotFromRevision(t, finalTree.Revision)

	unchangedParent := finalTree.Revision.ID
	unchangedCommand := knowledge.ImportCommand{
		OperationID: "86000000-0000-4000-8000-000000000005", ExpectedParentRevisionID: &unchangedParent,
		ExpectedParentProvided: true, Source: "knowledge-rebuild-unchanged", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "stable.md", Markdown: knowledgeRebuildStable()}},
	}
	unchanged, err := service.Import(ctx, unchangedCommand)
	if err != nil || !unchanged.Unchanged || unchanged.Revision.ID != finalTree.Revision.ID {
		t.Fatalf("unchanged import result=%+v err=%v", unchanged, err)
	}
	for replayIndex := 0; replayIndex < 2; replayIndex++ {
		replay, replayErr := service.Import(ctx, unchangedCommand)
		if replayErr != nil || !replay.Replayed || !replay.Unchanged || replay.Revision.ID != finalTree.Revision.ID {
			t.Fatalf("unchanged replay %d result=%+v err=%v", replayIndex+1, replay, replayErr)
		}
	}

	var generationBefore int64
	if err := pool.QueryRow(ctx, `SELECT learner_generation FROM privacy_owner_generation_gates WHERE owner_kind='knowledge'`).Scan(&generationBefore); err != nil {
		t.Fatal(err)
	}
	staleParent := base.Revision.ID
	if _, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "86000000-0000-4000-8000-000000000006", ExpectedParentRevisionID: &staleParent,
		ExpectedParentProvided: true, Source: "knowledge-rebuild-out-of-order", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "late.md", Markdown: "# late\nlate body\n"}},
	}); knowledge.ErrorCode(err) != knowledge.CodeRevisionConflict {
		t.Fatalf("out-of-order parent error=%v code=%q", err, knowledge.ErrorCode(err))
	}
	if _, err := service.Import(ctx, knowledge.ImportCommand{
		OperationID: "86000000-0000-4000-8000-000000000007", ExpectedParentRevisionID: &unchangedParent,
		ExpectedParentProvided: true, Source: "knowledge-rebuild-bad-event", ActorDeviceID: integrationActorID,
		Documents: []knowledge.ImportDocument{{Path: "broken.md", Markdown: "<!-- edu-agent-node:v1 not-json -->\n# Broken\nbody\n"}},
	}); err == nil {
		t.Fatal("bad knowledge event unexpectedly advanced the catalog")
	}
	var headAfter string
	var generationAfter int64
	if err := pool.QueryRow(ctx, `SELECT head_revision_id::text FROM knowledge_catalog WHERE singleton_id=1`).Scan(&headAfter); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT learner_generation FROM privacy_owner_generation_gates WHERE owner_kind='knowledge'`).Scan(&generationAfter); err != nil {
		t.Fatal(err)
	}
	if headAfter != finalTree.Revision.ID || generationAfter != generationBefore {
		t.Fatalf("failed knowledge events changed active state head=%s generation=%d; want head=%s generation=%d", headAfter, generationAfter, finalTree.Revision.ID, generationBefore)
	}
	stillReadable, err := service.Tree(ctx, finalTree.Revision.ID)
	if err != nil {
		t.Fatalf("old knowledge read model unavailable after failures: %v", err)
	}
	assertKnowledgeSnapshotEqual(t, canonicalKnowledgeSnapshotFromRevision(t, stillReadable.Revision), actual, "post-failure active tree", "pre-failure active tree")
	if ids.next != len(ids.values) {
		t.Fatalf("deterministic UUID use=%d want=%d", ids.next, len(ids.values))
	}
	return knowledgeCorpusResult{
		Snapshot: actual, BaseSnapshot: baseSnapshot, Generation: generationAfter,
		FinalRevision: finalTree.Revision.ID, DeletedDocID: knowledgeRebuildDeleteDocumentID,
		UnchangedDocID: knowledgeRebuildStableDocumentID,
	}
}

const knowledgeRebuildGoldenJSON = `{
  "revision_id": "81000000-0000-4000-8000-000000000003",
  "revision_no": 2,
  "parent_revision_id": "81000000-0000-4000-8000-000000000001",
  "manifest_hash": "af397d9f568a14093f3dcc39ef86af7c2d680027a626c2778c6255fa517fde6e",
  "source": "knowledge_maintenance",
  "created_by_device_id": "90000000-0000-4000-8000-000000000001",
  "created_at": "2026-08-28T09:00:00Z",
  "canonicalizer_version": "edu-markdown-v1",
  "parser_version": "goldmark-v1.8.5-commonmark-0.31.2-gfm",
  "indexer_version": "knowledge-indexer-v1",
  "identity_policy_version": "identity-policy-v1",
  "documents": [
    {
      "path": "added.md",
      "document_revision_id": "a77c2474-de8f-583c-a9fa-aa6d5355f385",
      "document_id": "85000000-0000-4000-8000-000000000001",
      "root_node_id": "85000000-0000-4000-8000-000000000002",
      "canonical_hash": "04830fbebc9caf381a1f7b928209236d1f46c561edf1a8667a43e43cedb9d56a",
      "semantic_hash": "3ad98cf6e889afbf8ea501d21c1cdb709109888b228321f27f33ac82adc01ca6",
      "canonical_markdown": "---\nedu-agent-format: 1\nedu-agent-document-id: 85000000-0000-4000-8000-000000000001\nedu-agent-root-node-id: 85000000-0000-4000-8000-000000000002\n---\n<!-- edu-agent-node:v1 {\"id\":\"85000000-0000-4000-8000-000000000003\"} -->\n# Added\nnew canonical material joins the tree\n",
      "nodes": [
        {
          "revision": {
            "node_revision_id": "e9b3cfc4-2bff-57e2-96b3-751369ab88c5",
            "node_id": "85000000-0000-4000-8000-000000000002",
            "document_revision_id": "a77c2474-de8f-583c-a9fa-aa6d5355f385",
            "parent_node_revision_id": null,
            "sibling_index": 0,
            "heading_level": 0,
            "title": "",
            "ancestor_titles": [],
            "heading_range": {"start": 149, "end": 149, "start_line": 6, "end_line": 6},
            "local_body_range": {"start": 149, "end": 149, "start_line": 6, "end_line": 6},
            "section_range": {"start": 149, "end": 268, "start_line": 6, "end_line": 8},
            "semantic_local_body_hash": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
            "children": ["d8ea5818-3a80-5a59-9428-b655d87f1970"]
          },
          "heading_slice": "",
          "local_body_slice": "",
          "section_slice": "<!-- edu-agent-node:v1 {\"id\":\"85000000-0000-4000-8000-000000000003\"} -->\n# Added\nnew canonical material joins the tree\n"
        },
        {
          "revision": {
            "node_revision_id": "d8ea5818-3a80-5a59-9428-b655d87f1970",
            "node_id": "85000000-0000-4000-8000-000000000003",
            "document_revision_id": "a77c2474-de8f-583c-a9fa-aa6d5355f385",
            "parent_node_revision_id": "e9b3cfc4-2bff-57e2-96b3-751369ab88c5",
            "sibling_index": 0,
            "heading_level": 1,
            "title": "Added",
            "ancestor_titles": [],
            "heading_range": {"start": 222, "end": 230, "start_line": 7, "end_line": 7},
            "local_body_range": {"start": 230, "end": 268, "start_line": 8, "end_line": 8},
            "section_range": {"start": 149, "end": 268, "start_line": 6, "end_line": 8},
            "semantic_local_body_hash": "e1aaa88e7ddae7eb28a303afeca6ddf5e57d3aa88b36d684117bb9a13c5b2561"
          },
          "heading_slice": "# Added\n",
          "local_body_slice": "new canonical material joins the tree\n",
          "section_slice": "<!-- edu-agent-node:v1 {\"id\":\"85000000-0000-4000-8000-000000000003\"} -->\n# Added\nnew canonical material joins the tree\n"
        }
      ]
    },
    {
      "path": "moved/guide.md",
      "document_revision_id": "0eb00cc5-49d3-5ca9-9c0b-757d5096bc7f",
      "document_id": "82000000-0000-4000-8000-000000000001",
      "root_node_id": "82000000-0000-4000-8000-000000000002",
      "canonical_hash": "fe26d0a6ca6f07181de12e402ce92a5d38901e7d0ac680b1dfeff1ddb6476da1",
      "semantic_hash": "7a2828ec1c6c6f03b2260999bcd497c93db03ade9ed4a3d005e8b31de859fd66",
      "canonical_markdown": "---\nedu-agent-format: 1\nedu-agent-document-id: 82000000-0000-4000-8000-000000000001\nedu-agent-root-node-id: 82000000-0000-4000-8000-000000000002\n---\n<!-- edu-agent-node:v1 {\"id\":\"81000000-0000-4000-8000-000000000002\"} -->\n# Details\nmercury venus earth mars jupiter saturn uranus neptune pluto ceres\n<!-- edu-agent-node:v1 {\"id\":\"82000000-0000-4000-8000-000000000003\"} -->\n# Introduction Updated\nalpha beta gamma delta epsilon zeta eta theta iota kappa\n",
      "nodes": [
        {
          "revision": {
            "node_revision_id": "a56f8759-0a8a-59d6-bdf8-25a6d197a25f",
            "node_id": "82000000-0000-4000-8000-000000000002",
            "document_revision_id": "0eb00cc5-49d3-5ca9-9c0b-757d5096bc7f",
            "parent_node_revision_id": null,
            "sibling_index": 0,
            "heading_level": 0,
            "title": "",
            "ancestor_titles": [],
            "heading_range": {"start": 149, "end": 149, "start_line": 6, "end_line": 6},
            "local_body_range": {"start": 149, "end": 149, "start_line": 6, "end_line": 6},
            "section_range": {"start": 149, "end": 452, "start_line": 6, "end_line": 11},
            "semantic_local_body_hash": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
            "children": ["f1079f7d-794f-5c66-8c6e-efd2fb64c418", "57332b74-7c49-5442-9041-0809c2df68fb"]
          },
          "heading_slice": "",
          "local_body_slice": "",
          "section_slice": "<!-- edu-agent-node:v1 {\"id\":\"81000000-0000-4000-8000-000000000002\"} -->\n# Details\nmercury venus earth mars jupiter saturn uranus neptune pluto ceres\n<!-- edu-agent-node:v1 {\"id\":\"82000000-0000-4000-8000-000000000003\"} -->\n# Introduction Updated\nalpha beta gamma delta epsilon zeta eta theta iota kappa\n"
        },
        {
          "revision": {
            "node_revision_id": "f1079f7d-794f-5c66-8c6e-efd2fb64c418",
            "node_id": "81000000-0000-4000-8000-000000000002",
            "document_revision_id": "0eb00cc5-49d3-5ca9-9c0b-757d5096bc7f",
            "parent_node_revision_id": "a56f8759-0a8a-59d6-bdf8-25a6d197a25f",
            "sibling_index": 0,
            "heading_level": 1,
            "title": "Details",
            "ancestor_titles": [],
            "heading_range": {"start": 222, "end": 232, "start_line": 7, "end_line": 7},
            "local_body_range": {"start": 232, "end": 299, "start_line": 8, "end_line": 8},
            "section_range": {"start": 149, "end": 299, "start_line": 6, "end_line": 8},
            "semantic_local_body_hash": "b88950878a7b3879d9ddac35c801ca5471ecca6313b542a3ed57b4ddaddb6cea"
          },
          "heading_slice": "# Details\n",
          "local_body_slice": "mercury venus earth mars jupiter saturn uranus neptune pluto ceres\n",
          "section_slice": "<!-- edu-agent-node:v1 {\"id\":\"81000000-0000-4000-8000-000000000002\"} -->\n# Details\nmercury venus earth mars jupiter saturn uranus neptune pluto ceres\n"
        },
        {
          "revision": {
            "node_revision_id": "57332b74-7c49-5442-9041-0809c2df68fb",
            "node_id": "82000000-0000-4000-8000-000000000003",
            "document_revision_id": "0eb00cc5-49d3-5ca9-9c0b-757d5096bc7f",
            "parent_node_revision_id": "a56f8759-0a8a-59d6-bdf8-25a6d197a25f",
            "sibling_index": 1,
            "heading_level": 1,
            "title": "Introduction Updated",
            "ancestor_titles": [],
            "heading_range": {"start": 372, "end": 395, "start_line": 10, "end_line": 10},
            "local_body_range": {"start": 395, "end": 452, "start_line": 11, "end_line": 11},
            "section_range": {"start": 299, "end": 452, "start_line": 9, "end_line": 11},
            "semantic_local_body_hash": "7bc7a74f7ff3cc4ee0b041a6128f56a14459da8c8fa273cebca98c71d61caba3"
          },
          "heading_slice": "# Introduction Updated\n",
          "local_body_slice": "alpha beta gamma delta epsilon zeta eta theta iota kappa\n",
          "section_slice": "<!-- edu-agent-node:v1 {\"id\":\"82000000-0000-4000-8000-000000000003\"} -->\n# Introduction Updated\nalpha beta gamma delta epsilon zeta eta theta iota kappa\n"
        }
      ]
    },
    {
      "path": "stable.md",
      "document_revision_id": "8a75fcd7-52e5-5ef1-972b-8dc1e67eb830",
      "document_id": "83000000-0000-4000-8000-000000000001",
      "root_node_id": "83000000-0000-4000-8000-000000000002",
      "canonical_hash": "766e282d45e25dfa0f6005d0d82a0104675b886ca0a17811390a3748c8f1a15c",
      "semantic_hash": "e0a9d4096658dee1c1ecce32cf1c1740a637bce9568c255ca6832c0b2790e694",
      "canonical_markdown": "---\nedu-agent-format: 1\nedu-agent-document-id: 83000000-0000-4000-8000-000000000001\nedu-agent-root-node-id: 83000000-0000-4000-8000-000000000002\n---\n<!-- edu-agent-node:v1 {\"id\":\"83000000-0000-4000-8000-000000000003\"} -->\n# Stable\nunchanged canonical material remains available\n",
      "nodes": [
        {
          "revision": {
            "node_revision_id": "f76d418b-ed49-524b-8d2d-a301e00f22a5",
            "node_id": "83000000-0000-4000-8000-000000000002",
            "document_revision_id": "8a75fcd7-52e5-5ef1-972b-8dc1e67eb830",
            "parent_node_revision_id": null,
            "sibling_index": 0,
            "heading_level": 0,
            "title": "",
            "ancestor_titles": [],
            "heading_range": {"start": 149, "end": 149, "start_line": 6, "end_line": 6},
            "local_body_range": {"start": 149, "end": 149, "start_line": 6, "end_line": 6},
            "section_range": {"start": 149, "end": 278, "start_line": 6, "end_line": 8},
            "semantic_local_body_hash": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
            "children": ["ab127fb2-4b30-5cf7-aee6-bd5116bcaf15"]
          },
          "heading_slice": "",
          "local_body_slice": "",
          "section_slice": "<!-- edu-agent-node:v1 {\"id\":\"83000000-0000-4000-8000-000000000003\"} -->\n# Stable\nunchanged canonical material remains available\n"
        },
        {
          "revision": {
            "node_revision_id": "ab127fb2-4b30-5cf7-aee6-bd5116bcaf15",
            "node_id": "83000000-0000-4000-8000-000000000003",
            "document_revision_id": "8a75fcd7-52e5-5ef1-972b-8dc1e67eb830",
            "parent_node_revision_id": "f76d418b-ed49-524b-8d2d-a301e00f22a5",
            "sibling_index": 0,
            "heading_level": 1,
            "title": "Stable",
            "ancestor_titles": [],
            "heading_range": {"start": 222, "end": 231, "start_line": 7, "end_line": 7},
            "local_body_range": {"start": 231, "end": 278, "start_line": 8, "end_line": 8},
            "section_range": {"start": 149, "end": 278, "start_line": 6, "end_line": 8},
            "semantic_local_body_hash": "02465936f742abb9ccf6c888b0c02cfeb8c12813a70d64a1133908dc647bc0db"
          },
          "heading_slice": "# Stable\n",
          "local_body_slice": "unchanged canonical material remains available\n",
          "section_slice": "<!-- edu-agent-node:v1 {\"id\":\"83000000-0000-4000-8000-000000000003\"} -->\n# Stable\nunchanged canonical material remains available\n"
        }
      ]
    }
  ],
  "lineages": [
    {
      "lineage_id": "81000000-0000-4000-8000-000000000004",
      "knowledge_revision_id": "81000000-0000-4000-8000-000000000003",
      "action": "rewrite",
      "actor_device_id": "90000000-0000-4000-8000-000000000001",
      "reason": "replace details while preserving explicit lineage",
      "policy_version": "identity-policy-v1",
      "created_at": "2026-08-28T09:00:00Z",
      "members": [
        {"role": "source", "node_revision_id": "0dc537a7-58e5-5e14-90f1-9406fccfc5c8"},
        {"role": "target", "node_revision_id": "f1079f7d-794f-5c66-8c6e-efd2fb64c418"}
      ]
    }
  ],
  "origin": {
    "version": "knowledge-revision-origin-v1",
    "kind": "candidate",
    "proposal_id": "81000000-0000-4000-8000-000000000005",
    "base_revision_id": "81000000-0000-4000-8000-000000000001",
    "basis_hash": "fff4a997fcf716761cb1006abd971209b67700f27ed82152ade71ce7c32445f8"
  }
}`

func knowledgeRebuildGoldenSnapshot(t *testing.T) canonicalKnowledgeSnapshot {
	t.Helper()
	var golden canonicalKnowledgeSnapshot
	if err := json.Unmarshal([]byte(knowledgeRebuildGoldenJSON), &golden); err != nil {
		t.Fatalf("decode static knowledge full-tree golden: %v", err)
	}
	return golden
}

func canonicalKnowledgeSnapshotFromRevision(t *testing.T, revision knowledge.KnowledgeRevision) canonicalKnowledgeSnapshot {
	t.Helper()
	result := canonicalKnowledgeSnapshot{
		ID: revision.ID, RevisionNo: revision.RevisionNo, ParentRevisionID: cloneKnowledgeString(revision.ParentRevisionID),
		ManifestHash: revision.ManifestHash, Source: revision.Source, CreatedByDeviceID: revision.CreatedByDeviceID,
		CreatedAt: revision.CreatedAt.UTC(), CanonicalizerVersion: revision.CanonicalizerVersion,
		ParserVersion: revision.ParserVersion, IndexerVersion: revision.IndexerVersion,
		IdentityPolicyVersion: revision.IdentityPolicyVersion,
	}
	for _, document := range revision.Documents {
		canonical := canonicalKnowledgeDocument{
			Path: document.Path, ID: document.Revision.ID, DocumentID: document.Revision.DocumentID,
			RootNodeID: document.Revision.RootNodeID, CanonicalHash: document.Revision.CanonicalHash,
			SemanticHash: document.Revision.SemanticHash, CanonicalMarkdown: document.Revision.CanonicalMarkdown,
		}
		for _, node := range document.Revision.Nodes {
			copy := node
			copy.ParentNodeRevisionID = cloneKnowledgeString(node.ParentNodeRevisionID)
			copy.AncestorTitles = append([]string{}, node.AncestorTitles...)
			copy.Children = append([]string(nil), node.Children...)
			canonical.Nodes = append(canonical.Nodes, canonicalKnowledgeNode{
				Revision:     copy,
				HeadingSlice: knowledgeRangeSlice(t, document.Revision.CanonicalMarkdown, node.HeadingRange),
				LocalSlice:   knowledgeRangeSlice(t, document.Revision.CanonicalMarkdown, node.LocalBodyRange),
				SectionSlice: knowledgeRangeSlice(t, document.Revision.CanonicalMarkdown, node.SectionRange),
			})
		}
		result.Documents = append(result.Documents, canonical)
	}
	sort.Slice(result.Documents, func(i, j int) bool { return result.Documents[i].Path < result.Documents[j].Path })
	for _, lineage := range revision.Lineages {
		copy := lineage
		copy.CreatedAt = copy.CreatedAt.UTC()
		copy.Members = append([]knowledge.LineageMember{}, lineage.Members...)
		result.Lineages = append(result.Lineages, copy)
	}
	if revision.Origin != nil {
		origin := *revision.Origin
		origin.RollbackTargetRevisionID = cloneKnowledgeString(origin.RollbackTargetRevisionID)
		result.Origin = &origin
	}
	return result
}

func assertKnowledgeSnapshotEqual(t *testing.T, got, want canonicalKnowledgeSnapshot, gotName, wantName string) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	gotJSON, _ := json.MarshalIndent(got, "", "  ")
	wantJSON, _ := json.MarshalIndent(want, "", "  ")
	t.Fatalf("knowledge snapshots differ: %s != %s\n%s:\n%s\n%s:\n%s", gotName, wantName, gotName, gotJSON, wantName, wantJSON)
}

func knowledgeRangeSlice(t *testing.T, canonical string, source knowledge.SourceRange) string {
	t.Helper()
	if source.Start < 0 || source.End < source.Start || source.End > len(canonical) {
		t.Fatalf("invalid canonical range %+v for %d bytes", source, len(canonical))
	}
	return canonical[source.Start:source.End]
}

func hasKnowledgeDocument(snapshot canonicalKnowledgeSnapshot, documentID string) bool {
	for _, document := range snapshot.Documents {
		if document.DocumentID == documentID {
			return true
		}
	}
	return false
}

func cloneKnowledgeString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func orderKnowledgeDocuments(documents []knowledge.ImportDocument, order knowledgeDocumentOrder) {
	switch order {
	case knowledgeOrderForward:
		return
	case knowledgeOrderReverse:
		for left, right := 0, len(documents)-1; left < right; left, right = left+1, right-1 {
			documents[left], documents[right] = documents[right], documents[left]
		}
	case knowledgeOrderRotated:
		if len(documents) > 1 {
			first := documents[0]
			copy(documents, documents[1:])
			documents[len(documents)-1] = first
		}
	default:
		panic("unknown knowledge document order: " + order)
	}
}

func knowledgeRebuildSource(locator string) knowledge.ProposalSource {
	excerpt := "deterministic operations hardening corpus"
	return knowledge.ProposalSource{
		Kind: "other", Locator: locator, Excerpt: excerpt, SHA256: knowledgeRebuildSourceSHA256,
	}
}

func knowledgeRebuildEnvelope(documentID, rootNodeID, body string) string {
	return "---\n" +
		"edu-agent-format: 1\n" +
		"edu-agent-document-id: " + documentID + "\n" +
		"edu-agent-root-node-id: " + rootNodeID + "\n" +
		"---\n" + body
}

func knowledgeRebuildMarker(nodeID string) string {
	return "<!-- edu-agent-node:v1 {\"id\":\"" + nodeID + "\"} -->\n"
}

func knowledgeRebuildBaseGuide() string {
	return knowledgeRebuildEnvelope(knowledgeRebuildGuideDocumentID, knowledgeRebuildGuideRootID,
		knowledgeRebuildMarker(knowledgeRebuildIntroNodeID)+
			"# Introduction\nalpha beta gamma delta epsilon zeta eta theta iota kappa\n"+
			knowledgeRebuildMarker(knowledgeRebuildDetailsNodeID)+
			"# Details\none two three four five six seven eight nine ten\n")
}

func knowledgeRebuildCandidateGuide(detailsNodeID string) string {
	return knowledgeRebuildEnvelope(knowledgeRebuildGuideDocumentID, knowledgeRebuildGuideRootID,
		knowledgeRebuildMarker(detailsNodeID)+
			"# Details\nmercury venus earth mars jupiter saturn uranus neptune pluto ceres\n"+
			knowledgeRebuildMarker(knowledgeRebuildIntroNodeID)+
			"# Introduction Updated\nalpha beta gamma delta epsilon zeta eta theta iota kappa\n")
}

func knowledgeRebuildStable() string {
	return knowledgeRebuildEnvelope(knowledgeRebuildStableDocumentID, knowledgeRebuildStableRootID,
		knowledgeRebuildMarker(knowledgeRebuildStableNodeID)+"# Stable\nunchanged canonical material remains available\n")
}

func knowledgeRebuildDeleted() string {
	return knowledgeRebuildEnvelope(knowledgeRebuildDeleteDocumentID, knowledgeRebuildDeleteRootID,
		knowledgeRebuildMarker(knowledgeRebuildDeleteNodeID)+"# Obsolete\nthis document is deleted by replacement snapshot\n")
}

func knowledgeRebuildAdded() string {
	return knowledgeRebuildEnvelope(knowledgeRebuildAddedDocumentID, knowledgeRebuildAddedRootID,
		knowledgeRebuildMarker(knowledgeRebuildAddedNodeID)+"# Added\nnew canonical material joins the tree\n")
}

func knowledgeRebuildPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; PostgreSQL knowledge rebuild parity corpus not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("knowledge_rebuild_%d", time.Now().UnixNano())
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
	if err := migrations.Check(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
