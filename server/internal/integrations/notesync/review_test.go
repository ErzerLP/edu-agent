package notesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
)

func TestPreviewClassificationPriorityAndPersistence(t *testing.T) {
	snapshot := func(value string) ReviewSnapshot {
		if value == "" {
			return ReviewSnapshot{Missing: true}
		}
		return ReviewSnapshot{Markdown: value, SHA256: markdownSHA(value)}
	}
	tests := []struct {
		name                     string
		invalid, moved, occupied bool
		unbased                  bool
		base, local, remote      ReviewSnapshot
		want                     string
		persist                  bool
	}{
		{"invalid", true, true, true, true, snapshot("base"), snapshot("base"), snapshot("remote"), PreviewCategoryInvalidRemoteMarkdown, true},
		{"moved", false, true, true, true, snapshot("base"), snapshot("base"), snapshot("remote"), PreviewCategoryRemoteMoved, true},
		{"occupied", false, false, true, true, snapshot("base"), snapshot("base"), snapshot("remote"), PreviewCategoryPathOccupied, true},
		{"unbased", false, false, false, true, snapshot(""), snapshot("local"), snapshot("remote"), PreviewCategoryUnbasedRemote, true},
		{"missing", false, false, false, false, snapshot("base"), snapshot("base"), snapshot(""), PreviewCategoryRemoteMissing, true},
		{"in sync", false, false, false, false, snapshot("same"), snapshot("same"), snapshot("same"), PreviewCategoryInSync, false},
		{"remote unchanged with missing local", false, false, false, false, snapshot("base"), snapshot(""), snapshot("base"), PreviewCategoryRemoteUnchanged, false},
		{"local changed", false, false, false, false, snapshot("base"), snapshot("local"), snapshot("base"), PreviewCategoryLocalChanged, false},
		{"converged changes", false, false, false, false, snapshot("base"), snapshot("shared"), snapshot("shared"), PreviewCategoryInSync, false},
		{"remote changed", false, false, false, false, snapshot("base"), snapshot("base"), snapshot("remote"), PreviewCategoryRemoteChanged, true},
		{"both changed", false, false, false, false, snapshot("base"), snapshot("local"), snapshot("remote"), PreviewCategoryBothChanged, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyPreview(test.invalid, test.moved, test.occupied, test.unbased, test.base, test.local, test.remote)
			if got != test.want || actionablePreview(got) != test.persist {
				t.Fatalf("classification=%q persist=%t want=%q/%t", got, actionablePreview(got), test.want, test.persist)
			}
		})
	}
}

func TestReviewBasisBindsAllAuthorityAndSnapshotDimensions(t *testing.T) {
	base := Review{
		Category: PreviewCategoryBothChanged, ReasonCode: ReviewReasonBothSidesChanged,
		Generation: 3, HeadRevisionID: "10000000-0000-4000-8000-000000000000", HeadRevisionNo: 7,
		DocumentID: "20000000-0000-4000-8000-000000000000", RemoteDocumentID: "20000000-0000-4000-8000-000000000000",
		CanonicalPath: "topic.md", RemoteVault: "Knowledge", RemotePath: "edu-agent/topic.md",
		Base: ReviewSnapshot{
			KnowledgeRevisionID: "30000000-0000-4000-8000-000000000000", KnowledgeRevisionNo: 6,
			DocumentRevisionID: "40000000-0000-4000-8000-000000000000", Path: "old/topic.md",
			SHA256: markdownSHA("base"), RemoteVersion: 8, RemoteLastTime: 9,
		},
		Local: ReviewSnapshot{
			KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000", KnowledgeRevisionNo: 7,
			DocumentRevisionID: "50000000-0000-4000-8000-000000000000", Path: "topic.md", SHA256: markdownSHA("local"),
		},
		Remote: ReviewSnapshot{
			SourceRevisionID: "60000000-0000-4000-8000-000000000000", SHA256: markdownSHA("remote"),
			RemoteVersion: 10, RemoteLastTime: 11,
		},
	}
	first := ReviewBasisHash(base)
	mutations := []func(*Review){
		func(value *Review) { value.Category = PreviewCategoryRemoteChanged },
		func(value *Review) { value.ReasonCode = ReviewReasonRemoteContentChanged },
		func(value *Review) { value.Generation++ },
		func(value *Review) { value.HeadRevisionID = "70000000-0000-4000-8000-000000000000" },
		func(value *Review) { value.HeadRevisionNo++ },
		func(value *Review) { value.DocumentID = "70000000-0000-4000-8000-000000000000" },
		func(value *Review) { value.RemoteDocumentID = "70000000-0000-4000-8000-000000000000" },
		func(value *Review) { value.CanonicalPath = "other.md" },
		func(value *Review) { value.RemoteVault = "Other" },
		func(value *Review) { value.RemotePath = "edu-agent/other.md" },
		func(value *Review) { value.Base.Missing = true },
		func(value *Review) { value.Base.KnowledgeRevisionNo++ },
		func(value *Review) { value.Base.Path = "another/base.md" },
		func(value *Review) { value.Base.RemoteVersion++ },
		func(value *Review) { value.Base.RemoteLastTime++ },
		func(value *Review) { value.Local.KnowledgeRevisionNo++ },
		func(value *Review) { value.Local.Path = "another/local.md" },
		func(value *Review) { value.Local.SHA256 = markdownSHA("other local") },
		func(value *Review) { value.Remote.SHA256 = markdownSHA("other remote") },
		func(value *Review) { value.Remote.SourceRevisionID = "70000000-0000-4000-8000-000000000000" },
		func(value *Review) { value.Remote.RemoteVersion++ },
		func(value *Review) { value.Remote.RemoteLastTime++ },
	}
	for index, mutate := range mutations {
		candidate := base
		mutate(&candidate)
		if got := ReviewBasisHash(candidate); got == first {
			t.Fatalf("mutation %d did not change basis", index)
		}
	}
}

func TestPreviewUsesExactGetContentAndDeduplicatesReviewByBasis(t *testing.T) {
	remoteMarkdown := validRemoteMarkdown(t, "remote body")
	store := &reviewFixtureStore{state: PreviewState{
		Generation: 2, HeadRevisionID: "10000000-0000-4000-8000-000000000000", HeadRevisionNo: 4,
		DocumentID: "20000000-0000-4000-8000-000000000000", CanonicalPath: "topic.md",
		Mapping: &PublicationMapping{DocumentID: "20000000-0000-4000-8000-000000000000", RemoteVault: "Knowledge", RemotePath: "edu-agent/topic.md", KnowledgeRevisionID: "30000000-0000-4000-8000-000000000000", DocumentRevisionID: "40000000-0000-4000-8000-000000000000", RevisionNo: 3, BaseMarkdown: "base"},
		Local:   ReviewSnapshot{KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000", KnowledgeRevisionNo: 4, DocumentRevisionID: "50000000-0000-4000-8000-000000000000", Path: "topic.md", Markdown: "local", SHA256: markdownSHA("local")},
	}}
	remote := &reviewFixtureRemote{
		capability: Capability{Compatible: true},
		page: NotePage{Notes: []Note{
			{Path: "outside.md"},
			{Path: "edu-agent/topic.md", Content: "truncated list content", Version: 1, LastTime: 1},
		}, Page: 1, PageSize: 10, TotalRows: 11},
		notes: map[string]Note{
			"edu-agent/topic.md": {Vault: "Knowledge", Path: "edu-agent/topic.md", Content: remoteMarkdown, Version: 2, LastTime: 3},
		},
	}
	service := newReviewFixtureService(t, store, remote)
	first, err := service.Preview(context.Background(), PreviewCommand{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Preview(context.Background(), PreviewCommand{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.Items[0].Category != PreviewCategoryBothChanged || first.Items[0].ReviewID == "" ||
		first.Items[0].Remote.SHA256 != markdownSHA(remoteMarkdown) || first.NextPage != 2 {
		t.Fatalf("first preview=%+v", first)
	}
	if second.Items[0].ReviewID != first.Items[0].ReviewID || store.inserts != 1 {
		t.Fatalf("preview replay first=%+v second=%+v inserts=%d", first.Items[0], second.Items[0], store.inserts)
	}
	if want := []string{"edu-agent/topic.md", "edu-agent/topic.md"}; !reflect.DeepEqual(remote.gets, want) {
		t.Fatalf("prefix preview exact GETs=%v want=%v", remote.gets, want)
	}
}

func TestResolutionRechecksRemoteBeforeAnyLocalMutationAndReplaysByDeviceOperation(t *testing.T) {
	now := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	remoteMarkdown := validRemoteMarkdown(t, "remote body")
	review := reviewFixture(remoteMarkdown)
	store := &reviewFixtureStore{reviews: map[string]Review{review.ReviewID: review}, operations: make(map[string]ResolutionOperationRecord)}
	remote := &reviewFixtureRemote{capability: Capability{Compatible: true}, notes: map[string]Note{
		review.RemotePath: {Path: review.RemotePath, Content: "changed after preview", Version: 3, LastTime: 4},
	}}
	importer := &reviewFixtureImporter{}
	service, err := NewReviewService(ReviewServiceOptions{
		Store: store, Remote: remote, Importer: importer, Canonicalizer: knowledge.NewCanonicalizer(),
		Vault: "Knowledge", PathPrefix: "edu-agent", ScanPageSize: 25, ScanMaxPages: 20,
		Now: func() time.Time { return now }, NewUUID: func() string { return "90000000-0000-4000-8000-000000000000" },
	})
	if err != nil {
		t.Fatal(err)
	}
	command := ResolutionCommand{
		ReviewID: review.ReviewID, BasisHash: review.BasisHash, OperationID: "80000000-0000-4000-8000-000000000000",
		DeviceID: "70000000-0000-4000-8000-000000000000", Kind: ResolutionAcceptRemote,
	}
	if _, err := service.Resolve(context.Background(), command); ReviewErrorCode(err) != CodeReviewStale {
		t.Fatalf("stale remote err=%v", err)
	}
	if importer.calls != 0 || store.keepCalls != 0 {
		t.Fatalf("stale resolution had side effects importer=%d keep=%d", importer.calls, store.keepCalls)
	}

	remote.notes[review.RemotePath] = Note{Path: review.RemotePath, Content: remoteMarkdown, Version: 1, LastTime: 2}
	store.keepResult = ResolutionResult{ReviewID: review.ReviewID, ResolutionKind: ResolutionKeepCanonical, KnowledgeRevisionID: review.HeadRevisionID, DocumentID: review.DocumentID, DocumentRevisionID: review.Local.DocumentRevisionID}
	command.Kind = ResolutionKeepCanonical
	result, err := service.Resolve(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	requestHash := resolutionRequestHash(command)
	store.operations[command.DeviceID+"/"+command.OperationID] = ResolutionOperationRecord{RequestHash: requestHash, Result: result}
	replay, err := service.Resolve(context.Background(), command)
	if err != nil || !replay.Replayed || store.keepCalls != 1 {
		t.Fatalf("keep replay=%+v err=%v keepCalls=%d", replay, err, store.keepCalls)
	}
	changed := command
	changed.Kind = ResolutionAcceptRemote
	if _, err := service.Resolve(context.Background(), changed); ReviewErrorCode(err) != CodeReviewIdempotencyConflict {
		t.Fatalf("changed replay err=%v", err)
	}
}

func TestResolutionAcceptRemotePassesAtomicReviewMetadataToKnowledgeImport(t *testing.T) {
	remoteMarkdown := validRemoteMarkdown(t, "accepted body")
	review := reviewFixture(remoteMarkdown)
	store := &reviewFixtureStore{reviews: map[string]Review{review.ReviewID: review}, operations: make(map[string]ResolutionOperationRecord)}
	remote := &reviewFixtureRemote{capability: Capability{Compatible: true}, notes: map[string]Note{
		review.RemotePath: {Path: review.RemotePath, Content: remoteMarkdown, Version: 1, LastTime: 2},
	}}
	importer := &reviewFixtureImporter{result: knowledge.ImportResult{Revision: knowledge.KnowledgeRevision{
		ID: "a0000000-0000-4000-8000-000000000000", Documents: []knowledge.SnapshotDocument{{
			Path: review.CanonicalPath, Revision: knowledge.DocumentRevision{ID: "b0000000-0000-4000-8000-000000000000", DocumentID: review.DocumentID},
		}},
	}}}
	service := newReviewFixtureServiceWithImporter(t, store, remote, importer)
	command := ResolutionCommand{
		ReviewID: review.ReviewID, BasisHash: review.BasisHash, OperationID: "80000000-0000-4000-8000-000000000000",
		DeviceID: "70000000-0000-4000-8000-000000000000", Kind: ResolutionAcceptRemote,
	}
	result, err := service.Resolve(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.KnowledgeRevisionID != importer.result.Revision.ID || importer.command.NotesyncResolution == nil {
		t.Fatalf("resolution=%+v import=%+v", result, importer.command)
	}
	metadata := importer.command.NotesyncResolution
	if metadata.ReviewID != review.ReviewID || metadata.BasisHash != review.BasisHash || metadata.DeviceID != command.DeviceID ||
		metadata.OperationID != command.OperationID || metadata.ObservedRemoteSHA256 != review.Remote.SHA256 || metadata.CanonicalPath != review.CanonicalPath {
		t.Fatalf("atomic metadata=%+v", metadata)
	}
	if importer.command.ExpectedParentRevisionID == nil || *importer.command.ExpectedParentRevisionID != review.HeadRevisionID ||
		!importer.command.ExpectedParentProvided || importer.command.Source != KnowledgeImportSource ||
		importer.command.Documents[0].Markdown != remoteMarkdown {
		t.Fatalf("knowledge import command=%+v", importer.command)
	}
	if metadata.ExpectedDocumentID != review.RemoteDocumentID || metadata.ObservedRemoteVersion != review.Remote.RemoteVersion ||
		metadata.ObservedRemoteLastTime != review.Remote.RemoteLastTime {
		t.Fatalf("resolution authority metadata=%+v", metadata)
	}
}

func TestExplicitMissingPreviewCarriesCanonicalPathAndFindsLocalAuthority(t *testing.T) {
	store := &reviewFixtureStore{state: PreviewState{
		Generation: 1, HeadRevisionID: "10000000-0000-4000-8000-000000000000", HeadRevisionNo: 2,
		DocumentID: "20000000-0000-4000-8000-000000000000", CanonicalPath: "topic.md",
		Mapping: &PublicationMapping{
			DocumentID: "20000000-0000-4000-8000-000000000000", RemoteVault: "Knowledge", RemotePath: "edu-agent/topic.md",
			KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000", DocumentRevisionID: "30000000-0000-4000-8000-000000000000",
			RevisionNo: 2, BaseMarkdown: "base", RemoteVersion: 0, RemoteLastTime: 0, Generation: 1,
		},
		Local: ReviewSnapshot{
			KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000", KnowledgeRevisionNo: 2,
			DocumentRevisionID: "30000000-0000-4000-8000-000000000000", Path: "topic.md", Markdown: "base", SHA256: markdownSHA("base"),
		},
	}}
	remote := &reviewFixtureRemote{capability: Capability{Compatible: true}, notes: map[string]Note{}}
	result, err := newReviewFixtureService(t, store, remote).Preview(context.Background(), PreviewCommand{Path: "edu-agent/topic.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Category != PreviewCategoryRemoteMissing || !result.Items[0].Remote.Missing {
		t.Fatalf("missing preview=%+v", result)
	}
	if store.loadedCanonicalPath != "topic.md" || store.loadedRemotePath != "edu-agent/topic.md" || store.loadedRemoteDocumentID != "" {
		t.Fatalf("preview authority lookup path=%q remote=%q identity=%q", store.loadedCanonicalPath, store.loadedRemotePath, store.loadedRemoteDocumentID)
	}
}

func TestAcceptRemoteWithoutHeadUsesNilExpectedParentAndFixedSource(t *testing.T) {
	remoteMarkdown := validRemoteMarkdown(t, "first canonical revision")
	review := reviewFixture(remoteMarkdown)
	review.HeadRevisionID = ""
	review.HeadRevisionNo = 0
	review.DocumentID = ""
	review.Base = ReviewSnapshot{Missing: true}
	review.Local = ReviewSnapshot{Missing: true}
	review.BasisHash = ReviewBasisHash(review)
	store := &reviewFixtureStore{reviews: map[string]Review{review.ReviewID: review}, operations: make(map[string]ResolutionOperationRecord)}
	remote := &reviewFixtureRemote{capability: Capability{Compatible: true}, notes: map[string]Note{
		review.RemotePath: {Path: review.RemotePath, Content: remoteMarkdown, Version: 1, LastTime: 2},
	}}
	importer := &reviewFixtureImporter{result: knowledge.ImportResult{Revision: knowledge.KnowledgeRevision{
		ID: "a0000000-0000-4000-8000-000000000000", Documents: []knowledge.SnapshotDocument{{
			Path: review.CanonicalPath, Revision: knowledge.DocumentRevision{ID: "b0000000-0000-4000-8000-000000000000", DocumentID: review.RemoteDocumentID},
		}},
	}}}
	service := newReviewFixtureServiceWithImporter(t, store, remote, importer)
	_, err := service.Resolve(context.Background(), ResolutionCommand{
		ReviewID: review.ReviewID, BasisHash: review.BasisHash, OperationID: "80000000-0000-4000-8000-000000000000",
		DeviceID: "70000000-0000-4000-8000-000000000000", Kind: ResolutionAcceptRemote,
	})
	if err != nil {
		t.Fatal(err)
	}
	if importer.command.ExpectedParentRevisionID != nil || !importer.command.ExpectedParentProvided || importer.command.Source != KnowledgeImportSource {
		t.Fatalf("headless import command=%+v", importer.command)
	}
	if importer.command.NotesyncResolution == nil || importer.command.NotesyncResolution.ExpectedDocumentID != review.RemoteDocumentID {
		t.Fatalf("headless resolution metadata=%+v", importer.command.NotesyncResolution)
	}
}

func TestResolutionIdentitySelectionAndMergedMarkerValidation(t *testing.T) {
	review := Review{
		DocumentID: "20000000-0000-4000-8000-000000000000", RemoteDocumentID: "90000000-0000-4000-8000-000000000000",
		Local: ReviewSnapshot{}, Remote: ReviewSnapshot{},
	}
	if got := expectedResolutionDocumentID(review, ResolutionAcceptRemote); got != review.RemoteDocumentID {
		t.Fatalf("accept expected document=%q", got)
	}
	if got := expectedResolutionDocumentID(review, ResolutionMerged); got != review.DocumentID {
		t.Fatalf("merged expected document=%q", got)
	}
	review.Local.Missing = true
	if got := expectedResolutionDocumentID(review, ResolutionMerged); got != review.RemoteDocumentID {
		t.Fatalf("merged missing-local expected document=%q", got)
	}

	fixture := reviewFixture(validRemoteMarkdown(t, "remote body"))
	store := &reviewFixtureStore{reviews: map[string]Review{fixture.ReviewID: fixture}, operations: make(map[string]ResolutionOperationRecord)}
	remote := &reviewFixtureRemote{capability: Capability{Compatible: true}, notes: map[string]Note{
		fixture.RemotePath: {Path: fixture.RemotePath, Content: fixture.Remote.Markdown, Version: 1, LastTime: 2},
	}}
	importer := &reviewFixtureImporter{}
	service := newReviewFixtureServiceWithImporter(t, store, remote, importer)
	_, err := service.Resolve(context.Background(), ResolutionCommand{
		ReviewID: fixture.ReviewID, BasisHash: fixture.BasisHash, OperationID: "80000000-0000-4000-8000-000000000000",
		DeviceID: "70000000-0000-4000-8000-000000000000", Kind: ResolutionMerged, MergedMarkdown: "# merged without identities\n",
	})
	if ReviewErrorCode(err) != CodeReviewInvalidRequest || importer.calls != 0 {
		t.Fatalf("invalid merged identity err=%v importerCalls=%d", err, importer.calls)
	}
}

func TestReviewCursorBindsGenerationAndRejectsTrailingOrMalformedData(t *testing.T) {
	created := time.Date(2026, 8, 28, 2, 3, 4, 5000, time.UTC)
	reviewID := "80000000-0000-4000-8000-000000000000"
	cursor := EncodeReviewCursor(4, created, reviewID)
	gotTime, gotID, err := DecodeReviewCursor(cursor, 4)
	if err != nil || !gotTime.Equal(created) || gotID != reviewID {
		t.Fatalf("cursor time=%s id=%s err=%v", gotTime, gotID, err)
	}
	if _, _, err := DecodeReviewCursor(cursor, 5); ReviewErrorCode(err) != CodeReviewStale {
		t.Fatalf("stale generation err=%v", err)
	}
	for _, value := range []string{"not-base64", cursor + "extra"} {
		if _, _, err := DecodeReviewCursor(value, 4); ReviewErrorCode(err) != CodeReviewStale {
			t.Fatalf("bad cursor %q err=%v", value, err)
		}
	}
}

func TestMarkdownSHARepresentsPresentEmptyContent(t *testing.T) {
	wantHash := sha256.Sum256(nil)
	want := hex.EncodeToString(wantHash[:])
	if got := markdownSHA(""); got != want || !validSHA256(got) {
		t.Fatalf("empty Markdown SHA=%q want=%q", got, want)
	}
}

func TestCollectionSummariesDoNotExposeMarkdownFields(t *testing.T) {
	review := reviewFixture(validRemoteMarkdown(t, "remote body"))
	review.Diff = BuildThreeWayDiff(review.Base, review.Local, review.Remote)
	for name, value := range map[string]any{
		"preview":     previewItem(review),
		"review_list": SummarizeReview(review),
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"markdown"`) {
			t.Fatalf("%s summary exposed Markdown field: %s", name, encoded)
		}
	}
	encodedReview, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encodedReview), `"markdown"`) {
		t.Fatalf("review show omitted full Markdown snapshots: %s", encodedReview)
	}
}

func TestThreeWayDiffIsBoundedAndUTF8Safe(t *testing.T) {
	base := strings.Repeat("基线内容", 50000)
	local := strings.Repeat("本地内容", 50000)
	diff := BuildThreeWayDiff(
		ReviewSnapshot{Markdown: base},
		ReviewSnapshot{Markdown: local},
		ReviewSnapshot{Markdown: base},
	)
	if !diff.LocalTruncated || len(diff.BaseToLocal) > maxDiffBytes || !utf8.ValidString(diff.BaseToLocal) {
		t.Fatalf("bounded diff bytes=%d truncated=%t utf8=%t", len(diff.BaseToLocal), diff.LocalTruncated, utf8.ValidString(diff.BaseToLocal))
	}
	if diff.RemoteTruncated || diff.BaseToRemote != "" {
		t.Fatalf("unchanged remote diff=%+v", diff)
	}
}

func TestPreviewEnforcesConfiguredScanBounds(t *testing.T) {
	remoteMarkdown := validRemoteMarkdown(t, "bounded page")
	store := &reviewFixtureStore{state: PreviewState{Generation: 1, CanonicalPath: "topic.md", Local: ReviewSnapshot{Missing: true}}}
	remote := &reviewFixtureRemote{
		capability: Capability{Compatible: true},
		page:       NotePage{Notes: []Note{{Path: "edu-agent/topic.md"}}, Page: 2, PageSize: 10, TotalRows: 30},
		notes: map[string]Note{
			"edu-agent/topic.md": {Vault: "Knowledge", Path: "edu-agent/topic.md", Content: remoteMarkdown, Version: 0, LastTime: 0},
		},
	}
	service, err := NewReviewService(ReviewServiceOptions{
		Store: store, Remote: remote, Importer: &reviewFixtureImporter{}, Canonicalizer: knowledge.NewCanonicalizer(),
		Vault: "Knowledge", PathPrefix: "edu-agent", ScanPageSize: 10, ScanMaxPages: 2,
		NewUUID: func() string { return "90000000-0000-4000-8000-000000000001" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Preview(context.Background(), PreviewCommand{Page: 3, PageSize: 10}); ReviewErrorCode(err) != CodeReviewInvalidRequest {
		t.Fatalf("page beyond configured maximum err=%v", err)
	}
	if _, err := service.Preview(context.Background(), PreviewCommand{Page: 1, PageSize: 11}); ReviewErrorCode(err) != CodeReviewInvalidRequest {
		t.Fatalf("page size beyond configured maximum err=%v", err)
	}
	result, err := service.Preview(context.Background(), PreviewCommand{Page: 2, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.NextPage != 0 || result.Items[0].Remote.RemoteVersion != 0 || result.Items[0].Remote.Missing {
		t.Fatalf("bounded final page=%+v", result)
	}
	if want := []string{"edu-agent/topic.md"}; !reflect.DeepEqual(remote.gets, want) {
		t.Fatalf("bounded preview exact GETs=%v want=%v", remote.gets, want)
	}
}

func TestPreviewNonActionableDoesNotAllocateReviewID(t *testing.T) {
	remoteMarkdown := validRemoteMarkdown(t, "in sync")
	remoteHash := markdownSHA(remoteMarkdown)
	store := &reviewFixtureStore{state: PreviewState{
		Generation: 1, HeadRevisionID: "10000000-0000-4000-8000-000000000000", HeadRevisionNo: 2,
		DocumentID: "20000000-0000-4000-8000-000000000000", CanonicalPath: "topic.md",
		Mapping: &PublicationMapping{
			DocumentID: "20000000-0000-4000-8000-000000000000", RemoteVault: "Knowledge", RemotePath: "edu-agent/topic.md",
			KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000", DocumentRevisionID: "30000000-0000-4000-8000-000000000000",
			RevisionNo: 2, BaseMarkdown: remoteMarkdown, Generation: 1,
		},
		Local: ReviewSnapshot{
			KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000", KnowledgeRevisionNo: 2,
			DocumentRevisionID: "30000000-0000-4000-8000-000000000000", Path: "topic.md", Markdown: remoteMarkdown, SHA256: remoteHash,
		},
	}}
	remote := &reviewFixtureRemote{
		capability: Capability{Compatible: true},
		page:       NotePage{Notes: []Note{{Path: "edu-agent/topic.md"}}, Page: 1, PageSize: 10, TotalRows: 1},
		notes: map[string]Note{
			"edu-agent/topic.md": {Vault: "Knowledge", Path: "edu-agent/topic.md", Content: remoteMarkdown, Version: 1, LastTime: 2},
		},
	}
	uuidCalls := 0
	service, err := NewReviewService(ReviewServiceOptions{
		Store: store, Remote: remote, Importer: &reviewFixtureImporter{}, Canonicalizer: knowledge.NewCanonicalizer(),
		Vault: "Knowledge", PathPrefix: "edu-agent", ScanPageSize: 10, ScanMaxPages: 2,
		NewUUID: func() string {
			uuidCalls++
			return "90000000-0000-4000-8000-000000000001"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Preview(context.Background(), PreviewCommand{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Category != PreviewCategoryInSync || result.Items[0].ReviewID != "" || uuidCalls != 0 || store.inserts != 0 {
		t.Fatalf("non-actionable preview=%+v uuid_calls=%d inserts=%d", result, uuidCalls, store.inserts)
	}
}

func TestPreviewRejectsMismatchedExactRemoteSnapshot(t *testing.T) {
	remoteMarkdown := validRemoteMarkdown(t, "strict snapshot")
	for _, test := range []struct {
		name string
		note Note
	}{
		{name: "vault", note: Note{Vault: "Other", Path: "edu-agent/topic.md", Content: remoteMarkdown}},
		{name: "path", note: Note{Vault: "Knowledge", Path: "edu-agent/other.md", Content: remoteMarkdown}},
		{name: "content", note: Note{Vault: "Knowledge", Path: "edu-agent/topic.md", Content: string([]byte{0xff})}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &reviewFixtureStore{state: PreviewState{Generation: 1, CanonicalPath: "topic.md", Local: ReviewSnapshot{Missing: true}}}
			remote := &reviewFixtureRemote{capability: Capability{Compatible: true}, notes: map[string]Note{"edu-agent/topic.md": test.note}}
			service := newReviewFixtureService(t, store, remote)
			if _, err := service.Preview(context.Background(), PreviewCommand{Path: "edu-agent/topic.md"}); ReviewErrorCode(err) != CodeReviewUnavailable {
				t.Fatalf("mismatched exact snapshot err=%v", err)
			}
			if store.inserts != 0 {
				t.Fatalf("mismatched exact snapshot persisted %d reviews", store.inserts)
			}
		})
	}
}

func validRemoteMarkdown(t *testing.T, body string) string {
	t.Helper()
	canonicalizer := knowledge.NewCanonicalizer()
	inspected, err := canonicalizer.Inspect("# Topic\n" + body + "\n")
	if err != nil {
		t.Fatal(err)
	}
	document, err := canonicalizer.Materialize(inspected,
		"20000000-0000-4000-8000-000000000000", "21000000-0000-4000-8000-000000000000",
		[]string{"22000000-0000-4000-8000-000000000000"})
	if err != nil {
		t.Fatal(err)
	}
	exported, err := knowledge.ExportMarkdown(document.CanonicalMarkdown, "30000000-0000-4000-8000-000000000000")
	if err != nil {
		t.Fatal(err)
	}
	return exported
}

func reviewFixture(remoteMarkdown string) Review {
	review := Review{
		ReviewID: "60000000-0000-4000-8000-000000000000", Category: PreviewCategoryRemoteChanged,
		ReasonCode: ReviewReasonRemoteContentChanged, Status: ReviewStatusOpen, Generation: 1,
		HeadRevisionID: "10000000-0000-4000-8000-000000000000", HeadRevisionNo: 2,
		DocumentID: "20000000-0000-4000-8000-000000000000", RemoteDocumentID: "20000000-0000-4000-8000-000000000000",
		CanonicalPath: "topic.md", RemoteVault: "Knowledge", RemotePath: "edu-agent/topic.md",
		Base:      ReviewSnapshot{Markdown: "base", SHA256: markdownSHA("base")},
		Local:     ReviewSnapshot{KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000", KnowledgeRevisionNo: 2, DocumentRevisionID: "40000000-0000-4000-8000-000000000000", Markdown: "local", SHA256: markdownSHA("local")},
		Remote:    ReviewSnapshot{Markdown: remoteMarkdown, SHA256: markdownSHA(remoteMarkdown), SourceRevisionID: "30000000-0000-4000-8000-000000000000", RemoteVersion: 1, RemoteLastTime: 2},
		CreatedAt: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
	}
	review.BasisHash = ReviewBasisHash(review)
	return review
}

type reviewFixtureStore struct {
	state                  PreviewState
	reviews                map[string]Review
	byBasis                map[string]Review
	operations             map[string]ResolutionOperationRecord
	keepResult             ResolutionResult
	loadedRemotePath       string
	loadedCanonicalPath    string
	loadedRemoteDocumentID string
	inserts                int
	keepCalls              int
}

func (s *reviewFixtureStore) LoadNotesyncPreviewState(_ context.Context, _ string, remotePath, canonicalPath, remoteDocumentID string) (PreviewState, error) {
	s.loadedRemotePath = remotePath
	s.loadedCanonicalPath = canonicalPath
	s.loadedRemoteDocumentID = remoteDocumentID
	return s.state, nil
}
func (s *reviewFixtureStore) SaveNotesyncReview(_ context.Context, review Review) (Review, error) {
	if s.byBasis == nil {
		s.byBasis = make(map[string]Review)
	}
	if stored, exists := s.byBasis[review.BasisHash]; exists {
		return stored, nil
	}
	s.inserts++
	s.byBasis[review.BasisHash] = review
	if s.reviews == nil {
		s.reviews = make(map[string]Review)
	}
	s.reviews[review.ReviewID] = review
	return review, nil
}
func (s *reviewFixtureStore) ListNotesyncReviews(context.Context, ReviewListCommand) (ReviewPage, error) {
	return ReviewPage{}, nil
}
func (s *reviewFixtureStore) NotesyncReview(_ context.Context, reviewID string) (Review, error) {
	review, exists := s.reviews[reviewID]
	if !exists {
		return Review{}, &ReviewError{Code: CodeReviewNotFound}
	}
	return review, nil
}
func (s *reviewFixtureStore) LookupNotesyncResolution(_ context.Context, deviceID, operationID string) (ResolutionOperationRecord, bool, error) {
	record, exists := s.operations[deviceID+"/"+operationID]
	return record, exists, nil
}
func (s *reviewFixtureStore) ResolveNotesyncKeep(_ context.Context, request KeepResolutionRequest) (ResolutionResult, error) {
	s.keepCalls++
	return s.keepResult, nil
}

type reviewFixtureRemote struct {
	capability Capability
	page       NotePage
	notes      map[string]Note
	gets       []string
}

func (r *reviewFixtureRemote) Probe(context.Context, string) Capability { return r.capability }
func (r *reviewFixtureRemote) GetNote(_ context.Context, _, path string) (Note, error) {
	r.gets = append(r.gets, path)
	note, exists := r.notes[path]
	if !exists {
		return Note{}, &Error{category: CategoryNotFound, operation: "get_note"}
	}
	return note, nil
}
func (r *reviewFixtureRemote) ListNotes(context.Context, string, int, int) (NotePage, error) {
	return r.page, nil
}

type reviewFixtureImporter struct {
	calls   int
	command knowledge.ImportCommand
	result  knowledge.ImportResult
	err     error
}

func (i *reviewFixtureImporter) Import(_ context.Context, command knowledge.ImportCommand) (knowledge.ImportResult, error) {
	i.calls++
	i.command = command
	return i.result, i.err
}

func newReviewFixtureService(t *testing.T, store *reviewFixtureStore, remote *reviewFixtureRemote) *ReviewService {
	t.Helper()
	return newReviewFixtureServiceWithImporter(t, store, remote, &reviewFixtureImporter{})
}

func newReviewFixtureServiceWithImporter(t *testing.T, store *reviewFixtureStore, remote *reviewFixtureRemote, importer *reviewFixtureImporter) *ReviewService {
	t.Helper()
	counter := 0
	service, err := NewReviewService(ReviewServiceOptions{
		Store: store, Remote: remote, Importer: importer, Canonicalizer: knowledge.NewCanonicalizer(),
		Vault: "Knowledge", PathPrefix: "edu-agent", ScanPageSize: 25, ScanMaxPages: 20,
		NewUUID: func() string {
			counter++
			if counter == 1 {
				return "90000000-0000-4000-8000-000000000001"
			}
			return "90000000-0000-4000-8000-000000000002"
		},
		Now: func() time.Time { return time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestKnowledgeImportOperationIDAdvancesForIdentityReviewContinuation(t *testing.T) {
	command := ResolutionCommand{
		ReviewID:    "90000000-0000-4000-8000-000000000001",
		OperationID: "90000000-0000-4000-8000-000000000002",
		DeviceID:    "90000000-0000-4000-8000-000000000003",
	}
	initial := knowledgeImportOperationID(command)
	if got := knowledgeImportOperationID(command); got != initial {
		t.Fatalf("initial operation ID is unstable: %s != %s", got, initial)
	}

	command.IdentityReviewOperationID = initial
	command.IdentityReviewReceipt = strings.Repeat("a", 64)
	continuation := knowledgeImportOperationID(command)
	if continuation == initial {
		t.Fatal("identity-review continuation reused the operation that issued the review")
	}
	if got := knowledgeImportOperationID(command); got != continuation {
		t.Fatalf("continuation operation ID is unstable: %s != %s", got, continuation)
	}
}

func TestReviewServiceRejectsIncompleteDependencies(t *testing.T) {
	_, err := NewReviewService(ReviewServiceOptions{})
	if err == nil {
		t.Fatal("incomplete review service dependencies were accepted")
	}
	var target *ReviewError
	if errors.As(err, &target) {
		t.Fatalf("constructor error should not masquerade as request error: %v", err)
	}
}
