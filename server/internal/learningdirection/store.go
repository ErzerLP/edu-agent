// Package learningdirection owns confirmed learning intent and its managed goal.
package learningdirection

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningpg "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	tutoringpg "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrUnavailable = errors.New("direction_storage_unavailable")

type Revision struct {
	ID             string `json:"id"`
	Version        int64  `json:"version"`
	Text           string `json:"text"`
	GoalID         string `json:"goal_id,omitempty"`
	GoalRevisionID string `json:"goal_revision_id,omitempty"`
	GoalVersion    int64  `json:"goal_version,omitempty"`
}
type Store struct {
	pool *pgxpool.Pool
	aead cipher.AEAD
}

func New(pool *pgxpool.Pool, key []byte) (*Store, error) {
	s := &Store{pool: pool}
	if len(key) == 0 {
		return s, nil
	}
	if len(key) != 32 {
		return nil, ErrUnavailable
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	s.aead, err = cipher.NewGCM(b)
	return s, err
}
func aad(id, space string, generation, version int64) string {
	return fmt.Sprintf("direction:v1:%s:%s:%d:%d", id, space, generation, version)
}

// ReadTx accepts 0 for the current version. Callers hold the learning privacy gate.
func (s *Store) ReadTx(ctx context.Context, tx pgx.Tx, space string, version int64) (Revision, error) {
	var d Revision
	var generation int64
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT r.id::text,r.version,r.privacy_generation,r.ciphertext FROM learning_direction_revisions r JOIN learning_space_directions h ON h.space_id=r.space_id AND h.privacy_generation=r.privacy_generation WHERE r.space_id=$1 AND r.version=CASE WHEN $2::bigint=0 THEN h.version ELSE $2 END`, space, version).Scan(&d.ID, &d.Version, &generation, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if s == nil || s.aead == nil || len(raw) < s.aead.NonceSize() {
		return d, ErrUnavailable
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], []byte(aad(d.ID, space, generation, d.Version)))
	if err != nil {
		return d, ErrUnavailable
	}
	defer clear(plain)
	var body struct {
		Schema int    `json:"schema"`
		Text   string `json:"text"`
	}
	if json.Unmarshal(plain, &body) != nil || body.Schema != 1 {
		return d, ErrUnavailable
	}
	d.Text = body.Text
	return d, nil
}

// SaveTx runs under the space row lock in the same transaction as its metadata.
func (s *Store) SaveTx(ctx context.Context, tx pgx.Tx, space string, generation int64, text string) (Revision, error) {
	if s == nil || s.aead == nil {
		return Revision{}, ErrUnavailable
	}
	old, err := s.ReadTx(ctx, tx, space, 0)
	if err != nil {
		return old, err
	}
	text = strings.TrimSpace(text)
	if old.Text == text {
		return old, nil
	}
	d := Revision{ID: uuid.NewString(), Version: old.Version + 1, Text: text}
	raw, _ := json.Marshal(struct {
		Schema int    `json:"schema"`
		Text   string `json:"text"`
	}{1, text})
	nonce := make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return d, err
	}
	ciphertext := s.aead.Seal(nonce, nonce, raw, []byte(aad(d.ID, space, generation, d.Version)))
	if _, err = tx.Exec(ctx, `INSERT INTO learning_space_directions(space_id,privacy_generation,version) VALUES($1,$2,$3) ON CONFLICT(space_id) DO UPDATE SET version=excluded.version`, space, generation, d.Version); err != nil {
		return d, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_direction_revisions(id,space_id,privacy_generation,version,ciphertext) VALUES($1,$2,$3,$4,$5)`, d.ID, space, generation, d.Version, ciphertext)
	return d, err
}

// EnsureTx serializes with direction edits and creates revisions via learning's
// command/event/receipt path. It never creates a teaching session or calls a model.
func (s *Store) EnsureTx(ctx context.Context, tx pgx.Tx, actor, space string, generation, version int64) (Revision, error) {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM learning_spaces WHERE id=$1 FOR UPDATE`, space).Scan(&status); err != nil {
		return Revision{}, err
	}
	if status != "active" {
		return Revision{}, &learningspace.Error{Code: "learning_space_archived"}
	}
	d, err := s.ReadTx(ctx, tx, space, 0)
	if err != nil {
		return d, err
	}
	if d.Version == 0 {
		return d, &learningspace.Error{Code: "learning_direction_required"}
	}
	if d.Version != version {
		return d, &learningspace.Error{Code: "version_conflict"}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_direction_goals(space_id,privacy_generation,goal_id) VALUES($1,$2,$3) ON CONFLICT(space_id) DO NOTHING`, space, generation, uuid.NewString()); err != nil {
		return d, err
	}
	var ensured int64
	var previous *string
	if err = tx.QueryRow(ctx, `SELECT goal_id::text,direction_version,goal_revision_id::text FROM learning_direction_goals WHERE space_id=$1 AND privacy_generation=$2 FOR UPDATE`, space, generation).Scan(&d.GoalID, &ensured, &previous); err != nil {
		return d, err
	}
	ctx, _ = learningspace.WithScope(ctx, space)
	knowledgeStore := knowledgepg.New(s.pool)
	owner := learningpg.New(s.pool, tutoringpg.New(s.pool), knowledgeStore).ForDirectionGoal(tx, d.GoalID)
	if previous != nil {
		goal, e := owner.LoadGoalRevision(ctx, *previous)
		if e != nil {
			return d, e
		}
		d.GoalRevisionID = goal.ID
		d.GoalVersion = goal.Revision
	}
	if ensured == version {
		return d, nil
	}
	knowledgeService, err := knowledge.NewService(knowledgeStore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		return d, err
	}
	service, err := learning.NewService(owner, owner, learningknowledge.New(knowledgeService), learning.ServiceOptions{})
	if err != nil {
		return d, err
	}
	command := learning.GoalCommand{GoalID: d.GoalID, Text: d.Text, Source: "learning_direction", PreviousRevisionID: previous, Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), AggregateType: "goal", AggregateID: d.GoalID, ExpectedVersion: d.GoalVersion, PayloadSchemaVersion: 1, Payload: json.RawMessage(`{}`)}}
	result, err := service.CreateGoal(ctx, actor, command)
	if err != nil {
		return d, err
	}
	var goal learning.GoalRevision
	if err = json.Unmarshal(result.Result, &goal); err != nil {
		return d, err
	}
	d.GoalRevisionID, d.GoalVersion = goal.ID, goal.Revision
	_, err = tx.Exec(ctx, `UPDATE learning_direction_goals SET direction_version=$2,goal_revision_id=$3 WHERE space_id=$1`, space, version, goal.ID)
	return d, err
}
func RotateKeyTx(ctx context.Context, tx pgx.Tx, rewrap keyrotation.Rewrap) error {
	if _, err := tx.Exec(ctx, `LOCK TABLE learning_direction_revisions IN ACCESS EXCLUSIVE MODE NOWAIT`); err != nil {
		return err
	}
	return keyrotation.Rewrite(ctx, tx, `SELECT id::text,'direction:v1:'||id||':'||space_id||':'||privacy_generation||':'||version,ciphertext FROM learning_direction_revisions WHERE id::text>$1 ORDER BY id::text LIMIT 16`, `UPDATE learning_direction_revisions SET ciphertext=$2 WHERE id=$1::uuid`, rewrap)
}
