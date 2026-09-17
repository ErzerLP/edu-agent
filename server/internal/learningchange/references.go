package learningchange

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ReferenceRequest struct {
	OperationID         string                       `json:"operation_id"`
	ExpectedGoalVersion int64                        `json:"expected_goal_version"`
	Selection           knowledge.ReferenceSelection `json:"selection"`
}
type ReferencePreview struct {
	Receipt                   string                       `json:"receipt"`
	Before                    knowledge.ReferenceState     `json:"before"`
	After                     knowledge.ReferenceSelection `json:"after"`
	RequiresScopeConfirmation bool                         `json:"requires_scope_confirmation"`
	Impact                    string                       `json:"impact"`
}
type ReferenceConfirmation struct {
	Request      ReferenceRequest `json:"request"`
	Receipt      string           `json:"receipt"`
	ConfirmScope bool             `json:"confirm_scope"`
}

type ReferenceMaterial struct {
	State     knowledge.ReferenceState   `json:"state"`
	Documents []knowledge.ExportDocument `json:"documents"`
	Truncated bool                       `json:"truncated"`
}

// 正文只来自有效目标的已采用范围；模型不能指定其他范围或传入新的身份决定。
func (s *Service) ReferenceMaterial(ctx context.Context, actor identity.Credential, goal, session string) (ReferenceMaterial, error) {
	result := ReferenceMaterial{Documents: []knowledge.ExportDocument{}}
	tx, _, err := s.referenceBegin(ctx, actor, false)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	if uuid.Validate(goal) != nil || session != "" && uuid.Validate(session) != nil {
		return result, ErrInvalid
	}
	g, err := s.scope(ctx, tx, goal, session, false)
	if err != nil {
		return result, err
	}
	if session != "" {
		l := s.learning.WithinTx(tx)
		if err = l.ValidateSessionScope(ctx, session); err != nil {
			return result, err
		}
		a, e := l.LoadSessionAuthority(ctx, session)
		if e != nil {
			return result, e
		}
		bound, e := l.LoadGoalRevision(ctx, a.Session.Context.GoalRevisionID)
		if e != nil {
			return result, e
		}
		if bound.GoalID != goal {
			return result, ErrNotFound
		}
	}
	result.State, err = s.knowledge.EffectiveReferenceTx(ctx, tx, goal, session)
	if err != nil || result.State.Version == 0 {
		return result, err
	}
	k, err := s.knowledge.ContextTx(ctx, tx, result.State.ContextID)
	if err != nil {
		return result, err
	}
	if k.Policy.GoalRevisionID != g.ID {
		return result, ErrConflict
	}
	tree, err := s.knowledge.TreeTx(ctx, tx, result.State.ScopeSnapshotID)
	if err != nil {
		return result, err
	}
	remaining := 12000
	for _, d := range tree.Revision.Documents {
		text := d.Revision.CanonicalMarkdown
		if d.SelectedRange != nil {
			text = text[d.SelectedRange.Start:d.SelectedRange.End]
		}
		if len(text) > remaining {
			n := remaining
			for n > 0 && !utf8.RuneStart(text[n]) {
				n--
			}
			text, result.Truncated = text[:n], true
		}
		if text != "" {
			result.Documents = append(result.Documents, knowledge.ExportDocument{CollectionID: d.CollectionID, KnowledgeRevisionID: d.KnowledgeRevisionID, Path: d.Path, Markdown: text})
		}
		remaining -= len(text)
		if remaining <= 0 {
			result.Truncated = true
			break
		}
	}
	return result, nil
}

type referenceBasis struct {
	Space, Goal, Actor, Hash      string
	Generation                    int64
	Expires                       int64
	Before                        knowledge.ReferenceState
	Previous, Scope, GoalRevision string
	Parents                       map[string]string
}

func (s *Service) ReferencesAvailable() bool {
	return s != nil && s.aead != nil && s.learning != nil && s.knowledge != nil
}

func referencePermission(ctx context.Context, tx pgx.Tx, actor identity.Credential) error {
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT d.revoked_at IS NULL AND t.revoked_at IS NULL AND ARRAY['references:manage','knowledge:read','knowledge:write','knowledge:approve']::text[] <@ t.scopes FROM devices d JOIN device_tokens t ON t.device_id=d.id WHERE d.id=$1 AND t.id=$2 FOR SHARE OF d,t`, actor.Device.ID, actor.TokenID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !allowed {
		return ErrForbidden
	}
	return err
}

func (s *Service) referenceBegin(ctx context.Context, actor identity.Credential, write bool) (pgx.Tx, int64, error) {
	if !s.ReferencesAvailable() {
		return nil, 0, ErrUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	g, err := gates(ctx, tx, actor, write)
	if err == nil {
		err = referencePermission(ctx, tx, actor)
	}
	if err != nil {
		tx.Rollback(context.Background())
		return nil, 0, err
	}
	return tx, g, nil
}

func (s *Service) referenceBasis(ctx context.Context, tx pgx.Tx, actor identity.Credential, goal string, c ReferenceRequest, generation int64) (referenceBasis, error) {
	b := referenceBasis{Space: learningspace.Scope(ctx), Goal: goal, Actor: actor.Device.ID, Hash: digest(c), Generation: generation, Parents: map[string]string{}}
	if uuid.Validate(goal) != nil || uuid.Validate(c.OperationID) != nil || c.Selection.Validate() != nil || c.ExpectedGoalVersion < 1 {
		return b, ErrInvalid
	}
	g, err := s.scope(ctx, tx, goal, c.Selection.SessionID, true)
	if err != nil {
		return b, err
	}
	if g.Revision != c.ExpectedGoalVersion {
		return b, ErrConflict
	}
	b.GoalRevision = g.ID
	b.Scope = g.GoalManagement().Details.ScopeSnapshotID
	if c.Selection.SessionID == "" {
		id, scope, e := s.knowledge.LatestGoalContextTx(ctx, tx, goal, g.ID)
		if e != nil {
			return b, e
		}
		if id != "" {
			b.Previous, b.Scope = id, scope
		}
	}
	if c.Selection.SessionID != "" {
		l := s.learning.WithinTx(tx)
		if err = l.ValidateSessionScope(ctx, c.Selection.SessionID); err != nil {
			return b, err
		}
		a, e := l.LoadSessionAuthority(ctx, c.Selection.SessionID)
		if e != nil {
			return b, e
		}
		bound, e := l.LoadGoalRevision(ctx, a.Session.Context.GoalRevisionID)
		if e != nil {
			return b, e
		}
		if bound.GoalID != goal {
			return b, ErrNotFound
		}
		b.Scope = a.Session.Context.KnowledgeRevisionID
		id, e := l.ReadSessionContextTx(ctx, tx, c.Selection.SessionID)
		if e != nil {
			return b, e
		}
		if id != nil {
			b.Previous = *id
		}
	}
	b.Before, err = s.knowledge.EffectiveReferenceTx(ctx, tx, goal, c.Selection.SessionID)
	if err != nil {
		return b, err
	}
	if b.Before.Version > 0 {
		b.Previous, b.Scope = b.Before.ContextID, b.Before.ScopeSnapshotID
	}
	if err = s.knowledge.ValidateReferenceEntriesTx(ctx, tx, c.Selection.Entries); err != nil {
		return b, err
	}
	for _, e := range c.Selection.Entries {
		var head *string
		if err = tx.QueryRow(ctx, `SELECT head_revision_id::text FROM knowledge_collections WHERE id=$1 FOR SHARE`, e.CollectionID).Scan(&head); err != nil {
			return b, err
		}
		if head != nil {
			b.Parents[e.CollectionID] = *head
		}
	}
	base, err := s.knowledge.ReferenceBaseTx(ctx, tx, b.Previous, b.Scope)
	if err != nil {
		return b, err
	}
	_, err = s.knowledge.ReferenceScopeTx(ctx, tx, base, c.Selection.Entries)
	return b, err
}

func referenceScopeChange(before, after knowledge.ReferenceSelection) bool {
	filter := func(s knowledge.ReferenceSelection) []knowledge.ScopeEntry {
		v := []knowledge.ScopeEntry{}
		for _, e := range s.Entries {
			if e.Role == "restrict" {
				v = append(v, e.ScopeEntry)
			}
		}
		return v
	}
	return digest(filter(before)) != digest(filter(after))
}

func (s *Service) PreviewReferences(ctx context.Context, actor identity.Credential, goal string, c ReferenceRequest) (ReferencePreview, error) {
	tx, generation, err := s.referenceBegin(ctx, actor, false)
	if err != nil {
		return ReferencePreview{}, err
	}
	defer tx.Rollback(context.Background())
	b, err := s.referenceBasis(ctx, tx, actor, goal, c, generation)
	if err != nil {
		return ReferencePreview{}, err
	}
	b.Expires = time.Now().Add(15 * time.Minute).Unix()
	raw, _ := json.Marshal(b)
	n := make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(n); err != nil {
		return ReferencePreview{}, err
	}
	sealed := s.aead.Seal(n, n, raw, []byte("reference-preview-v1"))
	return ReferencePreview{Receipt: base64.RawURLEncoding.EncodeToString(sealed), Before: b.Before, After: c.Selection, RequiresScopeConfirmation: referenceScopeChange(b.Before.Selection, c.Selection), Impact: "只用于所选目标或本次教学现场的后续准备。已发行题目、答案、评分和旧冻结来源保持原版本；不会开课或切换当前题目。"}, nil
}

func (s *Service) ReferenceOperation(ctx context.Context, actor identity.Credential, goal, id string) (knowledge.ReferenceState, error) {
	tx, _, err := s.referenceBegin(ctx, actor, false)
	if err != nil {
		return knowledge.ReferenceState{}, err
	}
	defer tx.Rollback(context.Background())
	if uuid.Validate(goal) != nil || uuid.Validate(id) != nil {
		return knowledge.ReferenceState{}, ErrInvalid
	}
	if _, err = s.scope(ctx, tx, goal, "", false); err != nil {
		return knowledge.ReferenceState{}, err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT result FROM knowledge_reference_operations WHERE device_id=$1 AND operation_id=$2 AND space_id=$3 AND goal_id=$4`, actor.Device.ID, id, learningspace.Scope(ctx), goal).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledge.ReferenceState{}, ErrNotFound
	}
	var result knowledge.ReferenceState
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return result, err
}

func (s *Service) ConfirmReferences(ctx context.Context, actor identity.Credential, goal string, c ReferenceConfirmation) (knowledge.ReferenceState, error) {
	tx, generation, err := s.referenceBegin(ctx, actor, true)
	if err != nil {
		return knowledge.ReferenceState{}, err
	}
	defer tx.Rollback(context.Background())
	if uuid.Validate(goal) != nil || uuid.Validate(c.Request.OperationID) != nil {
		return knowledge.ReferenceState{}, ErrInvalid
	}
	if err = lock(ctx, tx, "reference-operation:"+actor.Device.ID+":"+c.Request.OperationID); err != nil {
		return knowledge.ReferenceState{}, err
	}
	hash := digest(struct {
		Space, Goal string
		Request     ReferenceRequest
	}{learningspace.Scope(ctx), goal, c.Request})
	var oldHash string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,result FROM knowledge_reference_operations WHERE device_id=$1 AND operation_id=$2`, actor.Device.ID, c.Request.OperationID).Scan(&oldHash, &raw)
	if err == nil {
		if hash != oldHash {
			return knowledge.ReferenceState{}, ErrConflict
		}
		var result knowledge.ReferenceState
		err = json.Unmarshal(raw, &result)
		return result, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return knowledge.ReferenceState{}, err
	}
	sealed, err := base64.RawURLEncoding.DecodeString(c.Receipt)
	n := s.aead.NonceSize()
	if err != nil || len(sealed) < n {
		return knowledge.ReferenceState{}, ErrConflict
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], []byte("reference-preview-v1"))
	var approved referenceBasis
	if err != nil || json.Unmarshal(plain, &approved) != nil || approved.Expires < time.Now().Unix() {
		return knowledge.ReferenceState{}, ErrConflict
	}
	b, err := s.referenceBasis(ctx, tx, actor, goal, c.Request, generation)
	if err != nil {
		return knowledge.ReferenceState{}, err
	}
	b.Expires = approved.Expires
	if digest(b) != digest(approved) || referenceScopeChange(b.Before.Selection, c.Request.Selection) && !c.ConfirmScope {
		return knowledge.ReferenceState{}, ErrConflict
	}
	result, err := s.knowledge.PublishReferencesTx(ctx, tx, goal, b.GoalRevision, actor.Device.ID, c.Request.OperationID, b.Previous, b.Scope, b.Before, c.Request.Selection)
	if err != nil {
		return result, err
	}
	raw, _ = json.Marshal(result)
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_reference_operations(device_id,operation_id,space_id,goal_id,request_hash,result) VALUES($1,$2,$3,$4,$5,$6)`, actor.Device.ID, c.Request.OperationID, learningspace.Scope(ctx), goal, hash, raw); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

func (s *Service) ReadReferences(ctx context.Context, actor identity.Credential, goal, session string) (knowledge.ReferenceState, error) {
	tx, _, err := s.referenceBegin(ctx, actor, false)
	if err != nil {
		return knowledge.ReferenceState{}, err
	}
	defer tx.Rollback(context.Background())
	if uuid.Validate(goal) != nil || session != "" && uuid.Validate(session) != nil {
		return knowledge.ReferenceState{}, ErrInvalid
	}
	if _, err = s.scope(ctx, tx, goal, session, false); err != nil {
		return knowledge.ReferenceState{}, err
	}
	if session != "" {
		l := s.learning.WithinTx(tx)
		if err = l.ValidateSessionScope(ctx, session); err != nil {
			return knowledge.ReferenceState{}, err
		}
		a, e := l.LoadSessionAuthority(ctx, session)
		if e != nil {
			return knowledge.ReferenceState{}, e
		}
		g, e := l.LoadGoalRevision(ctx, a.Session.Context.GoalRevisionID)
		if e != nil {
			return knowledge.ReferenceState{}, e
		}
		if g.GoalID != goal {
			return knowledge.ReferenceState{}, ErrNotFound
		}
	}
	return s.knowledge.EffectiveReferenceTx(ctx, tx, goal, session)
}
