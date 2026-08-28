package postgresstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type eventDB interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

const (
	rebuildLeaseDuration           = 2 * time.Minute
	rebuildHeartbeatInterval       = 30 * time.Second
	projectionVersionUpgradeReason = "projection_version_upgrade_required"
)

type rebuildLease struct {
	generationID string
	token        string
	target       int64
}

type rebuildHeartbeat struct {
	cancel context.CancelFunc
	done   <-chan error
}

func loadEvents(ctx context.Context, db eventDB, after, through int64) ([]learning.LearningEvent, error) {
	rows, err := db.Query(ctx, `SELECT e.event_seq,e.id,e.event_type,e.event_schema_version,e.aggregate_type,e.aggregate_id,e.aggregate_version,e.device_id,e.operation_id,e.operation_ordinal,e.received_at,e.occurred_at,e.payload_id,e.payload_hash,p.payload_hash,p.payload,p.redacted_at,(SELECT COALESCE(max(redacted_through_event_seq),0) FROM privacy_redaction_barriers),COALESCE(e.parent_session_id::text,''),e.event_source,COALESCE(e.archive_disposition,''),COALESCE(e.evidence_disposition,''),COALESCE(e.goal_revision_id::text,''),COALESCE(e.route_revision_id::text,''),COALESCE(e.knowledge_revision_id::text,''),COALESCE(e.activity_id::text,''),COALESCE(e.activity_revision,0) FROM learning_events e JOIN learning_event_payloads p ON p.id=e.payload_id WHERE e.event_seq>$1 AND e.event_seq<=$2 ORDER BY e.event_seq`, after, through)
	if err != nil {
		return nil, fmt.Errorf("read learning events: %w", err)
	}
	defer rows.Close()
	var result []learning.LearningEvent
	for rows.Next() {
		var event learning.LearningEvent
		var eventHash, payloadHash []byte
		var redactedAt *time.Time
		var redactedThrough int64
		if err := rows.Scan(&event.EventSequence, &event.ID, &event.Type, &event.SchemaVersion, &event.AggregateType, &event.AggregateID, &event.AggregateVersion, &event.DeviceID, &event.OperationID, &event.OperationOrdinal, &event.ReceivedAt, &event.OccurredAt, &event.PayloadID, &eventHash, &payloadHash, &event.Payload, &redactedAt, &redactedThrough, &event.ParentSessionID, &event.Source, &event.ArchiveDisposition, &event.EvidenceDisposition, &event.GoalRevisionID, &event.RouteRevisionID, &event.KnowledgeRevisionID, &event.ActivityID, &event.ActivityRevision); err != nil {
			return nil, fmt.Errorf("scan learning event: %w", err)
		}
		if !bytes.Equal(eventHash, payloadHash) {
			return nil, &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "event_payload_hash_mismatch"}
		}
		if redactedAt != nil && event.EventSequence <= redactedThrough {
			event.Payload = nil
			event.PayloadHash = hex.EncodeToString(eventHash)
			event.Redacted = true
			result = append(result, event)
			continue
		}
		canonical, err := canonicalJSON(event.Payload)
		if err != nil {
			return nil, &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "invalid_event_payload", Cause: err}
		}
		computed := learning.SHA256(canonical)
		if computed != hex.EncodeToString(eventHash) || computed != hex.EncodeToString(payloadHash) {
			return nil, &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "event_payload_hash_mismatch"}
		}
		event.Payload = canonical
		event.PayloadHash = computed
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate learning events: %w", err)
	}
	return result, nil
}

func replaceProjection(ctx context.Context, tx pgx.Tx, generationID string, projection learning.Projection, highWater int64, now time.Time) error {
	var generationStatus string
	var existingIncomplete bool
	var existingReasons []string
	if err := tx.QueryRow(ctx, `SELECT status,incomplete,reason_codes FROM learning_projection_generations WHERE id=$1 FOR UPDATE`, generationID).Scan(&generationStatus, &existingIncomplete, &existingReasons); err != nil {
		return fmt.Errorf("lock learning projection generation: %w", err)
	}
	for _, table := range []string{"learning_projection_timeline", "learning_projection_routes", "learning_projection_sessions", "learning_projection_nodes", "learning_projection_evidence", "learning_projection_reviews", "learning_projection_misconceptions", "learning_projection_stats", "learning_projection_carryovers"} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE generation_id=$1`, generationID); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	for _, item := range projection.Timeline {
		encoded, _ := json.Marshal(item)
		if _, err := tx.Exec(ctx, `INSERT INTO learning_projection_timeline(generation_id,event_seq,event_id,item) VALUES($1,$2,$3,$4)`, generationID, item.EventSequence, item.EventID, encoded); err != nil {
			return fmt.Errorf("project timeline: %w", err)
		}
	}
	for _, item := range projection.Routes {
		encoded, _ := json.Marshal(item)
		if _, err := tx.Exec(ctx, `INSERT INTO learning_projection_routes(generation_id,route_revision_id,route_id,revision,event_seq,is_current,item) VALUES($1,$2,$3,$4,$5,$6,$7)`, generationID, item.Route.ID, item.Route.RouteID, item.Route.Revision, item.EventSequence, item.Current, encoded); err != nil {
			return fmt.Errorf("project route: %w", err)
		}
	}
	for _, item := range projection.Sessions {
		encoded, _ := json.Marshal(item)
		if _, err := tx.Exec(ctx, `INSERT INTO learning_projection_sessions(generation_id,session_id,updated_event_seq,item) VALUES($1,$2,$3,$4)`, generationID, item.Session.ID, item.UpdatedEventSequence, encoded); err != nil {
			return fmt.Errorf("project session: %w", err)
		}
	}
	statSessions := make([]string, 0, len(projection.Stats))
	for sessionID := range projection.Stats {
		statSessions = append(statSessions, sessionID)
	}
	sort.Strings(statSessions)
	for _, sessionID := range statSessions {
		encoded, _ := json.Marshal(projection.Stats[sessionID])
		if _, err := tx.Exec(ctx, `INSERT INTO learning_projection_stats(generation_id,session_id,item) VALUES($1,$2,$3)`, generationID, sessionID, encoded); err != nil {
			return fmt.Errorf("project session stats: %w", err)
		}
	}
	nodes := make([]string, 0, len(projection.Nodes))
	for node := range projection.Nodes {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		item := projection.Nodes[node]
		encoded, _ := json.Marshal(item)
		if _, err := tx.Exec(ctx, `INSERT INTO learning_projection_nodes(generation_id,node_revision_id,updated_event_seq,item) VALUES($1,$2,$3,$4)`, generationID, node, highWater, encoded); err != nil {
			return fmt.Errorf("project node: %w", err)
		}
		if item.Review != nil {
			reviewJSON, _ := json.Marshal(item.Review)
			if _, err := tx.Exec(ctx, `INSERT INTO learning_projection_reviews(generation_id,node_revision_id,due_at,stable_id,item) VALUES($1,$2,$3,$4,$5)`, generationID, node, item.Review.DueAt, stableReviewID(node), reviewJSON); err != nil {
				return fmt.Errorf("project review: %w", err)
			}
		}
		for _, misconception := range item.Misconceptions {
			encoded, _ := json.Marshal(misconception)
			if _, err := tx.Exec(ctx, `INSERT INTO learning_projection_misconceptions(generation_id,misconception_id,node_revision_id,item) VALUES($1,$2,$3,$4)`, generationID, misconception.ID, node, encoded); err != nil {
				return fmt.Errorf("project misconception: %w", err)
			}
		}
	}
	for id, item := range projection.Evidence {
		encoded, _ := json.Marshal(item)
		if _, err := tx.Exec(ctx, `INSERT INTO learning_projection_evidence(generation_id,evidence_id,node_revision_id,received_at,accepted_event_seq,item) VALUES($1,$2,$3,$4,$5,$6)`, generationID, id, item.NodeRevisionID, item.ReceivedAt, item.AcceptedEventSequence, encoded); err != nil {
			return fmt.Errorf("project evidence: %w", err)
		}
	}
	carryoverIDs := make([]string, 0, len(projection.Carryovers))
	for id := range projection.Carryovers {
		carryoverIDs = append(carryoverIDs, id)
	}
	sort.Strings(carryoverIDs)
	for _, id := range carryoverIDs {
		item := projection.Carryovers[id]
		encoded, _ := json.Marshal(item)
		if _, err := tx.Exec(ctx, `INSERT INTO learning_projection_carryovers(generation_id,proposal_id,approved_event_seq,item) VALUES($1,$2,$3,$4)`, generationID, id, item.ApprovedEventSequence, encoded); err != nil {
			return fmt.Errorf("project evidence carryover: %w", err)
		}
	}
	fingerprint, err := learning.ProjectionFingerprint(projection)
	if err != nil {
		return fmt.Errorf("fingerprint learning projection: %w", err)
	}
	fingerprintBytes, _ := hex.DecodeString(fingerprint)
	incomplete := projection.Metadata.Incomplete
	reasons := normalizeTextArray(projection.Metadata.ReasonCodes)
	if generationStatus == "active" && existingIncomplete {
		for _, reason := range existingReasons {
			if reason != projectionVersionUpgradeReason {
				reasons = appendUnique(reasons, reason)
			}
		}
		incomplete = incomplete || len(reasons) > 0
	}
	if _, err := tx.Exec(ctx, `UPDATE learning_projection_generations SET projection_version=$2,reducer_version=$3,assessment_policy_version=$4,review_policy_version=$5,target_high_water=$6,checkpoint_event_seq=$6,fingerprint=$7,incomplete=$8,reason_codes=$9,completed_at=$10,knowledge_revision_id=$11 WHERE id=$1`, generationID, learning.ProjectionVersion, learning.MasteryReducerVersion, learning.AssessmentPolicyVersion, learning.ReviewPolicyVersion, highWater, fingerprintBytes, incomplete, reasons, now, optionalUUIDFilter(projection.Metadata.KnowledgeRevisionID)); err != nil {
		return fmt.Errorf("update learning projection generation: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO learning_projection_checkpoints(generation_id,event_seq,fingerprint,updated_at) VALUES($1,$2,$3,$4) ON CONFLICT(generation_id) DO UPDATE SET event_seq=EXCLUDED.event_seq,fingerprint=EXCLUDED.fingerprint,updated_at=EXCLUDED.updated_at`, generationID, highWater, fingerprintBytes, now); err != nil {
		return fmt.Errorf("update learning projection checkpoint: %w", err)
	}
	return nil
}

func (s *Store) Rebuild(ctx context.Context) (status learning.ProjectionStatus, resultErr error) {
	lease, err := s.beginRebuild(ctx)
	if err != nil {
		return learning.ProjectionStatus{}, err
	}
	workCtx, cancelWork := context.WithCancel(ctx)
	heartbeat := s.startRebuildHeartbeat(workCtx, cancelWork, lease)
	heartbeatStopped := false
	defer func() {
		if !heartbeatStopped {
			if heartbeatErr := heartbeat.stop(); heartbeatErr != nil {
				status = learning.ProjectionStatus{}
				resultErr = heartbeatErr
			}
		}
		cancelWork()
		if resultErr != nil {
			s.failRebuild(lease, resultErr)
		}
	}()

	readTx, err := s.pool.BeginTx(workCtx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return learning.ProjectionStatus{}, fmt.Errorf("begin rebuild replay snapshot: %w", err)
	}
	events, err := loadEvents(workCtx, readTx, 0, lease.target)
	if err == nil {
		err = readTx.Commit(workCtx)
	} else {
		_ = readTx.Rollback(context.Background())
	}
	if err != nil {
		return learning.ProjectionStatus{}, err
	}
	projection, err := learning.Replay(events, s.registry, lease.generationID)
	if err != nil {
		return learning.ProjectionStatus{}, err
	}
	if heartbeatErr := heartbeat.stop(); heartbeatErr != nil {
		heartbeatStopped = true
		return learning.ProjectionStatus{}, heartbeatErr
	}
	heartbeatStopped = true
	if err := s.renewRebuildLease(ctx, lease); err != nil {
		return learning.ProjectionStatus{}, err
	}
	if err := s.finishRebuild(ctx, lease, &projection); err != nil {
		return learning.ProjectionStatus{}, err
	}
	return s.ProjectionStatus(ctx)
}

func (h rebuildHeartbeat) stop() error {
	h.cancel()
	return <-h.done
}

func (s *Store) startRebuildHeartbeat(ctx context.Context, cancelWork context.CancelFunc, lease rebuildLease) rebuildHeartbeat {
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(rebuildHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				done <- nil
				return
			case <-ticker.C:
				if err := s.renewRebuildLease(heartbeatCtx, lease); err != nil {
					if heartbeatCtx.Err() != nil {
						done <- nil
					} else {
						done <- err
						cancelWork()
					}
					return
				}
			}
		}
	}()
	return rebuildHeartbeat{cancel: cancelHeartbeat, done: done}
}

func (s *Store) renewRebuildLease(ctx context.Context, lease rebuildLease) error {
	command, err := s.pool.Exec(ctx, `UPDATE learning_projection_head SET rebuild_lease_expires_at=clock_timestamp()+($3 * interval '1 microsecond'),updated_at=clock_timestamp() WHERE singleton_id=1 AND rebuilding_generation_id=$1 AND rebuild_lease_token=$2 AND rebuild_lease_expires_at>clock_timestamp()`, lease.generationID, lease.token, rebuildLeaseDuration.Microseconds())
	if err != nil {
		return fmt.Errorf("renew projection rebuild lease: %w", err)
	}
	if command.RowsAffected() != 1 {
		return &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "rebuild_ownership_lost"}
	}
	return nil
}

func (s *Store) beginRebuild(ctx context.Context) (rebuildLease, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return rebuildLease{}, fmt.Errorf("begin projection rebuild: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var rebuilding, existingToken *string
	var leaseExpires *time.Time
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT rebuilding_generation_id,rebuild_lease_token::text,rebuild_lease_expires_at,clock_timestamp() FROM learning_projection_head WHERE singleton_id=1 FOR UPDATE`).Scan(&rebuilding, &existingToken, &leaseExpires, &now); err != nil {
		return rebuildLease{}, fmt.Errorf("lock projection rebuild owner: %w", err)
	}
	now = now.UTC().Truncate(time.Microsecond)
	if rebuilding != nil {
		if existingToken != nil && leaseExpires != nil && leaseExpires.After(now) {
			return rebuildLease{}, &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "rebuild_in_progress"}
		}
		command, err := tx.Exec(ctx, `UPDATE learning_projection_generations SET status='failed',incomplete=TRUE,reason_codes=ARRAY['rebuild_lease_expired']::text[],completed_at=$2 WHERE id=$1 AND status='building'`, *rebuilding, now)
		if err != nil {
			return rebuildLease{}, fmt.Errorf("fail expired projection rebuild: %w", err)
		}
		if command.RowsAffected() != 1 {
			return rebuildLease{}, &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "rebuild_marker_invalid"}
		}
	}
	high, err := eventHighWater(ctx, tx, false)
	if err != nil {
		return rebuildLease{}, err
	}
	lease := rebuildLease{generationID: uuid.NewString(), token: uuid.NewString(), target: high}
	if _, err := tx.Exec(ctx, `INSERT INTO learning_projection_generations(id,projection_version,reducer_version,assessment_policy_version,review_policy_version,status,target_high_water,checkpoint_event_seq,created_at) VALUES($1,$2,$3,$4,$5,'building',$6,0,$7)`, lease.generationID, learning.ProjectionVersion, learning.MasteryReducerVersion, learning.AssessmentPolicyVersion, learning.ReviewPolicyVersion, high, now); err != nil {
		return rebuildLease{}, fmt.Errorf("create projection generation: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE learning_projection_head SET rebuilding_generation_id=$1,rebuild_lease_token=$2,rebuild_lease_expires_at=$3,updated_at=$4 WHERE singleton_id=1`, lease.generationID, lease.token, now.Add(rebuildLeaseDuration), now); err != nil {
		return rebuildLease{}, fmt.Errorf("mark projection rebuilding: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return rebuildLease{}, fmt.Errorf("commit projection rebuild marker: %w", err)
	}
	return lease, nil
}

func (s *Store) finishRebuild(ctx context.Context, lease rebuildLease, projection *learning.Projection) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin projection switch: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	lockedHigh, err := eventHighWater(ctx, tx, true)
	if err != nil {
		return err
	}
	var old string
	var rebuilding, token *string
	var leaseExpires *time.Time
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT active_generation_id,rebuilding_generation_id,rebuild_lease_token::text,rebuild_lease_expires_at,clock_timestamp() FROM learning_projection_head WHERE singleton_id=1 FOR UPDATE`).Scan(&old, &rebuilding, &token, &leaseExpires, &now); err != nil {
		return fmt.Errorf("lock projection head: %w", err)
	}
	now = now.UTC().Truncate(time.Microsecond)
	if rebuilding == nil || *rebuilding != lease.generationID || token == nil || *token != lease.token || leaseExpires == nil || !leaseExpires.After(now) {
		return &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "rebuild_ownership_lost"}
	}
	if lockedHigh > lease.target {
		tail, err := loadEvents(ctx, tx, lease.target, lockedHigh)
		if err != nil {
			return err
		}
		for _, event := range tail {
			if err := learning.ApplyEvent(projection, s.registry, event); err != nil {
				return err
			}
		}
	}
	if err := replaceProjection(ctx, tx, lease.generationID, *projection, lockedHigh, now); err != nil {
		return err
	}
	activeSeal, err := loadProjectionSeal(ctx, tx, old)
	if err != nil {
		return err
	}
	rebuildSeal, err := loadProjectionSeal(ctx, tx, lease.generationID)
	if err != nil {
		return err
	}
	if err := validateProjectionSwitch(lockedHigh, activeSeal, rebuildSeal); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE learning_projection_generations SET status='retired' WHERE id=$1 AND status='active'`, old)
	if err != nil {
		return fmt.Errorf("retire old projection generation: %w", err)
	}
	if command.RowsAffected() != 1 {
		return &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "active_generation_changed"}
	}
	command, err = tx.Exec(ctx, `UPDATE learning_projection_generations SET status='active',completed_at=$2 WHERE id=$1 AND status='building'`, lease.generationID, now)
	if err != nil {
		return fmt.Errorf("activate projection generation: %w", err)
	}
	if command.RowsAffected() != 1 {
		return &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "rebuild_ownership_lost"}
	}
	command, err = tx.Exec(ctx, `UPDATE learning_projection_head SET active_generation_id=$1,rebuilding_generation_id=NULL,rebuild_lease_token=NULL,rebuild_lease_expires_at=NULL,updated_at=$2 WHERE singleton_id=1 AND rebuilding_generation_id=$1 AND rebuild_lease_token=$3`, lease.generationID, now, lease.token)
	if err != nil {
		return fmt.Errorf("switch projection generation: %w", err)
	}
	if command.RowsAffected() != 1 {
		return &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "rebuild_ownership_lost"}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit projection rebuild: %w", err)
	}
	return nil
}

type projectionSeal struct {
	ProjectionVersion     string
	GenerationCheckpoint  int64
	Checkpoint            int64
	GenerationFingerprint []byte
	CheckpointFingerprint []byte
	Incomplete            bool
	ReasonCodes           []string
}

func loadProjectionSeal(ctx context.Context, db rowQuerier, generationID string) (projectionSeal, error) {
	var seal projectionSeal
	err := db.QueryRow(ctx, `SELECT g.projection_version,g.checkpoint_event_seq,c.event_seq,g.fingerprint,c.fingerprint,g.incomplete,g.reason_codes FROM learning_projection_generations g JOIN learning_projection_checkpoints c ON c.generation_id=g.id WHERE g.id=$1`, generationID).Scan(&seal.ProjectionVersion, &seal.GenerationCheckpoint, &seal.Checkpoint, &seal.GenerationFingerprint, &seal.CheckpointFingerprint, &seal.Incomplete, &seal.ReasonCodes)
	if err != nil {
		return seal, fmt.Errorf("read projection generation seal: %w", err)
	}
	return seal, nil
}

func validateProjectionSwitch(highWater int64, active, rebuild projectionSeal) error {
	if active.GenerationCheckpoint != highWater || active.Checkpoint != highWater || rebuild.GenerationCheckpoint != highWater || rebuild.Checkpoint != highWater {
		return &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "projection_checkpoint_mismatch"}
	}
	if len(active.GenerationFingerprint) == 0 || len(rebuild.GenerationFingerprint) == 0 ||
		!bytes.Equal(active.GenerationFingerprint, active.CheckpointFingerprint) ||
		!bytes.Equal(rebuild.GenerationFingerprint, rebuild.CheckpointFingerprint) {
		return &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "projection_fingerprint_mismatch"}
	}
	if bytes.Equal(active.GenerationFingerprint, rebuild.GenerationFingerprint) {
		return nil
	}
	if active.ProjectionVersion != learning.ProjectionVersion && rebuild.ProjectionVersion == learning.ProjectionVersion &&
		active.Incomplete && slices.Contains(active.ReasonCodes, projectionVersionUpgradeReason) {
		return nil
	}
	return &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: "projection_fingerprint_mismatch"}
}

func (s *Store) failRebuild(lease rebuildLease, cause error) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reason := "rebuild_failed"
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		reason = "rebuild_cancelled"
	} else if learning.ErrorCode(cause) == learning.CodeUnsupportedEventSchema {
		reason = learning.CodeUnsupportedEventSchema
	} else {
		var domainErr *learning.Error
		if errors.As(cause, &domainErr) && (domainErr.Reason == "projection_checkpoint_mismatch" || domainErr.Reason == "projection_fingerprint_mismatch") {
			reason = domainErr.Reason
		}
	}
	tx, err := s.pool.BeginTx(cleanupCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var rebuilding, token *string
	if err := tx.QueryRow(cleanupCtx, `SELECT rebuilding_generation_id,rebuild_lease_token::text FROM learning_projection_head WHERE singleton_id=1 FOR UPDATE`).Scan(&rebuilding, &token); err != nil || rebuilding == nil || *rebuilding != lease.generationID || token == nil || *token != lease.token {
		return
	}
	_, _ = tx.Exec(cleanupCtx, `UPDATE learning_projection_generations SET status='failed',incomplete=TRUE,reason_codes=ARRAY[$2]::text[],completed_at=$3 WHERE id=$1 AND status='building'`, lease.generationID, reason, now)
	_, _ = tx.Exec(cleanupCtx, `UPDATE learning_projection_head SET rebuilding_generation_id=NULL,rebuild_lease_token=NULL,rebuild_lease_expires_at=NULL,updated_at=$2 WHERE singleton_id=1 AND rebuilding_generation_id=$1 AND rebuild_lease_token=$3`, lease.generationID, now, lease.token)
	_, _ = tx.Exec(cleanupCtx, `UPDATE learning_projection_generations g SET incomplete=TRUE,reason_codes=CASE WHEN $1=ANY(g.reason_codes) THEN g.reason_codes ELSE array_append(g.reason_codes,$1) END FROM learning_projection_head h WHERE h.singleton_id=1 AND g.id=h.active_generation_id`, reason)
	_ = tx.Commit(cleanupCtx)
}

func (s *Store) ProjectionStatus(ctx context.Context) (learning.ProjectionStatus, error) {
	metadata, high, fingerprint, rebuilding, err := s.metadata(ctx)
	if err != nil {
		return learning.ProjectionStatus{}, err
	}
	return learning.ProjectionStatus{Metadata: metadata, HighWater: high, Fingerprint: fingerprint, ActiveGenerationID: metadata.GenerationID, RebuildingGenerationID: rebuilding}, nil
}

func (s *Store) metadata(ctx context.Context) (learning.ProjectionMetadata, int64, string, *string, error) {
	return metadataFrom(ctx, s.pool)
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func metadataFrom(ctx context.Context, db rowQuerier) (learning.ProjectionMetadata, int64, string, *string, error) {
	var metadata learning.ProjectionMetadata
	var high, checkpoint int64
	var fingerprint []byte
	var rebuilding *string
	var incomplete bool
	var reasons []string
	var knowledgeRevisionID *string
	err := db.QueryRow(ctx, `SELECT h.active_generation_id,h.rebuilding_generation_id,g.projection_version,g.reducer_version,g.assessment_policy_version,g.review_policy_version,g.knowledge_revision_id::text,g.checkpoint_event_seq,g.fingerprint,g.incomplete,g.reason_codes,c.current_event_seq FROM learning_projection_head h JOIN learning_projection_generations g ON g.id=h.active_generation_id JOIN learning_event_clock c ON c.singleton_id=1 WHERE h.singleton_id=1`).Scan(&metadata.GenerationID, &rebuilding, &metadata.ProjectionVersion, &metadata.MasteryReducerVersion, &metadata.AssessmentPolicy, &metadata.ReviewPolicy, &knowledgeRevisionID, &checkpoint, &fingerprint, &incomplete, &reasons, &high)
	if err != nil {
		return metadata, 0, "", nil, fmt.Errorf("read projection metadata: %w", err)
	}
	metadata.AsOfEventSequence = checkpoint
	metadata.KnowledgeRevisionID = deref(knowledgeRevisionID)
	metadata.Rebuilding = rebuilding != nil
	metadata.Incomplete = incomplete || checkpoint < high
	metadata.Degraded = metadata.Incomplete
	metadata.ReasonCodes = normalizeTextArray(reasons)
	if checkpoint < high {
		metadata.ReasonCodes = appendUnique(metadata.ReasonCodes, "checkpoint_lag")
	}
	return metadata, high, hex.EncodeToString(fingerprint), rebuilding, nil
}

func withProjectionRead[T any](ctx context.Context, s *Store, read func(pgx.Tx, learning.ProjectionMetadata) (T, error)) (T, error) {
	var zero T
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return zero, fmt.Errorf("begin projection read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning); err != nil {
		return zero, err
	}
	metadata, _, _, _, err := metadataFrom(ctx, tx)
	if err != nil {
		return zero, err
	}
	result, err := read(tx, metadata)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, fmt.Errorf("commit projection read: %w", err)
	}
	return result, nil
}

func (s *Store) LoadSessionAuthority(ctx context.Context, id string) (learning.SessionAuthority, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return learning.SessionAuthority{}, fmt.Errorf("begin session authority read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	session, err := s.tutoring.LoadSessionWith(ctx, tx, id)
	if err != nil {
		return learning.SessionAuthority{}, mapTutoringLoadError(err)
	}
	high, err := eventHighWater(ctx, tx, false)
	if err != nil {
		return learning.SessionAuthority{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return learning.SessionAuthority{}, fmt.Errorf("commit session authority read: %w", err)
	}
	return learning.SessionAuthority{Session: session, AsOfEventSequence: high}, nil
}

func (s *Store) LoadSession(ctx context.Context, id string) (tutoring.Session, error) {
	authority, err := s.LoadSessionAuthority(ctx, id)
	return authority.Session, err
}

func (s *Store) CurrentSession(ctx context.Context) (learning.SessionView, error) {
	return s.readSessionView(ctx, "", true)
}

func (s *Store) Session(ctx context.Context, id string) (learning.SessionView, error) {
	return s.readSessionView(ctx, id, false)
}

func sessionViewFromProjection(metadata learning.ProjectionMetadata, raw, rawStats []byte) (learning.SessionView, error) {
	var projected learning.SessionProjection
	if err := json.Unmarshal(raw, &projected); err != nil {
		return learning.SessionView{}, fmt.Errorf("decode projected session: %w", err)
	}
	estimate := learning.EstimateActiveTime(projected.Session.ID, nil)
	if len(rawStats) > 0 && string(rawStats) != "null" {
		if err := json.Unmarshal(rawStats, &estimate); err != nil {
			return learning.SessionView{}, fmt.Errorf("decode projected session stats: %w", err)
		}
	}
	return learning.SessionView{Metadata: metadata, Session: projected.Session, Estimate: estimate}, nil
}

func (s *Store) Timeline(ctx context.Context, query learning.TimelineQuery) (learning.TimelinePage, error) {
	return withProjectionRead(ctx, s, func(tx pgx.Tx, metadata learning.ProjectionMetadata) (learning.TimelinePage, error) {
		keys, err := decodeCursor(query.Page.Cursor, "timeline", metadata.GenerationID, metadata.AsOfEventSequence, 1)
		if err != nil {
			return learning.TimelinePage{}, err
		}
		var after int64
		if len(keys) > 0 {
			after, err = strconv.ParseInt(keys[0], 10, 64)
			if err != nil || after < 0 {
				return learning.TimelinePage{}, &learning.Error{Code: learning.CodeStaleCursor}
			}
		}
		limit := normalizeLimit(query.Page.Limit)
		rows, err := tx.Query(ctx, `SELECT event_seq,item FROM learning_projection_timeline WHERE generation_id=$1 AND event_seq>$2 AND ($3='' OR item->>'aggregate_id'=$3 OR item->>'parent_session_id'=$3) ORDER BY event_seq LIMIT $4`, metadata.GenerationID, after, query.SessionID, limit+1)
		if err != nil {
			return learning.TimelinePage{}, fmt.Errorf("query timeline: %w", err)
		}
		defer rows.Close()
		result := learning.TimelinePage{Metadata: metadata}
		var sequences []int64
		for rows.Next() {
			var sequence int64
			var raw []byte
			if err := rows.Scan(&sequence, &raw); err != nil {
				return result, err
			}
			var item learning.TimelineItem
			if err := json.Unmarshal(raw, &item); err != nil {
				return result, err
			}
			result.Items = append(result.Items, item)
			sequences = append(sequences, sequence)
		}
		if err := rows.Err(); err != nil {
			return result, err
		}
		if len(result.Items) > limit {
			result.Items = result.Items[:limit]
			result.NextCursor = encodeCursor("timeline", metadata.GenerationID, metadata.AsOfEventSequence, strconv.FormatInt(sequences[limit-1], 10))
		}
		return result, nil
	})
}
func (s *Store) Routes(ctx context.Context, page learning.CursorPageRequest) (learning.RoutesPage, error) {
	return withProjectionRead(ctx, s, func(tx pgx.Tx, metadata learning.ProjectionMetadata) (learning.RoutesPage, error) {
		cursorKind := "routes"
		if page.CurrentOnly {
			cursorKind = "routes-current"
		}
		keys, err := decodeCursor(page.Cursor, cursorKind, metadata.GenerationID, metadata.AsOfEventSequence, 2)
		if err != nil {
			return learning.RoutesPage{}, err
		}
		var afterSeq int64
		afterID := uuid.Nil.String()
		if len(keys) > 0 {
			afterSeq, err = strconv.ParseInt(keys[0], 10, 64)
			if err != nil || afterSeq < 0 || uuid.Validate(keys[1]) != nil {
				return learning.RoutesPage{}, &learning.Error{Code: learning.CodeStaleCursor}
			}
			afterID = keys[1]
		}
		limit := normalizeLimit(page.Limit)
		rows, err := tx.Query(ctx, `SELECT event_seq,route_revision_id,item FROM learning_projection_routes WHERE generation_id=$1 AND (event_seq,route_revision_id)>($2,$3) AND (NOT $4 OR is_current) ORDER BY event_seq,route_revision_id LIMIT $5`, metadata.GenerationID, afterSeq, afterID, page.CurrentOnly, limit+1)
		if err != nil {
			return learning.RoutesPage{}, err
		}
		defer rows.Close()
		result := learning.RoutesPage{Metadata: metadata}
		type key struct {
			sequence int64
			id       string
		}
		var positions []key
		for rows.Next() {
			var position key
			var raw []byte
			if err := rows.Scan(&position.sequence, &position.id, &raw); err != nil {
				return result, err
			}
			var item learning.RouteProjection
			if err := json.Unmarshal(raw, &item); err != nil {
				return result, err
			}
			result.Items = append(result.Items, item)
			positions = append(positions, position)
		}
		if err := rows.Err(); err != nil {
			return result, err
		}
		if len(result.Items) > limit {
			result.Items = result.Items[:limit]
			last := positions[limit-1]
			result.NextCursor = encodeCursor(cursorKind, metadata.GenerationID, metadata.AsOfEventSequence, strconv.FormatInt(last.sequence, 10), last.id)
		}
		return result, nil
	})
}
func (s *Store) Node(ctx context.Context, node string) (learning.NodeView, error) {
	return withProjectionRead(ctx, s, func(tx pgx.Tx, metadata learning.ProjectionMetadata) (learning.NodeView, error) {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT item FROM learning_projection_nodes WHERE generation_id=$1 AND node_revision_id=$2`, metadata.GenerationID, node).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return learning.NodeView{}, &learning.Error{Code: learning.CodeNotFound}
		}
		if err != nil {
			return learning.NodeView{}, err
		}
		var reduction learning.NodeReduction
		if err := json.Unmarshal(raw, &reduction); err != nil {
			return learning.NodeView{}, err
		}
		rows, err := tx.Query(ctx, `SELECT item FROM learning_projection_evidence WHERE generation_id=$1 AND node_revision_id=$2 ORDER BY accepted_event_seq,evidence_id`, metadata.GenerationID, node)
		if err != nil {
			return learning.NodeView{}, err
		}
		defer rows.Close()
		var evidence []learning.AcceptedEvidence
		for rows.Next() {
			var encoded []byte
			if err := rows.Scan(&encoded); err != nil {
				return learning.NodeView{}, err
			}
			var item learning.AcceptedEvidence
			if err := json.Unmarshal(encoded, &item); err != nil {
				return learning.NodeView{}, err
			}
			evidence = append(evidence, item)
		}
		if err := rows.Err(); err != nil {
			return learning.NodeView{}, err
		}
		return learning.NodeView{Metadata: metadata, Node: reduction, Evidence: evidence}, nil
	})
}
func (s *Store) EvidenceList(ctx context.Context, query learning.EvidenceQuery) (learning.EvidencePage, error) {
	return withProjectionRead(ctx, s, func(tx pgx.Tx, metadata learning.ProjectionMetadata) (learning.EvidencePage, error) {
		keys, err := decodeCursor(query.Page.Cursor, "evidence", metadata.GenerationID, metadata.AsOfEventSequence, 2)
		if err != nil {
			return learning.EvidencePage{}, err
		}
		afterTime := time.Time{}
		afterID := uuid.Nil.String()
		if len(keys) > 0 {
			afterTime, err = time.Parse(time.RFC3339Nano, keys[0])
			if err != nil || uuid.Validate(keys[1]) != nil {
				return learning.EvidencePage{}, &learning.Error{Code: learning.CodeStaleCursor}
			}
			afterID = keys[1]
		}
		limit := normalizeLimit(query.Page.Limit)
		nodeRevisionID := optionalUUIDFilter(query.NodeRevisionID)
		rows, err := tx.Query(ctx, `SELECT received_at,evidence_id,item FROM learning_projection_evidence WHERE generation_id=$1 AND ($2::uuid IS NULL OR node_revision_id=$2::uuid) AND (received_at,evidence_id)>($3,$4) ORDER BY received_at,evidence_id LIMIT $5`, metadata.GenerationID, nodeRevisionID, afterTime, afterID, limit+1)
		if err != nil {
			return learning.EvidencePage{}, err
		}
		defer rows.Close()
		result := learning.EvidencePage{Metadata: metadata}
		type key struct {
			received time.Time
			id       string
		}
		var positions []key
		for rows.Next() {
			var position key
			var raw []byte
			if err := rows.Scan(&position.received, &position.id, &raw); err != nil {
				return result, err
			}
			var item learning.AcceptedEvidence
			if err := json.Unmarshal(raw, &item); err != nil {
				return result, err
			}
			result.Items = append(result.Items, item)
			positions = append(positions, position)
		}
		if err := rows.Err(); err != nil {
			return result, err
		}
		if len(result.Items) > limit {
			result.Items = result.Items[:limit]
			last := positions[limit-1]
			result.NextCursor = encodeCursor("evidence", metadata.GenerationID, metadata.AsOfEventSequence, last.received.UTC().Format(time.RFC3339Nano), last.id)
		}
		return result, nil
	})
}
func (s *Store) Reviews(ctx context.Context, query learning.ReviewQuery) (learning.ReviewsPage, error) {
	return withProjectionRead(ctx, s, func(tx pgx.Tx, metadata learning.ProjectionMetadata) (learning.ReviewsPage, error) {
		keys, err := decodeCursor(query.Page.Cursor, "reviews", metadata.GenerationID, metadata.AsOfEventSequence, 3)
		if err != nil {
			return learning.ReviewsPage{}, err
		}
		afterDue := time.Time{}
		afterNode := uuid.Nil.String()
		afterStable := uuid.Nil.String()
		if len(keys) > 0 {
			afterDue, err = time.Parse(time.RFC3339Nano, keys[0])
			if err != nil || uuid.Validate(keys[1]) != nil || uuid.Validate(keys[2]) != nil {
				return learning.ReviewsPage{}, &learning.Error{Code: learning.CodeStaleCursor}
			}
			afterNode, afterStable = keys[1], keys[2]
		}
		limit := normalizeLimit(query.Page.Limit)
		due := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		if query.DueBefore != nil {
			due = query.DueBefore.UTC()
		}
		rows, err := tx.Query(ctx, `SELECT due_at,node_revision_id,stable_id,item FROM learning_projection_reviews WHERE generation_id=$1 AND due_at<=$2 AND (due_at,node_revision_id,stable_id)>($3,$4,$5) ORDER BY due_at,node_revision_id,stable_id LIMIT $6`, metadata.GenerationID, due, afterDue, afterNode, afterStable, limit+1)
		if err != nil {
			return learning.ReviewsPage{}, err
		}
		defer rows.Close()
		result := learning.ReviewsPage{Metadata: metadata}
		type key struct {
			due          time.Time
			node, stable string
		}
		var positions []key
		for rows.Next() {
			var position key
			var raw []byte
			if err := rows.Scan(&position.due, &position.node, &position.stable, &raw); err != nil {
				return result, err
			}
			var item learning.ReviewSchedule
			if err := json.Unmarshal(raw, &item); err != nil {
				return result, err
			}
			result.Items = append(result.Items, item)
			positions = append(positions, position)
		}
		if err := rows.Err(); err != nil {
			return result, err
		}
		if len(result.Items) > limit {
			result.Items = result.Items[:limit]
			last := positions[limit-1]
			result.NextCursor = encodeCursor("reviews", metadata.GenerationID, metadata.AsOfEventSequence, last.due.UTC().Format(time.RFC3339Nano), last.node, last.stable)
		}
		return result, nil
	})
}

func optionalUUIDFilter(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func normalizeTextArray(values []string) []string {
	normalized := make([]string, len(values))
	copy(normalized, values)
	return normalized
}

func appendUnique(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}
