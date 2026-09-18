package learningcontent

import (
	"context"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
	"github.com/jackc/pgx/v5"
)

type Citation struct {
	Reference  learning.KnowledgeReference `json:"reference"`
	Title      string                      `json:"title"`
	Context    string                      `json:"context"`
	Status     string                      `json:"status"`
	Parser     string                      `json:"parser"`
	Historical bool                        `json:"historical"`
	Coverage   string                      `json:"coverage"`
	Locator    string                      `json:"locator"`
	PDF        *PDFCitation                `json:"pdf,omitempty"`
}

type PDFCitation struct {
	Fingerprint  string           `json:"fingerprint"`
	PageCount    int              `json:"page_count"`
	Pages        []pdfsource.Page `json:"pages"`
	CollectionID string           `json:"collection_id,omitempty"`
}

type ReferenceReader interface {
	ContentCitationTx(context.Context, pgx.Tx, learning.KnowledgeReference) (Citation, error)
}

func (s *Store) referenceAccess(ctx context.Context, tx pgx.Tx, actor identity.Credential) error {
	if s.references == nil {
		return nil
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT 'knowledge:read'=ANY(scopes) FROM device_tokens WHERE id=$1`, actor.TokenID).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

func (s *Store) checkSources(ctx context.Context, tx pgx.Tx, r Revision) error {
	if s.references == nil {
		return nil
	}
	for _, ref := range r.Body.References {
		if _, err := s.references.ContentCitationTx(ctx, tx, ref); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Citation(ctx context.Context, actor identity.Credential, id string, version int64, reference string) (Citation, error) {
	tx, g, err := s.begin(ctx, actor, false)
	if err != nil {
		return Citation{}, err
	}
	defer tx.Rollback(context.Background())
	var allowed bool
	if err = tx.QueryRow(ctx, `SELECT 'knowledge:read'=ANY(scopes) FROM device_tokens WHERE id=$1`, actor.TokenID).Scan(&allowed); err != nil {
		return Citation{}, err
	}
	if !allowed {
		return Citation{}, ErrForbidden
	}
	r, err := s.read(ctx, tx, id, version, g, false)
	if err != nil {
		return Citation{}, err
	}
	for _, ref := range r.Body.References {
		if ref.NodeRevisionID == reference {
			if s.references == nil {
				return Citation{}, ErrUnavailable
			}
			result, err := s.references.ContentCitationTx(ctx, tx, ref)
			if err != nil {
				return Citation{}, err
			}
			return result, tx.Commit(ctx)
		}
	}
	return Citation{}, ErrNotFound
}
