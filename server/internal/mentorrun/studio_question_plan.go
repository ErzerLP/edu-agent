package mentorrun

import (
	"context"
	"encoding/json"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/jackc/pgx/v5"
)

func studioQuizDifficulty(value string) bool {
	return value == "easy" || value == "medium" || value == "hard"
}

func studioQuestionPlanSchema() json.RawMessage {
	var schema map[string]any
	_ = json.Unmarshal(studioPlanSchema, &schema)
	sections := schema["properties"].(map[string]any)["sections"].(map[string]any)
	sections["maxItems"] = 8
	item := sections["items"].(map[string]any)
	item["required"] = []string{"id", "topic", "objective", "chapters", "question_type", "difficulty"}
	fields := item["properties"].(map[string]any)
	fields["question_type"] = map[string]any{"type": "string", "enum": []string{"short_answer", "explanation"}}
	fields["difficulty"] = map[string]any{"type": "string", "enum": []string{"easy", "medium", "hard"}}
	raw, _ := json.Marshal(schema)
	return raw
}

// Exactly one unit per question. Its bounded source ranges are fixed before the
// first stage is dispatched; long chapters do not silently create extra questions.
func (s *Service) prepareStudioQuestionsTx(ctx context.Context, tx pgx.Tx, j *learningcontent.StudioJob) error {
	store := knowledgepg.New(s.pool)
	for index, question := range j.Plan.Sections {
		u := learningcontent.StudioUnit{ID: question.ID, SectionID: question.ID, Ordinal: index + 1, Status: "pending", Topic: question.Topic, Objective: question.Objective, QuestionType: question.QuestionType, Difficulty: question.Difficulty}
		for _, chapter := range question.Chapters {
			q := knowledge.SourceSliceRequest{SourceIdentity: chapter.SourceIdentity, NodeID: chapter.NodeID, NodeRevisionID: chapter.NodeRevisionID, Start: chapter.Range.Start, End: min(chapter.Range.End, chapter.Range.Start+j.Plan.SliceBytes/len(question.Chapters))}
			source, err := store.ReadSourceSliceTx(ctx, tx, j.Input.Manifest, q)
			if err != nil {
				return err
			}
			q.Start, q.End = source.Fragments[0].Range.Start, source.Fragments[0].Range.End
			u.Slices = append(u.Slices, q)
		}
		raw, _ := json.Marshal(struct {
			Plan   string
			Slices []knowledge.SourceSliceRequest
		}{j.Plan.InputFingerprint, u.Slices})
		u.InputFingerprint = knowledge.SourceTextHash(string(raw))
		if err := s.studio.SaveStudioUnitTx(ctx, tx, *j, u); err != nil {
			return err
		}
	}
	return s.studio.StudioProgressTx(ctx, tx, j)
}
