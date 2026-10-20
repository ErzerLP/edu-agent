package mentorrun

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type WebResource = research.WebResource

func (h *executionHost) registerWebResource(ctx context.Context, url, title, summary, origin string) (WebResource, error) {
	result := WebResource{ID: uuid.NewString(), Title: bounded(title, 1024), URL: url, Summary: bounded(summary, 800), SummaryOnly: true}
	u, err := research.ValidateURL(url)
	if err != nil {
		return result, err
	}
	result.URL = u.String()
	mac := hmac.New(sha256.New, h.body.Web.ResourceKey)
	mac.Write([]byte(result.URL))
	digest := mac.Sum(nil)
	err = h.service.mutateTx(ctx, &h.owned, "", func(tx pgx.Tx, item *row) error {
		if err := h.webGateTx(ctx, tx); err != nil {
			return err
		}
		var raw []byte
		var id string
		err := tx.QueryRow(ctx, `SELECT id::text,ciphertext FROM learning_web_resources WHERE owner_kind='mentor_run' AND owner_id=$1 AND url_digest=$2`, item.RunID, digest).Scan(&id, &raw)
		if err == nil {
			return h.service.openWeb("resource", id, item.Generation, raw, &result)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		raw, err = h.service.sealWeb("resource", result.ID, item.Generation, result)
		if err != nil {
			return err
		}
		if err = h.service.quota(ctx, tx, int64(len(raw))); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO learning_web_resources(id,owner_kind,owner_id,space_id,device_id,privacy_generation,url_digest,origin,ciphertext,expires_at) VALUES($1,'mentor_run',$2,$3,$4,$5,$6,$7,$8,$9)`, result.ID, item.RunID, item.SpaceID, item.device, item.Generation, digest, origin, raw, item.ExpiresAt)
		return err
	})
	return result, err
}
func (h *executionHost) webResource(ctx context.Context, id string) (WebResource, error) {
	var resource WebResource
	err := h.service.mutateTx(ctx, &h.owned, "", func(tx pgx.Tx, item *row) error {
		if err := h.webGateTx(ctx, tx); err != nil {
			return err
		}
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT ciphertext FROM learning_web_resources WHERE id=$1 AND owner_id=$2 AND space_id=$3 AND privacy_generation=$4 AND expires_at>clock_timestamp()`, id, item.RunID, item.SpaceID, item.Generation).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return h.service.openWeb("resource", id, item.Generation, raw, &resource)
	})
	return resource, err
}
func (h *executionHost) storeWebDocument(ctx context.Context, source research.Source) (research.WebDocument, error) {
	d, err := research.NewWebDocument(uuid.NewString(), source)
	if err != nil {
		return d, err
	}
	for _, link := range source.Links {
		resource, e := h.registerWebResource(ctx, link, link, "", "page")
		if e != nil {
			return d, e
		}
		d.Resources = append(d.Resources, resource)
	}
	err = h.service.mutateTx(ctx, &h.owned, "web_read", func(tx pgx.Tx, item *row) error {
		if err := h.webGateTx(ctx, tx); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_web_resources WHERE id=$1 AND owner_id=$2 AND space_id=$3 AND privacy_generation=$4 AND expires_at>clock_timestamp())`, source.ID, item.RunID, item.SpaceID, item.Generation).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		raw, err := h.service.sealWeb("document", d.ID, item.Generation, d)
		if err != nil {
			return err
		}
		var attachment []byte
		if len(source.PDFOriginal) > 0 {
			attachment, err = h.service.sealWeb("attachment", d.ID, item.Generation, source.PDFOriginal)
			if err != nil {
				return err
			}
		}
		if err = h.service.quota(ctx, tx, int64(len(raw)+len(attachment))); err != nil {
			return err
		}
		hash, _ := hex.DecodeString(d.SHA256)
		_, err = tx.Exec(ctx, `INSERT INTO learning_web_documents(id,revision_id,resource_id,owner_kind,owner_id,space_id,device_id,privacy_generation,document_hash,expires_at) VALUES($1,$2,$3,'mentor_run',$4,$5,$6,$7,$8,$9)`, d.ID, d.RevisionID, source.ID, item.RunID, item.SpaceID, item.device, item.Generation, hash, item.ExpiresAt)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO learning_web_document_payloads(id,privacy_generation,ciphertext,attachment) VALUES($1,$2,$3,$4)`, d.ID, item.Generation, raw, attachment)
		return err
	})
	return d, err
}

func (s *Service) webDocumentTx(ctx context.Context, tx pgx.Tx, item row, id string) (research.WebDocument, error) {
	var d research.WebDocument
	var raw []byte
	if !item.BodyAvailable || time.Now().After(item.ExpiresAt) || !item.Saved && item.process != s.process {
		return d, ErrNotFound
	}
	err := tx.QueryRow(ctx, `SELECT p.ciphertext FROM learning_web_documents d JOIN learning_web_document_payloads p USING(id) WHERE d.id=$1 AND d.owner_id=$2 AND d.space_id=$3 AND d.device_id=$4 AND d.privacy_generation=$5 AND d.expires_at>clock_timestamp()`, id, item.RunID, item.SpaceID, item.device, item.Generation).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	return d, s.openWeb("document", id, item.Generation, raw, &d)
}
func (s *Service) withWebDocument(ctx context.Context, actor identity.Credential, space, id string, fn func(pgx.Tx, row, research.WebDocument) error) error {
	if !validID(space) || !validID(id) {
		return ErrInvalid
	}
	var run string
	err := s.pool.QueryRow(ctx, `SELECT owner_id::text FROM learning_web_documents WHERE id=$1 AND space_id=$2 AND device_id=$3`, id, space, actor.Device.ID).Scan(&run)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.read(ctx, actor, space, run, func(tx pgx.Tx, item row) error {
		d, err := s.webDocumentTx(ctx, tx, item, id)
		if err != nil {
			return err
		}
		return fn(tx, item, d)
	})
}
func (s *Service) documentPage(d research.WebDocument, g int64, cursor string) (research.WebDocument, error) {
	start := 0
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || len(raw) > 256 {
			return d, ErrInvalid
		}
		if err = s.openWeb("cursor", d.ID, g, raw, &start); err != nil {
			return d, ErrInvalid
		}
	}
	if start < 0 || start >= len(d.Fragments) {
		return d, ErrInvalid
	}
	end := min(start+3, len(d.Fragments))
	d.Fragments = d.Fragments[start:end]
	d.Text = ""
	d.NextCursor = ""
	if d.PDF != nil {
		copy := *d.PDF
		copy.Pages = append([]pdfsource.Page{}, copy.Pages...)
		for i := range copy.Pages {
			copy.Pages[i].Text = ""
		}
		d.PDF = &copy
	}
	return d, nil
}
func (s *Service) pageWebDocument(d research.WebDocument, g int64, cursor string) (research.WebDocument, error) {
	total := len(d.Fragments)
	page, err := s.documentPage(d, g, cursor)
	if err != nil {
		return page, err
	}
	last := page.Fragments[len(page.Fragments)-1].ID
	next := 0
	for i, f := range d.Fragments {
		if f.ID == last {
			next = i + 1
			break
		}
	}
	if next < total {
		raw, err := s.sealWeb("cursor", d.ID, g, next)
		if err != nil {
			return page, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}
func (s *Service) ReadWebDocument(ctx context.Context, actor identity.Credential, space, id, cursor string, send func(research.WebDocument) error) error {
	return s.withWebDocument(ctx, actor, space, id, func(_ pgx.Tx, item row, d research.WebDocument) error {
		page, err := s.pageWebDocument(d, item.Generation, cursor)
		if err != nil {
			return err
		}
		return send(page)
	})
}
func (s *Service) ReadWebPage(ctx context.Context, actor identity.Credential, space, id string, page int, send func([]byte) error) error {
	if page < 1 || page > pdfsource.MaxPages {
		return ErrInvalid
	}
	return s.withWebDocument(ctx, actor, space, id, func(tx pgx.Tx, item row, d research.WebDocument) error {
		if d.PDF == nil || page > d.PDF.PageCount {
			return ErrNotFound
		}
		original, err := s.webAttachment(ctx, tx, item, id)
		if err != nil {
			return err
		}
		png, err := pdfsource.Render(ctx, original, page)
		if err != nil {
			return err
		}
		return send(png)
	})
}
func (s *Service) webAttachment(ctx context.Context, tx pgx.Tx, item row, id string) ([]byte, error) {
	var raw, original []byte
	if err := tx.QueryRow(ctx, `SELECT attachment FROM learning_web_document_payloads WHERE id=$1 AND privacy_generation=$2`, id, item.Generation).Scan(&raw); err != nil {
		return nil, err
	}
	if err := s.openWeb("attachment", id, item.Generation, raw, &original); err != nil {
		return nil, err
	}
	return original, nil
}
