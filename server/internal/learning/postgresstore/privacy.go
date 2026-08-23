package postgresstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var redactedProjectionNamespace = uuid.MustParse("8c3cd916-16fd-4450-b073-2d640c17cf89")

var _ privacy.LocalOwnerPort = (*Store)(nil)
var _ privacy.RedactionEventAppender = (*Store)(nil)

var redactionEventNamespace = uuid.MustParse("c52d28ac-f341-4ba2-846a-707681b19391")

func (s *Store) Owner() privacy.OwnerKind { return privacy.OwnerLearning }

func (s *Store) AppendEventRedactedTx(ctx context.Context, db privacy.DBTX, request privacy.RedactionEventAppendRequest) (privacy.RedactionEventAppendResult, error) {
	if db == nil {
		return privacy.RedactionEventAppendResult{}, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "redaction_event_database_required"}
	}
	if err := request.Validate(); err != nil {
		return privacy.RedactionEventAppendResult{}, err
	}
	var through int64
	if err := db.QueryRow(ctx, `SELECT current_event_seq FROM learning_event_clock WHERE singleton_id=1 FOR UPDATE`).Scan(&through); err != nil {
		return privacy.RedactionEventAppendResult{}, fmt.Errorf("lock learning event clock for redaction: %w", err)
	}
	payload := privacy.RedactionPayload{ErasureID: request.ErasureID, Generation: request.LearnerGeneration, RedactedThroughEventSeq: through, PolicyVersion: privacy.PolicyVersion, ReasonCode: request.ReasonCode}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return privacy.RedactionEventAppendResult{}, fmt.Errorf("encode learning redaction event: %w", err)
	}
	encoded, err = canonicalJSON(encoded)
	if err != nil {
		return privacy.RedactionEventAppendResult{}, fmt.Errorf("canonicalize learning redaction event: %w", err)
	}
	payloadHash := sha256.Sum256(encoded)
	eventID := uuid.NewSHA1(redactionEventNamespace, []byte(request.ErasureID)).String()
	payloadID := uuid.NewSHA1(redactionEventNamespace, []byte("payload\n"+request.ErasureID)).String()
	sequence := through + 1
	if _, err := db.Exec(ctx, `INSERT INTO learning_aggregate_heads(aggregate_type,aggregate_id,aggregate_version,last_event_seq,updated_at) VALUES('privacy',$1,1,$2,$3)`, request.ErasureID, sequence, request.At.UTC()); err != nil {
		return privacy.RedactionEventAppendResult{}, fmt.Errorf("insert privacy learning aggregate head: %w", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO learning_event_payloads(id,payload,payload_hash,created_at) VALUES($1,$2,$3,$4)`, payloadID, encoded, payloadHash[:], request.At.UTC()); err != nil {
		return privacy.RedactionEventAppendResult{}, fmt.Errorf("insert learning redaction payload: %w", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO learning_events(event_seq,id,event_type,event_schema_version,aggregate_type,aggregate_id,aggregate_version,device_id,operation_id,operation_ordinal,received_at,payload_id,payload_hash) VALUES($1,$2,'EventRedacted',$3,'privacy',$4,1,$5,$6,0,$7,$8,$9)`, sequence, eventID, learning.EventRedactedSchemaVersion, request.ErasureID, request.ActorDeviceID, request.OperationID, request.At.UTC(), payloadID, payloadHash[:]); err != nil {
		return privacy.RedactionEventAppendResult{}, fmt.Errorf("insert canonical learning redaction event: %w", err)
	}
	if _, err := db.Exec(ctx, `UPDATE learning_event_clock SET current_event_seq=$1,updated_at=$2 WHERE singleton_id=1`, sequence, request.At.UTC()); err != nil {
		return privacy.RedactionEventAppendResult{}, fmt.Errorf("advance learning event clock for redaction: %w", err)
	}
	return privacy.RedactionEventAppendResult{EventID: eventID, RedactedThroughEvent: through}, nil
}

func (s *Store) CloseGenerationTx(ctx context.Context, db privacy.DBTX, transition privacy.GenerationTransition) error {
	if db == nil {
		return &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "learning_generation_database_required"}
	}
	if err := transition.Validate(false); err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET learner_generation=$3,read_open=FALSE,write_open=FALSE,active_erasure_id=$1,updated_at=$4
		WHERE owner_kind='learning' AND learner_generation=$2
		  AND read_open AND write_open AND active_erasure_id IS NULL`,
		transition.ErasureID, transition.FromGeneration, transition.TargetGeneration, transition.At.UTC())
	if err != nil {
		return fmt.Errorf("close learning privacy generation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &privacy.Error{Code: privacy.CodeErasureInProgress, Reason: "learning_generation_close_cas_failed"}
	}
	return nil
}

func (s *Store) OpenGenerationTx(ctx context.Context, db privacy.DBTX, transition privacy.GenerationTransition) error {
	if db == nil {
		return &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "learning_generation_database_required"}
	}
	if err := transition.Validate(true); err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `
		UPDATE privacy_owner_generation_gates g
		SET read_open=TRUE,write_open=TRUE,active_erasure_id=NULL,updated_at=$4
		WHERE g.owner_kind='learning' AND g.learner_generation=$2
		  AND NOT g.read_open AND NOT g.write_open AND g.active_erasure_id=$1
		  AND EXISTS (
			SELECT 1
			FROM privacy_erasure_receipt_heads h
			JOIN privacy_erasure_step_receipts r
			  ON r.id=h.current_receipt_id AND r.erasure_id=h.erasure_id AND r.store_kind=h.store_kind
			WHERE h.erasure_id=$1 AND h.current_receipt_id=$3
			  AND h.store_kind IN ('learning_event_payload','learning_typed_payload','projection_generations')
			  AND r.status IN ('succeeded','not_applicable')
		  )
		  AND (
			SELECT count(*)=3 AND bool_and(r.status IN ('succeeded','not_applicable'))
			FROM privacy_erasure_receipt_heads h
			JOIN privacy_erasure_step_receipts r
			  ON r.id=h.current_receipt_id AND r.erasure_id=h.erasure_id AND r.store_kind=h.store_kind
			WHERE h.erasure_id=$1
			  AND h.store_kind IN ('learning_event_payload','learning_typed_payload','projection_generations')
		  )`, transition.ErasureID, transition.TargetGeneration, transition.ReceiptID, transition.At.UTC())
	if err != nil {
		return fmt.Errorf("open learning privacy generation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "learning_generation_open_cas_failed"}
	}
	return nil
}

func (s *Store) RedactTx(ctx context.Context, request privacy.LocalRedactionRequest) error {
	if err := request.Validate(privacy.OwnerLearning); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin learning privacy redaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var permit string
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT privacy_begin_owner_scrub($1,$2,'learning',$3)::text,clock_timestamp()`, request.ErasureID, request.LearnerGeneration, request.ReceiptID).Scan(&permit, &now); err != nil {
		return fmt.Errorf("authorize learning privacy redaction: %w", err)
	}

	switch request.Store {
	case privacy.StoreLearningEventPayload:
		if err := redactLearningEventPayloads(ctx, tx, request, now); err != nil {
			return err
		}
	case privacy.StoreLearningTypedPayload:
		if err := redactLearningTypedPayloads(ctx, tx); err != nil {
			return err
		}
	case privacy.StoreProjectionGenerations:
		if err := s.redactLearningProjections(ctx, tx, request, now); err != nil {
			return err
		}
	default:
		return &privacy.Error{Code: privacy.CodeUnsupportedReceiptStore, Reason: string(request.Store)}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit learning privacy redaction: %w", err)
	}
	return nil
}

func redactLearningEventPayloads(ctx context.Context, tx pgx.Tx, request privacy.LocalRedactionRequest, now time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE learning_event_payloads p
		SET payload='{"redacted":true}'::jsonb,redacted_at=$2
		FROM learning_events e
		WHERE e.payload_id=p.id AND e.event_seq<=$1
		  AND (p.redacted_at IS NULL OR p.payload<>'{"redacted":true}'::jsonb)`, request.RedactedThroughEvent, now.UTC())
	if err != nil {
		return fmt.Errorf("redact learning event payloads: %w", err)
	}
	return nil
}

func redactLearningTypedPayloads(ctx context.Context, tx pgx.Tx) error {
	statements := []struct {
		name string
		sql  string
	}{
		{"learning inbox", `UPDATE learning_inbox SET result='{"redacted":true}'::jsonb`},
		{"learning goals", `UPDATE learning_goal_revisions SET goal_text='[redacted]',source='privacy_erasure'`},
		{"learning route steps", `UPDATE learning_route_steps SET teaching_intent='[redacted]',completion_condition='[redacted]'`},
		{"learning activities", `UPDATE learning_activities SET prompt='[redacted]',rubric_revision='[redacted]',rubric='{"redacted":true}'::jsonb`},
		{"learning activity references", `UPDATE learning_activity_references SET source_start=0,source_end=1,slice_text='',slice_hash=decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex')`},
		{"learning attempt payloads", `UPDATE learning_attempt_payloads SET answer_text='',payload_hash=decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex')`},
		{"learning attempts", `UPDATE learning_attempts SET payload_hash=decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex')`},
		{"learning assessments", `UPDATE learning_assessments SET rubric_complete=FALSE,confidence=0,risk_flags='{}'::text[],trusted_model_id='[redacted]',model_parameters='{}'::jsonb,prompt_revision='[redacted]',proposal_input_hash=decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex'),model_attempts=1,attempt_categories='{}'::text[]`},
		{"learning assessment item keys", `UPDATE learning_assessment_items SET rubric_item_id='privacy-stage-'||assessment_id::text||'-'||ordinal::text`},
		{"learning assessment items", `UPDATE learning_assessment_items SET rubric_item_id='redacted-'||ordinal::text,conclusion='unassessed',answer_start=0,answer_end=0,answer_quote='',answer_quote_hash=decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex'),knowledge_revision_id=NULL,knowledge_node_revision_id=NULL,knowledge_node_id=NULL,knowledge_document_revision_id=NULL,knowledge_start=NULL,knowledge_end=NULL,knowledge_quote=NULL,knowledge_quote_hash=NULL,misconception_candidate=NULL`},
		{"learning assessment decisions", `UPDATE learning_assessment_decisions SET conclusions='{"redacted":true}'::jsonb,reason=NULL`},
		{"learning evidence", `UPDATE learning_evidence SET rubric_revision='[redacted]',outcome='partial',help_level='none',misconception_candidates='[]'::jsonb,rubric_outcomes='[]'::jsonb`},
		{"learning evidence invalidations", `UPDATE learning_evidence_invalidations SET reason='privacy_erasure'`},
		{"learning exposures", `UPDATE learning_exposures SET content='',references_snapshot='[]'::jsonb`},
		{"learning misconceptions", `UPDATE learning_misconception_revisions SET rubric_item_id='[redacted]',candidate_hash=decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex'),candidate_text='',status='resolved',source_evidence_ids='{}'::uuid[],counter_evidence_ids='{}'::uuid[]`},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.sql); err != nil {
			return fmt.Errorf("redact %s: %w", statement.name, err)
		}
	}
	return nil
}

func (s *Store) redactLearningProjections(ctx context.Context, tx pgx.Tx, request privacy.LocalRedactionRequest, now time.Time) error {
	event, err := loadCanonicalRedactionEvent(ctx, tx, request)
	if err != nil {
		return err
	}
	generationID := redactedProjectionGenerationID(request)
	var oldActive string
	if err := tx.QueryRow(ctx, `SELECT active_generation_id::text FROM learning_projection_head WHERE singleton_id=1 FOR UPDATE`).Scan(&oldActive); err != nil {
		return fmt.Errorf("lock learning projection head for redaction: %w", err)
	}

	for _, table := range []string{
		"learning_projection_timeline", "learning_projection_routes", "learning_projection_sessions",
		"learning_projection_nodes", "learning_projection_evidence", "learning_projection_reviews",
		"learning_projection_misconceptions", "learning_projection_stats",
	} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+table); err != nil {
			return fmt.Errorf("clear redacted %s: %w", table, err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM learning_projection_checkpoints`); err != nil {
		return fmt.Errorf("clear redacted learning projection checkpoints: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE learning_projection_generations
		SET status='retired',target_high_water=0,checkpoint_event_seq=0,fingerprint=NULL,
		    incomplete=TRUE,
		    reason_codes=CASE WHEN 'content_redacted'=ANY(reason_codes) THEN reason_codes ELSE array_append(reason_codes,'content_redacted') END,
		    knowledge_revision_id=NULL,completed_at=$2
		WHERE id<>$1`, generationID, now.UTC()); err != nil {
		return fmt.Errorf("retire redacted learning projection generations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO learning_projection_generations(
			id,projection_version,reducer_version,assessment_policy_version,review_policy_version,
			status,target_high_water,checkpoint_event_seq,fingerprint,incomplete,reason_codes,created_at,completed_at)
		VALUES($1,$2,$3,$4,$5,'building',$6,0,NULL,FALSE,'{}'::text[],$7,NULL)
		ON CONFLICT(id) DO UPDATE SET
			projection_version=EXCLUDED.projection_version,reducer_version=EXCLUDED.reducer_version,
			assessment_policy_version=EXCLUDED.assessment_policy_version,review_policy_version=EXCLUDED.review_policy_version,
			knowledge_revision_id=NULL,status='building',target_high_water=EXCLUDED.target_high_water,
			checkpoint_event_seq=0,fingerprint=NULL,incomplete=FALSE,reason_codes='{}'::text[],
			created_at=EXCLUDED.created_at,completed_at=NULL`,
		generationID, learning.ProjectionVersion, learning.MasteryReducerVersion, learning.AssessmentPolicyVersion,
		learning.ReviewPolicyVersion, event.EventSequence, now.UTC()); err != nil {
		return fmt.Errorf("create redacted learning projection generation: %w", err)
	}

	projection, err := learning.Replay([]learning.LearningEvent{event}, s.registry, generationID)
	if err != nil {
		return fmt.Errorf("replay canonical learning redaction event: %w", err)
	}
	if err := replaceProjection(ctx, tx, generationID, projection, event.EventSequence, now.UTC()); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE learning_projection_generations SET status='active',completed_at=$2 WHERE id=$1 AND status='building'`, generationID, now.UTC())
	if err != nil {
		return fmt.Errorf("activate redacted learning projection generation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &privacy.Error{Code: privacy.CodeVerificationFailed, Reason: "redacted_projection_activation_cas_failed"}
	}
	tag, err = tx.Exec(ctx, `
		UPDATE learning_projection_head
		SET active_generation_id=$1,rebuilding_generation_id=NULL,rebuild_lease_token=NULL,rebuild_lease_expires_at=NULL,updated_at=$3
		WHERE singleton_id=1 AND active_generation_id=$2`, generationID, oldActive, now.UTC())
	if err != nil {
		return fmt.Errorf("switch redacted learning projection generation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &privacy.Error{Code: privacy.CodeVerificationFailed, Reason: "redacted_projection_head_cas_failed"}
	}
	return nil
}

type redactionEventDB interface {
	eventDB
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadCanonicalRedactionEvent(ctx context.Context, db redactionEventDB, request privacy.LocalRedactionRequest) (learning.LearningEvent, error) {
	var eventID, policyVersion, reasonCode string
	if err := db.QueryRow(ctx, `
		SELECT event_id::text,policy_version,reason_code
		FROM privacy_redaction_barriers
		WHERE erasure_id=$1 AND learner_generation=$2 AND redacted_through_event_seq=$3`,
		request.ErasureID, request.LearnerGeneration, request.RedactedThroughEvent).Scan(&eventID, &policyVersion, &reasonCode); err != nil {
		return learning.LearningEvent{}, &privacy.Error{Code: privacy.CodeVerificationFailed, Reason: "canonical_redaction_barrier_missing", Cause: err}
	}
	events, err := loadEvents(ctx, db, request.RedactedThroughEvent, request.RedactedThroughEvent+1)
	if err != nil {
		return learning.LearningEvent{}, err
	}
	if len(events) != 1 {
		return learning.LearningEvent{}, &privacy.Error{Code: privacy.CodeVerificationFailed, Reason: "canonical_redaction_event_count_invalid"}
	}
	return validateCanonicalRedactionEvent(events[0], request, eventID, policyVersion, reasonCode)
}

func validateCanonicalRedactionEvent(event learning.LearningEvent, request privacy.LocalRedactionRequest, eventID, policyVersion, reasonCode string) (learning.LearningEvent, error) {
	decoded, err := learning.NewEventRegistry().Decode(event)
	if err != nil {
		return learning.LearningEvent{}, &privacy.Error{Code: privacy.CodeVerificationFailed, Reason: "canonical_redaction_payload_invalid", Cause: err}
	}
	var payload privacy.RedactionPayload
	if err := json.Unmarshal(decoded.Payload, &payload); err != nil {
		return learning.LearningEvent{}, &privacy.Error{Code: privacy.CodeVerificationFailed, Reason: "canonical_redaction_payload_invalid", Cause: err}
	}
	if err := payload.Validate(); err != nil {
		return learning.LearningEvent{}, err
	}
	if decoded.ID != eventID || decoded.EventSequence != request.RedactedThroughEvent+1 || decoded.Type != learning.EventRedacted || (decoded.SchemaVersion != learning.EventSchemaVersion && decoded.SchemaVersion != learning.EventRedactedSchemaVersion) || decoded.AggregateType != "privacy" || decoded.AggregateID != request.ErasureID || decoded.Redacted || payload.ErasureID != request.ErasureID || payload.Generation != request.LearnerGeneration || payload.RedactedThroughEventSeq != request.RedactedThroughEvent || payload.PolicyVersion != policyVersion || payload.ReasonCode != reasonCode {
		return learning.LearningEvent{}, &privacy.Error{Code: privacy.CodeVerificationFailed, Reason: "canonical_redaction_event_mismatch"}
	}
	// Replay must decode the canonical payload as the canonical schema. The
	// stored schema version remains unchanged in learning_events for audit.
	decoded.SchemaVersion = learning.EventRedactedSchemaVersion
	return decoded, nil
}

func redactedProjectionGenerationID(request privacy.LocalRedactionRequest) string {
	return uuid.NewSHA1(redactedProjectionNamespace, []byte(fmt.Sprintf("%s\n%d", request.ErasureID, request.LearnerGeneration))).String()
}

func (s *Store) VerifyRedacted(ctx context.Context, request privacy.LocalRedactionRequest) (int64, error) {
	if err := request.Validate(privacy.OwnerLearning); err != nil {
		return 0, err
	}
	switch request.Store {
	case privacy.StoreLearningEventPayload:
		return verifyLearningEventPayloads(ctx, s.pool, request)
	case privacy.StoreLearningTypedPayload:
		return verifyLearningTypedPayloads(ctx, s.pool)
	case privacy.StoreProjectionGenerations:
		return s.verifyLearningProjections(ctx, request)
	default:
		return 0, &privacy.Error{Code: privacy.CodeUnsupportedReceiptStore, Reason: string(request.Store)}
	}
}

func verifyLearningEventPayloads(ctx context.Context, db redactionEventDB, request privacy.LocalRedactionRequest) (int64, error) {
	var remaining int64
	if err := db.QueryRow(ctx, `
		SELECT count(*)
		FROM learning_events e
		JOIN learning_event_payloads p ON p.id=e.payload_id
		WHERE e.event_seq<=$1
		  AND (p.redacted_at IS NULL OR p.payload<>'{"redacted":true}'::jsonb)`, request.RedactedThroughEvent).Scan(&remaining); err != nil {
		return 0, fmt.Errorf("verify learning event payload redaction: %w", err)
	}
	return remaining, nil
}

func verifyLearningTypedPayloads(ctx context.Context, db redactionEventDB) (int64, error) {
	var remaining int64
	err := db.QueryRow(ctx, `
		SELECT COALESCE(sum(remaining),0)::bigint FROM (
			SELECT count(*)::bigint AS remaining FROM learning_inbox WHERE result<>'{"redacted":true}'::jsonb
			UNION ALL SELECT count(*) FROM learning_goal_revisions WHERE goal_text<>'[redacted]' OR source<>'privacy_erasure'
			UNION ALL SELECT count(*) FROM learning_route_steps WHERE teaching_intent<>'[redacted]' OR completion_condition<>'[redacted]'
			UNION ALL SELECT count(*) FROM learning_activities WHERE prompt<>'[redacted]' OR rubric_revision<>'[redacted]' OR rubric<>'{"redacted":true}'::jsonb
			UNION ALL SELECT count(*) FROM learning_activity_references WHERE source_start<>0 OR source_end<>1 OR slice_text<>'' OR slice_hash<>decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex')
			UNION ALL SELECT count(*) FROM learning_attempt_payloads WHERE answer_text<>'' OR payload_hash<>decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex')
			UNION ALL SELECT count(*) FROM learning_attempts WHERE payload_hash<>decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex')
			UNION ALL SELECT count(*) FROM learning_assessments WHERE rubric_complete OR confidence<>0 OR risk_flags<>'{}'::text[] OR trusted_model_id<>'[redacted]' OR model_parameters<>'{}'::jsonb OR prompt_revision<>'[redacted]' OR proposal_input_hash<>decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex') OR model_attempts<>1 OR attempt_categories<>'{}'::text[]
			UNION ALL SELECT count(*) FROM learning_assessment_items WHERE rubric_item_id<>'redacted-'||ordinal::text OR conclusion<>'unassessed' OR answer_start<>0 OR answer_end<>0 OR answer_quote<>'' OR answer_quote_hash<>decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex') OR knowledge_revision_id IS NOT NULL OR knowledge_node_revision_id IS NOT NULL OR knowledge_node_id IS NOT NULL OR knowledge_document_revision_id IS NOT NULL OR knowledge_start IS NOT NULL OR knowledge_end IS NOT NULL OR knowledge_quote IS NOT NULL OR knowledge_quote_hash IS NOT NULL OR misconception_candidate IS NOT NULL
			UNION ALL SELECT count(*) FROM learning_assessment_decisions WHERE conclusions<>'{"redacted":true}'::jsonb OR reason IS NOT NULL
			UNION ALL SELECT count(*) FROM learning_evidence WHERE rubric_revision<>'[redacted]' OR outcome<>'partial' OR help_level<>'none' OR misconception_candidates<>'[]'::jsonb OR rubric_outcomes<>'[]'::jsonb
			UNION ALL SELECT count(*) FROM learning_evidence_invalidations WHERE reason<>'privacy_erasure'
			UNION ALL SELECT count(*) FROM learning_exposures WHERE content<>'' OR references_snapshot<>'[]'::jsonb
			UNION ALL SELECT count(*) FROM learning_misconception_revisions WHERE rubric_item_id<>'[redacted]' OR candidate_hash<>decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex') OR candidate_text<>'' OR status<>'resolved' OR source_evidence_ids<>'{}'::uuid[] OR counter_evidence_ids<>'{}'::uuid[]
		) residuals`).Scan(&remaining)
	if err != nil {
		return 0, fmt.Errorf("verify learning typed payload redaction: %w", err)
	}
	return remaining, nil
}

func (s *Store) verifyLearningProjections(ctx context.Context, request privacy.LocalRedactionRequest) (int64, error) {
	event, err := loadCanonicalRedactionEvent(ctx, s.pool, request)
	if err != nil {
		return 0, err
	}
	expectedProjection, err := learning.Replay([]learning.LearningEvent{event}, s.registry, redactedProjectionGenerationID(request))
	if err != nil {
		return 0, fmt.Errorf("replay learning redaction event for verification: %w", err)
	}
	expectedFingerprint, err := learning.ProjectionFingerprint(expectedProjection)
	if err != nil {
		return 0, fmt.Errorf("fingerprint learning redaction projection for verification: %w", err)
	}
	fingerprint, err := hex.DecodeString(expectedFingerprint)
	if err != nil {
		return 0, fmt.Errorf("decode learning redaction projection fingerprint: %w", err)
	}
	generationID := redactedProjectionGenerationID(request)
	var remaining int64
	err = s.pool.QueryRow(ctx, `
		WITH expected AS (
			SELECT $1::uuid AS generation_id,$2::bigint AS event_seq,$3::uuid AS event_id,$4::uuid AS erasure_id,$5::bytea AS fingerprint
		), head_ok AS (
			SELECT count(*)::bigint AS matched
			FROM learning_projection_head h
			JOIN learning_projection_generations g ON g.id=h.active_generation_id
			JOIN learning_projection_checkpoints c ON c.generation_id=g.id
			CROSS JOIN expected e
			WHERE h.singleton_id=1 AND h.active_generation_id=e.generation_id
			  AND h.rebuilding_generation_id IS NULL AND h.rebuild_lease_token IS NULL AND h.rebuild_lease_expires_at IS NULL
			  AND g.status='active' AND g.target_high_water=e.event_seq AND g.checkpoint_event_seq=e.event_seq
			  AND g.fingerprint=e.fingerprint AND c.event_seq=e.event_seq AND c.fingerprint=e.fingerprint
			  AND g.fingerprint=c.fingerprint AND g.knowledge_revision_id IS NULL
			  AND NOT g.incomplete AND cardinality(g.reason_codes)=0
		), residuals AS (
			SELECT abs(count(*)-1)::bigint AS remaining FROM learning_projection_generations WHERE status='active'
			UNION ALL SELECT CASE WHEN matched=1 THEN 0 ELSE 1 END FROM head_ok
			UNION ALL SELECT count(*) FROM learning_projection_generations g CROSS JOIN expected e WHERE g.id<>e.generation_id AND (g.status<>'retired' OR NOT g.incomplete OR NOT ('content_redacted'=ANY(g.reason_codes)) OR g.target_high_water<>0 OR g.checkpoint_event_seq<>0 OR g.fingerprint IS NOT NULL OR g.knowledge_revision_id IS NOT NULL)
			UNION ALL SELECT count(*) FROM learning_projection_checkpoints c CROSS JOIN expected e WHERE c.generation_id<>e.generation_id
			UNION ALL SELECT count(*) FROM learning_projection_timeline t CROSS JOIN expected e WHERE t.generation_id<>e.generation_id
			UNION ALL SELECT abs(count(*)-1) FROM learning_projection_timeline t CROSS JOIN expected e WHERE t.generation_id=e.generation_id
			UNION ALL SELECT count(*) FROM learning_projection_timeline t CROSS JOIN expected e WHERE t.generation_id=e.generation_id AND (t.event_seq<>e.event_seq OR t.event_id<>e.event_id OR t.item->>'event_type' IS DISTINCT FROM 'EventRedacted' OR t.item->>'aggregate_id' IS DISTINCT FROM e.erasure_id::text)
			UNION ALL SELECT count(*) FROM learning_projection_routes
			UNION ALL SELECT count(*) FROM learning_projection_sessions
			UNION ALL SELECT count(*) FROM learning_projection_nodes
			UNION ALL SELECT count(*) FROM learning_projection_evidence
			UNION ALL SELECT count(*) FROM learning_projection_reviews
			UNION ALL SELECT count(*) FROM learning_projection_misconceptions
			UNION ALL SELECT count(*) FROM learning_projection_stats
		)
		SELECT COALESCE(sum(remaining),0)::bigint FROM residuals`, generationID, event.EventSequence, event.ID, request.ErasureID, fingerprint).Scan(&remaining)
	if err != nil {
		return 0, fmt.Errorf("verify learning projection redaction: %w", err)
	}

	return remaining, nil
}
