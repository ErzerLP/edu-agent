package postgresstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	notesyncintegration "github.com/edu-agent/edu-agent/server/internal/integrations/notesync"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

func scopeMissing() error { return &knowledge.Error{Code: knowledge.CodeNotFound} }

func checkNotesyncReference(ctx context.Context, tx pgx.Tx, write bool) error {
	if space.Scope(ctx) != space.DefaultID || knowledge.CollectionID(ctx) != knowledge.DefaultCollectionID {
		return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewUnavailable}
	}
	var err error
	if write {
		err = lockCollectionWrite(ctx, tx)
	} else {
		err = checkCollection(ctx, tx, knowledge.DefaultCollectionID)
	}
	if err != nil {
		return &notesyncintegration.ReviewError{Code: notesyncintegration.CodeReviewUnavailable, Cause: err}
	}
	return nil
}

func lockSpaceWrite(ctx context.Context, tx pgx.Tx) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM learning_spaces WHERE id=$1 FOR SHARE`, space.Scope(ctx)).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &space.Error{Code: "learning_space_not_found"}
		}
		return err
	}
	if status != "active" {
		return &space.Error{Code: "learning_space_archived"}
	}
	_, err := tx.Exec(ctx, `SELECT set_config('edu_agent.knowledge_space',$1,true)`, space.Scope(ctx))
	return err
}

func lockCollectionWrite(ctx context.Context, tx pgx.Tx) error {
	if err := lockSpaceWrite(ctx, tx); err != nil {
		return err
	}
	if knowledge.CollectionID(ctx) == knowledge.DefaultCollectionID {
		if _, err := tx.Exec(ctx, `SELECT singleton_id FROM knowledge_catalog WHERE singleton_id=1 FOR UPDATE`); err != nil {
			return err
		}
	}
	var collection string
	if err := tx.QueryRow(ctx, `SELECT id FROM knowledge_collections WHERE id=$1 FOR UPDATE`, knowledge.CollectionID(ctx)).Scan(&collection); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return scopeMissing()
		}
		return err
	}
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM learning_spaces WHERE id=$1 FOR SHARE`, space.Scope(ctx)).Scan(&status); err != nil {
		return err
	}
	if status != "active" {
		return &space.Error{Code: "learning_space_archived"}
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT collection_id FROM knowledge_collection_links WHERE space_id=$1 AND collection_id=$2 FOR SHARE`, space.Scope(ctx), knowledge.CollectionID(ctx)).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return scopeMissing()
		}
		return err
	}
	return nil
}

func loadScopedRevision(ctx context.Context, db queryer, id string) (knowledge.KnowledgeRevision, error) {
	var isScope bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_scope_snapshots WHERE id=$1)`, id).Scan(&isScope); err != nil {
		return knowledge.KnowledgeRevision{}, err
	}
	if !isScope {
		if err := checkRevision(ctx, db, id); err != nil {
			return knowledge.KnowledgeRevision{}, err
		}
		return loadRevision(ctx, db, id)
	}
	if knowledge.HasCollection(ctx) {
		return knowledge.KnowledgeRevision{}, &knowledge.Error{Code: knowledge.CodeInvalidRequest}
	}
	snapshot, err := readScope(ctx, db, id)
	if err != nil {
		return knowledge.KnowledgeRevision{}, err
	}
	result := knowledge.KnowledgeRevision{ID: id, Documents: []knowledge.SnapshotDocument{}}
	seen := map[string]bool{}
	for _, entry := range snapshot.Entries {
		revision, err := loadRevision(ctx, db, entry.RevisionID)
		if err != nil {
			return knowledge.KnowledgeRevision{}, err
		}
		if revision.Redacted {
			return knowledge.KnowledgeRevision{}, &knowledge.Error{Code: knowledge.CodeContentRedacted}
		}
		for _, doc := range revision.Documents {
			doc.CollectionID = entry.CollectionID
			doc.KnowledgeRevisionID = entry.RevisionID
			if entry.DocumentID != "" && entry.DocumentID != doc.Revision.DocumentID {
				continue
			}
			key := doc.Revision.ID + ":" + entry.NodeID
			if seen[key] {
				continue
			}
			seen[key] = true
			if entry.NodeID != "" {
				var selected *knowledge.NodeRevision
				for i := range doc.Revision.Nodes {
					if doc.Revision.Nodes[i].NodeID == entry.NodeID {
						selected = &doc.Revision.Nodes[i]
						break
					}
				}
				if selected == nil {
					return knowledge.KnowledgeRevision{}, scopeMissing()
				}
				doc.SelectedRange = &selected.SectionRange
				if selected.HeadingLevel != 0 {
					root := doc.Revision.Nodes[0]
					root.Title = ""
					root.Children = []string{selected.ID}
					root.SectionRange = knowledge.SourceRange{}
					root.LocalBodyRange = knowledge.SourceRange{}
					nodes := []knowledge.NodeRevision{root}
					for _, n := range doc.Revision.Nodes {
						if n.SectionRange.Start >= selected.SectionRange.Start && n.SectionRange.End <= selected.SectionRange.End && n.HeadingLevel != 0 {
							// 范围外的祖先标题不能参与评分或进入模型上下文。
							prefix := len(selected.AncestorTitles)
							if len(n.AncestorTitles) >= prefix {
								n.AncestorTitles = append([]string{}, n.AncestorTitles[prefix:]...)
							}
							if n.ID == selected.ID {
								n.ParentNodeRevisionID = &root.ID
							}
							nodes = append(nodes, n)
						}
					}
					doc.Revision.Nodes = nodes
				}
			}
			result.Documents = append(result.Documents, doc)
		}
	}
	return result, nil
}

func checkCollection(ctx context.Context, db queryer, id string) error {
	var linked bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_collection_links WHERE space_id=$1 AND collection_id=$2)`, space.Scope(ctx), id).Scan(&linked)
	if err != nil {
		return err
	}
	if !linked {
		return scopeMissing()
	}
	return nil
}

func checkRevision(ctx context.Context, db queryer, id string) error {
	var linked bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_revisions r JOIN knowledge_collection_links l ON l.collection_id=r.collection_id WHERE r.id=$1 AND l.space_id=$2 AND r.collection_id=$3)`, id, space.Scope(ctx), knowledge.CollectionID(ctx)).Scan(&linked)
	if err != nil {
		return err
	}
	if !linked {
		return scopeMissing()
	}
	return nil
}

func (s *Store) Collections(ctx context.Context, shared bool) ([]knowledge.Collection, error) {
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, `SELECT c.id,c.name,c.source,c.shared,c.head_revision_id,c.version FROM knowledge_collections c WHERE EXISTS(SELECT 1 FROM knowledge_collection_links l WHERE l.collection_id=c.id AND l.space_id=$1) OR ($2 AND (c.shared OR c.owner_space_id=$1)) ORDER BY c.id`, space.Scope(ctx), shared)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []knowledge.Collection{}
	for rows.Next() {
		var c knowledge.Collection
		if err = rows.Scan(&c.ID, &c.Name, &c.Source, &c.Shared, &c.HeadRevisionID, &c.Version); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *Store) ChangeCollection(ctx context.Context, c knowledge.CollectionCommand) (knowledge.Collection, error) {
	if !space.ValidID(c.ID) {
		return knowledge.Collection{}, &knowledge.Error{Code: knowledge.CodeInvalidRequest}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return knowledge.Collection{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return knowledge.Collection{}, err
	}
	if err := lockSpaceWrite(ctx, tx); err != nil {
		return knowledge.Collection{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "knowledge-collection:"+c.ID); err != nil {
		return knowledge.Collection{}, err
	}
	if c.ID == knowledge.DefaultCollectionID {
		if _, err = tx.Exec(ctx, `SELECT singleton_id FROM knowledge_catalog WHERE singleton_id=1 FOR UPDATE`); err != nil {
			return knowledge.Collection{}, err
		}
	}
	var result knowledge.Collection
	var owner string
	err = tx.QueryRow(ctx, `SELECT id,name,source,shared,head_revision_id,version,owner_space_id FROM knowledge_collections WHERE id=$1 FOR UPDATE`, c.ID).Scan(&result.ID, &result.Name, &result.Source, &result.Shared, &result.HeadRevisionID, &result.Version, &owner)
	if c.Action == "create" {
		if c.ExpectedVersion != 0 || strings.TrimSpace(c.Name) == "" || utf8.RuneCountInString(c.Name) > 120 || !utf8.ValidString(c.Name) || strings.TrimSpace(c.Source) == "" || utf8.RuneCountInString(c.Source) > 500 || !utf8.ValidString(c.Source) {
			return result, &knowledge.Error{Code: knowledge.CodeInvalidRequest}
		}
		if err == nil {
			if result.Name == c.Name && result.Source == c.Source && result.Shared == c.Shared && checkCollection(ctx, tx, c.ID) == nil {
				return result, nil
			}
			return result, &knowledge.Error{Code: knowledge.CodeIdempotencyConflict}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return result, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO knowledge_collections(id,name,source,shared,owner_space_id) VALUES($1,$2,$3,$4,$5)`, c.ID, c.Name, c.Source, c.Shared, space.Scope(ctx)); err != nil {
			return result, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO knowledge_collection_links VALUES($1,$2)`, space.Scope(ctx), c.ID); err != nil {
			return result, err
		}
		result = knowledge.Collection{ID: c.ID, Name: c.Name, Source: c.Source, Shared: c.Shared, Version: 1}
	} else {
		if errors.Is(err, pgx.ErrNoRows) {
			return result, scopeMissing()
		}
		if err != nil {
			return result, err
		}
		switch c.Action {
		case "link":
			if !result.Shared && owner != space.Scope(ctx) {
				if err = checkCollection(ctx, tx, c.ID); err != nil {
					return knowledge.Collection{}, err
				}
			}
			_, err = tx.Exec(ctx, `INSERT INTO knowledge_collection_links VALUES($1,$2) ON CONFLICT DO NOTHING`, space.Scope(ctx), c.ID)
		case "unlink":
			if err = checkCollection(ctx, tx, c.ID); err != nil {
				return knowledge.Collection{}, err
			}
			_, err = tx.Exec(ctx, `DELETE FROM knowledge_collection_links WHERE space_id=$1 AND collection_id=$2`, space.Scope(ctx), c.ID)
		case "share":
			if err = checkCollection(ctx, tx, c.ID); err != nil && owner != space.Scope(ctx) {
				return knowledge.Collection{}, err
			}
			if c.ExpectedVersion != result.Version {
				return knowledge.Collection{}, &knowledge.Error{Code: knowledge.CodeRevisionConflict}
			}
			_, err = tx.Exec(ctx, `UPDATE knowledge_collections SET shared=$2,version=version+1 WHERE id=$1`, c.ID, c.Shared)
			result.Shared = c.Shared
			result.Version++
		default:
			return knowledge.Collection{}, &knowledge.Error{Code: knowledge.CodeInvalidRequest}
		}
		if err != nil {
			return knowledge.Collection{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return knowledge.Collection{}, err
	}
	return result, nil
}

func (s *Store) FreezeScope(ctx context.Context, snapshot knowledge.ScopeSnapshot) (knowledge.ScopeSnapshot, error) {
	if knowledge.HasCollection(ctx) {
		return knowledge.ScopeSnapshot{}, &knowledge.Error{Code: knowledge.CodeInvalidRequest}
	}
	snapshot.Updates = nil
	if !space.ValidID(snapshot.ID) || len(snapshot.Entries) == 0 || len(snapshot.Entries) > 1000 || (snapshot.SpaceID != "" && snapshot.SpaceID != space.Scope(ctx)) {
		return knowledge.ScopeSnapshot{}, &knowledge.Error{Code: knowledge.CodeInvalidRequest}
	}
	snapshot.SpaceID = space.Scope(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return knowledge.ScopeSnapshot{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return knowledge.ScopeSnapshot{}, err
	}
	if err := lockSpaceWrite(ctx, tx); err != nil {
		return knowledge.ScopeSnapshot{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "knowledge-scope:"+snapshot.ID); err != nil {
		return knowledge.ScopeSnapshot{}, err
	}
	if old, readErr := readScope(ctx, tx, snapshot.ID); readErr == nil {
		left, _ := json.Marshal(old.Entries)
		right, _ := json.Marshal(snapshot.Entries)
		if bytes.Equal(left, right) {
			return old, nil
		}
		return knowledge.ScopeSnapshot{}, &knowledge.Error{Code: knowledge.CodeIdempotencyConflict}
	} else if knowledge.ErrorCode(readErr) != knowledge.CodeNotFound {
		return knowledge.ScopeSnapshot{}, readErr
	}
	for _, entry := range snapshot.Entries {
		if !space.ValidID(entry.CollectionID) || !space.ValidID(entry.RevisionID) || (entry.DocumentID != "" && !space.ValidID(entry.DocumentID)) || (entry.NodeID != "" && (entry.DocumentID == "" || !space.ValidID(entry.NodeID))) {
			return knowledge.ScopeSnapshot{}, &knowledge.Error{Code: knowledge.CodeInvalidRequest}
		}
		var locked string
		if err = tx.QueryRow(ctx, `SELECT collection_id FROM knowledge_collection_links WHERE collection_id=$1 AND space_id=$2 FOR SHARE`, entry.CollectionID, snapshot.SpaceID).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return knowledge.ScopeSnapshot{}, scopeMissing()
			}
			return knowledge.ScopeSnapshot{}, err
		}
		var owned bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_revisions r JOIN knowledge_collection_links l ON l.collection_id=r.collection_id WHERE r.id=$1 AND r.collection_id=$2 AND l.space_id=$3 AND r.redacted_at IS NULL)`, entry.RevisionID, entry.CollectionID, snapshot.SpaceID).Scan(&owned)
		if err != nil {
			return knowledge.ScopeSnapshot{}, err
		}
		if !owned {
			return knowledge.ScopeSnapshot{}, scopeMissing()
		}
		if entry.DocumentID != "" {
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_snapshot_documents d WHERE d.knowledge_revision_id=$1 AND d.document_id=$2 AND ($3='' OR EXISTS(SELECT 1 FROM knowledge_node_revisions n WHERE n.document_revision_id=d.document_revision_id AND n.node_id::text=$3)))`, entry.RevisionID, entry.DocumentID, entry.NodeID).Scan(&owned)
			if err != nil {
				return knowledge.ScopeSnapshot{}, err
			}
			if !owned {
				return knowledge.ScopeSnapshot{}, scopeMissing()
			}
		}
	}
	data, _ := json.Marshal(snapshot.Entries)
	_, err = tx.Exec(ctx, `INSERT INTO knowledge_scope_snapshots(id,space_id,entries) VALUES($1,$2,$3)`, snapshot.ID, snapshot.SpaceID, data)
	if err != nil {
		return knowledge.ScopeSnapshot{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return knowledge.ScopeSnapshot{}, err
	}
	return snapshot, nil
}

func readScope(ctx context.Context, db queryer, id string) (knowledge.ScopeSnapshot, error) {
	var result knowledge.ScopeSnapshot
	var data []byte
	var redacted bool
	err := db.QueryRow(ctx, `SELECT id,space_id,entries,redacted FROM knowledge_scope_snapshots WHERE id=$1 AND space_id=$2`, id, space.Scope(ctx)).Scan(&result.ID, &result.SpaceID, &data, &redacted)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, scopeMissing()
	}
	if err != nil {
		return result, err
	}
	if redacted {
		return result, &knowledge.Error{Code: knowledge.CodeContentRedacted}
	}
	err = json.Unmarshal(data, &result.Entries)
	return result, err
}

func (s *Store) ReadScope(ctx context.Context, id string) (knowledge.ScopeSnapshot, error) {
	if !space.ValidID(id) {
		return knowledge.ScopeSnapshot{}, scopeMissing()
	}
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return knowledge.ScopeSnapshot{}, err
	}
	defer tx.Rollback(context.Background())
	result, err := readScope(ctx, tx, id)
	if err != nil {
		return result, err
	}
	for _, entry := range result.Entries {
		var latest *string
		if err = tx.QueryRow(ctx, `SELECT head_revision_id FROM knowledge_collections WHERE id=$1`, entry.CollectionID).Scan(&latest); err != nil {
			return knowledge.ScopeSnapshot{}, err
		}
		if latest != nil && *latest != entry.RevisionID {
			result.Updates = append(result.Updates, knowledge.ScopeEntry{CollectionID: entry.CollectionID, RevisionID: *latest})
		}
	}
	return result, nil
}
