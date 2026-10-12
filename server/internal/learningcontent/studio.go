package learningcontent

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/google/uuid"
)

// StudioBody is encrypted as a whole, including title, provenance and generation inputs.
// Reserved generation fields are owner-written only; the note API cannot set them.
type StudioBody struct {
	Schema           int                       `json:"schema"`
	Title            string                    `json:"title"`
	Markdown         string                    `json:"markdown"`
	SourceManifest   *knowledge.SourceManifest `json:"source_manifest,omitempty"`
	Citations        []StudioCitation          `json:"citations"`
	Model            string                    `json:"model,omitempty"`
	InputFingerprint string                    `json:"input_fingerprint,omitempty"`
	Coverage         string                    `json:"coverage,omitempty"`
	Guide            *StudioGuide              `json:"guide,omitempty"`
	Brief            *StudioBrief              `json:"brief,omitempty"`
}
type StudioCitation struct {
	ID             string                   `json:"id"`
	Source         knowledge.SourceItem     `json:"source"`
	Fragment       knowledge.SourceFragment `json:"fragment"`
	DocumentSHA256 string                   `json:"document_sha256"`
	SliceSHA256    string                   `json:"slice_sha256"`
}
type StudioRevision struct {
	ArtifactID    string     `json:"artifact_id"`
	SpaceID       string     `json:"learning_space_id"`
	GoalID        *string    `json:"goal_id"`
	Kind          string     `json:"kind"`
	Version       int64      `json:"version"`
	Generation    int64      `json:"privacy_generation"`
	ActorDeviceID string     `json:"actor_device_id"`
	CreatedAt     time.Time  `json:"created_at"`
	Body          StudioBody `json:"body"`
}
type StudioSave struct {
	OperationID     string  `json:"operation_id"`
	ExpectedVersion int64   `json:"expected_version"`
	Generation      int64   `json:"privacy_generation"`
	Kind            string  `json:"kind"`
	GoalID          *string `json:"goal_id"`
	Title           string  `json:"title"`
	Markdown        string  `json:"markdown"`
}

func (c StudioSave) Validate(id string) error {
	if uuid.Validate(c.OperationID) != nil || c.Generation < 1 || c.Kind != "note" || c.ExpectedVersion < 0 || (id == "") != (c.ExpectedVersion == 0) || id != "" && uuid.Validate(id) != nil || c.GoalID != nil && uuid.Validate(*c.GoalID) != nil {
		return ErrInvalid
	}
	if !utf8.ValidString(c.Title) || !utf8.ValidString(c.Markdown) || strings.TrimSpace(c.Title) == "" || utf8.RuneCountInString(c.Title) > 200 || strings.ContainsAny(c.Title, "\x00\r\n") || strings.TrimSpace(c.Markdown) == "" || len(c.Markdown) > 64000 || strings.ContainsRune(c.Markdown, 0) {
		return ErrInvalid
	}
	return nil
}
func studioAAD(r StudioRevision) []byte {
	goal := ""
	if r.GoalID != nil {
		goal = *r.GoalID
	}
	return []byte(fmt.Sprintf("studio:v1:%s:%d:%d:%s:%s:%s:%s", r.ArtifactID, r.Version, r.Generation, r.SpaceID, goal, r.Kind, r.ActorDeviceID))
}

type StudioItem struct {
	ArtifactID string    `json:"artifact_id"`
	GoalID     *string   `json:"goal_id"`
	Kind       string    `json:"kind"`
	Version    int64     `json:"version"`
	Title      string    `json:"title"`
	UpdatedAt  time.Time `json:"updated_at"`
}
type StudioPage struct {
	Items      []StudioItem `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}
