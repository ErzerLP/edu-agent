package postgresstore

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var eventNamespace = uuid.MustParse("7d812fdc-fc90-4c6b-8f8f-9badf3281f70")

// Store is the PostgreSQL authority for learning commands and projections.
type Store struct {
	pool     *pgxpool.Pool
	registry *learning.EventRegistry
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, registry: learning.NewEventRegistry()}
}

type aggregateKey struct{ kind, id string }

func (s *Store) LookupOperation(ctx context.Context, lookup learning.OperationLookup) (learning.OperationResult, error, bool) {
	if err := validateOperationLookup(lookup); err != nil {
		return learning.OperationResult{}, err, false
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return learning.OperationResult{}, fmt.Errorf("begin learning operation lookup: %w", err), false
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockOperation(ctx, tx, lookup); err != nil {
		return learning.OperationResult{}, err, false
	}
	result, replayErr, found := lookupArchivedOperation(ctx, tx, lookup)
	if !found || replayErr == nil {
		if err := tx.Commit(ctx); err != nil {
			return learning.OperationResult{}, fmt.Errorf("commit learning operation lookup: %w", err), false
		}
	}
	return result, replayErr, found
}

func (s *Store) ArchiveRejection(ctx context.Context, rejection learning.OperationRejection) (learning.OperationResult, error) {
	if err := validateOperationLookup(rejection.Lookup); err != nil {
		return learning.OperationResult{}, err
	}
	if rejection.AggregateType != "goal" && rejection.AggregateType != "session" || rejection.AggregateID == "" || len(rejection.Expectations) == 0 || rejection.Error.Code == "" || rejection.CompletedAt.IsZero() {
		return learning.OperationResult{}, &learning.Error{Code: learning.CodeInvalidRequest}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return learning.OperationResult{}, fmt.Errorf("begin learning rejection archive: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockOperation(ctx, tx, rejection.Lookup); err != nil {
		return learning.OperationResult{}, err
	}
	if result, replayErr, found := lookupArchivedOperation(ctx, tx, rejection.Lookup); found {
		if replayErr == nil {
			if err := tx.Commit(ctx); err != nil {
				return learning.OperationResult{}, fmt.Errorf("commit learning rejection replay: %w", err)
			}
		}
		return result, replayErr
	}
	expectations := append([]learning.AggregateExpectation(nil), rejection.Expectations...)
	sort.Slice(expectations, func(i, j int) bool {
		if expectations[i].Type != expectations[j].Type {
			return expectations[i].Type < expectations[j].Type
		}
		return expectations[i].ID < expectations[j].ID
	})
	seen := map[aggregateKey]bool{}
	var conflict *learning.Error
	for _, expected := range expectations {
		if (expected.Type != "goal" && expected.Type != "session") || expected.ID == "" || expected.ExpectedVersion < 0 {
			return learning.OperationResult{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_rejection_expectation"}
		}
		key := aggregateKey{expected.Type, expected.ID}
		if seen[key] {
			return learning.OperationResult{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "duplicate_rejection_expectation"}
		}
		seen[key] = true
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "learning-aggregate:"+key.kind+":"+key.id); err != nil {
			return learning.OperationResult{}, fmt.Errorf("lock rejected learning aggregate: %w", err)
		}
		var current int64
		err := tx.QueryRow(ctx, `SELECT aggregate_version FROM learning_aggregate_heads WHERE aggregate_type=$1 AND aggregate_id=$2 FOR UPDATE`, key.kind, key.id).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			current = 0
		} else if err != nil {
			return learning.OperationResult{}, fmt.Errorf("read rejected learning aggregate: %w", err)
		}
		if current != expected.ExpectedVersion && conflict == nil {
			conflict = &learning.Error{Code: learning.CodeVersionConflict, AggregateType: key.kind, AggregateID: key.id, ExpectedVersion: expected.ExpectedVersion, CurrentVersion: current}
		}
		if rejection.Error.Code == learning.CodeVersionConflict && rejection.Error.AggregateType == key.kind && rejection.Error.AggregateID == key.id {
			rejection.Error.CurrentVersion = current
		}
	}
	high, err := eventHighWater(ctx, tx, false)
	if err != nil {
		return learning.OperationResult{}, err
	}
	if conflict != nil {
		conflict.AsOfEventSequence = high
		rejection.Error = *conflict
	} else if rejection.Error.Code == learning.CodeVersionConflict {
		rejection.Error.AsOfEventSequence = high
	}
	requestHash, err := decodeHash(rejection.Lookup.RequestHash)
	if err != nil {
		return learning.OperationResult{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_request_hash", Cause: err}
	}
	encodedError, err := json.Marshal(rejection.Error)
	if err != nil {
		return learning.OperationResult{}, fmt.Errorf("encode learning rejection: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO learning_inbox(device_id,operation_id,request_hash,aggregate_type,aggregate_id,terminal_status,result,completed_at) VALUES($1,$2,$3,$4,$5,'rejected',$6,$7)`, rejection.Lookup.DeviceID, rejection.Lookup.OperationID, requestHash, rejection.AggregateType, rejection.AggregateID, encodedError, rejection.CompletedAt); err != nil {
		return learning.OperationResult{}, fmt.Errorf("insert learning rejection: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return learning.OperationResult{}, fmt.Errorf("commit learning rejection: %w", err)
	}
	result := learning.OperationResult{Status: "rejected", Archived: true, AggregateType: rejection.AggregateType, AggregateID: rejection.AggregateID, Result: encodedError}
	archived := rejection.Error
	return result, &archived
}

func lockOperation(ctx context.Context, tx pgx.Tx, lookup learning.OperationLookup) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "learning-operation:"+lookup.DeviceID+":"+lookup.OperationID); err != nil {
		return fmt.Errorf("lock learning operation: %w", err)
	}
	return nil
}

func validateOperationLookup(lookup learning.OperationLookup) error {
	if _, err := uuid.Parse(lookup.DeviceID); err != nil {
		return &learning.Error{Code: learning.CodeInvalidRequest}
	}
	if _, err := uuid.Parse(lookup.OperationID); err != nil {
		return &learning.Error{Code: learning.CodeInvalidRequest}
	}
	if _, err := decodeHash(lookup.RequestHash); err != nil {
		return &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_request_hash", Cause: err}
	}
	return nil
}

func (s *Store) Commit(ctx context.Context, request learning.CommitRequest) (learning.OperationResult, error) {
	if err := validateCommitRequest(request); err != nil {
		return learning.OperationResult{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return learning.OperationResult{}, fmt.Errorf("begin learning command: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	lookup := learning.OperationLookup{DeviceID: request.DeviceID, OperationID: request.Operation.OperationID, RequestHash: request.RequestHash}
	if err := lockOperation(ctx, tx, lookup); err != nil {
		return learning.OperationResult{}, err
	}
	if replay, replayErr, found := lookupArchivedOperation(ctx, tx, lookup); found {
		if replayErr == nil {
			if err := tx.Commit(ctx); err != nil {
				return learning.OperationResult{}, fmt.Errorf("commit learning replay: %w", err)
			}
		}
		return replay, replayErr
	}

	expectations := append([]learning.AggregateExpectation(nil), request.Expectations...)
	sort.Slice(expectations, func(i, j int) bool {
		if expectations[i].Type != expectations[j].Type {
			return expectations[i].Type < expectations[j].Type
		}
		return expectations[i].ID < expectations[j].ID
	})
	versions := make(map[aggregateKey]int64, len(expectations))
	for _, expected := range expectations {
		key := aggregateKey{expected.Type, expected.ID}
		if _, duplicate := versions[key]; duplicate {
			return learning.OperationResult{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "duplicate_aggregate_expectation"}
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "learning-aggregate:"+key.kind+":"+key.id); err != nil {
			return learning.OperationResult{}, fmt.Errorf("lock learning aggregate: %w", err)
		}
		var current int64
		err := tx.QueryRow(ctx, `SELECT aggregate_version FROM learning_aggregate_heads WHERE aggregate_type=$1 AND aggregate_id=$2 FOR UPDATE`, key.kind, key.id).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			current = 0
			if expected.ExpectedVersion == 0 {
				if _, err := tx.Exec(ctx, `INSERT INTO learning_aggregate_heads(aggregate_type,aggregate_id,aggregate_version,last_event_seq,updated_at) VALUES($1,$2,0,0,$3)`, key.kind, key.id, request.ReceivedAt); err != nil {
					return learning.OperationResult{}, fmt.Errorf("create learning aggregate head: %w", err)
				}
			}
		} else if err != nil {
			return learning.OperationResult{}, fmt.Errorf("read learning aggregate head: %w", err)
		}
		if current != expected.ExpectedVersion {
			high, highErr := eventHighWater(ctx, tx, false)
			if highErr != nil {
				return learning.OperationResult{}, highErr
			}
			return learning.OperationResult{}, &learning.Error{Code: learning.CodeVersionConflict, AggregateType: key.kind, AggregateID: key.id, ExpectedVersion: expected.ExpectedVersion, CurrentVersion: current, AsOfEventSequence: high}
		}
		versions[key] = current
	}

	if len(request.Batch.Events) == 0 {
		return learning.OperationResult{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "empty_event_batch"}
	}
	var clock int64
	if err := tx.QueryRow(ctx, `SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1 FOR UPDATE`).Scan(&clock); err != nil {
		return learning.OperationResult{}, fmt.Errorf("lock learning event clock: %w", err)
	}
	firstSequence := clock + 1
	focusCreatedSequence := int64(0)
	for ordinal, draft := range request.Batch.Events {
		if draft.Type == learning.EventFocusSuspended {
			focusCreatedSequence = firstSequence + int64(ordinal)
			break
		}
	}
	if request.Batch.FocusFrame != nil && request.Batch.FocusFrame.CreatedEventSequence == 0 {
		request.Batch.FocusFrame.CreatedEventSequence = focusCreatedSequence
	}
	events := make([]learning.LearningEvent, 0, len(request.Batch.Events))
	for ordinal, draft := range request.Batch.Events {
		key := aggregateKey{draft.AggregateType, draft.AggregateID}
		current, ok := versions[key]
		if !ok {
			return learning.OperationResult{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "event_aggregate_not_locked"}
		}
		current++
		versions[key] = current
		sequence := firstSequence + int64(ordinal)
		payloadSource := draft.Payload
		if isSessionSnapshotEvent(draft.Type) {
			var snapshot learning.SessionProjection
			if err := json.Unmarshal(payloadSource, &snapshot); err != nil || snapshot.Session.ID != draft.AggregateID {
				return learning.OperationResult{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_session_projection_payload", Cause: err}
			}
			snapshot.Session.AggregateVer = current
			if snapshot.Session.ActiveFrame != nil && snapshot.Session.ActiveFrame.CreatedEventSequence == 0 {
				snapshot.Session.ActiveFrame.CreatedEventSequence = focusCreatedSequence
			}
			payloadSource, err = json.Marshal(snapshot)
			if err != nil {
				return learning.OperationResult{}, fmt.Errorf("encode session projection payload: %w", err)
			}
		}
		payload, err := canonicalJSON(payloadSource)
		if err != nil {
			return learning.OperationResult{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_event_payload", Cause: err}
		}
		payloadHash := learning.SHA256(payload)
		eventID := uuid.NewSHA1(eventNamespace, []byte(request.DeviceID+"\n"+request.Operation.OperationID+fmt.Sprintf("\n%d", ordinal))).String()
		payloadID := uuid.NewSHA1(eventNamespace, []byte("payload\n"+eventID)).String()
		events = append(events, learning.LearningEvent{
			EventSequence: sequence, ID: eventID, Type: draft.Type, SchemaVersion: learning.EventSchemaVersion,
			AggregateType: key.kind, AggregateID: key.id, AggregateVersion: current,
			DeviceID: request.DeviceID, OperationID: request.Operation.OperationID, OperationOrdinal: ordinal,
			ReceivedAt: request.ReceivedAt, OccurredAt: request.Operation.OccurredAt,
			PayloadID: payloadID, PayloadHash: payloadHash, Payload: payload,
		})
	}
	lastSequence := events[len(events)-1].EventSequence
	for index := range request.Batch.Invalidations {
		if request.Batch.Invalidations[index].EventSeq == 0 {
			for _, event := range events {
				if event.Type == learning.EventEvidenceInvalidated {
					request.Batch.Invalidations[index].EventSeq = event.EventSequence
					break
				}
			}
		}
		if request.Batch.Invalidations[index].CreatedAt.IsZero() {
			request.Batch.Invalidations[index].CreatedAt = request.ReceivedAt
		}
	}

	if err := finalizeSessionResult(&request.Batch, versions, focusCreatedSequence); err != nil {
		return learning.OperationResult{}, err
	}
	// Persist immutable records before their canonical event envelopes.
	if err := insertTypedRecords(ctx, tx, request); err != nil {
		return learning.OperationResult{}, err
	}
	for _, event := range events {
		payloadHash, _ := hex.DecodeString(event.PayloadHash)
		if _, err := tx.Exec(ctx, `INSERT INTO learning_event_payloads(id,payload,payload_hash,created_at) VALUES($1,$2,$3,$4)`, event.PayloadID, event.Payload, payloadHash, event.ReceivedAt); err != nil {
			return learning.OperationResult{}, fmt.Errorf("insert learning event payload: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO learning_events(event_seq,id,event_type,event_schema_version,aggregate_type,aggregate_id,aggregate_version,device_id,operation_id,operation_ordinal,received_at,occurred_at,payload_id,payload_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, event.EventSequence, event.ID, event.Type, event.SchemaVersion, event.AggregateType, event.AggregateID, event.AggregateVersion, event.DeviceID, event.OperationID, event.OperationOrdinal, event.ReceivedAt, event.OccurredAt, event.PayloadID, payloadHash); err != nil {
			return learning.OperationResult{}, fmt.Errorf("insert learning event: %w", err)
		}
	}
	for key, version := range versions {
		if _, err := tx.Exec(ctx, `UPDATE learning_aggregate_heads SET aggregate_version=$3,last_event_seq=$4,updated_at=$5 WHERE aggregate_type=$1 AND aggregate_id=$2`, key.kind, key.id, version, lastSequence, request.ReceivedAt); err != nil {
			return learning.OperationResult{}, fmt.Errorf("advance learning aggregate head: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE learning_event_clock SET current_event_seq=$1,updated_at=$2 WHERE singleton_id=1`, lastSequence, request.ReceivedAt); err != nil {
		return learning.OperationResult{}, fmt.Errorf("advance learning event clock: %w", err)
	}
	for _, message := range request.Batch.Outbox {
		if _, err := postgresstore.EnqueueWith(ctx, tx, message); err != nil {
			return learning.OperationResult{}, fmt.Errorf("enqueue learning outbox message: %w", err)
		}
	}

	// Reuse the same reducer for read-your-writes and full rebuilds.
	allEvents, err := loadEvents(ctx, tx, 0, lastSequence)
	if err != nil {
		return learning.OperationResult{}, err
	}
	var generationID string
	if err := tx.QueryRow(ctx, `SELECT active_generation_id FROM learning_projection_head WHERE singleton_id=1 FOR UPDATE`).Scan(&generationID); err != nil {
		return learning.OperationResult{}, fmt.Errorf("lock active learning projection: %w", err)
	}
	projection, err := learning.Replay(allEvents, s.registry, generationID)
	if err != nil {
		return learning.OperationResult{}, err
	}
	if err := replaceProjection(ctx, tx, generationID, projection, lastSequence, request.ReceivedAt); err != nil {
		return learning.OperationResult{}, err
	}

	primary := aggregateKey{request.Operation.AggregateType, request.Operation.AggregateID}
	result := learning.OperationResult{
		Status: "succeeded", Archived: true, AggregateType: primary.kind, AggregateID: primary.id, AggregateVersion: versions[primary],
		FirstEventSequence: firstSequence, LastEventSequence: lastSequence, ProjectionAsOf: lastSequence,
		TutoringState: request.Batch.TutoringState, EvidenceDisposition: request.Batch.Disposition,
		Result: append(json.RawMessage(nil), request.Batch.TypedResult...),
	}
	if len(result.Result) == 0 {
		result.Result = json.RawMessage(`{}`)
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return learning.OperationResult{}, fmt.Errorf("encode learning operation result: %w", err)
	}
	requestHash, err := decodeHash(request.RequestHash)
	if err != nil {
		return learning.OperationResult{}, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "invalid_request_hash", Cause: err}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO learning_inbox(device_id,operation_id,request_hash,aggregate_type,aggregate_id,terminal_status,result,first_event_seq,last_event_seq,completed_at) VALUES($1,$2,$3,$4,$5,'succeeded',$6,$7,$8,$9)`, request.DeviceID, request.Operation.OperationID, requestHash, primary.kind, primary.id, resultJSON, firstSequence, lastSequence, request.ReceivedAt); err != nil {
		return learning.OperationResult{}, fmt.Errorf("insert learning inbox result: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return learning.OperationResult{}, fmt.Errorf("commit learning command: %w", err)
	}
	return result, nil
}

func finalizeSessionResult(batch *learning.CommandBatch, versions map[aggregateKey]int64, focusCreatedSequence int64) error {
	if batch.Session != nil {
		if version, ok := versions[aggregateKey{"session", batch.Session.ID}]; ok {
			batch.Session.AggregateVer = version
		}
		if batch.Session.ActiveFrame != nil && batch.Session.ActiveFrame.CreatedEventSequence == 0 {
			batch.Session.ActiveFrame.CreatedEventSequence = focusCreatedSequence
		}
	}
	if !batch.ResultSession {
		return nil
	}
	if batch.Session == nil {
		return &learning.Error{Code: learning.CodeInvalidRequest, Reason: "session_result_missing"}
	}
	encoded, err := json.Marshal(batch.Session)
	if err != nil {
		return fmt.Errorf("encode final session result: %w", err)
	}
	batch.TypedResult = encoded
	return nil
}

func validateCommitRequest(request learning.CommitRequest) error {
	if err := learning.ValidateOperation(request.Operation); err != nil {
		return err
	}
	if _, err := uuid.Parse(request.DeviceID); err != nil || request.RequestHash == "" || request.ReceivedAt.IsZero() || len(request.Expectations) == 0 {
		return &learning.Error{Code: learning.CodeInvalidRequest}
	}
	if request.Operation.AggregateType != "goal" && request.Operation.AggregateType != "session" {
		return &learning.Error{Code: learning.CodeInvalidRequest}
	}
	for _, expected := range request.Expectations {
		if (expected.Type != "goal" && expected.Type != "session") || expected.ID == "" || expected.ExpectedVersion < 0 {
			return &learning.Error{Code: learning.CodeInvalidRequest}
		}
	}
	if goal := request.Batch.GoalRevision; goal != nil {
		if request.Operation.AggregateType != "goal" || request.Operation.AggregateID != goal.GoalID || goal.Revision != request.Operation.ExpectedVersion+1 {
			return &learning.Error{Code: learning.CodeInvalidRequest, Reason: "goal_revision_head_mismatch"}
		}
		if (goal.Revision == 1) != (goal.PreviousRevisionID == nil) {
			return &learning.Error{Code: learning.CodeInvalidRequest, Reason: "goal_revision_lineage"}
		}
		goalEvents := 0
		for _, event := range request.Batch.Events {
			if event.Type == learning.EventGoalRevisionCreated && event.AggregateType == "goal" && event.AggregateID == goal.GoalID {
				goalEvents++
			}
		}
		if goalEvents != 1 || len(request.Batch.Events) != 1 {
			return &learning.Error{Code: learning.CodeInvalidRequest, Reason: "goal_revision_event_count"}
		}
	}
	return nil
}

func lookupArchivedOperation(ctx context.Context, tx pgx.Tx, lookup learning.OperationLookup) (learning.OperationResult, error, bool) {
	var storedHash []byte
	var payload []byte
	var terminalStatus, aggregateType, aggregateID string
	err := tx.QueryRow(ctx, `SELECT request_hash,terminal_status,aggregate_type,aggregate_id,result FROM learning_inbox WHERE device_id=$1 AND operation_id=$2 FOR UPDATE`, lookup.DeviceID, lookup.OperationID).Scan(&storedHash, &terminalStatus, &aggregateType, &aggregateID, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return learning.OperationResult{}, nil, false
	}
	if err != nil {
		return learning.OperationResult{}, fmt.Errorf("read learning inbox: %w", err), false
	}
	if hex.EncodeToString(storedHash) != lookup.RequestHash {
		return learning.OperationResult{}, &learning.Error{Code: learning.CodeIdempotencyConflict}, true
	}
	if terminalStatus == "rejected" {
		var archived learning.Error
		if err := json.Unmarshal(payload, &archived); err != nil {
			return learning.OperationResult{}, fmt.Errorf("decode learning inbox rejection: %w", err), true
		}
		result := learning.OperationResult{Status: "rejected", Replayed: true, Archived: true, AggregateType: aggregateType, AggregateID: aggregateID, Result: append(json.RawMessage(nil), payload...)}
		return result, &archived, true
	}
	var result learning.OperationResult
	if err := json.Unmarshal(payload, &result); err != nil {
		return learning.OperationResult{}, fmt.Errorf("decode learning inbox result: %w", err), true
	}
	result.Replayed = true
	result.Archived = true
	return result, nil, true
}

func canonicalJSON(value json.RawMessage) (json.RawMessage, error) {
	if len(value) == 0 {
		value = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(strings.NewReader(string(value)))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func isSessionSnapshotEvent(kind learning.EventType) bool {
	switch kind {
	case learning.EventLearningSessionStarted, learning.EventTutoringStateChanged,
		learning.EventFocusSuspended, learning.EventFocusResumed,
		learning.EventRouteAdvanced, learning.EventLearningCompleted:
		return true
	default:
		return false
	}
}

func decodeHash(value string) ([]byte, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("invalid SHA-256")
	}
	return decoded, nil
}

func eventHighWater(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, lock bool) (int64, error) {
	query := `SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1`
	if lock {
		query += ` FOR UPDATE`
	}
	var high int64
	if err := db.QueryRow(ctx, query).Scan(&high); err != nil {
		return 0, fmt.Errorf("read learning event high water: %w", err)
	}
	return high, nil
}

func encodeCursor(kind, generation string, asOfEventSequence int64, keys ...string) string {
	checkpoint := asOfEventSequence
	payload, _ := json.Marshal(struct {
		Kind              string   `json:"kind"`
		Generation        string   `json:"generation"`
		AsOfEventSequence *int64   `json:"as_of_event_seq"`
		Keys              []string `json:"keys"`
	}{Kind: kind, Generation: generation, AsOfEventSequence: &checkpoint, Keys: keys})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeCursor(value, kind, generation string, asOfEventSequence int64, keyCount int) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, &learning.Error{Code: learning.CodeStaleCursor}
	}
	var cursor struct {
		Kind              string   `json:"kind"`
		Generation        string   `json:"generation"`
		AsOfEventSequence *int64   `json:"as_of_event_seq"`
		Keys              []string `json:"keys"`
	}
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.Kind != kind || cursor.Generation != generation || cursor.AsOfEventSequence == nil || *cursor.AsOfEventSequence != asOfEventSequence || len(cursor.Keys) != keyCount {
		return nil, &learning.Error{Code: learning.CodeStaleCursor}
	}
	return cursor.Keys, nil
}

func normalizeLimit(value int) int {
	if value <= 0 {
		return 50
	}
	if value > 200 {
		return 200
	}
	return value
}

func stableReviewID(node string) string {
	return uuid.NewSHA1(eventNamespace, []byte("review\n"+strings.ToLower(node))).String()
}
