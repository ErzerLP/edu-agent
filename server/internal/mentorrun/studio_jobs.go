package mentorrun

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type StudioJobCreate struct {
	ConversationScope       *learningcontent.StudioConversationScope `json:"conversation_scope,omitempty"`
	OperationID             string                                   `json:"operation_id"`
	Generation              int64                                    `json:"privacy_generation"`
	Kind                    string                                   `json:"kind"`
	SourceSelection         knowledge.SourceSelection                `json:"source_selection"`
	DirectionVersion        int64                                    `json:"direction_version"`
	Focus                   string                                   `json:"focus,omitempty"`
	QuestionCount           *int                                     `json:"question_count,omitempty"`
	Difficulty              string                                   `json:"difficulty,omitempty"`
	ArtifactID              string                                   `json:"artifact_id,omitempty"`
	ExpectedArtifactVersion int64                                    `json:"expected_artifact_version,omitempty"`
}

func (c StudioJobCreate) Validate() error {
	if c.ConversationScope != nil && ((c.Kind != "note" && c.Kind != "quiz") || validateStudioConversationScope(*c.ConversationScope) != nil) {
		return ErrInvalid
	}
	if !validID(c.OperationID) || c.Generation < 1 || (c.Kind != "guide" && c.Kind != "brief" && c.Kind != "note" && c.Kind != "quiz") || c.SourceSelection.Validate() != nil || c.SourceSelection.Mode != "items" || len(c.SourceSelection.Items) == 0 || c.DirectionVersion < 0 || c.Focus != "" && !validText(c.Focus, 2000) || c.ExpectedArtifactVersion < 0 || (c.ArtifactID == "") != (c.ExpectedArtifactVersion == 0) || c.ArtifactID != "" && !validID(c.ArtifactID) {
		return ErrInvalid
	}
	if c.Kind == "quiz" {
		if c.DirectionVersion < 1 || c.QuestionCount != nil && (*c.QuestionCount < 1 || *c.QuestionCount > 8) || c.Difficulty != "" && !studioQuizDifficulty(c.Difficulty) {
			return ErrInvalid
		}
	} else if c.QuestionCount != nil || c.Difficulty != "" {
		return ErrInvalid
	}
	return nil
}

type StudioPlanCommand struct {
	OperationID     string   `json:"operation_id"`
	ExpectedVersion int64    `json:"expected_version"`
	PlanRevision    int64    `json:"plan_revision"`
	Text            string   `json:"text,omitempty"`
	DeleteIDs       []string `json:"delete_ids,omitempty"`
	OrderedIDs      []string `json:"ordered_ids,omitempty"`
	Kind            string   `json:"kind,omitempty"`
}

func (c StudioPlanCommand) Validate(action string) error {
	if !validID(c.OperationID) || c.ExpectedVersion < 1 || c.PlanRevision < 0 {
		return ErrInvalid
	}
	switch action {
	case "message":
		if !validText(c.Text, 4000) || len(c.DeleteIDs) > 0 || len(c.OrderedIDs) > 0 || c.Kind != "" {
			return ErrInvalid
		}
	case "patch":
		if c.Text != "" || c.Kind != "" || len(c.DeleteIDs)+len(c.OrderedIDs) == 0 || len(c.DeleteIDs) > 24 || len(c.OrderedIDs) > 24 {
			return ErrInvalid
		}
	case "confirm":
		if c.Text != "" || c.Kind != "" || len(c.DeleteIDs)+len(c.OrderedIDs) > 0 {
			return ErrInvalid
		}
	case "retry", "delete", "stop", "resume", "save_as_new", "revise":
		if c.Kind != action || c.Text != "" || len(c.DeleteIDs)+len(c.OrderedIDs) > 0 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type StudioPlanStage struct {
	JobID           string `json:"job_id"`
	OperationID     string `json:"operation_id"`
	ExpectedVersion int64  `json:"expected_version"`
}
type StudioFocus struct {
	SuggestedFocus []string `json:"suggested_focus"`
	Coverage       string   `json:"coverage"`
}

func (s *Service) studioJobBegin(ctx context.Context, actor identity.Credential, write bool) (pgx.Tx, int64, error) {
	return s.beginStudioJob(ctx, actor, write, false)
}

func (s *Service) beginStudioJob(ctx context.Context, actor identity.Credential, write, creatingQuiz bool) (pgx.Tx, int64, error) {
	if !s.CanSave() || !s.studio.Available() {
		return nil, 0, ErrStorage
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	g, err := gates(ctx, tx, write)
	if err == nil {
		err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, write)
	}
	if err == nil {
		_, err = privacy.LockOwnerRead(ctx, tx, privacy.OwnerTutoring)
	}
	if err == nil {
		err = sourceActor(ctx, tx, actor.TokenID)
	}
	if err == nil {
		_, err = privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	}
	if err == nil && creatingQuiz {
		// EnsureTx needs this exclusive lock. Take it before goalGate's shared
		// lock so concurrent creates cannot deadlock while upgrading their locks.
		_, err = tx.Exec(ctx, `SELECT id FROM learning_spaces WHERE id=$1 FOR UPDATE`, learningspace.Scope(ctx))
	}
	if err == nil && write {
		_, err = goalGate(ctx, tx, learningspace.Scope(ctx), uuid.Nil.String(), 1)
	}
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, 0, err
	}
	return tx, g, nil
}
func (s *Service) studioJobSourcesTx(ctx context.Context, tx pgx.Tx, j learningcontent.StudioJob, token string) error {
	if err := s.studioConversationGateTx(ctx, tx, j, token); err != nil {
		return err
	}
	g, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerTutoring)
	if err != nil {
		return err
	}
	if g != j.Input.TutoringGeneration {
		return ErrInactive
	}
	return knowledgepg.New(s.pool).ValidateSourceManifestTx(ctx, tx, j.Input.Manifest)
}
func (s *Service) StudioFocus(ctx context.Context, actor identity.Credential) (StudioFocus, error) {
	tx, _, err := s.studioJobBegin(ctx, actor, false)
	if err != nil {
		return StudioFocus{}, err
	}
	defer tx.Rollback(context.Background())
	d, err := s.directions.ReadTx(ctx, tx, learningspace.Scope(ctx), 0)
	if err != nil {
		return StudioFocus{}, err
	}
	state, err := studioLearningStateTx(ctx, tx, d.ID)
	if err != nil {
		return StudioFocus{}, err
	}
	return StudioFocus{state.SuggestedFocus, state.Coverage}, tx.Commit(ctx)
}
func (s *Service) CreateStudioJob(ctx context.Context, actor identity.Credential, c StudioJobCreate) (learningcontent.StudioJob, error) {
	if c.Validate() != nil {
		return learningcontent.StudioJob{}, ErrInvalid
	}
	tx, g, err := s.beginStudioJob(ctx, actor, true, c.Kind == "quiz")
	if err != nil {
		return learningcontent.StudioJob{}, err
	}
	defer tx.Rollback(context.Background())
	if g != c.Generation {
		return learningcontent.StudioJob{}, ErrInactive
	}
	hash := studioJobHash("create", learningspace.Scope(ctx), "", c)
	if old, found, e := s.studio.StudioJobOperationTx(ctx, tx, actor.Device.ID, c.OperationID, hash, g); e != nil || found {
		if e == nil {
			e = s.studioJobSourcesTx(ctx, tx, old, actor.TokenID)
		}
		return old, e
	}
	d, err := s.directions.ReadTx(ctx, tx, learningspace.Scope(ctx), 0)
	if err != nil {
		return learningcontent.StudioJob{}, err
	}
	if d.Version != c.DirectionVersion {
		return learningcontent.StudioJob{}, learningcontent.ErrConflict
	}
	manifest, err := knowledgepg.New(s.pool).FreezeSourceSelectionTx(ctx, tx, c.SourceSelection)
	if err != nil {
		return learningcontent.StudioJob{}, err
	}
	state, err := studioLearningStateTx(ctx, tx, d.ID)
	if err != nil {
		return learningcontent.StudioJob{}, err
	}
	tutoringGeneration, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerTutoring)
	if err != nil {
		return learningcontent.StudioJob{}, err
	}
	j := learningcontent.StudioJob{ID: uuid.NewString(), SpaceID: learningspace.Scope(ctx), DeviceID: actor.Device.ID, Generation: g, Version: 1, Kind: c.Kind, Status: "planning", Messages: []learningcontent.StudioPlanMessage{}, SuggestedFocus: state.SuggestedFocus, Input: learningcontent.StudioJobInput{Manifest: *manifest, DirectionID: d.ID, DirectionVersion: d.Version, Direction: d.Text, LearningState: state, TutoringGeneration: tutoringGeneration, Focus: c.Focus, ArtifactID: c.ArtifactID, ExpectedArtifactVersion: c.ExpectedArtifactVersion}}
	if c.ConversationScope != nil {
		j.Input.Conversation, err = s.freezeStudioConversationTx(ctx, tx, actor, j, *c.ConversationScope)
		if err != nil {
			return j, err
		}
	}
	if c.Kind == "quiz" {
		if !s.CanStart() {
			return j, ErrStorage
		}
		d, err = s.directions.EnsureTx(ctx, tx, actor.Device.ID, j.SpaceID, g, c.DirectionVersion)
		if err != nil {
			return j, err
		}
		model, _, configuration, e := s.settings.MentorClient()
		if e != nil || model == nil {
			return j, ErrModel
		}
		policy := automaticAssessmentPolicy(configuration)
		j.Input.QuestionCount, j.Input.Difficulty = 3, c.Difficulty
		if c.QuestionCount != nil {
			j.Input.QuestionCount = *c.QuestionCount
		}
		j.Input.QuizGoalID = d.GoalID
		j.Input.Quiz = &learningcontent.StudioQuiz{GoalVersion: d.GoalVersion, Direction: &learningcontent.StudioQuizDirection{ID: d.ID, Version: d.Version, GoalRevisionID: d.GoalRevisionID}, Assessment: &policy}
	}
	if c.ArtifactID != "" {
		var kind string
		var version int64
		err = tx.QueryRow(ctx, `SELECT kind,latest_version FROM learning_studio_artifacts WHERE id=$1 AND space_id=$2 AND privacy_generation=$3 FOR SHARE`, c.ArtifactID, j.SpaceID, g).Scan(&kind, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return j, learningcontent.ErrNotFound
		}
		if err != nil {
			return j, err
		}
		if kind != c.Kind || version != c.ExpectedArtifactVersion {
			return j, learningcontent.ErrConflict
		}
		if c.Kind == "quiz" {
			if err = s.studio.StudioTargetTx(ctx, tx, actor, learningcontent.StudioTarget{ArtifactID: &c.ArtifactID, ExpectedVersion: c.ExpectedArtifactVersion, Generation: g, Kind: c.Kind, GoalID: &j.Input.QuizGoalID}); err != nil {
				return j, err
			}
		}
	}
	input, _ := json.Marshal(j.Input)
	j.Input.Fingerprint = knowledge.SourceTextHash(string(input))
	if err = s.quota(ctx, tx, 512<<10); err != nil {
		return j, err
	}
	if err = s.studio.InsertStudioJobTx(ctx, tx, &j); err != nil {
		return j, err
	}
	if c.ConversationScope != nil {
		if _, err = tx.Exec(ctx, `UPDATE learning_studio_jobs SET conversation_id=$2 WHERE id=$1`, j.ID, c.ConversationScope.ConversationID); err != nil {
			return j, err
		}
	}
	if err = s.enqueueStudioPlanTx(ctx, tx, actor, &j, c.OperationID); err != nil {
		return j, err
	}
	if err = s.studio.SaveStudioJobTx(ctx, tx, &j); err != nil {
		return j, err
	}
	if err = s.studio.RecordStudioJobOperationTx(ctx, tx, j, actor.Device.ID, c.OperationID, hash); err != nil {
		return j, err
	}
	return j, tx.Commit(ctx)
}
func studioJobHash(action, space, id string, c any) string {
	h := requestHash("studio-job:v1:"+action, space, id, c)
	return hex.EncodeToString(h[:])
}

func (s *Service) enqueueStudioPlanTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, j *learningcontent.StudioJob, operation string) error {
	return s.enqueueStudioStageTx(ctx, tx, actor, j, operation, nil)
}
func (s *Service) enqueueStudioStageTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, j *learningcontent.StudioJob, operation string, content *StudioContentStage) error {
	model, _, configuration, err := s.settings.MentorClient()
	if err != nil || model == nil {
		return ErrModel
	}
	if content != nil && j.Input.Configuration != "" && j.Input.Configuration != configuration {
		return ErrInactive
	}
	if j.Input.Configuration == "" {
		j.Input.Configuration = configuration
		j.Input.Destination = s.studioDestination()
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_mentor_processes(id,live_until) VALUES($1,clock_timestamp()+interval '30 seconds') ON CONFLICT(id) DO UPDATE SET live_until=excluded.live_until`, s.process); err != nil {
		return err
	}
	var session string
	err = tx.QueryRow(ctx, `INSERT INTO learning_mentor_sessions(id,device_id,space_id,goal_id,privacy_generation,kind,studio_job_id) VALUES($1,$2,$3,$4,$5,'studio',$6) ON CONFLICT(studio_job_id) WHERE studio_job_id IS NOT NULL DO UPDATE SET id=learning_mentor_sessions.id RETURNING id::text`, uuid.NewString(), actor.Device.ID, j.SpaceID, uuid.Nil.String(), j.Generation, j.ID).Scan(&session)
	if err != nil {
		return err
	}
	item := row{Meta: Meta{RunID: uuid.NewString(), SessionID: session, SpaceID: j.SpaceID, GoalID: uuid.Nil.String(), GoalVersion: 1, Generation: j.Generation, Kind: "studio", Status: "queued", Stage: "planning", Saved: true, BodyAvailable: true, ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour), Configuration: configuration}, device: actor.Device.ID, token: actor.TokenID, process: s.process}
	item.body.StudioPlan = &StudioPlanStage{JobID: j.ID, OperationID: operation, ExpectedVersion: j.Version}
	if content != nil {
		item.body.StudioPlan = nil
		item.body.StudioContent = content
		item.Stage = "studio_section"
		if j.Kind == "quiz" {
			item.Stage = "studio_question"
		}
		if content.UnitID == "" {
			item.Stage = "studio_summary"
		}
	}
	item.body.SourceManifest = &j.Input.Manifest
	raw, _ := json.Marshal(item.Meta)
	if _, err = tx.Exec(ctx, `INSERT INTO learning_mentor_runs(id,session_id,device_id,token_id,space_id,goal_id,goal_version,privacy_generation,state,process_id,expires_at,studio_job_id,studio_stage_operation) VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$9,$10,$11,$12)`, item.RunID, session, actor.Device.ID, actor.TokenID, j.SpaceID, uuid.Nil.String(), j.Generation, raw, s.process, item.ExpiresAt, j.ID, operation); err != nil {
		return err
	}
	if err = s.save(ctx, tx, &item, "accepted"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_mentor_sessions SET current_run_id=$2 WHERE id=$1`, session, item.RunID); err != nil {
		return err
	}
	j.RunID = item.RunID
	j.Status = "planning"
	if content != nil {
		j.Status = "generating"
		if content.UnitID == "" {
			j.Status = "summarizing"
		}
	}
	j.Reason = ""
	return nil
}
func (s *Service) StudioJob(ctx context.Context, actor identity.Credential, id string) (learningcontent.StudioJob, error) {
	if !validID(id) {
		return learningcontent.StudioJob{}, ErrInvalid
	}
	tx, g, err := s.studioJobBegin(ctx, actor, false)
	if err != nil {
		return learningcontent.StudioJob{}, err
	}
	defer tx.Rollback(context.Background())
	j, err := s.studio.StudioJobTx(ctx, tx, id, g, false)
	if err != nil {
		return j, err
	}
	if err = s.studioJobSourcesTx(ctx, tx, j, actor.TokenID); err != nil {
		return learningcontent.StudioJob{}, err
	}
	// Lease expiry and process restart are observable even before the next worker
	// claim. Keep the last valid plan and let an explicit retry create a new run.
	if (j.Status == "planning" || j.Status == "queued" || j.Status == "generating" || j.Status == "summarizing") && j.RunID != "" {
		var status, reason string
		var expired bool
		err = tx.QueryRow(ctx, `SELECT state->>'status',COALESCE(state->>'reason',''),expires_at<=clock_timestamp() FROM learning_mentor_runs WHERE id=$1`, j.RunID).Scan(&status, &reason, &expired)
		if err != nil {
			return j, err
		}
		if terminal(status) || expired {
			j.Status = "failed"
			j.Reason = reason
			if expired {
				j.Reason = "lease_expired"
			}
		}
	}
	return j, tx.Commit(ctx)
}
func (s *Service) StudioJobs(ctx context.Context, actor identity.Credential, cursor string, limit int) (learningcontent.StudioJobPage, error) {
	page := learningcontent.StudioJobPage{Items: []learningcontent.StudioJob{}}
	after := uuid.Nil.String()
	if cursor != "" {
		var c struct{ Space, ID string }
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || len(cursor) > 1024 || json.Unmarshal(raw, &c) != nil || c.Space != learningspace.Scope(ctx) || !validID(c.ID) {
			return page, ErrInvalid
		}
		after = c.ID
	}
	if limit < 1 || limit > 100 {
		return page, ErrInvalid
	}
	tx, g, err := s.studioJobBegin(ctx, actor, false)
	if err != nil {
		return page, err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, `SELECT id::text FROM learning_studio_jobs WHERE space_id=$1 AND privacy_generation=$2 AND id>$3 ORDER BY id LIMIT $4`, learningspace.Scope(ctx), g, after, limit+1)
	if err != nil {
		return page, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return page, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(ids) > limit {
		ids = ids[:limit]
		raw, _ := json.Marshal(struct{ Space, ID string }{learningspace.Scope(ctx), ids[limit-1]})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	for _, id := range ids {
		j, e := s.studio.StudioJobTx(ctx, tx, id, g, false)
		if e != nil {
			return page, e
		}
		if e = s.studioJobSourcesTx(ctx, tx, j, actor.TokenID); e != nil {
			if !sourceUnavailable(e) && !errors.Is(e, ErrInactive) {
				return page, e
			}
			j.Status = "invalidated"
			j.Plan = nil
			j.Messages = []learningcontent.StudioPlanMessage{}
			j.SuggestedFocus = []string{}
			j.Reason = "source_unavailable"
			j.Result = nil
		}
		page.Items = append(page.Items, j)
	}
	return page, tx.Commit(ctx)
}
