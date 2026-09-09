package learning

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

// PlanningContent 是对话和表单共享的建议，不是目标或掌握度真值。
type PlanningContent struct {
	Details   GoalDetails        `json:"details"`
	Steps     []PlanningStep     `json:"steps"`
	Questions []PlanningQuestion `json:"questions"`
	Gaps      []string           `json:"gaps"`
}
type PlanningQuestion struct {
	Field    string `json:"field"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
}
type PlanningStep struct {
	Name           string `json:"name"`
	Content        string `json:"content"`
	Reason         string `json:"reason"`
	Exercise       string `json:"exercise"`
	Completion     string `json:"completion"`
	Minutes        int    `json:"minutes"`
	Prerequisites  []int  `json:"prerequisites"`
	NodeRevisionID string `json:"node_revision_id"`
}
type PlanningSource struct {
	Name      string             `json:"name"`
	Reference KnowledgeReference `json:"reference"`
}
type PlanningDraft struct {
	ID                    string           `json:"id"`
	Version               int64            `json:"version"`
	Goal                  GoalRevision     `json:"goal"`
	Content               PlanningContent  `json:"content"`
	Suggestion            *PlanningContent `json:"suggestion,omitempty"`
	Sources               []PlanningSource `json:"sources"`
	SessionID             string           `json:"session_id,omitempty"`
	SessionVersion        int64            `json:"session_version,omitempty"`
	State                 string           `json:"state"`
	StaleReasons          []string         `json:"stale_reasons"`
	LastError             string           `json:"last_error,omitempty"`
	ModelSource           string           `json:"model_source"`
	AppliedSessionID      string           `json:"applied_session_id,omitempty"`
	AppliedGoalRevisionID string           `json:"applied_goal_revision_id,omitempty"`
}
type PlanningCommand struct {
	OperationID     string           `json:"operation_id"`
	ExpectedVersion int64            `json:"expected_version"`
	Action          string           `json:"action"`
	Content         *PlanningContent `json:"content,omitempty"`
	SessionID       string           `json:"session_id,omitempty"`
	Target          string           `json:"target,omitempty"`
	UpdateGoal      bool             `json:"update_goal,omitempty"`
}
type PlanningStore interface {
	ReadPlanning(context.Context, string, string) (PlanningDraft, error)
	ListPlanning(context.Context, string) ([]PlanningDraft, error)
	ChangePlanning(context.Context, string, string, string, PlanningCommand, func(context.Context, ApplicationStore, ProposalRepository, *PlanningDraft) error) (PlanningDraft, error)
}
type PlanningKnowledge interface {
	PlanningSources(context.Context, string) ([]PlanningSource, bool, error)
}

func (s *Service) PlanningList(ctx context.Context, goalID string) ([]PlanningDraft, error) {
	p, ok := s.authority.(PlanningStore)
	if !ok {
		return nil, &Error{Code: CodeNotFound}
	}
	return p.ListPlanning(ctx, goalID)
}
func (s *Service) Planning(ctx context.Context, goalID, id string) (PlanningDraft, error) {
	p, ok := s.authority.(PlanningStore)
	if !ok {
		return PlanningDraft{}, &Error{Code: CodeNotFound}
	}
	d, err := p.ReadPlanning(ctx, goalID, id)
	if err != nil {
		return d, err
	}
	// 已应用的记录是历史回执，不把本次确认产生的新版本误报为过期。
	if d.State == "applied" {
		return d, nil
	}
	err = s.inspectPlanning(ctx, &d)
	return d, err
}
func (s *Service) inspectPlanning(ctx context.Context, d *PlanningDraft) error {
	d.StaleReasons = []string{}
	g, err := s.GetGoal(ctx, d.Goal.GoalID)
	if err != nil {
		return err
	}
	if g.ID != d.Goal.ID {
		d.StaleReasons = append(d.StaleReasons, "目标版本已变化，请新建草稿并核对差异")
	}
	if status := g.GoalManagement().Status; status != "draft" && status != "active" {
		d.StaleReasons = append(d.StaleReasons, "目标当前不允许开始学习")
	}
	if d.SessionID != "" {
		a, e := s.authority.LoadSessionAuthority(ctx, d.SessionID)
		if e != nil {
			return e
		}
		if a.Session.AggregateVer != d.SessionVersion {
			d.StaleReasons = append(d.StaleReasons, "适用教学状态已变化，请新建草稿")
		}
	}
	sources, changed, err := s.planningSources(ctx, d.Content.Details.ScopeSnapshotID)
	if err != nil {
		return err
	}
	d.Sources = sources
	if changed {
		d.StaleReasons = append(d.StaleReasons, "资料集合已有新版本，请重新选择并冻结范围")
	}
	return nil
}
func (s *Service) planningSources(ctx context.Context, id string) ([]PlanningSource, bool, error) {
	if id == "" {
		return []PlanningSource{}, false, nil
	}
	k, ok := s.knowledge.(PlanningKnowledge)
	if !ok {
		return nil, false, &Error{Code: CodeKnowledgeReferenceInvalid}
	}
	return k.PlanningSources(ctx, id)
}

func ValidatePlanningContent(c PlanningContent) error {
	invalid := func() error { return &Error{Code: CodeInvalidRequest, Reason: "invalid_planning_content"} }
	if err := c.Details.Validate(); err != nil {
		return err
	}
	if len(c.Steps) > 100 || len(c.Questions) > 7 || len(c.Gaps) > 30 {
		return invalid()
	}
	validText := func(v string) bool { return utf8.ValidString(v) && utf8.RuneCountInString(v) <= 4000 }
	for _, v := range c.Gaps {
		if !validText(v) {
			return invalid()
		}
	}
	fields := map[string]bool{}
	for _, q := range c.Questions {
		if _, ok := planningKnown(c.Details)[q.Field]; !ok || fields[q.Field] || !validText(q.Question) || !validText(q.Answer) {
			return invalid()
		}
		fields[q.Field] = true
	}
	nodes := map[string]bool{}
	for i, st := range c.Steps {
		if strings.TrimSpace(st.Name) == "" || strings.TrimSpace(st.Content) == "" || strings.TrimSpace(st.Completion) == "" || st.Minutes < 1 || st.Minutes > 10080 {
			return invalid()
		}
		for _, v := range []string{st.Name, st.Content, st.Reason, st.Exercise, st.Completion} {
			if !validText(v) {
				return invalid()
			}
		}
		if st.NodeRevisionID != "" {
			if !learningspace.ValidID(st.NodeRevisionID) || nodes[st.NodeRevisionID] {
				return invalid()
			}
			nodes[st.NodeRevisionID] = true
		}
		seen := map[int]bool{}
		for _, p := range st.Prerequisites {
			if p < 0 || p >= i || seen[p] {
				return invalid()
			}
			seen[p] = true
		}
	}
	return nil
}
func planningKnown(d GoalDetails) map[string]string {
	timeKnown := ""
	if d.WeeklyMinutes != nil || d.Deadline != nil {
		timeKnown = "已填写时间约束"
	}
	return map[string]string{"expected_outcome": d.ExpectedOutcome, "self_assessment": d.SelfAssessment, "purpose": d.Purpose, "scope": d.Scope, "exclusions": d.Exclusions, "completion_criteria": d.CompletionCriteria, "time": timeKnown}
}

func (s *Service) ChangePlanning(ctx context.Context, device, goalID, id string, c PlanningCommand) (PlanningDraft, error) {
	if !learningspace.ValidID(device) || !learningspace.ValidID(goalID) || !learningspace.ValidID(id) || !learningspace.ValidID(c.OperationID) || c.ExpectedVersion < 0 {
		return PlanningDraft{}, &Error{Code: CodeInvalidRequest}
	}
	switch c.Action {
	case "create", "edit", "generate", "confirm":
	default:
		return PlanningDraft{}, &Error{Code: CodeInvalidRequest}
	}
	if c.SessionID != "" && !learningspace.ValidID(c.SessionID) {
		return PlanningDraft{}, &Error{Code: CodeInvalidRequest}
	}
	if (c.Action == "create") != (c.ExpectedVersion == 0) || (c.Action != "edit" && c.Content != nil) || (c.Action != "confirm" && (c.Target != "" || c.UpdateGoal)) || (c.Action != "create" && c.SessionID != "") {
		return PlanningDraft{}, &Error{Code: CodeInvalidRequest}
	}
	p, ok := s.authority.(PlanningStore)
	if !ok {
		return PlanningDraft{}, &Error{Code: CodeNotFound}
	}
	// 网络取消后仍用有界事务保存真实结果；模型请求本身继续响应取消。
	durable, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	return p.ChangePlanning(durable, device, goalID, id, c, func(txctx context.Context, a ApplicationStore, r ProposalRepository, d *PlanningDraft) error {
		local := *s
		local.authority = a
		local.queries = a
		local.proposals = r
		if c.Action == "create" {
			g, err := local.GetGoal(txctx, goalID)
			if err != nil {
				return err
			}
			*d = PlanningDraft{ID: id, Goal: g, Content: PlanningContent{Details: g.GoalManagement().Details, Steps: []PlanningStep{}, Questions: []PlanningQuestion{}, Gaps: []string{}}, State: "draft", Sources: []PlanningSource{}, StaleReasons: []string{}, ModelSource: "服务端教学模型配置（不使用客户端 API Key）：" + s.modelID}
			if c.SessionID != "" {
				v, e := a.LoadSessionAuthority(txctx, c.SessionID)
				if e != nil {
					return e
				}
				if v.Session.Context.GoalRevisionID != g.ID {
					return &Error{Code: CodeStaleProposal}
				}
				d.SessionID = c.SessionID
				d.SessionVersion = v.Session.AggregateVer
			}
		} else if d.State == "applied" {
			return &Error{Code: CodeInvalidTransition, Reason: "planning_already_applied"}
		}
		if c.Action == "edit" {
			if c.Content == nil {
				return &Error{Code: CodeInvalidRequest}
			}
			if err := ValidatePlanningContent(*c.Content); err != nil {
				return err
			}
			d.Content = *c.Content
			d.LastError = ""
		}
		if err := local.inspectPlanning(txctx, d); err != nil {
			return err
		}
		if c.Action == "edit" {
			if err := validatePlanningReferences(d.Content, d.Sources, false); err != nil {
				return err
			}
		}
		if c.Action == "generate" {
			if len(d.StaleReasons) > 0 {
				return &Error{Code: CodeStaleProposal}
			}
			local.generatePlanning(ctx, d)
		}
		if c.Action == "confirm" {
			if len(d.StaleReasons) > 0 {
				return &Error{Code: CodeStaleProposal}
			}
			if err := local.applyPlanning(txctx, device, d, c); err != nil {
				return err
			}
			d.State = "applied"
		}
		normalizePlanning(&d.Content)
		if d.Suggestion != nil {
			normalizePlanning(d.Suggestion)
		}
		d.Version = c.ExpectedVersion + 1
		return nil
	})
}

func normalizePlanning(c *PlanningContent) {
	if c.Steps == nil {
		c.Steps = []PlanningStep{}
	}
	if c.Questions == nil {
		c.Questions = []PlanningQuestion{}
	}
	if c.Gaps == nil {
		c.Gaps = []string{}
	}
	for i := range c.Steps {
		if c.Steps[i].Prerequisites == nil {
			c.Steps[i].Prerequisites = []int{}
		}
	}
}
func validatePlanningReferences(c PlanningContent, sources []PlanningSource, executable bool) error {
	known := map[string]bool{}
	for _, src := range sources {
		known[src.Reference.NodeRevisionID] = true
	}
	if executable && len(c.Steps) == 0 {
		return &Error{Code: CodeProposalRejected, Reason: "planning_outline_only"}
	}
	for _, st := range c.Steps {
		if st.NodeRevisionID == "" && !executable {
			continue
		}
		if !known[st.NodeRevisionID] {
			return &Error{Code: CodeKnowledgeReferenceInvalid, Reason: "planning_reference_outside_scope"}
		}
	}
	return nil
}
func (s *Service) generatePlanning(ctx context.Context, d *PlanningDraft) {
	d.LastError = ""
	if s.model == nil {
		d.LastError = "模型未配置；草稿已保留，可继续手动编辑"
		return
	}
	input, _ := json.Marshal(map[string]any{"instruction": "只建议学习规划；复用已填信息，不重复提问。步骤前置关系是零起始序号。只能引用给定资料；无资料可以输出无引用大纲与不确定的缺口。不得推断用户掌握度。", "goal": d.Goal, "draft": d.Content, "sources": d.Sources})
	request := ProposalRequest{Type: ProposalType("planning"), Input: input}
	var raw json.RawMessage
	var err error
	for i := 0; i < 2; i++ {
		call, cancel := context.WithTimeout(ctx, 20*time.Second)
		raw, err = s.model.Generate(call, request)
		cancel()
		if err == nil || !retryableModelCategory(modelCategory(err)) || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		d.LastError = "模型请求失败（" + modelCategory(err) + "）；原草稿已保留"
		return
	}
	var output PlanningContent
	if len(raw) > 256<<10 {
		d.LastError = "模型结果超过草稿容量；原草稿已保留"
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decodeOne(decoder, &output); err == nil {
		err = ValidatePlanningContent(output)
	}
	if err == nil {
		if output.Details.ScopeSnapshotID != d.Content.Details.ScopeSnapshotID {
			err = &Error{Code: CodeKnowledgeReferenceInvalid}
		} else {
			err = validatePlanningReferences(output, d.Sources, false)
		}
	}
	if err != nil {
		d.LastError = "模型结构或资料引用无效；原草稿已保留"
		return
	}
	// 过滤已知信息的问题；建议保持独立，用户选择接受后才成为可编辑内容。
	known := planningKnown(d.Content.Details)
	for _, q := range d.Content.Questions {
		if strings.TrimSpace(q.Answer) != "" {
			known[q.Field] = q.Answer
		}
	}
	questions := []PlanningQuestion{}
	for _, q := range output.Questions {
		if strings.TrimSpace(known[q.Field]) == "" {
			questions = append(questions, q)
		}
	}
	output.Questions = questions
	d.Suggestion = &output
}

type planningRouteModel struct{ route []RouteProposalStep }

func (m planningRouteModel) Generate(context.Context, ProposalRequest) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"route": m.route})
}

func (s *Service) applyPlanning(ctx context.Context, device string, d *PlanningDraft, c PlanningCommand) error {
	if c.Target != "goal" && c.Target != "new_session" && c.Target != "current_session" {
		return &Error{Code: CodeInvalidRequest}
	}
	if err := ValidatePlanningContent(d.Content); err != nil {
		return err
	}
	if c.Target != "goal" {
		if err := validatePlanningReferences(d.Content, d.Sources, true); err != nil {
			return err
		}
	}
	goal := d.Goal
	operation := func(kind, id string, version int64) OperationEnvelope {
		return OperationEnvelope{OperationID: s.newUUID(), PayloadSchemaVersion: 1, AggregateType: kind, AggregateID: id, ExpectedVersion: version, Payload: json.RawMessage(`{}`)}
	}
	if c.UpdateGoal {
		result, err := s.CreateGoal(ctx, device, GoalCommand{Operation: operation("goal", goal.GoalID, goal.Revision), GoalID: goal.GoalID, Text: goal.Text, Source: "planning-confirmed", PreviousRevisionID: &goal.ID, Details: &d.Content.Details})
		if err != nil {
			return err
		}
		if err = json.Unmarshal(result.Result, &goal); err != nil {
			return err
		}
	} else if c.Target == "goal" || goal.GoalManagement().Details.ScopeSnapshotID != d.Content.Details.ScopeSnapshotID {
		return &Error{Code: CodeInvalidRequest, Reason: "planning_goal_confirmation_required"}
	}
	d.AppliedGoalRevisionID = goal.ID
	if c.Target == "goal" {
		return nil
	}
	sessionID := d.SessionID
	if c.Target == "new_session" {
		sessionID = s.newUUID()
		if _, err := s.CreateSession(ctx, device, SessionCommand{Operation: operation("session", sessionID, 0), GoalRevisionID: goal.ID}); err != nil {
			return err
		}
	} else if sessionID == "" {
		return &Error{Code: CodeInvalidRequest}
	}
	a, err := s.authority.LoadSessionAuthority(ctx, sessionID)
	if err != nil {
		return err
	}
	if a.Session.State != tutoring.StateGoalReady && a.Session.State != tutoring.StateDiagnostic && a.Session.State != tutoring.StateRouteActive {
		return &Error{Code: CodeInvalidTransition, Reason: "planning_choose_new_session"}
	}
	if a.Session.Context.GoalRevisionID != goal.ID {
		if _, err = s.ApplyAction(ctx, device, sessionID, ActionCommand{Operation: operation("session", sessionID, a.Session.AggregateVer), Action: tutoring.ActionSwitchGoal, GoalRevisionID: goal.ID}); err != nil {
			return err
		}
		a, err = s.authority.LoadSessionAuthority(ctx, sessionID)
		if err != nil {
			return err
		}
	}
	if a.Session.State == tutoring.StateGoalReady {
		if _, err = s.ApplyAction(ctx, device, sessionID, ActionCommand{Operation: operation("session", sessionID, a.Session.AggregateVer), Action: tutoring.ActionStartDiagnostic}); err != nil {
			return err
		}
		a, err = s.authority.LoadSessionAuthority(ctx, sessionID)
		if err != nil {
			return err
		}
	}
	route := []RouteProposalStep{}
	nodes := []string{}
	for _, st := range d.Content.Steps {
		nodes = append(nodes, st.NodeRevisionID)
		route = append(route, RouteProposalStep{NodeRevisionID: st.NodeRevisionID, TeachingIntent: st.Name + "：" + st.Content, CompletionCondition: st.Completion})
	}
	manual := *s
	manual.model = planningRouteModel{route}
	manual.modelID = "user-reviewed-planning"
	manual.promptRevision = "planning-confirm-v1"
	req := ProposalRequest{RequestID: s.newUUID(), Type: ProposalRoute, AggregateType: "session", AggregateID: sessionID, AggregateVersion: a.Session.AggregateVer, KnowledgeRevisionID: d.Content.Details.ScopeSnapshotID, NodeRevisionIDs: nodes, Input: json.RawMessage(`{"source":"confirmed_planning"}`)}
	proposal, err := manual.Propose(ctx, device, req)
	if err != nil {
		return err
	}
	if _, err = s.ApplyAction(ctx, device, sessionID, ActionCommand{Operation: operation("session", sessionID, a.Session.AggregateVer), Action: tutoring.ActionApplyRoute, ProposalID: proposal.ID}); err != nil {
		return err
	}
	d.AppliedSessionID = sessionID
	return nil
}
