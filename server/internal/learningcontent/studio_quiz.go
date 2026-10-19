package learningcontent

import (
	"encoding/json"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
)

func StudioQuestionSchema() json.RawMessage {
	var schema map[string]any
	_ = json.Unmarshal(StudioSchema("quiz"), &schema)
	questions := schema["properties"].(map[string]any)["questions"].(map[string]any)
	questions["maxItems"] = 1
	raw, _ := json.Marshal(schema)
	return raw
}

func DecodeStudioQuestion(raw []byte, manifest *knowledge.SourceManifest, sources []knowledge.SourceDetail) (StudioBody, error) {
	body, err := DecodeStudioCandidate("quiz", raw, manifest, sources)
	if err == nil && len(body.Quiz.Questions) != 1 {
		err = ErrInvalid
	}
	return body, err
}

type StudioQuiz struct {
	GoalVersion int64                `json:"goal_version"`
	Questions   []StudioQuestion     `json:"questions"`
	Direction   *StudioQuizDirection `json:"direction,omitempty"`
	Assessment  *AssessmentPolicy    `json:"assessment_policy,omitempty"`
}
type StudioQuizDirection struct {
	ID             string `json:"id"`
	Version        int64  `json:"version"`
	GoalRevisionID string `json:"goal_revision_id"`
}
type StudioRubricItem struct {
	ID        string `json:"id"`
	Criterion string `json:"criterion"`
}
type StudioQuestion struct {
	Prompt      string             `json:"prompt"`
	Rubric      []StudioRubricItem `json:"rubric"`
	AllowedHelp []string           `json:"allowed_help"`
	Citations   []string           `json:"citations"`
}
type studioCandidateQuestion struct {
	Prompt      string             `json:"prompt"`
	Rubric      []StudioRubricItem `json:"rubric"`
	AllowedHelp []string           `json:"allowed_help"`
	Citations   []studioReference  `json:"citations"`
}

func validStudioQuestion(prompt string, rubric []StudioRubricItem, help []string) bool {
	if !studioText(prompt, 1500) || len(rubric) < 1 || len(rubric) > 10 || len(help) < 1 || len(help) > 4 {
		return false
	}
	seen := map[string]bool{}
	for _, item := range rubric {
		if !studioText(item.ID, 100) || !studioText(item.Criterion, 1000) || seen[item.ID] {
			return false
		}
		seen[item.ID] = true
	}
	seen = map[string]bool{}
	for _, h := range help {
		if seen[h] || h != "none" && h != "hint" && h != "scaffold" && h != "answer_revealed" {
			return false
		}
		seen[h] = true
	}
	return true
}
func studioQuizSchema(ref any) any {
	text := func(max int) any { return map[string]any{"type": "string", "minLength": 1, "maxLength": max} }
	item := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"id", "criterion"}, "properties": map[string]any{"id": text(100), "criterion": text(1000)}}
	question := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"prompt", "rubric", "allowed_help", "citations"}, "properties": map[string]any{
		"prompt":       text(1500),
		"rubric":       map[string]any{"type": "array", "minItems": 1, "maxItems": 10, "items": item},
		"allowed_help": map[string]any{"type": "array", "minItems": 1, "maxItems": 4, "items": map[string]any{"type": "string", "enum": []string{"none", "hint", "scaffold", "answer_revealed"}}},
		"citations":    map[string]any{"type": "array", "minItems": 1, "maxItems": 10, "items": ref},
	}}
	return map[string]any{"type": "array", "minItems": 1, "maxItems": 8, "items": question}
}
