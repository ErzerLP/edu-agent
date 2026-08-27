package postgresstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) LockReadWith(ctx context.Context, tx pgx.Tx) (int64, error) {
	return privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
}

func (s *Store) RevisionHeadLockedWith(ctx context.Context, tx pgx.Tx, revisionID string) (bool, string, error) {
	var redactedAt *time.Time
	var headRevisionID *string
	if err := tx.QueryRow(ctx, `
		SELECT revision.redacted_at,catalog.head_revision_id::text
		FROM knowledge_revisions revision
		CROSS JOIN knowledge_catalog catalog
		WHERE revision.id=$1 AND catalog.singleton_id=1`, revisionID).Scan(&redactedAt, &headRevisionID); err != nil {
		return false, "", fmt.Errorf("read knowledge revision status and head: %w", err)
	}
	if headRevisionID == nil {
		return redactedAt != nil, "", nil
	}
	return redactedAt != nil, *headRevisionID, nil
}

func (s *Store) beginPrivacyRead(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin knowledge privacy read: %w", err)
	}
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, err
	}
	return tx, nil
}

func (s *Store) Head(ctx context.Context) (*knowledge.KnowledgeRevision, error) {
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var id *string
	if err := tx.QueryRow(ctx, `SELECT head_revision_id FROM knowledge_catalog WHERE singleton_id=1`).Scan(&id); err != nil {
		return nil, fmt.Errorf("read knowledge catalog: %w", err)
	}
	if id == nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit empty knowledge head read: %w", err)
		}
		return nil, nil
	}
	revision, err := loadRevision(ctx, tx, *id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit knowledge head read: %w", err)
	}
	return &revision, nil
}

func (s *Store) Revision(ctx context.Context, id string) (knowledge.KnowledgeRevision, error) {
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return knowledge.KnowledgeRevision{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	revision, err := loadRevision(ctx, tx, id)
	if err != nil {
		return knowledge.KnowledgeRevision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return knowledge.KnowledgeRevision{}, fmt.Errorf("commit knowledge revision read: %w", err)
	}
	return revision, nil
}

func (s *Store) DocumentIdentityExists(ctx context.Context, id string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_documents WHERE id=$1)`, id).Scan(&exists); err != nil {
		return false, fmt.Errorf("check document identity: %w", err)
	}
	return exists, nil
}

func (s *Store) NodeIdentityOwner(ctx context.Context, id string) (string, bool, error) {
	var owner string
	err := s.pool.QueryRow(ctx, `SELECT document_id FROM knowledge_nodes WHERE id=$1`, id).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("check node identity: %w", err)
	}
	return owner, true, nil
}

func (s *Store) ReadyNodeArtifacts(ctx context.Context, knowledgeRevisionID string) ([]knowledge.NodeArtifact, error) {
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (a.node_revision_id)
		       a.id,a.node_revision_id,a.kind,a.producer_version,a.prompt_version,a.model_version,
		       a.input_hash,a.content,a.status,a.created_at
		FROM knowledge_node_artifacts a
		JOIN knowledge_node_revisions nr ON nr.id=a.node_revision_id
		JOIN knowledge_snapshot_documents sd ON sd.document_revision_id=nr.document_revision_id
		JOIN knowledge_revisions kr ON kr.id=sd.knowledge_revision_id AND kr.redacted_at IS NULL
		WHERE sd.knowledge_revision_id=$1 AND a.kind='summary' AND a.status='ready'
		ORDER BY a.node_revision_id,a.created_at DESC,a.id DESC`, knowledgeRevisionID)
	if err != nil {
		return nil, fmt.Errorf("read ready node artifacts: %w", err)
	}
	var artifacts []knowledge.NodeArtifact
	for rows.Next() {
		var artifact knowledge.NodeArtifact
		var inputHash []byte
		if err := rows.Scan(
			&artifact.ID, &artifact.NodeRevisionID, &artifact.Kind, &artifact.ProducerVersion,
			&artifact.PromptVersion, &artifact.ModelVersion, &inputHash, &artifact.Content,
			&artifact.Status, &artifact.CreatedAt,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan ready node artifact: %w", err)
		}
		artifact.InputHash = hex.EncodeToString(inputHash)
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate ready node artifacts: %w", err)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit ready node artifacts read: %w", err)
	}
	return artifacts, nil
}

func (s *Store) LookupImportOperation(ctx context.Context, operationID string) (knowledge.ImportOperationRecord, bool, error) {
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return knowledge.ImportOperationRecord{}, false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var requestHash []byte
	var revisionID string
	var unchanged bool
	err = tx.QueryRow(ctx, `
		SELECT request_hash,result_revision_id,unchanged
		FROM knowledge_import_operations WHERE operation_id=$1`, operationID).
		Scan(&requestHash, &revisionID, &unchanged)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return knowledge.ImportOperationRecord{}, false, fmt.Errorf("commit missing import operation read: %w", err)
		}
		return knowledge.ImportOperationRecord{}, false, nil
	}
	if err != nil {
		return knowledge.ImportOperationRecord{}, false, fmt.Errorf("read import operation: %w", err)
	}
	revision, err := loadRevision(ctx, tx, revisionID)
	if err != nil {
		return knowledge.ImportOperationRecord{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return knowledge.ImportOperationRecord{}, false, fmt.Errorf("commit import operation read: %w", err)
	}
	return knowledge.ImportOperationRecord{
		RequestHash: hex.EncodeToString(requestHash), Result: knowledge.ImportResult{Revision: revision, Unchanged: unchanged},
	}, true, nil
}

func (s *Store) CommitImport(ctx context.Context, prepared knowledge.PreparedCommit) (knowledge.ImportResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return knowledge.ImportResult{}, fmt.Errorf("begin knowledge import: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return knowledge.ImportResult{}, err
	}

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, prepared.OperationID); err != nil {
		return knowledge.ImportResult{}, fmt.Errorf("lock knowledge import operation key: %w", err)
	}

	var storedHash []byte
	var storedRevisionID string
	var storedUnchanged bool
	err = tx.QueryRow(ctx, `
		SELECT request_hash,result_revision_id,unchanged
		FROM knowledge_import_operations WHERE operation_id=$1 FOR UPDATE`, prepared.OperationID).
		Scan(&storedHash, &storedRevisionID, &storedUnchanged)
	if err == nil {
		if hex.EncodeToString(storedHash) != prepared.RequestHash {
			return knowledge.ImportResult{}, &knowledge.Error{Code: knowledge.CodeIdempotencyConflict}
		}
		revision, err := loadRevision(ctx, tx, storedRevisionID)
		if err != nil {
			return knowledge.ImportResult{}, err
		}
		return knowledge.ImportResult{Revision: revision, Unchanged: storedUnchanged, Replayed: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return knowledge.ImportResult{}, fmt.Errorf("lock import operation: %w", err)
	}

	var currentHead *string
	if err := tx.QueryRow(ctx, `SELECT head_revision_id FROM knowledge_catalog WHERE singleton_id=1 FOR UPDATE`).Scan(&currentHead); err != nil {
		return knowledge.ImportResult{}, fmt.Errorf("lock knowledge catalog: %w", err)
	}
	if !sameOptional(currentHead, prepared.ExpectedParentRevisionID) {
		return knowledge.ImportResult{}, &knowledge.Error{Code: knowledge.CodeRevisionConflict, CurrentRevisionID: currentHead, CurrentRevisionKnown: true}
	}
	if !prepared.Unchanged {
		if err := insertRevision(ctx, tx, prepared.Revision, prepared.Lineages); err != nil {
			return knowledge.ImportResult{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE knowledge_catalog SET head_revision_id=$1,updated_at=$2 WHERE singleton_id=1`, prepared.Revision.ID, prepared.Revision.CreatedAt); err != nil {
			return knowledge.ImportResult{}, fmt.Errorf("advance knowledge head: %w", err)
		}
	}
	requestHash, err := hex.DecodeString(prepared.RequestHash)
	if err != nil || len(requestHash) != 32 {
		return knowledge.ImportResult{}, fmt.Errorf("invalid prepared request hash")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge_import_operations(operation_id,request_hash,result_revision_id,unchanged,completed_at)
		VALUES($1,$2,$3,$4,$5)`, prepared.OperationID, requestHash, prepared.Revision.ID, prepared.Unchanged, prepared.Revision.CreatedAt); err != nil {
		return knowledge.ImportResult{}, fmt.Errorf("record knowledge import operation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return knowledge.ImportResult{}, fmt.Errorf("commit knowledge import: %w", err)
	}
	return knowledge.ImportResult{Revision: prepared.Revision, Unchanged: prepared.Unchanged}, nil
}

func insertRevision(ctx context.Context, tx pgx.Tx, revision knowledge.KnowledgeRevision, lineages []knowledge.Lineage) error {
	manifestHash, err := decodeHash(revision.ManifestHash)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge_revisions(
			id,revision_no,parent_revision_id,manifest_hash,source,created_by_device_id,created_at,
			canonicalizer_version,parser_version,indexer_version,identity_policy_version)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		revision.ID, revision.RevisionNo, revision.ParentRevisionID, manifestHash, revision.Source,
		revision.CreatedByDeviceID, revision.CreatedAt, revision.CanonicalizerVersion, revision.ParserVersion,
		revision.IndexerVersion, revision.IdentityPolicyVersion); err != nil {
		return fmt.Errorf("insert knowledge revision: %w", err)
	}
	for _, snapshot := range revision.Documents {
		document := snapshot.Revision
		if _, err := tx.Exec(ctx, `
			INSERT INTO knowledge_documents(id,created_at) VALUES($1,$2)
			ON CONFLICT(id) DO NOTHING`, document.DocumentID, revision.CreatedAt); err != nil {
			return fmt.Errorf("insert document identity: %w", err)
		}
		canonicalHash, err := decodeHash(document.CanonicalHash)
		if err != nil {
			return err
		}
		semanticHash, err := decodeHash(document.SemanticHash)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO knowledge_document_revisions(
				id,document_id,canonical_hash,semantic_hash,root_node_id,parser_version,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(id) DO NOTHING`,
			document.ID, document.DocumentID, canonicalHash, semanticHash, document.RootNodeID, knowledge.ParserVersion, revision.CreatedAt); err != nil {
			return fmt.Errorf("insert document revision: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO knowledge_document_payloads(document_revision_id,canonical_markdown)
			VALUES($1,$2) ON CONFLICT(document_revision_id) DO NOTHING`, document.ID, document.CanonicalMarkdown); err != nil {
			return fmt.Errorf("insert document payload: %w", err)
		}
		for _, node := range document.Nodes {
			if _, err := tx.Exec(ctx, `
				INSERT INTO knowledge_nodes(id,document_id,created_at) VALUES($1,$2,$3)
				ON CONFLICT(id) DO NOTHING`, node.NodeID, document.DocumentID, revision.CreatedAt); err != nil {
				return fmt.Errorf("insert node identity: %w", err)
			}
			bodyHash, err := decodeHash(node.SemanticLocalBodyHash)
			if err != nil {
				return err
			}
			ancestors, _ := json.Marshal(node.AncestorTitles)
			if _, err := tx.Exec(ctx, `
				INSERT INTO knowledge_node_revisions(
					id,node_id,document_id,document_revision_id,parent_node_revision_id,sibling_index,heading_level,title,ancestor_titles,
					heading_start,heading_end,heading_start_line,heading_end_line,
					local_body_start,local_body_end,local_body_start_line,local_body_end_line,
					section_start,section_end,section_start_line,section_end_line,
					semantic_local_body_hash,indexer_version)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
				ON CONFLICT(id) DO NOTHING`,
				node.ID, node.NodeID, document.DocumentID, node.DocumentRevisionID, node.ParentNodeRevisionID, node.SiblingIndex,
				node.HeadingLevel, node.Title, ancestors,
				node.HeadingRange.Start, node.HeadingRange.End, node.HeadingRange.StartLine, node.HeadingRange.EndLine,
				node.LocalBodyRange.Start, node.LocalBodyRange.End, node.LocalBodyRange.StartLine, node.LocalBodyRange.EndLine,
				node.SectionRange.Start, node.SectionRange.End, node.SectionRange.StartLine, node.SectionRange.EndLine,
				bodyHash, knowledge.IndexerVersion); err != nil {
				return fmt.Errorf("insert node revision: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO knowledge_snapshot_documents(knowledge_revision_id,canonical_path,folded_path,document_id,document_revision_id)
			VALUES($1,$2,$3,$4,$5)`, revision.ID, snapshot.Path, knowledge.FoldPath(snapshot.Path), document.DocumentID, document.ID); err != nil {
			return fmt.Errorf("insert snapshot document: %w", err)
		}
	}
	for _, lineage := range lineages {
		if _, err := tx.Exec(ctx, `
			INSERT INTO knowledge_lineages(id,knowledge_revision_id,action,actor_device_id,reason,policy_version,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, lineage.ID, revision.ID, lineage.Action, lineage.ActorDeviceID, lineage.Reason, lineage.PolicyVersion, lineage.CreatedAt); err != nil {
			return fmt.Errorf("insert node lineage: %w", err)
		}
		ordinals := map[string]int{}
		for _, member := range lineage.Members {
			ordinal := ordinals[member.Role]
			ordinals[member.Role]++
			if _, err := tx.Exec(ctx, `
				INSERT INTO knowledge_lineage_members(lineage_id,role,node_revision_id,ordinal)
				VALUES($1,$2,$3,$4)`, lineage.ID, member.Role, member.NodeRevisionID, ordinal); err != nil {
				return fmt.Errorf("insert node lineage member: %w", err)
			}
		}
	}
	return nil
}

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func loadRevision(ctx context.Context, db queryer, id string) (knowledge.KnowledgeRevision, error) {
	var revision knowledge.KnowledgeRevision
	var manifestHash []byte
	err := db.QueryRow(ctx, `
		SELECT id,revision_no,parent_revision_id,manifest_hash,source,created_by_device_id,created_at,
		       canonicalizer_version,parser_version,indexer_version,identity_policy_version,
		       redacted_at IS NOT NULL
		FROM knowledge_revisions WHERE id=$1`, id).Scan(
		&revision.ID, &revision.RevisionNo, &revision.ParentRevisionID, &manifestHash, &revision.Source,
		&revision.CreatedByDeviceID, &revision.CreatedAt, &revision.CanonicalizerVersion, &revision.ParserVersion,
		&revision.IndexerVersion, &revision.IdentityPolicyVersion, &revision.Redacted,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledge.KnowledgeRevision{}, &knowledge.Error{Code: knowledge.CodeNotFound}
	}
	if err != nil {
		return knowledge.KnowledgeRevision{}, fmt.Errorf("read knowledge revision: %w", err)
	}
	revision.CreatedAt = revision.CreatedAt.UTC()
	revision.ManifestHash = hex.EncodeToString(manifestHash)
	if revision.Redacted {
		return revision, nil
	}
	rows, err := db.Query(ctx, `
		SELECT sd.canonical_path,dr.id,dr.document_id,dr.root_node_id,dr.canonical_hash,dr.semantic_hash,p.canonical_markdown
		FROM knowledge_snapshot_documents sd
		JOIN knowledge_document_revisions dr ON dr.id=sd.document_revision_id
		JOIN knowledge_document_payloads p ON p.document_revision_id=dr.id
		WHERE sd.knowledge_revision_id=$1 ORDER BY sd.canonical_path`, id)
	if err != nil {
		return knowledge.KnowledgeRevision{}, fmt.Errorf("read snapshot documents: %w", err)
	}
	var snapshots []knowledge.SnapshotDocument
	for rows.Next() {
		var snapshot knowledge.SnapshotDocument
		var canonicalHash, semanticHash []byte
		if err := rows.Scan(&snapshot.Path, &snapshot.Revision.ID, &snapshot.Revision.DocumentID, &snapshot.Revision.RootNodeID, &canonicalHash, &semanticHash, &snapshot.Revision.CanonicalMarkdown); err != nil {
			rows.Close()
			return knowledge.KnowledgeRevision{}, fmt.Errorf("scan snapshot document: %w", err)
		}
		snapshot.Revision.CanonicalHash = hex.EncodeToString(canonicalHash)
		snapshot.Revision.SemanticHash = hex.EncodeToString(semanticHash)
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return knowledge.KnowledgeRevision{}, fmt.Errorf("iterate snapshot documents: %w", err)
	}
	rows.Close()
	for _, snapshot := range snapshots {
		nodes, err := loadNodes(ctx, db, snapshot.Revision.ID)
		if err != nil {
			return knowledge.KnowledgeRevision{}, err
		}
		snapshot.Revision.Nodes = nodes
		revision.Documents = append(revision.Documents, snapshot)
	}
	lineages, err := loadLineages(ctx, db, revision.ID)
	if err != nil {
		return knowledge.KnowledgeRevision{}, err
	}
	revision.Lineages = lineages
	return revision, nil
}

func loadNodes(ctx context.Context, db queryer, documentRevisionID string) ([]knowledge.NodeRevision, error) {
	rows, err := db.Query(ctx, `
		SELECT id,node_id,document_revision_id,parent_node_revision_id,sibling_index,heading_level,title,ancestor_titles,
		       heading_start,heading_end,heading_start_line,heading_end_line,
		       local_body_start,local_body_end,local_body_start_line,local_body_end_line,
		       section_start,section_end,section_start_line,section_end_line,semantic_local_body_hash
		FROM knowledge_node_revisions WHERE document_revision_id=$1
		ORDER BY heading_level,section_start,id`, documentRevisionID)
	if err != nil {
		return nil, fmt.Errorf("read node revisions: %w", err)
	}
	defer rows.Close()
	var nodes []knowledge.NodeRevision
	for rows.Next() {
		var node knowledge.NodeRevision
		var ancestors, bodyHash []byte
		if err := rows.Scan(
			&node.ID, &node.NodeID, &node.DocumentRevisionID, &node.ParentNodeRevisionID, &node.SiblingIndex,
			&node.HeadingLevel, &node.Title, &ancestors,
			&node.HeadingRange.Start, &node.HeadingRange.End, &node.HeadingRange.StartLine, &node.HeadingRange.EndLine,
			&node.LocalBodyRange.Start, &node.LocalBodyRange.End, &node.LocalBodyRange.StartLine, &node.LocalBodyRange.EndLine,
			&node.SectionRange.Start, &node.SectionRange.End, &node.SectionRange.StartLine, &node.SectionRange.EndLine,
			&bodyHash,
		); err != nil {
			return nil, fmt.Errorf("scan node revision: %w", err)
		}
		if err := json.Unmarshal(ancestors, &node.AncestorTitles); err != nil {
			return nil, fmt.Errorf("decode node ancestors: %w", err)
		}
		node.SemanticLocalBodyHash = hex.EncodeToString(bodyHash)
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate node revisions: %w", err)
	}
	byID := make(map[string]int, len(nodes))
	for i := range nodes {
		byID[nodes[i].ID] = i
	}
	for _, node := range nodes {
		if node.ParentNodeRevisionID != nil {
			parent := byID[*node.ParentNodeRevisionID]
			nodes[parent].Children = append(nodes[parent].Children, node.ID)
		}
	}
	for i := range nodes {
		sort.Slice(nodes[i].Children, func(left, right int) bool {
			return nodes[byID[nodes[i].Children[left]]].SiblingIndex < nodes[byID[nodes[i].Children[right]]].SiblingIndex
		})
	}
	// Restore deterministic preorder, with the synthetic root first.
	var ordered []knowledge.NodeRevision
	var visit func(string)
	visit = func(id string) {
		node := nodes[byID[id]]
		ordered = append(ordered, node)
		for _, child := range node.Children {
			visit(child)
		}
	}
	for _, node := range nodes {
		if node.ParentNodeRevisionID == nil {
			visit(node.ID)
			break
		}
	}
	return ordered, nil
}

func loadLineages(ctx context.Context, db queryer, knowledgeRevisionID string) ([]knowledge.Lineage, error) {
	rows, err := db.Query(ctx, `
		SELECT id,knowledge_revision_id,action,actor_device_id,reason,policy_version,created_at
		FROM knowledge_lineages
		WHERE knowledge_revision_id=$1
		ORDER BY created_at,id`, knowledgeRevisionID)
	if err != nil {
		return nil, fmt.Errorf("read node lineages: %w", err)
	}
	var lineages []knowledge.Lineage
	for rows.Next() {
		var lineage knowledge.Lineage
		if err := rows.Scan(&lineage.ID, &lineage.KnowledgeRevisionID, &lineage.Action, &lineage.ActorDeviceID, &lineage.Reason, &lineage.PolicyVersion, &lineage.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan node lineage: %w", err)
		}
		lineage.CreatedAt = lineage.CreatedAt.UTC()
		lineages = append(lineages, lineage)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate node lineages: %w", err)
	}
	rows.Close()
	for i := range lineages {
		memberRows, err := db.Query(ctx, `
			SELECT role,node_revision_id FROM knowledge_lineage_members
			WHERE lineage_id=$1
			ORDER BY CASE role WHEN 'source' THEN 0 ELSE 1 END,ordinal`, lineages[i].ID)
		if err != nil {
			return nil, fmt.Errorf("read node lineage members: %w", err)
		}
		for memberRows.Next() {
			var member knowledge.LineageMember
			if err := memberRows.Scan(&member.Role, &member.NodeRevisionID); err != nil {
				memberRows.Close()
				return nil, fmt.Errorf("scan node lineage member: %w", err)
			}
			lineages[i].Members = append(lineages[i].Members, member)
		}
		if err := memberRows.Err(); err != nil {
			memberRows.Close()
			return nil, fmt.Errorf("iterate node lineage members: %w", err)
		}
		memberRows.Close()
	}
	return lineages, nil
}

func decodeHash(value string) ([]byte, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("invalid prepared SHA-256 hash")
	}
	return decoded, nil
}

func sameOptional(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
