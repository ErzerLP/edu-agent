package mentorrun

import (
	"context"
	"errors"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type WebOwner struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}
type WebPreviewRequest struct {
	OperationID string   `json:"operation_id"`
	Owner       WebOwner `json:"owner"`
	RevisionIDs []string `json:"document_revision_ids"`
}
type WebPreviewItem struct {
	RevisionID            string               `json:"document_revision_id"`
	Hash                  string               `json:"hash"`
	Coverage              string               `json:"coverage"`
	RequiresPartialAccept bool                 `json:"requires_partial_accept"`
	Document              research.WebDocument `json:"document"`
}
type WebPreview struct {
	ID        string           `json:"preview_id"`
	Version   int64            `json:"preview_version"`
	Items     []WebPreviewItem `json:"items"`
	ExpiresAt time.Time        `json:"expires_at"`
}
type WebImportRequest struct {
	OperationID     string `json:"operation_id"`
	PreviewID       string `json:"preview_id"`
	ExpectedVersion int64  `json:"expected_preview_version"`
	RevisionID      string `json:"document_revision_id"`
	StorageConsent  bool   `json:"storage_consent"`
	AcceptPartial   bool   `json:"accept_partial"`
}
type WebImportReceipt struct {
	OperationID string                   `json:"operation_id"`
	RunID       string                   `json:"run_id"`
	DocumentID  string                   `json:"document_id"`
	RevisionID  string                   `json:"document_revision_id"`
	Source      knowledge.SourceIdentity `json:"source"`
}

func (s *Service) webImportTx(ctx context.Context, actor identity.Credential, space, run string) (pgx.Tx, row, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, row{}, err
	}
	fail := func(err error) (pgx.Tx, row, error) { tx.Rollback(context.Background()); return nil, row{}, err }
	g, err := gates(ctx, tx, true)
	if err != nil {
		return fail(err)
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, true); err != nil {
		return fail(err)
	}
	if err = sourceActor(ctx, tx, actor.TokenID); err != nil {
		return fail(err)
	}
	if err = knowledgeActor(ctx, tx, actor.Device.ID, actor.TokenID); err != nil {
		return fail(err)
	}
	// Read lock serializes deletion and import. Knowledge takes its own owner gate.
	item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1 AND space_id=$2 AND device_id=$3 AND privacy_generation=$4 FOR SHARE`, run, space, actor.Device.ID, g))
	if err != nil {
		return fail(err)
	}
	if !item.BodyAvailable || time.Now().After(item.ExpiresAt) {
		return fail(ErrNotFound)
	}
	return tx, item, nil
}
func (s *Service) PreviewWebImport(ctx context.Context, actor identity.Credential, space string, r WebPreviewRequest) (WebPreview, error) {
	result := WebPreview{}
	if !validID(space) || !validID(r.OperationID) || r.Owner.Kind != "mentor_run" || !validID(r.Owner.ID) || len(r.RevisionIDs) != 1 || !validID(r.RevisionIDs[0]) {
		return result, ErrInvalid
	}
	tx, item, err := s.webImportTx(ctx, actor, space, r.Owner.ID)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	hash := requestHash("web_preview", space, r.Owner.ID, r)
	if found, err := s.webOperation(ctx, tx, actor.Device.ID, space, r.OperationID, item.Generation, hash, &result); err != nil || found {
		if found && time.Now().After(result.ExpiresAt) {
			return result, ErrConflict
		}
		if err == nil && found {
			var id string
			if err = tx.QueryRow(ctx, `SELECT document_id::text FROM learning_web_import_previews WHERE id=$1 AND privacy_generation=$2`, result.ID, item.Generation).Scan(&id); err != nil {
				return result, ErrNotFound
			}
			result.Items[0].Document, err = s.webDocumentTx(ctx, tx, item, id)
		}
		return result, err
	}
	var id string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM learning_web_documents WHERE revision_id=$1 AND owner_id=$2 AND space_id=$3`, r.RevisionIDs[0], item.RunID, space).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	} else if err != nil {
		return result, err
	}
	d, err := s.webDocumentTx(ctx, tx, item, id)
	if err != nil {
		return result, err
	}
	result = WebPreview{ID: uuid.NewString(), Version: 1, ExpiresAt: time.Now().Add(15 * time.Minute), Items: []WebPreviewItem{{RevisionID: d.RevisionID, Hash: d.SHA256, Coverage: d.Coverage, RequiresPartialAccept: d.PDF != nil && d.Coverage != "complete_text", Document: d}}}
	if result.ExpiresAt.After(item.ExpiresAt) {
		result.ExpiresAt = item.ExpiresAt
	}
	// Only identities, hash and coverage are duplicated in the preview receipt.
	stored := result
	stored.Items = append([]WebPreviewItem{}, result.Items...)
	stored.Items[0].Document = research.WebDocument{}
	raw, err := s.sealWeb("preview", result.ID, item.Generation, stored)
	if err != nil {
		return result, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_web_import_previews(id,owner_id,document_id,device_id,space_id,privacy_generation,ciphertext,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, result.ID, item.RunID, d.ID, actor.Device.ID, space, item.Generation, raw, result.ExpiresAt)
	if err != nil {
		return result, err
	}
	if err = s.saveWebOperation(ctx, tx, actor.Device.ID, space, item.ConversationID, item.RunID, r.OperationID, item.Generation, hash, stored); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}
func (s *Service) ImportWebDocument(ctx context.Context, actor identity.Credential, space string, r WebImportRequest) (WebImportReceipt, error) {
	result := WebImportReceipt{}
	if !validID(space) || !validID(r.OperationID) || !validID(r.PreviewID) || !validID(r.RevisionID) || r.ExpectedVersion < 1 || !r.StorageConsent {
		return result, ErrInvalid
	}
	var run string
	err := s.pool.QueryRow(ctx, `SELECT owner_id::text FROM learning_web_import_previews WHERE id=$1 AND device_id=$2 AND space_id=$3`, r.PreviewID, actor.Device.ID, space).Scan(&run)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	tx, item, err := s.webImportTx(ctx, actor, space, run)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	hash := requestHash("web_import", space, run, r)
	if found, err := s.webOperation(ctx, tx, actor.Device.ID, space, r.OperationID, item.Generation, hash, &result); err != nil || found {
		return result, err
	}
	var raw []byte
	var documentID string
	var version int64
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT document_id::text,version,ciphertext,expires_at FROM learning_web_import_previews WHERE id=$1 AND device_id=$2 AND space_id=$3 AND privacy_generation=$4 FOR SHARE`, r.PreviewID, actor.Device.ID, space, item.Generation).Scan(&documentID, &version, &raw, &expires)
	if err != nil {
		return result, err
	}
	if version != r.ExpectedVersion || !time.Now().Before(expires) {
		return result, ErrConflict
	}
	var preview WebPreview
	if err = s.openWeb("preview", r.PreviewID, item.Generation, raw, &preview); err != nil {
		return result, err
	}
	if preview.Items[0].RevisionID != r.RevisionID || preview.Items[0].RequiresPartialAccept && !r.AcceptPartial {
		return result, ErrConflict
	}
	d, err := s.webDocumentTx(ctx, tx, item, documentID)
	if err != nil {
		return result, err
	}
	if d.SHA256 != preview.Items[0].Hash {
		return result, ErrConflict
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,52))`, run+r.RevisionID+space); err != nil {
		return result, err
	}
	var receiptID string
	err = tx.QueryRow(ctx, `SELECT id::text,ciphertext FROM learning_web_import_receipts WHERE owner_id=$1 AND revision_id=$2 AND space_id=$3`, run, r.RevisionID, space).Scan(&receiptID, &raw)
	if err == nil {
		err = s.openWeb("receipt", receiptID, item.Generation, raw, &result)
	} else if errors.Is(err, pgx.ErrNoRows) {
		source := research.Source{ID: uuid.NewString(), RevisionID: d.RevisionID, SpaceID: space, Purpose: "notebook_web", Locator: d.RequestURL, FinalURL: d.URL, Title: d.Title, Kind: d.MediaType, Status: "parsed", StorageAllowed: true, Text: d.Text, Fragments: d.Fragments, Coverage: d.Coverage, FetchedAt: &d.FetchedAt, Fingerprint: d.Fingerprint, Parser: d.Parser, PDF: d.PDF}
		if d.Coverage != "complete_text" {
			source.Status = "partial"
		}
		if d.PDF != nil {
			original, e := s.webAttachment(ctx, tx, item, d.ID)
			if e != nil {
				return result, e
			}
			report, e := pdfsource.Parse(ctx, original)
			if e != nil {
				return result, e
			}
			if report.Fingerprint != d.Fingerprint || research.TextHash(report.Text()) != d.SHA256 {
				return result, ErrConflict
			}
			source.PDF = &report
			source.PDFOriginal = original
		}
		scoped, _ := learningspace.WithScope(ctx, space)
		imported, e := knowledgepg.New(s.pool).AdoptSourceTx(scoped, tx, knowledge.SourceImport{RunID: run, Metadata: source, OperationID: r.OperationID, SourceID: source.ID, SourceRevisionID: d.RevisionID, ActorDeviceID: actor.Device.ID, Locator: d.RequestURL, Title: d.Title, Fingerprint: d.Fingerprint, Coverage: d.Coverage, Text: d.Text, FetchedAt: d.FetchedAt})
		if e != nil {
			return result, e
		}
		doc := imported.Revision.Documents[0].Revision
		result = WebImportReceipt{OperationID: r.OperationID, RunID: run, DocumentID: d.ID, RevisionID: d.RevisionID, Source: knowledge.SourceIdentity{CollectionID: source.ID, RevisionID: imported.Revision.ID, DocumentID: doc.DocumentID, DocumentRevisionID: doc.ID}}
		receiptID = uuid.NewString()
		raw, err = s.sealWeb("receipt", receiptID, item.Generation, result)
		if err != nil {
			return result, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO learning_web_import_receipts(id,owner_id,document_id,revision_id,space_id,privacy_generation,ciphertext) VALUES($1,$2,$3,$4,$5,$6,$7)`, receiptID, run, d.ID, d.RevisionID, space, item.Generation, raw)
	}
	if err != nil {
		return result, err
	}
	if err = s.saveWebOperation(ctx, tx, actor.Device.ID, space, item.ConversationID, run, r.OperationID, item.Generation, hash, result); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}
