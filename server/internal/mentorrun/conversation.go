package mentorrun

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrHistorySchema = errors.New("tutor_history_schema_unsupported")
	ErrDestination   = errors.New("tutor_destination_confirmation_required")
	ErrTemporary     = errors.New("tutor_temporary_unavailable")
)

type Conversation struct {
	ID                   string    `json:"id"`
	SpaceID              string    `json:"space_id"`
	GoalID               string    `json:"goal_id,omitempty"`
	TeachingSessionID    string    `json:"teaching_session_id,omitempty"`
	Generation           int64     `json:"privacy_generation"`
	Version              int64     `json:"version"`
	Saved                bool      `json:"saved"`
	Title                string    `json:"title"`
	Provider             string    `json:"provider"`
	Endpoint             string    `json:"endpoint"`
	UpdatedAt            time.Time `json:"updated_at"`
	CurrentRunID         string    `json:"current_run_id,omitempty"`
	GoalVersion          int64     `json:"goal_version"`
	Writable             bool      `json:"writable"`
	StorageState         string    `json:"storage_state"`
	Destination          string    `json:"destination"`
	DestinationProvider  string    `json:"destination_provider"`
	DestinationEndpoint  string    `json:"destination_endpoint"`
	ConfirmationRequired bool      `json:"confirmation_required"`
	titleCipher          []byte
	requestHash          []byte
	deleted              bool
}

type NewConversation struct {
	ID                string `json:"id"`
	GoalID            string `json:"goal_id,omitempty"`
	TeachingSessionID string `json:"teaching_session_id,omitempty"`
	Saved             bool   `json:"saved"`
	Title             string `json:"title,omitempty"`
}

type SubmitTurn struct {
	OperationID        string `json:"operation_id"`
	ExpectedVersion    int64  `json:"expected_version"`
	Prompt             string `json:"prompt"`
	RequestBudget      int    `json:"request_budget"`
	TokenBudget        int    `json:"token_budget"`
	ConfirmDestination string `json:"confirm_destination,omitempty"`
}

// Submit 使用调用者原始载荷查回执，不能因受理后目标版本变化把重试解释为新操作。
func (s *Service) Submit(ctx context.Context, actor identity.Credential, space, id string, request SubmitTurn) (Receipt, error) {
	if !validID(space) || !validID(id) || !validID(request.OperationID) {
		return Receipt{}, ErrInvalid
	}
	hash := requestHash("conversation_turn", space, id, request)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = gates(ctx, tx, false); err != nil {
		return Receipt{}, err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, true); err != nil {
		return Receipt{}, err
	}
	if result, found, e := operation(ctx, tx, actor.Device.ID, request.OperationID, hash); e != nil || found {
		return result, e
	}
	if err = tx.Rollback(ctx); err != nil {
		return Receipt{}, err
	}
	var c Conversation
	if err = s.ReadConversation(ctx, actor, space, id, 0, 1, func(page TurnPage) error { c = page.Conversation; return nil }); err != nil {
		return Receipt{}, err
	}
	return s.Create(ctx, actor, space, runtimeGoal(c.GoalID), Create{operationHash: &hash, ConversationID: id, SessionID: id, ConversationVersion: request.ExpectedVersion, ConfirmDestination: request.ConfirmDestination, OperationID: request.OperationID, ExpectedVersion: c.GoalVersion, TeachingSessionID: c.TeachingSessionID, Prompt: request.Prompt, Save: c.Saved, RequestBudget: request.RequestBudget, TokenBudget: request.TokenBudget})
}

type ConversationQuery struct {
	GoalID, TeachingSessionID, Search, Cursor string
	AllContexts                               bool
	Limit                                     int
}

type ConversationPage struct {
	Items         []Conversation `json:"items"`
	NextCursor    string         `json:"next_cursor,omitempty"`
	SaveAvailable bool           `json:"save_available"`
}

type TutorTurn struct {
	RunID         string                `json:"run_id"`
	Ordinal       int64                 `json:"ordinal"`
	Status        string                `json:"status"`
	BodyAvailable bool                  `json:"body_available"`
	Messages      []modelclient.Message `json:"messages"`
	Output        string                `json:"output"`
}

type TurnPage struct {
	Conversation Conversation `json:"conversation"`
	Items        []TutorTurn  `json:"items"`
	NextCursor   int64        `json:"next_cursor,omitempty"`
}

type conversationTitle struct {
	Text      string `json:"text"`
	Automatic bool   `json:"automatic"`
}

// 版本位于认证密文内，不能将未来格式解释成空历史。AAD 同时绑定用途、会话和代次。
func sealHistory(a cipher.AEAD, aad string, value any) ([]byte, error) {
	if a == nil {
		return nil, ErrStorage
	}
	data, err := json.Marshal(value)
	if err != nil || len(data) > MaxBody {
		return nil, ErrLimit
	}
	raw, err := json.Marshal(struct {
		Version int             `json:"version"`
		Value   json.RawMessage `json:"value"`
	}{1, data})
	if err != nil {
		return nil, ErrStorage
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, ErrStorage
	}
	return a.Seal(nonce, nonce, raw, []byte(aad)), nil
}

func openHistory(a cipher.AEAD, aad string, raw []byte, value any) error {
	if a == nil || len(raw) < a.NonceSize() || len(raw) > MaxBody+256 {
		return ErrStorage
	}
	plain, err := a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], []byte(aad))
	if err != nil {
		return ErrStorage
	}
	var envelope struct {
		Version int             `json:"version"`
		Value   json.RawMessage `json:"value"`
	}
	if json.Unmarshal(plain, &envelope) != nil {
		return ErrStorage
	}
	if envelope.Version != 1 {
		return ErrHistorySchema
	}
	decoder := json.NewDecoder(bytes.NewReader(envelope.Value))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return ErrStorage
	}
	return nil
}

func historyAAD(id string, generation int64, part string) string {
	return fmt.Sprintf("tutor:%s:%d:%s", id, generation, part)
}

func destination(provider, endpoint string) string {
	hash := sha256.Sum256([]byte(provider + "\n" + endpoint))
	return hex.EncodeToString(hash[:])
}

const conversationColumns = `c.id::text,c.space_id::text,COALESCE(c.goal_id::text,''),COALESCE(c.teaching_session_id::text,''),c.privacy_generation,c.version,c.saved,c.provider,c.endpoint,c.title,c.updated_at,COALESCE(s.current_run_id::text,''),c.deleted,c.request_hash`

func scanConversation(r pgx.Row) (Conversation, error) {
	var c Conversation
	err := r.Scan(&c.ID, &c.SpaceID, &c.GoalID, &c.TeachingSessionID, &c.Generation, &c.Version, &c.Saved, &c.Provider, &c.Endpoint, &c.titleCipher, &c.UpdatedAt, &c.CurrentRunID, &c.deleted, &c.requestHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func conversationRow(ctx context.Context, tx pgx.Tx, actor identity.Credential, space, id string, generation int64, write bool) (Conversation, error) {
	lock := " FOR SHARE OF c"
	if write {
		lock = " FOR UPDATE OF c"
	}
	return scanConversation(tx.QueryRow(ctx, `SELECT `+conversationColumns+` FROM learning_tutor_conversations c JOIN learning_mentor_sessions s ON s.id=c.id WHERE c.id=$1 AND c.device_id=$2 AND c.space_id=$3 AND c.privacy_generation=$4`+lock, id, actor.Device.ID, space, generation))
}

func (s *Service) describeConversation(ctx context.Context, tx pgx.Tx, c *Conversation) error {
	c.GoalVersion = 1
	c.Title = "临时对话"
	c.StorageState = "temporary"
	if c.Saved {
		var title conversationTitle
		if err := openHistory(s.aead, historyAAD(c.ID, c.Generation, "title"), c.titleCipher, &title); err != nil {
			return err
		}
		c.Title = title.Text
		c.StorageState = "saved"
	}
	c.GoalVersion = 1
	if c.GoalID != "" {
		if err := tx.QueryRow(ctx, `SELECT aggregate_version FROM learning_aggregate_heads WHERE aggregate_type='goal' AND aggregate_id=$1`, c.GoalID).Scan(&c.GoalVersion); err != nil {
			return err
		}
	}
	_, err := goalGate(ctx, tx, c.SpaceID, runtimeGoal(c.GoalID), c.GoalVersion)
	if err != nil && !errors.Is(err, ErrInactive) {
		return err
	}
	c.Writable = err == nil
	view := s.settings.View().EffectiveMentor
	c.DestinationProvider, c.DestinationEndpoint = view.Provider, view.Endpoint
	c.Destination = destination(view.Provider, view.Endpoint)
	c.ConfirmationRequired = c.Provider != view.Provider || c.Endpoint != view.Endpoint
	if !c.Saved && c.CurrentRunID != "" {
		item, e := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1`, c.CurrentRunID))
		if e != nil {
			return e
		}
		if !item.BodyAvailable || time.Now().After(item.ExpiresAt) || s.decode(&item) != nil {
			c.StorageState = "temporary_unavailable"
			c.Writable = false
		}
	}
	return nil
}

func runtimeGoal(goal string) string {
	if goal == "" {
		return uuid.Nil.String()
	}
	return goal
}

func (s *Service) NewConversation(ctx context.Context, actor identity.Credential, space string, request NewConversation) (string, error) {
	if !validID(space) || !validID(request.ID) || request.GoalID != "" && !validID(request.GoalID) || request.TeachingSessionID != "" && (!validID(request.TeachingSessionID) || request.GoalID == "") || request.Title != "" && !validText(request.Title, 240) || !request.Saved && request.Title != "" {
		return "", ErrInvalid
	}
	if request.TeachingSessionID != "" {
		if !s.changes.Available() {
			return "", ErrInvalid
		}
		scoped, _ := learningspace.WithScope(ctx, space)
		if _, err := s.changes.Snapshot(scoped, actor, request.GoalID, request.TeachingSessionID); err != nil {
			return "", err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, true)
	if err != nil {
		return "", err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, true); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,33))`, request.ID); err != nil {
		return "", err
	}
	hash := requestHash("conversation", space, request.ID, request)
	old, err := conversationRow(ctx, tx, actor, space, request.ID, generation, true)
	if err == nil {
		if old.deleted || !bytes.Equal(old.requestHash, hash[:]) {
			return "", ErrOperation
		}
		return old.ID, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	version := int64(1)
	if request.GoalID != "" {
		if err = tx.QueryRow(ctx, `SELECT aggregate_version FROM learning_aggregate_heads WHERE aggregate_type='goal' AND aggregate_id=$1`, request.GoalID).Scan(&version); err != nil {
			return "", ErrNotFound
		}
	}
	if _, err = goalGate(ctx, tx, space, runtimeGoal(request.GoalID), version); err != nil {
		return "", err
	}
	if err = s.quota(ctx, tx, 2048); err != nil {
		return "", err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM learning_tutor_conversations WHERE device_id=$1 AND NOT deleted`, actor.Device.ID).Scan(&count); err != nil {
		return "", err
	}
	if count >= 200 {
		return "", ErrLimit
	}
	var ciphertext []byte
	if request.Saved {
		title := request.Title
		if title == "" {
			title = "新导师对话"
		}
		ciphertext, err = sealHistory(s.aead, historyAAD(request.ID, generation, "title"), conversationTitle{Text: title, Automatic: request.Title == ""})
		if err != nil {
			return "", err
		}
	}
	view := s.settings.View().EffectiveMentor
	if _, err = tx.Exec(ctx, `INSERT INTO learning_mentor_sessions(id,device_id,space_id,goal_id,privacy_generation,kind,history) VALUES($1,$2,$3,$4,$5,'mentor',TRUE)`, request.ID, actor.Device.ID, space, runtimeGoal(request.GoalID), generation); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_tutor_conversations(id,device_id,space_id,goal_id,teaching_session_id,privacy_generation,saved,provider,endpoint,title,request_hash) VALUES($1,$2,$3,NULLIF($4,'')::uuid,NULLIF($5,'')::uuid,$6,$7,$8,$9,$10,$11)`, request.ID, actor.Device.ID, space, request.GoalID, request.TeachingSessionID, generation, request.Saved, view.Provider, view.Endpoint, ciphertext, hash[:]); err != nil {
		return "", err
	}
	return request.ID, tx.Commit(ctx)
}

func (s *Service) ReadConversations(ctx context.Context, actor identity.Credential, space string, q ConversationQuery, send func(ConversationPage) error) error {
	if !validID(space) || q.Limit < 1 || q.Limit > 50 || len(q.Search) > 240 || q.GoalID != "" && !validID(q.GoalID) || q.TeachingSessionID != "" && !validID(q.TeachingSessionID) {
		return ErrInvalid
	}
	var cursor struct {
		Time time.Time
		ID   string
	}
	if q.Cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(q.Cursor)
		if e != nil || len(raw) > 256 || json.Unmarshal(raw, &cursor) != nil || !validID(cursor.ID) || cursor.Time.IsZero() {
			return ErrInvalid
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, false)
	if err != nil {
		return err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, false); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT `+conversationColumns+` FROM learning_tutor_conversations c JOIN learning_mentor_sessions s ON s.id=c.id WHERE c.device_id=$1 AND c.space_id=$2 AND c.privacy_generation=$3 AND NOT c.deleted AND ($4 OR (COALESCE(c.goal_id::text,'')=$5 AND COALESCE(c.teaching_session_id::text,'')=$6)) ORDER BY c.updated_at DESC,c.id DESC LIMIT 201`, actor.Device.ID, space, generation, q.AllContexts, q.GoalID, q.TeachingSessionID)
	if err != nil {
		return err
	}
	items := []Conversation{}
	for rows.Next() {
		c, e := scanConversation(rows)
		if e != nil {
			rows.Close()
			return e
		}
		items = append(items, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(items) > 200 {
		return ErrLimit
	}
	page := ConversationPage{Items: []Conversation{}, SaveAvailable: s.CanSave()}
	for _, c := range items {
		if q.Cursor != "" && (c.UpdatedAt.After(cursor.Time) || c.UpdatedAt.Equal(cursor.Time) && c.ID >= cursor.ID) {
			continue
		}
		if err = s.describeConversation(ctx, tx, &c); err != nil {
			// 损坏项仍可定位和删除；详情返回原始错误，不伪造为空历史。
			if !errors.Is(err, ErrStorage) && !errors.Is(err, ErrHistorySchema) {
				return err
			}
			c.Title = "历史不可读取"
			c.StorageState = err.Error()
			c.Writable = false
		}
		if q.Search != "" && !strings.Contains(strings.ToLower(c.Title), strings.ToLower(q.Search)) {
			continue
		}
		page.Items = append(page.Items, c)
		if len(page.Items) > q.Limit {
			page.Items = page.Items[:q.Limit]
			last := page.Items[q.Limit-1]
			raw, _ := json.Marshal(struct {
				Time time.Time
				ID   string
			}{last.UpdatedAt, last.ID})
			page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
	}
	return send(page)
}

func (s *Service) ReadConversation(ctx context.Context, actor identity.Credential, space, id string, after int64, limit int, send func(TurnPage) error) error {
	if !validID(space) || !validID(id) || after < 0 || limit < 1 || limit > 50 {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, false)
	if err != nil {
		return err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, false); err != nil {
		return err
	}
	c, err := conversationRow(ctx, tx, actor, space, id, generation, false)
	if err != nil {
		return err
	}
	if c.deleted {
		return ErrNotFound
	}
	if err = s.describeConversation(ctx, tx, &c); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT t.run_id::text,t.ordinal,t.ciphertext,r.state FROM learning_tutor_turns t JOIN learning_mentor_runs r ON r.id=t.run_id WHERE t.conversation_id=$1 AND t.ordinal>$2 ORDER BY t.ordinal LIMIT $3`, id, after, limit+1)
	if err != nil {
		return err
	}
	page := TurnPage{Conversation: c, Items: []TutorTurn{}}
	for rows.Next() {
		var turn TutorTurn
		var raw, state []byte
		var meta Meta
		if err = rows.Scan(&turn.RunID, &turn.Ordinal, &raw, &state); err != nil {
			rows.Close()
			return err
		}
		if json.Unmarshal(state, &meta) != nil {
			rows.Close()
			return ErrStorage
		}
		turn.Status = meta.Status
		turn.Messages = []modelclient.Message{}
		if c.Saved && raw != nil {
			var body Body
			if err = openHistory(s.aead, historyAAD(id, generation, turn.RunID), raw, &body); err != nil {
				rows.Close()
				return err
			}
			turn.Messages = body.Messages
			turn.Output = body.Output
			turn.BodyAvailable = true
		}
		page.Items = append(page.Items, turn)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = page.Items[limit-1].Ordinal
	}
	if !c.Saved {
		for i := range page.Items {
			item, e := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1`, page.Items[i].RunID))
			if e != nil {
				return e
			}
			if item.BodyAvailable && time.Now().Before(item.ExpiresAt) && s.decode(&item) == nil {
				page.Items[i].Messages = turnBody(item).Messages
				page.Items[i].Output = item.body.Output
				page.Items[i].BodyAvailable = true
			}
		}
	}
	return send(page)
}

// 删除/改名均由正式事务确认。会话锁不进入 worker，锁序固定为会话、运行、轮次。
func (s *Service) ChangeConversation(ctx context.Context, actor identity.Credential, space, id string, version int64, title string, remove bool) error {
	if !validID(space) || !validID(id) || version < 1 || !remove && !validText(title, 240) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, true)
	if err != nil {
		return err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, true); err != nil {
		return err
	}
	c, err := conversationRow(ctx, tx, actor, space, id, generation, true)
	if err != nil {
		return err
	}
	if c.deleted {
		if remove {
			return nil
		}
		return ErrNotFound
	}
	if c.Version != version {
		return ErrConflict
	}
	if !remove {
		if !c.Saved {
			return ErrInvalid
		}
		raw, e := sealHistory(s.aead, historyAAD(id, generation, "title"), conversationTitle{Text: title})
		if e != nil {
			return e
		}
		_, err = tx.Exec(ctx, `UPDATE learning_tutor_conversations SET title=$2,version=version+1,updated_at=clock_timestamp() WHERE id=$1`, id, raw)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE session_id=$1 ORDER BY id FOR UPDATE`, id)
	if err != nil {
		return err
	}
	items := []row{}
	for rows.Next() {
		item, e := scan(rows)
		if e != nil {
			rows.Close()
			return e
		}
		items = append(items, item)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, item := range items {
		item.BodyAvailable = false
		item.body = Body{}
		item.Status = "cancelled"
		item.Stage = "cancelled"
		item.Reason = "conversation_deleted"
		item.lease = nil
		item.leaseUntil = nil
		item.ResultUnknown = item.ResultUnknown || item.callStarted
		item.callStarted = false
		if _, err = tx.Exec(ctx, `DELETE FROM learning_mentor_events WHERE run_id=$1`, item.RunID); err != nil {
			return err
		}
		if err = s.save(ctx, tx, &item, "conversation_deleted"); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM learning_tutor_turns WHERE conversation_id=$1`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_mentor_operations SET request_hash=decode(repeat('00',32),'hex') WHERE run_id IN (SELECT id FROM learning_mentor_runs WHERE session_id=$1)`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_tutor_conversations SET title=NULL,request_hash=''::bytea,provider='',endpoint='',deleted=TRUE,version=version+1 WHERE id=$1`, id); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	for _, item := range items {
		s.drop(item.RunID)
	}
	return nil
}

func turnBody(item row) Body {
	start := item.body.HistoryCount
	if start < 0 || start > len(item.body.Messages) {
		return Body{}
	}
	return Body{Messages: item.body.Messages[start:], Output: item.body.Output}
}

func (s *Service) saveTurn(ctx context.Context, tx pgx.Tx, item *row) error {
	if item.ConversationID == "" || !item.Saved || !item.BodyAvailable {
		return nil
	}
	body := turnBody(*item)
	// 增量文本仍由短期 checkpoint 承担；历史只记录已提交消息及终态结果。
	if !terminal(item.Status) {
		body.Output = ""
	}
	raw, err := sealHistory(s.aead, historyAAD(item.ConversationID, item.Generation, item.RunID), body)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE learning_tutor_turns SET ciphertext=$2 WHERE run_id=$1`, item.RunID, raw)
	return err
}

// 未完成工具组整体排除。历史工具消息只作为模型上下文，不交给工具执行器。
func committedMessages(messages []modelclient.Message) []modelclient.Message {
	result := []modelclient.Message{}
	for i := 0; i < len(messages); {
		m := messages[i]
		if m.Role == "tool" {
			i++
			continue
		}
		if len(m.ToolCalls) == 0 {
			result = append(result, m)
			i++
			continue
		}
		end := i + 1
		complete := true
		for _, call := range m.ToolCalls {
			if end >= len(messages) || messages[end].Role != "tool" || messages[end].ToolCallID != call.ID {
				complete = false
				break
			}
			end++
		}
		if !complete {
			break
		}
		result = append(result, messages[i:end]...)
		i = end
	}
	return result
}

func (s *Service) prepareConversation(ctx context.Context, tx pgx.Tx, actor identity.Credential, space, goal string, generation int64, fingerprint string, request Create) ([]modelclient.Message, error) {
	c, err := conversationRow(ctx, tx, actor, space, request.ConversationID, generation, true)
	if err != nil {
		return nil, err
	}
	if c.deleted {
		return nil, ErrNotFound
	}
	if c.Version != request.ConversationVersion {
		return nil, ErrConflict
	}
	if runtimeGoal(c.GoalID) != goal || c.TeachingSessionID != request.TeachingSessionID || c.Saved != request.Save {
		return nil, ErrInvalid
	}
	view := s.settings.View().EffectiveMentor
	_, _, actual, err := s.settings.MentorClient()
	if err != nil || actual != fingerprint {
		return nil, ErrInactive
	}
	if (c.Provider != view.Provider || c.Endpoint != view.Endpoint) && request.ConfirmDestination != destination(view.Provider, view.Endpoint) {
		return nil, ErrDestination
	}
	history := []modelclient.Message{}
	if c.Saved {
		rows, e := tx.Query(ctx, `SELECT run_id::text,ciphertext FROM learning_tutor_turns WHERE conversation_id=$1 ORDER BY ordinal`, c.ID)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var runID string
			var raw []byte
			var body Body
			if e = rows.Scan(&runID, &raw); e != nil {
				rows.Close()
				return nil, e
			}
			if raw == nil {
				continue
			}
			if e = openHistory(s.aead, historyAAD(c.ID, generation, runID), raw, &body); e != nil {
				rows.Close()
				return nil, e
			}
			history = append(history, committedMessages(body.Messages)...)
			if checkpointJSONSize(Body{Messages: history}) > MaxBody-20000 {
				rows.Close()
				return nil, ErrLimit
			}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, err
		}
		var title conversationTitle
		if err = openHistory(s.aead, historyAAD(c.ID, generation, "title"), c.titleCipher, &title); err != nil {
			return nil, err
		}
		if title.Automatic {
			title.Text = bounded(strings.TrimSpace(request.Prompt), 120)
			title.Automatic = false
			c.titleCipher, err = sealHistory(s.aead, historyAAD(c.ID, generation, "title"), title)
			if err != nil {
				return nil, err
			}
		}
	} else if c.CurrentRunID != "" {
		item, e := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1`, c.CurrentRunID))
		if e != nil {
			return nil, e
		}
		if !item.BodyAvailable || time.Now().After(item.ExpiresAt) || s.decode(&item) != nil {
			return nil, ErrTemporary
		}
		history = committedMessages(item.body.Messages)
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_tutor_conversations SET version=version+1,provider=$2,endpoint=$3,title=$4,updated_at=clock_timestamp() WHERE id=$1`, c.ID, view.Provider, view.Endpoint, c.titleCipher); err != nil {
		return nil, err
	}
	return history, nil
}
