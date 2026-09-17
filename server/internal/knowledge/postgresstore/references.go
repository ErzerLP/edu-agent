package postgresstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func referenceTarget(goal, session string) string {
	if session != "" {
		return session
	}
	return goal
}

func (s *Store) LatestGoalContextTx(ctx context.Context, tx pgx.Tx, goal, revision string) (string, string, error) {
	var id, scope string
	err := tx.QueryRow(ctx, `SELECT c.id,c.scope_snapshot_id FROM knowledge_context_revisions c JOIN knowledge_policies p ON p.id=c.policy_id WHERE p.space_id=$1 AND p.goal_id=$2 AND p.goal_revision_id=$3 AND c.reference_selection IS NULL ORDER BY c.created_at DESC,c.id DESC LIMIT 1`, learningspace.Scope(ctx), goal, revision).Scan(&id, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return id, scope, err
}

func (s *Store) ReferenceHeadTx(ctx context.Context, tx pgx.Tx, goal, session string) (knowledge.ReferenceState, error) {
	r := knowledge.ReferenceState{Selection: knowledge.ReferenceSelection{SessionID: session, Entries: []knowledge.ReferenceEntry{}}}
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return r, err
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT h.version,c.id,c.scope_snapshot_id,c.reference_selection FROM knowledge_reference_heads h JOIN knowledge_context_revisions c ON c.id=h.context_id WHERE h.space_id=$1 AND h.goal_id=$2 AND h.target_id=$3`, learningspace.Scope(ctx), goal, referenceTarget(goal, session)).Scan(&r.Version, &r.ContextID, &r.ScopeSnapshotID, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(raw, &r.Selection)
	return r, err
}

// EffectiveReferenceTx 优先使用明确的本次会话政策；其他目标和现场不会被此覆盖。
func (s *Store) EffectiveReferenceTx(ctx context.Context, tx pgx.Tx, goal, session string) (knowledge.ReferenceState, error) {
	r, err := s.ReferenceHeadTx(ctx, tx, goal, session)
	if err != nil || r.Version > 0 || session == "" {
		return r, err
	}
	return s.ReferenceHeadTx(ctx, tx, goal, "")
}

func (s *Store) ReferenceBaseTx(ctx context.Context, tx pgx.Tx, previous, scope string) ([]knowledge.ScopeEntry, error) {
	if previous != "" {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT reference_base_entries FROM knowledge_context_revisions c JOIN knowledge_policies p ON p.id=c.policy_id WHERE c.id=$1 AND p.space_id=$2`, previous, learningspace.Scope(ctx)).Scan(&raw); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			var entries []knowledge.ScopeEntry
			err := json.Unmarshal(raw, &entries)
			return entries, err
		}
	}
	if scope == "" {
		return []knowledge.ScopeEntry{}, nil
	}
	var isScope bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_scope_snapshots WHERE id=$1)`, scope).Scan(&isScope); err != nil {
		return nil, err
	}
	if isScope {
		r, err := readScope(ctx, tx, scope)
		return r.Entries, err
	}
	if err := checkRevision(ctx, tx, scope); err != nil {
		return nil, err
	}
	var collection string
	if err := tx.QueryRow(ctx, `SELECT collection_id FROM knowledge_revisions WHERE id=$1`, scope).Scan(&collection); err != nil {
		return nil, err
	}
	return []knowledge.ScopeEntry{{CollectionID: collection, RevisionID: scope}}, nil
}

// ValidateReferenceEntriesTx 只校验真实引用与章节，不创建冻结范围或发布正文。
func (s *Store) ValidateReferenceEntriesTx(ctx context.Context, tx pgx.Tx, entries []knowledge.ReferenceEntry) error {
	for _, e := range entries {
		if err := checkCollection(ctx, tx, e.CollectionID); err != nil {
			return err
		}
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_revisions WHERE id=$1 AND collection_id=$2 AND redacted_at IS NULL)`, e.RevisionID, e.CollectionID).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return scopeMissing()
		}
		r, err := loadRevision(ctx, tx, e.RevisionID)
		if err != nil {
			return err
		}
		if e.DocumentID == "" {
			continue
		}
		found := false
		for _, d := range r.Documents {
			if d.Revision.DocumentID != e.DocumentID {
				continue
			}
			if e.NodeID == "" {
				found = true
				break
			}
			for _, n := range d.Revision.Nodes {
				found = found || n.NodeID == e.NodeID
			}
		}
		if !found {
			return scopeMissing()
		}
	}
	return nil
}

// PublishReferencesTx 由已确认的 learningchange 事务调用，保留原概念、正文及引用历史。
func (s *Store) PublishReferencesTx(ctx context.Context, tx pgx.Tx, goal, revision, device, operation, previous, scope string, current knowledge.ReferenceState, selection knowledge.ReferenceSelection) (knowledge.ReferenceState, error) {
	var generation int64
	if err := tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('knowledge','write',NULL)`).Scan(&generation); err != nil {
		return current, err
	}
	if err := selection.Validate(); err != nil {
		return current, err
	}
	if err := s.ValidateReferenceEntriesTx(ctx, tx, selection.Entries); err != nil {
		return current, err
	}
	base, err := s.ReferenceBaseTx(ctx, tx, previous, scope)
	if err != nil {
		return current, err
	}
	entries, err := s.ReferenceScopeTx(ctx, tx, base, selection.Entries)
	if err != nil {
		return current, err
	}
	k := knowledge.KnowledgeContextRevision{ID: uuid.NewString(), ScopeSnapshotID: uuid.NewString(), Concepts: []knowledge.ConceptRevision{}}
	request := research.Request{Policy: research.Policy{Mode: "supplement", Domains: []string{}}}
	if previous != "" {
		old, e := s.ContextTx(ctx, tx, previous)
		if e != nil {
			return current, e
		}
		request, k.Concepts, k.PreviousRevisionID = old.Policy.Request, old.Concepts, old.ID
	}
	if len(entries) == 0 {
		// 清空用户选择可以恢复无参考合同；公开冻结接口仍拒绝无意的空范围。
		if err = lockSpaceWrite(ctx, tx); err != nil {
			return current, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO knowledge_scope_snapshots(id,space_id,entries) VALUES($1,$2,'[]')`, k.ScopeSnapshotID, learningspace.Scope(ctx))
	} else {
		_, err = s.FreezeScopeTx(ctx, tx, knowledge.ScopeSnapshot{ID: k.ScopeSnapshotID, Entries: entries})
	}
	if err != nil {
		return current, err
	}
	if err = s.EnsureTeachingScopeWith(ctx, tx, k.ScopeSnapshotID, device); err != nil {
		return current, err
	}
	policy := uuid.NewString()
	raw, _ := json.Marshal(request)
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_policies(id,space_id,goal_id,goal_revision_id,request) VALUES($1,$2,$3,$4,$5)`, policy, learningspace.Scope(ctx), goal, revision, raw); err != nil {
		return current, err
	}
	ids := []string{}
	for _, c := range k.Concepts {
		ids = append(ids, c.RevisionID)
	}
	sel, _ := json.Marshal(selection)
	b, _ := json.Marshal(base)
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_context_revisions(id,policy_id,scope_snapshot_id,run_id,previous_revision_id,concept_revision_ids,reference_selection,reference_base_entries) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,$6,$7,$8)`, k.ID, policy, k.ScopeSnapshotID, operation, k.PreviousRevisionID, ids, sel, b); err != nil {
		return current, err
	}
	result := knowledge.ReferenceState{Version: current.Version + 1, ContextID: k.ID, ScopeSnapshotID: k.ScopeSnapshotID, Selection: selection}
	_, err = tx.Exec(ctx, `INSERT INTO knowledge_reference_heads(space_id,goal_id,target_id,version,context_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT(space_id,goal_id,target_id) DO UPDATE SET version=EXCLUDED.version,context_id=EXCLUDED.context_id`, learningspace.Scope(ctx), goal, referenceTarget(goal, selection.SessionID), result.Version, result.ContextID)
	return result, err
}

// 新版只替换同一文档的旧版；不删除其余基础来源，也不改写任何历史快照。
func (s *Store) ReferenceScopeTx(ctx context.Context, tx pgx.Tx, base []knowledge.ScopeEntry, selected []knowledge.ReferenceEntry) ([]knowledge.ScopeEntry, error) {
	chosen := knowledge.ReferenceScope(nil, selected)
	versions := map[string]string{}
	for _, e := range chosen {
		r, err := loadRevision(ctx, tx, e.RevisionID)
		if err != nil {
			return nil, err
		}
		for _, d := range r.Documents {
			if e.DocumentID != "" && e.DocumentID != d.Revision.DocumentID {
				continue
			}
			if old := versions[d.Revision.DocumentID]; old != "" && old != d.Revision.ID {
				return nil, &knowledge.Error{Code: knowledge.CodeInvalidRequest}
			}
			versions[d.Revision.DocumentID] = d.Revision.ID
		}
	}
	remaining := []knowledge.ScopeEntry{}
	for _, e := range base {
		r, err := loadRevision(ctx, tx, e.RevisionID)
		if err != nil {
			return nil, err
		}
		for _, d := range r.Documents {
			if e.DocumentID != "" && e.DocumentID != d.Revision.DocumentID {
				continue
			}
			if next := versions[d.Revision.DocumentID]; next != "" && next != d.Revision.ID {
				continue
			}
			entry := e
			entry.DocumentID = d.Revision.DocumentID
			remaining = append(remaining, entry)
		}
	}
	return knowledge.ReferenceScope(remaining, selected), nil
}

// PreferredDocuments 在候选截断之前排序；不改变旧检索响应合同。
func (s *Store) PreferredDocuments(ctx context.Context, scope string) (map[string]bool, error) {
	result := map[string]bool{}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err = privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return nil, err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT c.reference_selection FROM knowledge_context_revisions c JOIN knowledge_policies p ON p.id=c.policy_id WHERE c.scope_snapshot_id=$1 AND p.space_id=$2 AND c.reference_selection IS NOT NULL LIMIT 1`, scope, learningspace.Scope(ctx)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	var selection knowledge.ReferenceSelection
	if err = json.Unmarshal(raw, &selection); err != nil {
		return nil, err
	}
	r, err := loadScopedRevision(ctx, tx, scope)
	if err != nil {
		return nil, err
	}
	for _, e := range selection.Entries {
		if e.Role != "prefer" {
			continue
		}
		for _, d := range r.Documents {
			if d.CollectionID == e.CollectionID && d.KnowledgeRevisionID == e.RevisionID && (e.DocumentID == "" || d.Revision.DocumentID == e.DocumentID) {
				result[d.Revision.ID] = true
			}
		}
	}
	return result, nil
}
