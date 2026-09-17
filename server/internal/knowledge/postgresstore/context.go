package postgresstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ContextMatchesTx 用 knowledge 自己的政策校验现场中的可空上下文引用。
func (s *Store) ContextMatchesTx(ctx context.Context, tx pgx.Tx, id, goalRevision, scope string) (bool, error) {
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return false, err
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_context_revisions c JOIN knowledge_policies p ON p.id=c.policy_id WHERE c.id=$1 AND p.goal_revision_id=$2 AND c.scope_snapshot_id=$3 AND p.space_id=$4)`, id, goalRevision, scope, learningspace.Scope(ctx)).Scan(&valid)
	return valid, err
}

// RebindContextTx 在用户已确认同一目标的新标准后追加政策版本，保持原来源和概念不变。
func (s *Store) RebindContextTx(ctx context.Context, tx pgx.Tx, id, goal, revision, operation string) (string, error) {
	old, err := s.ContextTx(ctx, tx, id)
	if err != nil {
		return "", err
	}
	var owned bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_policies WHERE id=$1 AND goal_id=$2 AND space_id=$3)`, old.Policy.ID, goal, learningspace.Scope(ctx)).Scan(&owned)
	if err != nil {
		return "", err
	}
	if !owned {
		return "", scopeMissing()
	}
	policy, newID := uuid.NewString(), uuid.NewString()
	raw, err := json.Marshal(old.Policy.Request)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO knowledge_policies(id,space_id,goal_id,goal_revision_id,request) VALUES($1,$2,$3,$4,$5)`, policy, learningspace.Scope(ctx), goal, revision, raw)
	if err != nil {
		return "", err
	}
	ids := []string{}
	for _, c := range old.Concepts {
		ids = append(ids, c.RevisionID)
	}
	_, err = tx.Exec(ctx, `INSERT INTO knowledge_context_revisions(id,policy_id,scope_snapshot_id,run_id,previous_revision_id,concept_revision_ids) VALUES($1,$2,$3,$4,$5,$6)`, newID, policy, old.ScopeSnapshotID, operation, id, ids)
	return newID, err
}

// ConceptKeys 只提供当前目标已存在的语义身份，供后续准备复用，不读取其他目标。
func (s *Store) ConceptKeys(ctx context.Context, goal string) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err = privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT semantic_key FROM knowledge_concepts WHERE space_id=$1 AND goal_id=$2 ORDER BY semantic_key LIMIT 100`, learningspace.Scope(ctx), goal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// PublishContextTx 仅接受当前运行已正规采纳且符合政策的来源，AI 文本不进入原始文档。
func (s *Store) PublishContextTx(ctx context.Context, tx pgx.Tx, run, goal, goalRevision, device string, request research.Request, concept knowledge.ConceptRevision) (knowledge.KnowledgeContextRevision, error) {
	var result knowledge.KnowledgeContextRevision
	if err := request.Validate(); err != nil || !request.AutoAdopt {
		return result, &knowledge.Error{Code: knowledge.CodeInvalidRequest}
	}
	sources, err := s.ResearchSourcesTx(ctx, tx, run, goal)
	if err != nil {
		return result, err
	}
	if err = (research.Synthesis{Points: []research.Point{{Text: concept.Name, Citations: concept.Support}}}).Validate(sources); err != nil {
		return result, err
	}
	if len(concept.Support) == 0 || strings.TrimSpace(concept.SemanticKey) == "" || len(concept.SemanticKey) > 200 {
		return result, &knowledge.Error{Code: knowledge.CodeInvalidRequest}
	}
	raw, _ := json.Marshal(request)
	policyID := uuid.NewSHA1(uuid.MustParse(goalRevision), raw).String()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "knowledge-policy:"+policyID); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_policies(id,space_id,goal_id,goal_revision_id,request) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, policyID, learningspace.Scope(ctx), goal, goalRevision, raw); err != nil {
		return result, err
	}
	result = knowledge.KnowledgeContextRevision{ID: uuid.NewString(), Policy: knowledge.KnowledgePolicy{ID: policyID, GoalRevisionID: goalRevision, Request: request}, ScopeSnapshotID: uuid.NewString(), Concepts: []knowledge.ConceptRevision{}}
	entries := []knowledge.ScopeEntry{}
	var previousScope string
	err = tx.QueryRow(ctx, `SELECT id,scope_snapshot_id FROM knowledge_context_revisions WHERE policy_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, policyID).Scan(&result.PreviousRevisionID, &previousScope)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if previousScope != "" {
		previous, e := s.ContextTx(ctx, tx, result.PreviousRevisionID)
		if e != nil {
			return result, e
		}
		result.Concepts = previous.Concepts
		snapshot, e := readScope(ctx, tx, previousScope)
		if e != nil {
			return result, e
		}
		entries = snapshot.Entries
	}
	known := map[string]bool{}
	for _, e := range entries {
		known[e.RevisionID] = true
	}
	for _, source := range sources {
		if !request.Policy.Allows(source.Locator) || !request.Policy.Allows(source.FinalURL) {
			return result, research.ErrPolicy
		}
		if !known[source.KnowledgeRevisionID] {
			entries = append(entries, knowledge.ScopeEntry{CollectionID: source.CollectionID, RevisionID: source.KnowledgeRevisionID})
			known[source.KnowledgeRevisionID] = true
		}
	}
	if _, err = s.FreezeScopeTx(ctx, tx, knowledge.ScopeSnapshot{ID: result.ScopeSnapshotID, Entries: entries}); err != nil {
		return result, err
	}
	if err = s.EnsureTeachingScopeWith(ctx, tx, result.ScopeSnapshotID, device); err != nil {
		return result, err
	}
	concept.SemanticKey = strings.ToLower(strings.Join(strings.Fields(concept.SemanticKey), " "))
	concept.ConceptID = uuid.NewSHA1(uuid.MustParse(goal), []byte(concept.SemanticKey)).String()
	concept.RevisionID = uuid.NewString()
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_concepts(id,space_id,goal_id,semantic_key) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, concept.ConceptID, learningspace.Scope(ctx), goal, concept.SemanticKey); err != nil {
		return result, err
	}
	raw, _ = json.Marshal(concept.Support)
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_concept_revisions(id,concept_id,name,support) VALUES($1,$2,$3,$4)`, concept.RevisionID, concept.ConceptID, concept.Name, raw); err != nil {
		return result, err
	}
	replaced := false
	for i := range result.Concepts {
		if result.Concepts[i].ConceptID == concept.ConceptID {
			result.Concepts[i] = concept
			replaced = true
		}
	}
	if !replaced {
		result.Concepts = append(result.Concepts, concept)
	}
	ids := make([]string, len(result.Concepts))
	for i, c := range result.Concepts {
		ids[i] = c.RevisionID
	}
	var previous *string
	if result.PreviousRevisionID != "" {
		previous = &result.PreviousRevisionID
	}
	_, err = tx.Exec(ctx, `INSERT INTO knowledge_context_revisions(id,policy_id,scope_snapshot_id,run_id,previous_revision_id,concept_revision_ids) VALUES($1,$2,$3,$4,$5,$6)`, result.ID, policyID, result.ScopeSnapshotID, run, previous, ids)
	return result, err
}

func (s *Store) ContextTx(ctx context.Context, tx pgx.Tx, id string) (knowledge.KnowledgeContextRevision, error) {
	var result knowledge.KnowledgeContextRevision
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return result, err
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT c.id,c.scope_snapshot_id,COALESCE(c.previous_revision_id::text,''),p.id,p.goal_revision_id,p.request FROM knowledge_context_revisions c JOIN knowledge_policies p ON p.id=c.policy_id WHERE c.id=$1 AND p.space_id=$2`, id, learningspace.Scope(ctx)).Scan(&result.ID, &result.ScopeSnapshotID, &result.PreviousRevisionID, &result.Policy.ID, &result.Policy.GoalRevisionID, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, scopeMissing()
	}
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(raw, &result.Policy.Request); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id,r.id,c.semantic_key,r.name,r.support FROM knowledge_context_revisions k CROSS JOIN LATERAL unnest(k.concept_revision_ids) WITH ORDINALITY v(id,n) JOIN knowledge_concept_revisions r ON r.id=v.id JOIN knowledge_concepts c ON c.id=r.concept_id WHERE k.id=$1 ORDER BY v.n`, id)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	result.Concepts = []knowledge.ConceptRevision{}
	for rows.Next() {
		var c knowledge.ConceptRevision
		if err = rows.Scan(&c.ConceptID, &c.RevisionID, &c.SemanticKey, &c.Name, &raw); err != nil {
			return result, err
		}
		if err = json.Unmarshal(raw, &c.Support); err != nil {
			return result, err
		}
		result.Concepts = append(result.Concepts, c)
	}
	return result, rows.Err()
}

// TreeTx 保留正规资料范围解析，在发布事务内可见刚采用的来源。
func (s *Store) TreeTx(ctx context.Context, tx pgx.Tx, id string) (knowledge.TreeResult, error) {
	r, err := loadScopedRevision(ctx, tx, id)
	return knowledge.TreeResult{Revision: r}, err
}
