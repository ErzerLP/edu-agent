package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/keybackend"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/offline"
)

type secretByteReader interface {
	ReadSecretBytes(string) ([]byte, error)
}

type offlinePurgeAPI interface {
	OfflineDevicePurgeTask(context.Context, string) (*api.OfflinePurgeTask, error)
	AckOfflineDevicePurge(context.Context, api.OfflinePurgeTask, api.OfflinePurgeAckRequest) (api.OfflinePurgeAckResponse, error)
}

type platformOfflineKeyStore struct{}

func (platformOfflineKeyStore) Available(account string) bool { return keybackend.Available(account) }
func (platformOfflineKeyStore) Generate() ([]byte, error)     { return keybackend.Generate() }
func (platformOfflineKeyStore) Load(account string) ([]byte, error) {
	return keybackend.Load(account)
}
func (platformOfflineKeyStore) Store(account string, secret []byte) error {
	return keybackend.Store(account, secret)
}
func (platformOfflineKeyStore) Delete(account string) error { return keybackend.Delete(account) }

func (a *App) offlineKeyStore() OfflineKeyStore {
	if a.OfflineKeys != nil {
		return a.OfflineKeys
	}
	return platformOfflineKeyStore{}
}

type migrationKeyProvider struct{ store OfflineKeyStore }

func (p migrationKeyProvider) Generate() ([]byte, error) { return p.store.Generate() }
func (p migrationKeyProvider) Load(locator string) ([]byte, error) {
	secret, err := p.store.Load(locator)
	switch {
	case err == nil:
		return secret, nil
	case errors.Is(err, keybackend.ErrNotFound):
		return nil, offline.ErrSystemKeyNotFound
	default:
		return nil, offline.ErrKeyBackendUnavailable
	}
}
func (p migrationKeyProvider) Store(locator string, secret []byte) error {
	if err := p.store.Store(locator, secret); err != nil {
		return offline.ErrKeyBackendUnavailable
	}
	return nil
}
func (p migrationKeyProvider) Delete(locator string) error {
	if err := p.store.Delete(locator); err != nil {
		if errors.Is(err, keybackend.ErrNotFound) {
			return offline.ErrSystemKeyNotFound
		}
		return offline.ErrKeyBackendUnavailable
	}
	return nil
}

func offlineSystemUnlockError() error {
	return commandError("offline_key_backend_unavailable", "the operating-system key backend could not unlock the bound offline profile", "restore access to the configured system key service and retry; passphrase fallback is forbidden", ExitUnavailable)
}

func (a *App) runOffline(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return commandError("usage", "offline requires prepare, learn, status, sync, assessments, assessment, discard, key-migrate, or purge", "run edu-agent offline prepare", ExitInput)
	}
	switch args[0] {
	case "prepare":
		return a.runOfflinePrepare(ctx, args[1:])
	case "learn":
		return a.runOfflineLearn(ctx, args[1:])
	case "status":
		return a.runOfflineStatus(ctx, args[1:])
	case "sync":
		return a.runOfflineSync(ctx, args[1:])
	case "assessments":
		return a.runOfflineAssessments(ctx, args[1:])
	case "assessment":
		return a.runOfflineAssessment(ctx, args[1:])
	case "discard":
		return a.runOfflineDiscard(ctx, args[1:])
	case "key-migrate":
		return a.runOfflineKeyMigrate(ctx, args[1:])
	case "purge":
		return a.runOfflinePurge(ctx, args[1:])
	default:
		return commandError("usage", "unknown offline command "+args[0], "run edu-agent offline prepare, learn, status, sync, assessments, assessment, discard, key-migrate, or purge", ExitInput)
	}
}

func (a *App) runOfflinePrepare(ctx context.Context, args []string) error {
	set := newFlagSet("offline prepare")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	count := 5
	ttl := 72 * time.Hour
	set.IntVar(&count, "count", count, "number of offline activities, from 1 to 20")
	set.DurationVar(&ttl, "ttl", ttl, "offline eligibility duration, from 15m to 168h")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 || count < 1 || count > 20 || ttl < 15*time.Minute || ttl > 7*24*time.Hour || ttl%time.Second != 0 {
		return commandError("usage", "offline prepare flags are invalid", "use --count 1..20 and --ttl 15m..168h", ExitInput)
	}
	bound, timeout, err := a.loadBinding(flags.overrides())
	if err != nil {
		return err
	}
	store, trust, err := a.openOfflineStore(ctx, bound.Config, true)
	if err != nil {
		return err
	}
	defer store.Close()
	client := a.NewClient(bound.Config.ServerURL, bound.Token, timeout)

	currentTrustState := store.TrustState()
	verificationTrust := trust
	intentHasTrustState := false
	var request api.OfflinePrepareRequest
	intent, intentErr := store.PendingPrepareIntent(ctx)
	switch {
	case intentErr == nil:
		if err := decodeClosedJSON(intent.Canonical, &request); err != nil {
			return offlineStoreError(fmt.Errorf("decode prepare journal: %w", err))
		}
		if intentTrustBytes := intent.TrustState.Bytes(); len(intentTrustBytes) != 0 {
			verificationTrust, err = loadOfflineTrustCheckpoint(intentTrustBytes, bound.Config)
			if err != nil || request.TrustedManifestRevision != api.Uint63Decimal(strconv.FormatUint(verificationTrust.manifestRevision, 10)) || request.TrustedManifestDigest != verificationTrust.manifestDigest {
				return offlineStoreError(fmt.Errorf("%w: prepare intent trust checkpoint mismatch", offline.ErrCorruptStore))
			}
			intentHasTrustState = true
		}
	case errors.Is(intentErr, offline.ErrNotFound):
		view, currentErr := client.CurrentSession(ctx)
		if currentErr != nil {
			return mapAPIError(currentErr)
		}
		if view.Session.AggregateVersion < 0 {
			return commandError("protocol_error", "the current session version is invalid", "check the server version", ExitInternal)
		}
		operationID, uuidErr := a.NewUUID()
		if uuidErr != nil {
			return commandError("uuid_generation_failed", "a secure prepare operation ID could not be generated", "inspect the operating system random source", ExitInternal)
		}
		request = api.OfflinePrepareRequest{
			OperationID:             operationID,
			PayloadSchemaVersion:    1,
			ExpectedSessionVersion:  api.Uint63Decimal(strconv.FormatInt(view.Session.AggregateVersion, 10)),
			TrustedManifestRevision: api.Uint63Decimal(strconv.FormatUint(trust.manifestRevision, 10)),
			TrustedManifestDigest:   trust.manifestDigest,
			RequestedCount:          &count,
			RequestedTTLSeconds:     intPointer(int(ttl / time.Second)),
		}
		canonical, canonicalErr := canonicalJSON(request)
		if canonicalErr != nil {
			return offlineStoreError(canonicalErr)
		}
		if err := store.SavePrepareIntent(ctx, offline.PrepareIntent{RequestID: operationID, CreatedAt: time.Now().UTC(), Canonical: canonical, TrustState: currentTrustState}); err != nil {
			return offlineStoreError(err)
		}
	default:
		return offlineStoreError(intentErr)
	}

	response, _, err := client.PrepareOffline(ctx, request)
	if err != nil {
		return mapAPIError(err)
	}
	verificationResponse := response
	if !intentHasTrustState {
		alignedChain, alignErr := alignOfflinePrepareReplay(bound.Config, request, trust, response.ManifestChain)
		if alignErr != nil {
			return commandError("offline_signature_invalid", "the prepared offline pack failed trust verification", "do not use the pack; inspect the paired server and signer configuration", ExitConflict)
		}
		verificationResponse.ManifestChain = alignedChain
		verificationTrust = trust
	}
	packBytes, nextTrust, err := verifyPreparedPack(verificationResponse, request, bound.Config, verificationTrust)
	if err != nil {
		return commandError("offline_signature_invalid", "the prepared offline pack failed trust verification", "do not use the pack; inspect the paired server and signer configuration", ExitConflict)
	}
	checkpointCovered, err := offlineTrustOnVerifiedPath(bound.Config, verificationTrust, trust, verificationResponse.ManifestChain)
	if err != nil || !checkpointCovered {
		return commandError("offline_signature_invalid", "the prepared offline pack failed trust verification", "do not use the pack; inspect the paired server and signer configuration", ExitConflict)
	}
	nextState, stateErr := offline.NewTrustState(nextTrust.canonicalEnvelope)
	if stateErr != nil {
		return offlineStoreError(stateErr)
	}
	eligible, err := parseOfflineTime(response.Pack.Payload.EligibleUntil)
	if err != nil {
		return commandError("protocol_error", "the prepared pack eligibility time is invalid", "check the server version", ExitInternal)
	}
	archive, err := parseOfflineTime(response.Pack.Payload.ArchiveUntil)
	if err != nil {
		return commandError("protocol_error", "the prepared pack archive time is invalid", "check the server version", ExitInternal)
	}
	pack := offline.Pack{ID: response.Pack.Payload.PackID, EligibleUntil: eligible, ArchiveUntil: archive, ItemCount: len(response.Pack.Payload.Items), Canonical: packBytes}
	if err := store.PublishPreparedPack(ctx, request.OperationID, currentTrustState, nextState, pack); err != nil {
		return offlineStoreError(err)
	}
	_, err = fmt.Fprintf(a.Out, "Offline pack prepared: %s\nItems: %d\nEligible until: %s\n", safeText(pack.ID), pack.ItemCount, safeText(pack.EligibleUntil.Format(time.RFC3339Nano)))
	return err
}

func (a *App) runOfflineLearn(ctx context.Context, args []string) error {
	set := newFlagSet("offline learn")
	packID := ""
	set.StringVar(&packID, "pack", "", "specific offline pack UUID")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "offline learn accepts only --pack", "run edu-agent offline learn [--pack UUID]", ExitInput)
	}
	value, err := a.loadStoredConfig()
	if err != nil {
		return err
	}
	store, _, err := a.openOfflineStore(ctx, value, false)
	if err != nil {
		return err
	}
	defer store.Close()

	var pack offline.Pack
	if packID != "" {
		pack, err = store.GetPack(ctx, packID)
	} else {
		available, listErr := store.ListAvailablePacks(ctx, time.Now().UTC())
		if listErr != nil {
			return offlineStoreError(listErr)
		}
		for _, candidate := range available {
			if candidate.Available {
				pack, err = store.GetPack(ctx, candidate.ID)
				break
			}
		}
		if pack.ID == "" && err == nil {
			err = offline.ErrNotFound
		}
	}
	if err != nil {
		if errors.Is(err, offline.ErrNotFound) {
			return commandError("offline_pack_unavailable", "no eligible offline activity is available", "run edu-agent offline prepare while connected", ExitInput)
		}
		return offlineStoreError(err)
	}
	if !time.Now().UTC().Before(pack.EligibleUntil) {
		return commandError("offline_pack_expired", "the selected offline pack is no longer eligible for a new answer", "sync or discard it, then prepare a new pack while connected", ExitConflict)
	}
	var envelope api.OfflinePackEnvelope
	if err := decodeClosedJSON(pack.Canonical, &envelope); err != nil {
		return offlineStoreError(fmt.Errorf("decode sealed pack: %w", err))
	}
	var selected *api.OfflinePackItem
	for index := range envelope.Payload.Items {
		operationID := envelope.Payload.Items[index].Authorization.Payload.OperationID
		if _, operationErr := store.GetOperation(ctx, operationID); operationErr == nil {
			continue
		} else if !errors.Is(operationErr, offline.ErrNotFound) {
			return offlineStoreError(operationErr)
		}
		if _, statusErr := store.GetStatus(ctx, operationID); statusErr == nil {
			continue
		} else if !errors.Is(statusErr, offline.ErrNotFound) {
			return offlineStoreError(statusErr)
		}
		selected = &envelope.Payload.Items[index]
		break
	}
	if selected == nil {
		return commandError("offline_pack_consumed", "the selected pack has no unanswered activities", "run edu-agent offline status or prepare a new pack", ExitConflict)
	}
	presentedAt := time.Now().UTC()
	_, _ = fmt.Fprintf(a.Out, "Offline activity %s\n%s\n", safeText(selected.Activity.ActivityID), safeText(selected.Activity.Prompt))
	answer, err := a.Terminal.ReadLine("Answer: ")
	if err != nil {
		return commandError("offline_answer_failed", "the offline answer could not be read", "retry without redirecting invalid input", ExitInput)
	}
	if answer == "" || !utf8.ValidString(answer) || len([]byte(answer)) > 262144 {
		return commandError("offline_answer_invalid", "the offline answer must be non-empty UTF-8 and at most 256 KiB", "enter a shorter answer", ExitInput)
	}
	answeredAt := time.Now().UTC()
	answerDigest := sha256.Sum256([]byte(answer))
	presentedText, answeredText := api.OfflineTimestamp(presentedAt.Format(time.RFC3339Nano)), api.OfflineTimestamp(answeredAt.Format(time.RFC3339Nano))
	payload := api.OfflineAttemptPayload{
		Answer: answer, AnswerSHA256: hex.EncodeToString(answerDigest[:]), Help: api.OfflineHelpNone,
		Observations: []api.OfflineObservation{{Kind: api.OfflineActivityPresented, OccurredAt: &presentedText}, {Kind: api.OfflineAnswerRecorded, OccurredAt: &answeredText}},
	}
	payloadBytes, err := canonicalJSON(payload)
	if err != nil {
		return offlineStoreError(err)
	}
	authorization := selected.Authorization.Payload
	operation := api.OfflineOperation{
		OperationID: authorization.OperationID, DeviceID: authorization.DeviceID, DeviceSequence: authorization.DeviceSequence,
		SubmissionID: authorization.SubmissionID, PayloadSchemaVersion: 1, AggregateType: "offline_attempt", AggregateID: authorization.SubmissionID,
		ExpectedVersion: authorization.ExpectedVersion, OfflineActivityID: authorization.OfflineActivityID, ActivityRevision: authorization.ActivityRevision,
		Authorization: authorization, Signature: selected.Authorization.Signature, OccurredAt: &answeredText, OperationType: api.OfflineAttemptCompleted, Payload: payloadBytes,
	}
	canonicalOperation, err := canonicalJSON(operation)
	if err != nil {
		return offlineStoreError(err)
	}
	sequence, err := strconv.ParseUint(string(authorization.DeviceSequence), 10, 63)
	if err != nil || sequence == 0 {
		return commandError("protocol_error", "the offline authorization sequence is invalid", "discard the pack and prepare again", ExitInternal)
	}
	if err := store.SaveImmutableOperation(ctx, offline.QueuedOperation{
		ID: operation.OperationID, SubmissionID: operation.SubmissionID, PackID: authorization.PackID, DeviceSequence: sequence, QueuedAt: answeredAt, Canonical: canonicalOperation,
	}); err != nil {
		return offlineStoreError(err)
	}
	answer = ""
	_, err = fmt.Fprintf(a.Out, "Offline answer queued: %s\n", safeText(operation.OperationID))
	return err
}

func (a *App) runOfflineStatus(ctx context.Context, args []string) error {
	if len(args) > 1 {
		return commandError("usage", "offline status accepts at most one operation UUID", "run edu-agent offline status [operation-id]", ExitInput)
	}
	value, err := a.loadStoredConfig()
	if err != nil {
		return err
	}
	store, _, err := a.openOfflineStore(ctx, value, false)
	if err != nil {
		return err
	}
	defer store.Close()
	if len(args) == 1 {
		status, statusErr := store.GetStatus(ctx, args[0])
		if statusErr == nil {
			_, err = fmt.Fprintf(a.Out, "Operation: %s\nState: %s\nArchive: %s\nAssessment: %s\nEvidence: %s\n", safeText(status.OperationID), safeText(string(status.State)), safeText(status.ArchiveStatus), safeText(status.AssessmentStatus), safeText(status.EvidenceStatus))
			return err
		}
		if !errors.Is(statusErr, offline.ErrNotFound) {
			return offlineStoreError(statusErr)
		}
		operation, operationErr := store.GetOperation(ctx, args[0])
		if operationErr != nil {
			return offlineStoreError(operationErr)
		}
		_, err = fmt.Fprintf(a.Out, "Operation: %s\nState: queued\n", safeText(operation.ID))
		return err
	}
	summary, err := store.Summary(ctx, time.Now().UTC())
	if err != nil {
		return offlineStoreError(err)
	}
	_, err = fmt.Fprintf(a.Out, "Packs: %d (%d available, %d items)\nQueued: %d\nUploading: %d\nArchived pending evidence: %d\nTerminal: %d\nConflicts: %d\nBlocked: %d\nPending journals: %d\n",
		summary.PackCount, summary.AvailablePackCount, summary.AvailableItemCount, summary.QueuedCount, summary.UploadingCount,
		summary.ArchivedPendingCount, summary.TerminalCount, summary.ConflictCount, summary.BlockedCount, summary.PendingJournalCount)
	return err
}

func (a *App) runOfflineSync(ctx context.Context, args []string) error {
	set := newFlagSet("offline sync")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	limit := api.OfflineMaxSyncItems
	set.IntVar(&limit, "limit", limit, "maximum queued operations, from 1 to 50")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 || limit < 1 || limit > api.OfflineMaxSyncItems {
		return commandError("usage", "offline sync flags are invalid", "use --limit 1..50", ExitInput)
	}
	bound, timeout, err := a.loadBinding(flags.overrides())
	if err != nil {
		return err
	}
	store, _, err := a.openOfflineStore(ctx, bound.Config, false)
	if err != nil {
		return err
	}
	defer store.Close()
	queued, err := store.ListQueuedOperations(ctx, limit)
	if err != nil {
		return offlineStoreError(err)
	}
	if len(queued) == 0 {
		_, err = fmt.Fprintln(a.Out, "No queued offline operations.")
		return err
	}
	syncID, err := a.NewUUID()
	if err != nil {
		return commandError("uuid_generation_failed", "a secure sync request ID could not be generated", "inspect the operating system random source", ExitInternal)
	}
	operationIDs := make([]string, len(queued))
	for index := range queued {
		operationIDs[index] = queued[index].ID
	}
	batch, err := store.BeginSync(ctx, syncID, operationIDs)
	if err != nil {
		return offlineStoreError(err)
	}
	request := api.OfflineSyncRequest{SyncRequestID: syncID, PayloadSchemaVersion: 1, Operations: make([]api.OfflineOperation, len(batch.Operations))}
	for index := range batch.Operations {
		if err := decodeClosedJSON(batch.Operations[index].Canonical, &request.Operations[index]); err != nil {
			_ = store.Recover(ctx)
			return offlineStoreError(fmt.Errorf("decode queued operation: %w", err))
		}
	}
	canonicalRequest, err := canonicalJSON(request)
	if err != nil {
		_ = store.Recover(ctx)
		return offlineStoreError(err)
	}
	response, err := a.NewClient(bound.Config.ServerURL, bound.Token, timeout).SyncOfflineCanonical(ctx, canonicalRequest)
	if err != nil {
		_ = store.Recover(ctx)
		return mapAPIError(err)
	}
	counts := map[offline.LocalState]int{}
	for index := range response.Results {
		result := response.Results[index]
		state := localStateForSyncResult(result)
		receipt, err := canonicalJSONOrNil(result.IngestReceipt)
		if err != nil {
			_ = store.Recover(ctx)
			return offlineStoreError(err)
		}
		statusBytes, err := canonicalJSON(result)
		if err != nil {
			_ = store.Recover(ctx)
			return offlineStoreError(err)
		}
		updatedAt := time.Now().UTC()
		if result.IngestReceipt != nil {
			if parsed, parseErr := parseOfflineTime(result.IngestReceipt.ArchivedAt); parseErr == nil {
				updatedAt = parsed
			}
		} else if result.StatusTicket != nil {
			if parsed, parseErr := parseOfflineTime(result.StatusTicket.UpdatedAt); parseErr == nil {
				updatedAt = parsed
			}
		}
		reasons := make([]string, len(result.ReasonCodes))
		for reasonIndex := range result.ReasonCodes {
			reasons[reasonIndex] = string(result.ReasonCodes[reasonIndex])
		}
		if err := store.ApplySyncResult(ctx, offline.SyncResult{
			OperationID: result.OperationID, SubmissionID: result.SubmissionID, State: state,
			ArchiveStatus: string(result.ArchiveStatus), AssessmentStatus: string(result.AssessmentStatus), EvidenceStatus: string(result.EvidenceStatus),
			ReasonCodes: reasons, Receipt: receipt, Status: statusBytes, UpdatedAt: updatedAt,
		}); err != nil {
			_ = store.Recover(ctx)
			return offlineStoreError(err)
		}
		counts[state]++
	}
	if err := store.FinishSync(ctx, syncID); err != nil {
		return offlineStoreError(err)
	}
	_, err = fmt.Fprintf(a.Out, "Offline sync completed: %d result(s), terminal=%d pending=%d retryable=%d conflict=%d blocked=%d\n",
		len(response.Results), counts[offline.StateTerminal], counts[offline.StateArchivedPendingEvidence], counts[offline.StateQueued], counts[offline.StateConflict], counts[offline.StateBlocked])
	return err
}

func (a *App) runOfflineDiscard(ctx context.Context, args []string) error {
	set := newFlagSet("offline discard")
	all := false
	yes := false
	kindText := "operation"
	set.BoolVar(&all, "all", false, "cryptographically discard the complete offline profile")
	set.BoolVar(&yes, "yes", false, "confirm destructive discard without prompting")
	set.StringVar(&kindText, "kind", kindText, "pack, operation, or receipt")
	if err := set.Parse(args); err != nil || (all && len(set.Args()) != 0) || (!all && len(set.Args()) != 1) {
		return commandError("usage", "offline discard requires one object UUID or --all", "run edu-agent offline discard [--kind pack|operation|receipt] UUID, or --all", ExitInput)
	}
	value, err := a.loadStoredConfig()
	if err != nil {
		return err
	}
	store, _, err := a.openOfflineStore(ctx, value, false)
	if err != nil {
		return err
	}
	defer store.Close()
	if !yes {
		confirmed, confirmErr := a.Terminal.Confirm("Permanently discard encrypted offline state?")
		if confirmErr != nil || !confirmed {
			return commandError("cancelled", "offline state was not discarded", "rerun with --yes when ready", ExitInput)
		}
	}
	if all {
		if err := store.DiscardAll(ctx); err != nil {
			return offlineStoreError(err)
		}
		_ = keybackend.Delete(keybackend.Account(value.ServerURL, value.DeviceID))
		_, err = fmt.Fprintln(a.Out, "Encrypted offline profile discarded.")
		return err
	}
	kind, ok := parseOfflineObjectKind(kindText)
	if !ok {
		return commandError("usage", "offline discard kind is invalid", "use pack, operation, or receipt", ExitInput)
	}
	if err := store.Discard(ctx, kind, set.Args()[0]); err != nil {
		return offlineStoreError(err)
	}
	_, err = fmt.Fprintf(a.Out, "Encrypted offline %s discarded: %s\n", safeText(kindText), safeText(set.Args()[0]))
	return err
}

func (a *App) runOfflinePurge(ctx context.Context, args []string) error {
	set := newFlagSet("offline purge")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	if err := set.Parse(args); err != nil || len(set.Args()) != 1 {
		return commandError("usage", "offline purge requires one privacy erasure UUID", "run edu-agent offline purge ERASURE_UUID", ExitInput)
	}
	bound, timeout, err := a.loadBinding(flags.overrides())
	if err != nil {
		return err
	}
	client := a.NewClient(bound.Config.ServerURL, bound.Token, timeout)
	purger, ok := client.(offlinePurgeAPI)
	if !ok {
		return commandError("client_unsupported", "this client cannot process offline privacy purge tasks", "upgrade the CLI", ExitInternal)
	}
	task, err := purger.OfflineDevicePurgeTask(ctx, set.Args()[0])
	if err != nil {
		return mapAPIError(err)
	}
	if task == nil {
		_, err = fmt.Fprintln(a.Out, "No pending offline-device purge task.")
		return err
	}
	if task.DeviceID != bound.Config.DeviceID || bound.Config.Offline == nil || strconv.FormatInt(task.OldGeneration, 10) != bound.Config.Offline.LearnerGeneration || task.Status != "pending" {
		return commandError("offline_binding_mismatch", "the purge task does not match this paired offline profile", "do not delete local state; inspect the erasure and device binding", ExitConflict)
	}
	if task.OldGeneration <= 0 || task.CurrentGeneration <= task.OldGeneration {
		return commandError("offline_purge_challenge_invalid", "the offline purge challenge has invalid learner generations", "fetch a fresh purge task and retry", ExitConflict)
	}
	root, err := a.offlineRoot()
	if err != nil {
		return offlineStoreError(err)
	}
	account := keybackend.Account(bound.Config.ServerURL, bound.Config.DeviceID)
	purge, purgeErr := offline.BeginPurgeProfile(ctx, root)
	outcome := "succeeded"
	managedObjectsAbsent := true
	failureCode := ""
	if purgeErr != nil {
		outcome = "failed"
		failureCode = "path_delete_failed"
	} else if purge.KeyBackend() == offline.KeyBackendSystem || purge.KeyBackend() == 0 {
		secret, loadErr := keybackend.Load(account)
		if loadErr == nil {
			clearSecretBytes(secret)
			if deleteErr := keybackend.Delete(account); deleteErr != nil {
				purgeErr = fmt.Errorf("delete offline system key: %w", deleteErr)
			}
		} else if !errors.Is(loadErr, keybackend.ErrNotFound) {
			purgeErr = fmt.Errorf("load offline system key for purge: %w", loadErr)
		}
		if purgeErr == nil {
			remaining, verifyErr := keybackend.Load(account)
			clearSecretBytes(remaining)
			if verifyErr == nil {
				purgeErr = errors.New("offline system key remains after deletion")
			} else if !errors.Is(verifyErr, keybackend.ErrNotFound) {
				purgeErr = fmt.Errorf("verify offline system key deletion: %w", verifyErr)
			}
		}
		if purgeErr != nil {
			outcome = "failed"
			failureCode = "key_delete_failed"
		}
	}
	request := api.OfflinePurgeAckRequest{
		ChallengeRevision: task.ChallengeRevision,
		Challenge:         task.Challenge,
		Outcome:           outcome,
	}
	if purgeErr == nil {
		request.ManagedObjectsAbsent = &managedObjectsAbsent
	} else {
		request.FailureCode = failureCode
	}
	_, ackErr := purger.AckOfflineDevicePurge(ctx, *task, request)
	var closeErr error
	if purge != nil {
		if ackErr != nil || purgeErr != nil {
			closeErr = purge.Release()
		} else {
			closeErr = purge.Close()
		}
	}
	if ackErr != nil {
		return mapAPIError(ackErr)
	}
	if purgeErr != nil {
		return offlineStoreError(purgeErr)
	}
	if closeErr != nil {
		return offlineStoreError(closeErr)
	}
	_, err = fmt.Fprintln(a.Out, "Encrypted offline profile purged and acknowledged.")
	return err
}

func (a *App) runOfflineKeyMigrate(ctx context.Context, args []string) error {
	set := newFlagSet("offline key-migrate")
	target := "system"
	set.StringVar(&target, "to", target, "system or passphrase")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 || (target != "system" && target != "passphrase") {
		return commandError("usage", "offline key-migrate target is invalid", "run edu-agent offline key-migrate [--to system|passphrase]", ExitInput)
	}
	value, err := a.loadStoredConfig()
	if err != nil {
		return err
	}
	root, binding, trustState, err := a.offlineStoreParameters(value)
	if err != nil {
		return err
	}
	metadata, err := offline.InspectKey(ctx, root, binding)
	if err != nil {
		return offlineStoreError(err)
	}
	keys := a.offlineKeyStore()
	account := keybackend.Account(value.ServerURL, value.DeviceID)
	var unlockMaterial []byte
	switch metadata.Backend {
	case offline.KeyBackendSystem:
		unlockMaterial, err = keys.Load(account)
		if err != nil {
			clear(unlockMaterial)
			return offlineSystemUnlockError()
		}
	case offline.KeyBackendPassphrase:
		unlockMaterial, err = a.readOfflinePassphrase(false)
		if err != nil {
			return err
		}
	default:
		return offlineStoreError(offline.ErrCorruptStore)
	}
	defer clear(unlockMaterial)

	destinationBackend := offline.KeyBackendSystem
	var destinationPassphrase []byte
	if target == "passphrase" {
		destinationBackend = offline.KeyBackendPassphrase
		if metadata.Backend == offline.KeyBackendSystem {
			destinationPassphrase, err = a.readOfflinePassphrase(true)
			if err != nil {
				return err
			}
			defer clear(destinationPassphrase)
		} else {
			destinationPassphrase = unlockMaterial
		}
	} else if metadata.Backend != offline.KeyBackendSystem && !keys.Available(account) {
		return commandError("offline_key_backend_unavailable", "the operating-system key backend is unavailable", "enable Secret Service, Keychain, or Windows data protection and retry", ExitUnavailable)
	}

	migration, err := offline.BeginKeyMigration(ctx, root, binding, trustState, unlockMaterial, metadata.Backend)
	if err != nil {
		if metadata.Backend == offline.KeyBackendSystem && errors.Is(err, offline.ErrKeyUnavailable) {
			return offlineSystemUnlockError()
		}
		return offlineStoreError(err)
	}
	defer migration.Close()
	result, err := migration.Migrate(offline.KeyMigrationOptions{
		DestinationBackend:    destinationBackend,
		SystemLocator:         account,
		DestinationPassphrase: destinationPassphrase,
		SystemKeys:            migrationKeyProvider{store: keys},
	})
	if err != nil {
		return offlineStoreError(err)
	}
	if !result.Changed {
		_, err = fmt.Fprintf(a.Out, "Offline profile key already uses the %s backend.\n", safeText(target))
		return err
	}
	_, err = fmt.Fprintf(a.Out, "Offline profile key migrated to the %s backend.\n", safeText(target))
	return err
}

func (a *App) offlineStoreParameters(value config.Config) (string, offline.Binding, offline.TrustState, error) {
	trust, err := loadOfflineTrust(value)
	if err != nil {
		return "", offline.Binding{}, offline.TrustState{}, commandError("offline_trust_unavailable", "the paired profile has no valid offline signer trust root", "pair again with an offline-capable server", ExitConflict)
	}
	generation, err := strconv.ParseUint(value.Offline.LearnerGeneration, 10, 63)
	if err != nil || generation == 0 {
		return "", offline.Binding{}, offline.TrustState{}, commandError("offline_binding_invalid", "the offline learner generation is invalid", "pair again", ExitConflict)
	}
	binding, err := offline.NewBinding(value.ServerURL, value.DeviceID, generation)
	if err != nil {
		return "", offline.Binding{}, offline.TrustState{}, offlineStoreError(err)
	}
	trustState, err := offline.NewTrustState(trust.canonicalEnvelope)
	if err != nil {
		return "", offline.Binding{}, offline.TrustState{}, offlineStoreError(err)
	}
	root, err := a.offlineRoot()
	if err != nil {
		return "", offline.Binding{}, offline.TrustState{}, offlineStoreError(err)
	}
	return root, binding, trustState, nil
}

func (a *App) openOfflineStore(ctx context.Context, value config.Config, create bool) (*offline.Store, offlineTrustRoot, error) {
	root, binding, trustState, err := a.offlineStoreParameters(value)
	if err != nil {
		return nil, offlineTrustRoot{}, err
	}
	exists, err := offline.Exists(root)
	if err != nil {
		return nil, offlineTrustRoot{}, offlineStoreError(err)
	}
	if !exists && !create {
		return nil, offlineTrustRoot{}, commandError("offline_profile_not_found", "no encrypted offline profile exists", "run edu-agent offline prepare while connected", ExitInput)
	}
	account := keybackend.Account(value.ServerURL, value.DeviceID)
	keys := a.offlineKeyStore()
	if exists {
		metadata, inspectErr := offline.InspectKey(ctx, root, binding)
		if inspectErr != nil {
			return nil, offlineTrustRoot{}, offlineStoreError(inspectErr)
		}
		switch metadata.Backend {
		case offline.KeyBackendSystem:
			secret, loadErr := keys.Load(account)
			if loadErr != nil {
				clear(secret)
				return nil, offlineTrustRoot{}, offlineSystemUnlockError()
			}
			store, openErr := offline.OpenWithBackend(ctx, root, binding, trustState, secret, offline.KeyBackendSystem)
			clear(secret)
			if openErr != nil {
				if errors.Is(openErr, offline.ErrKeyUnavailable) {
					return nil, offlineTrustRoot{}, offlineSystemUnlockError()
				}
				return nil, offlineTrustRoot{}, offlineStoreError(openErr)
			}
			return offlineOpenResult(store, value)
		case offline.KeyBackendPassphrase:
			passphrase, passErr := a.readOfflinePassphrase(false)
			if passErr != nil {
				return nil, offlineTrustRoot{}, passErr
			}
			defer clear(passphrase)
			store, openErr := offline.OpenWithBackend(ctx, root, binding, trustState, passphrase, offline.KeyBackendPassphrase)
			if openErr != nil {
				return nil, offlineTrustRoot{}, offlineStoreError(openErr)
			}
			return offlineOpenResult(store, value)
		default:
			return nil, offlineTrustRoot{}, offlineStoreError(offline.ErrCorruptStore)
		}
	}
	if keys.Available(account) {
		secret, generateErr := keys.Generate()
		if generateErr != nil {
			return nil, offlineTrustRoot{}, commandError("offline_key_backend_unavailable", "the operating-system key backend could not create an offline key", "inspect the platform key service and retry", ExitUnavailable)
		}
		if storeErr := keys.Store(account, secret); storeErr != nil {
			clear(secret)
			return nil, offlineTrustRoot{}, commandError("offline_key_backend_unavailable", "the operating-system key backend could not protect the offline key", "inspect the platform key service and retry", ExitUnavailable)
		}
		store, createErr := offline.CreateSystem(ctx, root, offline.CreateOptions{Binding: binding, TrustState: trustState}, secret)
		clear(secret)
		if createErr == nil {
			return offlineOpenResult(store, value)
		}
		_ = keys.Delete(account)
		return nil, offlineTrustRoot{}, offlineStoreError(createErr)
	}
	passphrase, err := a.readOfflinePassphrase(true)
	if err != nil {
		return nil, offlineTrustRoot{}, err
	}
	defer clear(passphrase)
	store, createErr := offline.CreatePassphrase(ctx, root, offline.CreateOptions{Binding: binding, TrustState: trustState}, passphrase)
	if createErr != nil {
		return nil, offlineTrustRoot{}, offlineStoreError(createErr)
	}
	return offlineOpenResult(store, value)
}

func offlineOpenResult(store *offline.Store, value config.Config) (*offline.Store, offlineTrustRoot, error) {
	checkpoint, err := loadOfflineTrustCheckpoint(store.TrustState().Bytes(), value)
	if err != nil {
		_ = store.Close()
		return nil, offlineTrustRoot{}, commandError("offline_trust_unavailable", "the encrypted offline trust checkpoint is invalid", "preserve the profile and inspect the paired server history", ExitConflict)
	}
	return store, checkpoint, nil
}

func (a *App) readOfflinePassphrase(confirm bool) ([]byte, error) {
	read := func(prompt string) ([]byte, error) {
		if terminal, ok := a.Terminal.(secretByteReader); ok {
			return terminal.ReadSecretBytes(prompt)
		}
		value, err := a.Terminal.ReadSecret(prompt)
		return []byte(value), err
	}
	passphrase, err := read("Offline passphrase: ")
	if err != nil || len(passphrase) == 0 {
		clear(passphrase)
		return nil, commandError("offline_key_unavailable", "a non-empty offline passphrase is required", "retry and provide the profile passphrase", ExitAuth)
	}
	if confirm {
		repeated, repeatErr := read("Confirm offline passphrase: ")
		matched := repeatErr == nil && bytes.Equal(passphrase, repeated)
		clear(repeated)
		if !matched {
			clear(passphrase)
			return nil, commandError("offline_key_unavailable", "the offline passphrase confirmation did not match", "retry without exposing the passphrase", ExitAuth)
		}
	}
	return passphrase, nil
}

func (a *App) offlineRoot() (string, error) {
	if a.OfflineRoot != nil {
		return a.OfflineRoot()
	}
	return offline.DefaultRoot()
}

func (a *App) loadStoredConfig() (config.Config, error) {
	value, err := a.Config.Load()
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return config.Config{}, commandError("not_paired", "no local device binding exists", "run edu-agent pair", ExitAuth)
		}
		return config.Config{}, commandError("local_state_invalid", "configuration cannot be safely read", "run edu-agent device forget-local", ExitInput)
	}
	if err := value.Validate(); err != nil {
		return config.Config{}, commandError("local_state_invalid", "configuration is invalid", "run edu-agent device forget-local", ExitInput)
	}
	return value, nil
}

func clearSecretBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func offlineStoreError(err error) error {
	switch {
	case errors.Is(err, offline.ErrNotFound):
		return commandError("offline_not_found", "the requested encrypted offline object was not found", "run edu-agent offline status", ExitInput)
	case errors.Is(err, offline.ErrKeyUnavailable):
		return commandError("offline_key_unavailable", "the offline profile could not be unlocked", "retry with the correct passphrase; plaintext fallback is forbidden", ExitAuth)
	case errors.Is(err, offline.ErrKeyBackendUnavailable):
		return commandError("offline_key_backend_unavailable", "the operating-system key backend could not complete the offline key operation", "restore the configured key service and retry; passphrase fallback is forbidden", ExitUnavailable)
	case errors.Is(err, offline.ErrKeyMigrationPending):
		return commandError("offline_key_migration_pending", "the offline profile has an incomplete durable key migration", "rerun offline key-migrate with the original --to target", ExitConflict)
	case errors.Is(err, offline.ErrKeyAuthorityChanged), errors.Is(err, offline.ErrKeyMigrationConflict):
		return commandError("offline_key_migration_conflict", "the offline key authority or migration target changed", "inspect the profile and rerun key-migrate with the original target", ExitConflict)
	case errors.Is(err, offline.ErrKeyMigrationMismatch):
		return commandError("offline_key_migration_mismatch", "the migration destination does not unwrap the authoritative offline data key", "do not use fallback material; restore the selected backend or explicitly discard the profile", ExitConflict)
	case errors.Is(err, offline.ErrBindingMismatch):
		return commandError("offline_binding_mismatch", "the offline profile belongs to another server, device, generation, or trust root", "discard it explicitly or restore the matching paired profile", ExitConflict)
	case errors.Is(err, offline.ErrProfileBusy):
		return commandError("offline_profile_busy", "another process is using the offline profile", "wait for the other command to finish and retry", ExitUnavailable)
	case errors.Is(err, offline.ErrImmutableOperation), errors.Is(err, offline.ErrInvalidState):
		return commandError("offline_state_conflict", "the encrypted offline queue rejected a conflicting state change", "inspect offline status and discard only with explicit confirmation", ExitConflict)
	case errors.Is(err, offline.ErrCounterRollback), errors.Is(err, offline.ErrCounterOverflow), errors.Is(err, offline.ErrCorruptStore), errors.Is(err, offline.ErrUnsafePath):
		return commandError("offline_store_corrupt", "the encrypted offline store failed a security or integrity check", "do not retry writes; preserve evidence or explicitly discard the profile", ExitInternal)
	default:
		return commandError("offline_store_error", "the encrypted offline store operation failed", "inspect local permissions and profile integrity", ExitInternal)
	}
}

func localStateForSyncResult(result api.OfflineSyncItemResult) offline.LocalState {
	switch result.ResultKind {
	case api.OfflineResultArchived:
		if result.AssessmentStatus == api.OfflineAssessmentQueued || result.AssessmentStatus == api.OfflineAssessmentProcessing || result.AssessmentStatus == api.OfflineAssessmentPendingRetry || result.EvidenceStatus == api.OfflineEvidencePendingEvaluation {
			return offline.StateArchivedPendingEvidence
		}
		return offline.StateTerminal
	case api.OfflineResultConflict:
		return offline.StateConflict
	case api.OfflineResultBlocked:
		return offline.StateBlocked
	default:
		return offline.StateQueued
	}
}

func canonicalJSONOrNil(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	return canonicalJSON(value)
}

func parseOfflineObjectKind(value string) (offline.ObjectKind, bool) {
	switch value {
	case "pack":
		return offline.ObjectPack, true
	case "operation":
		return offline.ObjectOperation, true
	case "receipt":
		return offline.ObjectReceipt, true
	default:
		return 0, false
	}
}

func intPointer(value int) *int { return &value }
