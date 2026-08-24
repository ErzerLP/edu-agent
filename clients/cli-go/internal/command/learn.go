package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/terminal"
)

func (a *App) runLearn(ctx context.Context, args []string) error {
	set := newFlagSet("learn")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "learn accepts only connection flags", "run edu-agent learn", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	view, active, err := a.currentSession(ctx, online.client)
	if err != nil {
		return err
	}
	if !active {
		goalText, readErr := a.Terminal.ReadLine("Goal: ")
		if readErr != nil || strings.TrimSpace(goalText) == "" {
			return commandError("invalid_goal", "a goal is required when no active session exists", "enter a non-empty goal or run goal set", ExitInput)
		}
		view, err = a.createInteractiveSession(ctx, online.client, strings.TrimSpace(goalText))
		if err != nil {
			return err
		}
	}
	return a.learnLoop(ctx, online.client, view)
}

func (a *App) createInteractiveSession(ctx context.Context, client APIClient, goalText string) (api.SessionView, error) {
	if !utf8.ValidString(goalText) || len([]rune(goalText)) > 4000 {
		return api.SessionView{}, commandError("invalid_goal", "goal text must be valid UTF-8 with at most 4000 characters", "enter a shorter goal", ExitInput)
	}
	goal, err := a.createGoal(ctx, client, goalText)
	if err != nil {
		return api.SessionView{}, err
	}
	sessionID, err := a.operationID()
	if err != nil {
		return api.SessionView{}, err
	}
	operationID, err := a.operationID()
	if err != nil {
		return api.SessionView{}, err
	}
	_, err = client.CreateSession(ctx, api.TutoringSessionRequest{
		OperationID: operationID, PayloadSchemaVersion: 1, AggregateType: "session",
		AggregateID: sessionID, ExpectedVersion: 0, GoalRevisionID: goal.GoalRevisionID,
	})
	if err != nil {
		return api.SessionView{}, mapAPIError(err)
	}
	view, err := client.CurrentSession(ctx)
	if err != nil {
		return api.SessionView{}, mapAPIError(err)
	}
	return view, nil
}

func (a *App) learnLoop(ctx context.Context, client APIClient, view api.SessionView) error {
	for {
		printProjectionWarning(a.Err, view.Metadata)
		switch view.Session.State {
		case "GoalReady":
			fresh, _, err := a.noFieldAction(ctx, client, view, "start_diagnostic")
			if err != nil {
				return err
			}
			view = fresh
		case "Diagnostic":
			fresh, err := a.learnDiagnostic(ctx, client, view)
			if err != nil {
				return err
			}
			view = fresh
		case "RouteActive":
			fresh, quit, err := a.learnRouteActive(ctx, client, view)
			if err != nil {
				return err
			}
			if quit {
				return nil
			}
			view = fresh
		case "ActivityIssued":
			fresh, quit, err := a.learnActivityIssued(ctx, client, view)
			if err != nil {
				return err
			}
			if quit {
				return nil
			}
			view = fresh
		case "AwaitingResponse":
			fresh, quit, err := a.learnAwaitingResponse(ctx, client, view)
			if err != nil {
				return err
			}
			if quit {
				return nil
			}
			view = fresh
		case "Evaluating":
			fresh, err := a.learnEvaluating(ctx, client, view)
			if err != nil {
				return err
			}
			view = fresh
		case "Feedback":
			fresh, quit, err := a.learnFeedback(ctx, client, view)
			if err != nil {
				return err
			}
			if quit {
				return nil
			}
			view = fresh
		case "FreeQuestion":
			fresh, err := a.learnFreeQuestion(ctx, client, view)
			if err != nil {
				return err
			}
			view = fresh
		case "FreeAnswer":
			fresh, quit, err := a.learnFreeAnswer(ctx, client, view)
			if err != nil {
				return err
			}
			if quit {
				return nil
			}
			view = fresh
		case "Completed":
			_, err := fmt.Fprintf(a.Out, "Result: session=%s completed active_time=%ds estimated=%t samples=%d\nNext: run goal set to start a new session\n", safeText(view.Session.SessionID), view.EstimatedActiveTime.DurationSeconds, view.EstimatedActiveTime.Estimated, view.EstimatedActiveTime.SampleCount)
			return err
		default:
			return commandError("invalid_state", "the server returned a non-resumable tutoring state", "retry after the authoritative projection is repaired", ExitUnavailable)
		}
	}
}

func (a *App) learnDiagnostic(ctx context.Context, client APIClient, view api.SessionView) (api.SessionView, error) {
	if view.WorkItem == nil || view.WorkItem.GoalRevision == nil || !allowed(view.WorkItem.AllowedActions, "apply_route") {
		return api.SessionView{}, commandError("invalid_state", "Diagnostic work item is incomplete", "refresh the session", ExitConflict)
	}
	head, err := client.KnowledgeHead(ctx)
	if err != nil {
		return api.SessionView{}, mapAPIError(err)
	}
	retrieval, err := a.retrieveForWorkItem(ctx, client, view, view.WorkItem.GoalRevision.Text, head.RevisionID)
	if err != nil {
		return api.SessionView{}, err
	}
	requestID, err := a.operationID()
	if err != nil {
		return api.SessionView{}, err
	}
	request, err := proposalRequest(view, "route", retrieval, requestID)
	if err != nil {
		return api.SessionView{}, err
	}
	proposal, fresh, stale, err := a.createProposalAndRefetch(ctx, client, view, request)
	if err != nil {
		return api.SessionView{}, err
	}
	if stale {
		return fresh, nil
	}
	_, _ = fmt.Fprintln(a.Out, "Current: proposed route")
	for index, step := range proposal.Route {
		_, _ = fmt.Fprintf(a.Out, "%d node=%s intent=%s completion=%s\n", index, safeText(step.NodeRevisionID), safeText(step.TeachingIntent), safeText(step.CompletionCondition))
	}
	fresh, _, err = a.proposalAction(ctx, client, fresh, "apply_route", proposal.ProposalID)
	return fresh, err
}

func (a *App) learnRouteActive(ctx context.Context, client APIClient, view api.SessionView) (api.SessionView, bool, error) {
	if view.WorkItem == nil || view.WorkItem.GoalRevision == nil || view.WorkItem.RouteRevision == nil {
		return view, false, commandError("invalid_state", "RouteActive work item is incomplete", "refresh the session", ExitConflict)
	}
	step, err := currentRouteStep(view)
	if err != nil {
		return view, false, err
	}
	_, _ = fmt.Fprintf(a.Out, "Current: route step %d node=%s intent=%s\n", step.Ordinal, safeText(step.NodeRevisionID), safeText(step.TeachingIntent))
	for {
		line, readErr := a.readLearnLine("> ")
		if readErr != nil {
			return view, false, learnInputError(readErr, "route input ended", "run learn again to resume")
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		if !strings.HasPrefix(trimmed, ":") {
			return view, false, commandError("invalid_input", "plain text is not accepted in RouteActive", "press Enter to continue or use a displayed command", ExitInput)
		}
		return a.handleLearnCommand(ctx, client, view, trimmed)
	}
	if allowed(view.WorkItem.AllowedActions, "present_review") {
		now := time.Now().UTC()
		reviews, err := client.Reviews(ctx, "", defaultPageLimit, &now)
		if err != nil {
			return view, false, mapAPIError(err)
		}
		printProjectionWarning(a.Err, reviews.Metadata)
		for _, review := range reviews.Items {
			if review.NodeRevisionID == view.Session.Focus.FocusNodeRevisionID {
				confirmed, confirmErr := a.Terminal.Confirm("A review is due for the current node. Present it now?")
				if confirmErr != nil {
					return view, false, commandError("confirmation_failed", "review confirmation could not be read", "retry in an interactive terminal", ExitInput)
				}
				if confirmed {
					fresh, err := a.issueRouteActivity(ctx, client, view, "present_review")
					return fresh, false, err
				}
				break
			}
		}
	}
	if allowed(view.WorkItem.AllowedActions, "record_exposure") {
		step, err := currentRouteStep(view)
		if err != nil {
			return view, false, err
		}
		query := step.TeachingIntent
		if strings.TrimSpace(query) == "" {
			query = view.WorkItem.GoalRevision.Text
		}
		retrieval, err := a.retrieveForWorkItem(ctx, client, view, query, view.WorkItem.RouteRevision.KnowledgeRevisionID)
		if err != nil {
			return view, false, err
		}
		requestID, err := a.operationID()
		if err != nil {
			return view, false, err
		}
		request, err := proposalRequest(view, "explanation", retrieval, requestID)
		if err != nil {
			return view, false, err
		}
		proposal, fresh, stale, err := a.createProposalAndRefetch(ctx, client, view, request)
		if err != nil {
			return view, false, err
		}
		if stale {
			return fresh, false, nil
		}
		_, _ = fmt.Fprintf(a.Out, "Current: explanation (not scored)\n%s\n", safeText(proposal.Text.Text))
		operationID, err := a.operationID()
		if err != nil {
			return view, false, err
		}
		fresh, conflict, err := a.applyAndRefetch(ctx, client, fresh, api.ActionProposalExposureRequest{
			SessionOperation: sessionOperation(fresh, operationID), Action: "record_exposure", ProposalID: proposal.ProposalID, ExposureKind: "explanation",
		})
		if err != nil {
			return view, false, err
		}
		if conflict || fresh.Session.State != "RouteActive" {
			return fresh, false, nil
		}
		view = fresh
	}
	if !allowed(view.WorkItem.AllowedActions, "issue_activity") {
		return a.learnCommandPrompt(ctx, client, view)
	}
	fresh, err := a.issueRouteActivity(ctx, client, view, "issue_activity")
	return fresh, false, err
}

func (a *App) issueRouteActivity(ctx context.Context, client APIClient, view api.SessionView, action string) (api.SessionView, error) {
	step, err := currentRouteStep(view)
	if err != nil {
		return view, err
	}
	query := step.TeachingIntent
	if strings.TrimSpace(query) == "" {
		query = view.WorkItem.GoalRevision.Text
	}
	retrieval, err := a.retrieveForWorkItem(ctx, client, view, query, view.WorkItem.RouteRevision.KnowledgeRevisionID)
	if err != nil {
		return view, err
	}
	requestID, err := a.operationID()
	if err != nil {
		return view, err
	}
	request, err := proposalRequest(view, "activity", retrieval, requestID)
	if err != nil {
		return view, err
	}
	proposal, fresh, stale, err := a.createProposalAndRefetch(ctx, client, view, request)
	if err != nil {
		return view, err
	}
	if stale {
		return fresh, nil
	}
	if proposal.Activity == nil {
		return view, commandError("protocol_error", "activity proposal omitted the activity", "check the server version", ExitInternal)
	}
	_, _ = fmt.Fprintf(a.Out, "Question: %s\nRubric items: %d difficulty=%d help=%s\n", safeText(proposal.Activity.Prompt), len(proposal.Activity.Rubric.Items), proposal.Activity.Difficulty, safeText(strings.Join(proposal.Activity.AllowedHelp, ",")))
	fresh, _, err = a.proposalAction(ctx, client, fresh, action, proposal.ProposalID)
	return fresh, err
}

func currentRouteStep(view api.SessionView) (api.RouteStep, error) {
	if view.WorkItem == nil || view.WorkItem.RouteRevision == nil || view.Session.Focus.RouteStepID == "" {
		return api.RouteStep{}, commandError("invalid_state", "the current route step is unavailable", "refresh the session", ExitConflict)
	}
	for _, step := range view.WorkItem.RouteRevision.Steps {
		if step.RouteStepID == view.Session.Focus.RouteStepID {
			return step, nil
		}
	}
	return api.RouteStep{}, commandError("invalid_state", "the focus route step does not belong to the current route", "wait for projection repair", ExitUnavailable)
}

func printActivity(out interface{ Write([]byte) (int, error) }, activity api.Activity) {
	_, _ = fmt.Fprintf(out, "Question: %s\nCurrent: type=%s difficulty=%d rubric_items=%d help=%s review=%t\n", safeText(activity.Prompt), safeText(activity.Type), activity.Difficulty, len(activity.Rubric.Items), safeText(strings.Join(activity.AllowedHelp, ",")), activity.Review)
}

func (a *App) learnActivityIssued(ctx context.Context, client APIClient, view api.SessionView) (api.SessionView, bool, error) {
	if view.WorkItem == nil || view.WorkItem.Activity == nil {
		return view, false, commandError("invalid_state", "ActivityIssued omitted the authoritative activity", "retry after the projection is repaired", ExitUnavailable)
	}
	printActivity(a.Out, *view.WorkItem.Activity)
	for {
		line, err := a.readLearnLine("> ")
		if err != nil {
			return view, false, learnInputError(err, "activity input ended", "run learn again to resume")
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			fresh, _, presentErr := a.noFieldAction(ctx, client, view, "present_activity")
			return fresh, false, presentErr
		}
		if !strings.HasPrefix(trimmed, ":") {
			return view, false, commandError("invalid_input", "plain text is not accepted before the activity is presented", "press Enter to continue or use :ask", ExitInput)
		}
		return a.handleLearnCommand(ctx, client, view, trimmed)
	}
}

func (a *App) learnAwaitingResponse(ctx context.Context, client APIClient, view api.SessionView) (api.SessionView, bool, error) {
	if view.WorkItem == nil || view.WorkItem.Activity == nil {
		return view, false, commandError("invalid_state", "AwaitingResponse omitted the authoritative activity", "refresh the session", ExitConflict)
	}
	printActivity(a.Out, *view.WorkItem.Activity)
	for {
		line, err := a.readLearnLine("> ")
		if err != nil {
			return view, false, learnInputError(err, "answer input ended before submission", "run learn again to resume from the server work item")
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ":") {
			if trimmed == ":answer" {
				answer, blockErr := a.readMultilineAnswer()
				if blockErr != nil {
					return view, false, blockErr
				}
				fresh, submitErr := a.submitAttempt(ctx, client, view, answer)
				return fresh, false, submitErr
			}
			return a.handleLearnCommand(ctx, client, view, trimmed)
		}
		if trimmed == "" {
			return view, false, commandError("invalid_answer", "answer must not be empty", "enter one line or use :answer for a multiline block", ExitInput)
		}
		fresh, submitErr := a.submitAttempt(ctx, client, view, line)
		return fresh, false, submitErr
	}
}

func (a *App) readMultilineAnswer() (string, error) {
	_, _ = fmt.Fprintln(a.Err, "Enter answer lines; a single . ends the block.")
	var lines []string
	total := 0
	for {
		line, err := a.Terminal.ReadLine("")
		if err != nil {
			return "", commandError("input_closed", "multiline answer ended before the terminator", "run learn again and end the block with a single .", ExitInput)
		}
		if line == "." {
			break
		}
		total += len(line)
		if total+len(lines) > 262144 {
			return "", commandError("answer_too_large", "answer exceeds 262144 UTF-8 bytes", "shorten the answer", ExitInput)
		}
		lines = append(lines, line)
	}
	answer := strings.Join(lines, "\n")
	if strings.TrimSpace(answer) == "" || !utf8.ValidString(answer) {
		return "", commandError("invalid_answer", "multiline answer must be non-empty valid UTF-8", "enter the answer again", ExitInput)
	}
	return answer, nil
}

func (a *App) submitAttempt(ctx context.Context, client APIClient, view api.SessionView, answer string) (api.SessionView, error) {
	if view.WorkItem == nil || view.WorkItem.Activity == nil || !allowed(view.WorkItem.AllowedActions, "submit_attempt") {
		return view, commandError("invalid_state", "submit_attempt is not allowed by the current work item", "refresh the session and use a displayed allowed action", ExitConflict)
	}
	help, err := a.chooseHelp(view.WorkItem.Activity.AllowedHelp)
	if err != nil {
		return view, err
	}
	operationID, err := a.operationID()
	if err != nil {
		return view, err
	}
	fresh, _, err := a.applyAndRefetch(ctx, client, view, api.ActionAttemptRequest{
		SessionOperation: sessionOperation(view, operationID), Action: "submit_attempt", Answer: answer, Help: help,
	})
	return fresh, err
}

func (a *App) chooseHelp(values []string) (string, error) {
	_, _ = fmt.Fprintf(a.Out, "Allowed help: %s\n", safeText(strings.Join(values, ",")))
	defaultNone := allowed(values, "none")
	prompt := "Help: "
	if defaultNone {
		prompt = "Help [none]: "
	}
	value, err := a.Terminal.ReadLine(prompt)
	if err != nil {
		return "", commandError("input_closed", "help selection could not be read", "run learn again and choose an allowed help level", ExitInput)
	}
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" && defaultNone {
		value = "none"
	}
	if value == "" || !allowed(values, value) {
		return "", commandError("invalid_help", "help must exactly match an allowed value", "choose one displayed help level", ExitInput)
	}
	return value, nil
}

func (a *App) learnEvaluating(ctx context.Context, client APIClient, view api.SessionView) (api.SessionView, error) {
	if view.WorkItem == nil || view.WorkItem.Activity == nil || view.WorkItem.Attempt == nil || !allowed(view.WorkItem.AllowedActions, "record_assessment") {
		return view, commandError("invalid_state", "Evaluating work item is incomplete", "refresh the session", ExitConflict)
	}
	operationID, err := a.operationID()
	if err != nil {
		return view, err
	}
	request := api.ActionAssessmentRequest{SessionOperation: sessionOperation(view, operationID), Action: "record_assessment"}
	if view.WorkItem.Activity.Type != "objective" {
		requestID, idErr := a.operationID()
		if idErr != nil {
			return view, idErr
		}
		proposalRequest, requestErr := assessmentProposalRequest(view, requestID)
		if requestErr != nil {
			return view, requestErr
		}
		proposal, fresh, stale, proposalErr := a.createProposalAndRefetch(ctx, client, view, proposalRequest)
		if proposalErr != nil {
			return view, proposalErr
		}
		if stale {
			return fresh, nil
		}
		view = fresh
		request.SessionOperation = sessionOperation(view, operationID)
		request.ProposalID = proposal.ProposalID
	}
	fresh, _, err := a.applyAndRefetch(ctx, client, view, request)
	return fresh, err
}

func (a *App) learnFeedback(ctx context.Context, client APIClient, view api.SessionView) (api.SessionView, bool, error) {
	if view.WorkItem == nil || view.WorkItem.Assessment == nil || view.WorkItem.AssessmentDecision == nil {
		return view, false, commandError("invalid_state", "Feedback work item is incomplete", "refresh the session", ExitConflict)
	}
	printAssessment(a.Out, *view.WorkItem.Assessment, *view.WorkItem.AssessmentDecision, view.WorkItem.AllowedAssessmentDecisions)
	for {
		line, err := a.readLearnLine("> ")
		if err != nil {
			return view, false, learnInputError(err, "feedback input ended", "run learn again to resume from Feedback")
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ":assessment") {
			fresh, decisionErr := a.learnAssessmentDecision(ctx, client, view, strings.TrimSpace(strings.TrimPrefix(trimmed, ":assessment")))
			return fresh, false, decisionErr
		}
		if strings.HasPrefix(trimmed, ":") {
			return a.handleLearnCommand(ctx, client, view, trimmed)
		}
		if trimmed != "" {
			return view, false, commandError("invalid_input", "plain text is not an answer in Feedback", "use :assessment, a displayed command, or Enter to acknowledge resolved feedback", ExitInput)
		}
		if view.WorkItem.AssessmentDecision.Disposition == "provisional" {
			return view, false, commandError("provisional_assessment", "provisional feedback cannot be acknowledged", "use :assessment with an allowed confirm, override, or void decision", ExitConflict)
		}
		fresh, _, acknowledgeErr := a.noFieldAction(ctx, client, view, "acknowledge_feedback")
		return fresh, false, acknowledgeErr
	}
}

func (a *App) learnAssessmentDecision(ctx context.Context, client APIClient, view api.SessionView, action string) (api.SessionView, error) {
	if action == "" || action == "show" {
		printAssessment(a.Out, *view.WorkItem.Assessment, *view.WorkItem.AssessmentDecision, view.WorkItem.AllowedAssessmentDecisions)
		return view, nil
	}
	if action != "confirm" && action != "override" && action != "void" {
		return view, commandError("usage", ":assessment requires show, confirm, override, or void", "use a displayed assessment decision", ExitInput)
	}
	if !allowed(view.WorkItem.AllowedAssessmentDecisions, action) {
		return view, commandError("assessment_decision_not_allowed", "the server did not allow this assessment decision", "use a displayed decision", ExitConflict)
	}
	operationID, err := a.operationID()
	if err != nil {
		return view, err
	}
	base := sessionOperation(view, operationID)
	decision := view.WorkItem.AssessmentDecision
	var request api.AssessmentDecisionRequest
	switch action {
	case "confirm":
		request = api.AssessmentConfirmRequest{SessionOperation: base, Kind: "confirm", ExpectedDispositionVersion: decision.Version}
	case "override":
		reason, readErr := a.Terminal.ReadLine("Override reason: ")
		if readErr != nil || strings.TrimSpace(reason) == "" {
			return view, commandError("invalid_assessment_reason", "override requires a non-empty reason", "retry :assessment override", ExitInput)
		}
		items, collectErr := a.collectAssessmentOverride(*view.WorkItem.Assessment, *decision)
		if collectErr != nil {
			return view, collectErr
		}
		request = api.AssessmentOverrideRequest{SessionOperation: base, Kind: "override", ExpectedDispositionVersion: decision.Version, Reason: strings.TrimSpace(reason), Items: items}
	case "void":
		reason, readErr := a.Terminal.ReadLine("Void reason: ")
		if readErr != nil || strings.TrimSpace(reason) == "" {
			return view, commandError("invalid_assessment_reason", "void requires a non-empty reason", "retry :assessment void", ExitInput)
		}
		request = api.AssessmentVoidRequest{SessionOperation: base, Kind: "void", ExpectedDispositionVersion: decision.Version, Reason: strings.TrimSpace(reason)}
	}
	return a.decideAndRefetch(ctx, client, view, request)
}

func (a *App) learnFreeQuestion(ctx context.Context, client APIClient, view api.SessionView) (api.SessionView, error) {
	if view.WorkItem == nil || view.WorkItem.FreeQuestion == nil || view.WorkItem.FreeAnswer != nil || !allowed(view.WorkItem.AllowedActions, "record_free_answer") {
		return view, commandError("invalid_state", "FreeQuestion work item is incomplete", "refresh the session", ExitConflict)
	}
	question := view.WorkItem.FreeQuestion
	retrieval, err := a.retrieveForWorkItem(ctx, client, view, question.Text, question.KnowledgeRevisionID)
	if err != nil {
		return view, err
	}
	requestID, err := a.operationID()
	if err != nil {
		return view, err
	}
	request, err := proposalRequest(view, "free_answer", retrieval, requestID)
	if err != nil {
		return view, err
	}
	proposal, fresh, stale, err := a.createProposalAndRefetch(ctx, client, view, request)
	if err != nil {
		return view, err
	}
	if stale {
		return fresh, nil
	}
	fresh, _, err = a.proposalAction(ctx, client, fresh, "record_free_answer", proposal.ProposalID)
	return fresh, err
}

func (a *App) learnFreeAnswer(ctx context.Context, client APIClient, view api.SessionView) (api.SessionView, bool, error) {
	if view.WorkItem == nil || view.WorkItem.FreeQuestion == nil || view.WorkItem.FreeAnswer == nil {
		return view, false, commandError("invalid_state", "FreeAnswer work item is incomplete", "refresh the session", ExitConflict)
	}
	_, _ = fmt.Fprintf(a.Out, "Question: %s\nCurrent: answer (not scored)\n%s\n", safeText(view.WorkItem.FreeQuestion.Text), safeText(view.WorkItem.FreeAnswer.Text))
	for {
		line, err := a.readLearnLine("> ")
		if err != nil {
			return view, false, learnInputError(err, "free-answer input ended", "run learn again to resume from the server")
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			fresh, _, resumeErr := a.noFieldAction(ctx, client, view, "resume_focus")
			return fresh, false, resumeErr
		}
		if strings.HasPrefix(trimmed, ":") {
			return a.handleLearnCommand(ctx, client, view, trimmed)
		}
		fresh, askErr := a.askFreeQuestion(ctx, client, view, line)
		return fresh, false, askErr
	}
}

func (a *App) handleLearnCommand(ctx context.Context, client APIClient, view api.SessionView, input string) (api.SessionView, bool, error) {
	parts := strings.SplitN(strings.TrimSpace(input), " ", 2)
	command := parts[0]
	argument := ""
	if len(parts) == 2 {
		argument = strings.TrimSpace(parts[1])
	}
	switch command {
	case ":quit":
		return view, true, nil
	case ":ask":
		if argument == "" {
			value, err := a.Terminal.ReadLine("Question: ")
			if err != nil {
				return view, false, commandError("input_closed", "question input ended", "run learn again", ExitInput)
			}
			argument = strings.TrimSpace(value)
		}
		fresh, err := a.askFreeQuestion(ctx, client, view, argument)
		return fresh, false, err
	case ":quiz":
		fresh, err := a.convertFreeAnswerToQuiz(ctx, client, view)
		return fresh, false, err
	case ":resume":
		fresh, _, err := a.noFieldAction(ctx, client, view, "resume_focus")
		return fresh, false, err
	case ":assessment":
		if view.WorkItem == nil || view.WorkItem.Assessment == nil || view.WorkItem.AssessmentDecision == nil {
			return view, false, commandError("assessment_not_found", "the current work item has no assessment", "continue until Feedback", ExitConflict)
		}
		printAssessment(a.Out, *view.WorkItem.Assessment, *view.WorkItem.AssessmentDecision, view.WorkItem.AllowedAssessmentDecisions)
		return view, false, nil
	case ":progress":
		return view, false, a.showLearnProgress(ctx, client, view)
	case ":route":
		if view.WorkItem == nil || view.WorkItem.RouteRevision == nil {
			_, _ = fmt.Fprintln(a.Out, "Current: no route yet")
			return view, false, nil
		}
		printRoute(a.Out, *view.WorkItem.RouteRevision, true, view.Session.Focus.RouteStepID)
		return view, false, nil
	case ":reviews":
		page, err := a.reviewsPage(ctx, client, "", defaultPageLimit, nil)
		if err != nil {
			return view, false, err
		}
		printReviewsPage(a.Out, a.Err, page)
		return view, false, nil
	case ":end":
		confirmed, err := a.Terminal.Confirm("Ending the activity may invalidate the active focus. Continue?")
		if err != nil || !confirmed {
			return view, false, commandError("end_activity_declined", "the activity was not ended", "continue learning or retry :end", ExitInput)
		}
		fresh, _, err := a.noFieldAction(ctx, client, view, "end_activity")
		return fresh, false, err
	case ":complete":
		confirmed, err := a.Terminal.Confirm("Completing the session may invalidate the active focus. Continue?")
		if err != nil || !confirmed {
			return view, false, commandError("complete_session_declined", "the session was not completed", "continue learning or retry :complete", ExitInput)
		}
		fresh, _, err := a.noFieldAction(ctx, client, view, "complete_session")
		return fresh, false, err
	case ":help":
		_, _ = fmt.Fprintf(a.Out, "Commands: %s\n", strings.Join(learnHelpCommands(view), " "))
		return view, false, nil
	default:
		return view, false, commandError("unknown_interactive_command", "the interactive command is not recognized", "use :help to list commands", ExitInput)
	}
}

func learnHelpCommands(view api.SessionView) []string {
	commands := []string{":progress", ":route", ":reviews", ":clear", ":help", ":quit"}
	if view.WorkItem == nil {
		return commands
	}
	if allowed(view.WorkItem.AllowedActions, "ask_free_question") {
		commands = append([]string{":ask"}, commands...)
	}
	if view.Session.State == "AwaitingResponse" && allowed(view.WorkItem.AllowedActions, "submit_attempt") {
		commands = append([]string{":answer"}, commands...)
	}
	if allowed(view.WorkItem.AllowedActions, "convert_free_answer_to_quiz") {
		commands = append([]string{":quiz"}, commands...)
	}
	if allowed(view.WorkItem.AllowedActions, "resume_focus") {
		commands = append([]string{":resume"}, commands...)
	}
	if view.WorkItem.Assessment != nil && view.WorkItem.AssessmentDecision != nil {
		commands = append([]string{":assessment"}, commands...)
	}
	if allowed(view.WorkItem.AllowedActions, "end_activity") {
		commands = append(commands, ":end")
	}
	if allowed(view.WorkItem.AllowedActions, "complete_session") {
		commands = append(commands, ":complete")
	}
	return commands
}

func (a *App) askFreeQuestion(ctx context.Context, client APIClient, view api.SessionView, question string) (api.SessionView, error) {
	question = strings.TrimSpace(question)
	if view.WorkItem == nil || !allowed(view.WorkItem.AllowedActions, "ask_free_question") {
		return view, commandError("invalid_state", "free questions are not allowed in the current state", "use a displayed allowed action", ExitConflict)
	}
	if question == "" || len([]rune(question)) > 8000 || !utf8.ValidString(question) {
		return view, commandError("invalid_question", "question must be valid UTF-8 with 1 to 8000 characters", "enter a shorter question", ExitInput)
	}
	operationID, err := a.operationID()
	if err != nil {
		return view, err
	}
	fresh, _, err := a.applyAndRefetch(ctx, client, view, api.ActionQuestionRequest{
		SessionOperation: sessionOperation(view, operationID), Action: "ask_free_question", Question: question,
	})
	return fresh, err
}

func (a *App) convertFreeAnswerToQuiz(ctx context.Context, client APIClient, view api.SessionView) (api.SessionView, error) {
	if view.WorkItem == nil || view.WorkItem.FreeQuestion == nil || view.WorkItem.FreeAnswer == nil || !allowed(view.WorkItem.AllowedActions, "convert_free_answer_to_quiz") {
		return view, commandError("invalid_state", "an attached quiz is not allowed in the current state", "use :resume or ask another question", ExitConflict)
	}
	question, answer := view.WorkItem.FreeQuestion, view.WorkItem.FreeAnswer
	retrieval, err := a.retrieveForWorkItem(ctx, client, view, question.Text, question.KnowledgeRevisionID)
	if err != nil {
		return view, err
	}
	requestID, err := a.operationID()
	if err != nil {
		return view, err
	}
	request, err := proposalRequest(view, "activity", retrieval, requestID)
	if err != nil {
		return view, err
	}
	proposal, fresh, stale, err := a.createProposalAndRefetch(ctx, client, view, request)
	if err != nil {
		return view, err
	}
	if stale {
		return fresh, nil
	}
	operationID, err := a.operationID()
	if err != nil {
		return view, err
	}
	fresh, _, err = a.applyAndRefetch(ctx, client, fresh, api.ActionAttachedQuizRequest{
		SessionOperation: sessionOperation(fresh, operationID), Action: "convert_free_answer_to_quiz",
		ProposalID: proposal.ProposalID, Question: question.FreeQuestionID, Answer: answer.FreeAnswerID,
	})
	return fresh, err
}

func (a *App) showLearnProgress(ctx context.Context, client APIClient, view api.SessionView) error {
	status, err := client.ProjectionStatus(ctx)
	if err != nil {
		return mapAPIError(err)
	}
	printProjectionWarning(a.Err, status.Metadata)
	_, _ = fmt.Fprintf(a.Out, "Current: session=%s state=%s active_time=%ds estimated=%t samples=%d\n", safeText(view.Session.SessionID), safeText(view.Session.State), view.EstimatedActiveTime.DurationSeconds, view.EstimatedActiveTime.Estimated, view.EstimatedActiveTime.SampleCount)
	if view.Session.Focus.FocusNodeRevisionID != "" {
		node, nodeErr := client.Node(ctx, view.Session.Focus.FocusNodeRevisionID)
		if nodeErr != nil {
			return mapAPIError(nodeErr)
		}
		printProjectionWarning(a.Err, node.Metadata)
		_, _ = fmt.Fprintf(a.Out, "Node: %s mastery=%s evidence=%d pending_assessments=%d\n", safeText(node.Node.Mastery.NodeRevisionID), safeText(node.Node.Mastery.State), node.Node.Mastery.ValidEvidenceCount, node.Node.Mastery.PendingAssessments)
	}
	return nil
}

func (a *App) learnCommandPrompt(ctx context.Context, client APIClient, view api.SessionView) (api.SessionView, bool, error) {
	if view.WorkItem != nil {
		_, _ = fmt.Fprintf(a.Out, "Allowed actions: %s\n", safeText(strings.Join(view.WorkItem.AllowedActions, ",")))
	}
	for {
		line, err := a.readLearnLine("> ")
		if err != nil {
			var commandErr *Error
			if errors.As(err, &commandErr) {
				return view, false, commandErr
			}
			if errors.Is(err, io.EOF) {
				return view, false, commandError("input_closed", "interactive input ended", "run learn again to resume", ExitInput)
			}
			return view, false, commandError("input_failed", "interactive input could not be read", "retry in a working terminal", ExitInput)
		}
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, ":") {
			return view, false, commandError("invalid_input", "plain text is not accepted in the current state", "use a displayed interactive command", ExitInput)
		}
		return a.handleLearnCommand(ctx, client, view, trimmed)
	}
}

func (a *App) readLearnLine(prompt string) (string, error) {
	nextPrompt := prompt
	for {
		line, err := a.Terminal.ReadLine(nextPrompt)
		if err != nil {
			return "", err
		}
		if !terminal.IsControlL([]byte(line)) && strings.TrimSpace(line) != ":clear" {
			return line, nil
		}
		if err := a.clearLearn(); err != nil {
			return "", err
		}
		nextPrompt = ""
	}
}

func learnInputError(err error, detail, next string) error {
	var commandErr *Error
	if errors.As(err, &commandErr) {
		return commandErr
	}
	return commandError("input_closed", detail, next, ExitInput)
}

func (a *App) clearLearn() error {
	if err := a.Terminal.Clear(); err != nil {
		return commandError("not_a_terminal", "interactive clear requires a TTY and emits no fallback control sequence", "continue without clearing", ExitInput)
	}
	return nil
}
