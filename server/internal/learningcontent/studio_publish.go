package learningcontent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type StudioSourceReader interface {
	ReadSourceSelectionTx(context.Context, pgx.Tx, knowledge.SourceManifest) ([]knowledge.SourceDetail, error)
}

func (s *Store) ConfigureStudioSources(reader StudioSourceReader) { s.studioSources = reader }

type StudioTarget struct {
	ArtifactID      *string `json:"artifact_id"`
	ExpectedVersion int64   `json:"expected_version"`
	Generation      int64   `json:"privacy_generation"`
	Kind            string  `json:"kind"`
	GoalID          *string `json:"goal_id"`
}

func (c StudioTarget) Validate() error {
	canonicalID := func(value string) bool {
		id, err := uuid.Parse(value)
		return err == nil && id != uuid.Nil && id.String() == value
	}
	if c.Generation < 1 || studioFields(c.Kind) == nil || c.ExpectedVersion < 0 || (c.ArtifactID == nil) != (c.ExpectedVersion == 0) || c.ArtifactID != nil && !canonicalID(*c.ArtifactID) || c.GoalID != nil && !canonicalID(*c.GoalID) {
		return ErrInvalid
	}
	return nil
}
func (s *Store) studioSourcesTx(ctx context.Context, tx pgx.Tx, token string, body StudioBody) error {
	if s.studioSources == nil {
		return ErrUnavailable
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT 'knowledge:read'=ANY(scopes) AND revoked_at IS NULL FROM device_tokens WHERE id=$1 FOR SHARE`, token).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	if body.SourceManifest == nil {
		return ErrInvalid
	}
	sources, err := s.studioSources.ReadSourceSelectionTx(ctx, tx, *body.SourceManifest)
	if err != nil {
		return err
	}
	return VerifyStudioSources(body, sources)
}
func (s *Store) StudioTargetTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, c StudioTarget) error {
	if !s.Available() {
		return ErrUnavailable
	}
	if err := c.Validate(); err != nil {
		return err
	}
	g, err := gates(ctx, tx, actor, true)
	if err != nil {
		return err
	}
	if g != c.Generation {
		return &privacy.Error{Code: privacy.CodeContentRedacted}
	}
	var status string
	if err = tx.QueryRow(ctx, `SELECT status FROM learning_spaces WHERE id=$1 FOR SHARE`, learningspace.Scope(ctx)).Scan(&status); err != nil {
		return ErrNotFound
	}
	if status != "active" {
		return ErrForbidden
	}
	if c.GoalID != nil {
		var owned bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_goal_revisions WHERE goal_id=$1 AND space_id=$2)`, *c.GoalID, learningspace.Scope(ctx)).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return ErrNotFound
		}
	}
	if c.ArtifactID == nil {
		return nil
	}
	var version int64
	var kind string
	var goal *string
	err = tx.QueryRow(ctx, `SELECT latest_version,kind,goal_id FROM learning_studio_artifacts WHERE id=$1 AND space_id=$2 AND privacy_generation=$3 FOR UPDATE`, *c.ArtifactID, learningspace.Scope(ctx), g).Scan(&version, &kind, &goal)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if version != c.ExpectedVersion || kind != c.Kind || (goal == nil) != (c.GoalID == nil) || goal != nil && *goal != *c.GoalID {
		return ErrConflict
	}
	_, err = s.studioRead(ctx, tx, *c.ArtifactID, version, g, actor.TokenID)
	return err
}

// The caller commits both this revision and the terminal run state in one tx.
func (s *Store) StudioPublishTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, operation string, c StudioTarget, body StudioBody) (StudioRevision, error) {
	if err := s.StudioTargetTx(ctx, tx, actor, c); err != nil {
		return StudioRevision{}, err
	}
	if err := s.studioSourcesTx(ctx, tx, actor.TokenID, body); err != nil {
		return StudioRevision{}, err
	}
	r := StudioRevision{ArtifactID: uuid.NewString(), SpaceID: learningspace.Scope(ctx), GoalID: c.GoalID, Kind: c.Kind, Version: c.ExpectedVersion + 1, Generation: c.Generation, ActorDeviceID: actor.Device.ID, Body: body}
	var err error
	if c.ArtifactID == nil {
		_, err = tx.Exec(ctx, `INSERT INTO learning_studio_artifacts(id,space_id,goal_id,kind,privacy_generation,latest_version) VALUES($1,$2,$3,$4,$5,1)`, r.ArtifactID, r.SpaceID, r.GoalID, r.Kind, r.Generation)
	} else {
		r.ArtifactID = *c.ArtifactID
		_, err = tx.Exec(ctx, `UPDATE learning_studio_artifacts SET latest_version=$2 WHERE id=$1`, r.ArtifactID, r.Version)
	}
	if err != nil {
		return r, err
	}
	plain, err := json.Marshal(body)
	if err != nil {
		return r, err
	}
	defer clear(plain)
	if len(plain) > MaxBody {
		return r, ErrInvalid
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return r, err
	}
	raw := s.aead.Seal(nonce, nonce, plain, studioAAD(r))
	if err = tx.QueryRow(ctx, `INSERT INTO learning_studio_revisions(artifact_id,version,actor_device_id,ciphertext) VALUES($1,$2,$3,$4) RETURNING created_at`, r.ArtifactID, r.Version, r.ActorDeviceID, raw).Scan(&r.CreatedAt); err != nil {
		return r, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_studio_operations(device_id,operation_id,artifact_id,version,request_hash) VALUES($1,$2,$3,$4,decode($5,'hex'))`, actor.Device.ID, operation, r.ArtifactID, r.Version, fingerprint(c))
	return r, err
}
