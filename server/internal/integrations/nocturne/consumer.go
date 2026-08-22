package nocturne

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
)

type ConsumerOptions struct {
	Lease      time.Duration
	Namespace  string
	Domain     string
	ParentPath string
	Now        func() time.Time
}

type Consumer struct {
	store      memory.DeliveryProtocolPersistence
	remote     memory.NocturneRemote
	purger     *Purger
	lease      time.Duration
	namespace  string
	domain     string
	parentPath string
	now        func() time.Time
}

func NewConsumer(store memory.DeliveryProtocolPersistence, remote memory.NocturneRemote, options ConsumerOptions) (*Consumer, error) {
	if store == nil || remote == nil || options.Lease <= 0 || strings.TrimSpace(options.Namespace) == "" ||
		strings.TrimSpace(options.Domain) == "" || strings.Trim(options.ParentPath, "/") != options.ParentPath || options.ParentPath == "" {
		return nil, errors.New("valid memory store, Nocturne remote, lease, and fixed routing are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	purger, err := NewPurger(store, remote, options.Namespace, options.Domain, options.ParentPath, options.Now)
	if err != nil {
		return nil, err
	}
	return &Consumer{store: store, remote: remote, purger: purger, lease: options.Lease, namespace: options.Namespace,
		domain: options.Domain, parentPath: options.ParentPath, now: options.Now}, nil
}

func (c *Consumer) CanApply(ctx context.Context, message outbox.Message) (outbox.ApplyDecision, error) {
	intent, err := c.intent(message)
	if err != nil {
		return outbox.ApplyDecision{}, err
	}
	work, decision, err := c.store.LoadDeliveryWork(ctx, intent)
	if err != nil {
		return outbox.ApplyDecision{}, err
	}
	if err := c.validateMessageWork(message, intent, work); err != nil {
		return outbox.ApplyDecision{}, err
	}
	return decision, decision.Validate()
}

func (c *Consumer) Apply(ctx context.Context, message outbox.Message) error {
	intent, err := c.intent(message)
	if err != nil {
		return err
	}
	work, decision, err := c.store.LoadDeliveryWork(ctx, intent)
	if err != nil {
		return err
	}
	if err := c.validateMessageWork(message, intent, work); err != nil {
		return err
	}
	if !decision.Apply {
		return &protocolError{category: "delivery_not_applicable"}
	}
	if work.Delivery.Status == memory.DeliveryStatusApplied {
		return nil
	}
	if work.Delivery.Kind != memory.DeliveryAdmit && work.Delivery.Kind != memory.DeliveryCorrection && work.Delivery.Kind != memory.DeliveryDelete {
		return &protocolError{category: "unsupported_delivery_kind", permanent: true}
	}
	if (work.Delivery.Kind == memory.DeliveryAdmit || work.Delivery.Kind == memory.DeliveryCorrection) &&
		memory.ValidateDeliveryPayload(work.Policy, work.Content, work.Delivery.PayloadHash) != nil {
		policyErr := memory.ValidateDeliveryPayload(work.Policy, work.Content, work.Delivery.PayloadHash)
		if rejectErr := c.store.PermanentlyRejectDelivery(ctx, memory.PolicyRejection{
			DeliveryID: work.Delivery.ID, OutboxLeaseToken: message.LeaseToken, ReceiptID: uuid.NewString(),
			Reason: "adapter_policy_rejected", ErrorCategory: "adapter_policy_rejected", At: c.now().UTC(),
		}); rejectErr != nil {
			return rejectErr
		}
		return &protocolError{category: "adapter_policy_rejected", permanent: true, cause: errors.Join(outbox.ErrConsumerFinalized, policyErr)}
	}
	attempt, err := c.store.ClaimAttempt(ctx, work.Delivery.ID, c.now().UTC(), c.lease)
	if err != nil {
		return err
	}
	if attempt.State == memory.AttemptReconciling {
		if work.Delivery.Kind == memory.DeliveryDelete {
			return c.sendDelete(ctx, work, attempt)
		}
		return c.reconcile(ctx, work, attempt)
	}
	if attempt.State != memory.AttemptPrepared {
		return &protocolError{category: "unexpected_attempt_state"}
	}
	capabilities, err := c.remote.Capabilities(ctx)
	if err != nil {
		return err
	}
	attempt, err = c.store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: capabilities.BootEpoch, At: c.now().UTC(),
	})
	if err != nil {
		return err
	}
	if work.Delivery.Kind == memory.DeliveryDelete {
		return c.sendDelete(ctx, work, attempt)
	}
	return c.send(ctx, work, attempt)
}

func (c *Consumer) send(ctx context.Context, work memory.DeliveryWork, attempt memory.Attempt) error {
	if work.Delivery.Kind == memory.DeliveryAdmit || work.Delivery.Kind == memory.DeliveryCorrection {
		if err := c.remote.EnsureParent(ctx); err != nil {
			return c.afterMutationError(ctx, attempt, err, false)
		}
	}
	path := c.path(work.Delivery.LogicalMemoryID)
	node, err := c.remote.GetNode(ctx, path)
	if err == nil && memory.SHA256String(node.Content) == work.Delivery.PayloadHash {
		return c.finalizeApplied(ctx, work, attempt, node, 0, "preflight_hash_match")
	}
	if err != nil && !IsNotFound(err) {
		return c.afterMutationError(ctx, attempt, err, false)
	}
	var mutation memory.RemoteMutation
	switch work.Delivery.Kind {
	case memory.DeliveryAdmit:
		if err == nil {
			return c.finalizePermanent(ctx, attempt, "remote_content_conflict", CategoryValidation)
		}
		mutation, err = c.remote.CreateNode(ctx, work.Delivery.LogicalMemoryID, work.Content)
	case memory.DeliveryCorrection:
		if err != nil {
			return c.finalizePermanent(ctx, attempt, "correction_target_missing", CategoryNotFound)
		}
		if work.PreviousContentHash == "" || memory.SHA256String(node.Content) != work.PreviousContentHash {
			return c.finalizePermanent(ctx, attempt, "correction_base_hash_conflict", CategoryValidation)
		}
		mutation, err = c.remote.UpdateNode(ctx, path, work.Content)
	}
	if err != nil {
		return c.afterMutationError(ctx, attempt, err, MutationDispatched(err))
	}
	verified, err := c.remote.GetNode(ctx, path)
	if err != nil {
		return c.afterMutationError(ctx, attempt, err, true)
	}
	if memory.SHA256String(verified.Content) != work.Delivery.PayloadHash {
		return c.afterMutationError(ctx, attempt, contract("post_write_readback", errors.New("post-write content hash mismatch")), true)
	}
	return c.finalizeApplied(ctx, work, attempt, verified, mutation.MemoryID, "post_write_hash_verified")
}

func (c *Consumer) reconcile(ctx context.Context, work memory.DeliveryWork, attempt memory.Attempt) error {
	capabilities, err := c.remote.Capabilities(ctx)
	if err != nil {
		return err
	}
	path := c.path(work.Delivery.LogicalMemoryID)
	node, err := c.remote.GetNode(ctx, path)
	if err == nil {
		if memory.SHA256String(node.Content) == work.Delivery.PayloadHash {
			return c.finalizeApplied(ctx, work, attempt, node, 0, "unknown_hash_reconciled")
		}
		return &protocolError{category: "unknown_remote_hash_conflict"}
	}
	if !IsNotFound(err) {
		return err
	}
	if capabilities.BootEpoch == attempt.BootEpoch {
		return &protocolError{category: "same_boot_absence_unknown"}
	}
	second, secondErr := c.remote.GetNode(ctx, path)
	if secondErr == nil {
		if memory.SHA256String(second.Content) == work.Delivery.PayloadHash {
			return c.finalizeApplied(ctx, work, attempt, second, 0, "restart_second_read_hash_match")
		}
		return &protocolError{category: "restart_remote_hash_conflict"}
	}
	if !IsNotFound(secondErr) {
		return secondErr
	}
	evidence := memory.SHA256String(attempt.BootEpoch + "\x00" + capabilities.BootEpoch + "\x00" + path + "\x002")
	replacement, err := c.store.AuthorizeAttemptRetry(ctx, memory.AttemptRetryAuthorization{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: memory.AttemptReconciling, ObservedBootEpoch: capabilities.BootEpoch,
		AbsenceObservations: 2, EvidenceDigest: evidence, At: c.now().UTC(),
	})
	if err != nil {
		return err
	}
	replacement, err = c.store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: replacement.ID, AttemptToken: replacement.AttemptToken, LeaseToken: replacement.LeaseToken,
		From: memory.AttemptPrepared, To: memory.AttemptSent, BootEpoch: capabilities.BootEpoch, At: c.now().UTC(),
	})
	if err != nil {
		return err
	}
	return c.send(ctx, work, replacement)
}

func (c *Consumer) finalizeApplied(ctx context.Context, work memory.DeliveryWork, attempt memory.Attempt, node memory.RemoteNode, memoryID int64, reason string) error {
	if memoryID <= 0 {
		if work.ExternalNodeID == node.NodeID && work.ExternalMemoryID > 0 {
			memoryID = work.ExternalMemoryID
		} else {
			references, err := c.remote.References(ctx, node.NodeID)
			if err != nil {
				return err
			}
			if !references.Complete || references.ActiveMemoryID <= 0 {
				return &protocolError{category: "active_memory_id_unavailable"}
			}
			memoryID = references.ActiveMemoryID
		}
	}
	intent := memory.OutboxIntent{DeliveryID: work.Delivery.ID,
		PayloadHash: work.Delivery.PayloadHash, RecordRevision: work.Delivery.RecordRevision,
		LearnerGeneration: work.Delivery.LearnerGeneration, RecordGeneration: work.Delivery.RecordGeneration}
	latest, decision, err := c.store.LoadDeliveryWork(ctx, intent)
	if err != nil {
		return err
	}
	if !decision.Apply || latest.Delivery.Status != memory.DeliveryStatusQueued || latest.Delivery.ID != work.Delivery.ID {
		return outbox.ErrLeaseLost
	}
	resultDigest := memory.SHA256String(node.NodeID + fmt.Sprintf("\x00%d\x00%s", memoryID, work.Delivery.PayloadHash))
	_, err = c.store.FinalizeAttempt(ctx, memory.AttemptOutcome{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: attempt.State, Kind: memory.AttemptOutcomeApplied, ReceiptID: uuid.NewString(),
		ReceiptStatus: memory.ReceiptSucceeded, Reason: reason, VerificationMethod: "nocturne_uri_content_sha256",
		EvidenceDigest: work.Delivery.PayloadHash, ExternalNodeID: node.NodeID, ExternalMemoryID: memoryID,
		ResultDigest: resultDigest, At: c.now().UTC(),
	})
	return err
}

func (c *Consumer) afterMutationError(ctx context.Context, attempt memory.Attempt, remoteErr error, mutationDispatched bool) error {
	category := Category(remoteErr)
	if !mutationDispatched && (category == CategoryAuth || category == CategoryValidation || category == CategoryContractMismatch) {
		return c.finalizePermanent(ctx, attempt, "remote_"+string(category), category)
	}
	// A concrete 401/403/422 response proves that the mutation was rejected. Every other
	// failure after dispatch, including a malformed 2xx response, remains result-unknown.
	if category == CategoryAuth || category == CategoryValidation {
		return c.finalizePermanent(ctx, attempt, "remote_"+string(category), category)
	}
	_, err := c.store.TransitionAttempt(ctx, memory.AttemptTransition{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: attempt.State, To: memory.AttemptUnknown, ErrorCategory: string(category), At: c.now().UTC(),
	})
	if err != nil {
		return err
	}
	return remoteErr
}

func (c *Consumer) sendDelete(ctx context.Context, work memory.DeliveryWork, attempt memory.Attempt) error {
	err := c.purger.Purge(ctx, work.Delivery.ID, work.Delivery.LogicalMemoryID, work.Delivery.PayloadHash)
	if err != nil {
		if attempt.State == memory.AttemptSent {
			_, transitionErr := c.store.TransitionAttempt(ctx, memory.AttemptTransition{
				AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
				From: memory.AttemptSent, To: memory.AttemptUnknown, ErrorCategory: "delete_reconciliation_pending", At: c.now().UTC(),
			})
			if transitionErr != nil {
				return transitionErr
			}
		}
		return err
	}
	_, err = c.store.FinalizeAttempt(ctx, memory.AttemptOutcome{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: attempt.State, Kind: memory.AttemptOutcomeDeleted, ReceiptID: uuid.NewString(),
		ReceiptStatus: memory.ReceiptSucceeded, Reason: "remote_logical_delete_verified",
		VerificationMethod: "nocturne_complete_reference_absence", EvidenceDigest: work.Delivery.PayloadHash, At: c.now().UTC(),
	})
	return err
}

func (c *Consumer) finalizePermanent(ctx context.Context, attempt memory.Attempt, reason string, category ErrorCategory) error {
	_, err := c.store.FinalizeAttempt(ctx, memory.AttemptOutcome{
		AttemptID: attempt.ID, AttemptToken: attempt.AttemptToken, LeaseToken: attempt.LeaseToken,
		From: attempt.State, Kind: memory.AttemptOutcomePermanentlyRejected, ReceiptID: uuid.NewString(),
		ReceiptStatus: memory.ReceiptFailed, Reason: reason, VerificationMethod: "nocturne_fixed_contract",
		ErrorCategory: string(category), At: c.now().UTC(),
	})
	if err != nil {
		return err
	}
	return &protocolError{category: string(category), permanent: true}
}

func (c *Consumer) intent(message outbox.Message) (memory.OutboxIntent, error) {
	if message.BusinessType != "memory.delivery" {
		return memory.OutboxIntent{}, &protocolError{category: "invalid_business_type", permanent: true}
	}
	intent, err := memory.DecodeOutboxIntent(message.Payload)
	if err != nil {
		return intent, &protocolError{category: "invalid_outbox_payload", permanent: true, cause: err}
	}
	return intent, nil
}

func (c *Consumer) validateMessageWork(message outbox.Message, intent memory.OutboxIntent, work memory.DeliveryWork) error {
	if err := work.ValidateIntent(intent); err != nil {
		return &protocolError{category: "delivery_tuple_conflict", permanent: true, cause: err}
	}
	if message.AggregateID != work.Delivery.LogicalMemoryID || message.Generation != intent.LearnerGeneration ||
		message.IdempotencyKey != "memory.delivery:"+intent.DeliveryID {
		return &protocolError{category: "outbox_envelope_conflict", permanent: true}
	}
	if (work.Delivery.Kind == memory.DeliveryAdmit || work.Delivery.Kind == memory.DeliveryCorrection) &&
		!c.now().UTC().Before(work.Delivery.ValidUntil) {
		return &protocolError{category: "delivery_expired", permanent: true}
	}
	return nil
}

func (c *Consumer) path(logicalMemoryID string) string { return c.parentPath + "/" + logicalMemoryID }

type protocolError struct {
	category  string
	permanent bool
	cause     error
}

func (e *protocolError) Error() string {
	return "nocturne delivery protocol failed: category=" + e.category
}
func (e *protocolError) Unwrap() error    { return e.cause }
func (e *protocolError) Category() string { return e.category }
func (e *protocolError) Permanent() bool  { return e.permanent }

type WorkerError struct {
	Worker    string
	Category  string
	Permanent bool
	Err       error
}

func (e WorkerError) Error() string {
	return "nocturne worker failed: worker=" + e.Worker + " category=" + e.Category
}
func (e WorkerError) Unwrap() error { return e.Err }

type ErrorObserver func(WorkerError)

type AttemptReconciler struct {
	consumer *Consumer
	interval time.Duration
	onError  ErrorObserver
	errors   chan WorkerError
}

func NewAttemptReconciler(consumer *Consumer, interval time.Duration, observers ...ErrorObserver) (*AttemptReconciler, error) {
	if consumer == nil || interval <= 0 || len(observers) > 1 {
		return nil, errors.New("valid consumer, reconciliation interval, and at most one observer are required")
	}
	var observer ErrorObserver
	if len(observers) == 1 {
		observer = observers[0]
	}
	return &AttemptReconciler{consumer: consumer, interval: interval, onError: observer, errors: make(chan WorkerError, 16)}, nil
}
func (w *AttemptReconciler) Errors() <-chan WorkerError { return w.errors }
func (w *AttemptReconciler) RunOnce(ctx context.Context) (int, error) {
	attempt, err := w.consumer.store.ClaimUnknownAttempt(ctx, w.consumer.now().UTC(), w.consumer.lease)
	if memory.ErrorCode(err) == memory.CodeNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	intent := memory.OutboxIntent{DeliveryID: attempt.DeliveryID}
	work, err := loadWorkByDelivery(ctx, w.consumer.store, attempt.DeliveryID)
	if err != nil {
		return 1, err
	}
	intent = memory.OutboxIntent{DeliveryID: work.Delivery.ID,
		PayloadHash: work.Delivery.PayloadHash, RecordRevision: work.Delivery.RecordRevision,
		LearnerGeneration: work.Delivery.LearnerGeneration, RecordGeneration: work.Delivery.RecordGeneration}
	work, decision, err := w.consumer.store.LoadDeliveryWork(ctx, intent)
	if err != nil || !decision.Apply {
		return 1, firstError(err, outbox.ErrLeaseLost)
	}
	if (work.Delivery.Kind == memory.DeliveryAdmit || work.Delivery.Kind == memory.DeliveryCorrection) &&
		memory.ValidateDeliveryPayload(work.Policy, work.Content, work.Delivery.PayloadHash) != nil {
		err := memory.ValidateDeliveryPayload(work.Policy, work.Content, work.Delivery.PayloadHash)
		if rejectErr := w.consumer.store.PermanentlyRejectDelivery(ctx, memory.PolicyRejection{
			DeliveryID: work.Delivery.ID, ReceiptID: uuid.NewString(), Reason: "adapter_policy_rejected",
			ErrorCategory: "adapter_policy_rejected", At: w.consumer.now().UTC(),
		}); rejectErr != nil {
			return 1, rejectErr
		}
		return 1, &protocolError{category: "adapter_policy_rejected", permanent: true, cause: errors.Join(outbox.ErrConsumerFinalized, err)}
	}
	if work.Delivery.Kind == memory.DeliveryDelete {
		return 1, w.consumer.sendDelete(ctx, work, attempt)
	}
	return 1, w.consumer.reconcile(ctx, work, attempt)
}
func (w *AttemptReconciler) Run(ctx context.Context) error {
	return runLoop(ctx, "attempt_reconciler", w.interval, w.RunOnce, w.onError, w.errors)
}

// loadWorkByDelivery is intentionally a persistence-port operation. Implementations may resolve
// the immutable intent tuple without exposing tables to the adapter.
func loadWorkByDelivery(ctx context.Context, store memory.DeliveryProtocolPersistence, deliveryID string) (memory.DeliveryWork, error) {
	if loader, ok := store.(interface {
		LoadDeliveryWorkByID(context.Context, string) (memory.DeliveryWork, error)
	}); ok {
		return loader.LoadDeliveryWorkByID(ctx, deliveryID)
	}
	return memory.DeliveryWork{}, &protocolError{category: "delivery_lookup_port_unavailable", permanent: true}
}

type Purger struct {
	store            memory.DeliveryProtocolPersistence
	maintenanceStore memory.MaintenanceReconciliationPersistence
	remote           memory.NocturneRemote
	namespace        string
	domain           string
	parentPath       string
	now              func() time.Time
}

func NewPurger(store memory.DeliveryProtocolPersistence, remote memory.NocturneRemote, namespace, domain, parentPath string, now func() time.Time) (*Purger, error) {
	if store == nil || remote == nil || strings.TrimSpace(namespace) == "" || strings.TrimSpace(domain) == "" || strings.Trim(parentPath, "/") != parentPath || parentPath == "" {
		return nil, errors.New("valid delete persistence, remote, and fixed routing are required")
	}
	if now == nil {
		now = time.Now
	}
	maintenanceStore, _ := store.(memory.MaintenanceReconciliationPersistence)
	return &Purger{store: store, maintenanceStore: maintenanceStore, remote: remote, namespace: namespace, domain: domain, parentPath: parentPath, now: now}, nil
}

func (p *Purger) Purge(ctx context.Context, deliveryID, logicalMemoryID, expectedHash string) error {
	return p.purge(ctx, deliveryID, logicalMemoryID, expectedHash, nil)
}

func (p *Purger) PurgeMaintenance(ctx context.Context, auth memory.MaintenanceAuthorization, deliveryID, logicalMemoryID, expectedHash string) error {
	if err := auth.Validate(); err != nil {
		return err
	}
	if p.maintenanceStore == nil {
		return &protocolError{category: "maintenance_persistence_unavailable", permanent: true}
	}
	return p.purge(ctx, deliveryID, logicalMemoryID, expectedHash, func(ctx context.Context, plan memory.RemoteDeletePlan) (memory.RemoteDeletePlan, error) {
		return p.maintenanceStore.SaveMaintenanceRemoteDeletePlan(ctx, auth, plan)
	})
}

type remoteDeletePlanSaver func(context.Context, memory.RemoteDeletePlan) (memory.RemoteDeletePlan, error)

func (p *Purger) purge(ctx context.Context, deliveryID, logicalMemoryID, expectedHash string, maintenanceSave remoteDeletePlanSaver) error {
	plan, err := p.store.LoadRemoteDeletePlan(ctx, deliveryID)
	if memory.ErrorCode(err) == memory.CodeNotFound {
		path := p.parentPath + "/" + logicalMemoryID
		node, getErr := p.remote.GetNode(ctx, path)
		if getErr != nil {
			return getErr
		}
		if memory.SHA256String(node.Content) != expectedHash {
			return &protocolError{category: "delete_hash_conflict"}
		}
		references, refErr := p.remote.References(ctx, node.NodeID)
		if refErr != nil {
			return refErr
		}
		if !references.Complete || references.ActiveMemoryID <= 0 || !slicesContain(references.MemoryIDs, references.ActiveMemoryID) || len(references.Paths) == 0 {
			return &protocolError{category: "delete_references_incomplete"}
		}
		for _, ref := range references.Paths {
			if ref.Namespace != p.namespace || ref.Domain != p.domain || ref.URI != p.domain+"://"+ref.Path {
				return &protocolError{category: "delete_unmanaged_reference"}
			}
		}
		digest, digestErr := memory.RemoteDeleteSnapshotDigest(references)
		if digestErr != nil {
			return digestErr
		}
		plan = memory.RemoteDeletePlan{ID: uuid.NewString(), DeliveryID: deliveryID, NodeID: node.NodeID,
			ExternalURI: memory.DeterministicExternalURI(logicalMemoryID), ActiveMemoryID: references.ActiveMemoryID,
			MemoryIDs: references.MemoryIDs, Paths: references.Paths, ReviewCleanupNeeded: len(references.ReviewReferences) > 0,
			SnapshotDigest: digest, CreatedAt: p.now().UTC()}
		if maintenanceSave == nil {
			plan, err = p.store.SaveRemoteDeletePlan(ctx, plan)
		} else {
			plan, err = maintenanceSave(ctx, plan)
		}
	} else if err == nil && maintenanceSave != nil {
		// Re-authorize an existing durable plan before any maintenance mutation.
		plan, err = maintenanceSave(ctx, plan)
	}
	if err != nil {
		return err
	}
	for _, ref := range plan.Paths {
		if err := p.remote.DeletePath(ctx, ref.Path); err != nil && !IsNotFound(err) {
			return err
		}
	}
	active, err := p.remote.OrphanDetail(ctx, plan.ActiveMemoryID)
	if err != nil && (!IsNotFound(err) || maintenanceSave == nil) {
		return err
	}
	if err == nil && (active.NodeID != plan.NodeID || !active.Deprecated || active.MigratedTo != 0) {
		return &protocolError{category: "active_orphan_not_observed"}
	}
	refs, err := p.remote.References(ctx, plan.NodeID)
	if err != nil && (!IsNotFound(err) || maintenanceSave == nil) {
		return err
	}
	if err == nil && !refs.Complete {
		return &protocolError{category: "delete_references_incomplete"}
	}
	if err == nil && len(refs.ReviewReferences) > 0 {
		if err := p.remote.ClearReviewReferences(ctx, plan.NodeID); err != nil {
			return err
		}
		refs, err = p.remote.References(ctx, plan.NodeID)
		if err != nil || !refs.Complete || len(refs.ReviewReferences) > 0 {
			return firstError(err, &protocolError{category: "review_reference_remaining"})
		}
	}
	ids := append([]int64(nil), plan.MemoryIDs...)
	sort.Slice(ids, func(i, j int) bool {
		if ids[i] == plan.ActiveMemoryID {
			return false
		}
		if ids[j] == plan.ActiveMemoryID {
			return true
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		_, deleteErr := p.remote.PermanentDelete(ctx, id)
		if deleteErr != nil && !IsNotFound(deleteErr) {
			return deleteErr
		}
		if _, detailErr := p.remote.OrphanDetail(ctx, id); !IsNotFound(detailErr) {
			if detailErr != nil {
				return detailErr
			}
			return &protocolError{category: "deleted_orphan_still_present"}
		}
	}
	return p.verifyAbsent(ctx, plan, logicalMemoryID)
}

func (p *Purger) verifyAbsent(ctx context.Context, plan memory.RemoteDeletePlan, logicalMemoryID string) error {
	for _, ref := range plan.Paths {
		if _, err := p.remote.GetNode(ctx, ref.Path); !IsNotFound(err) {
			if err != nil {
				return err
			}
			return &protocolError{category: "browse_reference_remaining"}
		}
	}
	results, err := p.remote.Search(ctx, logicalMemoryID)
	if err != nil {
		return err
	}
	for _, result := range results {
		for _, ref := range plan.Paths {
			if result.URI == ref.URI || result.Path == ref.Path {
				return &protocolError{category: "search_reference_remaining"}
			}
		}
	}
	refs, err := p.remote.References(ctx, plan.NodeID)
	if err != nil && !IsNotFound(err) {
		return err
	}
	if err == nil && (!refs.Complete || refs.HasNonMemoryReferences() || len(refs.MemoryIDs) != 0 || refs.ActiveMemoryID != 0) {
		return &protocolError{category: "complete_reference_remaining"}
	}
	orphans, err := p.remote.ListOrphans(ctx)
	if err != nil {
		return err
	}
	for _, orphan := range orphans {
		if orphan.NodeID == plan.NodeID || slicesContain(plan.MemoryIDs, orphan.MemoryID) {
			return &protocolError{category: "global_orphan_remaining"}
		}
	}
	for _, id := range plan.MemoryIDs {
		if _, err := p.remote.OrphanDetail(ctx, id); !IsNotFound(err) {
			if err != nil {
				return err
			}
			return &protocolError{category: "orphan_detail_remaining"}
		}
	}
	return nil
}

type ExpiryReconciler struct {
	store    memory.DeliveryProtocolPersistence
	remote   memory.NocturneRemote
	purger   *Purger
	lease    time.Duration
	interval time.Duration
	now      func() time.Time
	onError  ErrorObserver
	errors   chan WorkerError
}

func NewExpiryReconciler(store memory.DeliveryProtocolPersistence, remote memory.NocturneRemote, purger *Purger, lease, interval time.Duration, now func() time.Time, observers ...ErrorObserver) (*ExpiryReconciler, error) {
	if store == nil || remote == nil || purger == nil || lease <= 0 || interval <= 0 || len(observers) > 1 {
		return nil, errors.New("valid expiry dependencies, lease, interval, and at most one observer are required")
	}
	if now == nil {
		now = time.Now
	}
	var observer ErrorObserver
	if len(observers) == 1 {
		observer = observers[0]
	}
	return &ExpiryReconciler{store: store, remote: remote, purger: purger, lease: lease, interval: interval, now: now, onError: observer, errors: make(chan WorkerError, 16)}, nil
}
func (w *ExpiryReconciler) Errors() <-chan WorkerError { return w.errors }
func (w *ExpiryReconciler) RunOnce(ctx context.Context) (int, error) {
	value, err := w.store.ClaimExpiryReconciliation(ctx, w.now().UTC(), w.lease)
	if memory.ErrorCode(err) == memory.CodeNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if value.Status == memory.ReconciliationDeletePending {
		if err := w.purger.Purge(ctx, value.DeliveryID, value.LogicalMemoryID, value.ContentHash); err != nil {
			return 1, err
		}
		_, err = w.store.FinalizeExpiryReconciliation(ctx, memory.ReconciliationFinalization{ReconciliationID: value.ID, LeaseToken: value.LeaseToken, From: memory.ReconciliationDeletePending, Result: memory.ReconciliationDeleteResult, ReceiptID: uuid.NewString(), Reason: "remote_logical_delete_verified", EvidenceDigest: value.ContentHash, At: w.now().UTC()})
		return 1, err
	}
	capabilities, err := w.remote.Capabilities(ctx)
	if err != nil {
		return 1, err
	}
	path := w.purger.parentPath + "/" + value.LogicalMemoryID
	node, err := w.remote.GetNode(ctx, path)
	if err == nil {
		if memory.SHA256String(node.Content) != value.ContentHash {
			return 1, w.finalizeConflict(ctx, value, "expiry_remote_hash_conflict")
		}
		if value.Status == memory.ReconciliationReconciling {
			value, err = w.store.TransitionExpiryReconciliation(ctx, memory.ReconciliationTransition{ReconciliationID: value.ID, LeaseToken: value.LeaseToken, From: value.Status, To: memory.ReconciliationDeletePending, At: w.now().UTC()})
			if err != nil {
				return 1, err
			}
		}
		if err := w.purger.Purge(ctx, value.DeliveryID, value.LogicalMemoryID, value.ContentHash); err != nil {
			return 1, err
		}
		_, err = w.store.FinalizeExpiryReconciliation(ctx, memory.ReconciliationFinalization{ReconciliationID: value.ID, LeaseToken: value.LeaseToken, From: memory.ReconciliationDeletePending, Result: memory.ReconciliationDeleteResult, ReceiptID: uuid.NewString(), Reason: "remote_logical_delete_verified", EvidenceDigest: value.ContentHash, At: w.now().UTC()})
		return 1, err
	}
	if !IsNotFound(err) {
		return 1, err
	}
	if capabilities.BootEpoch == value.SentBootEpoch {
		return 1, &protocolError{category: "same_boot_expiry_absence_unknown"}
	}
	if _, err := w.remote.GetNode(ctx, path); !IsNotFound(err) {
		if err != nil {
			return 1, err
		}
		return 1, &protocolError{category: "expiry_absence_not_stable"}
	}
	_, err = w.store.FinalizeExpiryReconciliation(ctx, memory.ReconciliationFinalization{ReconciliationID: value.ID, LeaseToken: value.LeaseToken, From: memory.ReconciliationReconciling, Result: memory.ReconciliationAbsenceResult, ReceiptID: uuid.NewString(), Reason: "new_boot_double_absence_verified", EvidenceDigest: memory.SHA256String(value.SentBootEpoch + "\x00" + capabilities.BootEpoch + "\x00" + path), At: w.now().UTC()})
	return 1, err
}
func (w *ExpiryReconciler) finalizeConflict(ctx context.Context, value memory.ExpiryReconciliation, reason string) error {
	_, err := w.store.FinalizeExpiryReconciliation(ctx, memory.ReconciliationFinalization{ReconciliationID: value.ID, LeaseToken: value.LeaseToken, From: value.Status, Result: memory.ReconciliationConflictResult, ReceiptID: uuid.NewString(), Reason: reason, EvidenceDigest: value.ContentHash, At: w.now().UTC()})
	return err
}
func (w *ExpiryReconciler) Run(ctx context.Context) error {
	return runLoop(ctx, "expiry_reconciler", w.interval, w.RunOnce, w.onError, w.errors)
}

type MaintenanceExpiryReconciler struct {
	store  memory.MaintenanceReconciliationPersistence
	remote memory.NocturneRemote
	purger *Purger
	auth   memory.MaintenanceAuthorization
	lease  time.Duration
	now    func() time.Time
}

func NewMaintenanceExpiryReconciler(store memory.MaintenanceReconciliationPersistence, remote memory.NocturneRemote, purger *Purger, auth memory.MaintenanceAuthorization, lease time.Duration, now func() time.Time) (*MaintenanceExpiryReconciler, error) {
	if store == nil || remote == nil || purger == nil || purger.maintenanceStore == nil || lease <= 0 {
		return nil, errors.New("valid maintenance reconciliation dependencies and lease are required")
	}
	if err := auth.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &MaintenanceExpiryReconciler{store: store, remote: remote, purger: purger, auth: auth, lease: lease, now: now}, nil
}

func (w *MaintenanceExpiryReconciler) RunOnce(ctx context.Context) (int, error) {
	value, err := w.store.ClaimMaintenanceExpiryReconciliation(ctx, w.auth, w.now().UTC(), w.lease)
	if memory.ErrorCode(err) == memory.CodeNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if value.Status == memory.ReconciliationDeletePending {
		return 1, w.purgeAndFinalize(ctx, value)
	}
	if value.Status != memory.ReconciliationReconciling {
		return 1, &protocolError{category: "unexpected_maintenance_reconciliation_status", permanent: true}
	}
	capabilities, err := w.remote.Capabilities(ctx)
	if err != nil {
		return 1, err
	}
	path := w.purger.parentPath + "/" + value.LogicalMemoryID
	node, err := w.remote.GetNode(ctx, path)
	if err == nil {
		return 1, w.reconcileObservedNode(ctx, value, node)
	}
	if !IsNotFound(err) {
		return 1, err
	}
	if capabilities.BootEpoch == value.SentBootEpoch {
		return 1, &protocolError{category: "same_boot_maintenance_absence_unknown"}
	}
	second, secondErr := w.remote.GetNode(ctx, path)
	if secondErr == nil {
		return 1, w.reconcileObservedNode(ctx, value, second)
	}
	if !IsNotFound(secondErr) {
		return 1, secondErr
	}
	_, err = w.store.FinalizeMaintenanceExpiryReconciliation(ctx, w.auth, memory.ReconciliationFinalization{
		ReconciliationID: value.ID, LeaseToken: value.LeaseToken, From: memory.ReconciliationReconciling,
		Result: memory.ReconciliationAbsenceResult, ReceiptID: uuid.NewString(), Reason: "new_boot_double_absence_verified",
		EvidenceDigest: memory.SHA256String(value.SentBootEpoch + "\x00" + capabilities.BootEpoch + "\x00" + path), At: w.now().UTC(),
	})
	return 1, err
}

func (w *MaintenanceExpiryReconciler) reconcileObservedNode(ctx context.Context, value memory.ExpiryReconciliation, node memory.RemoteNode) error {
	if memory.SHA256String(node.Content) != value.ContentHash {
		_, err := w.store.FinalizeMaintenanceExpiryReconciliation(ctx, w.auth, memory.ReconciliationFinalization{
			ReconciliationID: value.ID, LeaseToken: value.LeaseToken, From: value.Status,
			Result: memory.ReconciliationConflictResult, ReceiptID: uuid.NewString(), Reason: "maintenance_remote_hash_conflict",
			EvidenceDigest: value.ContentHash, At: w.now().UTC(),
		})
		if err != nil {
			return err
		}
		return &protocolError{category: "maintenance_remote_hash_conflict"}
	}
	if value.Status == memory.ReconciliationReconciling {
		var err error
		value, err = w.store.TransitionMaintenanceExpiryReconciliation(ctx, w.auth, memory.ReconciliationTransition{
			ReconciliationID: value.ID, LeaseToken: value.LeaseToken, From: memory.ReconciliationReconciling,
			To: memory.ReconciliationDeletePending, At: w.now().UTC(),
		})
		if err != nil {
			return err
		}
	}
	return w.purgeAndFinalize(ctx, value)
}

func (w *MaintenanceExpiryReconciler) purgeAndFinalize(ctx context.Context, value memory.ExpiryReconciliation) error {
	if err := w.purger.PurgeMaintenance(ctx, w.auth, value.DeliveryID, value.LogicalMemoryID, value.ContentHash); err != nil {
		return err
	}
	_, err := w.store.FinalizeMaintenanceExpiryReconciliation(ctx, w.auth, memory.ReconciliationFinalization{
		ReconciliationID: value.ID, LeaseToken: value.LeaseToken, From: memory.ReconciliationDeletePending,
		Result: memory.ReconciliationDeleteResult, ReceiptID: uuid.NewString(), Reason: "remote_logical_delete_verified",
		EvidenceDigest: value.ContentHash, At: w.now().UTC(),
	})
	return err
}

type RemoteEraserOptions struct {
	Lease              time.Duration
	MaxReconciliations int
	Now                func() time.Time
}

type RemoteEraser struct {
	store   memory.MaintenanceReconciliationPersistence
	remote  memory.NocturneRemote
	purger  *Purger
	lease   time.Duration
	maxRuns int
	now     func() time.Time
}

func NewRemoteEraser(store memory.MaintenanceReconciliationPersistence, remote memory.NocturneRemote, purger *Purger, options RemoteEraserOptions) (*RemoteEraser, error) {
	if store == nil || remote == nil || purger == nil || purger.maintenanceStore == nil || options.Lease <= 0 || options.MaxReconciliations <= 0 {
		return nil, errors.New("valid remote eraser dependencies, lease, and bound are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &RemoteEraser{store: store, remote: remote, purger: purger, lease: options.Lease, maxRuns: options.MaxReconciliations, now: options.Now}, nil
}

func (e *RemoteEraser) Erase(ctx context.Context, request privacy.RemoteEraseRequest) (privacy.RemoteEraseResult, error) {
	auth := memory.MaintenanceAuthorization{ErasureID: request.ErasureID, ReceiptID: request.Receipt.ID, TargetLearnerGeneration: request.LearnerGeneration}
	if err := auth.Validate(); err != nil || (request.Receipt.Store != privacy.StoreNocturnePaths && request.Receipt.Store != privacy.StoreNocturneOrphanHistory) {
		return privacy.RemoteEraseResult{}, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_nocturne_remote_erasure_request"}
	}
	if request.Receipt.Status != privacy.StepPending && request.Receipt.Status != privacy.StepPartial && request.Receipt.Status != privacy.StepUnknown {
		return privacy.RemoteEraseResult{}, &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "nocturne_remote_receipt_not_resumable"}
	}
	reconciler, err := NewMaintenanceExpiryReconciler(e.store, e.remote, e.purger, auth, e.lease, e.now)
	if err != nil {
		return privacy.RemoteEraseResult{}, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_nocturne_maintenance_authorization"}
	}
	for run := 0; run < e.maxRuns; run++ {
		processed, runErr := reconciler.RunOnce(ctx)
		if runErr != nil {
			if ctx.Err() != nil {
				return privacy.RemoteEraseResult{}, ctx.Err()
			}
			status, remoteFailure := maintenanceFailureStatus(runErr)
			if !remoteFailure {
				return privacy.RemoteEraseResult{}, &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "maintenance_reconciliation_persistence_failed"}
			}
			summary, summaryErr := e.store.MaintenanceReconciliationSummary(ctx, auth)
			if summaryErr != nil {
				return e.remoteEraseResult(auth, memory.MaintenanceReconciliationSummary{Pending: 1}, privacy.StepUnknown, "remote_reconciliation_state_unknown"), nil
			}
			reason := "remote_reconciliation_incomplete"
			if summary.Conflicts > 0 {
				reason = "remote_reconciliation_conflict"
			} else if status == privacy.StepUnknown {
				reason = "remote_reconciliation_unknown"
			}
			return e.remoteEraseResult(auth, summary, status, reason), nil
		}
		if processed == 0 {
			break
		}
	}
	summary, err := e.store.MaintenanceReconciliationSummary(ctx, auth)
	if err != nil {
		return privacy.RemoteEraseResult{}, &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "maintenance_reconciliation_summary_unavailable"}
	}
	switch {
	case summary.Conflicts > 0:
		return e.remoteEraseResult(auth, summary, privacy.StepPartial, "remote_reconciliation_conflict"), nil
	case summary.Pending > 0:
		return e.remoteEraseResult(auth, summary, privacy.StepPartial, "remote_reconciliation_pending"), nil
	default:
		return e.remoteEraseResult(auth, summary, privacy.StepSucceeded, "all_old_generation_remote_reconciliations_verified"), nil
	}
}

func maintenanceFailureStatus(err error) (privacy.StepStatus, bool) {
	var remoteErr *Error
	if errors.As(err, &remoteErr) {
		switch remoteErr.category {
		case CategoryActive, CategoryAuth, CategoryValidation:
			return privacy.StepPartial, true
		default:
			return privacy.StepUnknown, true
		}
	}
	var reconciliationErr *protocolError
	if errors.As(err, &reconciliationErr) {
		if strings.Contains(reconciliationErr.category, "unknown") || strings.Contains(reconciliationErr.category, "not_stable") {
			return privacy.StepUnknown, true
		}
		return privacy.StepPartial, true
	}
	return "", false
}

func (e *RemoteEraser) remoteEraseResult(auth memory.MaintenanceAuthorization, summary memory.MaintenanceReconciliationSummary, status privacy.StepStatus, reason string) privacy.RemoteEraseResult {
	evidence := memory.SHA256String(fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d\x00%s", auth.ErasureID, auth.ReceiptID, auth.TargetLearnerGeneration, summary.Pending, summary.Conflicts, status))
	return privacy.RemoteEraseResult{Status: status, StableReason: reason, EvidenceDigest: evidence, CompletedAt: e.now().UTC()}
}

func runLoop(ctx context.Context, worker string, interval time.Duration, run func(context.Context) (int, error), observer ErrorObserver, errorStream chan<- WorkerError) error {
	delay := time.Duration(0)
	maxDelay := interval * 8
	if maxDelay < interval {
		maxDelay = interval
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			_, err := run(ctx)
			if err == nil {
				delay = interval
				timer.Reset(delay)
				continue
			}
			category, permanent := "worker_error", false
			var classified outbox.ClassifiedError
			if errors.As(err, &classified) {
				category, permanent = classified.Category(), classified.Permanent()
				if category == "" {
					category = "worker_error"
				}
			}
			event := WorkerError{Worker: worker, Category: category, Permanent: permanent, Err: err}
			if observer != nil {
				observer(event)
			}
			select {
			case errorStream <- event:
			default:
			}
			if permanent {
				return event
			}
			if delay < interval {
				delay = interval
			} else if delay < maxDelay/2 {
				delay *= 2
			} else {
				delay = maxDelay
			}
			timer.Reset(delay)
		}
	}
}
func firstError(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
func slicesContain(values []int64, target int64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

var _ outbox.Consumer = (*Consumer)(nil)
var _ privacy.RemoteEraser = (*RemoteEraser)(nil)
