package learningcontent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) studioBegin(ctx context.Context, actor identity.Credential, write bool) (pgx.Tx, int64, error) {
	if !s.Available() {
		return nil, 0, ErrUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	g, err := gates(ctx, tx, actor, write)
	if err == nil {
		var status string
		err = tx.QueryRow(ctx, `SELECT status FROM learning_spaces WHERE id=$1 FOR SHARE`, learningspace.Scope(ctx)).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		if err == nil && write && status != "active" {
			err = &learningspace.Error{Code: "learning_space_archived"}
		}
	}
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, 0, err
	}
	return tx, g, nil
}
func (s *Store) studioRead(ctx context.Context, tx pgx.Tx, id string, version, g int64) (StudioRevision, error) {
	var r StudioRevision
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT a.id,a.space_id,a.goal_id,a.kind,a.privacy_generation,r.version,r.actor_device_id,r.created_at,r.ciphertext FROM learning_studio_artifacts a JOIN learning_studio_revisions r ON r.artifact_id=a.id AND r.version=CASE WHEN $2::bigint=0 THEN a.latest_version ELSE $2::bigint END WHERE a.id=$1 AND a.space_id=$3 AND a.privacy_generation=$4`, id, version, learningspace.Scope(ctx), g).Scan(&r.ArtifactID, &r.SpaceID, &r.GoalID, &r.Kind, &r.Generation, &r.Version, &r.ActorDeviceID, &r.CreatedAt, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if len(raw) < s.aead.NonceSize() {
		return r, ErrUnavailable
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], studioAAD(r))
	if err != nil {
		return StudioRevision{}, ErrUnavailable
	}
	defer clear(plain)
	if json.Unmarshal(plain, &r.Body) != nil || r.Body.Schema != 1 {
		return StudioRevision{}, ErrUnsupported
	}
	// Generated kinds require source revalidation and publication in 3b/3c.
	if r.Kind != "note" || r.Body.SourceManifest != nil || len(r.Body.Citations) > 0 {
		return StudioRevision{}, ErrUnsupported
	}
	return r, nil
}
func (s *Store) StudioGet(ctx context.Context, actor identity.Credential, id string, version int64) (StudioRevision, error) {
	if uuid.Validate(id) != nil || version < 0 {
		return StudioRevision{}, ErrInvalid
	}
	tx, g, err := s.studioBegin(ctx, actor, false)
	if err != nil {
		return StudioRevision{}, err
	}
	defer tx.Rollback(context.Background())
	r, err := s.studioRead(ctx, tx, id, version, g)
	if err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}
func (s *Store) StudioSave(ctx context.Context, actor identity.Credential, id string, c StudioSave) (StudioRevision, error) {
	if err := c.Validate(id); err != nil {
		return StudioRevision{}, err
	}
	tx, g, err := s.studioBegin(ctx, actor, true)
	if err != nil {
		return StudioRevision{}, err
	}
	defer tx.Rollback(context.Background())
	if g != c.Generation {
		return StudioRevision{}, &privacy.Error{Code: privacy.CodeContentRedacted}
	}
	hash := fingerprint(struct {
		Space, ID string
		Command   StudioSave
	}{learningspace.Scope(ctx), id, c})
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "studio-operation:"+actor.Device.ID+":"+c.OperationID); err != nil {
		return StudioRevision{}, err
	}
	var storedID, storedHash string
	var version int64
	err = tx.QueryRow(ctx, `SELECT artifact_id,version,encode(request_hash,'hex') FROM learning_studio_operations WHERE device_id=$1 AND operation_id=$2`, actor.Device.ID, c.OperationID).Scan(&storedID, &version, &storedHash)
	if err == nil {
		if storedHash != hash {
			return StudioRevision{}, ErrConflict
		}
		r, e := s.studioRead(ctx, tx, storedID, version, g)
		if e != nil {
			return r, e
		}
		return r, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return StudioRevision{}, err
	}
	r := StudioRevision{ArtifactID: id, SpaceID: learningspace.Scope(ctx), GoalID: c.GoalID, Kind: "note", Generation: g, ActorDeviceID: actor.Device.ID, Version: 1, Body: StudioBody{Schema: 1, Title: c.Title, Markdown: c.Markdown, Citations: []StudioCitation{}}}
	if id == "" {
		if c.GoalID != nil {
			var owned bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_goal_revisions WHERE goal_id=$1 AND space_id=$2)`, *c.GoalID, r.SpaceID).Scan(&owned)
			if err != nil {
				return r, err
			}
			if !owned {
				return r, ErrNotFound
			}
			normalized := uuid.MustParse(*c.GoalID).String()
			r.GoalID = &normalized
		}
		r.ArtifactID = uuid.NewString()
		_, err = tx.Exec(ctx, `INSERT INTO learning_studio_artifacts(id,space_id,goal_id,kind,privacy_generation,latest_version) VALUES($1,$2,$3,$4,$5,1)`, r.ArtifactID, r.SpaceID, r.GoalID, r.Kind, g)
	} else {
		err = tx.QueryRow(ctx, `SELECT latest_version FROM learning_studio_artifacts WHERE id=$1 AND space_id=$2 AND privacy_generation=$3 FOR UPDATE`, id, r.SpaceID, g).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			return r, ErrNotFound
		}
		if err != nil {
			return r, err
		}
		if version != c.ExpectedVersion {
			return r, ErrConflict
		}
		previous, e := s.studioRead(ctx, tx, id, 0, g)
		if e != nil {
			return r, e
		}
		if (previous.GoalID == nil) != (c.GoalID == nil) || previous.GoalID != nil && *previous.GoalID != uuid.MustParse(*c.GoalID).String() {
			return r, ErrConflict
		}
		r.ArtifactID = previous.ArtifactID
		r.GoalID = previous.GoalID
		r.Version = version + 1
		_, err = tx.Exec(ctx, `UPDATE learning_studio_artifacts SET latest_version=$2 WHERE id=$1`, r.ArtifactID, r.Version)
	}
	if err != nil {
		return r, err
	}
	plain, err := json.Marshal(r.Body)
	if err != nil {
		return r, err
	}
	defer clear(plain)
	nonce := make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return r, err
	}
	raw := s.aead.Seal(nonce, nonce, plain, studioAAD(r))
	err = tx.QueryRow(ctx, `INSERT INTO learning_studio_revisions(artifact_id,version,actor_device_id,ciphertext) VALUES($1,$2,$3,$4) RETURNING created_at`, r.ArtifactID, r.Version, r.ActorDeviceID, raw).Scan(&r.CreatedAt)
	if err != nil {
		return r, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_studio_operations(device_id,operation_id,artifact_id,version,request_hash) VALUES($1,$2,$3,$4,decode($5,'hex'))`, actor.Device.ID, c.OperationID, r.ArtifactID, r.Version, hash)
	if err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}
func (s *Store) StudioOperation(ctx context.Context, actor identity.Credential, operation string) (StudioRevision, error) {
	if uuid.Validate(operation) != nil {
		return StudioRevision{}, ErrInvalid
	}
	tx, g, err := s.studioBegin(ctx, actor, false)
	if err != nil {
		return StudioRevision{}, err
	}
	defer tx.Rollback(context.Background())
	var id string
	var version int64
	err = tx.QueryRow(ctx, `SELECT artifact_id,version FROM learning_studio_operations WHERE device_id=$1 AND operation_id=$2`, actor.Device.ID, operation).Scan(&id, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return StudioRevision{}, ErrNotFound
	}
	if err != nil {
		return StudioRevision{}, err
	}
	r, err := s.studioRead(ctx, tx, id, version, g)
	if err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}
func (s *Store) StudioHistory(ctx context.Context, actor identity.Credential, id string) ([]RevisionInfo, error) {
	if uuid.Validate(id) != nil {
		return nil, ErrInvalid
	}
	tx, g, err := s.studioBegin(ctx, actor, false)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err = s.studioRead(ctx, tx, id, 0, g); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT version,'committed',created_at FROM learning_studio_revisions WHERE artifact_id=$1 ORDER BY version DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	items := []RevisionInfo{}
	for rows.Next() {
		var item RevisionInfo
		if err = rows.Scan(&item.Version, &item.Status, &item.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return items, tx.Commit(ctx)
}
func (s *Store) StudioList(ctx context.Context, actor identity.Credential, cursor string, limit int) (StudioPage, error) {
	page := StudioPage{Items: []StudioItem{}}
	if limit < 1 || limit > 100 {
		return page, ErrInvalid
	}
	after := "00000000-0000-0000-0000-000000000000"
	if cursor != "" {
		var c struct{ Space, ID string }
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if len(cursor) > 1024 || e != nil || json.Unmarshal(raw, &c) != nil || c.Space != learningspace.Scope(ctx) || uuid.Validate(c.ID) != nil {
			return page, ErrInvalid
		}
		after = c.ID
	}
	tx, g, err := s.studioBegin(ctx, actor, false)
	if err != nil {
		return page, err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, `SELECT id FROM learning_studio_artifacts WHERE space_id=$1 AND privacy_generation=$2 AND id>$3 ORDER BY id LIMIT $4`, learningspace.Scope(ctx), g, after, limit+1)
	if err != nil {
		return page, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return page, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(ids) > limit {
		ids = ids[:limit]
		raw, _ := json.Marshal(struct{ Space, ID string }{learningspace.Scope(ctx), ids[limit-1]})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	for _, id := range ids {
		r, e := s.studioRead(ctx, tx, id, 0, g)
		if e != nil {
			return page, e
		}
		page.Items = append(page.Items, StudioItem{r.ArtifactID, r.GoalID, r.Kind, r.Version, r.Body.Title, r.CreatedAt})
	}
	return page, tx.Commit(ctx)
}
