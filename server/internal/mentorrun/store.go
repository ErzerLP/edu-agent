package mentorrun

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	search              func(func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error)
	fetcher             *research.Fetcher
	pool                *pgxpool.Pool
	settings            *settings.Service
	aead                cipher.AEAD
	process             string
	mu                  sync.Mutex
	temporary           map[string]temporaryBody
	temporaryGeneration int64
	Lease               time.Duration
}

type temporaryBody struct {
	Version int64
	Body    Body
}

func New(pool *pgxpool.Pool, configuration *settings.Service, key []byte) (*Service, error) {
	if pool == nil || configuration == nil {
		return nil, ErrInvalid
	}
	a, err := newCipher(key)
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, settings: configuration, search: configuration.SearchClient, fetcher: research.NewFetcher(), aead: a, process: uuid.NewString(), temporary: map[string]temporaryBody{}, Lease: 15 * time.Second}, nil
}
func (s *Service) CanSave() bool { return s.aead != nil }

type row struct {
	Meta
	device, token, process string
	lease                  *string
	leaseUntil             *time.Time
	callStarted            bool
	leaseValid             bool
	ciphertext             []byte
	body                   Body
}

const columns = `state,device_id::text,token_id::text,process_id::text,lease_id::text,lease_until,call_started,checkpoint,COALESCE(lease_until>clock_timestamp(),FALSE)`

func scan(r pgx.Row) (row, error) {
	var item row
	var raw []byte
	err := r.Scan(&raw, &item.device, &item.token, &item.process, &item.lease, &item.leaseUntil, &item.callStarted, &item.ciphertext, &item.leaseValid)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, ErrNotFound
	}
	if err != nil {
		return item, err
	}
	if json.Unmarshal(raw, &item.Meta) != nil {
		return item, ErrStorage
	}
	return item, nil
}

func (s *Service) decode(item *row) error {
	if !item.BodyAvailable {
		return nil
	}
	if item.Saved {
		b, err := unseal(s.aead, item.RunID, item.ciphertext)
		item.body = b
		return err
	}
	s.mu.Lock()
	b, ok := s.temporary[item.RunID]
	// 复制 checkpoint，避免 worker 和读者共享消息切片。
	raw, err := json.Marshal(b.Body)
	s.mu.Unlock()
	if !ok || item.process != s.process || b.Version != item.Version {
		return ErrStorage
	}
	if err != nil || json.Unmarshal(raw, &item.body) != nil {
		return ErrStorage
	}
	return nil
}

func (s *Service) cache(item row) {
	if item.Saved {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.temporaryGeneration != item.Generation {
		clear(s.temporary)
		s.temporaryGeneration = item.Generation
	}
	if !item.BodyAvailable {
		delete(s.temporary, item.RunID)
		return
	}
	raw, _ := json.Marshal(item.body)
	var copy Body
	_ = json.Unmarshal(raw, &copy)
	s.temporary[item.RunID] = temporaryBody{Version: item.Version, Body: copy}
}

// 锁顺序与已有学习写入一致：隐私、设备、区、目标，最后运行行。
func gates(ctx context.Context, tx pgx.Tx, write bool) (int64, error) {
	var identityGeneration, generation int64
	if err := tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('identity','read',NULL)`).Scan(&identityGeneration); err != nil {
		return 0, err
	}
	access := "read"
	if write {
		access = "write"
	}
	err := tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning',$1,NULL)`, access).Scan(&generation)
	return generation, err
}

func actorGate(ctx context.Context, tx pgx.Tx, device, token string, write bool) error {
	var active bool
	var scopes []string
	err := tx.QueryRow(ctx, `SELECT d.revoked_at IS NULL AND t.revoked_at IS NULL,t.scopes FROM devices d JOIN device_tokens t ON t.device_id=d.id WHERE d.id=$1 AND t.id=$2 FOR SHARE OF d,t`, device, token).Scan(&active, &scopes)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !active {
		return ErrForbidden
	}
	want := "learning:read"
	if write {
		want = "learning:write"
	}
	for _, scope := range scopes {
		if scope == want {
			return nil
		}
	}
	return ErrForbidden
}

func goalGate(ctx context.Context, tx pgx.Tx, space, goal string, version int64) (string, error) {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM learning_spaces WHERE id=$1 FOR SHARE`, space).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if status != "active" {
		return "", ErrInactive
	}
	var actual int64
	if err := tx.QueryRow(ctx, `SELECT aggregate_version FROM learning_aggregate_heads WHERE aggregate_type='goal' AND aggregate_id=$1 FOR SHARE`, goal).Scan(&actual); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	var text string
	if err := tx.QueryRow(ctx, `SELECT goal_text,CASE WHEN source='privacy_erasure' THEN 'redacted' ELSE COALESCE(management->>'status','active') END FROM learning_goal_revisions WHERE goal_id=$1 AND space_id=$2 ORDER BY revision DESC LIMIT 1`, goal, space).Scan(&text, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if actual != version {
		return "", ErrConflict
	}
	if status != "draft" && status != "active" {
		return "", ErrInactive
	}
	return text, nil
}

func (s *Service) save(ctx context.Context, tx pgx.Tx, item *row, kind string) error {
	item.Version++
	item.Watermark++
	item.UpdatedAt = time.Now().UTC()
	var ciphertext []byte
	var err error
	if item.BodyAvailable {
		raw, e := json.Marshal(item.body)
		if e != nil || len(raw) > MaxBody {
			return ErrLimit
		}
		if item.Saved {
			ciphertext, err = seal(s.aead, item.RunID, item.body)
			if err != nil {
				return err
			}
		}
	}
	raw, err := json.Marshal(item.Meta)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE learning_mentor_runs SET state=$2,checkpoint=$3,lease_id=$4,lease_until=$5,call_started=$6 WHERE id=$1`, item.RunID, raw, ciphertext, item.lease, item.leaseUntil, item.callStarted)
	if err != nil {
		return err
	}
	event := Event{RunID: item.RunID, SessionID: item.SessionID, SpaceID: item.SpaceID, GoalID: item.GoalID, Generation: item.Generation, Version: item.Version, Seq: item.Watermark, Type: kind}
	raw, err = json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_mentor_events(run_id,seq,event) VALUES($1,$2,$3)`, item.RunID, item.Watermark, raw); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM learning_mentor_events WHERE run_id=$1 AND seq<=$2`, item.RunID, item.Watermark-EventWindow)
	return err
}

func requestHash(kind, space, target string, value any) [32]byte {
	raw, _ := json.Marshal(struct {
		Kind, Space, Target string
		Value               any
	}{kind, space, target, value})
	return sha256.Sum256(raw)
}

func operation(ctx context.Context, tx pgx.Tx, device, id string, hash [32]byte) (Receipt, bool, error) {
	var result Receipt
	// 操作锁覆盖查重到插入，两个创建请求也不能同时受理。
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,21))`, device+id); err != nil {
		return result, false, err
	}
	var stored, raw []byte
	err := tx.QueryRow(ctx, `SELECT request_hash,receipt FROM learning_mentor_operations WHERE device_id=$1 AND operation_id=$2`, device, id).Scan(&stored, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if !bytes.Equal(stored, hash[:]) {
		return result, false, ErrOperation
	}
	if json.Unmarshal(raw, &result) != nil {
		return result, false, ErrStorage
	}
	return result, true, nil
}

func receipt(ctx context.Context, tx pgx.Tx, item row, operationID string, hash [32]byte) (Receipt, error) {
	r := Receipt{OperationID: operationID, RunID: item.RunID, SessionID: item.SessionID, Version: item.Version}
	raw, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_mentor_operations(device_id,operation_id,request_hash,run_id,receipt) VALUES($1,$2,$3,$4,$5)`, item.device, operationID, hash[:], item.RunID, raw)
	return r, err
}

func (s *Service) Create(ctx context.Context, actor identity.Credential, space, goal string, c Create) (Receipt, error) {
	kind := "mentor"
	searchConfiguration := ""
	if c.Research != nil {
		if c.Research.Validate() != nil {
			return Receipt{}, ErrInvalid
		}
		kind = "research"
		// 公开主题是唯一外发输入；私人 prompt 不进入研究模型或搜索。
		if c.Prompt != c.Research.Topic {
			return Receipt{}, ErrInvalid
		}
	}
	if !validID(space) || !validID(goal) || !validID(c.OperationID) || !validID(c.SessionID) || c.ExpectedVersion < 1 || !validText(c.Prompt, 16000) || c.RequestBudget < 1 || c.TokenBudget < 1 {
		return Receipt{}, ErrInvalid
	}
	hash := requestHash("create", space, goal, c)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, true)
	if err != nil {
		return Receipt{}, err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, true); err != nil {
		return Receipt{}, err
	}
	if r, found, e := operation(ctx, tx, actor.Device.ID, c.OperationID, hash); e != nil || found {
		return r, e
	}
	if _, err = goalGate(ctx, tx, space, goal, c.ExpectedVersion); err != nil {
		return Receipt{}, err
	}
	model, limits, fingerprint, err := s.settings.MentorClient()
	if err != nil || model == nil {
		return Receipt{}, ErrModel
	}
	if c.Research != nil {
		if _, searchConfiguration, err = s.search(nil); err != nil {
			return Receipt{}, ErrModel
		}
		if c.Research.AutoAdopt {
			if err = knowledgeActor(ctx, tx, actor.Device.ID, actor.TokenID); err != nil {
				return Receipt{}, err
			}
		}
	}
	if c.RequestBudget > limits.ResearchRequests || c.TokenBudget > limits.ResearchTokens {
		return Receipt{}, ErrInvalid
	}
	if c.Save && !s.CanSave() {
		return Receipt{}, ErrStorage
	}
	if err = s.quota(ctx, tx, 512<<10); err != nil {
		return Receipt{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_mentor_processes(id,live_until) VALUES($1,clock_timestamp()+interval '30 seconds') ON CONFLICT(id) DO UPDATE SET live_until=excluded.live_until`, s.process); err != nil {
		return Receipt{}, err
	}
	var sessionID string
	err = tx.QueryRow(ctx, `INSERT INTO learning_mentor_sessions(id,device_id,space_id,goal_id,privacy_generation,kind) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(device_id,space_id,goal_id,privacy_generation,kind) DO UPDATE SET id=learning_mentor_sessions.id RETURNING id::text`, c.SessionID, actor.Device.ID, space, goal, generation, kind).Scan(&sessionID)
	if err != nil {
		return Receipt{}, err
	}
	var busy bool
	if sessionID != c.SessionID {
		return Receipt{}, ErrConflict
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_mentor_runs WHERE session_id=$1 AND state->>'status' NOT IN ('succeeded','partial','failed','cancelled'))`, sessionID).Scan(&busy); err != nil {
		return Receipt{}, err
	}
	if busy {
		return Receipt{}, ErrConflict
	}
	item := row{Meta: Meta{RunID: uuid.NewString(), SessionID: sessionID, SpaceID: space, GoalID: goal, GoalVersion: c.ExpectedVersion, Generation: generation, Status: "queued", Stage: "queued", Saved: c.Save, BodyAvailable: true, RequestsLeft: c.RequestBudget, TokensLeft: c.TokenBudget, ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour), Configuration: fingerprint}, device: actor.Device.ID, token: actor.TokenID, process: s.process}
	item.body.Messages = []modelclient.Message{{Role: "user", Content: c.Prompt}}
	item.Kind = kind
	if c.Research != nil {
		item.body.Messages = nil
		item.body.Research = &research.State{Request: *c.Research, Sources: []research.Source{}, SearchConfiguration: searchConfiguration}
		item.Stage = "query_planned"
	}
	raw, _ := json.Marshal(item.Meta)
	if _, err = tx.Exec(ctx, `INSERT INTO learning_mentor_runs(id,session_id,device_id,token_id,space_id,goal_id,goal_version,privacy_generation,state,process_id,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, item.RunID, sessionID, item.device, item.token, space, goal, c.ExpectedVersion, generation, raw, s.process, item.ExpiresAt); err != nil {
		return Receipt{}, err
	}
	if err = s.save(ctx, tx, &item, "accepted"); err != nil {
		return Receipt{}, err
	}
	r, err := receipt(ctx, tx, item, c.OperationID, hash)
	if err != nil {
		return Receipt{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_mentor_sessions SET current_run_id=$2 WHERE id=$1`, sessionID, item.RunID); err != nil {
		return Receipt{}, err
	}
	s.cache(item)
	if err = tx.Commit(ctx); err != nil {
		s.drop(item.RunID)
		return Receipt{}, err
	}
	return r, nil
}

func (s *Service) drop(id string) { s.mu.Lock(); delete(s.temporary, id); s.mu.Unlock() }

func (s *Service) quota(ctx context.Context, tx pgx.Tx, additional int64) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(210021)`); err != nil {
		return err
	}
	var reserved int64
	err := tx.QueryRow(ctx, `SELECT (COALESCE(sum(CASE WHEN (state->>'body_available')::boolean THEN 524288 ELSE 2048 END),0)+(SELECT count(*)*1024 FROM learning_mentor_operations))::bigint FROM learning_mentor_runs`).Scan(&reserved)
	if err != nil {
		return err
	}
	if reserved+additional > int64(s.settings.View().Limits.StorageMiB)<<20 {
		return ErrLimit
	}
	return nil
}
