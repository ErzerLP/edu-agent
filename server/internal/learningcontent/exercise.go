package learningcontent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type AssessmentPolicy struct {
	Configuration string `json:"configuration"`
	Requests      int    `json:"requests"`
	Tokens        int    `json:"tokens"`
}
type ExerciseBody struct {
	Manifest   knowledge.SourceManifest `json:"manifest"`
	Assessment AssessmentPolicy         `json:"assessment"`
}
type Exercise struct {
	ID                string
	SpaceID           string
	Generation        int64
	SessionID         string
	ActivityID        string
	ContentArtifactID string
	ContentVersion    int64
	Body              ExerciseBody
	Help              ExerciseHelpState
}
type ExerciseHelpState struct {
	Version int64
	Level   learning.HelpLevel
}
type ExerciseHelp struct {
	OperationID string             `json:"operation_id"`
	Level       learning.HelpLevel `json:"level"`
	Text        string             `json:"text"`
}

func exerciseAAD(e Exercise) string {
	return fmt.Sprintf("exercise:v1:%s:%d:%s", e.ID, e.Generation, e.SpaceID)
}
func helpAAD(id, exercise, device string, level learning.HelpLevel) string {
	return "exercise-help:v1:" + id + ":" + exercise + ":" + device + ":" + string(level)
}
func (s *Store) sealExercise(aad string, value any) ([]byte, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	if len(plain) > MaxBody {
		return nil, ErrInvalid
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, []byte(aad)), nil
}
func (s *Store) openExercise(aad string, raw []byte, value any) error {
	if !s.Available() || len(raw) < s.aead.NonceSize() {
		return ErrUnavailable
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], []byte(aad))
	if err != nil {
		return err
	}
	defer clear(plain)
	return json.Unmarshal(plain, value)
}

func (s *Store) ExerciseTransaction(ctx context.Context, actor identity.Credential, work func(pgx.Tx) error) error {
	tx, _, err := s.begin(ctx, actor, true)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err = work(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ValidateQuestionTx rechecks exact source revisions and every supplied citation.
func (s *Store) ValidateQuestionTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, generation int64, manifest knowledge.SourceManifest, q StudioQuestion, citations []StudioCitation) error {
	g, err := gates(ctx, tx, actor, true)
	if err != nil {
		return err
	}
	if g != generation || !validStudioQuestion(q.Prompt, q.Rubric, q.AllowedHelp) || len(q.Citations) == 0 {
		return ErrInvalid
	}
	if err = s.studioSourcesTx(ctx, tx, actor.TokenID, StudioBody{SourceManifest: &manifest, Citations: citations}); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, id := range q.Citations {
		found := false
		for _, c := range citations {
			if c.ID == id {
				found = true
			}
		}
		if !found || seen[id] {
			return ErrStudioCitation
		}
		seen[id] = true
	}
	return nil
}

// PublishExerciseTx owns publication idempotency. The callback creates only a
// formal question; mapping and caller's history commit in the same transaction.
func (s *Store) PublishExerciseTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, operation string, generation int64, hash string, body ExerciseBody, create func() (Revision, error)) (Exercise, error) {
	if uuid.Validate(operation) != nil || body.Assessment.Configuration == "" || body.Assessment.Requests < 1 || body.Assessment.Tokens < 1 {
		return Exercise{}, ErrInvalid
	}
	g, err := gates(ctx, tx, actor, true)
	if err != nil {
		return Exercise{}, err
	}
	if g != generation {
		return Exercise{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "exercise-operation:"+actor.Device.ID+":"+operation); err != nil {
		return Exercise{}, err
	}
	var id, stored string
	err = tx.QueryRow(ctx, `SELECT id,request_hash FROM learning_exercises WHERE device_id=$1 AND operation_id=$2`, actor.Device.ID, operation).Scan(&id, &stored)
	if err == nil {
		if stored != hash {
			return Exercise{}, ErrConflict
		}
		return s.ExerciseTx(ctx, tx, actor, id)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Exercise{}, err
	}
	r, err := create()
	if err != nil {
		return Exercise{}, err
	}
	e := Exercise{ID: uuid.NewString(), SpaceID: r.SpaceID, Generation: g, SessionID: r.SessionID, ActivityID: r.ActivityID, ContentArtifactID: r.ArtifactID, ContentVersion: r.Version, Body: body}
	raw, err := s.sealExercise(exerciseAAD(e), body)
	if err != nil {
		return e, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_exercises(id,device_id,operation_id,request_hash,space_id,privacy_generation,session_id,activity_id,content_artifact_id,content_version,ciphertext) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, e.ID, actor.Device.ID, operation, hash, e.SpaceID, g, e.SessionID, e.ActivityID, e.ContentArtifactID, e.ContentVersion, raw)
	return e, err
}

func (s *Store) ExerciseTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, id string) (Exercise, error) {
	var e Exercise
	var raw []byte
	g, err := gates(ctx, tx, actor, false)
	if err != nil {
		return e, err
	}
	err = tx.QueryRow(ctx, `SELECT id,space_id,privacy_generation,session_id,activity_id,content_artifact_id,content_version,ciphertext FROM learning_exercises WHERE id=$1 AND space_id=$2 AND privacy_generation=$3`, id, learningspace.Scope(ctx), g).Scan(&e.ID, &e.SpaceID, &e.Generation, &e.SessionID, &e.ActivityID, &e.ContentArtifactID, &e.ContentVersion, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, err
	}
	if err = s.openExercise(exerciseAAD(e), raw, &e.Body); err != nil {
		return e, err
	}
	var allowed bool
	if err = tx.QueryRow(ctx, `SELECT 'knowledge:read'=ANY(scopes) FROM device_tokens WHERE id=$1 FOR SHARE`, actor.TokenID).Scan(&allowed); err != nil {
		return e, err
	}
	if !allowed {
		return e, ErrForbidden
	}
	if s.studioSources == nil {
		return e, ErrUnavailable
	}
	if _, err = s.studioSources.ReadSourceSelectionTx(ctx, tx, e.Body.Manifest); err != nil {
		return e, err
	}
	var rank int
	err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(max(CASE level WHEN 'hint' THEN 1 WHEN 'scaffold' THEN 2 WHEN 'answer_revealed' THEN 3 END),0) FROM learning_exercise_help WHERE exercise_id=$1`, e.ID).Scan(&e.Help.Version, &rank)
	if rank > 0 {
		e.Help.Level = []learning.HelpLevel{"", learning.HelpHint, learning.HelpScaffold, learning.HelpAnswerRevealed}[rank]
	}
	return e, err
}

// HelpOperationTx recovers a committed disclosure after response loss. The
// original device, exercise, privacy generation and sources are rechecked.
func (s *Store) HelpOperationTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, exercise, operation string) (ExerciseHelp, error) {
	var h ExerciseHelp
	if !s.Available() || uuid.Validate(operation) != nil {
		return h, ErrInvalid
	}
	if _, err := s.ExerciseTx(ctx, tx, actor, exercise); err != nil {
		return h, err
	}
	var id, level string
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT id,level,ciphertext FROM learning_exercise_help WHERE exercise_id=$1 AND device_id=$2 AND operation_id=$3`, exercise, actor.Device.ID, operation).Scan(&id, &level, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, ErrNotFound
	}
	if err != nil {
		return h, err
	}
	return h, s.openExercise(helpAAD(id, exercise, actor.Device.ID, learning.HelpLevel(level)), raw, &h)
}

// PublishHelp returns text only after its disclosure level has committed. A new
// disclosure after submission is rejected, including concurrent late results.
func (s *Store) PublishHelp(ctx context.Context, actor identity.Credential, id string, h ExerciseHelp) (ExerciseHelp, error) {
	if uuid.Validate(id) != nil || uuid.Validate(h.OperationID) != nil || h.Level == learning.HelpNone || helpRank(h.Level) < 0 || !studioText(h.Text, 16000) {
		return ExerciseHelp{}, ErrInvalid
	}
	tx, _, err := s.begin(ctx, actor, true)
	if err != nil {
		return ExerciseHelp{}, err
	}
	defer tx.Rollback(context.Background())
	e, err := s.ExerciseTx(ctx, tx, actor, id)
	if err != nil {
		return ExerciseHelp{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learning-aggregate:session:"+e.SessionID); err != nil {
		return ExerciseHelp{}, err
	}
	var hid, exercise, level string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT id,exercise_id,level,ciphertext FROM learning_exercise_help WHERE device_id=$1 AND operation_id=$2`, actor.Device.ID, h.OperationID).Scan(&hid, &exercise, &level, &raw)
	if err == nil {
		var old ExerciseHelp
		if exercise != e.ID || s.openExercise(helpAAD(hid, exercise, actor.Device.ID, learning.HelpLevel(level)), raw, &old) != nil || old != h {
			return ExerciseHelp{}, ErrConflict
		}
		return old, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ExerciseHelp{}, err
	}
	var allowed, submitted bool
	if err = tx.QueryRow(ctx, `SELECT $2=ANY(allowed_help),EXISTS(SELECT 1 FROM learning_attempts WHERE activity_id=$1) FROM learning_activities WHERE id=$1`, e.ActivityID, h.Level).Scan(&allowed, &submitted); err != nil {
		return ExerciseHelp{}, err
	}
	if !allowed || submitted {
		return ExerciseHelp{}, ErrConflict
	}
	hid = uuid.NewString()
	raw, err = s.sealExercise(helpAAD(hid, e.ID, actor.Device.ID, h.Level), h)
	if err != nil {
		return ExerciseHelp{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_exercise_help(id,exercise_id,device_id,operation_id,level,ciphertext) VALUES($1,$2,$3,$4,$5,$6)`, hid, e.ID, actor.Device.ID, h.OperationID, h.Level, raw); err != nil {
		return ExerciseHelp{}, err
	}
	return h, tx.Commit(ctx)
}
func helpRank(h learning.HelpLevel) int {
	switch h {
	case learning.HelpNone:
		return 0
	case learning.HelpHint:
		return 1
	case learning.HelpScaffold:
		return 2
	case learning.HelpAnswerRevealed:
		return 3
	}
	return -1
}

// ExerciseAttemptTx is called under the common session aggregate lock, before
// typed records are written. Unmapped legacy activities remain manual.
func (s *Store) ExerciseAttemptTx(ctx context.Context, tx pgx.Tx, a *learning.Attempt) (*Exercise, identity.Credential, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM learning_exercises WHERE activity_id=$1`, a.ActivityID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, identity.Credential{}, nil
	}
	if err != nil {
		return nil, identity.Credential{}, err
	}
	actor, ok := learning.AssessmentActor(ctx)
	if !ok || actor.Device.ID != a.ActorDeviceID {
		return nil, actor, ErrForbidden
	}
	if _, err = gates(ctx, tx, actor, true); err != nil {
		return nil, actor, err
	}
	e, err := s.ExerciseTx(ctx, tx, actor, id)
	if err != nil {
		return nil, actor, err
	}
	artifact, version := AnswerVersion(ctx)
	if e.SessionID != a.SessionID || artifact != nil && (*artifact != e.ContentArtifactID || version == nil || *version != e.ContentVersion) {
		return nil, actor, ErrConflict
	}
	var floor int
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(CASE level WHEN 'hint' THEN 1 WHEN 'scaffold' THEN 2 WHEN 'answer_revealed' THEN 3 END),0) FROM learning_exercise_help WHERE exercise_id=$1`, id).Scan(&floor); err != nil {
		return nil, actor, err
	}
	if helpRank(a.Help) < floor {
		return nil, actor, &learning.Error{Code: learning.CodeInvalidRequest, Reason: "help_below_disclosed_floor"}
	}
	return &e, actor, nil
}
