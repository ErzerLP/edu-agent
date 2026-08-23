package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

const (
	testDevice    = "10000000-0000-4000-8000-000000000001"
	testOperation = "20000000-0000-4000-8000-000000000001"
	testHash      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func candidateFixture(now time.Time) Candidate {
	id := "30000000-0000-4000-8000-000000000001"
	return Candidate{
		ID: id, URI: CandidateURI(id), PayloadID: "40000000-0000-4000-8000-000000000001",
		ContentHash: SHA256String("Prefer concise examples"), Source: SourceUserStatement,
		ProposerID: testDevice, Reason: "user preference", Category: CategoryInteractionPreference,
		Sensitivity: SensitivityNonSensitive, Stability: StabilityStable, ValidUntil: now.Add(time.Hour),
		PolicyVersion: AdmissionPolicyVersion, Status: CandidatePending, Revision: 1, CreatedAt: now,
	}
}

func TestAdmissionMatrix(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	base := candidateFixture(now)
	tests := []struct {
		name    string
		content string
		edit    func(*Candidate)
		want    CandidateStatus
	}{
		{name: "user stable preference", content: "Prefer concise examples", want: CandidateAdmitted},
		{name: "Chinese preference", content: "我偏好简洁的分步解释", want: CandidateAdmitted},
		{name: "time constraint", content: "I can study on weekday evenings", edit: func(c *Candidate) { c.Category = CategoryTimeConstraint }, want: CandidateAdmitted},
		{name: "Chinese time constraint", content: "我每周二晚上有空学习", edit: func(c *Candidate) { c.Category = CategoryTimeConstraint }, want: CandidateAdmitted},
		{name: "category masquerades answer", content: "Complete answer: x=42", want: CandidateRejected},
		{name: "category masquerades transcript", content: "User: hello\nAssistant: saved transcript", want: CandidateRejected},
		{name: "category masquerades learning truth", content: "Learning goal: pass calculus", want: CandidateRejected},
		{name: "category masquerades credential", content: "My API key is secret-value", want: CandidateRejected},
		{name: "Chinese forbidden answer", content: "参考答案：第一题选 A", want: CandidateRejected},
		{name: "ambiguous preference", content: "Sometimes I study late", want: CandidatePending},
		{name: "model inference", content: "Prefer concise examples", edit: func(c *Candidate) { c.Source = SourceModelInference }, want: CandidatePending},
		{name: "long term background", content: "Prefer concise examples", edit: func(c *Candidate) { c.Source = SourceLongTermBackground }, want: CandidatePending},
		{name: "generated summary", content: "Prefer concise examples", edit: func(c *Candidate) { c.Source = SourceGeneratedSummary }, want: CandidatePending},
		{name: "sensitive", content: "Prefer concise examples", edit: func(c *Candidate) { c.Sensitivity = SensitivitySensitive }, want: CandidatePending},
		{name: "transient", content: "Prefer concise examples", edit: func(c *Candidate) { c.Stability = StabilityTransient }, want: CandidatePending},
		{name: "other category", content: "Prefer concise examples", edit: func(c *Candidate) { c.Category = CategoryPersonalContext }, want: CandidatePending},
		{name: "expired", content: "Prefer concise examples", edit: func(c *Candidate) { c.ValidUntil = now }, want: CandidatePending},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			if test.edit != nil {
				test.edit(&value)
			}
			if got := EvaluateAdmission(value, test.content, now); got != test.want {
				t.Fatalf("status=%s want=%s", got, test.want)
			}
		})
	}
}

func TestForbiddenBusinessTruthRejectedAtBothGates(t *testing.T) {
	for _, category := range []Category{
		CategoryRawChat, CategoryCompleteAttempt, CategoryQuestionOrRubric, CategoryGoal, CategoryRoute,
		CategoryMastery, CategoryEvidence, CategoryMisconception, CategoryReviewQueue, CategorySyncState,
		CategoryDeviceToken, CategoryModelSecret, CategoryNocturneSecret,
	} {
		if err := ValidateProposedContent(category, "payload"); ErrorCode(err) != CodeMemoryPolicyRejected {
			t.Errorf("candidate gate category=%s err=%v", category, err)
		}
		policy := deliveryPolicyFixture(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), "payload")
		policy.Category = category
		if err := ValidateDeliveryPayload(policy, "payload", SHA256String("payload")); ErrorCode(err) != CodeMemoryPolicyRejected {
			t.Errorf("delivery gate category=%s err=%v", category, err)
		}
	}
}

func TestStateMachinesAreClosed(t *testing.T) {
	if !CanTransitionCandidate(CandidatePending, DecisionAdmit) || CanTransitionCandidate(CandidateAdmitted, DecisionReject) {
		t.Fatal("candidate terminal state was mutable")
	}
	for _, transition := range [][2]AttemptState{
		{AttemptPrepared, AttemptSent}, {AttemptSent, AttemptUnknown}, {AttemptUnknown, AttemptReconciling},
		{AttemptReconciling, AttemptConfirmed}, {AttemptPrepared, AttemptFenced},
	} {
		if !CanTransitionAttempt(transition[0], transition[1]) {
			t.Fatalf("attempt transition rejected: %v", transition)
		}
	}
	if CanTransitionAttempt(AttemptConfirmed, AttemptSent) || CanTransitionAttempt(AttemptUnknown, AttemptSent) {
		t.Fatal("terminal or unknown attempt could be resent")
	}
	if PublicDeliveryStatus(DeliveryStatusExpiryReconciling) != DeliveryQueued || PublicDeliveryStatus(DeliveryStatusFenced) != DeliveryRejected {
		t.Fatal("delivery public state mapping is not stable")
	}
}

func TestDomainStateValidationAndTransitionMatrices(t *testing.T) {
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)

	candidateStatuses := []CandidateStatus{CandidatePending, CandidateAdmitted, CandidateRejected, CandidateExpired}
	for _, status := range candidateStatuses {
		t.Run("candidate_validate_"+string(status), func(t *testing.T) {
			candidate := candidateFixture(now)
			candidate.Status = status
			if status != CandidatePending {
				candidate.Revision = 2
			}
			if err := candidate.Validate(); err != nil {
				t.Fatalf("status=%s: %v", status, err)
			}
		})
	}
	invalidCandidate := candidateFixture(now)
	invalidCandidate.Status = CandidateStatus("invalid")
	if err := invalidCandidate.Validate(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid candidate status accepted: %v", err)
	}
	candidateTransitions := map[[2]string]bool{
		{string(CandidatePending), string(DecisionAdmit)}:  true,
		{string(CandidatePending), string(DecisionReject)}: true,
		{string(CandidatePending), string(DecisionExpire)}: true,
	}
	decisions := []Decision{DecisionAdmit, DecisionReject, DecisionExpire, Decision("invalid")}
	for _, from := range append(candidateStatuses, CandidateStatus("invalid")) {
		for _, decision := range decisions {
			want := candidateTransitions[[2]string{string(from), string(decision)}]
			if got := CanTransitionCandidate(from, decision); got != want {
				t.Errorf("candidate transition %s/%s=%v want=%v", from, decision, got, want)
			}
		}
	}
	for decision, want := range map[Decision]CandidateStatus{
		DecisionAdmit: CandidateAdmitted, DecisionReject: CandidateRejected, DecisionExpire: CandidateExpired,
	} {
		if got := CandidateStatusForDecision(decision); got != want {
			t.Errorf("decision %s status=%s want=%s", decision, got, want)
		}
	}
	if got := CandidateStatusForDecision(Decision("invalid")); got != "" {
		t.Errorf("invalid decision derived status %q", got)
	}

	recordStatuses := []RecordStatus{
		RecordQueued, RecordApplied, RecordPermanentlyRejected, RecordSuperseded, RecordDeletePending, RecordDeleted,
	}
	record := Record{
		LogicalMemoryID: "50000000-0000-4000-8000-000000000001",
		ID:              "50000000-0000-4000-8000-000000000002",
		Revision:        1, RecordGeneration: 1, LearnerGeneration: 1,
		CandidateID: "50000000-0000-4000-8000-000000000003",
		DeliveryID:  "50000000-0000-4000-8000-000000000004",
		ReceiptID:   "50000000-0000-4000-8000-000000000005",
		ContentHash: SHA256String("record body"), CreatedAt: now,
	}
	record.ExternalURI = DeterministicExternalURI(record.LogicalMemoryID)
	record.ExternalURIDigest = SHA256String(record.ExternalURI)
	for _, status := range recordStatuses {
		t.Run("record_validate_"+string(status), func(t *testing.T) {
			value := record
			value.Status = status
			if err := value.Validate(); err != nil {
				t.Fatalf("status=%s: %v", status, err)
			}
		})
	}
	invalidRecord := record
	invalidRecord.Status = RecordStatus("invalid")
	if err := invalidRecord.Validate(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid record status accepted: %v", err)
	}
	recordTransitions := map[[2]RecordStatus]bool{
		{RecordQueued, RecordApplied}: true, {RecordQueued, RecordPermanentlyRejected}: true,
		{RecordQueued, RecordDeletePending}: true, {RecordApplied, RecordSuperseded}: true,
		{RecordApplied, RecordDeletePending}: true, {RecordDeletePending, RecordDeleted}: true,
	}
	for _, from := range append(recordStatuses, RecordStatus("invalid")) {
		for _, to := range append(recordStatuses, RecordStatus("invalid")) {
			want := recordTransitions[[2]RecordStatus{from, to}]
			if got := CanTransitionRecord(from, to); got != want {
				t.Errorf("record transition %s/%s=%v want=%v", from, to, got, want)
			}
		}
	}

	deliveryStatuses := []DeliveryStatus{
		DeliveryStatusQueued, DeliveryStatusApplied, DeliveryStatusPermanentlyRejected, DeliveryStatusFenced,
		DeliveryStatusExpiryReconciling, DeliveryStatusExpired, DeliveryStatusDeletePending, DeliveryStatusDeleted,
	}
	publicByInternal := map[DeliveryStatus]DeliveryPublicStatus{
		DeliveryStatusQueued: DeliveryQueued, DeliveryStatusApplied: DeliveryApplied,
		DeliveryStatusPermanentlyRejected: DeliveryRejected, DeliveryStatusFenced: DeliveryRejected,
		DeliveryStatusExpiryReconciling: DeliveryQueued, DeliveryStatusExpired: DeliveryRejected,
		DeliveryStatusDeletePending: DeliveryQueued, DeliveryStatusDeleted: DeliveryRejected,
	}
	delivery := Delivery{
		ID: "60000000-0000-4000-8000-000000000001", Kind: DeliveryAdmit,
		LogicalMemoryID:  "60000000-0000-4000-8000-000000000002",
		RecordRevisionID: "60000000-0000-4000-8000-000000000003", RecordRevision: 1,
		LearnerGeneration: 1, RecordGeneration: 1,
		PayloadID: "60000000-0000-4000-8000-000000000004", PayloadHash: SHA256String("delivery body"),
		AttemptState: AttemptPrepared, ValidUntil: now.Add(time.Hour),
		ReceiptID: "60000000-0000-4000-8000-000000000005", CreatedAt: now, UpdatedAt: now,
	}
	delivery.ExternalURI = DeterministicExternalURI(delivery.LogicalMemoryID)
	for _, status := range deliveryStatuses {
		t.Run("delivery_validate_"+string(status), func(t *testing.T) {
			value := delivery
			value.Status = status
			value.PublicStatus = PublicDeliveryStatus(status)
			if value.PublicStatus != publicByInternal[status] {
				t.Fatalf("status=%s public=%s want=%s", status, value.PublicStatus, publicByInternal[status])
			}
			if err := value.Validate(); err != nil {
				t.Fatalf("status=%s: %v", status, err)
			}
		})
	}
	invalidDelivery := delivery
	invalidDelivery.Status = DeliveryStatus("invalid")
	invalidDelivery.PublicStatus = DeliveryPublicStatus("invalid")
	if err := invalidDelivery.Validate(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid delivery status accepted: %v", err)
	}
	deliveryTransitions := map[[2]DeliveryStatus]bool{
		{DeliveryStatusQueued, DeliveryStatusApplied}:             true,
		{DeliveryStatusQueued, DeliveryStatusPermanentlyRejected}: true,
		{DeliveryStatusQueued, DeliveryStatusFenced}:              true,
		{DeliveryStatusQueued, DeliveryStatusExpiryReconciling}:   true,
		{DeliveryStatusQueued, DeliveryStatusExpired}:             true,
		{DeliveryStatusApplied, DeliveryStatusFenced}:             true,
		{DeliveryStatusApplied, DeliveryStatusDeleted}:            true,
		{DeliveryStatusFenced, DeliveryStatusDeleted}:             true,
		{DeliveryStatusExpiryReconciling, DeliveryStatusExpired}:  true,
		{DeliveryStatusExpiryReconciling, DeliveryStatusFenced}:   true,
		{DeliveryStatusExpiryReconciling, DeliveryStatusDeleted}:  true,
		{DeliveryStatusDeletePending, DeliveryStatusDeleted}:      true,
		{DeliveryStatusDeletePending, DeliveryStatusFenced}:       true,
	}
	for _, from := range append(deliveryStatuses, DeliveryStatus("invalid")) {
		for _, to := range append(deliveryStatuses, DeliveryStatus("invalid")) {
			want := deliveryTransitions[[2]DeliveryStatus{from, to}]
			if got := CanTransitionDelivery(from, to); got != want {
				t.Errorf("delivery transition %s/%s=%v want=%v", from, to, got, want)
			}
		}
	}

	attemptStates := []AttemptState{
		AttemptPrepared, AttemptSent, AttemptUnknown, AttemptReconciling, AttemptConfirmed, AttemptFenced, AttemptFailed,
	}
	attempt := Attempt{
		ID: "70000000-0000-4000-8000-000000000001", DeliveryID: delivery.ID,
		AttemptToken:   "70000000-0000-4000-8000-000000000002",
		LeaseToken:     "70000000-0000-4000-8000-000000000003",
		LeaseExpiresAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now,
	}
	for _, state := range attemptStates {
		t.Run("attempt_validate_"+string(state), func(t *testing.T) {
			value := attempt
			value.State = state
			if err := value.Validate(); err != nil {
				t.Fatalf("state=%s: %v", state, err)
			}
		})
	}
	invalidAttempt := attempt
	invalidAttempt.State = AttemptState("invalid")
	if err := invalidAttempt.Validate(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid attempt state accepted: %v", err)
	}
	attemptTransitions := map[[2]AttemptState]bool{
		{AttemptPrepared, AttemptSent}: true, {AttemptPrepared, AttemptFenced}: true, {AttemptPrepared, AttemptFailed}: true,
		{AttemptSent, AttemptUnknown}: true, {AttemptSent, AttemptReconciling}: true,
		{AttemptSent, AttemptConfirmed}: true, {AttemptSent, AttemptFenced}: true,
		{AttemptUnknown, AttemptReconciling}: true, {AttemptUnknown, AttemptConfirmed}: true,
		{AttemptUnknown, AttemptFenced}: true, {AttemptReconciling, AttemptConfirmed}: true,
		{AttemptReconciling, AttemptFenced}: true,
	}
	for _, from := range append(attemptStates, AttemptState("invalid")) {
		for _, to := range append(attemptStates, AttemptState("invalid")) {
			want := attemptTransitions[[2]AttemptState{from, to}]
			if got := CanTransitionAttempt(from, to); got != want {
				t.Errorf("attempt transition %s/%s=%v want=%v", from, to, got, want)
			}
		}
	}

	for _, status := range []ReceiptStatus{
		ReceiptPending, ReceiptSucceeded, ReceiptPartial, ReceiptFailed, ReceiptUnknown, ReceiptNotApplicable, ReceiptUnsupported,
	} {
		receipt := Receipt{
			ID: "80000000-0000-4000-8000-000000000001", DeliveryID: delivery.ID, Version: 1,
			Status: status, Reason: "logical row removal or absence verified",
			VerificationMethod: "uri_hash_or_absence", EvidenceDigest: SHA256String("receipt evidence"), CreatedAt: now,
		}
		if err := receipt.Validate(); err != nil {
			t.Errorf("receipt status=%s: %v", status, err)
		}
		wording := strings.ToLower(receipt.Reason + " " + receipt.VerificationMethod)
		if strings.Contains(wording, "secure erase") || strings.Contains(wording, "securely erased") {
			t.Errorf("receipt overclaims physical erasure: %+v", receipt)
		}
	}
	invalidReceipt := Receipt{
		ID: "80000000-0000-4000-8000-000000000001", DeliveryID: delivery.ID, Version: 1,
		Status: ReceiptStatus("invalid"), Reason: "logical deletion", VerificationMethod: "uri_absence", CreatedAt: now,
	}
	if err := invalidReceipt.Validate(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid receipt status accepted: %v", err)
	}
}

func TestAdmissionReviewExpiryAndDeterministicIdentityBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		content string
		edit    func(*Candidate)
		want    CandidateStatus
	}{
		{name: "automatic exact boundary", content: "I prefer concise examples", want: CandidateAdmitted},
		{name: "sensitive review", content: "I prefer concise examples", edit: func(c *Candidate) { c.Sensitivity = SensitivitySensitive }, want: CandidatePending},
		{name: "inference review", content: "Durable inferred context", edit: func(c *Candidate) {
			c.Source = SourceModelInference
			c.Category = CategoryPersonalContext
			c.SourceReference = SourceReference{ModelID: "trusted-model", PromptRevision: "prompt-v1", SourceHashes: []string{testHash}}
		}, want: CandidatePending},
		{name: "background review", content: "Durable background context", edit: func(c *Candidate) {
			c.Source = SourceLongTermBackground
			c.Category = CategoryPersonalContext
		}, want: CandidatePending},
		{name: "summary review", content: "Generated learner summary", edit: func(c *Candidate) {
			c.Source = SourceGeneratedSummary
			c.Category = CategoryGeneratedSummary
			c.SourceReference = SourceReference{ModelID: "trusted-model", PromptRevision: "prompt-v1", SourceHashes: []string{testHash}}
		}, want: CandidatePending},
		{name: "expiry boundary", content: "I prefer concise examples", edit: func(c *Candidate) { c.ValidUntil = now }, want: CandidatePending},
		{name: "forbidden body", content: "Complete answer: reveal the grading rubric", want: CandidateRejected},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidate := candidateFixture(now)
			if test.edit != nil {
				test.edit(&candidate)
			}
			if test.name != "expiry boundary" {
				if err := candidate.Validate(); err != nil {
					t.Fatalf("fixture is not a valid candidate: %v", err)
				}
			}
			if got := EvaluateAdmission(candidate, test.content, now); got != test.want {
				t.Fatalf("status=%s want=%s", got, test.want)
			}
		})
	}

	logicalID := "90000000-0000-4000-8000-000000000001"
	uri := DeterministicExternalURI(logicalID)
	if uri != "nocturne://core/edu-agent/"+logicalID || uri != DeterministicExternalURI(logicalID) {
		t.Fatalf("external URI is not deterministic: %q", uri)
	}
	if SHA256String(uri) != SHA256String(DeterministicExternalURI(logicalID)) || SHA256String(uri) == SHA256String(uri+"x") {
		t.Fatal("URI hash is not deterministic and content-sensitive")
	}
	candidateID := "90000000-0000-4000-8000-000000000002"
	if got := CandidateURI(candidateID); got != "candidate://"+candidateID || got != CandidateURI(candidateID) {
		t.Fatalf("candidate URI is not deterministic: %q", got)
	}
}

func TestCanonicalOperationHashIsStableAndSensitive(t *testing.T) {
	left, err := CanonicalRequestHash(map[string]any{"b": 2, "a": "x"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := CanonicalRequestHash(struct {
		A string `json:"a"`
		B int    `json:"b"`
	}{A: "x", B: 2})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := CanonicalRequestHash(map[string]any{"a": "x", "b": 3})
	if err != nil {
		t.Fatal(err)
	}
	if left != right || left == changed {
		t.Fatalf("left=%s right=%s changed=%s", left, right, changed)
	}
}

func TestOutboxIntentContainsExactAllowlist(t *testing.T) {
	payload, err := MemoryOutboxPayload(OutboxIntent{
		DeliveryID:  "50000000-0000-4000-8000-000000000001",
		PayloadHash: testHash, RecordRevision: 1, LearnerGeneration: 1, RecordGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"delivery_id": true, "payload_hash": true, "record_revision": true,
		"learner_generation": true, "record_generation": true,
	}
	if len(fields) != len(want) {
		t.Fatalf("outbox keys=%v payload=%s", fields, payload)
	}
	for key := range fields {
		if !want[key] {
			t.Fatalf("outbox contains unexpected key %q: %s", key, payload)
		}
	}
	for _, invalid := range []string{
		strings.TrimSuffix(string(payload), "}") + `,"payload_id":"60000000-0000-4000-8000-000000000001"}`,
		string(payload) + `{}`,
	} {
		if _, err := DecodeOutboxIntent(json.RawMessage(invalid)); ErrorCode(err) != CodeInvalidRequest {
			t.Fatalf("strict decoder accepted %s err=%v", invalid, err)
		}
	}
}

func TestDeliveryPolicyRequiresCompleteTrustedMetadataAndManualProvenance(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	content := "Prefer concise examples"
	base := deliveryPolicyFixture(now, content)
	if err := ValidateDeliveryPayload(base, content, base.ContentHash); err != nil {
		t.Fatalf("valid automatic policy err=%v", err)
	}
	tests := []struct {
		name string
		edit func(*DeliveryPolicy)
	}{
		{name: "source", edit: func(v *DeliveryPolicy) { v.Source = SourceModelInference }},
		{name: "sensitivity", edit: func(v *DeliveryPolicy) { v.Sensitivity = SensitivitySensitive }},
		{name: "stability", edit: func(v *DeliveryPolicy) { v.Stability = StabilityTransient }},
		{name: "policy version", edit: func(v *DeliveryPolicy) { v.PolicyVersion = "memory-admission-v0" }},
		{name: "candidate hash", edit: func(v *DeliveryPolicy) { v.ContentHash = SHA256String("different") }},
		{name: "decision candidate", edit: func(v *DeliveryPolicy) { v.AdmissionDecision.CandidateID = "30000000-0000-4000-8000-000000000099" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			test.edit(&value)
			if err := ValidateDeliveryPayload(value, content, base.ContentHash); ErrorCode(err) != CodeMemoryPolicyRejected {
				t.Fatalf("err=%v", err)
			}
		})
	}

	manualContent := "My background is that I learn best after a demonstration"
	manual := deliveryPolicyFixture(now, manualContent)
	manual.Category = CategoryPersonalContext
	manual.AdmissionDecision.ActorKind = "device"
	manual.AdmissionDecision.ActorID = testDevice
	manual.AdmissionDecision.OperationID = testOperation
	manual.AdmissionDecision.RequestHash = testHash
	manual.AdmissionDecision.Reason = "reviewed non-sensitive context"
	if err := ValidateDeliveryPayload(manual, manualContent, manual.ContentHash); err != nil {
		t.Fatalf("valid manual provenance err=%v", err)
	}
	manual.AdmissionDecision.OperationID = ""
	if err := ValidateDeliveryPayload(manual, manualContent, manual.ContentHash); ErrorCode(err) != CodeMemoryPolicyRejected {
		t.Fatalf("missing manual provenance err=%v", err)
	}
	manual.AdmissionDecision.OperationID = testOperation
	manual.Sensitivity = SensitivitySensitive
	if err := ValidateDeliveryPayload(manual, manualContent, manual.ContentHash); err != nil {
		t.Fatalf("reviewed sensitive manual admission rejected err=%v", err)
	}

	for _, test := range []struct {
		name      string
		source    SourceKind
		category  Category
		reference SourceReference
	}{
		{name: "reviewed inference", source: SourceModelInference, category: CategoryPersonalContext,
			reference: SourceReference{ModelID: "configured-model", PromptRevision: "prompt-v7", SourceHashes: []string{testHash}}},
		{name: "reviewed background", source: SourceLongTermBackground, category: CategoryPersonalContext},
		{name: "reviewed summary", source: SourceGeneratedSummary, category: CategoryGeneratedSummary,
			reference: SourceReference{ModelID: "configured-model", PromptRevision: "prompt-v7", SourceHashes: []string{testHash}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := "Reviewed durable learner context"
			value := deliveryPolicyFixture(now, content)
			value.Source = test.source
			value.SourceReference = test.reference
			value.Category = test.category
			value.AdmissionDecision.ActorKind = "device"
			value.AdmissionDecision.ActorID = testDevice
			value.AdmissionDecision.OperationID = testOperation
			value.AdmissionDecision.RequestHash = testHash
			value.AdmissionDecision.Reason = "reviewed source"
			if err := ValidateDeliveryPayload(value, content, value.ContentHash); err != nil {
				t.Fatalf("manual policy rejected: %v", err)
			}
			value.AdmissionDecision.ActorKind = "system"
			value.AdmissionDecision.ActorID = ""
			if err := ValidateDeliveryPayload(value, content, value.ContentHash); ErrorCode(err) != CodeMemoryPolicyRejected {
				t.Fatalf("system admitted reviewed source: %v", err)
			}
		})
	}
}

func TestGeneratedSummarySourceCategoryPairIsExact(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	candidate := candidateFixture(now)
	candidate.Source = SourceGeneratedSummary
	candidate.SourceReference = SourceReference{
		ModelID: "configured-model", PromptRevision: "prompt-v7", SourceHashes: []string{testHash},
	}
	if err := candidate.Validate(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("summary source with preference category err=%v", err)
	}
	candidate.Category = CategoryGeneratedSummary
	if err := candidate.Validate(); err != nil {
		t.Fatalf("valid summary source/category err=%v", err)
	}
	candidate.Source = SourceModelInference
	if err := candidate.Validate(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("summary category with inference source err=%v", err)
	}
}

func deliveryPolicyFixture(now time.Time, content string) DeliveryPolicy {
	candidateID := "30000000-0000-4000-8000-000000000001"
	return DeliveryPolicy{
		CandidateID: candidateID, Source: SourceUserStatement, Category: CategoryInteractionPreference,
		Sensitivity: SensitivityNonSensitive, Stability: StabilityStable,
		PolicyVersion: AdmissionPolicyVersion, ContentHash: SHA256String(content),
		AdmissionDecision: CandidateDecision{
			ID: "30000000-0000-4000-8000-000000000002", CandidateID: candidateID, Revision: 2,
			Decision: DecisionAdmit, Reason: "automatic_policy_match", ActorKind: "system",
			OperationID: testOperation, RequestHash: testHash, CreatedAt: now,
		},
	}
}

func TestValidationRejectsNonCanonicalUUIDHashAndTime(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.FixedZone("not-utc", 0))
	value := candidateFixture(now)
	if err := value.Validate(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("non-UTC candidate err=%v", err)
	}
	value = candidateFixture(now.UTC())
	value.ContentHash = "A" + value.ContentHash[1:]
	if err := value.Validate(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("uppercase hash err=%v", err)
	}
}

type recordingStore struct {
	createCalls int
	recordCalls int
	plan        CreatePlan
	decision    DecisionPlan
	deletePlan  DeletePlan
	replay      ReplayPlan
	record      RecordView
}

func (s *recordingStore) CreateCandidate(_ context.Context, plan CreatePlan) (OperationResult, error) {
	s.createCalls++
	s.plan = plan
	return OperationResult{Candidate: CandidateView{Candidate: plan.Candidate}}, nil
}
func (s *recordingStore) DecideCandidate(_ context.Context, plan DecisionPlan) (OperationResult, error) {
	s.decision = plan
	return OperationResult{}, nil
}
func (s *recordingStore) DeleteRecord(_ context.Context, plan DeletePlan) (OperationResult, error) {
	s.deletePlan = plan
	return OperationResult{}, nil
}
func (s *recordingStore) ReplayDelivery(_ context.Context, plan ReplayPlan) (OperationResult, error) {
	s.replay = plan
	return OperationResult{}, nil
}
func (*recordingStore) Candidate(context.Context, string) (CandidateView, error) {
	return CandidateView{}, nil
}
func (s *recordingStore) Record(context.Context, string) (RecordView, error) {
	s.recordCalls++
	return s.record, nil
}
func (*recordingStore) ListCandidates(context.Context, PageRequest) (CandidatePage, error) {
	return CandidatePage{}, nil
}
func (*recordingStore) ListRecords(context.Context, PageRequest) (RecordPage, error) {
	return RecordPage{}, nil
}
func (*recordingStore) ExpireCandidates(context.Context, time.Time, int) (int, error) { return 0, nil }
func (*recordingStore) ExpireDeliveries(context.Context, time.Time, int) (int, error) { return 0, nil }

func TestServiceRejectsForbiddenContentBeforePersistence(t *testing.T) {
	store := &recordingStore{}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	service, err := NewService(store, ServiceOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CreateCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, CreateCandidateCommand{
		OperationID: testOperation,
		Content:     "entire chat", Reason: "archive",
		Category: CategoryRawChat, Sensitivity: SensitivityNonSensitive, Stability: StabilityStable, ValidUntil: now.Add(time.Hour),
	})
	if ErrorCode(err) != CodeMemoryPolicyRejected || store.createCalls != 0 {
		t.Fatalf("err=%v calls=%d", err, store.createCalls)
	}
}

func TestServiceAutomaticallyRejectsForbiddenBodyWithoutDelivery(t *testing.T) {
	store := &recordingStore{}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	service, err := NewService(store, ServiceOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.CreateCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, CreateCandidateCommand{
		OperationID: testOperation, Content: "Complete answer: copy the grading rubric", Reason: "remember this",
		Category: CategoryInteractionPreference, Sensitivity: SensitivityNonSensitive,
		Stability: StabilityStable, ValidUntil: now.Add(time.Hour),
	})
	if err != nil || store.createCalls != 1 || result.Candidate.Candidate.Status != CandidateRejected ||
		store.plan.AutomaticDecision == nil || store.plan.AutomaticDecision.Decision != DecisionReject ||
		store.plan.DeliveryID != "" || store.plan.DeliveryPayloadID != "" || store.plan.OutboxID != "" {
		t.Fatalf("result=%+v plan=%+v calls=%d err=%v", result, store.plan, store.createCalls, err)
	}
}

func TestServiceDeleteDeliveryUsesConfiguredTTL(t *testing.T) {
	now := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	store := &recordingStore{}
	service, err := NewService(store, ServiceOptions{Now: func() time.Time { return now }, DeliveryTTL: 90 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.DeleteRecord(context.Background(), DevicePrincipal{DeviceID: testDevice}, DeleteRecordCommand{
		OperationID:      "20000000-0000-4000-8000-000000000009",
		LogicalMemoryID:  "70000000-0000-4000-8000-000000000009",
		ExpectedRevision: 2, ExpectedRecordGeneration: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(90 * time.Minute); !store.deletePlan.ValidUntil.Equal(want) {
		t.Fatalf("delete valid_until=%s want=%s", store.deletePlan.ValidUntil, want)
	}
	if _, err := NewService(store, ServiceOptions{DeliveryTTL: -time.Second}); err == nil {
		t.Fatal("negative delivery TTL was accepted")
	}
}

func TestServiceCorrectionCandidateUsesSharedPolicyAndTargetHash(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	store := &recordingStore{}
	service, err := NewService(store, ServiceOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	target := "70000000-0000-4000-8000-000000000001"
	command := CreateCorrectionCandidateCommand{
		OperationID: testOperation, LogicalMemoryID: target,
		ExpectedRevision: 3, ExpectedRecordGeneration: 5,
		Content: "Prefer shorter worked examples", Reason: "explicit correction",
		Category: CategoryInteractionPreference, Sensitivity: SensitivityNonSensitive,
		Stability: StabilityStable, ValidUntil: now.Add(time.Hour),
	}
	if _, err := service.CreateCorrectionCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, command); err != nil {
		t.Fatal(err)
	}
	firstHash := store.plan.Operation.RequestHash
	if !store.plan.Correction || store.plan.Candidate.LogicalMemoryID != target ||
		store.plan.ExpectedRecordRevision != 3 || store.plan.ExpectedRecordGeneration != 5 ||
		store.plan.AutomaticDecision == nil || store.plan.Candidate.Status != CandidateAdmitted ||
		store.plan.LogicalMemoryID != target {
		t.Fatalf("automatic correction plan=%+v", store.plan)
	}
	command.OperationID = "20000000-0000-4000-8000-000000000002"
	if _, err := service.CreateCorrectionCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, command); err != nil {
		t.Fatal(err)
	}
	if store.plan.Operation.RequestHash != firstHash {
		t.Fatal("operation id changed correction request hash")
	}
	command.OperationID = "20000000-0000-4000-8000-000000000003"
	command.LogicalMemoryID = "70000000-0000-4000-8000-000000000002"
	if _, err := service.CreateCorrectionCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, command); err != nil {
		t.Fatal(err)
	}
	if store.plan.Operation.RequestHash == firstHash {
		t.Fatal("correction request hash did not cover logical memory target")
	}

	command.OperationID = "20000000-0000-4000-8000-000000000004"
	command.LogicalMemoryID = target
	command.Sensitivity = SensitivitySensitive
	if _, err := service.CreateCorrectionCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, command); err != nil {
		t.Fatal(err)
	}
	if store.plan.AutomaticDecision != nil || store.plan.Candidate.Status != CandidatePending ||
		store.plan.LogicalMemoryID != target || store.plan.RecordRevisionID != "" || store.plan.DeliveryID != "" {
		t.Fatalf("pending correction created admission state: %+v", store.plan)
	}
	pendingCandidateID := store.plan.Candidate.ID
	if _, err := service.DecideCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, DecideCandidateCommand{
		OperationID: "20000000-0000-4000-8000-000000000005", CandidateID: pendingCandidateID,
		ExpectedRevision: 1, ExpectedRecordRevision: 3, ExpectedRecordGeneration: 5,
		Decision: DecisionAdmit, Reason: "reviewed correction",
	}); err != nil {
		t.Fatal(err)
	}
	if store.decision.ExpectedRecordRevision != 3 || store.decision.ExpectedRecordGeneration != 5 {
		t.Fatalf("manual correction fence not forwarded: %+v", store.decision)
	}

	calls := store.createCalls
	command.OperationID = "20000000-0000-4000-8000-000000000006"
	command.Category = CategoryRoute
	if _, err := service.CreateCorrectionCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, command); ErrorCode(err) != CodeMemoryPolicyRejected || store.createCalls != calls {
		t.Fatalf("forbidden correction err=%v calls=%d/%d", err, store.createCalls, calls)
	}
}

func TestServiceRecordUsesReadPermitAndReturnsNoBody(t *testing.T) {
	manager := privacy.NewReadPermitManager()
	store := &recordingStore{record: RecordView{
		Record:  Record{LogicalMemoryID: "70000000-0000-4000-8000-000000000001"},
		Receipt: Receipt{Status: ReceiptPending},
	}}
	service, err := NewService(store, ServiceOptions{ReadPermits: manager})
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.Record(context.Background(), store.record.Record.LogicalMemoryID)
	if err != nil || store.recordCalls != 1 || view.Receipt.Status != ReceiptPending {
		t.Fatalf("record view=%+v calls=%d err=%v", view, store.recordCalls, err)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"content":`) || strings.Contains(string(encoded), `"proposed_content":`) {
		t.Fatalf("record detail exposed body: %s", encoded)
	}
	if err := manager.CloseAndDrain(context.Background(), 2, privacy.OwnerMemory); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Record(context.Background(), store.record.Record.LogicalMemoryID); ErrorCode(err) != CodeContentRedacted || store.recordCalls != 1 {
		t.Fatalf("closed read permit err=%v calls=%d", err, store.recordCalls)
	}
}

func TestServiceReplayDeliveryOwnsCanonicalOperation(t *testing.T) {
	store := &recordingStore{}
	service, err := NewService(store, ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	command := ReplayDeliveryCommand{
		OperationID: testOperation, DeliveryID: "80000000-0000-4000-8000-000000000001",
	}
	if _, err := service.ReplayDelivery(context.Background(), DevicePrincipal{DeviceID: testDevice}, command); err != nil {
		t.Fatal(err)
	}
	firstHash := store.replay.Operation.RequestHash
	if store.replay.Operation.Kind != OperationDeliveryReplay || store.replay.DeliveryID != command.DeliveryID {
		t.Fatalf("replay plan=%+v", store.replay)
	}
	command.OperationID = "20000000-0000-4000-8000-000000000002"
	if _, err := service.ReplayDelivery(context.Background(), DevicePrincipal{DeviceID: testDevice}, command); err != nil {
		t.Fatal(err)
	}
	if store.replay.Operation.RequestHash != firstHash {
		t.Fatal("operation id changed replay request hash")
	}
}

func TestServiceUsesTrustedPrincipalsAndOwnsRequestHash(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	store := &recordingStore{}
	model := ModelPrincipal{
		DeviceID: testDevice, ProposerID: "10000000-0000-4000-8000-000000000002",
		ModelID: "configured-model", PromptRevision: "prompt-v7",
	}
	service, err := NewService(store, ServiceOptions{Now: func() time.Time { return now }, ModelPrincipal: &model})
	if err != nil {
		t.Fatal(err)
	}
	command := CreateCandidateCommand{
		OperationID: testOperation, Content: "Sometimes I study late", Reason: "background",
		Category: CategoryPersonalContext, Sensitivity: SensitivityNonSensitive, Stability: StabilityStable,
		ValidUntil: now.Add(time.Hour),
	}
	if _, err := service.CreateCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, command); err != nil {
		t.Fatal(err)
	}
	firstHash := store.plan.Operation.RequestHash
	if store.plan.Operation.Kind != OperationCreateCandidate || store.plan.Candidate.Source != SourceUserStatement || store.plan.Candidate.ProposerID != testDevice {
		t.Fatalf("untrusted user metadata reached plan: %+v", store.plan)
	}
	command.OperationID = "20000000-0000-4000-8000-000000000002"
	if _, err := service.CreateCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, command); err != nil {
		t.Fatal(err)
	}
	if store.plan.Operation.RequestHash != firstHash {
		t.Fatalf("operation id changed canonical request hash: first=%s second=%s", firstHash, store.plan.Operation.RequestHash)
	}
	command.Reason = "changed"
	if _, err := service.CreateCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, command); err != nil {
		t.Fatal(err)
	}
	if store.plan.Operation.RequestHash == firstHash {
		t.Fatal("service request hash did not cover the frozen request body")
	}

	if _, err := service.CreateModelCandidate(context.Background(), CreateModelCandidateCommand{
		OperationID: "20000000-0000-4000-8000-000000000003", Content: "Generated learning preference",
		Source: SourceGeneratedSummary, SourceHashes: []string{testHash}, Reason: "summary",
		Category: CategoryGeneratedSummary, Sensitivity: SensitivityNonSensitive, Stability: StabilityStable,
		ValidUntil: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	candidate := store.plan.Candidate
	if candidate.ProposerID != model.ProposerID || candidate.SourceReference.ModelID != model.ModelID || candidate.SourceReference.PromptRevision != model.PromptRevision {
		t.Fatalf("model principal was not enforced: %+v", candidate)
	}

	if _, err := service.CreateModelCandidate(context.Background(), CreateModelCandidateCommand{
		OperationID: "20000000-0000-4000-8000-000000000005", Content: "Durable background context",
		Source: SourceLongTermBackground, Reason: "trusted background", Category: CategoryPersonalContext,
		Sensitivity: SensitivitySensitive, Stability: StabilityStable, ValidUntil: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if store.plan.Candidate.Source != SourceLongTermBackground || store.plan.Correction {
		t.Fatalf("trusted background plan=%+v", store.plan)
	}

	if _, err := service.CreateModelCandidate(context.Background(), CreateModelCandidateCommand{
		OperationID:      "20000000-0000-4000-8000-000000000006",
		LogicalMemoryID:  "30000000-0000-4000-8000-000000000006",
		ExpectedRevision: 2, ExpectedRecordGeneration: 2, Content: "Corrected inferred context",
		Source: SourceModelInference, SourceHashes: []string{testHash}, Reason: "trusted correction",
		Category: CategoryPersonalContext, Sensitivity: SensitivityNonSensitive,
		Stability: StabilityStable, ValidUntil: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if !store.plan.Correction || store.plan.Candidate.Source != SourceModelInference ||
		store.plan.LogicalMemoryID != "30000000-0000-4000-8000-000000000006" ||
		store.plan.ExpectedRecordRevision != 2 || store.plan.ExpectedRecordGeneration != 2 {
		t.Fatalf("trusted correction plan=%+v", store.plan)
	}

	if _, err := service.DecideCandidate(context.Background(), DevicePrincipal{DeviceID: testDevice}, DecideCandidateCommand{
		OperationID: "20000000-0000-4000-8000-000000000004", CandidateID: candidate.ID,
		ExpectedRevision: 1, Decision: DecisionReject, Reason: "reviewed",
	}); err != nil {
		t.Fatal(err)
	}
	if store.decision.Operation.Kind != OperationCandidateDecision || store.decision.Decision.ActorID != testDevice || store.decision.Decision.RequestHash != store.decision.Operation.RequestHash {
		t.Fatalf("decision actor/hash were not service-owned: %+v", store.decision)
	}
}
