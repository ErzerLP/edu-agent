package postgresstore

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

// Selection reads only the named document payloads. Collection-wide export and
// retrieval would load unselected text and are deliberately not used here.
func (s *Store) selectedSourceTx(ctx context.Context, tx pgx.Tx, item knowledge.SourceIdentity) (knowledge.SourceDetail, string, error) {
	var linked string
	err := tx.QueryRow(ctx, `SELECT collection_id FROM knowledge_collection_links WHERE space_id=$1 AND collection_id=$2 FOR SHARE`, space.Scope(ctx), item.CollectionID).Scan(&linked)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledge.SourceDetail{}, "", scopeMissing()
	}
	if err != nil {
		return knowledge.SourceDetail{}, "", err
	}
	scoped, err := knowledge.WithCollection(ctx, item.CollectionID)
	if err != nil {
		return knowledge.SourceDetail{}, "", err
	}
	detail, hash, err := s.sourceTx(scoped, tx, item.RevisionID, item.DocumentRevisionID)
	if err != nil {
		return detail, "", err
	}
	if detail.DocumentID != item.DocumentID {
		return knowledge.SourceDetail{}, "", scopeMissing()
	}
	return detail, hash, nil
}

func (s *Store) FreezeSourceSelectionTx(ctx context.Context, tx pgx.Tx, selection knowledge.SourceSelection) (*knowledge.SourceManifest, error) {
	if err := selection.Validate(); err != nil {
		return nil, err
	}
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return nil, err
	}
	manifest := &knowledge.SourceManifest{Schema: 1, SpaceID: space.Scope(ctx), Generation: generation, Entries: []knowledge.SourceManifestEntry{}}
	for _, item := range selection.Items {
		_, hash, err := s.sourceIdentityTx(ctx, tx, item)
		if err != nil {
			return nil, err
		}
		manifest.Entries = append(manifest.Entries, knowledge.SourceManifestEntry{SourceIdentity: item, SHA256: hash})
	}
	knowledge.SortSourceEntries(manifest.Entries)
	manifest.SHA256 = manifest.Hash()
	return manifest, nil
}

func (s *Store) ReadSourceSelectionTx(ctx context.Context, tx pgx.Tx, manifest knowledge.SourceManifest) ([]knowledge.SourceDetail, error) {
	if manifest.Schema != 1 || manifest.SpaceID != space.Scope(ctx) || manifest.SHA256 != manifest.Hash() {
		return nil, &knowledge.Error{Code: knowledge.CodeInvalidRequest}
	}
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return nil, err
	}
	if generation != manifest.Generation {
		return nil, &knowledge.Error{Code: knowledge.CodeContentRedacted}
	}
	result := []knowledge.SourceDetail{}
	for _, entry := range manifest.Entries {
		detail, hash, err := s.selectedSourceTx(ctx, tx, entry.SourceIdentity)
		if err != nil {
			return nil, err
		}
		if hash != entry.SHA256 {
			return nil, &knowledge.Error{Code: knowledge.CodeContentRedacted}
		}
		result = append(result, detail)
	}
	return result, nil
}

// LockSourceLinksTx protects dispatch against unlink without retaining owner
// generation gates across network I/O. Privacy cancellation uses ReadPermit.
func (s *Store) LockSourceLinksTx(ctx context.Context, tx pgx.Tx, manifest knowledge.SourceManifest) error {
	for _, entry := range manifest.Entries {
		var linked string
		err := tx.QueryRow(ctx, `SELECT collection_id FROM knowledge_collection_links WHERE space_id=$1 AND collection_id=$2 FOR SHARE`, space.Scope(ctx), entry.CollectionID).Scan(&linked)
		if errors.Is(err, pgx.ErrNoRows) {
			return scopeMissing()
		}
		if err != nil {
			return err
		}
	}
	return nil
}
