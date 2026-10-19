package mentorrun

import (
	"context"
	"errors"
	"slices"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/jackc/pgx/v5"
)

// Public card projection deliberately excludes task request IDs, token IDs and
// the internal policy. Learning remains the answer/assessment authority.
type StudyExerciseView struct {
	PracticeView
	ConversationID string `json:"conversation_id"`
	RunID          string `json:"run_id"`
}
type PracticeView struct {
	CanContinue    bool                   `json:"can_continue"`
	ID             string                 `json:"id"`
	SpaceID        string                 `json:"space_id"`
	Generation     int64                  `json:"privacy_generation"`
	SessionID      string                 `json:"session_id"`
	ActivityID     string                 `json:"activity_id"`
	ArtifactID     string                 `json:"artifact_id"`
	ContentVersion int64                  `json:"content_version"`
	SessionVersion int64                  `json:"session_version"`
	Prompt         string                 `json:"prompt"`
	AllowedHelp    []learning.HelpLevel   `json:"allowed_help"`
	HelpVersion    int64                  `json:"help_version"`
	HelpFloor      learning.HelpLevel     `json:"help_floor,omitempty"`
	Deferred       bool                   `json:"deferred"`
	AttemptID      string                 `json:"attempt_id,omitempty"`
	Feedback       *learning.FeedbackView `json:"feedback,omitempty"`
	Assessment     *StudyAssessmentView   `json:"assessment,omitempty"`
}
type StudyAssessmentView struct {
	Status        string `json:"status"`
	Version       int64  `json:"version"`
	Reason        string `json:"reason,omitempty"`
	ResultUnknown bool   `json:"result_unknown"`
	CanResume     bool   `json:"can_resume"`
	Configuration string `json:"configuration"`
	Destination   string `json:"destination"`
}
type StudyExerciseCommand struct {
	OperationID string `json:"operation_id"`
	Kind        string `json:"kind"`
}
type StudyHelpRequest struct {
	OperationID     string                    `json:"operation_id"`
	SourceSelection knowledge.SourceSelection `json:"source_selection"`
}
type StudyAssessmentResume struct {
	OperationID     string `json:"operation_id"`
	ExpectedVersion int64  `json:"expected_version"`
	Configuration   string `json:"configuration"`
	RequestBudget   int    `json:"request_budget"`
	TokenBudget     int    `json:"token_budget"`
}

var ErrExerciseSources = errors.New("exercise_source_selection_mismatch")

func (s *Service) withStudyExercise(ctx context.Context, actor identity.Credential, id string, write bool, work func(pgx.Tx, learningcontent.Exercise, string, string, bool) error) error {
	if !s.CanStart() || !validID(id) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	g, err := gates(ctx, tx, write)
	if err != nil {
		return err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, write); err != nil {
		return err
	}
	var conversation, run string
	var deferred bool
	err = tx.QueryRow(ctx, `SELECT t.conversation_id,t.run_id,t.exercise_deferred FROM learning_tutor_turns t JOIN learning_tutor_conversations c ON c.id=t.conversation_id WHERE t.exercise_id=$1 AND c.device_id=$2 AND c.space_id=$3 AND c.privacy_generation=$4 AND NOT c.deleted AND c.mode='study' FOR SHARE OF c`, id, actor.Device.ID, learningspace.Scope(ctx), g).Scan(&conversation, &run, &deferred)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	e, err := s.starter.Content.ExerciseTx(ctx, tx, actor, id)
	if err != nil {
		return err
	}
	if err = work(tx, e, conversation, run, deferred); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) ReadStudyExercise(ctx context.Context, actor identity.Credential, id string, send func(StudyExerciseView) error) error {
	return s.withStudyExercise(ctx, actor, id, false, func(tx pgx.Tx, e learningcontent.Exercise, conversation, run string, deferred bool) error {
		v, err := s.exerciseViewTx(ctx, tx, actor, e, deferred)
		if err != nil {
			return err
		}
		return send(StudyExerciseView{PracticeView: v, ConversationID: conversation, RunID: run})
	})
}

func (s *Service) exerciseViewTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, e learningcontent.Exercise, deferred bool) (PracticeView, error) {
	content, err := s.starter.Content.ReadTx(ctx, tx, actor, e.ContentArtifactID, e.ContentVersion)
	if err != nil {
		return PracticeView{}, err
	}
	store := s.starter.Learning.WithinTx(tx)
	a, err := store.LoadSessionAuthority(ctx, e.SessionID)
	if err != nil {
		return PracticeView{}, err
	}
	activity, err := store.LoadActivity(ctx, e.ActivityID)
	if err != nil {
		return PracticeView{}, err
	}
	v := PracticeView{ID: e.ID, SpaceID: e.SpaceID, Generation: e.Generation, SessionID: e.SessionID, ActivityID: e.ActivityID, ArtifactID: content.ArtifactID, ContentVersion: content.Version, SessionVersion: a.Session.AggregateVer, Prompt: activity.Prompt, AllowedHelp: activity.AllowedHelp, HelpVersion: e.Help.Version, HelpFloor: e.Help.Level, Deferred: deferred}
	attempt, err := s.studyExerciseAttemptTx(ctx, tx, e)
	if err != nil {
		return PracticeView{}, err
	}
	if attempt != "" {
		v.AttemptID = attempt
		feedback, err := store.Feedback(ctx, v.AttemptID, false)
		if err != nil {
			return PracticeView{}, err
		}
		v.Feedback = &feedback
		if len(feedback.Decisions) > 0 {
			actions, _, err := learning.WorkItemActions(a.Session.State, learning.WorkItemActionContext{AssessmentDisposition: feedback.Decisions[len(feedback.Decisions)-1].Disposition})
			if err != nil {
				return PracticeView{}, err
			}
			v.CanContinue = slices.Contains(actions, tutoring.ActionAcknowledgeFeedback)
		}
		// Card reads are learning facts. The caller has already checked the
		// association, source access and privacy generation; task continuation
		// remains bound to the submitting device in ResumeAssessment.
		task, err := store.AssessmentTaskTx(ctx, tx, v.AttemptID)
		if err != nil {
			return PracticeView{}, err
		}
		if task.ExerciseID != e.ID || task.SessionID != e.SessionID {
			return PracticeView{}, ErrConflict
		}
		_, _, configuration, _ := s.settings.MentorClient()
		v.Assessment = &StudyAssessmentView{Status: "queued", Configuration: configuration, Destination: s.settings.View().EffectiveMentor.Endpoint}
		if task.FailureReason != "" {
			v.Assessment.Status = "failed"
			v.Assessment.Reason = task.FailureReason
		}
		if task.RunID != "" {
			item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1 AND space_id=$2 AND device_id=$3 AND privacy_generation=$4`, task.RunID, e.SpaceID, task.DeviceID, e.Generation))
			if err != nil {
				return PracticeView{}, err
			}
			m := item.Meta
			v.Assessment.Status = m.Status
			v.Assessment.Version = m.Version
			v.Assessment.Reason = m.Reason
			v.Assessment.ResultUnknown = m.ResultUnknown
			if m.Status == "failed" || m.Status == "paused_budget" {
				recovery, err := store.InspectAssessmentTx(ctx, tx, task)
				if err != nil {
					return PracticeView{}, err
				}
				if recovery.ProposalStatus == "ready" && recovery.OperationStatus == "" {
					v.Assessment.ResultUnknown = false
				}
				v.Assessment.CanResume = task.DeviceID == actor.Device.ID && assessmentCanResume(item, recovery)
			}
		}
	}
	return v, nil
}

func (s *Service) ChangeStudyExercise(ctx context.Context, actor identity.Credential, id string, c StudyExerciseCommand) (Receipt, error) {
	var result Receipt
	if !validID(c.OperationID) || c.Kind != "defer" && c.Kind != "resume" {
		return result, ErrInvalid
	}
	err := s.withStudyExercise(ctx, actor, id, true, func(tx pgx.Tx, e learningcontent.Exercise, conversation, run string, _ bool) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "study-exercise:"+conversation); err != nil {
			return err
		}
		hash := requestHash("study_exercise", learningspace.Scope(ctx), id, c)
		if r, found, err := operation(ctx, tx, actor.Device.ID, c.OperationID, hash); found || err != nil {
			result = r
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learning-aggregate:session:"+e.SessionID); err != nil {
			return err
		}
		attempt, err := s.studyExerciseAttemptTx(ctx, tx, e)
		if err != nil {
			return err
		}
		if attempt != "" {
			return ErrConflict
		}
		if c.Kind == "resume" {
			var other bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_tutor_turns t JOIN learning_exercises e ON e.id=t.exercise_id WHERE t.conversation_id=$1 AND t.exercise_id<>$2 AND NOT t.exercise_deferred AND NOT EXISTS(SELECT 1 FROM learning_attempts a WHERE a.activity_id=e.activity_id))`, conversation, id).Scan(&other); err != nil {
				return err
			}
			if other {
				return ErrConflict
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE learning_tutor_turns SET exercise_deferred=$2 WHERE exercise_id=$1`, id, c.Kind == "defer"); err != nil {
			return err
		}
		item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1`, run))
		if err != nil {
			return err
		}
		result, err = receipt(ctx, tx, item, c.OperationID, hash)
		return err
	})
	return result, err
}

// Acknowledging feedback clears the active focus. History must keep reading
// the original learning receipt instead of following that mutable focus.
func (s *Service) studyExerciseAttemptTx(ctx context.Context, tx pgx.Tx, e learningcontent.Exercise) (string, error) {
	page, err := s.starter.Learning.WithinTx(tx).ListFeedback(ctx, learning.FeedbackQuery{SessionID: e.SessionID, Status: "all", Page: learning.CursorPageRequest{Limit: 1}})
	if err != nil {
		return "", err
	}
	if len(page.Items) > 0 && page.Items[0].ActivityID == e.ActivityID {
		return page.Items[0].AttemptID, nil
	}
	return "", nil
}

func (s *Service) PublishStudyHelp(ctx context.Context, actor identity.Credential, id string, c StudyHelpRequest) (learningcontent.ExerciseHelp, error) {
	var result learningcontent.ExerciseHelp
	if !validID(c.OperationID) || c.SourceSelection.Validate() != nil {
		return result, ErrInvalid
	}
	err := s.withStudyExercise(ctx, actor, id, true, func(tx pgx.Tx, e learningcontent.Exercise, _, _ string, _ bool) error {
		// Recover an already disclosed hint before checking a changed selection.
		old, err := s.starter.Content.HelpOperationTx(ctx, tx, actor, id, c.OperationID)
		if err == nil {
			result = old
			return nil
		}
		if !errors.Is(err, learningcontent.ErrNotFound) {
			return err
		}
		manifest, err := s.starter.Knowledge.FreezeSourceSelectionTx(ctx, tx, c.SourceSelection)
		if err != nil {
			return err
		}
		if manifest.SHA256 != e.Body.Manifest.SHA256 {
			return ErrExerciseSources
		}
		// A small strategy hint needs no additional model request. The text is
		// disclosed only after the C3a help ledger commits its minimum level.
		result, err = s.starter.Content.PublishHelp(ctx, actor, id, learningcontent.ExerciseHelp{OperationID: c.OperationID, Level: learning.HelpHint, Text: "先找出题目中的关键概念，用自己的话解释，再用所选资料核对理由。"})
		return err
	})
	return result, err
}

func (s *Service) StudyHelpOperation(ctx context.Context, actor identity.Credential, id, operation string) (learningcontent.ExerciseHelp, error) {
	var result learningcontent.ExerciseHelp
	err := s.withStudyExercise(ctx, actor, id, false, func(tx pgx.Tx, _ learningcontent.Exercise, _, _ string, _ bool) error {
		var err error
		result, err = s.starter.Content.HelpOperationTx(ctx, tx, actor, id, operation)
		return err
	})
	return result, err
}

func (s *Service) ResumeStudyAssessment(ctx context.Context, actor identity.Credential, id string, c StudyAssessmentResume) (Receipt, error) {
	var result Receipt
	err := s.withStudyExercise(ctx, actor, id, true, func(tx pgx.Tx, e learningcontent.Exercise, _, _ string, _ bool) error {
		a, err := s.starter.Learning.WithinTx(tx).LoadSessionAuthority(ctx, e.SessionID)
		if err != nil {
			return err
		}
		if a.Session.Context.AttemptID == nil {
			return ErrConflict
		}
		result, err = s.ResumeAssessment(ctx, actor, *a.Session.Context.AttemptID, AssessmentResume{OperationID: c.OperationID, ExpectedVersion: c.ExpectedVersion, Policy: learningcontent.AssessmentPolicy{Configuration: c.Configuration, Requests: c.RequestBudget, Tokens: c.TokenBudget}})
		return err
	})
	return result, err
}

func (s *Service) ReadStudyExerciseCitation(ctx context.Context, actor identity.Credential, id, reference string, send func(SourceCitation) error) error {
	if !validID(reference) {
		return ErrInvalid
	}
	return s.withStudyExercise(ctx, actor, id, false, func(tx pgx.Tx, e learningcontent.Exercise, _, _ string, _ bool) error {
		activity, err := s.starter.Learning.WithinTx(tx).LoadActivity(ctx, e.ActivityID)
		if err != nil {
			return err
		}
		found := false
		for _, ref := range activity.References {
			if ref.NodeRevisionID == reference {
				found = true
			}
		}
		if !found {
			return ErrNotFound
		}
		sources, err := s.starter.Knowledge.ReadSourceSelectionTx(ctx, tx, e.Body.Manifest)
		if err != nil {
			return err
		}
		for i, source := range sources {
			for _, fragment := range source.Fragments {
				if fragment.NodeRevisionID == reference {
					return send(SourceCitation{ID: reference, Source: source.SourceItem, Fragment: fragment, DocumentSHA256: e.Body.Manifest.Entries[i].SHA256, SliceSHA256: knowledge.SourceTextHash(fragment.Text)})
				}
			}
		}
		return ErrNotFound
	})
}
