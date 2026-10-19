package learningcontent

import (
	"context"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/jackc/pgx/v5"
)

// Keep the schema 1 quiz contract. References become artifact-local IDs while
// immutable section rows retain unit ID -> question ordinal independently of jobs.
func (s *Store) publishStudioQuizJobTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, j StudioJob, operation string) (StudioRevision, error) {
	quiz := *j.Input.Quiz
	quiz.Questions = []StudioQuestion{}
	body := StudioBody{Schema: 1, Title: j.Plan.Title, SourceManifest: &j.Input.Manifest, Citations: []StudioCitation{}, Coverage: j.Plan.Coverage, InputFingerprint: j.Plan.InputFingerprint, Summary: j.Input.Summary, Quiz: &quiz}
	page, err := s.StudioUnitsTx(ctx, tx, j, 0, 8)
	if err != nil {
		return StudioRevision{}, err
	}
	for _, unit := range page.Items {
		if unit.Status != "succeeded" {
			return StudioRevision{}, ErrConflict
		}
		if err = s.VerifyStudioUnitTx(ctx, tx, actor.TokenID, unit); err != nil {
			return StudioRevision{}, err
		}
		question := unit.Body.Quiz.Questions[0]
		question.Citations = make([]string, 0, len(question.Citations))
		refs := map[string]string{}
		for _, citation := range unit.Body.Citations {
			old := citation.ID
			citation.ID = strconv.Itoa(len(body.Citations) + 1)
			refs[old] = citation.ID
			body.Citations = append(body.Citations, citation)
		}
		for _, id := range unit.Body.Quiz.Questions[0].Citations {
			question.Citations = append(question.Citations, refs[id])
		}
		quiz.Questions = append(quiz.Questions, question)
		body.Model = unit.Body.Model
		if unit.Body.Coverage == "partial" {
			body.Coverage = "partial"
		}
	}
	target := StudioTarget{Generation: j.Generation, Kind: "quiz", ExpectedVersion: j.Input.ExpectedArtifactVersion, GoalID: &j.Input.QuizGoalID}
	if j.Input.ArtifactID != "" {
		target.ArtifactID = &j.Input.ArtifactID
	}
	r, err := s.StudioPublishTx(ctx, tx, actor, operation, target, body)
	if err != nil {
		return r, err
	}
	for _, unit := range page.Items {
		if err = s.insertStudioSectionTx(ctx, tx, r, StudioSection{ID: unit.ID, SectionID: unit.SectionID, Ordinal: unit.Ordinal, Body: *unit.Body}); err != nil {
			return r, err
		}
	}
	return r, nil
}
