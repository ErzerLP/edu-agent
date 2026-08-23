package nocturne

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

const (
	testAPIToken              = "api-token-0123456789-0123456789-abcdef"
	testMaintenanceToken      = "maintenance-token-0123456789-0123456789-abcdef"
	testNamespace             = "edu-agent"
	testDomain                = "core"
	testParent                = "edu-agent"
	testLogicalID             = "10000000-0000-4000-8000-000000000001"
	testCandidateID           = "10000000-0000-4000-8000-000000000002"
	testDeliveryID            = "20000000-0000-4000-8000-000000000001"
	testPayloadID             = "30000000-0000-4000-8000-000000000001"
	testRecordID              = "40000000-0000-4000-8000-000000000001"
	testAttemptID             = "50000000-0000-4000-8000-000000000001"
	testAttemptToken          = "60000000-0000-4000-8000-000000000001"
	testLeaseToken            = "70000000-0000-4000-8000-000000000001"
	testReceiptID             = "80000000-0000-4000-8000-000000000001"
	testNodeID                = "90000000-0000-4000-8000-000000000001"
	testErasureID             = "a0000000-0000-4000-8000-000000000001"
	testMaintenanceReceiptID  = "b0000000-0000-4000-8000-000000000001"
	testReconciliationID      = "c0000000-0000-4000-8000-000000000001"
	testReconciliationLeaseID = "d0000000-0000-4000-8000-000000000001"
	testErasureDeliveryID     = "e0000000-0000-4000-8000-000000000001"
	testMaintenanceGeneration = int64(2)
)

func TestClientStrictCRUDMaintenanceAndFixedRouting(t *testing.T) {
	fake := newStrictHTTPFake(t)
	server := httptest.NewServer(fake)
	defer server.Close()
	client := testClient(t, server.URL, time.Second, 64<<10)
	ctx := context.Background()
	if err := client.Health(ctx); err != nil {
		t.Fatal(err)
	}
	caps, err := client.Capabilities(ctx)
	if err != nil || caps.BootEpoch != "boot-1" {
		t.Fatalf("caps=%+v err=%v", caps, err)
	}
	created, err := client.CreateNode(ctx, testLogicalID, "first content")
	if err != nil || created.MemoryID != 1 || created.URI != "core://edu-agent/"+testLogicalID {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	node, err := client.GetNode(ctx, testParent+"/"+testLogicalID)
	if err != nil || node.NodeID != testNodeID || node.Content != "first content" {
		t.Fatalf("node=%+v err=%v", node, err)
	}
	updated, err := client.UpdateNode(ctx, testParent+"/"+testLogicalID, "second content")
	if err != nil || updated.MemoryID != 2 {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	results, err := client.Search(ctx, testLogicalID)
	if err != nil || len(results) != 1 || results[0].URI != created.URI {
		t.Fatalf("search=%+v err=%v", results, err)
	}
	refs, err := client.References(ctx, testNodeID)
	if err != nil || refs.ActiveMemoryID != 2 || !reflect.DeepEqual(refs.MemoryIDs, []int64{1, 2}) {
		t.Fatalf("refs=%+v err=%v", refs, err)
	}
	orphans, err := client.ListOrphans(ctx)
	if err != nil || len(orphans) != 1 || orphans[0].MemoryID != 1 {
		t.Fatalf("orphans=%+v err=%v", orphans, err)
	}
	if _, err := client.OrphanDetail(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PermanentDelete(ctx, 2); !IsActive(err) {
		t.Fatalf("active delete err=%v", err)
	}
	if _, err := client.PermanentDelete(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := client.DeletePath(ctx, testParent+"/"+testLogicalID); err != nil {
		t.Fatal(err)
	}
	if fake.badRequest != "" {
		t.Fatal(fake.badRequest)
	}
	if got := fake.calls; len(got) < 10 {
		t.Fatalf("calls=%v", got)
	}
}

func TestClientClassifiesFailuresAndRedactsSecrets(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		category ErrorCategory
	}{
		{"auth", 401, `{"detail":"raw-auth-body"}`, CategoryAuth}, {"forbidden", 403, `{}`, CategoryAuth},
		{"missing", 404, `{}`, CategoryNotFound}, {"active", 409, `{}`, CategoryActive},
		{"validation", 422, `{}`, CategoryValidation}, {"rate", 429, `{}`, CategoryRateLimited}, {"server", 503, `{}`, CategoryUpstream},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			_, err := testClient(t, server.URL, time.Second, 1024).GetNode(context.Background(), "edu-agent/"+testLogicalID)
			if Category(err) != test.category {
				t.Fatalf("category=%s err=%v", Category(err), err)
			}
			if strings.Contains(err.Error(), testAPIToken) || strings.Contains(err.Error(), "raw-auth-body") {
				t.Fatalf("secret/raw body leaked: %v", err)
			}
		})
	}
	for _, test := range []struct {
		name, body string
		limit      int64
	}{
		{"malformed", `{"node":`, 1024}, {"unknown", `{"node":{},"children":[],"breadcrumbs":[],"extra":true}`, 1024},
		{"missing", `{"node":{},"children":[],"breadcrumbs":[]}`, 1024}, {"oversize", strings.Repeat("x", 1025), 1024},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			_, err := testClient(t, server.URL, time.Second, test.limit).GetNode(context.Background(), "edu-agent/"+testLogicalID)
			if Category(err) != CategoryContractMismatch {
				t.Fatalf("category=%s err=%v", Category(err), err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
	}))
	defer server.Close()
	_, err := testClient(t, server.URL, 10*time.Millisecond, 1024).GetNode(context.Background(), "edu-agent/"+testLogicalID)
	if Category(err) != CategoryTimeout {
		t.Fatalf("timeout category=%s err=%v", Category(err), err)
	}
}

func TestClientPreflightDistinguishesContractMismatchFromNetworkOutage(t *testing.T) {
	compatible := httptest.NewServer(newStrictHTTPFake(t))
	defer compatible.Close()
	if err := testClient(t, compatible.URL, time.Second, 64<<10).Preflight(context.Background()); err != nil {
		t.Fatalf("compatible preflight failed: %v", err)
	}

	drifted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			writeJSON(w, map[string]any{"status": "ok", "database": "connected"})
		case "/internal/edu-agent/capabilities":
			writeJSON(w, map[string]any{"upstream_commit": ImageUpstreamCommit, "compat_revision": ImageCompatibilityRevision, "boot_epoch": "boot-1"})
		default:
			writeJSON(w, map[string]any{"unexpected": "exposed"})
		}
	}))
	defer drifted.Close()
	err := testClient(t, drifted.URL, time.Second, 64<<10).Preflight(context.Background())
	if !IsContractMismatch(err) {
		t.Fatalf("route drift was not a contract mismatch: %v", err)
	}

	unavailable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unavailableURL := unavailable.URL
	unavailable.Close()
	err = testClient(t, unavailableURL, 50*time.Millisecond, 64<<10).Preflight(context.Background())
	if err == nil || IsContractMismatch(err) || Category(err) != CategoryTransport {
		t.Fatalf("network outage classification=%s err=%v", Category(err), err)
	}
}

func TestClientOptionsRejectWeakOrAmbiguousAuthority(t *testing.T) {
	base, _ := url.Parse("http://localhost:8233")
	valid := Options{BaseURL: base, APIToken: testAPIToken, MaintenanceToken: testMaintenanceToken, Timeout: time.Second, BodyLimit: 1024, Namespace: testNamespace, Domain: testDomain, ParentPath: testParent, Disclosure: "preference"}
	mutations := []func(*Options){
		func(o *Options) { o.APIToken = "short" }, func(o *Options) { o.MaintenanceToken = "short" },
		func(o *Options) { o.MaintenanceToken = o.APIToken }, func(o *Options) { u := *o.BaseURL; u.RawQuery = "namespace=caller"; o.BaseURL = &u },
		func(o *Options) { o.Timeout = 0 }, func(o *Options) { o.BodyLimit = 0 }, func(o *Options) { o.Namespace = "" }, func(o *Options) { o.Domain = "" },
	}
	for i, mutate := range mutations {
		options := valid
		mutate(&options)
		if _, err := New(options); err == nil {
			t.Fatalf("invalid option %d accepted", i)
		}
	}
}

func TestConsumerResponseLostReconcilesWithoutSecondMutation(t *testing.T) {
	store := newProtocolStore()
	remote := &scriptedRemote{boot: "boot-1", nodeContent: "", createApplyThenLose: true, activeMemoryID: 7}
	consumer := testConsumer(t, store, remote)
	message := testMessage(t, store.work)
	if err := consumer.Apply(context.Background(), message); Category(err) != CategoryTransport {
		t.Fatalf("first apply err=%v", err)
	}
	if store.attempt.State != memory.AttemptUnknown || remote.createCalls != 1 {
		t.Fatalf("attempt=%+v creates=%d", store.attempt, remote.createCalls)
	}
	store.attempt.State = memory.AttemptReconciling
	store.attempt.LeaseToken = testLeaseToken
	if err := consumer.Apply(context.Background(), message); err != nil {
		t.Fatalf("reconcile err=%v", err)
	}
	if remote.createCalls != 1 || store.finalized.Kind != memory.AttemptOutcomeApplied || store.finalized.ExternalMemoryID != 7 || store.work.Content != "" {
		t.Fatalf("creates=%d finalized=%+v content=%q", remote.createCalls, store.finalized, store.work.Content)
	}
	wantPrefix := []string{"capabilities", "get", "create", "capabilities", "get", "references"}
	if !reflect.DeepEqual(remote.calls, wantPrefix) {
		t.Fatalf("calls=%v want=%v", remote.calls, wantPrefix)
	}
}

func TestCorrectionResponseLostResolvesNewActiveMemoryIDFromReferences(t *testing.T) {
	store := newProtocolStore()
	content := "Prefer detailed examples"
	store.work.Content = content
	store.work.Delivery.Kind = memory.DeliveryCorrection
	store.work.Delivery.PayloadHash = memory.SHA256String(content)
	store.work.Policy.ContentHash = store.work.Delivery.PayloadHash
	store.work.PreviousContentHash = memory.SHA256String("Prefer concise examples")
	store.work.ExternalNodeID = testNodeID
	store.work.ExternalMemoryID = 7
	store.attempt.State = memory.AttemptReconciling
	store.attempt.BootEpoch = "boot-1"
	remote := &scriptedRemote{boot: "boot-1", nodeContent: content, activeMemoryID: 8}
	consumer := testConsumer(t, store, remote)
	if err := consumer.Apply(context.Background(), testMessage(t, store.work)); err != nil {
		t.Fatalf("correction reconciliation err=%v", err)
	}
	if store.finalized.ExternalMemoryID != 8 || store.finalized.ExternalMemoryID == store.work.ExternalMemoryID {
		t.Fatalf("correction finalized=%+v prior_memory_id=%d", store.finalized, store.work.ExternalMemoryID)
	}
	if !reflect.DeepEqual(remote.calls, []string{"capabilities", "get", "references"}) {
		t.Fatalf("correction reconciliation calls=%v", remote.calls)
	}
}

func TestUnknownAttemptSameBootNeverMutatesAndRestartAtomicallyRetries(t *testing.T) {
	store := newProtocolStore()
	store.attempt.State = memory.AttemptReconciling
	store.attempt.BootEpoch = "boot-1"
	remote := &scriptedRemote{boot: "boot-1"}
	consumer := testConsumer(t, store, remote)
	if err := consumer.Apply(context.Background(), testMessage(t, store.work)); err == nil || store.authorized.AbsenceObservations != 0 || remote.createCalls != 0 {
		t.Fatalf("same boot err=%v authorized=%+v creates=%d", err, store.authorized, remote.createCalls)
	}
	remote.boot = "boot-2"
	remote.activeMemoryID = 7
	remote.calls = nil
	if err := consumer.Apply(context.Background(), testMessage(t, store.work)); err != nil {
		t.Fatalf("restart retry err=%v", err)
	}
	wantCalls := []string{"capabilities", "get", "get", "get", "create", "get"}
	if store.authorized.AbsenceObservations != 2 || store.authorized.ObservedBootEpoch != "boot-2" ||
		remote.createCalls != 1 || store.finalized.Kind != memory.AttemptOutcomeApplied || !reflect.DeepEqual(remote.calls, wantCalls) {
		t.Fatalf("authorization=%+v calls=%v creates=%d finalized=%+v", store.authorized, remote.calls, remote.createCalls, store.finalized)
	}
}

func TestConsumerStaleGenerationCannotFinalizeApplied(t *testing.T) {
	store := newProtocolStore()
	store.decisions = []outbox.ApplyDecision{{Apply: true}, {TerminalDisposition: outbox.DispositionPrivacyErasure}}
	remote := &scriptedRemote{boot: "boot-1", nodeContent: store.work.Content, activeMemoryID: 3}
	consumer := testConsumer(t, store, remote)
	if err := consumer.Apply(context.Background(), testMessage(t, store.work)); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Fatalf("stale err=%v", err)
	}
	if store.finalized.Kind == memory.AttemptOutcomeApplied {
		t.Fatalf("stale generation finalized: %+v", store.finalized)
	}
}

func TestAttemptReconcilerResumesDeleteThroughPurger(t *testing.T) {
	store := newProtocolStore()
	store.work.Delivery.Kind = memory.DeliveryDelete
	store.work.Delivery.Status = memory.DeliveryStatusDeletePending
	store.attempt.State = memory.AttemptReconciling
	remote := &scriptedRemote{boot: "boot-2", nodeContent: store.work.Content, activeMemoryID: 2,
		references: memory.RemoteReferences{NodeID: testNodeID, Complete: true, ActiveMemoryID: 2, MemoryIDs: []int64{1, 2}, Paths: []memory.RemotePathReference{{Namespace: testNamespace, Domain: testDomain, Path: testParent + "/" + testLogicalID, URI: testDomain + "://" + testParent + "/" + testLogicalID}}}}
	consumer := testConsumer(t, store, remote)
	reconciler, err := NewAttemptReconciler(consumer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	count, err := reconciler.RunOnce(context.Background())
	if err != nil || count != 1 || store.finalized.Kind != memory.AttemptOutcomeDeleted ||
		store.savedPlan.ID == "" || !remote.pathDeleted {
		t.Fatalf("count=%d err=%v finalized=%+v plan=%+v calls=%v", count, err, store.finalized, store.savedPlan, remote.calls)
	}
}

func TestPurgerPersistsEnumerationBeforeMutationAndActive409StaysPartial(t *testing.T) {
	store := newProtocolStore()
	remote := &scriptedRemote{nodeContent: store.work.Content, activeMemoryID: 2, references: memory.RemoteReferences{NodeID: testNodeID, Complete: true, ActiveMemoryID: 2, MemoryIDs: []int64{1, 2}, Paths: []memory.RemotePathReference{{Namespace: testNamespace, Domain: testDomain, Path: testParent + "/" + testLogicalID, URI: testDomain + "://" + testParent + "/" + testLogicalID}}}, activeDelete409: true}
	store.remote = remote
	purger, err := NewPurger(store, remote, testNamespace, testDomain, testParent, func() time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	err = purger.Purge(context.Background(), testDeliveryID, testLogicalID, store.work.Delivery.PayloadHash)
	if err == nil || store.savedPlan.ActiveMemoryID != 2 {
		t.Fatalf("err=%v plan=%+v", err, store.savedPlan)
	}
	saveIndex, deleteIndex := indexOf(remote.calls, "save_observed"), indexOf(remote.calls, "delete_path")
	if saveIndex < 0 || deleteIndex < 0 || saveIndex > deleteIndex {
		t.Fatalf("snapshot was not durable before unlink: %v", remote.calls)
	}
	if remote.permanentCalls != 2 || remote.globalClearCalls != 0 {
		t.Fatalf("permanent=%d globalClear=%d", remote.permanentCalls, remote.globalClearCalls)
	}
}

func TestMutationDriftAfterApplyRemainsUnknownWithPayload(t *testing.T) {
	for _, test := range []struct {
		name     string
		contract bool
		readback bool
	}{
		{name: "2xx dto drift", contract: true},
		{name: "successful write readback drift", readback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newProtocolStore()
			remote := &scriptedRemote{boot: "boot-1", activeMemoryID: 7, createContractDrift: test.contract, readbackContractDrift: test.readback}
			consumer := testConsumer(t, store, remote)
			err := consumer.Apply(context.Background(), testMessage(t, store.work))
			if Category(err) != CategoryContractMismatch || store.attempt.State != memory.AttemptUnknown ||
				store.work.Content == "" || store.finalized.Kind == memory.AttemptOutcomePermanentlyRejected {
				t.Fatalf("err=%v attempt=%+v content=%q finalized=%+v", err, store.attempt, store.work.Content, store.finalized)
			}
		})
	}
}

func TestAdapterPolicyRejectsIncompleteOrUntrustedWorkBeforeRemoteCall(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*memory.DeliveryWork)
	}{
		{name: "payload hash", mutate: func(v *memory.DeliveryWork) { v.Delivery.PayloadHash = memory.SHA256String("different") }},
		{name: "source", mutate: func(v *memory.DeliveryWork) { v.Policy.Source = memory.SourceModelInference }},
		{name: "category", mutate: func(v *memory.DeliveryWork) { v.Policy.Category = memory.CategoryGoal }},
		{name: "sensitivity", mutate: func(v *memory.DeliveryWork) { v.Policy.Sensitivity = memory.SensitivitySensitive }},
		{name: "stability", mutate: func(v *memory.DeliveryWork) { v.Policy.Stability = memory.StabilityTransient }},
		{name: "policy version", mutate: func(v *memory.DeliveryWork) { v.Policy.PolicyVersion = "memory-admission-v0" }},
		{name: "content hash", mutate: func(v *memory.DeliveryWork) { v.Policy.ContentHash = memory.SHA256String("different") }},
		{name: "decision provenance", mutate: func(v *memory.DeliveryWork) {
			v.Policy.AdmissionDecision.CandidateID = "10000000-0000-4000-8000-000000000099"
		}},
		{name: "automatic content unproven", mutate: func(v *memory.DeliveryWork) {
			v.Content = "Sometimes I study late"
			v.Delivery.PayloadHash = memory.SHA256String(v.Content)
			v.Policy.ContentHash = v.Delivery.PayloadHash
		}},
		{name: "forbidden body masquerade", mutate: func(v *memory.DeliveryWork) {
			v.Content = "Complete answer: use the grading rubric"
			v.Delivery.PayloadHash = memory.SHA256String(v.Content)
			v.Policy.ContentHash = v.Delivery.PayloadHash
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newProtocolStore()
			test.mutate(&store.work)
			remote := &scriptedRemote{boot: "boot-1"}
			err := testConsumer(t, store, remote).Apply(context.Background(), testMessage(t, store.work))
			if !errors.Is(err, outbox.ErrConsumerFinalized) || store.work.Content != "" ||
				store.work.Delivery.Status != memory.DeliveryStatusPermanentlyRejected || len(remote.calls) != 0 {
				t.Fatalf("err=%v work=%+v calls=%v", err, store.work, remote.calls)
			}
		})
	}
}

func TestConsumerRejectsDelayedWriteAfterExpiryWithoutRemoteCall(t *testing.T) {
	store := newProtocolStore()
	store.work.Delivery.ValidUntil = store.work.Delivery.CreatedAt
	remote := &scriptedRemote{boot: "boot-1"}
	err := testConsumer(t, store, remote).Apply(context.Background(), testMessage(t, store.work))
	var protocol *protocolError
	if !errors.As(err, &protocol) || !protocol.Permanent() || protocol.Category() != "delivery_expired" ||
		len(remote.calls) != 0 || store.finalized.Kind != "" {
		t.Fatalf("err=%v calls=%v finalized=%+v", err, remote.calls, store.finalized)
	}
}

func TestClientRejectsRedirectsAndParsesSnakeCaseBackupFixture(t *testing.T) {
	var redirectedAuth []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedAuth = append(redirectedAuth, r.Header.Get("Authorization"))
		writeJSON(w, map[string]any{})
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/internal/edu-agent/backups" {
			writeJSON(w, map[string]any{"validated": true, "manifest_sha256": strings.Repeat("b", 64), "artifacts": []any{map[string]any{
				"path": "managed-g00000000000000000001-fixture.backup.enc", "created_at": "2026-09-01T00:00:00Z", "size_bytes": 42,
				"sha256": strings.Repeat("a", 64), "learner_generation": 1,
				"wrapped_key_id": "10000000-0000-4000-8000-000000000009",
			}}})
			return
		}
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := testClient(t, source.URL, time.Second, 64<<10)
	if _, err := client.GetNode(context.Background(), testParent+"/"+testLogicalID); Category(err) != CategoryContractMismatch {
		t.Fatalf("API redirect err=%v", err)
	}
	if _, err := client.References(context.Background(), testNodeID); Category(err) != CategoryContractMismatch {
		t.Fatalf("maintenance redirect err=%v", err)
	}
	if len(redirectedAuth) != 0 {
		t.Fatalf("authorization reached redirect target: %v", redirectedAuth)
	}
	inventory, err := client.Backups(context.Background())
	if err != nil || !inventory.Validated || len(inventory.Artifacts) != 1 || inventory.Artifacts[0].LearnerGeneration != 1 {
		t.Fatalf("backup fixture=%+v err=%v", inventory, err)
	}
}

func TestClientBackupPruneProtocolStrict(t *testing.T) {
	operationID := "e0000000-0000-4000-8000-000000000001"
	cutoff := time.Date(2026, 9, 2, 3, 4, 5, 6000, time.UTC)
	oldDigest := strings.Repeat("a", 64)
	newDigest := strings.Repeat("b", 64)
	paths := []string{
		"managed-g00000000000000000001-a.backup.enc",
		"managed-g00000000000000000001-b.backup.enc",
	}
	var postCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/internal/edu-agent/backups":
			writeJSON(w, map[string]any{"validated": true, "manifest_sha256": oldDigest, "artifacts": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/internal/edu-agent/backups/prune":
			postCalls++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			expected := map[string]any{
				"operation_id": operationID, "cutoff": cutoff.Format(time.RFC3339Nano),
				"expected_manifest_sha256": oldDigest, "paths": []any{paths[0], paths[1]},
			}
			if !reflect.DeepEqual(body, expected) {
				t.Errorf("prune body=%v want=%v", body, expected)
			}
			writeJSON(w, map[string]any{"operation_id": operationID, "deleted_paths": paths, "manifest_sha256": newDigest})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := testClient(t, server.URL, time.Second, 64<<10)
	inventory, err := client.Backups(context.Background())
	if err != nil || inventory.ManifestSHA256 != oldDigest {
		t.Fatalf("inventory=%+v err=%v", inventory, err)
	}
	result, err := client.PruneBackups(context.Background(), memory.BackupPruneRequest{
		OperationID: operationID, Cutoff: cutoff, ExpectedManifestSHA256: oldDigest, Paths: paths,
	})
	if err != nil || result.OperationID != operationID || result.ManifestSHA256 != newDigest || !reflect.DeepEqual(result.DeletedPaths, paths) || postCalls != 1 {
		t.Fatalf("result=%+v calls=%d err=%v", result, postCalls, err)
	}
}

func TestClientBackupProtocolRejectsInvalidSchemaAndRequest(t *testing.T) {
	validDigest := strings.Repeat("a", 64)
	operationID := "e0000000-0000-4000-8000-000000000001"
	path := "managed-g00000000000000000001-a.backup.enc"
	for name, response := range map[string]map[string]any{
		"missing-digest":   {"validated": true, "artifacts": []any{}},
		"uppercase-digest": {"validated": true, "manifest_sha256": strings.Repeat("A", 64), "artifacts": []any{}},
		"unsafe-path": {"validated": true, "manifest_sha256": validDigest, "artifacts": []any{map[string]any{
			"path": "nested/backup.enc", "created_at": "2026-09-01T00:00:00Z", "size": 1, "sha256": validDigest,
			"learner_generation": 1, "wrapped_key_id": "10000000-0000-4000-8000-000000000009",
		}}},
	} {
		t.Run("get-"+name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				writeJSON(w, response)
			}))
			defer server.Close()
			if _, err := testClient(t, server.URL, time.Second, 64<<10).Backups(context.Background()); Category(err) != CategoryContractMismatch {
				t.Fatalf("err=%v", err)
			}
		})
	}
	validRequest := memory.BackupPruneRequest{
		OperationID: operationID, Cutoff: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		ExpectedManifestSHA256: validDigest, Paths: []string{path},
	}
	for name, response := range map[string]map[string]any{
		"operation":        {"operation_id": "e0000000-0000-4000-8000-000000000002", "deleted_paths": []string{path}, "manifest_sha256": strings.Repeat("b", 64)},
		"uppercase-digest": {"operation_id": operationID, "deleted_paths": []string{path}, "manifest_sha256": strings.Repeat("B", 64)},
		"unsorted-paths":   {"operation_id": operationID, "deleted_paths": []string{path + "-b", path + "-a"}, "manifest_sha256": strings.Repeat("b", 64)},
		"unknown-field":    {"operation_id": operationID, "deleted_paths": []string{path}, "manifest_sha256": strings.Repeat("b", 64), "extra": true},
	} {
		t.Run("post-"+name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				writeJSON(w, response)
			}))
			defer server.Close()
			_, err := testClient(t, server.URL, time.Second, 64<<10).PruneBackups(context.Background(), validRequest)
			if Category(err) != CategoryContractMismatch || !MutationDispatched(err) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	var calls int
	validationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		writeJSON(w, map[string]any{})
	}))
	defer validationServer.Close()
	client := testClient(t, validationServer.URL, time.Second, 64<<10)
	invalidRequests := []memory.BackupPruneRequest{
		{OperationID: "not-a-uuid", Cutoff: validRequest.Cutoff, ExpectedManifestSHA256: validDigest, Paths: []string{path}},
		{OperationID: operationID, Cutoff: validRequest.Cutoff.In(time.FixedZone("offset", 3600)), ExpectedManifestSHA256: validDigest, Paths: []string{path}},
		{OperationID: operationID, Cutoff: validRequest.Cutoff, ExpectedManifestSHA256: strings.Repeat("A", 64), Paths: []string{path}},
		{OperationID: operationID, Cutoff: validRequest.Cutoff, ExpectedManifestSHA256: validDigest, Paths: []string{path + "-b", path + "-a"}},
		{OperationID: operationID, Cutoff: validRequest.Cutoff, ExpectedManifestSHA256: validDigest, Paths: []string{"../escape"}},
	}
	for index, request := range invalidRequests {
		if _, err := client.PruneBackups(context.Background(), request); Category(err) != CategoryValidation {
			t.Fatalf("invalid request %d err=%v", index, err)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid requests reached remote: %d", calls)
	}
	rawServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"detail":"raw-prune-secret"}`))
	}))
	defer rawServer.Close()
	_, err := testClient(t, rawServer.URL, time.Second, 64<<10).PruneBackups(context.Background(), validRequest)
	if err == nil || strings.Contains(err.Error(), "raw-prune-secret") {
		t.Fatalf("raw response leaked: %v", err)
	}
}

func TestPurgerRejectsIncompleteAndResidualReferenceEnumerations(t *testing.T) {
	base := memory.RemoteReferences{NodeID: testNodeID, Complete: true}
	tests := []struct {
		name string
		edit func(*memory.RemoteReferences)
	}{
		{name: "incomplete empty", edit: func(v *memory.RemoteReferences) { v.Complete = false }},
		{name: "edge", edit: func(v *memory.RemoteReferences) { v.EdgeIDs = []string{"edge"} }},
		{name: "search", edit: func(v *memory.RemoteReferences) { v.SearchDocumentIDs = []string{"search"} }},
		{name: "access", edit: func(v *memory.RemoteReferences) { v.AccessLogIDs = []string{"access"} }},
		{name: "boot", edit: func(v *memory.RemoteReferences) {
			v.BootURIs = []memory.RemoteBootReference{{Preset: "p", Namespace: testNamespace, URI: "core://x"}}
		}},
		{name: "review", edit: func(v *memory.RemoteReferences) { v.ReviewReferences = []string{"review"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			refs := base
			test.edit(&refs)
			remote := &scriptedRemote{references: refs}
			store := newProtocolStore()
			purger, err := NewPurger(store, remote, testNamespace, testDomain, testParent, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan := memory.RemoteDeletePlan{NodeID: testNodeID, Paths: []memory.RemotePathReference{{Path: testParent + "/" + testLogicalID, URI: testDomain + "://" + testParent + "/" + testLogicalID}}}
			if err := purger.verifyAbsent(context.Background(), plan, testLogicalID); err == nil {
				t.Fatal("unsafe reference enumeration accepted")
			}
		})
	}
}

func TestMaintenanceRemoteEraserPurgesAndFinalizesWithBoundAuthorization(t *testing.T) {
	store := newMaintenanceProtocolStore(memory.ReconciliationPending, "boot-0")
	store.maintenanceHistoricalPending = 2
	remote := &scriptedRemote{
		boot: "boot-1", nodeContent: store.work.Content, activeMemoryID: 2,
		references: maintenanceReferences(),
	}
	eraser := testRemoteEraser(t, store, remote, 4)
	result, err := eraser.Erase(context.Background(), maintenanceEraseRequest(privacy.StepPending))
	if err != nil || result.Status != privacy.StepSucceeded || result.StableReason != "all_old_generation_remote_reconciliations_verified" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if store.maintenanceSaveCount != 1 || store.maintenanceFinalization.Result != memory.ReconciliationDeleteResult ||
		store.maintenanceFinalization.From != memory.ReconciliationDeletePending || store.maintenanceClaimCount != 1 ||
		store.maintenanceHistoricalPending != 0 || remote.permanentCalls != 2 {
		t.Fatalf("saves=%d finalization=%+v claims=%d historical_pending=%d permanent=%d", store.maintenanceSaveCount, store.maintenanceFinalization, store.maintenanceClaimCount, store.maintenanceHistoricalPending, remote.permanentCalls)
	}
	if saveIndex, deleteIndex := indexOf(remote.calls, "save_maintenance_observed"), indexOf(remote.calls, "delete_path"); saveIndex < 0 || deleteIndex < 0 || saveIndex > deleteIndex {
		t.Fatalf("maintenance plan was not durable before mutation: %v", remote.calls)
	}
	assertMaintenanceAuthorization(t, store.maintenanceAuths)
}

func TestMaintenanceRemoteEraserSameBootAbsenceRemainsUnknown(t *testing.T) {
	store := newMaintenanceProtocolStore(memory.ReconciliationPending, "boot-1")
	remote := &scriptedRemote{boot: "boot-1"}
	result, err := testRemoteEraser(t, store, remote, 4).Erase(context.Background(), maintenanceEraseRequest(privacy.StepPending))
	if err != nil || result.Status != privacy.StepUnknown || result.StableReason != "remote_reconciliation_unknown" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if store.maintenance.Status != memory.ReconciliationReconciling || store.maintenanceFinalization.ReconciliationID != "" ||
		remote.permanentCalls != 0 || indexOf(remote.calls, "delete_path") >= 0 {
		t.Fatalf("maintenance=%+v finalization=%+v calls=%v", store.maintenance, store.maintenanceFinalization, remote.calls)
	}
	assertMaintenanceAuthorization(t, store.maintenanceAuths)
}

func TestMaintenanceRemoteEraserHashConflictFinalizesPartial(t *testing.T) {
	store := newMaintenanceProtocolStore(memory.ReconciliationPending, "boot-1")
	store.maintenanceHistoricalPending = 1
	remote := &scriptedRemote{boot: "boot-1", nodeContent: "different content"}
	result, err := testRemoteEraser(t, store, remote, 4).Erase(context.Background(), maintenanceEraseRequest(privacy.StepPending))
	if err != nil || result.Status != privacy.StepPartial || result.StableReason != "remote_reconciliation_conflict" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if store.maintenance.Status != memory.ReconciliationConflict || store.maintenanceFinalization.Result != memory.ReconciliationConflictResult ||
		store.maintenanceFinalization.From != memory.ReconciliationReconciling || store.maintenanceSaveCount != 0 ||
		store.maintenanceClaimCount != 1 || store.maintenanceHistoricalPending != 1 || remote.permanentCalls != 0 {
		t.Fatalf("maintenance=%+v finalization=%+v saves=%d claims=%d historical_pending=%d calls=%v", store.maintenance, store.maintenanceFinalization, store.maintenanceSaveCount, store.maintenanceClaimCount, store.maintenanceHistoricalPending, remote.calls)
	}
	assertMaintenanceAuthorization(t, store.maintenanceAuths)
}

func TestMaintenanceRemoteEraserResumesDurableDeletePlanIdempotently(t *testing.T) {
	store := newMaintenanceProtocolStore(memory.ReconciliationPending, "boot-0")
	remote := &scriptedRemote{
		boot: "boot-1", nodeContent: store.work.Content, activeMemoryID: 2,
		references: maintenanceReferences(), activeDelete409: true,
	}
	eraser := testRemoteEraser(t, store, remote, 4)
	request := maintenanceEraseRequest(privacy.StepPending)
	first, err := eraser.Erase(context.Background(), request)
	if err != nil || first.Status != privacy.StepPartial || store.maintenance.Status != memory.ReconciliationDeletePending || store.maintenanceSaveCount != 1 {
		t.Fatalf("first=%+v err=%v maintenance=%+v saves=%d", first, err, store.maintenance, store.maintenanceSaveCount)
	}
	if store.maintenanceFinalization.ReconciliationID != "" || !remote.deletedMemoryIDs[1] || remote.deletedMemoryIDs[2] {
		t.Fatalf("unexpected first finalization=%+v deleted=%v", store.maintenanceFinalization, remote.deletedMemoryIDs)
	}

	remote.activeDelete409 = false
	resumeAt := len(remote.calls)
	request.Receipt.Status = privacy.StepPartial
	second, err := eraser.Erase(context.Background(), request)
	if err != nil || second.Status != privacy.StepSucceeded || store.maintenanceFinalization.Result != memory.ReconciliationDeleteResult || store.maintenanceSaveCount != 2 {
		t.Fatalf("second=%+v err=%v finalization=%+v saves=%d", second, err, store.maintenanceFinalization, store.maintenanceSaveCount)
	}
	resumeCalls := remote.calls[resumeAt:]
	if len(resumeCalls) < 2 || !reflect.DeepEqual(resumeCalls[:2], []string{"save_maintenance_observed", "delete_path"}) {
		t.Fatalf("resume did not use durable authorized plan first: %v", resumeCalls)
	}

	replayedAt := len(remote.calls)
	replayed, err := eraser.Erase(context.Background(), request)
	if err != nil || replayed.Status != privacy.StepSucceeded || len(remote.calls) != replayedAt {
		t.Fatalf("replayed=%+v err=%v calls=%v", replayed, err, remote.calls[replayedAt:])
	}
	assertMaintenanceAuthorization(t, store.maintenanceAuths)
}

func newMaintenanceProtocolStore(status memory.ReconciliationStatus, sentBoot string) *protocolStore {
	store := newProtocolStore()
	store.maintenance = memory.ExpiryReconciliation{
		ID: testReconciliationID, DeliveryID: testDeliveryID, ErasureDeliveryID: testErasureDeliveryID,
		LogicalMemoryID: testLogicalID,
		ExternalURI:     memory.DeterministicExternalURI(testLogicalID), ContentHash: store.work.Delivery.PayloadHash,
		AttemptToken: testAttemptToken, SentBootEpoch: sentBoot, LearnerGeneration: 1, RecordGeneration: 1,
		Status: status, CreatedAt: store.work.Delivery.CreatedAt, UpdatedAt: store.work.Delivery.UpdatedAt,
	}
	return store
}

func maintenanceReferences() memory.RemoteReferences {
	return memory.RemoteReferences{
		NodeID: testNodeID, Complete: true, ActiveMemoryID: 2, MemoryIDs: []int64{1, 2},
		Paths: []memory.RemotePathReference{{Namespace: testNamespace, Domain: testDomain, Path: testParent + "/" + testLogicalID, URI: testDomain + "://" + testParent + "/" + testLogicalID}},
	}
}

func maintenanceEraseRequest(status privacy.StepStatus) privacy.RemoteEraseRequest {
	return privacy.RemoteEraseRequest{
		ErasureID: testErasureID, LearnerGeneration: testMaintenanceGeneration,
		Receipt: privacy.StepReceipt{ID: testMaintenanceReceiptID, Store: privacy.StoreNocturnePaths, Status: status},
	}
}

func testRemoteEraser(t *testing.T, store *protocolStore, remote *scriptedRemote, maxRuns int) *RemoteEraser {
	t.Helper()
	store.remote = remote
	now := func() time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) }
	purger, err := NewPurger(store, remote, testNamespace, testDomain, testParent, now)
	if err != nil {
		t.Fatal(err)
	}
	eraser, err := NewRemoteEraser(store, remote, purger, RemoteEraserOptions{Lease: time.Minute, MaxReconciliations: maxRuns, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return eraser
}

func assertMaintenanceAuthorization(t *testing.T, values []memory.MaintenanceAuthorization) {
	t.Helper()
	want := memory.MaintenanceAuthorization{ErasureID: testErasureID, ReceiptID: testMaintenanceReceiptID, TargetLearnerGeneration: testMaintenanceGeneration}
	if len(values) == 0 {
		t.Fatal("maintenance authorization was not observed")
	}
	for _, value := range values {
		if value != want {
			t.Fatalf("authorization=%+v want=%+v", value, want)
		}
	}
}

func testClient(t *testing.T, rawURL string, timeout time.Duration, limit int64) *Client {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Options{BaseURL: parsed, APIToken: testAPIToken, MaintenanceToken: testMaintenanceToken, Timeout: timeout, BodyLimit: limit, Namespace: testNamespace, Domain: testDomain, ParentPath: testParent, Priority: 1, Disclosure: "stable preference"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type strictHTTPFake struct {
	t                   *testing.T
	mu                  sync.Mutex
	calls               []string
	badRequest, content string
	memoryID            int64
	deleted             map[int64]bool
}

func newStrictHTTPFake(t *testing.T) *strictHTTPFake {
	return &strictHTTPFake{t: t, content: "", deleted: map[int64]bool{}}
}
func (f *strictHTTPFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	w.Header().Set("Content-Type", "application/json")
	maintenance := strings.HasPrefix(r.URL.Path, "/internal/")
	wantToken := testAPIToken
	if maintenance {
		wantToken = testMaintenanceToken
	}
	if r.URL.Path != "/health" && r.Header.Get("Authorization") != "Bearer "+wantToken {
		f.badRequest = "wrong bearer"
		w.WriteHeader(401)
		return
	}
	if r.URL.Path != "/health" && r.Header.Get("X-Namespace") != testNamespace {
		f.badRequest = "wrong namespace"
		w.WriteHeader(422)
		return
	}
	path := testParent + "/" + testLogicalID
	uri := testDomain + "://" + path
	switch {
	case r.URL.Path == "/health":
		writeJSON(w, map[string]any{"status": "ok", "database": "connected"})
	case r.URL.Path == "/internal/edu-agent/capabilities":
		writeJSON(w, map[string]any{"upstream_commit": memory.NocturneUpstreamCommit, "compat_revision": memory.NocturneCompatRevision, "boot_epoch": "boot-1"})
	case r.URL.Path == "/api/browse/node" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		expected := map[string]any{"parent_path": testParent, "content": "first content", "priority": float64(1), "disclosure": "stable preference", "title": testLogicalID, "domain": testDomain}
		if !reflect.DeepEqual(body, expected) {
			f.badRequest = fmt.Sprintf("create body=%v", body)
			w.WriteHeader(422)
			return
		}
		f.memoryID = 1
		f.content = "first content"
		writeJSON(w, map[string]any{"success": true, "uri": uri, "memory_id": 1})
	case r.URL.Path == "/api/browse/node" && r.Method == http.MethodGet:
		if r.URL.Query().Get("domain") != testDomain || r.URL.Query().Get("path") != path {
			f.badRequest = "caller controlled route"
			w.WriteHeader(422)
			return
		}
		writeJSON(w, nodeResponse(path, uri, f.content))
	case r.URL.Path == "/api/browse/node" && r.Method == http.MethodPut:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, exists := body["domain"]; exists {
			f.badRequest = "PUT contained domain"
			w.WriteHeader(422)
			return
		}
		f.memoryID = 2
		f.content = "second content"
		writeJSON(w, map[string]any{"success": true, "memory_id": 2})
	case r.URL.Path == "/api/browse/search":
		writeJSON(w, map[string]any{"query": testLogicalID, "results": []any{map[string]any{"domain": testDomain, "path": path, "uri": uri, "name": testLogicalID, "snippet": "second", "priority": 1, "disclosure": "stable preference"}}, "count": 1})
	case r.URL.Path == "/internal/edu-agent/nodes/"+testNodeID+"/references":
		writeJSON(w, map[string]any{"node_uuid": testNodeID, "complete": true, "active_memory_id": 2, "memory_ids": []int64{1, 2}, "paths": []any{map[string]any{"namespace": testNamespace, "domain": testDomain, "path": path, "uri": uri, "alias": false}}, "edge_ids": []string{"edge-1"}, "glossary_keywords": []string{}, "search_document_ids": []string{"search-1"}, "access_log_ids": []string{}, "boot_uris": []any{}, "review_references": []string{}})
	case r.URL.Path == "/api/maintenance/orphans" && r.Method == http.MethodGet:
		writeJSON(w, []any{orphanResponse(1, true, 2, "deprecated")})
	case r.URL.Path == "/api/maintenance/orphans/1" && r.Method == http.MethodGet:
		if f.deleted[1] {
			w.WriteHeader(404)
			return
		}
		writeJSON(w, orphanDetailResponse(1, true, 2, "deprecated"))
	case r.URL.Path == "/api/maintenance/orphans/2" && r.Method == http.MethodDelete:
		w.WriteHeader(409)
		writeJSON(w, map[string]any{"detail": "active raw body"})
	case r.URL.Path == "/api/maintenance/orphans/1" && r.Method == http.MethodDelete:
		f.deleted[1] = true
		writeJSON(w, map[string]any{"deleted_memory_id": 1, "chain_repaired_to": 2, "rows_before": map[string]any{}, "rows_after": map[string]any{}})
	case r.URL.Path == "/api/browse/node" && r.Method == http.MethodDelete:
		f.content = ""
		writeJSON(w, map[string]any{"success": true, "uri": uri})
	default:
		w.WriteHeader(404)
		writeJSON(w, map[string]any{"detail": "missing"})
	}
}
func nodeResponse(path, uri, content string) map[string]any {
	return map[string]any{"node": map[string]any{"path": path, "domain": testDomain, "uri": uri, "name": testLogicalID, "content": content, "priority": 1, "disclosure": "stable preference", "created_at": "2026-09-01T00:00:00Z", "is_virtual": false, "aliases": []string{}, "node_uuid": testNodeID, "glossary_keywords": []string{}, "glossary_matches": []any{}}, "children": []any{}, "breadcrumbs": []any{map[string]any{"path": "", "label": "root"}, map[string]any{"path": path, "label": testLogicalID}}}
}
func orphanResponse(id int64, deprecated bool, migrated int64, category string) map[string]any {
	return map[string]any{"id": id, "node_uuid": testNodeID, "content_snippet": "discarded", "created_at": "2026-09-01T00:00:00Z", "deprecated": deprecated, "migrated_to": migrated, "category": category, "migration_target": map[string]any{"id": migrated, "paths": []string{"core://edu-agent/x"}, "content_snippet": "discarded"}}
}
func orphanDetailResponse(id int64, deprecated bool, migrated int64, category string) map[string]any {
	return map[string]any{"id": id, "node_uuid": testNodeID, "content": "discarded", "created_at": "2026-09-01T00:00:00Z", "deprecated": deprecated, "migrated_to": migrated, "category": category, "migration_target": map[string]any{"id": migrated, "paths": []string{"core://edu-agent/x"}, "content": "discarded", "created_at": "2026-09-01T00:00:00Z"}}
}
func writeJSON(w http.ResponseWriter, value any) { _ = json.NewEncoder(w).Encode(value) }

type protocolStore struct {
	work                         memory.DeliveryWork
	decisions                    []outbox.ApplyDecision
	loads                        int
	attempt                      memory.Attempt
	finalized                    memory.AttemptOutcome
	authorized                   memory.AttemptRetryAuthorization
	savedPlan                    memory.RemoteDeletePlan
	remote                       *scriptedRemote
	maintenance                  memory.ExpiryReconciliation
	maintenanceTransition        memory.ReconciliationTransition
	maintenanceFinalization      memory.ReconciliationFinalization
	maintenanceAuths             []memory.MaintenanceAuthorization
	maintenanceSaveCount         int
	maintenanceClaimCount        int
	maintenanceHistoricalPending int64
}

func newProtocolStore() *protocolStore {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	content := "Prefer concise examples"
	policy := memory.DeliveryPolicy{
		CandidateID: testCandidateID, Source: memory.SourceUserStatement,
		Category: memory.CategoryInteractionPreference, Sensitivity: memory.SensitivityNonSensitive,
		Stability: memory.StabilityStable, PolicyVersion: memory.AdmissionPolicyVersion,
		ContentHash: memory.SHA256String(content), AdmissionDecision: memory.CandidateDecision{
			ID: "10000000-0000-4000-8000-000000000003", CandidateID: testCandidateID, Revision: 2,
			Decision: memory.DecisionAdmit, Reason: "automatic_policy_match", ActorKind: "system",
			OperationID: "10000000-0000-4000-8000-000000000004", RequestHash: strings.Repeat("a", 64), CreatedAt: now,
		},
	}
	return &protocolStore{work: memory.DeliveryWork{Delivery: memory.Delivery{ID: testDeliveryID, Kind: memory.DeliveryAdmit, LogicalMemoryID: testLogicalID, RecordRevisionID: testRecordID, RecordRevision: 1, LearnerGeneration: 1, RecordGeneration: 1, PayloadID: testPayloadID, PayloadHash: memory.SHA256String(content), ExternalURI: memory.DeterministicExternalURI(testLogicalID), AttemptState: memory.AttemptPrepared, Status: memory.DeliveryStatusQueued, PublicStatus: memory.DeliveryQueued, ValidUntil: now.Add(time.Hour), ReceiptID: testReceiptID, CreatedAt: now, UpdatedAt: now}, Policy: policy, Content: content, CurrentGeneration: memory.Generation{LearnerGeneration: 1, MemoryGeneration: 1, ReadOpen: true, WriteOpen: true, UpdatedAt: now}}, attempt: memory.Attempt{ID: testAttemptID, DeliveryID: testDeliveryID, AttemptToken: testAttemptToken, State: memory.AttemptPrepared, LeaseToken: testLeaseToken, LeaseExpiresAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now}}
}
func (s *protocolStore) LoadDeliveryWork(_ context.Context, _ memory.OutboxIntent) (memory.DeliveryWork, outbox.ApplyDecision, error) {
	d := outbox.ApplyDecision{Apply: true}
	if s.loads < len(s.decisions) {
		d = s.decisions[s.loads]
	}
	s.loads++
	return s.work, d, nil
}
func (s *protocolStore) LoadDeliveryWorkByID(context.Context, string) (memory.DeliveryWork, error) {
	return s.work, nil
}
func (s *protocolStore) ClaimAttempt(context.Context, string, time.Time, time.Duration) (memory.Attempt, error) {
	return s.attempt, nil
}
func (s *protocolStore) ClaimUnknownAttempt(context.Context, time.Time, time.Duration) (memory.Attempt, error) {
	return s.attempt, nil
}
func (s *protocolStore) TransitionAttempt(_ context.Context, v memory.AttemptTransition) (memory.Attempt, error) {
	s.attempt.State = v.To
	s.attempt.BootEpoch = v.BootEpoch
	return s.attempt, nil
}
func (s *protocolStore) AuthorizeAttemptRetry(_ context.Context, v memory.AttemptRetryAuthorization) (memory.Attempt, error) {
	s.authorized = v
	s.attempt.State = memory.AttemptPrepared
	s.attempt.ID = "50000000-0000-4000-8000-000000000002"
	s.attempt.AttemptToken = "60000000-0000-4000-8000-000000000002"
	s.attempt.LeaseToken = "70000000-0000-4000-8000-000000000002"
	return s.attempt, nil
}
func (s *protocolStore) PermanentlyRejectDelivery(_ context.Context, _ memory.PolicyRejection) error {
	s.work.Content = ""
	s.work.Delivery.Status = memory.DeliveryStatusPermanentlyRejected
	return nil
}
func (s *protocolStore) FinalizeAttempt(_ context.Context, v memory.AttemptOutcome) (memory.Attempt, error) {
	s.finalized = v
	s.attempt.State = memory.AttemptConfirmed
	s.work.Content = ""
	s.work.Delivery.Status = memory.DeliveryStatusApplied
	return s.attempt, nil
}
func (*protocolStore) FenceDelivery(context.Context, string, string, time.Time) error { return nil }
func (*protocolStore) ClaimExpiryReconciliation(context.Context, time.Time, time.Duration) (memory.ExpiryReconciliation, error) {
	return memory.ExpiryReconciliation{}, &memory.Error{Code: memory.CodeNotFound}
}
func (*protocolStore) TransitionExpiryReconciliation(context.Context, memory.ReconciliationTransition) (memory.ExpiryReconciliation, error) {
	return memory.ExpiryReconciliation{}, nil
}
func (*protocolStore) FinalizeExpiryReconciliation(context.Context, memory.ReconciliationFinalization) (memory.ExpiryReconciliation, error) {
	return memory.ExpiryReconciliation{}, nil
}
func (s *protocolStore) SaveRemoteDeletePlan(_ context.Context, p memory.RemoteDeletePlan) (memory.RemoteDeletePlan, error) {
	s.savedPlan = p
	if s.remote != nil {
		s.remote.calls = append(s.remote.calls, "save_observed")
	}
	return p, nil
}
func (s *protocolStore) LoadRemoteDeletePlan(context.Context, string) (memory.RemoteDeletePlan, error) {
	if s.savedPlan.ID == "" {
		return memory.RemoteDeletePlan{}, &memory.Error{Code: memory.CodeNotFound}
	}
	return s.savedPlan, nil
}
func (s *protocolStore) ClaimMaintenanceExpiryReconciliation(_ context.Context, auth memory.MaintenanceAuthorization, _ time.Time, _ time.Duration) (memory.ExpiryReconciliation, error) {
	s.maintenanceAuths = append(s.maintenanceAuths, auth)
	if s.maintenance.ID == "" || s.maintenance.Status == memory.ReconciliationAbsenceVerified || s.maintenance.Status == memory.ReconciliationVerified || s.maintenance.Status == memory.ReconciliationConflict {
		return memory.ExpiryReconciliation{}, &memory.Error{Code: memory.CodeNotFound}
	}
	s.maintenanceClaimCount++
	if s.maintenance.Status == memory.ReconciliationPending {
		s.maintenance.Status = memory.ReconciliationReconciling
	}
	s.maintenance.LeaseToken = testReconciliationLeaseID
	return s.maintenance, nil
}
func (s *protocolStore) TransitionMaintenanceExpiryReconciliation(_ context.Context, auth memory.MaintenanceAuthorization, input memory.ReconciliationTransition) (memory.ExpiryReconciliation, error) {
	s.maintenanceAuths = append(s.maintenanceAuths, auth)
	s.maintenanceTransition = input
	s.maintenance.Status = input.To
	return s.maintenance, nil
}
func (s *protocolStore) FinalizeMaintenanceExpiryReconciliation(_ context.Context, auth memory.MaintenanceAuthorization, input memory.ReconciliationFinalization) (memory.ExpiryReconciliation, error) {
	s.maintenanceAuths = append(s.maintenanceAuths, auth)
	s.maintenanceFinalization = input
	switch input.Result {
	case memory.ReconciliationAbsenceResult:
		s.maintenance.Status = memory.ReconciliationAbsenceVerified
	case memory.ReconciliationDeleteResult:
		s.maintenance.Status = memory.ReconciliationVerified
		s.maintenanceHistoricalPending = 0
	case memory.ReconciliationConflictResult:
		s.maintenance.Status = memory.ReconciliationConflict
	}
	return s.maintenance, nil
}
func (s *protocolStore) LoadMaintenanceRemoteDeletePlan(_ context.Context, auth memory.MaintenanceAuthorization, erasureDeliveryID string) (memory.RemoteDeletePlan, error) {
	s.maintenanceAuths = append(s.maintenanceAuths, auth)
	if s.savedPlan.ID == "" || s.savedPlan.ErasureDeliveryID != erasureDeliveryID {
		return memory.RemoteDeletePlan{}, &memory.Error{Code: memory.CodeNotFound}
	}
	return s.savedPlan, nil
}
func (s *protocolStore) SaveMaintenanceRemoteDeletePlan(_ context.Context, auth memory.MaintenanceAuthorization, plan memory.RemoteDeletePlan) (memory.RemoteDeletePlan, error) {
	s.maintenanceAuths = append(s.maintenanceAuths, auth)
	s.maintenanceSaveCount++
	if s.savedPlan.ID == "" {
		s.savedPlan = plan
	}
	if s.remote != nil {
		s.remote.calls = append(s.remote.calls, "save_maintenance_observed")
	}
	return s.savedPlan, nil
}
func (s *protocolStore) MaintenanceReconciliationSummary(_ context.Context, auth memory.MaintenanceAuthorization) (memory.MaintenanceReconciliationSummary, error) {
	s.maintenanceAuths = append(s.maintenanceAuths, auth)
	summary := memory.MaintenanceReconciliationSummary{Pending: s.maintenanceHistoricalPending}
	switch s.maintenance.Status {
	case memory.ReconciliationPending, memory.ReconciliationReconciling, memory.ReconciliationDeletePending:
		summary.Pending++
	case memory.ReconciliationConflict:
		summary.Conflicts = 1
	}
	return summary, nil
}

func (*scriptedRemote) EnsureParent(context.Context) error { return nil }

type scriptedRemote struct {
	calls                                                           []string
	boot, nodeContent                                               string
	createApplyThenLose, createContractDrift, readbackContractDrift bool
	createCalls, activeMemoryID, permanentCalls, globalClearCalls   int64
	references                                                      memory.RemoteReferences
	activeDelete409, pathDeleted                                    bool
	deletedMemoryIDs                                                map[int64]bool
}

func (r *scriptedRemote) Health(context.Context) error { return nil }
func (r *scriptedRemote) Capabilities(context.Context) (memory.NocturneCapabilities, error) {
	r.calls = append(r.calls, "capabilities")
	return memory.NocturneCapabilities{UpstreamCommit: memory.NocturneUpstreamCommit, CompatRevision: memory.NocturneCompatRevision, BootEpoch: r.boot}, nil
}
func (r *scriptedRemote) GetNode(_ context.Context, _ string) (memory.RemoteNode, error) {
	r.calls = append(r.calls, "get")
	if r.readbackContractDrift && r.createCalls > 0 {
		return memory.RemoteNode{}, &Error{category: CategoryContractMismatch, operation: "get"}
	}
	if r.nodeContent == "" {
		return memory.RemoteNode{}, &Error{category: CategoryNotFound, operation: "get"}
	}
	return memory.RemoteNode{NodeID: testNodeID, Path: testParent + "/" + testLogicalID, URI: testDomain + "://" + testParent + "/" + testLogicalID, Content: r.nodeContent}, nil
}
func (r *scriptedRemote) CreateNode(_ context.Context, _ string, content string) (memory.RemoteMutation, error) {
	r.calls = append(r.calls, "create")
	r.createCalls++
	r.nodeContent = content
	if r.createContractDrift {
		return memory.RemoteMutation{}, &Error{category: CategoryContractMismatch, operation: "create", mutationDispatched: true}
	}
	if r.createApplyThenLose {
		return memory.RemoteMutation{}, &Error{category: CategoryTransport, operation: "create"}
	}
	return memory.RemoteMutation{URI: testDomain + "://x", MemoryID: r.activeMemoryID}, nil
}
func (r *scriptedRemote) UpdateNode(context.Context, string, string) (memory.RemoteMutation, error) {
	r.calls = append(r.calls, "update")
	return memory.RemoteMutation{MemoryID: r.activeMemoryID}, nil
}
func (r *scriptedRemote) DeletePath(context.Context, string) error {
	r.calls = append(r.calls, "delete_path")
	r.nodeContent = ""
	r.pathDeleted = true
	return nil
}
func (r *scriptedRemote) Search(context.Context, string) ([]memory.RemoteSearchResult, error) {
	r.calls = append(r.calls, "search")
	return []memory.RemoteSearchResult{}, nil
}
func (r *scriptedRemote) ListOrphans(context.Context) ([]memory.RemoteOrphan, error) {
	r.calls = append(r.calls, "list_orphans")
	return nil, nil
}
func (r *scriptedRemote) OrphanDetail(_ context.Context, id int64) (memory.RemoteOrphan, error) {
	r.calls = append(r.calls, fmt.Sprintf("orphan_%d", id))
	if r.deletedMemoryIDs[id] {
		return memory.RemoteOrphan{}, &Error{category: CategoryNotFound, operation: "orphan"}
	}
	return memory.RemoteOrphan{MemoryID: id, NodeID: testNodeID, Deprecated: true}, nil
}
func (r *scriptedRemote) PermanentDelete(_ context.Context, id int64) (memory.RemoteDeleteResult, error) {
	r.calls = append(r.calls, fmt.Sprintf("permanent_%d", id))
	if r.deletedMemoryIDs[id] {
		return memory.RemoteDeleteResult{}, &Error{category: CategoryNotFound, operation: "permanent"}
	}
	r.permanentCalls++
	if id == r.activeMemoryID && r.activeDelete409 {
		return memory.RemoteDeleteResult{}, &Error{category: CategoryActive, operation: "permanent"}
	}
	if r.deletedMemoryIDs == nil {
		r.deletedMemoryIDs = make(map[int64]bool)
	}
	r.deletedMemoryIDs[id] = true
	return memory.RemoteDeleteResult{DeletedMemoryID: id}, nil
}
func (r *scriptedRemote) References(context.Context, string) (memory.RemoteReferences, error) {
	r.calls = append(r.calls, "references")
	refs := r.references
	if refs.NodeID == "" {
		refs = memory.RemoteReferences{NodeID: testNodeID, Complete: true, ActiveMemoryID: r.activeMemoryID, MemoryIDs: []int64{int64(r.activeMemoryID)}, Paths: []memory.RemotePathReference{{Namespace: testNamespace, Domain: testDomain, Path: testParent + "/" + testLogicalID, URI: testDomain + "://" + testParent + "/" + testLogicalID}}}
	}
	allDeleted := r.pathDeleted && len(refs.MemoryIDs) > 0
	for _, id := range refs.MemoryIDs {
		allDeleted = allDeleted && r.deletedMemoryIDs[id]
	}
	if allDeleted {
		return memory.RemoteReferences{}, &Error{category: CategoryNotFound, operation: "references"}
	}
	return refs, nil
}
func (r *scriptedRemote) ClearReviewReferences(context.Context, string) error {
	r.calls = append(r.calls, "clear_review")
	return nil
}
func (*scriptedRemote) Backups(context.Context) (memory.BackupInventory, error) {
	return memory.BackupInventory{Validated: true, ManifestSHA256: strings.Repeat("0", 64)}, nil
}
func (*scriptedRemote) PruneBackups(_ context.Context, request memory.BackupPruneRequest) (memory.BackupPruneResult, error) {
	return memory.BackupPruneResult{OperationID: request.OperationID, DeletedPaths: append([]string(nil), request.Paths...), ManifestSHA256: strings.Repeat("1", 64)}, nil
}
func testConsumer(t *testing.T, store *protocolStore, remote *scriptedRemote) *Consumer {
	t.Helper()
	store.remote = remote
	c, err := NewConsumer(store, remote, ConsumerOptions{Lease: time.Minute, Namespace: testNamespace, Domain: testDomain, ParentPath: testParent, Now: func() time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func testMessage(t *testing.T, work memory.DeliveryWork) outbox.Message {
	t.Helper()
	payload, err := memory.MemoryOutboxPayload(memory.OutboxIntent{DeliveryID: work.Delivery.ID, PayloadHash: work.Delivery.PayloadHash, RecordRevision: work.Delivery.RecordRevision, LearnerGeneration: work.Delivery.LearnerGeneration, RecordGeneration: work.Delivery.RecordGeneration})
	if err != nil {
		t.Fatal(err)
	}
	return outbox.Message{BusinessType: "memory.delivery", AggregateID: work.Delivery.LogicalMemoryID, IdempotencyKey: "memory.delivery:" + work.Delivery.ID, Generation: work.Delivery.LearnerGeneration, Payload: payload}
}
func indexOf(values []string, target string) int {
	for i, value := range values {
		if value == target {
			return i
		}
	}
	return -1
}
