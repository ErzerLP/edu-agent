package command

import (
	"context"
	"encoding/json"
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
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action = args[0]
		args = args[1:]
	}
	if action == "help" {
		_, err := fmt.Fprintln(a.Out, "learn [browse|list|show|start] [--goal UUID] [--session UUID] [--space UUID]\n默认选择目标和会话；start 开始独立新会话；show 只读历史。\n原生页面 F2 可在请求等待中切换，Esc 返回但不结束教学。\n:switch 切目标/会话，:space 切区，:pause 暂停目标，:complete 结束本次学习。\n草稿仅保留在当前进程，重启后不保留，不写明文。")
		return err
	}
	set := newFlagSet("learn")
	var flags onlineFlags
	var sessionID, goalID string
	set.StringVar(&sessionID, "session", "", "指定教学会话")
	set.StringVar(&goalID, "goal", "", "指定目标")
	addOnlineFlags(set, &flags)
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "学习参数无效", "使用 learn help", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	if sessionID != "" && goalID != "" {
		return commandError("usage", "会话和目标选择不能混用", "使用 learn help", ExitInput)
	}
	if action == "list" {
		c, ok := online.client.(teachingSessionClient)
		if !ok {
			return commandError("session_selection_unavailable", "会话列表不可用", "升级客户端", ExitUnavailable)
		}
		page, err := c.Sessions(ctx, goalID, "", "", 100)
		if err != nil {
			return mapAPIError(err)
		}
		return json.NewEncoder(a.Out).Encode(page)
	}
	if action != "" && action != "browse" && action != "start" && action != "show" {
		return commandError("usage", "未知学习命令", "使用 learn help", ExitInput)
	}
	if action == "show" && sessionID == "" || action == "start" && goalID == "" {
		return commandError("usage", "缺少目标或会话参数", "使用 learn help", ExitInput)
	}
	if action == "" && sessionID == "" && goalID == "" {
		sessionID = a.learningSessions[a.teachingSpace()]
	}
	for {
		var view api.SessionView
		switch {
		case action == "start":
			view, err = a.startGoalSession(ctx, online.client, goalID)
		case sessionID != "":
			view, err = refetchSession(ctx, online.client, sessionID)
		default:
			view, err = a.pickTeachingSession(ctx, online.client, goalID)
		}
		if err != nil {
			return mapAPIError(err)
		}
		if view.Session.SessionID == "" {
			return nil
		}
		if action == "show" {
			return json.NewEncoder(a.Out).Encode(view)
		}
		a.selectTeachingSession(view.Session.SessionID)
		err = a.runTeachingLoop(ctx, online.client, view)
		if !errors.Is(err, errSelectTeachingSession) {
			return err
		}
		online, err = a.openOnline(flags)
		if err != nil {
			return err
		}
		sessionID = ""
		goalID = ""
		action = "browse"
	}
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
	view, err := refetchSession(ctx, client, sessionID)
	if err != nil {
		return api.SessionView{}, mapAPIError(err)
	}
	return view, nil
}

func (a *App) learnLoop(ctx context.Context, client APIClient, view api.SessionView) error {
	if view.WorkItem != nil && view.WorkItem.GoalRevision != nil {
		_, _ = fmt.Fprintf(a.Out, "学习区：%s · 目标：%s\n", safeText(a.teachingSpaceLabel()), safeText(view.WorkItem.GoalRevision.GoalManagement().Details.Name))
	}
	for {
		printProjectionWarning(a.Err, view.Metadata)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if terminal, ok := a.Terminal.(interface{ SetTeachingFocus(api.SessionView) }); ok {
			terminal.SetTeachingFocus(view)
		}
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
			_, err := fmt.Fprintf(a.Out, "Result: session=%s completed active_time=%ds estimated=%t samples=%d\n本次教学已完成。goal set 可独立保存后续目标。\n", safeText(view.Session.SessionID), view.EstimatedActiveTime.DurationSeconds, view.EstimatedActiveTime.Estimated, view.EstimatedActiveTime.SampleCount)
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
	knowledgeID := view.WorkItem.GoalRevision.GoalManagement().Details.ScopeSnapshotID
	if knowledgeID == "" {
		head, err := client.KnowledgeHead(ctx)
		if err != nil {
			return api.SessionView{}, mapAPIError(err)
		}
		knowledgeID = head.RevisionID
	}
	retrieval, err := a.retrieveForWorkItem(ctx, client, view, view.WorkItem.GoalRevision.Text, knowledgeID)
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
	confirmed, confirmErr := a.Terminal.Confirm("确认采用以上路线？如需编辑，请先返回 goal plan；取消不会应用路线。")
	if confirmErr != nil || !confirmed {
		return fresh, commandError("planning_confirmation_required", "路线尚未采用", "使用 goal plan --id 目标ID 编辑规划，或重新进入学习", ExitInput)
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
		if a.teachingSpace() == api.DefaultLearningSpaceID {
			resolvedView, _, metadata, refreshRequired, err := a.currentDueReview(ctx, client, view, time.Now().UTC())
			if err != nil {
				return view, false, err
			}
			if refreshRequired {
				return resolvedView, false, nil
			}
			view = resolvedView
			for _, pageMetadata := range metadata {
				printProjectionWarning(a.Err, pageMetadata)
			}
		}
		// 服务端 work_item 已按真实目标核验当前焦点的复习资格。
		confirmed, confirmErr := a.Terminal.Confirm(a.dashboardText("A review is due for the current node. Present it now?", "当前知识节点已到复习时间，是否现在开始复习？"))
		if confirmErr != nil {
			return view, false, commandError("confirmation_failed", "review confirmation could not be read", "retry in an interactive terminal", ExitInput)
		}
		if confirmed {
			fresh, err := a.issueRouteActivity(ctx, client, view, "present_review")
			return fresh, false, err
		}
	}
	if allowed(view.WorkItem.AllowedActions, "record_exposure") {
		fresh, explanation, err := a.recordExplanation(ctx, client, view)
		if err != nil {
			return view, false, err
		}
		if explanation != "" {
			_, _ = fmt.Fprintf(a.Out, "Current: explanation (not scored)\n%s\n", safeText(explanation))
		}
		if explanation == "" || fresh.Session.State != "RouteActive" {
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

func (a *App) currentDueReview(ctx context.Context, client APIClient, view api.SessionView, dueBefore time.Time) (api.SessionView, *api.ReviewSchedule, []api.ProjectionMetadata, bool, error) {
	current := view
	for attempt := 0; attempt <= progressSnapshotRestartLimit; attempt++ {
		pages, err := collectProjectionPages(current.Metadata.Generation, maxProgressPages, func(cursor string) (api.ProjectionMetadata, []api.ReviewSchedule, string, error) {
			var page api.ReviewsPage
			var pageErr error
			if reader, ok := client.(progressClient); ok && current.WorkItem != nil && current.WorkItem.GoalRevision != nil {
				page, pageErr = reader.ScopedReviews(ctx, api.ProgressQuery{GoalID: current.WorkItem.GoalRevision.GoalID, Status: "all", Cursor: cursor, Limit: defaultPageLimit}, &dueBefore)
			} else {
				page, pageErr = client.Reviews(ctx, cursor, defaultPageLimit, &dueBefore)
			}
			return page.Metadata, page.Items, page.NextCursor, pageErr
		})
		if err != nil {
			return view, nil, nil, false, err
		}
		if pages.restartReason == "" {
			for index := range pages.items {
				if pages.items[index].GoalRevisionID != "" && pages.items[index].GoalRevisionID != current.Session.Focus.GoalRevisionID {
					continue
				}
				if pages.items[index].NodeRevisionID == current.Session.Focus.FocusNodeRevisionID {
					return current, &pages.items[index], pages.metadata, false, nil
				}
			}
			if pages.truncated {
				return view, nil, nil, false, commandError("review_lookup_truncated", "the due review for the current node is beyond the bounded page budget", "run reviews to inspect due items, then retry learn", ExitConflict)
			}
			return view, nil, nil, false, commandError("projection_unavailable", "the session allows a review but no due review exists for the current node", "retry after the learning projection is rebuilt", ExitUnavailable)
		}
		if attempt == progressSnapshotRestartLimit {
			return view, nil, nil, false, commandError("unstable_review_snapshot", "due reviews changed while locating the current node", "retry after the projection stabilizes", ExitConflict)
		}
		if pages.restartReason == "stale_cursor" {
			_, _ = fmt.Fprintln(a.Err, "warning[stale_cursor]: due reviews changed; restarting without combining old pages")
		} else {
			_, _ = fmt.Fprintln(a.Err, "warning[progress_snapshot]: review generation changed; restarting without combining old pages")
		}
		fresh, refetchErr := refetchSession(ctx, client, current.Session.SessionID)
		if refetchErr != nil {
			return view, nil, nil, false, mapAPIError(refetchErr)
		}
		if fresh.Session.State != "RouteActive" || fresh.WorkItem == nil || !allowed(fresh.WorkItem.AllowedActions, "present_review") ||
			fresh.Session.AggregateVersion != current.Session.AggregateVersion || fresh.Session.Focus.FocusNodeRevisionID != current.Session.Focus.FocusNodeRevisionID {
			return fresh, nil, nil, true, nil
		}
		current = fresh
	}
	return view, nil, nil, false, commandError("unstable_review_snapshot", "due reviews changed while locating the current node", "retry after the projection stabilizes", ExitConflict)
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
				answer, blockErr := a.readMultilineAnswer(view.Session.SessionID + "/" + view.WorkItem.Activity.ActivityID)
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

func (a *App) readMultilineAnswer(key string) (string, error) {
	_, _ = fmt.Fprintln(a.Err, a.dashboardText("Enter answer lines; a single . ends the block.", "请输入多行答案；单独输入一行 . 结束。"))
	if a.learningDrafts == nil {
		a.learningDrafts = map[string][]string{}
	}
	lines := append([]string(nil), a.learningDrafts[key]...)
	total := len(strings.Join(lines, "\n"))
	if len(lines) > 0 {
		_, _ = fmt.Fprintf(a.Err, "已恢复本题 %d 行进程内草稿，继续输入或用 . 提交。\n", len(lines))
	}
	for {
		line, err := a.Terminal.ReadLine("")
		if err != nil {
			a.learningDrafts[key] = lines
			return "", commandError("input_closed", "multiline answer ended before the terminator", "run learn again and end the block with a single .", ExitInput)
		}
		if line == ":switch" {
			a.learningDrafts[key] = lines
			return "", errSelectTeachingSession
		}
		if line == ":discard" {
			lines = nil
			total = 0
			delete(a.learningDrafts, key)
			continue
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
	a.learningDrafts[key] = lines
	if strings.TrimSpace(answer) == "" || !utf8.ValidString(answer) {
		return "", commandError("invalid_answer", "multiline answer must be non-empty valid UTF-8", "enter the answer again", ExitInput)
	}
	return answer, nil
}

func (a *App) submitAttempt(ctx context.Context, client APIClient, view api.SessionView, answer string) (api.SessionView, error) {
	if view.WorkItem == nil || view.WorkItem.Activity == nil || !allowed(view.WorkItem.AllowedActions, "submit_attempt") {
		return view, commandError("invalid_state", "submit_attempt is not allowed by the current work item", "refresh the session and use a displayed allowed action", ExitConflict)
	}
	if a.learningDrafts == nil {
		a.learningDrafts = map[string][]string{}
	}
	a.learningDrafts[view.Session.SessionID+"/"+view.WorkItem.Activity.ActivityID] = strings.Split(answer, "\n")
	help, err := a.chooseHelp(view.WorkItem.Activity.AllowedHelp)
	if err != nil {
		return view, err
	}
	return a.submitAttemptWithHelp(ctx, client, view, answer, help)
}

// submitAttemptWithHelp 复用作答请求构造，界面只负责输入与帮助等级选择。
func (a *App) submitAttemptWithHelp(ctx context.Context, client APIClient, view api.SessionView, answer, help string) (api.SessionView, error) {
	if view.WorkItem == nil || view.WorkItem.Activity == nil || !allowed(view.WorkItem.AllowedActions, "submit_attempt") || !allowed(view.WorkItem.Activity.AllowedHelp, help) {
		return view, commandError("invalid_state", "answer or help is not allowed", "refresh the session", ExitConflict)
	}
	if strings.TrimSpace(answer) == "" || !utf8.ValidString(answer) || len(answer) > 262144 {
		return view, commandError("invalid_answer", "answer must contain non-empty UTF-8 text up to 262144 bytes", "edit the answer", ExitInput)
	}
	operationID, err := a.operationID()
	if err != nil {
		return view, err
	}
	fresh, _, err := a.applyAndRefetch(ctx, client, view, api.ActionAttemptRequest{
		SessionOperation: sessionOperation(view, operationID), Action: "submit_attempt", Answer: answer, Help: help,
	})
	if err == nil {
		delete(a.learningDrafts, view.Session.SessionID+"/"+view.WorkItem.Activity.ActivityID)
	}
	return fresh, err
}

func (a *App) chooseHelp(values []string) (string, error) {
	_, _ = fmt.Fprintf(a.Out, "Allowed help: %s\n", safeText(strings.Join(values, ",")))
	defaultNone := allowed(values, "none")
	prompt := a.dashboardText("Help: ", "帮助等级：")
	if defaultNone {
		prompt = a.dashboardText("Help [none]: ", "帮助等级 [none]：")
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
		reason, readErr := a.Terminal.ReadLine(a.dashboardText("Override reason: ", "覆盖评估原因："))
		if readErr != nil || strings.TrimSpace(reason) == "" {
			return view, commandError("invalid_assessment_reason", "override requires a non-empty reason", "retry :assessment override", ExitInput)
		}
		items, collectErr := a.collectAssessmentOverride(*view.WorkItem.Assessment, *decision)
		if collectErr != nil {
			return view, collectErr
		}
		request = api.AssessmentOverrideRequest{SessionOperation: base, Kind: "override", ExpectedDispositionVersion: decision.Version, Reason: strings.TrimSpace(reason), Items: items}
	case "void":
		reason, readErr := a.Terminal.ReadLine(a.dashboardText("Void reason: ", "作废评估原因："))
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
	case ":switch":
		return view, false, errSelectTeachingSession
	case ":pause":
		c, ok := client.(goalClient)
		if !ok || view.WorkItem == nil || view.WorkItem.GoalRevision == nil {
			return view, false, commandError("invalid_state", "当前目标不可用", "刷新会话", ExitConflict)
		}
		goal, err := c.Goal(ctx, view.WorkItem.GoalRevision.GoalID)
		if err != nil {
			return view, false, mapAPIError(err)
		}
		op, err := a.operationID()
		if err != nil {
			return view, false, err
		}
		request := goalRequest(goal, op)
		request.Action = "pause"
		if _, err := c.ReviseGoal(ctx, request); err != nil {
			return view, false, mapAPIError(err)
		}
		return view, true, nil
	case ":space":
		if c, ok := client.(spaceClient); ok {
			if err := a.browseSpaces(ctx, c); err != nil {
				return view, false, err
			}
			return view, false, errSelectTeachingSession
		}
		return view, false, commandError("learning_spaces_unsupported", "学习区入口不可用", "升级客户端", ExitUnavailable)
	case ":quit":
		return view, true, nil
	case ":ask":
		if argument == "" {
			value, err := a.Terminal.ReadLine(a.dashboardText("Question: ", "问题："))
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
		if a.teachingSpace() != api.DefaultLearningSpaceID {
			_, _ = fmt.Fprintf(a.Out, "当前会话焦点可开始复习：%t；跨目标复习总览不在本入口提供。\n", view.WorkItem != nil && allowed(view.WorkItem.AllowedActions, "present_review"))
			return view, false, nil
		}
		page, err := a.reviewsPage(ctx, client, "", defaultPageLimit, nil)
		if err != nil {
			return view, false, err
		}
		printReviewsPage(a.Out, a.Err, page)
		return view, false, nil
	case ":end":
		confirmed, err := a.Terminal.Confirm(a.dashboardText("Ending the activity may invalidate the active focus. Continue?", "结束当前活动可能使学习焦点失效，是否继续？"))
		if err != nil || !confirmed {
			return view, false, commandError("end_activity_declined", "the activity was not ended", "continue learning or retry :end", ExitInput)
		}
		fresh, _, err := a.noFieldAction(ctx, client, view, "end_activity")
		return fresh, false, err
	case ":complete":
		confirmed, err := a.Terminal.Confirm(a.dashboardText("Completing the session may invalidate the active focus. Continue?", "完成当前学习会话可能使学习焦点失效，是否继续？"))
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
	commands := []string{":switch", ":space", ":pause", ":progress", ":route", ":reviews", ":clear", ":help", ":quit"}
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
	fresh, err := refetchSession(ctx, client, view.Session.SessionID)
	if err != nil {
		return mapAPIError(err)
	}
	view = fresh
	printProjectionWarning(a.Err, view.Metadata)
	_, _ = fmt.Fprintf(a.Out, "Current: session=%s state=%s active_time=%ds estimated=%t samples=%d\n", safeText(view.Session.SessionID), safeText(view.Session.State), view.EstimatedActiveTime.DurationSeconds, view.EstimatedActiveTime.Estimated, view.EstimatedActiveTime.SampleCount)
	if view.WorkItem != nil && view.WorkItem.RouteRevision != nil {
		printRoute(a.Out, *view.WorkItem.RouteRevision, true, view.Session.Focus.RouteStepID)
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
