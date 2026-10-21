package mentorrun

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var errWebStopped = errors.New("web_search_stopped")

type WebProgress struct {
	Failures          []WebReadFailure `json:"failures,omitempty"`
	Queries           []string         `json:"queries,omitempty"`
	Stage             string           `json:"stage"`
	Query             string           `json:"query,omitempty"`
	Title             string           `json:"title,omitempty"`
	Notice            string           `json:"notice,omitempty"`
	ReadAttempted     int              `json:"read_attempted"`
	ReadSucceeded     int              `json:"read_succeeded"`
	ReadFailed        int              `json:"read_failed"`
	FetchHTTPRequests int              `json:"fetch_http_requests"`
}

type WebReadFailure struct {
	ResourceID string `json:"resource_id"`
	Title      string `json:"title"`
	Category   string `json:"category"`
	Reason     string `json:"reason"`
}
type WebState struct {
	ResourceKey []byte                 `json:"resource_key"`
	Failures    map[string]string      `json:"failures,omitempty"`
	Input       string                 `json:"input"`
	Epoch       int64                  `json:"epoch"`
	Enabled     bool                   `json:"enabled"`
	Routed      bool                   `json:"routed"`
	Done        bool                   `json:"done"`
	Messages    []modelclient.Message  `json:"messages,omitempty"`
	Pending     []modelclient.ToolCall `json:"pending,omitempty"`
	Documents   []string               `json:"documents,omitempty"`
	Supplied    map[string][]string    `json:"supplied"`
	Progress    WebProgress            `json:"progress"`
}
type WebSearchCommand struct {
	OperationID     string `json:"operation_id"`
	ExpectedVersion int64  `json:"expected_web_search_version"`
	Enabled         bool   `json:"enabled"`
}
type WebSearchPreference struct {
	Enabled bool  `json:"enabled"`
	Version int64 `json:"web_search_version"`
}

// Creation returns the current preference even when its idempotent creation
// receipt is retried after a preference change. It needs only creation's scope.
func (s *Service) ConversationWebPreference(ctx context.Context, actor identity.Credential, space, id string) (WebSearchPreference, error) {
	result := WebSearchPreference{}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	g, err := gates(ctx, tx, false)
	if err != nil {
		return result, err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, true); err != nil {
		return result, err
	}
	c, err := conversationRow(ctx, tx, actor, space, id, g, false)
	if err != nil {
		return result, err
	}
	if c.deleted {
		return result, ErrNotFound
	}
	return WebSearchPreference{Enabled: c.WebSearchEnabled, Version: c.WebSearchVersion}, nil
}

func webAAD(kind, id string, g int64) string { return fmt.Sprintf("web:%s:%s:%d", kind, id, g) }

func webPayloadLimit(kind string) int {
	if kind == "attachment" {
		// []byte is base64 inside JSON; support the parser's full file limit.
		return (pdfsource.MaxBytes+2)/3*4 + 2
	}
	return 3 << 20
}

func (s *Service) sealWeb(kind, id string, g int64, value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > webPayloadLimit(kind) || s.aead == nil {
		return nil, ErrStorage
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, raw, []byte(webAAD(kind, id, g))), nil
}
func (s *Service) openWeb(kind, id string, g int64, raw []byte, value any) error {
	if s.aead == nil || len(raw) < s.aead.NonceSize() || len(raw) > webPayloadLimit(kind)+256 {
		return ErrStorage
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], []byte(webAAD(kind, id, g)))
	if err != nil || json.Unmarshal(plain, value) != nil {
		return ErrStorage
	}
	return nil
}

func (s *Service) webOperation(ctx context.Context, tx pgx.Tx, device, space, op string, g int64, hash [32]byte, out any) (bool, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,51))`, device+op); err != nil {
		return false, err
	}
	var id string
	var old, raw []byte
	err := tx.QueryRow(ctx, `SELECT id::text,request_hash,ciphertext FROM learning_web_operations WHERE device_id=$1 AND operation_id=$2 AND space_id=$3 AND privacy_generation=$4`, device, op, space, g).Scan(&id, &old, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.Equal(old, hash[:]) {
		return false, ErrOperation
	}
	return true, s.openWeb("operation", id, g, raw, out)
}
func (s *Service) saveWebOperation(ctx context.Context, tx pgx.Tx, device, space, conversation, run, op string, g int64, hash [32]byte, out any) error {
	id := uuid.NewString()
	raw, err := s.sealWeb("operation", id, g, out)
	if err != nil {
		return err
	}
	if err = s.quota(ctx, tx, int64(len(raw))); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_web_operations(id,operation_id,device_id,space_id,conversation_id,run_id,privacy_generation,request_hash,ciphertext) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,NULLIF($6,'')::uuid,$7,$8,$9)`, id, op, device, space, conversation, run, g, hash[:], raw)
	return err
}

func (s *Service) ChangeWebSearch(ctx context.Context, actor identity.Credential, space, id string, c WebSearchCommand) (WebSearchPreference, error) {
	result := WebSearchPreference{}
	if !validID(space) || !validID(id) || !validID(c.OperationID) || c.ExpectedVersion < 1 {
		return result, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	g, err := gates(ctx, tx, true)
	if err != nil {
		return result, err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, true); err != nil {
		return result, err
	}
	conversation, err := conversationRow(ctx, tx, actor, space, id, g, true)
	if err != nil {
		return result, err
	}
	if conversation.deleted {
		return result, ErrNotFound
	}
	hash := requestHash("web_search", space, id, c)
	found, err := s.webOperation(ctx, tx, actor.Device.ID, space, c.OperationID, g, hash, &result)
	if err != nil || found {
		return result, err
	}
	if conversation.WebSearchVersion != c.ExpectedVersion {
		return result, ErrConflict
	}
	result = WebSearchPreference{Enabled: c.Enabled, Version: conversation.WebSearchVersion + 1}
	_, err = tx.Exec(ctx, `UPDATE learning_tutor_conversations SET web_search_enabled=$2,web_search_version=web_search_version+1,web_access_epoch=web_access_epoch+1 WHERE id=$1`, id, c.Enabled)
	if err != nil {
		return result, err
	}
	if err = s.saveWebOperation(ctx, tx, actor.Device.ID, space, id, "", c.OperationID, g, hash, result); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	// Every preference change revokes the old epoch, including a rapid off/on.
	s.mu.Lock()
	for _, cancel := range s.webCancels[id] {
		cancel()
	}
	s.mu.Unlock()
	return result, nil
}

func (h *executionHost) webGateTx(ctx context.Context, tx pgx.Tx) error {
	if h.body.Web == nil || !h.body.Web.Enabled {
		return errWebStopped
	}
	if h.owned.SourceResearchJobID != "" {
		_, configuration, err := h.service.search(nil)
		if err != nil || configuration != h.body.SourceResearch.SearchConfiguration {
			return ErrInactive
		}
		return h.service.sourceResearchGateTx(ctx, tx, h.owned, h.body.Web.Epoch)
	}
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT NOT deleted AND web_search_enabled AND web_access_epoch=$2 FROM learning_tutor_conversations WHERE id=$1 AND device_id=$3 AND space_id=$4 AND privacy_generation=$5 FOR SHARE`, h.owned.ConversationID, h.body.Web.Epoch, h.owned.device, h.owned.SpaceID, h.owned.Generation).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return errWebStopped
	}
	return nil
}
func (h *executionHost) webGate(ctx context.Context) error {
	return h.service.mutateTx(ctx, &h.owned, "", func(tx pgx.Tx, _ *row) error { return h.webGateTx(ctx, tx) })
}

// The watcher also notices changes committed by another server process. Every
// dispatch and content commit independently checks the durable epoch and lease.
func (h *executionHost) webContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	s := h.service
	id := uuid.NewString()
	s.mu.Lock()
	if s.webCancels[webCancelKey(h.owned)] == nil {
		s.webCancels[webCancelKey(h.owned)] = map[string]context.CancelFunc{}
	}
	s.webCancels[webCancelKey(h.owned)][id] = cancel
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if h.webGate(ctx) != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() {
		cancel()
		<-done
		s.mu.Lock()
		delete(s.webCancels[webCancelKey(h.owned)], id)
		if len(s.webCancels[webCancelKey(h.owned)]) == 0 {
			delete(s.webCancels, webCancelKey(h.owned))
		}
		s.mu.Unlock()
	}
}
