package notesync

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
)

type consumerFixtureStore struct {
	decision outbox.ApplyDecision
	work     PublicationWork
	outcome  PublicationOutcome
	applies  int
}

func (s *consumerFixtureStore) CanApplyNotesyncPublication(context.Context, outbox.Message, PublicationIntent) (outbox.ApplyDecision, error) {
	return s.decision, nil
}

func (s *consumerFixtureStore) ApplyNotesyncPublication(_ context.Context, _ outbox.Message, _ PublicationIntent, operation PublicationOperation) error {
	s.applies++
	outcome, err := operation(context.Background(), s.work)
	if err != nil {
		return err
	}
	s.outcome = outcome
	if outcome.Kind == OutcomeFailed {
		return FailedPublication(outcome.Category, outcome.Permanent)
	}
	return nil
}

type noteResult struct {
	note Note
	err  error
}

type consumerFixtureRemote struct {
	capability Capability
	gets       []noteResult
	writes     []NoteWrite
	writeErr   error
	probes     int
}

func (r *consumerFixtureRemote) Probe(context.Context, string) Capability {
	r.probes++
	return r.capability
}

func (r *consumerFixtureRemote) GetNote(context.Context, string, string) (Note, error) {
	if len(r.gets) == 0 {
		return Note{}, errors.New("unexpected GetNote")
	}
	result := r.gets[0]
	r.gets = r.gets[1:]
	return result.note, result.err
}

func (r *consumerFixtureRemote) CreateOrUpdateNote(_ context.Context, write NoteWrite) (Note, error) {
	r.writes = append(r.writes, write)
	return Note{Path: write.Path, Content: write.Content, Version: 2, Ctime: write.Ctime, Mtime: write.Mtime, LastTime: write.Mtime}, r.writeErr
}

func TestCanonicalPublicationIdempotencyKeyBindsMonotonicKnowledgeRevision(t *testing.T) {
	const documentID = "20000000-0000-4000-8000-000000000000"
	const documentRevisionID = "30000000-0000-4000-8000-000000000000"
	first := CanonicalPublicationIdempotencyKey(documentID, documentRevisionID, 1, 1)
	third := CanonicalPublicationIdempotencyKey(documentID, documentRevisionID, 3, 1)
	if first == third {
		t.Fatalf("A->B->A reused canonical publication key %q", first)
	}
	if replay := CanonicalPublicationIdempotencyKey(documentID, documentRevisionID, 1, 1); replay != first {
		t.Fatalf("canonical publication key is not deterministic: %q != %q", replay, first)
	}
}

func TestPublicationConsumerInitialCreateOnlyAndExactTargetReplay(t *testing.T) {
	now := time.Date(2026, 8, 27, 4, 0, 0, 0, time.UTC)
	work := publicationFixtureWork(now, nil)

	createStore := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
	createRemote := &consumerFixtureRemote{
		capability: Capability{Compatible: true, Version: SupportedVersion, Vault: "Knowledge"},
		gets: []noteResult{
			{err: &Error{category: CategoryNotFound, operation: "get_note"}},
			{note: fixtureNote(work.TargetMarkdown, now.UnixMilli(), 2)},
		},
	}
	consumer := newFixtureConsumer(t, createStore, createRemote, now)
	if err := consumer.Apply(context.Background(), publicationFixtureMessage(nil)); !errors.Is(err, outbox.ErrConsumerFinalized) {
		t.Fatalf("initial apply err=%v", err)
	}
	if createStore.outcome.Kind != OutcomeApplied || len(createRemote.writes) != 1 {
		t.Fatalf("initial outcome=%+v writes=%+v", createStore.outcome, createRemote.writes)
	}
	write := createRemote.writes[0]
	if !write.CreateOnly || write.Content != work.TargetMarkdown || write.Ctime != now.UnixMilli() || write.Mtime != now.UnixMilli() {
		t.Fatalf("initial write=%+v", write)
	}

	replayStore := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
	replayRemote := &consumerFixtureRemote{
		capability: Capability{Compatible: true, Version: SupportedVersion, Vault: "Knowledge"},
		gets:       []noteResult{{note: fixtureNote(work.TargetMarkdown, now.UnixMilli(), 7)}},
	}
	replayConsumer := newFixtureConsumer(t, replayStore, replayRemote, now)
	if err := replayConsumer.Apply(context.Background(), publicationFixtureMessage(nil)); !errors.Is(err, outbox.ErrConsumerFinalized) {
		t.Fatalf("target replay err=%v", err)
	}
	if replayStore.outcome.Kind != OutcomeApplied || len(replayRemote.writes) != 0 {
		t.Fatalf("target replay outcome=%+v writes=%+v", replayStore.outcome, replayRemote.writes)
	}
}

func TestPublicationConsumerUpdatesOnlyFromExactDurableBase(t *testing.T) {
	now := time.Date(2026, 8, 27, 4, 1, 0, 0, time.UTC)
	mapping := &PublicationMapping{
		RemoteVault: "Knowledge", RemotePath: "edu-agent/topic.md",
		KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000",
		DocumentRevisionID:  "30000000-0000-4000-8000-000000000000",
		RevisionNo:          1, BaseMarkdown: "base markdown", Generation: 1,
	}
	work := publicationFixtureWork(now, mapping)
	remoteCtime := now.Add(-time.Hour).UnixMilli()
	store := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
	remote := &consumerFixtureRemote{
		capability: Capability{Compatible: true, Version: SupportedVersion, Vault: "Knowledge"},
		gets: []noteResult{
			{note: fixtureNote(mapping.BaseMarkdown, remoteCtime, 4)},
			{note: fixtureNote(work.TargetMarkdown, remoteCtime, 5)},
		},
	}
	consumer := newFixtureConsumer(t, store, remote, now)
	if err := consumer.Apply(context.Background(), publicationFixtureMessage(nil)); !errors.Is(err, outbox.ErrConsumerFinalized) {
		t.Fatalf("update apply err=%v", err)
	}
	if store.outcome.Kind != OutcomeApplied || len(remote.writes) != 1 {
		t.Fatalf("update outcome=%+v writes=%+v", store.outcome, remote.writes)
	}
	write := remote.writes[0]
	if write.CreateOnly || write.Ctime != remoteCtime || write.Mtime != now.UnixMilli() || write.Content != work.TargetMarkdown {
		t.Fatalf("update write=%+v", write)
	}
}

func TestPublicationConsumerConflictsNeverPost(t *testing.T) {
	now := time.Date(2026, 8, 27, 4, 2, 0, 0, time.UTC)
	mapping := &PublicationMapping{
		RemoteVault: "Knowledge", RemotePath: "edu-agent/topic.md",
		KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000",
		DocumentRevisionID:  "30000000-0000-4000-8000-000000000000",
		RevisionNo:          1, BaseMarkdown: "base markdown", Generation: 1,
	}
	tests := []struct {
		name       string
		mapping    *PublicationMapping
		result     noteResult
		pathOwned  bool
		wantKind   string
		wantReason string
	}{
		{name: "initial occupied", result: noteResult{note: fixtureNote("someone else's content", now.UnixMilli(), 3)}, wantKind: ReviewKindPathOccupied, wantReason: ReviewReasonRemotePathOccupied},
		{name: "published missing", mapping: mapping, result: noteResult{err: &Error{category: CategoryNotFound, operation: "get_note"}}, wantKind: ReviewKindRemoteMissing, wantReason: ReviewReasonRemoteNoteMissing},
		{name: "published drift", mapping: mapping, result: noteResult{note: fixtureNote("remote drift", now.UnixMilli(), 4)}, wantKind: ReviewKindRemoteChanged, wantReason: ReviewReasonRemoteContentChanged},
		{name: "local mapped path occupied", pathOwned: true, wantKind: ReviewKindPathOccupied, wantReason: ReviewReasonRemotePathOccupied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			work := publicationFixtureWork(now, test.mapping)
			work.PathOccupied = test.pathOwned
			store := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
			remote := &consumerFixtureRemote{capability: Capability{Compatible: true, Version: SupportedVersion, Vault: "Knowledge"}}
			if !test.pathOwned {
				remote.gets = []noteResult{test.result}
			}
			consumer := newFixtureConsumer(t, store, remote, now)
			if err := consumer.Apply(context.Background(), publicationFixtureMessage(nil)); !errors.Is(err, outbox.ErrConsumerFinalized) {
				t.Fatalf("conflict apply err=%v", err)
			}
			if store.outcome.Kind != OutcomeReview || store.outcome.ReviewKind != test.wantKind || store.outcome.ReasonCode != test.wantReason || len(remote.writes) != 0 {
				t.Fatalf("conflict outcome=%+v writes=%+v", store.outcome, remote.writes)
			}
		})
	}
}

func TestPublicationConsumerReconcilesResponseLossAndUsesBoundedRetry(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	mapping := &PublicationMapping{
		RemoteVault: "Knowledge", RemotePath: "edu-agent/topic.md",
		KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000",
		DocumentRevisionID:  "30000000-0000-4000-8000-000000000000",
		RevisionNo:          1, BaseMarkdown: "base markdown", Generation: 1,
	}
	work := publicationFixtureWork(now, mapping)

	appliedStore := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
	appliedRemote := &consumerFixtureRemote{
		capability: Capability{Compatible: true, Version: SupportedVersion, Vault: "Knowledge"},
		gets: []noteResult{
			{note: fixtureNote(mapping.BaseMarkdown, now.Add(-time.Hour).UnixMilli(), 2)},
			{note: fixtureNote(work.TargetMarkdown, now.Add(-time.Hour).UnixMilli(), 3)},
		},
		writeErr: &Error{category: CategoryTimeout, operation: "write_note"},
	}
	if err := newFixtureConsumer(t, appliedStore, appliedRemote, now).Apply(context.Background(), publicationFixtureMessage(nil)); !errors.Is(err, outbox.ErrConsumerFinalized) {
		t.Fatalf("response-loss applied err=%v", err)
	}
	if appliedStore.outcome.Kind != OutcomeApplied || len(appliedRemote.writes) != 1 {
		t.Fatalf("response-loss applied outcome=%+v writes=%d", appliedStore.outcome, len(appliedRemote.writes))
	}

	deferredStore := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
	deferredRemote := &consumerFixtureRemote{
		capability: Capability{Compatible: true, Version: SupportedVersion, Vault: "Knowledge"},
		gets: []noteResult{
			{note: fixtureNote(mapping.BaseMarkdown, now.Add(-time.Hour).UnixMilli(), 2)},
			{note: fixtureNote(mapping.BaseMarkdown, now.Add(-time.Hour).UnixMilli(), 2)},
		},
		writeErr: &Error{category: CategoryTimeout, operation: "write_note"},
	}
	deferredErr := newFixtureConsumer(t, deferredStore, deferredRemote, now).Apply(context.Background(), publicationFixtureMessage(nil))
	assertPublicationFailure(t, deferredErr, "notesync_timeout", false)
	if deferredStore.outcome.Kind != OutcomeFailed || deferredStore.outcome.Category != "notesync_timeout" || deferredStore.outcome.Permanent {
		t.Fatalf("response-loss failure outcome=%+v", deferredStore.outcome)
	}
}

func TestReviewedPublicationRequiresFrozenRemoteSnapshotBeforeWrite(t *testing.T) {
	for _, reason := range []string{PublicationReasonReviewKeepCanonical, PublicationReasonReviewImport} {
		t.Run(reason, func(t *testing.T) {
			now := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
			work := publicationFixtureWork(now, nil)
			work.PublicationReason = reason
			work.ReviewID = "70000000-0000-4000-8000-000000000000"
			work.ReviewRemote = &RemoteObservation{Markdown: "frozen remote", Version: 0, LastTime: 0}
			message := reviewPublicationFixtureMessage(work.ReviewID, reason)

			driftStore := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
			driftRemote := &consumerFixtureRemote{
				capability: Capability{Compatible: true, Version: SupportedVersion, Vault: "Knowledge"},
				gets:       []noteResult{{note: fixtureNote("changed after resolution", now.UnixMilli(), 0)}},
			}
			if err := newFixtureConsumer(t, driftStore, driftRemote, now).Apply(context.Background(), message); !errors.Is(err, outbox.ErrConsumerFinalized) {
				t.Fatalf("review drift apply err=%v", err)
			}
			if driftStore.outcome.Kind != OutcomeReview || driftStore.outcome.ReasonCode != ReviewReasonRemoteContentChanged || len(driftRemote.writes) != 0 {
				t.Fatalf("review drift outcome=%+v writes=%+v", driftStore.outcome, driftRemote.writes)
			}

			applyStore := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
			applyRemote := &consumerFixtureRemote{
				capability: Capability{Compatible: true, Version: SupportedVersion, Vault: "Knowledge"},
				gets: []noteResult{
					{note: fixtureNote(work.ReviewRemote.Markdown, now.Add(-time.Hour).UnixMilli(), 0)},
					{note: fixtureNote(work.TargetMarkdown, now.Add(-time.Hour).UnixMilli(), 0)},
				},
			}
			if err := newFixtureConsumer(t, applyStore, applyRemote, now).Apply(context.Background(), message); !errors.Is(err, outbox.ErrConsumerFinalized) {
				t.Fatalf("review guarded apply err=%v", err)
			}
			if applyStore.outcome.Kind != OutcomeApplied || len(applyRemote.writes) != 1 || applyRemote.writes[0].CreateOnly {
				t.Fatalf("review guarded outcome=%+v writes=%+v", applyStore.outcome, applyRemote.writes)
			}
		})
	}
}

func TestPublicationConsumerCapabilityDependencyAndStaleSuppression(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	work := publicationFixtureWork(now, nil)

	capabilityStore := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
	capabilityRemote := &consumerFixtureRemote{capability: Capability{Reason: "version_untested", Version: "3.7.0", Vault: "Knowledge"}}
	if err := newFixtureConsumer(t, capabilityStore, capabilityRemote, now).Apply(context.Background(), publicationFixtureMessage(nil)); !errors.Is(err, outbox.ErrConsumerFinalized) {
		t.Fatalf("capability defer err=%v", err)
	}
	if capabilityStore.outcome.Kind != OutcomeDeferred || capabilityStore.outcome.Category != "notesync_version_untested" || len(capabilityRemote.gets) != 0 || len(capabilityRemote.writes) != 0 {
		t.Fatalf("capability outcome=%+v remote=%+v", capabilityStore.outcome, capabilityRemote)
	}

	dependencyStore := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
	dependencyRemote := &consumerFixtureRemote{
		capability: Capability{Compatible: true, Version: SupportedVersion, Vault: "Knowledge"},
		gets:       []noteResult{{err: &Error{category: CategoryTransport, operation: "get_note"}}},
	}
	dependencyErr := newFixtureConsumer(t, dependencyStore, dependencyRemote, now).Apply(context.Background(), publicationFixtureMessage(nil))
	assertPublicationFailure(t, dependencyErr, "notesync_transport_unavailable", false)
	if dependencyStore.outcome.Kind != OutcomeFailed || dependencyStore.outcome.Category != "notesync_transport_unavailable" || dependencyStore.outcome.Permanent || len(dependencyRemote.writes) != 0 {
		t.Fatalf("dependency outcome=%+v writes=%+v", dependencyStore.outcome, dependencyRemote.writes)
	}

	contractStore := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
	contractRemote := &consumerFixtureRemote{
		capability: Capability{Compatible: true, Version: SupportedVersion, Vault: "Knowledge"},
		gets:       []noteResult{{err: &Error{category: CategoryContractMismatch, operation: "get_note"}}},
	}
	contractErr := newFixtureConsumer(t, contractStore, contractRemote, now).Apply(context.Background(), publicationFixtureMessage(nil))
	assertPublicationFailure(t, contractErr, "notesync_contract_mismatch", true)
	if contractStore.outcome.Kind != OutcomeFailed || !contractStore.outcome.Permanent || len(contractRemote.writes) != 0 {
		t.Fatalf("contract outcome=%+v writes=%+v", contractStore.outcome, contractRemote.writes)
	}

	authStore := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
	authRemote := &consumerFixtureRemote{
		capability: Capability{Compatible: true, Version: SupportedVersion, Vault: "Knowledge"},
		gets:       []noteResult{{err: &Error{category: CategoryAuth, operation: "get_note"}}},
	}
	if err := newFixtureConsumer(t, authStore, authRemote, now).Apply(context.Background(), publicationFixtureMessage(nil)); !errors.Is(err, outbox.ErrConsumerFinalized) {
		t.Fatalf("auth defer err=%v", err)
	}
	if authStore.outcome.Kind != OutcomeDeferred || authStore.outcome.Category != "notesync_auth_unavailable" || authStore.outcome.AvailableAt.IsZero() || len(authRemote.writes) != 0 {
		t.Fatalf("auth outcome=%+v writes=%+v", authStore.outcome, authRemote.writes)
	}

	writeAuthStore := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
	writeAuthRemote := &consumerFixtureRemote{
		capability: Capability{Compatible: true, Version: SupportedVersion, Vault: "Knowledge"},
		gets: []noteResult{
			{err: &Error{category: CategoryNotFound, operation: "get_note"}},
			{err: &Error{category: CategoryNotFound, operation: "get_note"}},
		},
		writeErr: &Error{category: CategoryAuth, operation: "create_or_update_note"},
	}
	if err := newFixtureConsumer(t, writeAuthStore, writeAuthRemote, now).Apply(context.Background(), publicationFixtureMessage(nil)); !errors.Is(err, outbox.ErrConsumerFinalized) {
		t.Fatalf("write auth defer err=%v", err)
	}
	if writeAuthStore.outcome.Kind != OutcomeDeferred || writeAuthStore.outcome.Category != "notesync_auth_unavailable" || len(writeAuthRemote.writes) != 1 {
		t.Fatalf("write auth outcome=%+v writes=%+v", writeAuthStore.outcome, writeAuthRemote.writes)
	}

	staleStore := &consumerFixtureStore{decision: outbox.ApplyDecision{TerminalDisposition: outbox.DispositionSuperseded}, work: work}
	staleRemote := &consumerFixtureRemote{capability: Capability{Compatible: true}}
	consumer := newFixtureConsumer(t, staleStore, staleRemote, now)
	decision, err := consumer.CanApply(context.Background(), publicationFixtureMessage(nil))
	if err != nil || decision.Apply || decision.TerminalDisposition != outbox.DispositionSuperseded || staleRemote.probes != 0 || staleStore.applies != 0 {
		t.Fatalf("stale decision=%+v err=%v remote=%+v store=%+v", decision, err, staleRemote, staleStore)
	}
}

func assertPublicationFailure(t *testing.T, err error, category string, permanent bool) {
	t.Helper()
	var classified outbox.ClassifiedError
	if !errors.As(err, &classified) || classified.Category() != category || classified.Permanent() != permanent {
		t.Fatalf("publication failure err=%v classified=%T category=%q permanent=%t", err, classified, category, permanent)
	}
}

func TestPublicationIntentDecodeIsClosedAndCrossChecked(t *testing.T) {
	valid := publicationFixtureMessage(nil)
	intent, err := DecodePublicationIntent(valid)
	if err != nil || intent.SchemaVersion != 1 || intent.DocumentID != valid.AggregateID {
		t.Fatalf("valid intent=%+v err=%v", intent, err)
	}

	var fields map[string]any
	if err := json.Unmarshal(valid.Payload, &fields); err != nil {
		t.Fatal(err)
	}
	fields["unexpected"] = true
	unknown := valid
	unknown.Payload, _ = json.Marshal(fields)
	if _, err := DecodePublicationIntent(unknown); err == nil {
		t.Fatal("unknown payload field was accepted")
	}
	mismatched := valid
	mismatched.AggregateID = "40000000-0000-4000-8000-000000000001"
	if _, err := DecodePublicationIntent(mismatched); err == nil {
		t.Fatal("message/payload aggregate mismatch was accepted")
	}
	badKey := valid
	badKey.IdempotencyKey = "notesync.publish:wrong"
	if _, err := DecodePublicationIntent(badKey); err == nil {
		t.Fatal("message/payload idempotency mismatch was accepted")
	}
	wrongBusiness := valid
	wrongBusiness.BusinessType = "knowledge.other"
	if _, err := DecodePublicationIntent(wrongBusiness); err == nil {
		t.Fatal("wrong business type was accepted")
	}
}

func TestManagedPathUsesCanonicalSlashSemantics(t *testing.T) {
	valid, err := ManagedPath("edu-agent", "Résumé/Topic.md")
	if err != nil || valid != "edu-agent/Résumé/Topic.md" {
		t.Fatalf("managed path=%q err=%v", valid, err)
	}
	for _, candidate := range []string{
		"../escape.md", "nested/../escape.md", "/absolute.md", "double//slash.md",
		"windows\\path.md", "decomposed/re\u0301sume\u0301.md", "control/line\nbreak.md",
	} {
		if value, err := ManagedPath("edu-agent", candidate); err == nil {
			t.Fatalf("noncanonical path %q produced %q", candidate, value)
		}
	}
}

func newFixtureConsumer(t *testing.T, store *consumerFixtureStore, remote *consumerFixtureRemote, now time.Time) *Consumer {
	t.Helper()
	consumer, err := NewConsumer(ConsumerOptions{
		Store: store, Remote: remote, Vault: "Knowledge", PathPrefix: "edu-agent",
		RetryBackoff: 3 * time.Second, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return consumer
}

func publicationFixtureWork(now time.Time, mapping *PublicationMapping) PublicationWork {
	return PublicationWork{
		DocumentID:          "20000000-0000-4000-8000-000000000000",
		KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000",
		DocumentRevisionID:  "30000000-0000-4000-8000-000000000000",
		RevisionNo:          2, Generation: 1, CanonicalPath: "topic.md",
		TargetMarkdown: "target markdown", TargetModifiedAt: now,
		RemoteVault: "Knowledge", RemotePath: "edu-agent/topic.md", Mapping: mapping,
	}
}

func publicationFixtureMessage(payload json.RawMessage) outbox.Message {
	if payload == nil {
		payload = json.RawMessage(`{"schema_version":1,"document_id":"20000000-0000-4000-8000-000000000000","knowledge_revision_id":"10000000-0000-4000-8000-000000000000","document_revision_id":"30000000-0000-4000-8000-000000000000","publication_reason":"canonical_revision"}`)
	}
	return outbox.Message{
		ID: "50000000-0000-4000-8000-000000000000", BusinessType: PublicationBusinessType,
		AggregateID:    "20000000-0000-4000-8000-000000000000",
		IdempotencyKey: "notesync.publish:20000000-0000-4000-8000-000000000000:30000000-0000-4000-8000-000000000000:2:1",
		Revision:       2, Generation: 1, Payload: payload, AuditMetadata: json.RawMessage(`{}`),
		Status: outbox.StatusProcessing, Attempts: 1, MaxAttempts: 5,
		LeaseToken: "60000000-0000-4000-8000-000000000000",
	}
}

func reviewPublicationFixtureMessage(reviewID, reason string) outbox.Message {
	payload, _ := json.Marshal(PublicationIntent{
		SchemaVersion: 1, DocumentID: "20000000-0000-4000-8000-000000000000",
		KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000",
		DocumentRevisionID:  "30000000-0000-4000-8000-000000000000",
		PublicationReason:   reason, ReviewID: reviewID,
	})
	message := publicationFixtureMessage(payload)
	message.IdempotencyKey = ReviewPublicationIdempotencyKey(reviewID, "30000000-0000-4000-8000-000000000000", 1)
	return message
}

func fixtureNote(content string, ctime int64, version int64) Note {
	return Note{Path: "edu-agent/topic.md", Content: content, Version: version, Ctime: ctime, Mtime: ctime + 1, LastTime: ctime + 2}
}
