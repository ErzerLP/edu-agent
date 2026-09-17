package learningcontent

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool       *pgxpool.Pool
	source     SourceReader
	aead       cipher.AEAD
	references ReferenceReader
}

func (s *Store) ConfigureReferences(reader ReferenceReader) { s.references = reader }

func New(pool *pgxpool.Pool, source SourceReader, key []byte) (*Store, error) {
	s := &Store{pool: pool, source: source}
	if len(key) == 0 {
		return s, nil
	}
	if len(key) != 32 {
		return nil, ErrUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	s.aead, err = cipher.NewGCM(block)
	return s, err
}

func (s *Store) Available() bool { return s != nil && s.aead != nil }

func aad(r Revision) []byte {
	return []byte(fmt.Sprintf("learningcontent:v1:%s:%d:%d:%s:%s:%s:%s:%s:%s:%d:%s", r.ArtifactID, r.Version, r.Generation, r.SpaceID, r.GoalID, r.GoalRevisionID, r.SessionID, r.ActivityID, r.Status, r.ActivityRevision, r.ActorDeviceID))
}
func (s *Store) seal(r Revision) ([]byte, error) {
	if !s.Available() {
		return nil, ErrUnavailable
	}
	raw, err := json.Marshal(r.Body)
	if err != nil || len(raw) > MaxBody {
		return nil, ErrInvalid
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, raw, aad(r)), nil
}
func (s *Store) open(r *Revision, raw []byte) error {
	if !s.Available() {
		return ErrUnavailable
	}
	if len(raw) < s.aead.NonceSize() || len(raw) > MaxBody+64 {
		return ErrInvalid
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], aad(*r))
	if err != nil {
		return ErrUnavailable
	}
	return json.Unmarshal(plain, &r.Body)
}

// 代次和实时权限在同一事务内检查；不信任浏览器先前获得的 capabilities。
func gates(ctx context.Context, tx pgx.Tx, actor identity.Credential, write bool) (int64, error) {
	var generation int64
	mode, scope := "read", "learning:read"
	if write {
		mode, scope = "write", "learning:write"
	}
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerIdentity); err != nil {
		return 0, err
	}
	if err := tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning',$1,NULL)`, mode).Scan(&generation); err != nil {
		return 0, err
	}
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerTutoring); err != nil {
		return 0, err
	}
	var ok bool
	err := tx.QueryRow(ctx, `SELECT d.revoked_at IS NULL AND t.revoked_at IS NULL AND $3=ANY(t.scopes) FROM devices d JOIN device_tokens t ON t.device_id=d.id WHERE d.id=$1 AND t.id=$2 FOR SHARE OF d,t`, actor.Device.ID, actor.TokenID, scope).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !ok {
		return 0, ErrForbidden
	}
	return generation, err
}

func (s *Store) begin(ctx context.Context, actor identity.Credential, write bool) (pgx.Tx, int64, error) {
	if !s.Available() {
		return nil, 0, ErrUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	g, err := gates(ctx, tx, actor, write)
	if err == nil {
		err = s.referenceAccess(ctx, tx, actor)
	}
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, 0, err
	}
	return tx, g, nil
}

// Ensure 仅从已发行正规活动创建首版；并发标签页得到相同 Artifact/Revision。
func (s *Store) Ensure(ctx context.Context, actor identity.Credential, session, activity string) (Revision, error) {
	if uuid.Validate(session) != nil || uuid.Validate(activity) != nil {
		return Revision{}, ErrInvalid
	}
	tx, g, err := s.begin(ctx, actor, true)
	if err != nil {
		return Revision{}, err
	}
	defer tx.Rollback(context.Background())
	r, err := s.EnsureTx(ctx, tx, actor, session, activity, g)
	if err != nil {
		return Revision{}, err
	}
	return r, tx.Commit(ctx)
}

// EnsureTx 与正式活动、上下文和开学回执在同一事务发布首版正文。
func (s *Store) EnsureTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, session, activity string, g int64) (Revision, error) {
	source, err := s.source.LearningContentSource(ctx, tx, session, activity)
	if err != nil {
		return Revision{}, err
	}
	// 加密关联身份只取正规记录，避免请求中的 UUID 表示法与数据库规范形式不一致。
	r := Revision{ProtocolVersion: Protocol, ArtifactID: ArtifactID(source.Activity), Version: 1, CommittedVersion: 1, SpaceID: source.SpaceID, GoalID: source.GoalID, GoalRevisionID: source.Activity.GoalRevisionID, SessionID: source.Activity.SessionID, ActivityID: source.Activity.ID, ActivityRevision: source.Activity.Revision, Generation: g, ActorDeviceID: actor.Device.ID, Status: "committed", Body: Adapt(source)}
	if err = Validate(r.Body, source, r.Status); err != nil {
		return Revision{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learningcontent:"+r.ArtifactID); err != nil {
		return Revision{}, err
	}
	existing, e := s.read(ctx, tx, r.ArtifactID, 0, g, false)
	if e == nil {
		return existing, nil
	}
	if !errors.Is(e, ErrNotFound) {
		return Revision{}, e
	}
	ciphertext, err := s.seal(r)
	if err != nil {
		return Revision{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_content_artifacts(id,space_id,goal_id,goal_revision_id,session_id,activity_id,activity_revision,privacy_generation,latest_version,committed_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,1,1)`, r.ArtifactID, r.SpaceID, r.GoalID, r.GoalRevisionID, r.SessionID, r.ActivityID, r.ActivityRevision, g)
	if err != nil {
		return Revision{}, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO learning_content_revisions(artifact_id,version,status,actor_device_id,ciphertext) VALUES($1,1,'committed',$2,$3) RETURNING created_at`, r.ArtifactID, actor.Device.ID, ciphertext).Scan(&r.CreatedAt)
	if err != nil {
		return Revision{}, err
	}
	return r, nil
}

func (s *Store) read(ctx context.Context, tx pgx.Tx, id string, version, generation int64, lock bool) (Revision, error) {
	var r Revision
	var raw []byte
	q := `SELECT a.id,a.space_id,a.goal_id,a.goal_revision_id,a.session_id,a.activity_id,a.activity_revision,a.privacy_generation,a.committed_version,r.version,r.status,r.actor_device_id,r.created_at,r.ciphertext FROM learning_content_artifacts a JOIN learning_content_revisions r ON r.artifact_id=a.id AND r.version=CASE WHEN $2::bigint=0 THEN a.committed_version ELSE $2::bigint END WHERE a.id=$1 AND a.space_id=$3 AND a.privacy_generation=$4`
	if lock {
		q += ` FOR SHARE OF a`
	}
	err := tx.QueryRow(ctx, q, id, version, learningspace.Scope(ctx), generation).Scan(&r.ArtifactID, &r.SpaceID, &r.GoalID, &r.GoalRevisionID, &r.SessionID, &r.ActivityID, &r.ActivityRevision, &r.Generation, &r.CommittedVersion, &r.Version, &r.Status, &r.ActorDeviceID, &r.CreatedAt, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.ProtocolVersion = Protocol
	err = s.open(&r, raw)
	if err == nil {
		err = s.checkSources(ctx, tx, r)
	}
	return r, err
}

func (s *Store) Get(ctx context.Context, actor identity.Credential, id string, version int64) (Revision, error) {
	if uuid.Validate(id) != nil || version < 0 {
		return Revision{}, ErrInvalid
	}
	tx, g, err := s.begin(ctx, actor, false)
	if err != nil {
		return Revision{}, err
	}
	defer tx.Rollback(context.Background())
	r, err := s.read(ctx, tx, id, version, g, false)
	if err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}

func (s *Store) History(ctx context.Context, actor identity.Credential, id string) ([]RevisionInfo, error) {
	if uuid.Validate(id) != nil {
		return nil, ErrInvalid
	}
	tx, g, err := s.begin(ctx, actor, false)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err = s.read(ctx, tx, id, 0, g, false); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT version,status,created_at FROM learning_content_revisions WHERE artifact_id=$1 ORDER BY version DESC LIMIT 100`, id)
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
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return items, tx.Commit(ctx)
}

func (s *Store) Commit(ctx context.Context, actor identity.Credential, id string, c Commit) (Revision, error) {
	if c.ProtocolVersion != Protocol {
		return Revision{}, ErrUnsupported
	}
	if uuid.Validate(id) != nil || uuid.Validate(c.OperationID) != nil || c.ExpectedVersion < 1 {
		return Revision{}, ErrInvalid
	}
	tx, g, err := s.begin(ctx, actor, true)
	if err != nil {
		return Revision{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learningcontent-operation:"+actor.Device.ID+":"+c.OperationID); err != nil {
		return Revision{}, err
	}
	var storedID, hash string
	var version int64
	err = tx.QueryRow(ctx, `SELECT artifact_id,version,encode(request_hash,'hex') FROM learning_content_operations WHERE device_id=$1 AND operation_id=$2`, actor.Device.ID, c.OperationID).Scan(&storedID, &version, &hash)
	if err == nil {
		if storedID != id || hash != fingerprint(c) {
			return Revision{}, ErrConflict
		}
		r, e := s.read(ctx, tx, id, version, g, false)
		if e != nil {
			return r, e
		}
		return r, tx.Commit(ctx)
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
	if version != c.ExpectedVersion {
		return Revision{}, ErrConflict
	}
	r, err := s.read(ctx, tx, id, 0, g, false)
	if err != nil {
		return r, err
	}
	source, err := s.source.LearningContentSource(ctx, tx, r.SessionID, r.ActivityID)
	if err != nil {
		return r, err
	}
	// 来源与生成依据只取正规 owner；调用方不能伪造模型、引用或评分语义。
	body := r.Body
	body.Blocks = c.Blocks
	body.Interaction = c.Interaction
	body.Change = &Change{BaseVersion: r.Version, Action: "presentation", Reason: "显式提交内容版本", ChangedBlocks: changedBlocks(r.Body.Blocks, c.Blocks)}
	if err = Validate(body, source, c.Status); err != nil {
		return Revision{}, err
	}
	r.Version = version + 1
	r.Status = c.Status
	r.Body = body
	r.ActorDeviceID = actor.Device.ID
	if c.Status == "committed" {
		r.CommittedVersion = r.Version
	}
	ciphertext, err := s.seal(r)
	if err != nil {
		return Revision{}, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO learning_content_revisions(artifact_id,version,status,actor_device_id,ciphertext) VALUES($1,$2,$3,$4,$5) RETURNING created_at`, id, r.Version, r.Status, actor.Device.ID, ciphertext).Scan(&r.CreatedAt)
	if err != nil {
		return Revision{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_content_artifacts SET latest_version=$2,committed_version=$3 WHERE id=$1`, id, r.Version, r.CommittedVersion); err != nil {
		return Revision{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_content_operations(device_id,operation_id,artifact_id,version,request_hash) VALUES($1,$2,$3,$4,decode($5,'hex'))`, actor.Device.ID, c.OperationID, id, r.Version, fingerprint(c)); err != nil {
		return Revision{}, err
	}
	return r, tx.Commit(ctx)
}

type answerGuardKey struct{}
type answerGuard struct {
	store   *Store
	actor   identity.Credential
	id      string
	version int64
}

func (s *Store) WithAnswer(ctx context.Context, actor identity.Credential, id string, version int64) context.Context {
	return context.WithValue(ctx, answerGuardKey{}, answerGuard{s, actor, id, version})
}

// 原 learning 提交事务调用此端口，检查与答案写入原子完成，避免检查后换版。
func ValidateAttemptTx(ctx context.Context, tx pgx.Tx, attempt learning.Attempt) error {
	guard, ok := ctx.Value(answerGuardKey{}).(answerGuard)
	if !ok {
		return nil
	}
	g, err := gates(ctx, tx, guard.actor, true)
	if err != nil {
		return err
	}
	r, err := guard.store.read(ctx, tx, guard.id, guard.version, g, true)
	if err != nil {
		return err
	}
	if r.ActivityID != attempt.ActivityID || r.ActivityRevision != attempt.ActivityRevision || r.SessionID != attempt.SessionID {
		return ErrConflict
	}
	return CanAnswer(r, attempt.Answer)
}

// 清除沿用 learning owner 的授权事务；不保留可复活正文的墓碑或进程缓存。
func RedactTx(ctx context.Context, tx pgx.Tx) error {
	for _, table := range []string{"learning_content_preferences", "learning_content_operations", "learning_content_revisions", "learning_content_artifacts"} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+table); err != nil {
			return err
		}
	}
	return nil
}

func Remaining(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (int64, error) {
	var count int64
	err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_content_artifacts)+(SELECT count(*) FROM learning_content_revisions)+(SELECT count(*) FROM learning_content_operations)+(SELECT count(*) FROM learning_content_preferences)`).Scan(&count)
	return count, err
}
