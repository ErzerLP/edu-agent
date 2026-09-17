package learningcontent

import (
	"context"
	"errors"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Restore struct {
	OperationID     string `json:"operation_id"`
	ExpectedVersion int64  `json:"expected_version"`
	Version         int64  `json:"version"`
	Reason          string `json:"reason"`
}

// SelectionTx 供运行受理/模型请求/最终发布分别核验，所有正文取自内容 owner。
func (s *Store) SelectionTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, selection Selection) (Revision, string, error) {
	g, err := gates(ctx, tx, actor, false)
	if err != nil {
		return Revision{}, "", err
	}
	if err := s.referenceAccess(ctx, tx, actor); err != nil {
		return Revision{}, "", err
	}
	r, err := s.read(ctx, tx, selection.ArtifactID, 0, g, false)
	if err != nil {
		return Revision{}, "", err
	}
	text, err := Select(r, selection)
	return r, text, err
}

func (s *Store) ReadTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, id string, version int64) (Revision, error) {
	g, err := gates(ctx, tx, actor, false)
	if err != nil {
		return Revision{}, err
	}
	if err := s.referenceAccess(ctx, tx, actor); err != nil {
		return Revision{}, err
	}
	return s.read(ctx, tx, id, version, g, false)
}

// 修改只在已持有运行租约的事务内发布，取消与成功不能同时赢得提交。
func (s *Store) PatchTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, operation string, request EditRequest, candidate Candidate, reason, model string) (Revision, error) {
	if request.Validate() != nil {
		return Revision{}, ErrInvalid
	}
	hash := fingerprint(struct {
		Request       EditRequest
		Candidate     Candidate
		Reason, Model string
	}{request, candidate, reason, model})
	return s.changeTx(ctx, tx, actor, request.Selection.ArtifactID, operation, request.Selection.Version, hash, func(r Revision) (Body, error) {
		return Patch(r, request, candidate, reason, operation, model)
	})
}

func (s *Store) Restore(ctx context.Context, actor identity.Credential, id string, command Restore) (Revision, error) {
	if command.Version < 1 || strings.TrimSpace(command.Reason) == "" || len(command.Reason) > 16000 {
		return Revision{}, ErrInvalid
	}
	tx, g, err := s.begin(ctx, actor, true)
	if err != nil {
		return Revision{}, err
	}
	defer tx.Rollback(context.Background())
	r, err := s.changeTx(ctx, tx, actor, id, command.OperationID, command.ExpectedVersion, fingerprint(command), func(current Revision) (Body, error) {
		old, err := s.read(ctx, tx, id, command.Version, g, false)
		if err != nil {
			return Body{}, err
		}
		if old.Status != "committed" {
			return Body{}, ErrInvalid
		}
		body := old.Body
		body.Change = &Change{BaseVersion: current.Version, RestoredVersion: old.Version, Action: "restore", Reason: command.Reason, ChangedBlocks: changedBlocks(current.Body.Blocks, body.Blocks)}
		return body, nil
	})
	if err != nil {
		return Revision{}, err
	}
	return r, tx.Commit(ctx)
}

func changedBlocks(before, after []Block) []string {
	ids := []string{}
	var walk func([]Block, []Block)
	walk = func(blocks, other []Block) {
		for _, b := range blocks {
			found := FindBlock(other, b.ID)
			if found == nil || fingerprint(b) != fingerprint(*found) {
				exists := false
				for _, id := range ids {
					if id == b.ID {
						exists = true
					}
				}
				if !exists {
					ids = append(ids, b.ID)
				}
			}
			walk(b.Children, other)
		}
	}
	walk(before, after)
	walk(after, before)
	return ids
}

func (s *Store) changeTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, id, operation string, expected int64, hash string, change func(Revision) (Body, error)) (Revision, error) {
	if uuid.Validate(id) != nil || uuid.Validate(operation) != nil || expected < 1 {
		return Revision{}, ErrInvalid
	}
	g, err := gates(ctx, tx, actor, true)
	if err != nil {
		return Revision{}, err
	}
	if err := s.referenceAccess(ctx, tx, actor); err != nil {
		return Revision{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learningcontent-operation:"+actor.Device.ID+":"+operation); err != nil {
		return Revision{}, err
	}
	var storedID, storedHash string
	var version int64
	err = tx.QueryRow(ctx, `SELECT artifact_id,version,encode(request_hash,'hex') FROM learning_content_operations WHERE device_id=$1 AND operation_id=$2`, actor.Device.ID, operation).Scan(&storedID, &version, &storedHash)
	if err == nil {
		if storedID != id || storedHash != hash {
			return Revision{}, ErrConflict
		}
		return s.read(ctx, tx, id, version, g, false)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Revision{}, err
	}
	err = tx.QueryRow(ctx, `SELECT latest_version FROM learning_content_artifacts WHERE id=$1 AND space_id=$2 AND privacy_generation=$3 FOR UPDATE`, id, learningspace.Scope(ctx), g).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Revision{}, ErrNotFound
	}
	if err != nil {
		return Revision{}, err
	}
	r, err := s.read(ctx, tx, id, 0, g, false)
	if err != nil {
		return Revision{}, err
	}
	if r.Version != expected {
		return Revision{}, ErrConflict
	}
	body, err := change(r)
	if err != nil {
		return Revision{}, err
	}
	source, err := s.source.LearningContentSource(ctx, tx, r.SessionID, r.ActivityID)
	if err != nil {
		return Revision{}, err
	}
	if err = Validate(body, source, "committed"); err != nil {
		return Revision{}, err
	}
	r.Version = version + 1
	r.CommittedVersion = r.Version
	r.Status = "committed"
	r.ActorDeviceID = actor.Device.ID
	r.Body = body
	if err = s.checkSources(ctx, tx, r); err != nil {
		return Revision{}, err
	}
	raw, err := s.seal(r)
	if err != nil {
		return Revision{}, err
	}
	if err = tx.QueryRow(ctx, `INSERT INTO learning_content_revisions(artifact_id,version,status,actor_device_id,ciphertext) VALUES($1,$2,'committed',$3,$4) RETURNING created_at`, id, r.Version, actor.Device.ID, raw).Scan(&r.CreatedAt); err != nil {
		return Revision{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_content_artifacts SET latest_version=$2,committed_version=$2 WHERE id=$1`, id, r.Version); err != nil {
		return Revision{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_content_operations(device_id,operation_id,artifact_id,version,request_hash) VALUES($1,$2,$3,$4,decode($5,'hex'))`, actor.Device.ID, operation, id, r.Version, hash); err != nil {
		return Revision{}, err
	}
	return r, nil
}
