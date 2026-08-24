package postgresstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringpostgres "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/jackc/pgx/v5"
)

func (s *Store) readSessionView(ctx context.Context, id string, current bool) (learning.SessionView, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return learning.SessionView{}, fmt.Errorf("begin session work item read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	learningGeneration, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning)
	if err != nil {
		return learning.SessionView{}, sessionReadError(ctx, err)
	}
	tutoringGeneration, err := s.tutoring.LockReadWith(ctx, tx)
	if err != nil {
		return learning.SessionView{}, sessionReadError(ctx, err)
	}
	if learningGeneration != tutoringGeneration {
		return learning.SessionView{}, &learning.Error{Code: learning.CodeContentRedacted, Reason: "owner_generation_mismatch"}
	}

	metadata, _, _, _, err := metadataFrom(ctx, tx)
	if err != nil {
		return learning.SessionView{}, sessionReadError(ctx, err)
	}
	var raw, stats []byte
	if current {
		err = tx.QueryRow(ctx, `SELECT s.item,COALESCE(st.item,'null'::jsonb) FROM learning_projection_sessions s LEFT JOIN learning_projection_stats st ON st.generation_id=s.generation_id AND st.session_id=s.session_id WHERE s.generation_id=$1 AND s.item->'session'->>'state'<>'Completed' ORDER BY s.updated_event_seq DESC,s.session_id DESC LIMIT 1`, metadata.GenerationID).Scan(&raw, &stats)
	} else {
		err = tx.QueryRow(ctx, `SELECT s.item,COALESCE(st.item,'null'::jsonb) FROM learning_projection_sessions s LEFT JOIN learning_projection_stats st ON st.generation_id=s.generation_id AND st.session_id=s.session_id WHERE s.generation_id=$1 AND s.session_id=$2`, metadata.GenerationID, id).Scan(&raw, &stats)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return learning.SessionView{}, &learning.Error{Code: learning.CodeNotFound}
	}
	if err != nil {
		return learning.SessionView{}, sessionReadError(ctx, fmt.Errorf("read projected session: %w", err))
	}
	view, err := sessionViewFromProjection(metadata, raw, stats)
	if err != nil {
		return learning.SessionView{}, projectionFailure("projected_session_decode", err)
	}
	authority, err := s.tutoring.LoadSessionLockedWith(ctx, tx, view.Session.ID)
	if err != nil {
		return learning.SessionView{}, sessionTypedReadError(ctx, "session_authority_missing", err)
	}
	if !consistentSessionProjection(view.Session, authority) {
		return learning.SessionView{}, projectionFailure("session_projection_authority_mismatch", nil)
	}
	view.Session.ActiveFrame = authority.ActiveFrame
	if authority.State == tutoring.StateCompleted {
		view.WorkItem = nil
	} else {
		workItem, err := s.assembleSessionWorkItem(ctx, tx, metadata.GenerationID, authority)
		if err != nil {
			return learning.SessionView{}, err
		}
		view.WorkItem = workItem
	}
	if err := tx.Commit(ctx); err != nil {
		return learning.SessionView{}, sessionReadError(ctx, fmt.Errorf("commit session work item read: %w", err))
	}
	return view, nil
}

func consistentSessionProjection(projected, authority tutoring.Session) bool {
	if projected.ID != authority.ID ||
		projected.State != authority.State ||
		projected.AggregateVer != authority.AggregateVer ||
		projected.AttachedQuiz != authority.AttachedQuiz ||
		!reflect.DeepEqual(projected.Context, authority.Context) {
		return false
	}
	if reflect.DeepEqual(projected.ActiveFrame, authority.ActiveFrame) {
		return true
	}
	return projected.ActiveFrame != nil &&
		projected.ActiveFrame.Invalidated &&
		projected.ActiveFrame.ID != "" &&
		projected.ActiveFrame.SessionID == projected.ID &&
		projected.ActiveFrame.InvalidationReason != "" &&
		authority.ActiveFrame == nil &&
		authority.FocusFrameInvalidated
}

func (s *Store) assembleSessionWorkItem(ctx context.Context, tx pgx.Tx, generationID string, session tutoring.Session) (*learning.SessionWorkItem, error) {
	if session.State == tutoring.StateIdle || session.State == tutoring.StateAdvanceOrReview || session.State == tutoring.StateFocusSuspended || session.State == tutoring.StateFocusResumed || session.State == tutoring.StateCompleted {
		return nil, projectionFailure("unstable_session_state", nil)
	}
	if session.Context.GoalRevisionID == "" {
		return nil, projectionFailure("goal_context_missing", nil)
	}
	goal, err := loadGoalRevisionForView(ctx, tx, session.Context.GoalRevisionID)
	if err != nil || goal.ID != session.Context.GoalRevisionID {
		return nil, sessionTypedReadError(ctx, "goal_revision_ownership", err)
	}
	item := &learning.SessionWorkItem{GoalRevision: &goal}

	needsRoute := session.State != tutoring.StateGoalReady && session.State != tutoring.StateDiagnostic
	if needsRoute {
		if session.Context.RouteRevisionID == "" || session.Context.RouteStepID == "" || session.Context.KnowledgeRevisionID == "" || session.Context.FocusNodeRevisionID == "" {
			return nil, projectionFailure("route_context_missing", nil)
		}
		route, err := loadRouteRevisionForView(ctx, tx, session.Context.RouteRevisionID)
		if err != nil || !routeOwnsSessionFocus(route, session) {
			return nil, sessionTypedReadError(ctx, "route_revision_ownership", err)
		}
		item.RouteRevision = &route
	}

	needsActivity := session.State == tutoring.StateActivityIssued || session.State == tutoring.StateAwaitingResponse || session.State == tutoring.StateEvaluating || session.State == tutoring.StateFeedback
	if (session.State == tutoring.StateFreeQuestion || session.State == tutoring.StateFreeAnswer) && session.Context.ActivityID != nil {
		needsActivity = true
	}
	if needsActivity {
		if session.Context.ActivityID == nil {
			return nil, projectionFailure("activity_context_missing", nil)
		}
		activity, err := loadActivityForView(ctx, tx, *session.Context.ActivityID)
		if err != nil || !activityOwnsSessionFocus(activity, session) {
			return nil, sessionTypedReadError(ctx, "activity_ownership", err)
		}
		item.Activity = &activity
	}

	needsAttempt := session.State == tutoring.StateEvaluating || session.State == tutoring.StateFeedback
	if (session.State == tutoring.StateFreeQuestion || session.State == tutoring.StateFreeAnswer) && session.Context.AttemptID != nil {
		needsAttempt = true
	}
	if needsAttempt {
		if session.Context.AttemptID == nil || item.Activity == nil {
			return nil, projectionFailure("attempt_context_missing", nil)
		}
		attempt, err := loadAttemptForView(ctx, tx, *session.Context.AttemptID)
		if err != nil || attempt.SessionID != session.ID || attempt.ActivityID != item.Activity.ID || attempt.ActivityRevision != item.Activity.Revision {
			return nil, sessionTypedReadError(ctx, "attempt_ownership", err)
		}
		item.Attempt = &attempt
	}

	actionContext := learning.WorkItemActionContext{}
	if session.State == tutoring.StateFeedback {
		if item.Activity == nil || item.Attempt == nil {
			return nil, projectionFailure("feedback_context_missing", nil)
		}
		loaded, err := loadAssessmentWith(ctx, tx, item.Attempt.ID, true)
		if err != nil || loaded.artifact.SessionID != session.ID || loaded.artifact.AttemptID != item.Attempt.ID || loaded.artifact.ActivityID != item.Activity.ID || loaded.artifact.ActivityRevision != item.Activity.Revision || loaded.decision.AssessmentID != loaded.artifact.ID {
			return nil, sessionTypedReadError(ctx, "assessment_ownership", err)
		}
		item.Assessment = &loaded.artifact
		item.AssessmentDecision = &loaded.decision
		actionContext.AssessmentDisposition = loaded.decision.Disposition
		actionContext.AssessmentConfirmable = learning.ConfirmableAssessment(*item.Activity, *item.Attempt, loaded.artifact)
	}

	if session.ActiveFrame != nil {
		if !validWorkItemFocusFrame(session) {
			return nil, projectionFailure("active_focus_frame_ownership", nil)
		}
		question, err := s.tutoring.LatestFreeQuestionForFrameLockedWith(ctx, tx, session.ID, session.ActiveFrame.ID, session.AggregateVer)
		if err != nil || !questionMatchesWorkItemSession(session, question) {
			return nil, sessionTypedReadError(ctx, "free_question_ownership", err)
		}
		if err := validateFreeQuestionCommitVersion(ctx, tx, question); err != nil {
			return nil, sessionTypedReadError(ctx, "free_question_event_version", err)
		}
		answer, hasAnswer, err := s.tutoring.LoadFreeAnswerForQuestionLockedWith(ctx, tx, question.ID)
		if err != nil || (hasAnswer && (answer.SessionID != session.ID || answer.FocusFrameID != session.ActiveFrame.ID || answer.FreeQuestionID != question.ID || answer.KnowledgeRevisionID != question.KnowledgeRevisionID)) {
			return nil, sessionTypedReadError(ctx, "free_answer_ownership", err)
		}
		if session.State == tutoring.StateFreeQuestion && hasAnswer {
			return nil, projectionFailure("free_question_has_answer", nil)
		}
		if (session.State == tutoring.StateFreeAnswer || session.AttachedQuiz) && !hasAnswer {
			return nil, projectionFailure("free_answer_missing", nil)
		}
		item.FreeQuestion = &question
		if hasAnswer {
			item.FreeAnswer = &answer
		}
		if session.AttachedQuiz {
			if item.Activity == nil || item.Activity.AttachedFreeQuestionID != question.ID || item.Activity.AttachedFreeAnswerID != answer.ID {
				return nil, projectionFailure("attached_quiz_ownership", nil)
			}
		}
	} else if session.State == tutoring.StateFreeQuestion || session.State == tutoring.StateFreeAnswer || session.AttachedQuiz {
		return nil, projectionFailure("active_focus_frame_missing", nil)
	}

	if session.State == tutoring.StateRouteActive {
		due, err := dueReviewForFocus(ctx, tx, generationID, session.Context.FocusNodeRevisionID)
		if err != nil {
			return nil, sessionTypedReadError(ctx, "review_projection", err)
		}
		actionContext.DueReview = due
	}
	actions, decisions, err := learning.WorkItemActions(session.State, actionContext)
	if err != nil {
		return nil, projectionFailure("allowed_action_matrix", err)
	}
	item.AllowedActions = actions
	item.AllowedAssessmentDecisions = decisions
	normalizeWorkItem(item)
	return item, nil
}

func routeOwnsSessionFocus(route learning.RouteRevision, session tutoring.Session) bool {
	if route.ID != session.Context.RouteRevisionID || route.GoalRevisionID != session.Context.GoalRevisionID || route.KnowledgeRevisionID != session.Context.KnowledgeRevisionID {
		return false
	}
	for _, step := range route.Steps {
		if step.ID == session.Context.RouteStepID {
			return step.NodeRevisionID == session.Context.FocusNodeRevisionID
		}
	}
	return false
}

func activityOwnsSessionFocus(activity learning.Activity, session tutoring.Session) bool {
	return activity.ID == *session.Context.ActivityID && activity.SessionID == session.ID && activity.GoalRevisionID == session.Context.GoalRevisionID && activity.RouteRevisionID == session.Context.RouteRevisionID && activity.RouteStepID == session.Context.RouteStepID && activity.KnowledgeRevisionID == session.Context.KnowledgeRevisionID && activity.TargetNodeRevisionID == session.Context.FocusNodeRevisionID
}

func validWorkItemFocusFrame(session tutoring.Session) bool {
	frame := session.ActiveFrame
	if frame == nil || frame.Invalidated || session.FocusFrameInvalidated || frame.ID == "" || frame.SessionID != session.ID || frame.SavedAggregateVersion < 1 || frame.SavedAggregateVersion >= session.AggregateVer || frame.CreatedEventSequence < 1 {
		return false
	}
	if frame.Context.GoalRevisionID == "" || frame.Context.RouteRevisionID == "" || frame.Context.RouteStepID == "" || frame.Context.KnowledgeRevisionID == "" || frame.Context.FocusNodeRevisionID == "" || !sameStableWorkItemContext(session.Context, frame.Context) {
		return false
	}
	switch frame.SavedState {
	case tutoring.StateRouteActive:
		if frame.Context.ActivityID != nil || frame.Context.AttemptID != nil {
			return false
		}
	case tutoring.StateActivityIssued, tutoring.StateAwaitingResponse:
		if frame.Context.ActivityID == nil || frame.Context.AttemptID != nil {
			return false
		}
	default:
		return false
	}
	if session.State == tutoring.StateFreeQuestion || session.State == tutoring.StateFreeAnswer {
		return !session.AttachedQuiz && reflect.DeepEqual(session.Context, frame.Context)
	}
	if !session.AttachedQuiz {
		return false
	}
	switch session.State {
	case tutoring.StateActivityIssued, tutoring.StateAwaitingResponse, tutoring.StateEvaluating, tutoring.StateFeedback:
		return session.Context.ActivityID != nil
	default:
		return false
	}
}

func sameStableWorkItemContext(left, right tutoring.FocusContext) bool {
	return left.GoalRevisionID == right.GoalRevisionID &&
		left.RouteRevisionID == right.RouteRevisionID &&
		left.RouteStepID == right.RouteStepID &&
		left.KnowledgeRevisionID == right.KnowledgeRevisionID &&
		left.FocusNodeRevisionID == right.FocusNodeRevisionID
}

func questionMatchesWorkItemSession(session tutoring.Session, question tutoring.FreeQuestion) bool {
	if question.ID == "" || question.SessionID != session.ID || question.FocusFrameID != session.ActiveFrame.ID || question.KnowledgeRevisionID != session.ActiveFrame.Context.KnowledgeRevisionID || question.SessionAggregateVer <= session.ActiveFrame.SavedAggregateVersion || question.SessionAggregateVer > session.AggregateVer {
		return false
	}
	return session.State != tutoring.StateFreeQuestion || question.SessionAggregateVer == session.AggregateVer
}

func validateFreeQuestionCommitVersion(ctx context.Context, db learningLoaderDB, question tutoring.FreeQuestion) error {
	var matchCount, commitCount int64
	var committedVersion int64
	err := db.QueryRow(ctx, `
		WITH matches AS (
			SELECT e.device_id,e.operation_id,e.aggregate_id
			FROM learning_events e
			JOIN learning_event_payloads p ON p.id=e.payload_id AND p.payload_hash=e.payload_hash
			WHERE e.event_type='FreeQuestionAsked'
			  AND e.aggregate_type='session'
			  AND e.aggregate_id=$2::uuid
			  AND p.redacted_at IS NULL
			  AND p.payload->>'free_question_id'=$1::text
			  AND p.payload->>'session_id'=$2::text
			  AND p.payload->>'focus_frame_id'=$3::text
		), commits AS (
			SELECT m.device_id,m.operation_id,max(batch.aggregate_version) AS aggregate_version
			FROM matches m
			JOIN learning_events batch
			  ON batch.device_id=m.device_id
			 AND batch.operation_id=m.operation_id
			 AND batch.aggregate_type='session'
			 AND batch.aggregate_id=m.aggregate_id
			GROUP BY m.device_id,m.operation_id
		)
		SELECT (SELECT count(*) FROM matches),
		       (SELECT count(*) FROM commits),
		       COALESCE((SELECT max(aggregate_version) FROM commits),0)`, question.ID, question.SessionID, question.FocusFrameID).Scan(&matchCount, &commitCount, &committedVersion)
	if err != nil {
		return err
	}
	if matchCount != 1 || commitCount != 1 || committedVersion != question.SessionAggregateVer {
		return fmt.Errorf("free question event association matches=%d commits=%d version=%d stored=%d", matchCount, commitCount, committedVersion, question.SessionAggregateVer)
	}
	return nil
}

func loadGoalRevisionForView(ctx context.Context, db learningLoaderDB, id string) (learning.GoalRevision, error) {
	var value learning.GoalRevision
	err := db.QueryRow(ctx, `SELECT id,goal_id,revision,goal_text,source,actor_device_id,created_at,previous_revision_id FROM learning_goal_revisions WHERE id=$1`, id).Scan(&value.ID, &value.GoalID, &value.Revision, &value.Text, &value.Source, &value.ActorDeviceID, &value.CreatedAt, &value.PreviousRevisionID)
	return value, err
}

func loadRouteRevisionForView(ctx context.Context, db learningLoaderDB, id string) (learning.RouteRevision, error) {
	var value learning.RouteRevision
	var proposal *string
	if err := db.QueryRow(ctx, `SELECT id,route_id,revision,goal_revision_id,knowledge_revision_id,route_policy_version,source_proposal_id,created_at FROM learning_route_revisions WHERE id=$1`, id).Scan(&value.ID, &value.RouteID, &value.Revision, &value.GoalRevisionID, &value.KnowledgeRevisionID, &value.PolicyVersion, &proposal, &value.CreatedAt); err != nil {
		return value, err
	}
	value.SourceProposalID = deref(proposal)
	rows, err := db.Query(ctx, `SELECT id,ordinal,node_id,node_revision_id,teaching_intent,completion_condition FROM learning_route_steps WHERE route_revision_id=$1 ORDER BY ordinal`, id)
	if err != nil {
		return value, err
	}
	defer rows.Close()
	for rows.Next() {
		var step learning.RouteStep
		if err := rows.Scan(&step.ID, &step.Ordinal, &step.NodeID, &step.NodeRevisionID, &step.TeachingIntent, &step.CompletionCondition); err != nil {
			return value, err
		}
		value.Steps = append(value.Steps, step)
	}
	return value, rows.Err()
}

func loadActivityForView(ctx context.Context, db learningLoaderDB, id string) (learning.Activity, error) {
	var value learning.Activity
	var rubricRaw []byte
	var activityType string
	var allowed []string
	var proposal, freeQuestion, freeAnswer *string
	if err := db.QueryRow(ctx, `SELECT id,revision,session_id,goal_revision_id,route_revision_id,route_step_id,knowledge_revision_id,target_node_id,target_node_revision_id,prompt,activity_type,rubric_revision,rubric,difficulty,allowed_help,activity_policy_version,assessment_policy_version,review_policy_version,source_proposal_id,attached_free_question_id,attached_free_answer_id,is_review,created_at FROM learning_activities WHERE id=$1`, id).Scan(&value.ID, &value.Revision, &value.SessionID, &value.GoalRevisionID, &value.RouteRevisionID, &value.RouteStepID, &value.KnowledgeRevisionID, &value.TargetNodeID, &value.TargetNodeRevisionID, &value.Prompt, &activityType, &value.Rubric.Revision, &rubricRaw, &value.Difficulty, &allowed, &value.ActivityPolicyVersion, &value.AssessmentPolicyVersion, &value.ReviewPolicyVersion, &proposal, &freeQuestion, &freeAnswer, &value.Review, &value.CreatedAt); err != nil {
		return value, err
	}
	value.Type = learning.ActivityType(activityType)
	if err := json.Unmarshal(rubricRaw, &value.Rubric); err != nil {
		return value, err
	}
	value.SourceProposalID = deref(proposal)
	value.AttachedFreeQuestionID = deref(freeQuestion)
	value.AttachedFreeAnswerID = deref(freeAnswer)
	for _, item := range allowed {
		value.AllowedHelp = append(value.AllowedHelp, learning.HelpLevel(item))
	}
	rows, err := db.Query(ctx, `SELECT knowledge_revision_id,node_id,node_revision_id,document_revision_id,source_start,source_end,slice_text,slice_hash FROM learning_activity_references WHERE activity_id=$1 ORDER BY ordinal`, id)
	if err != nil {
		return value, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref learning.KnowledgeReference
		var hash []byte
		if err := rows.Scan(&ref.KnowledgeRevisionID, &ref.NodeID, &ref.NodeRevisionID, &ref.DocumentRevisionID, &ref.Range.Start, &ref.Range.End, &ref.Slice, &hash); err != nil {
			return value, err
		}
		ref.SliceSHA256 = hex.EncodeToString(hash)
		value.References = append(value.References, ref)
	}
	return value, rows.Err()
}

func loadAttemptForView(ctx context.Context, db learningLoaderDB, id string) (learning.Attempt, error) {
	var value learning.Attempt
	var hash []byte
	err := db.QueryRow(ctx, `SELECT a.id,a.session_id,a.activity_id,a.activity_revision,a.answer_payload_id,p.answer_text,a.help_level,a.actor_device_id,a.occurred_at,a.received_at,a.payload_hash FROM learning_attempts a JOIN learning_attempt_payloads p ON p.id=a.answer_payload_id WHERE a.id=$1`, id).Scan(&value.ID, &value.SessionID, &value.ActivityID, &value.ActivityRevision, &value.AnswerPayloadID, &value.Answer, &value.Help, &value.ActorDeviceID, &value.OccurredAt, &value.ReceivedAt, &hash)
	value.AnswerSHA256 = hex.EncodeToString(hash)
	return value, err
}

func dueReviewForFocus(ctx context.Context, db learningLoaderDB, generationID, nodeRevisionID string) (bool, error) {
	var raw []byte
	err := db.QueryRow(ctx, `SELECT item FROM learning_projection_reviews WHERE generation_id=$1 AND node_revision_id=$2 AND due_at<=transaction_timestamp()`, generationID, nodeRevisionID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var review learning.ReviewSchedule
	if err := json.Unmarshal(raw, &review); err != nil || review.NodeRevisionID != nodeRevisionID {
		return false, projectionFailure("review_projection_ownership", err)
	}
	return true, nil
}

func normalizeWorkItem(item *learning.SessionWorkItem) {
	if item.AllowedActions == nil {
		item.AllowedActions = []tutoring.Action{}
	}
	if item.AllowedAssessmentDecisions == nil {
		item.AllowedAssessmentDecisions = []string{}
	}
	if item.RouteRevision != nil && item.RouteRevision.Steps == nil {
		item.RouteRevision.Steps = []learning.RouteStep{}
	}
	if item.Activity != nil {
		if item.Activity.References == nil {
			item.Activity.References = []learning.KnowledgeReference{}
		}
		if item.Activity.AllowedHelp == nil {
			item.Activity.AllowedHelp = []learning.HelpLevel{}
		}
		if item.Activity.Rubric.Items == nil {
			item.Activity.Rubric.Items = []learning.RubricItem{}
		}
		for index := range item.Activity.Rubric.Items {
			if item.Activity.Rubric.Items[index].RequiredReferenceIDs == nil {
				item.Activity.Rubric.Items[index].RequiredReferenceIDs = []string{}
			}
		}
	}
	if item.Assessment != nil {
		if item.Assessment.Items == nil {
			item.Assessment.Items = []learning.AssessmentItem{}
		}
		if item.Assessment.RiskFlags == nil {
			item.Assessment.RiskFlags = []learning.RiskFlag{}
		}
		if item.Assessment.AttemptCategories == nil {
			item.Assessment.AttemptCategories = []string{}
		}
		if item.Assessment.ModelParameters == nil {
			item.Assessment.ModelParameters = map[string]any{}
		}
	}
	if item.AssessmentDecision != nil && item.AssessmentDecision.Items == nil {
		item.AssessmentDecision.Items = []learning.AssessmentItem{}
	}
	if item.FreeQuestion != nil && item.FreeQuestion.References == nil {
		item.FreeQuestion.References = []tutoring.FrozenReference{}
	}
	if item.FreeAnswer != nil && item.FreeAnswer.References == nil {
		item.FreeAnswer.References = []tutoring.FrozenReference{}
	}
}

func sessionReadError(ctx context.Context, err error) error {
	if privacy.ErrorCode(err) == privacy.CodeContentRedacted || privacy.ErrorCode(context.Cause(ctx)) == privacy.CodeContentRedacted {
		return &learning.Error{Code: learning.CodeContentRedacted, Reason: "privacy_read_cancelled", Cause: err}
	}
	return err
}

func sessionTypedReadError(ctx context.Context, reason string, err error) error {
	if mapped := sessionReadError(ctx, err); learning.ErrorCode(mapped) == learning.CodeContentRedacted {
		return mapped
	}
	return projectionFailure(reason, err)
}

func projectionFailure(reason string, err error) error {
	return &learning.Error{Code: learning.CodeProjectionUnavailable, Reason: reason, Cause: err}
}

var _ tutoringpostgres.DBTX = (pgx.Tx)(nil)
