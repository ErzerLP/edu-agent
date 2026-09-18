package postgresstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// 学习事实由 learning owner 提供；knowledge 只持有只读事务端口。
type StructureLearningReader interface {
	StructureLearningImpactTx(context.Context, pgx.Tx, []string) (knowledge.StructureImpact, error)
	ConceptLearningStateTx(context.Context, pgx.Tx, []string, []string) (string, error)
}

func (s *Store) SetStructureLearningReader(reader StructureLearningReader) {
	s.structureLearning = reader
}

// 在 LIMIT 前过滤全部出处，解除引用不能通过图中的摘要读取原资料。
const structureVisible = `c.space_id=$1
 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(COALESCE(r.structure->'sources','[]')) src
 WHERE NOT EXISTS(SELECT 1 FROM knowledge_collection_links l JOIN knowledge_revisions kr ON kr.collection_id=l.collection_id
 WHERE l.space_id=c.space_id AND l.collection_id::text=src->>'collection_id' AND kr.id::text=src->>'revision_id' AND kr.redacted_at IS NULL))
 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(r.support) citation WHERE NOT EXISTS(
 SELECT 1 FROM knowledge_collection_links l JOIN knowledge_revisions kr ON kr.collection_id=l.collection_id
 LEFT JOIN knowledge_source_revisions sr ON sr.knowledge_revision_id=kr.id
 LEFT JOIN knowledge_snapshot_documents sd ON sd.knowledge_revision_id=kr.id
 WHERE l.space_id=c.space_id AND kr.redacted_at IS NULL AND
 ((sr.source_id::text=citation->>'source_id' AND sr.source_revision_id::text=citation->>'revision_id') OR
 (sd.document_revision_id::text=citation->>'source_id' AND kr.id::text=citation->>'revision_id'))))`
const structureFrom = ` FROM knowledge_concepts c JOIN knowledge_concept_heads h ON h.concept_id=c.id JOIN knowledge_concept_revisions r ON r.id=h.revision_id `
const structureColumns = `c.id,r.id,COALESCE(c.goal_id::text,''),c.semantic_key,r.name,r.support,r.structure`

// 冻结提案包含前后正文，同样在分页之前使用节点的出处授权条件。
var structureProposalVisible = `space_id=$1 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(COALESCE(record->'before','[]') || COALESCE(record->'after','[]')) n WHERE NOT (` + strings.NewReplacer("c.space_id", "$1", "r.structure", "(n->'content')", "r.support", "(n->'support')").Replace(structureVisible) + `))`

func structureError(code string) error { return &knowledge.Error{Code: code} }
func normalizeStructureNode(n *knowledge.StructureNode) {
	if n.Support == nil {
		n.Support = []research.Citation{}
	}
	c := &n.Content
	c.Claims = append([]knowledge.ConceptClaim{}, c.Claims...)
	c.Relations = append([]knowledge.ConceptRelation{}, c.Relations...)
	if c.SourceStatus == "" {
		c.SourceStatus = "included"
		c.Suggested = true
	}
	if c.Sources == nil {
		c.Sources = []knowledge.ConceptSource{}
	}
	if c.Claims == nil {
		c.Claims = []knowledge.ConceptClaim{}
	}
	if c.Relations == nil {
		c.Relations = []knowledge.ConceptRelation{}
	}
	if c.ReplacedBy == nil {
		c.ReplacedBy = []string{}
	}
	for i := range c.Claims {
		if c.Claims[i].Sources == nil {
			c.Claims[i].Sources = []int{}
		}
	}
	for i := range c.Relations {
		if c.Relations[i].Sources == nil {
			c.Relations[i].Sources = []int{}
		}
	}
	n.LearningState = "insufficient_evidence"
}
func scanStructureNode(row pgx.Row) (knowledge.StructureNode, error) {
	var n knowledge.StructureNode
	var support, raw []byte
	err := row.Scan(&n.ConceptID, &n.RevisionID, &n.GoalID, &n.SemanticKey, &n.Name, &support, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return n, scopeMissing()
	}
	if err != nil {
		return n, err
	}
	if err = json.Unmarshal(support, &n.Support); err != nil {
		return n, err
	}
	if err = json.Unmarshal(raw, &n.Content); err != nil {
		return n, err
	}
	normalizeStructureNode(&n)
	return n, nil
}
func readStructureNode(ctx context.Context, tx pgx.Tx, id, revision string) (knowledge.StructureNode, error) {
	from := structureFrom
	if revision != "" {
		from = ` FROM knowledge_concepts c JOIN knowledge_concept_revisions r ON r.concept_id=c.id AND r.id=$3 `
	}
	args := []any{learningspace.Scope(ctx), id}
	if revision != "" {
		args = append(args, revision)
	}
	return scanStructureNode(tx.QueryRow(ctx, `SELECT `+structureColumns+from+` WHERE `+structureVisible+` AND c.id=$2`, args...))
}
func structureVersion(ctx context.Context, tx pgx.Tx) (int64, error) {
	var v int64
	err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM knowledge_structure_heads WHERE space_id=$1),0)`, learningspace.Scope(ctx)).Scan(&v)
	return v, err
}

type structureCursor struct {
	Space, Goal, Root, Search, After string
	Version, Generation              int64
}

func (s *Store) ReadStructure(ctx context.Context, q knowledge.StructureQuery) (knowledge.StructurePage, error) {
	tx, g, err := s.beginMaintenanceProposalRead(ctx)
	if err != nil {
		return knowledge.StructurePage{}, err
	}
	defer tx.Rollback(context.Background())
	p := knowledge.StructurePage{Generation: g, Items: []knowledge.StructureNode{}, Edges: []knowledge.StructureEdge{}}
	p.Version, err = structureVersion(ctx, tx)
	if err != nil {
		return p, err
	}
	cursor := structureCursor{Space: learningspace.Scope(ctx), Goal: q.GoalID, Root: q.RootID, Search: q.Search, Version: p.Version, Generation: g}
	if q.Cursor != "" {
		var old structureCursor
		b, e := base64.RawURLEncoding.DecodeString(q.Cursor)
		if e != nil || json.Unmarshal(b, &old) != nil || uuid.Validate(old.After) != nil {
			return p, structureError(knowledge.CodeInvalidRequest)
		}
		cursor.After = old.After
		if old != cursor {
			return p, structureError(knowledge.CodeProposalStale)
		}
	}
	if q.RootID != "" {
		if _, err = readStructureNode(ctx, tx, q.RootID, ""); err != nil {
			return p, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT `+structureColumns+structureFrom+` WHERE `+structureVisible+`
 AND ($2='' OR c.goal_id::text=$2) AND ($3='' OR c.id::text>$3)
 AND ($4='' OR position(lower($4) in lower(r.name))>0)
 AND ($5='' OR c.id::text=$5 OR EXISTS(SELECT 1 FROM jsonb_array_elements(COALESCE(r.structure->'relations','[]')) e WHERE e->>'target_id'=$5)
 OR c.id IN (SELECT (e->>'target_id')::uuid FROM knowledge_concept_revisions root CROSS JOIN LATERAL jsonb_array_elements(COALESCE(root.structure->'relations','[]')) e
 WHERE root.id=(SELECT revision_id FROM knowledge_concept_heads WHERE concept_id=NULLIF($5,'')::uuid))) ORDER BY c.id LIMIT $6`, learningspace.Scope(ctx), q.GoalID, cursor.After, q.Search, q.RootID, q.Limit+1)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		n, e := scanStructureNode(rows)
		if e != nil {
			rows.Close()
			return p, e
		}
		p.Items = append(p.Items, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, err
	}
	if len(p.Items) > q.Limit {
		p.Items = p.Items[:q.Limit]
		cursor.After = p.Items[len(p.Items)-1].ConceptID
		raw, _ := json.Marshal(cursor)
		p.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		p.Partial = true
	}
	visible := map[string]bool{}
	for _, n := range p.Items {
		visible[n.ConceptID] = true
	}
	for i := range p.Items {
		n := &p.Items[i]
		edges := []knowledge.ConceptRelation{}
		for _, e := range n.Content.Relations {
			if visible[e.TargetID] {
				edges = append(edges, e)
				p.Edges = append(p.Edges, knowledge.StructureEdge{SourceID: n.ConceptID, ConceptRelation: e})
			} else {
				p.Partial = true
			}
		}
		n.Content.Relations = edges
		if err = s.structureLearningState(ctx, tx, n); err != nil {
			return p, err
		}
	}
	p.Notice = "仅显示当前授权范围；图和列表使用同一页节点，关系不递归展开。"
	if p.Partial {
		p.Notice += "结果已分页或裁剪，缺少连线不代表没有关系。"
	}
	return p, tx.Commit(ctx)
}
func (s *Store) structureLearningState(ctx context.Context, tx pgx.Tx, n *knowledge.StructureNode) error {
	if s.structureLearning == nil {
		return nil
	}
	var contexts []string
	rows, err := tx.Query(ctx, `SELECT k.id FROM knowledge_context_revisions k JOIN knowledge_policies p ON p.id=k.policy_id WHERE p.space_id=$1 AND $2=ANY(k.concept_revision_ids) ORDER BY k.id LIMIT 1001`, learningspace.Scope(ctx), n.RevisionID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		contexts = append(contexts, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(contexts) > 1000 {
		n.LearningState = "insufficient_evidence"
		return nil
	}
	nodeIDs := []string{}
	for _, source := range n.Content.Sources {
		nodeIDs = append(nodeIDs, source.NodeID)
	}
	n.LearningState, err = s.structureLearning.ConceptLearningStateTx(ctx, tx, nodeIDs, contexts)
	return err
}
func (s *Store) ReadConcept(ctx context.Context, id, revision string) (knowledge.StructureNode, error) {
	tx, _, err := s.beginMaintenanceProposalRead(ctx)
	if err != nil {
		return knowledge.StructureNode{}, err
	}
	defer tx.Rollback(context.Background())
	n, err := readStructureNode(ctx, tx, id, revision)
	if err != nil {
		return n, err
	}
	edges := []knowledge.ConceptRelation{}
	for _, e := range n.Content.Relations {
		_, x := readStructureNode(ctx, tx, e.TargetID, "")
		if knowledge.ErrorCode(x) == knowledge.CodeNotFound {
			continue
		}
		if x != nil {
			return n, x
		}
		edges = append(edges, e)
	}
	n.Content.Relations = edges
	if err = s.structureLearningState(ctx, tx, &n); err != nil {
		return n, err
	}
	return n, tx.Commit(ctx)
}

func (s *Store) structureImpact(ctx context.Context, tx pgx.Tx, nodes []knowledge.StructureNode) (knowledge.StructureImpact, error) {
	impact := knowledge.StructureImpact{GoalIDs: []string{}}
	ids := []string{}
	for _, n := range nodes {
		ids = append(ids, n.ConceptID)
		if n.GoalID != "" && !slices.Contains(impact.GoalIDs, n.GoalID) {
			impact.GoalIDs = append(impact.GoalIDs, n.GoalID)
		}
	}
	slices.Sort(impact.GoalIDs)
	contexts := []string{}
	rows, err := tx.Query(ctx, `SELECT DISTINCT k.id FROM knowledge_context_revisions k JOIN knowledge_policies p ON p.id=k.policy_id JOIN knowledge_concept_revisions r ON r.id=ANY(k.concept_revision_ids) WHERE p.space_id=$1 AND r.concept_id=ANY($2::uuid[]) ORDER BY k.id LIMIT 10001`, learningspace.Scope(ctx), ids)
	if err != nil {
		return impact, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return impact, err
		}
		contexts = append(contexts, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return impact, err
	}
	// 影响超界时拒绝计划，不能以不完整的影响批准破坏性维护。
	if len(contexts) > 10000 {
		return impact, structureError(knowledge.CodePayloadTooLarge)
	}
	impact.Contexts = int64(len(contexts))
	if s.structureLearning != nil {
		v, e := s.structureLearning.StructureLearningImpactTx(ctx, tx, contexts)
		if e != nil {
			return impact, e
		}
		impact.Activities, impact.Contents, impact.Evidence = v.Activities, v.Contents, v.Evidence
		impact.Fingerprint = v.Fingerprint
	}
	impact.Fingerprint = knowledge.StructureDigest(struct {
		Impact   knowledge.StructureImpact
		Contexts []string
	}{impact, contexts})
	return impact, nil
}
func (s *Store) structureWrite(ctx context.Context, actor, operation string) (pgx.Tx, int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	fail := func(e error) (pgx.Tx, int64, error) { tx.Rollback(context.Background()); return nil, 0, e }
	g, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return fail(err)
	}
	if _, err = privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning); err != nil {
		return fail(err)
	}
	if err = lockSpaceWrite(ctx, tx); err != nil {
		return fail(err)
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT revoked_at IS NULL FROM devices WHERE id=$1 FOR SHARE`, actor).Scan(&active)
	if err != nil || !active {
		if err == nil || errors.Is(err, pgx.ErrNoRows) {
			err = scopeMissing()
		}
		return fail(err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "knowledge-structure-operation:"+operation); err != nil {
		return fail(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_structure_heads(space_id,version) VALUES($1,0) ON CONFLICT DO NOTHING`, learningspace.Scope(ctx)); err != nil {
		return fail(err)
	}
	if _, err = tx.Exec(ctx, `SELECT version FROM knowledge_structure_heads WHERE space_id=$1 FOR UPDATE`, learningspace.Scope(ctx)); err != nil {
		return fail(err)
	}
	return tx, g, nil
}
func readStructureProposal(ctx context.Context, tx pgx.Tx, id string) (knowledge.StructureProposal, error) {
	var p knowledge.StructureProposal
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT record FROM knowledge_structure_proposals WHERE id=$1 AND space_id=$2`, id, learningspace.Scope(ctx)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, scopeMissing()
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(raw, &p)
	return p, err
}
func structureReplay(ctx context.Context, tx pgx.Tx, operation, actor, hash string) (knowledge.StructureProposal, bool, error) {
	var p knowledge.StructureProposal
	var stored, id, space, device string
	err := tx.QueryRow(ctx, `SELECT request_hash,proposal_id,space_id,actor_device_id FROM knowledge_structure_operations WHERE operation_id=$1`, operation).Scan(&stored, &id, &space, &device)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	if stored != hash || space != learningspace.Scope(ctx) || device != actor {
		return p, false, structureError(knowledge.CodeIdempotencyConflict)
	}
	p, err = readStructureProposal(ctx, tx, id)
	p.Replayed = true
	return p, true, err
}
func saveStructureOperation(ctx context.Context, tx pgx.Tx, operation, actor, hash, id string) error {
	_, err := tx.Exec(ctx, `INSERT INTO knowledge_structure_operations(operation_id,space_id,actor_device_id,request_hash,proposal_id) VALUES($1,$2,$3,$4,$5)`, operation, learningspace.Scope(ctx), actor, hash, id)
	return err
}
func saveStructureProposal(ctx context.Context, tx pgx.Tx, p knowledge.StructureProposal) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE knowledge_structure_proposals SET record=$2 WHERE id=$1 AND space_id=$3`, p.ID, raw, learningspace.Scope(ctx))
	return err
}
func (s *Store) validateStructureSources(ctx context.Context, tx pgx.Tx, n knowledge.StructureNode) error {
	for _, source := range n.Content.Sources {
		var text string
		var start, end int
		err := tx.QueryRow(ctx, `SELECT p.canonical_markdown,n.section_start,n.section_end FROM knowledge_collection_links l
 JOIN knowledge_revisions r ON r.collection_id=l.collection_id JOIN knowledge_snapshot_documents d ON d.knowledge_revision_id=r.id
 JOIN knowledge_node_revisions n ON n.document_revision_id=d.document_revision_id JOIN knowledge_document_payloads p ON p.document_revision_id=d.document_revision_id
 WHERE l.space_id=$1 AND l.collection_id=$2 AND r.id=$3 AND d.document_id=$4 AND n.node_id=$5 AND r.redacted_at IS NULL`, learningspace.Scope(ctx), source.CollectionID, source.RevisionID, source.DocumentID, source.NodeID).Scan(&text, &start, &end)
		if errors.Is(err, pgx.ErrNoRows) {
			return scopeMissing()
		}
		if err != nil {
			return err
		}
		if start < 0 || end > len(text) || !strings.Contains(text[start:end], source.Quote) {
			return structureError(knowledge.CodeInvalidRequest)
		}
	}
	if n.Content.SourceStatus == "included" && len(n.Support) == 0 && len(n.Content.Sources) == 0 {
		return structureError(knowledge.CodeInvalidRequest)
	}
	return nil
}
func (s *Store) CreateStructureProposal(ctx context.Context, c knowledge.StructureCommand) (knowledge.StructureProposal, error) {
	var p knowledge.StructureProposal
	tx, g, err := s.structureWrite(ctx, c.ActorDeviceID, c.OperationID)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(context.Background())
	hash := knowledge.StructureDigest(c)
	if old, ok, e := structureReplay(ctx, tx, c.OperationID, c.ActorDeviceID, hash); e != nil || ok {
		if e == nil {
			e = s.refreshStructureProposal(ctx, tx, &old)
		}
		return old, e
	}
	version, err := structureVersion(ctx, tx)
	if err != nil {
		return p, err
	}
	if version != c.BaseVersion || g != c.Generation {
		return p, structureError(knowledge.CodeRevisionConflict)
	}
	p = knowledge.StructureProposal{ID: uuid.NewString(), OperationID: c.OperationID, Status: "open", Kind: c.Kind, Reason: c.Reason, BaseVersion: version, Generation: g, Compensates: c.Compensates, Before: []knowledge.StructureNode{}, After: []knowledge.StructureNode{}, CreatedAt: time.Now().UTC()}
	if c.Kind == "compensate" {
		old, e := readStructureProposal(ctx, tx, c.Compensates)
		if e != nil {
			return p, e
		}
		if old.Status != "applied" {
			return p, structureError(knowledge.CodeProposalClosed)
		}
		for _, after := range old.After {
			var before *knowledge.StructureNode
			for i := range old.Before {
				if old.Before[i].ConceptID == after.ConceptID {
					before = &old.Before[i]
					break
				}
			}
			if before != nil {
				c.Edits = append(c.Edits, knowledge.StructureEdit{ConceptID: before.ConceptID, GoalID: before.GoalID, Name: before.Name, Content: before.Content})
			} else {
				// 新建身份的补偿保留墓碑版本和旧引用，不删除学习事实。
				after.Content.SourceStatus = "candidate"
				after.Content.Description = "已补偿撤回：" + c.Reason
				after.Content.Relations = []knowledge.ConceptRelation{}
				after.Content.ReplacedBy = []string{}
				c.Edits = append(c.Edits, knowledge.StructureEdit{ConceptID: after.ConceptID, GoalID: after.GoalID, Name: after.Name, Content: after.Content})
			}
		}
	}
	for _, e := range c.Edits {
		old, x := readStructureNode(ctx, tx, e.ConceptID, "")
		if x != nil && knowledge.ErrorCode(x) != knowledge.CodeNotFound {
			return p, x
		}
		n := knowledge.StructureNode{ConceptRevision: knowledge.ConceptRevision{ConceptID: e.ConceptID, RevisionID: uuid.NewString(), SemanticKey: "reviewed:" + e.ConceptID, Name: e.Name}, GoalID: e.GoalID, Content: e.Content}
		if x == nil {
			if old.GoalID != e.GoalID {
				return p, structureError(knowledge.CodeInvalidRequest)
			}
			p.Before = append(p.Before, old)
			n.SemanticKey = old.SemanticKey
			n.Support = old.Support
		} else {
			var exists bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_concepts WHERE id=$1)`, e.ConceptID).Scan(&exists); err != nil {
				return p, err
			}
			if exists {
				return p, scopeMissing()
			}
		}
		if e.GoalID != "" {
			var owned bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_goal_revisions WHERE goal_id=$1 AND space_id=$2)`, e.GoalID, learningspace.Scope(ctx)).Scan(&owned)
			if err != nil {
				return p, err
			}
			if !owned {
				return p, scopeMissing()
			}
		}
		normalizeStructureNode(&n)
		if err = s.validateStructureSources(ctx, tx, n); err != nil {
			return p, err
		}
		p.After = append(p.After, n)
	}
	if err = validateStructureMapping(p); err != nil {
		return p, err
	}
	if err = validateStructureTargets(ctx, tx, p.After); err != nil {
		return p, err
	}
	p.Impact, err = s.structureImpact(ctx, tx, append(append([]knowledge.StructureNode{}, p.Before...), p.After...))
	if err != nil {
		return p, err
	}
	p.CurrentImpact = p.Impact
	p.Hash = knowledge.StructureDigest(p)
	raw, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_structure_proposals(id,space_id,actor_device_id,record) VALUES($1,$2,$3,$4)`, p.ID, learningspace.Scope(ctx), c.ActorDeviceID, raw); err != nil {
		return p, err
	}
	if err = saveStructureOperation(ctx, tx, c.OperationID, c.ActorDeviceID, hash, p.ID); err != nil {
		return p, err
	}
	return p, tx.Commit(ctx)
}
func validateStructureMapping(p knowledge.StructureProposal) error {
	old := map[string]bool{}
	for _, n := range p.Before {
		old[n.ConceptID] = true
	}
	retired, created := 0, 0
	targets := map[string]bool{}
	for _, n := range p.After {
		if p.Kind == "edit" {
			var previous []string
			for _, before := range p.Before {
				if before.ConceptID == n.ConceptID {
					previous = before.Content.ReplacedBy
				}
			}
			if !slices.Equal(previous, n.Content.ReplacedBy) {
				return structureError(knowledge.CodeInvalidRequest)
			}
		}
		if !old[n.ConceptID] {
			created++
		}
		if len(n.Content.ReplacedBy) > 0 {
			retired++
			for _, id := range n.Content.ReplacedBy {
				targets[id] = true
			}
		}
	}
	if p.Kind == "merge" && (retired < 2 || created != 1 || len(targets) != 1) || p.Kind == "split" && (retired != 1 || created < 2 || len(targets) != created) {
		return structureError(knowledge.CodeInvalidRequest)
	}
	if p.Kind == "merge" || p.Kind == "split" {
		for id := range targets {
			found := false
			for _, n := range p.After {
				if n.ConceptID == id && !old[id] {
					found = true
				}
			}
			if !found {
				return structureError(knowledge.CodeInvalidRequest)
			}
		}
	}
	return nil
}
func validateStructureTargets(ctx context.Context, tx pgx.Tx, nodes []knowledge.StructureNode) error {
	ids := map[string]bool{}
	for _, n := range nodes {
		ids[n.ConceptID] = true
	}
	for _, n := range nodes {
		targets := append([]string{}, n.Content.ReplacedBy...)
		for _, e := range n.Content.Relations {
			targets = append(targets, e.TargetID)
		}
		for _, id := range targets {
			if ids[id] {
				continue
			}
			if _, err := readStructureNode(ctx, tx, id, ""); err != nil {
				return err
			}
		}
	}
	// 关系只存一跳；前置环可展示，但绝不驱动递归展开或直接生成教学路线。
	return nil
}
func (s *Store) ReadStructureProposal(ctx context.Context, id string) (knowledge.StructureProposal, error) {
	tx, _, err := s.beginMaintenanceProposalRead(ctx)
	if err != nil {
		return knowledge.StructureProposal{}, err
	}
	defer tx.Rollback(context.Background())
	p, err := readStructureProposal(ctx, tx, id)
	if err != nil {
		return p, err
	}
	if err = s.refreshStructureProposal(ctx, tx, &p); err != nil {
		return p, err
	}
	return p, tx.Commit(ctx)
}
func (s *Store) refreshStructureProposal(ctx context.Context, tx pgx.Tx, p *knowledge.StructureProposal) error {
	// 提案可能包含已解除引用的原文；重新验证后才能返回冻结副本。
	for _, n := range append(append([]knowledge.StructureNode{}, p.Before...), p.After...) {
		if err := s.validateStructureSources(ctx, tx, n); err != nil {
			return err
		}
		if len(n.Support) > 0 {
			if _, err := readStructureNode(ctx, tx, n.ConceptID, n.RevisionID); err != nil && slices.ContainsFunc(p.Before, func(v knowledge.StructureNode) bool { return v.ConceptID == n.ConceptID }) {
				if _, err = readStructureNode(ctx, tx, n.ConceptID, ""); err != nil {
					return err
				}
			}
		}
	}
	var err error
	p.CurrentImpact, err = s.structureImpact(ctx, tx, append(append([]knowledge.StructureNode{}, p.Before...), p.After...))
	return err
}
func (s *Store) ListStructureProposals(ctx context.Context, cursor string, limit int) (knowledge.StructureProposalPage, error) {
	p := knowledge.StructureProposalPage{Items: []knowledge.StructureProposal{}}
	tx, _, err := s.beginMaintenanceProposalRead(ctx)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, `SELECT record FROM knowledge_structure_proposals WHERE `+structureProposalVisible+` AND ($2='' OR id::text>$2) ORDER BY id LIMIT $3`, learningspace.Scope(ctx), cursor, limit+1)
	if err != nil {
		return p, err
	}
	items := []knowledge.StructureProposal{}
	for rows.Next() {
		var raw []byte
		var item knowledge.StructureProposal
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return p, err
		}
		if err = json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			return p, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, err
	}
	if len(items) > limit {
		items = items[:limit]
		p.NextCursor = items[len(items)-1].ID
	}
	for _, item := range items {
		if err = s.refreshStructureProposal(ctx, tx, &item); knowledge.ErrorCode(err) == knowledge.CodeNotFound {
			continue
		}
		if err != nil {
			return p, err
		}
		p.Items = append(p.Items, item)
	}
	return p, tx.Commit(ctx)
}
func (s *Store) DecideStructureProposal(ctx context.Context, id string, c knowledge.StructureDecision) (knowledge.StructureProposal, error) {
	var p knowledge.StructureProposal
	tx, g, err := s.structureWrite(ctx, c.ActorDeviceID, c.OperationID)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(context.Background())
	hash := knowledge.StructureDigest(struct {
		ID      string
		Command knowledge.StructureDecision
	}{id, c})
	if old, ok, e := structureReplay(ctx, tx, c.OperationID, c.ActorDeviceID, hash); e != nil || ok {
		if e == nil {
			e = s.refreshStructureProposal(ctx, tx, &old)
		}
		return old, e
	}
	p, err = readStructureProposal(ctx, tx, id)
	if err != nil {
		return p, err
	}
	if p.Hash != c.Hash || p.Generation != g {
		return p, structureError(knowledge.CodeRevisionConflict)
	}
	if p.Status != "open" {
		return p, structureError(knowledge.CodeProposalClosed)
	}
	if err = s.refreshStructureProposal(ctx, tx, &p); err != nil {
		return p, err
	}
	version, err := structureVersion(ctx, tx)
	if err != nil {
		return p, err
	}
	if c.Decision == "reject" {
		p.Status = "rejected"
	} else if version != p.BaseVersion || p.CurrentImpact.Fingerprint != p.Impact.Fingerprint {
		p.Status = "stale"
	} else {
		if err = validateStructureTargets(ctx, tx, p.After); err != nil {
			return p, err
		}
		for _, n := range p.After {
			var goal *string
			if n.GoalID != "" {
				goal = &n.GoalID
			}
			if _, err = tx.Exec(ctx, `INSERT INTO knowledge_concepts(id,space_id,goal_id,semantic_key) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO NOTHING`, n.ConceptID, learningspace.Scope(ctx), goal, n.SemanticKey); err != nil {
				return p, err
			}
			raw, _ := json.Marshal(n.Content)
			support, _ := json.Marshal(n.Support)
			if _, err = tx.Exec(ctx, `INSERT INTO knowledge_concept_revisions(id,concept_id,name,support,structure) VALUES($1,$2,$3,$4,$5)`, n.RevisionID, n.ConceptID, n.Name, support, raw); err != nil {
				return p, err
			}
		}
		p.Status = "applied"
		p.AppliedVersion, err = structureVersion(ctx, tx)
		if err != nil {
			return p, err
		}
	}
	p.DecisionReason = c.Reason
	if err = saveStructureProposal(ctx, tx, p); err != nil {
		return p, err
	}
	if err = saveStructureOperation(ctx, tx, c.OperationID, c.ActorDeviceID, hash, p.ID); err != nil {
		return p, err
	}
	return p, tx.Commit(ctx)
}

var _ knowledge.StructureStore = (*Store)(nil)
