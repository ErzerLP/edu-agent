package learningcontent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/jackc/pgx/v5"
)

type StudioSection struct {
	ID        string     `json:"id"`
	SectionID string     `json:"section_id"`
	Ordinal   int        `json:"ordinal"`
	Body      StudioBody `json:"body"`
}
type StudioSectionPage struct {
	Items      []StudioSection `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

func sectionAAD(r StudioRevision, id string, version int64) []byte {
	return []byte(fmt.Sprintf("studio-section:v1:%s:%s:%d:%d:%s", r.SpaceID, r.ArtifactID, r.Generation, version, id))
}
func (s *Store) insertStudioSectionTx(ctx context.Context, tx pgx.Tx, r StudioRevision, section StudioSection) error {
	plain, err := json.Marshal(section)
	if err != nil {
		return err
	}
	defer clear(plain)
	if len(plain) > MaxBody {
		return ErrInvalid
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	ciphertext := s.aead.Seal(nonce, nonce, plain, sectionAAD(r, section.ID, r.Version))
	_, err = tx.Exec(ctx, `INSERT INTO learning_studio_section_revisions(artifact_id,version,section_id,ordinal,content_version,ciphertext) VALUES($1,$2,$3,$4,$2,$5)`, r.ArtifactID, r.Version, section.ID, section.Ordinal, ciphertext)
	return err
}

// Publication is owned here: every unit and source is rechecked in the same
// transaction as the target CAS. No candidate publication exists before this.
func (s *Store) PublishStudioJobTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, j StudioJob, operation string) (StudioRevision, error) {
	if j.Status != "summarizing" || j.Input.Summary == nil || j.TotalUnits == 0 || j.CompletedUnits != j.TotalUnits {
		return StudioRevision{}, ErrConflict
	}
	if j.Kind == "quiz" {
		return s.publishStudioQuizJobTx(ctx, tx, actor, j, operation)
	}
	body := StudioBody{Schema: 2, Title: j.Plan.Title, SourceManifest: &j.Input.Manifest, Citations: []StudioCitation{}, Coverage: j.Plan.Coverage, InputFingerprint: j.Plan.InputFingerprint, Summary: j.Input.Summary}
	counts := map[string]int{}
	for after := 0; ; {
		page, e := s.StudioUnitsTx(ctx, tx, j, after, 32)
		if e != nil {
			return StudioRevision{}, e
		}
		for _, u := range page.Items {
			if u.Status != "succeeded" {
				return StudioRevision{}, ErrConflict
			}
			if e = s.VerifyStudioUnitTx(ctx, tx, actor.TokenID, u); e != nil {
				return StudioRevision{}, e
			}
			counts[u.SectionID]++
			after = u.Ordinal
			if u.Body.Coverage == "partial" {
				body.Coverage = "partial"
			}
		}
		if page.NextCursor == "" {
			break
		}
	}
	for _, section := range j.Plan.Sections {
		body.Sections = append(body.Sections, StudioSectionInfo{section.ID, section.Topic, counts[section.ID]})
	}
	target := StudioTarget{Generation: j.Generation, Kind: j.Kind, ExpectedVersion: j.Input.ExpectedArtifactVersion}
	if j.Input.ArtifactID != "" {
		target.ArtifactID = &j.Input.ArtifactID
	}
	target, err := s.StudioDocumentTargetTx(ctx, tx, target)
	if err != nil {
		return StudioRevision{}, err
	}
	r, err := s.StudioPublishTx(ctx, tx, actor, operation, target, body)
	if err != nil {
		return r, err
	}
	after := 0
	for {
		page, e := s.StudioUnitsTx(ctx, tx, j, after, 32)
		if e != nil {
			return r, e
		}
		for _, u := range page.Items {
			if e = s.insertStudioSectionTx(ctx, tx, r, StudioSection{u.ID, u.SectionID, u.Ordinal, *u.Body}); e != nil {
				return r, e
			}
			after = u.Ordinal
		}
		if page.NextCursor == "" {
			break
		}
	}
	return r, nil
}
func (s *Store) studioSectionsTx(ctx context.Context, tx pgx.Tx, r StudioRevision, token string, after, limit int) (StudioSectionPage, error) {
	page := StudioSectionPage{Items: []StudioSection{}}
	rows, err := tx.Query(ctx, `SELECT p.section_id,p.ordinal,p.content_version,c.ciphertext FROM learning_studio_section_revisions p JOIN learning_studio_section_revisions c ON c.artifact_id=p.artifact_id AND c.version=p.content_version AND c.section_id=p.section_id WHERE p.artifact_id=$1 AND p.version=$2 AND p.ordinal>$3 ORDER BY p.ordinal LIMIT $4`, r.ArtifactID, r.Version, after, limit+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var id string
		var ordinal int
		var version int64
		var raw []byte
		if err = rows.Scan(&id, &ordinal, &version, &raw); err != nil {
			rows.Close()
			return page, err
		}
		if len(page.Items) == limit {
			page.NextCursor = strconv.Itoa(page.Items[len(page.Items)-1].Ordinal)
			break
		}
		if len(raw) < s.aead.NonceSize() {
			rows.Close()
			return page, ErrUnavailable
		}
		plain, e := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], sectionAAD(r, id, version))
		if e != nil {
			rows.Close()
			return page, ErrUnavailable
		}
		var section StudioSection
		e = json.Unmarshal(plain, &section)
		clear(plain)
		if e != nil {
			rows.Close()
			return page, e
		}
		page.Items = append(page.Items, section)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return page, err
	}
	for _, section := range page.Items {
		if err = s.studioSourcesTx(ctx, tx, token, section.Body); err != nil {
			return StudioSectionPage{}, err
		}
	}
	return page, nil
}
func (s *Store) StudioSections(ctx context.Context, actor identity.Credential, id string, version int64, after, limit int) (StudioSectionPage, error) {
	if version < 1 || after < 0 || limit < 1 || limit > 32 {
		return StudioSectionPage{}, ErrInvalid
	}
	tx, g, err := s.studioBegin(ctx, actor, false)
	if err != nil {
		return StudioSectionPage{}, err
	}
	defer tx.Rollback(context.Background())
	r, err := s.studioRead(ctx, tx, id, version, g, actor.TokenID)
	if err != nil {
		return StudioSectionPage{}, err
	}
	if r.Body.Schema != 2 {
		return StudioSectionPage{}, ErrUnsupported
	}
	page, err := s.studioSectionsTx(ctx, tx, r, actor.TokenID, after, limit)
	if err != nil {
		return page, err
	}
	return page, tx.Commit(ctx)
}

func (s *Store) StudioSectionCitation(ctx context.Context, actor identity.Credential, id string, version int64, citationID string) (StudioCitation, error) {
	parts := strings.Split(citationID, ":")
	if len(parts) != 2 || len(parts[0]) > 80 || version < 1 {
		return StudioCitation{}, ErrInvalid
	}
	tx, g, err := s.studioBegin(ctx, actor, false)
	if err != nil {
		return StudioCitation{}, err
	}
	defer tx.Rollback(context.Background())
	r, err := s.studioRead(ctx, tx, id, version, g, actor.TokenID)
	if err != nil {
		return StudioCitation{}, err
	}
	var ordinal int
	err = tx.QueryRow(ctx, `SELECT ordinal FROM learning_studio_section_revisions WHERE artifact_id=$1 AND version=$2 AND section_id=$3`, id, version, parts[0]).Scan(&ordinal)
	if errors.Is(err, pgx.ErrNoRows) {
		return StudioCitation{}, ErrNotFound
	}
	if err != nil {
		return StudioCitation{}, err
	}
	page, err := s.studioSectionsTx(ctx, tx, r, actor.TokenID, ordinal-1, 1)
	if err != nil {
		return StudioCitation{}, err
	}
	for _, citation := range page.Items[0].Body.Citations {
		if citation.ID == parts[1] {
			citation.ID = citationID
			return citation, tx.Commit(ctx)
		}
	}
	return StudioCitation{}, ErrNotFound
}
