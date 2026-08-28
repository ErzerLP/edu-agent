package postgresstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	notesyncintegration "github.com/edu-agent/edu-agent/server/internal/integrations/notesync"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
)

func TestNotesyncPublicationOptionIsExplicitAndDocumentRevisionBased(t *testing.T) {
	if New(nil).notesyncPublication {
		t.Fatal("default knowledge store must not publish notesync intents")
	}
	if !New(nil, WithNotesyncPublication()).notesyncPublication {
		t.Fatal("notesync publication option did not enable intents")
	}

	revision := knowledge.KnowledgeRevision{Documents: []knowledge.SnapshotDocument{
		{Path: "renamed/a.md", Revision: knowledge.DocumentRevision{DocumentID: "document-a", ID: "revision-a"}},
		{Path: "b.md", Revision: knowledge.DocumentRevision{DocumentID: "document-b", ID: "revision-b2"}},
		{Path: "new/c.md", Revision: knowledge.DocumentRevision{DocumentID: "document-c", ID: "revision-c1"}},
	}}
	changed := notesyncPublicationDocuments(revision, map[string]notesyncParentDocument{
		"document-a": {RevisionID: "revision-a", Path: "a.md"},
		"document-b": {RevisionID: "revision-b1", Path: "b.md"},
		"document-d": {RevisionID: "revision-d1", Path: "removed/d.md"},
	})
	got := make([]string, 0, len(changed))
	for _, snapshot := range changed {
		got = append(got, snapshot.Revision.DocumentID)
	}
	if want := []string{"document-b", "document-c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changed documents=%v want=%v", got, want)
	}
	affected := notesyncAffectedDocumentIDs(revision, map[string]notesyncParentDocument{
		"document-a": {RevisionID: "revision-a", Path: "a.md"},
		"document-b": {RevisionID: "revision-b1", Path: "b.md"},
		"document-d": {RevisionID: "revision-d1", Path: "removed/d.md"},
	})
	if want := []string{"document-a", "document-b", "document-c", "document-d"}; !reflect.DeepEqual(affected, want) {
		t.Fatalf("affected documents=%v want=%v", affected, want)
	}
}

func TestCanonicalPublicationIdempotencyKeyDoesNotCollapseABA(t *testing.T) {
	const documentID = "20000000-0000-4000-8000-000000000000"
	const documentRevisionA = "30000000-0000-4000-8000-000000000000"
	first := notesyncintegration.CanonicalPublicationIdempotencyKey(documentID, documentRevisionA, 1, 1)
	third := notesyncintegration.CanonicalPublicationIdempotencyKey(documentID, documentRevisionA, 3, 1)
	if first == third {
		t.Fatalf("A->B->A publication key collided: %q", first)
	}
	if replay := notesyncintegration.CanonicalPublicationIdempotencyKey(documentID, documentRevisionA, 1, 1); replay != first {
		t.Fatalf("publication key is not deterministic: %q != %q", replay, first)
	}
}

func TestNotesyncReviewStorePreservesPresentEmptyAndZeroRemoteVersions(t *testing.T) {
	emptyHash := sha256.Sum256(nil)
	review := notesyncintegration.Review{
		ReviewID:   "60000000-0000-4000-8000-000000000000",
		Category:   notesyncintegration.PreviewCategoryUnbasedRemote,
		ReasonCode: notesyncintegration.ReviewReasonUnmanagedRemoteNote,
		Status:     notesyncintegration.ReviewStatusOpen, Generation: 1,
		CanonicalPath: "topic.md", RemoteVault: "Knowledge", RemotePath: "edu-agent/topic.md",
		Base: ReviewSnapshotMissing(), Local: ReviewSnapshotMissing(),
		Remote: notesyncintegration.ReviewSnapshot{
			Markdown: "", SHA256: hex.EncodeToString(emptyHash[:]), RemoteVersion: 0, RemoteLastTime: 0,
		},
		CreatedAt: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
	}
	review.BasisHash = notesyncintegration.ReviewBasisHash(review)
	if err := validateNotesyncReviewRecord(review); err != nil {
		t.Fatalf("present-empty review rejected: %v", err)
	}
	if got := nullableRemoteValue(review.Remote, review.Remote.RemoteVersion); got != int64(0) {
		t.Fatalf("present remote version 0 became %#v", got)
	}
	if got := nullableRemoteValue(notesyncintegration.ReviewSnapshot{Missing: true}, 0); got != nil {
		t.Fatalf("missing remote version became %#v", got)
	}
}

func ReviewSnapshotMissing() notesyncintegration.ReviewSnapshot {
	return notesyncintegration.ReviewSnapshot{Missing: true}
}

func TestNotesyncReviewListColumnsAreCompact(t *testing.T) {
	for _, forbidden := range []string{
		"base_markdown", "local_markdown", "remote_markdown",
		"base_to_local_diff", "base_to_remote_diff", "local_diff_truncated", "remote_diff_truncated",
	} {
		if strings.Contains(notesyncReviewSummaryColumns, forbidden) {
			t.Fatalf("review list columns load %s", forbidden)
		}
		if !strings.Contains(notesyncReviewColumns, forbidden) {
			t.Fatalf("review show columns omit %s", forbidden)
		}
	}
}

func TestNotesyncPublicationIntentJSONIsClosed(t *testing.T) {
	encoded, err := json.Marshal(NotesyncPublicationIntent{
		SchemaVersion:       1,
		DocumentID:          "document-1",
		KnowledgeRevisionID: "knowledge-revision-1",
		DocumentRevisionID:  "document-revision-1",
		PublicationReason:   "canonical_revision",
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"schema_version":1,"document_id":"document-1","knowledge_revision_id":"knowledge-revision-1","document_revision_id":"document-revision-1","publication_reason":"canonical_revision"}`
	if string(encoded) != want {
		t.Fatalf("intent JSON=%s want=%s", encoded, want)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 5 {
		t.Fatalf("intent payload fields=%v", fields)
	}
}
