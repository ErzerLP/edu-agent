package learningcontent

import (
	"context"
	"errors"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type StudioSectionSave struct {
	OperationID     string `json:"operation_id"`
	Generation      int64  `json:"privacy_generation"`
	ExpectedVersion int64  `json:"expected_version"`
	Markdown        string `json:"markdown"`
}

func (s *Store) StudioSaveSection(ctx context.Context, actor identity.Credential, id, sectionID string, c StudioSectionSave) (StudioRevision, error) {
	if uuid.Validate(id) != nil || uuid.Validate(c.OperationID) != nil || c.ExpectedVersion < 1 || c.Generation < 1 || !studioText(c.Markdown, 64000) || len(c.Markdown) > 64000 || len(sectionID) > 80 || sectionID == "" {
		return StudioRevision{}, ErrInvalid
	}
	tx, g, err := s.studioBegin(ctx, actor, true)
	if err != nil {
		return StudioRevision{}, err
	}
	defer tx.Rollback(context.Background())
	if g != c.Generation {
		return StudioRevision{}, &privacy.Error{Code: privacy.CodeContentRedacted}
	}
	hash := fingerprint(struct {
		Space, ID, Section string
		Command            StudioSectionSave
	}{learningspace.Scope(ctx), id, sectionID, c})
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "studio-operation:"+actor.Device.ID+":"+c.OperationID); err != nil {
		return StudioRevision{}, err
	}
	var storedID, storedHash string
	var storedVersion int64
	err = tx.QueryRow(ctx, `SELECT artifact_id,version,encode(request_hash,'hex') FROM learning_studio_operations WHERE device_id=$1 AND operation_id=$2`, actor.Device.ID, c.OperationID).Scan(&storedID, &storedVersion, &storedHash)
	if err == nil {
		if storedHash != hash {
			return StudioRevision{}, ErrConflict
		}
		r, e := s.studioRead(ctx, tx, storedID, storedVersion, g, actor.TokenID)
		if e != nil {
			return r, e
		}
		return r, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return StudioRevision{}, err
	}
	target := StudioTarget{ArtifactID: &id, ExpectedVersion: c.ExpectedVersion, Generation: g, Kind: "note"}
	target, err = s.StudioDocumentTargetTx(ctx, tx, target)
	if err != nil {
		return StudioRevision{}, err
	}
	if err = s.StudioTargetTx(ctx, tx, actor, target); err != nil {
		return StudioRevision{}, err
	}
	previous, err := s.studioRead(ctx, tx, id, c.ExpectedVersion, g, actor.TokenID)
	if err != nil {
		return previous, err
	}
	if previous.Body.Schema != 2 {
		return StudioRevision{}, ErrUnsupported
	}
	var ordinal int
	if err = tx.QueryRow(ctx, `SELECT ordinal FROM learning_studio_section_revisions WHERE artifact_id=$1 AND version=$2 AND section_id=$3`, id, c.ExpectedVersion, sectionID).Scan(&ordinal); errors.Is(err, pgx.ErrNoRows) {
		return StudioRevision{}, ErrNotFound
	} else if err != nil {
		return StudioRevision{}, err
	}
	page, err := s.studioSectionsTx(ctx, tx, previous, actor.TokenID, ordinal-1, 1)
	if err != nil {
		return previous, err
	}
	section := page.Items[0]
	section.Body.Markdown = c.Markdown
	section.Body.UserAuthored = true
	section.Body.Claims = nil
	section.Body.Citations = []StudioCitation{}
	// Edited prose is explicitly user-authored. Old verified statements remain
	// in the previous revision, and no summary may continue claiming this text.
	body := previous.Body
	summary := *body.Summary
	filter := func(items []StudioSummaryClaim) []StudioSummaryClaim {
		out := []StudioSummaryClaim{}
		for _, v := range items {
			if !strings.HasPrefix(v.ID, sectionID+":") {
				out = append(out, v)
			}
		}
		return out
	}
	summary.Overview = filter(summary.Overview)
	summary.Transitions = filter(summary.Transitions)
	body.Summary = &summary
	r, err := s.studioPublishTx(ctx, tx, actor, c.OperationID, target, body, hash)
	if err != nil {
		return r, err
	}
	if err = s.insertStudioSectionTx(ctx, tx, r, section); err != nil {
		return r, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_studio_section_revisions(artifact_id,version,section_id,ordinal,content_version) SELECT artifact_id,$3,section_id,ordinal,content_version FROM learning_studio_section_revisions WHERE artifact_id=$1 AND version=$2 AND section_id<>$4`, id, c.ExpectedVersion, r.Version, sectionID); err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}

// Only this explicit import path assembles saved sections, up to the existing
// knowledge import limit. Normal reads and editing remain paginated.
func (s *Store) StudioNoteSource(ctx context.Context, actor identity.Credential, id string, version int64) (knowledge.SavedNote, error) {
	if uuid.Validate(id) != nil || version < 1 {
		return knowledge.SavedNote{}, ErrInvalid
	}
	tx, g, err := s.studioBegin(ctx, actor, false)
	if err != nil {
		return knowledge.SavedNote{}, err
	}
	defer tx.Rollback(context.Background())
	r, err := s.studioRead(ctx, tx, id, version, g, actor.TokenID)
	if err != nil {
		return knowledge.SavedNote{}, err
	}
	if r.Kind != "note" {
		return knowledge.SavedNote{}, ErrInvalid
	}
	note := knowledge.SavedNote{Title: r.Body.Title, Markdown: r.Body.Markdown}
	if r.Body.Schema == 2 {
		var text strings.Builder
		after := 0
		for {
			page, e := s.studioSectionsTx(ctx, tx, r, actor.TokenID, after, 32)
			if e != nil {
				return note, e
			}
			for _, section := range page.Items {
				chunk := "## " + section.Body.Title + "\n\n" + section.Body.Markdown + "\n\n"
				if text.Len()+len(chunk) > knowledge.MaxDocumentBytes {
					return knowledge.SavedNote{}, &knowledge.Error{Code: knowledge.CodePayloadTooLarge}
				}
				text.WriteString(chunk)
				after = section.Ordinal
			}
			if page.NextCursor == "" {
				break
			}
		}
		note.Markdown = text.String()
	}
	return note, tx.Commit(ctx)
}
