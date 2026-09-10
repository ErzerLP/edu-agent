package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

type workbenchService struct{ app App }

func (s workbenchService) Load(ctx context.Context, req workbench.Request) (result workbench.Page, resultErr error) {
	defer func() {
		var e *Error
		if errors.As(resultErr, &e) && (e.Code == "content_redacted" || e.Code == "privacy_clear_in_progress") {
			resultErr = &workbench.Failure{Message: "内容已清除或正在隐私清除；本地草稿已释放", ClearDrafts: true}
		}
	}()
	a := s.app
	a.learningSpace = req.Space
	a.learningSessions = map[string]string{}
	if req.Session != "" {
		a.learningSessions[req.Space] = req.Session
	}
	a.strictLearningConflicts = true
	a.Out, a.Err = io.Discard, io.Discard
	p := workbench.Page{Title: "学习工作台"}
	if req.Page == "help" {
		p.Content = "1–7 切页；方向键/Tab 选择可见操作，Enter 执行；PgUp/PgDn 滚动正文。\n输入中 Enter 换行、Tab 切字段、Ctrl+S 提交、Esc 保留草稿返回。\n导入页 F10 返回工作台，保留草稿；预览和确认仍按导入页提示操作。\n取消仅停止等待，远端可能已提交；刷新原对象核对。\n保存目标不会启动或切换教学；目标页可明确新建教学会话，学习页可选择已有会话。\n目标/教学详情可打开明确绑定的 Agent；Agent F2 恢复聊天，F7 选择其他区/目标。聊天不重放教学动作。\n总览与复习页可切换到全局范围，点击续学保留真实目标和会话。"
		return p, nil
	}
	online, err := a.openOnline(onlineFlags{})
	if err != nil {
		return p, err
	}
	client := online.client
	spaces, ok := client.(spaceClient)
	if !ok {
		return p, commandError("learning_spaces_unsupported", "服务不支持学习区", "升级服务端", ExitUnavailable)
	}
	if req.Page == "spaces" {
		page, err := spaces.LearningSpaces(ctx, req.Search, "", req.Cursor, 50)
		if err != nil {
			return p, mapAPIError(err)
		}
		p.Title, p.Content, p.NextCursor = "全局 · 选择学习区", "选择学习区；不改变其他区的目标或教学。", page.NextCursor
		for _, space := range page.Items {
			p.Entries = append(p.Entries, workbench.Entry{ID: space.ID, Label: space.Name + "（" + space.Status + "）"})
			p.Content += "\n" + space.Name + " · " + space.Description
		}
		if len(page.Items) == 0 {
			p.Content = "没有匹配的学习区。"
		}
		return p, nil
	}
	space, err := spaces.LearningSpace(ctx, req.Space)
	if err != nil {
		return p, mapAPIError(err)
	}
	p.SpaceName = space.Name
	a.learningSpaceName = space.Name
	caps, err := spaces.LearningSpacesCapabilities(ctx)
	if err != nil {
		return p, mapAPIError(err)
	}
	module := "learning"
	if req.Page == "materials" || req.Page == "collection" || req.Page == "shared" || req.Page == "import" {
		module = "knowledge"
	}
	if req.Page == "learn" || req.Page == "session" || req.Page == "sessions" {
		module = "tutoring"
	}
	if req.Space != api.DefaultLearningSpaceID && caps.Modules[module] == "default_only" {
		p.Content = "当前服务仅支持默认学习区的此项能力；没有读取其他区数据。"
		return p, nil
	}
	switch req.Page {
	case "overview", "global-overview", "goal-progress", "global-reviews":
		return a.workbenchProgress(ctx, client, req, p)
	case "materials", "collection", "shared":
		return a.workbenchMaterials(ctx, client, req, p)
	case "import":
		real, ok := client.(*api.Client)
		if !ok {
			return p, fmt.Errorf("客户端不支持导入向导")
		}
		draft := &importDraft{space: req.Space, spaceName: space.Name}
		draft.collection.ID = req.Resource
		p.Leaf = &embeddedImport{importModel: newImportModel(ctx, real, draft, a.NewUUID)}
		return p, nil
	case "goals", "goal", "new-goal", "history":
		return a.workbenchGoals(ctx, client, req, p)
	case "reviews":
		return a.workbenchProgress(ctx, client, req, p)
	case "sessions":
		return a.workbenchSessions(ctx, client, req, p)
	}
	if req.Page == "session" {
		req.Session = req.Resource
	}
	if req.Session == "" {
		if req.Page == "overview" {
			p.Content = "从资料页查看或导入内容，在目标页创建目标，再选择教学会话开始学习。\n保存目标不自动开始教学。"
			return p, nil
		}
		return a.workbenchSessions(ctx, client, req, p)
	}
	view, err := refetchSession(ctx, client, req.Session)
	if err != nil {
		return p, mapAPIError(err)
	}
	explanation := ""
	var routePreview *api.TutoringProposal
	if req.Action != "" {
		if view.Session.SessionID != req.Session || view.Session.AggregateVersion != req.Version {
			return p, commandError("version_conflict", "教学上下文已改变，未重放输入", "刷新后重新确认", ExitConflict)
		}
		switch req.Action {
		case "apply_route":
			var proposal api.TutoringProposal
			var stale bool
			proposal, view, stale, err = a.diagnosticRouteProposal(ctx, client, view)
			if err == nil && !stale {
				routePreview = &proposal
			}
		case "confirm_route":
			view, _, err = a.proposalAction(ctx, client, view, "apply_route", req.Basis)
		case "record_exposure":
			view, explanation, err = a.recordExplanation(ctx, client, view)
		case "assessment_void", "assessment_override":
			view, err = a.workbenchAssessment(ctx, client, view, req)
		default:
			view, err = a.workbenchAction(ctx, client, view, req.Action, req.Text)
		}
		if err != nil {
			return p, err
		}
	}
	p.Session, p.Version, p.ContextKnown = view.Session.SessionID, view.Session.AggregateVersion, true
	p.Stage = view.Session.State
	if view.WorkItem != nil && view.WorkItem.GoalRevision != nil {
		p.Goal = view.WorkItem.GoalRevision.GoalManagement().Details.Name
		p.Entries = append(p.Entries, workbench.Entry{ID: "agent:" + view.WorkItem.GoalRevision.GoalID + "/" + view.Session.SessionID, Label: "打开绑定此教学上下文的 AI 聊天"})
	}
	p.Title = "区内 · 结构化学习"
	p.Content = sessionContent(view, true)
	if explanation != "" {
		p.Content += "\n\n讲解（不计分）：\n" + explanation
	}
	p.Entries = append(p.Entries, workbench.Entry{ID: "page:sessions", Label: "选择其他教学会话（不结束当前学习）"})
	if view.Metadata.Degraded || view.Metadata.Incomplete || view.Metadata.Rebuilding {
		p.Content += "\n投影状态：" + strings.Join(view.Metadata.ReasonCodes, ", ")
	}
	p.Actions = learningActions(view)
	if routePreview != nil {
		p.Basis = routePreview.ProposalID
		p.Content += "\n\n待确认路线（尚未采用）："
		for i, step := range routePreview.Route {
			p.Content += fmt.Sprintf("\n%d. %s\n完成依据：%s\n资料节点：%s", i+1, step.TeachingIntent, step.CompletionCondition, step.NodeRevisionID)
		}
		p.Content += "\n如需编辑，可返回目标的 Agent 入口打开正式规划流程，或使用 goal plan。返回或刷新不会采用本提案。"
		p.Actions = []workbench.Action{{ID: "confirm_route", Label: "确认采用已展示路线", Confirmation: "确认将以上路线用于当前教学会话？"}}
	}
	for i := range p.Actions {
		p.Actions[i].DraftKey = "/" + view.Session.SessionID + "/" + view.Session.Focus.GoalRevisionID + "/" + view.Session.Focus.ActivityID
	}
	return p, nil
}

func sessionContent(view api.SessionView, active bool) string {
	if !active {
		return "没有当前教学会话。请到目标页创建学习目标。"
	}
	text := "教学会话：" + view.Session.SessionID + "\n阶段：" + view.Session.State
	w := view.WorkItem
	if w == nil {
		return text
	}
	if w.GoalRevision != nil {
		text += "\n目标：" + w.GoalRevision.Text
	}
	if w.RouteRevision != nil {
		for _, step := range w.RouteRevision.Steps {
			text += "\n路线：" + step.TeachingIntent + " · " + step.CompletionCondition
		}
	}
	if w.Activity != nil {
		text += "\n\n题目：\n" + w.Activity.Prompt
	}
	if w.Attempt != nil {
		text += "\n\n作答：\n" + w.Attempt.Answer
	}
	if w.Assessment != nil {
		for _, item := range w.Assessment.Items {
			text += "\n反馈：" + item.Conclusion + "\n作答证据：" + item.AnswerQuote + "\n知识证据：" + item.KnowledgeQuote
		}
	}
	if w.AssessmentDecision != nil {
		text += "\n评估处置：" + w.AssessmentDecision.Disposition
	}
	if w.FreeQuestion != nil {
		text += "\n\n自由提问：" + w.FreeQuestion.Text
	}
	if w.FreeAnswer != nil {
		text += "\n\n自由回答（不计分）：" + w.FreeAnswer.Text
	}
	return text
}

func learningActions(view api.SessionView) []workbench.Action {
	if view.WorkItem == nil {
		return nil
	}
	labels := map[string]string{
		"start_diagnostic": "开始诊断", "apply_route": "生成路线预览", "issue_activity": "开始练习", "present_review": "开始复习", "present_activity": "开始作答", "record_assessment": "获取评估反馈", "acknowledge_feedback": "确认反馈并继续", "ask_free_question": "自由提问", "record_free_answer": "获取自由回答", "resume_focus": "回到原教学焦点", "convert_free_answer_to_quiz": "将自由回答转为练习", "end_activity": "结束当前活动", "complete_session": "完成教学会话",
	}
	var actions []workbench.Action
	for _, id := range view.WorkItem.AllowedActions {
		if id == "submit_attempt" && view.WorkItem.Activity != nil {
			for _, help := range view.WorkItem.Activity.AllowedHelp {
				actions = append(actions, workbench.Action{ID: "submit_attempt:" + help, Label: "提交答案（帮助等级：" + help + "）", Input: "输入多行答案"})
			}
			continue
		}
		label := labels[id]
		if label == "" {
			continue
		}
		a := workbench.Action{ID: id, Label: label}
		if id == "ask_free_question" {
			a.Input = "输入自由问题"
		}
		if id == "end_activity" || id == "complete_session" {
			a.Confirmation = "此操作可能使当前教学焦点失效，是否继续？"
		}
		actions = append(actions, a)
	}
	if allowed(view.WorkItem.AllowedAssessmentDecisions, "confirm") {
		actions = append(actions, workbench.Action{ID: "assessment_confirm", Label: "确认评估", Confirmation: "确认当前评估结果？"})
	}
	if allowed(view.WorkItem.AllowedActions, "record_exposure") {
		actions = append(actions, workbench.Action{ID: "record_exposure", Label: "获取当前节点讲解"})
	}
	if allowed(view.WorkItem.AllowedAssessmentDecisions, "void") {
		actions = append(actions, workbench.Action{ID: "assessment_void", Label: "作废评估", Input: "作废原因", Confirmation: "确认作废当前评估？"})
	}
	if allowed(view.WorkItem.AllowedAssessmentDecisions, "override") {
		actions = append(actions, workbench.Action{ID: "assessment_override", Label: "覆盖评估结论", Fields: assessmentFields(view), Confirmation: "确认按这些结论覆盖评估？不可变证据仍由原业务规则校验。"})
	}
	return actions
}

func (a *App) workbenchAction(ctx context.Context, client APIClient, view api.SessionView, action, text string) (api.SessionView, error) {
	if strings.HasPrefix(action, "submit_attempt:") {
		return a.submitAttemptWithHelp(ctx, client, view, text, strings.TrimPrefix(action, "submit_attempt:"))
	}
	switch action {
	case "issue_activity", "present_review":
		return a.issueRouteActivity(ctx, client, view, action)
	case "record_assessment":
		return a.learnEvaluating(ctx, client, view)
	case "ask_free_question":
		return a.askFreeQuestion(ctx, client, view, text)
	case "record_free_answer":
		return a.learnFreeQuestion(ctx, client, view)
	case "convert_free_answer_to_quiz":
		return a.convertFreeAnswerToQuiz(ctx, client, view)
	case "assessment_confirm":
		return a.learnAssessmentDecision(ctx, client, view, "confirm")
	case "start_diagnostic", "present_activity", "acknowledge_feedback", "resume_focus", "end_activity", "complete_session":
		fresh, _, err := a.noFieldAction(ctx, client, view, action)
		return fresh, err
	}
	return view, commandError("unsupported_action", "此动作尚未接入工作台", "使用现有 learn 入口", ExitUnavailable)
}
