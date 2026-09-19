package command

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

func studyJSON(v any) json.RawMessage { raw, _ := json.Marshal(v); return raw }

func (a *App) workbenchStudy(ctx context.Context, client APIClient, req workbench.Request, p workbench.Page) (workbench.Page, error) {
	c, ok := client.(studyClient)
	if !ok {
		return p, fmt.Errorf("客户端不支持新学习服务，请升级")
	}
	call := func(action string, q api.StudyQuery, body any) (api.StudyDocument, error) {
		var raw json.RawMessage
		if body != nil {
			raw = studyJSON(body)
		}
		d, err := c.Study(ctx, action, q, raw)
		if err != nil {
			return nil, mapAPIError(err)
		}
		return d, nil
	}
	p.Title = "正式学习服务"
	p.Content = "已保存状态来自同一服务；未提交草稿只保留在原进程/标签页。\n取消等待不代表远端取消，结果未知请刷新原对象核对。\n"
	first, second, _ := strings.Cut(req.Resource, "/")
	switch req.Page {
	case "study-goal", "study-adjust":
		gclient, ok := client.(goalClient)
		if !ok {
			return p, fmt.Errorf("目标管理不可用")
		}
		g, err := gclient.Goal(ctx, first)
		if err != nil {
			return p, mapAPIError(err)
		}
		p.Goal, p.Version = g.GoalManagement().Details.Name, g.Revision
		p.Content += goalContent(g)
		if req.Action != "" {
			requests, e := strconv.Atoi(req.Values["requests"])
			if e != nil {
				return p, fmt.Errorf("请求预算须为正整数")
			}
			tokens, e := strconv.Atoi(req.Values["tokens"])
			if e != nil {
				return p, fmt.Errorf("Token 预算须为正整数")
			}
			body := map[string]any{"operation_id": req.Operation, "session_id": req.Entity, "expected_version": req.Version, "prompt": req.Values["prompt"], "save": true, "request_budget": requests, "token_budget": tokens}
			var sessions map[string]string
			if json.Unmarshal([]byte(req.Basis), &sessions) != nil {
				return p, fmt.Errorf("运行恢复依据缺失，请刷新目标页面后重试")
			}
			if session := sessions[req.Action]; session != "" {
				body["session_id"] = session
			}
			if req.Action == "mentor" {
				view, e := refetchSession(ctx, client, second)
				if e != nil {
					return p, mapAPIError(e)
				}
				if view.WorkItem == nil || view.WorkItem.GoalRevision == nil || view.WorkItem.GoalRevision.GoalID != first {
					return p, fmt.Errorf("教学会话不属于此目标")
				}
				body["teaching_session_id"] = second
			} else {
				body["research"] = map[string]any{"topic": req.Values["prompt"], "external_consent": true, "auto_adopt": req.Action == "start", "policy": map[string]any{"mode": "supplement", "domains": []string{}}}
				if req.Action == "start" {
					body["start_learning"] = map[string]any{"new_session": true, "model_consent": true}
				}
			}
			result, e := call(req.Action, api.StudyQuery{Goal: first}, body)
			if e != nil {
				return p, e
			}
			p.Redirect = "study-run/" + result.String("run_id")
			return p, nil
		}
		fields := []workbench.Field{{ID: "prompt", Label: "确认可外发的公开研究主题（不超过 300 字节）"}, {ID: "requests", Label: "本次请求预算", Value: "10"}, {ID: "tokens", Label: "本次 Token 预算", Value: "20000"}}
		if req.Page == "study-adjust" {
			fields[0].Label = "向服务端导师请求本会话的路径调整"
			p.Actions = []workbench.Action{{ID: "mentor", Label: "请求教学调整", Fields: fields, Confirmation: "允许服务端模型读取此目标与明确选择的教学会话并提出变更？具体差异随后在变更页审阅。"}}
		} else {
			p.Actions = []workbench.Action{{ID: "research", Label: "仅研究公开主题", Fields: fields, Confirmation: "允许按以上公开主题搜索和调用模型？本次不自动采纳来源，不开始课堂。"}, {ID: "start", Label: "研究资料并开始新的课堂", Fields: fields, Confirmation: "允许搜索、模型准备并自动采纳来源，为此目标创建新课堂？此操作有模型副作用。"}}
		}
		p.Entries = []workbench.Entry{{ID: "page:study-runs", Label: "查看原设备运行与恢复"}, {ID: "page:study-library/" + first, Label: "打开此目标的内容库"}, {ID: "page:study-changes/" + first, Label: "审阅此目标的教学变更"}, {ID: "page:sessions/" + first, Label: "按名称选择课堂继续"}}
		sessions := map[string]string{}
		for _, action := range p.Actions {
			kind := action.ID
			if kind == "start" {
				kind = "start_learning"
			}
			d, e := call("current", api.StudyQuery{Goal: first, Kind: kind}, nil)
			if e != nil {
				return p, e
			}
			if run := d.Object("run"); run != nil {
				sessions[action.ID] = run.String("session_id")
				p.Entries = append(p.Entries, workbench.Entry{ID: "page:study-run/" + run.String("run_id"), Label: "恢复「" + action.Label + "」· " + run.String("status")})
			}
		}
		p.Basis = string(studyJSON(sessions))
	case "study-runs":
		d, err := call("runs", api.StudyQuery{Cursor: req.Cursor, Limit: 20}, nil)
		if err != nil {
			return p, err
		}
		p.NextCursor = d.String("next_cursor")
		p.Content += "运行属于原设备。其他端共享正式教学会话、内容与变更；不会接管设备私有聊天。\n"
		goals := map[string]string{}
		for _, item := range d.Items("items") {
			goal := item.String("goal_id")
			name := goals[goal]
			if name == "" {
				if gc, ok := client.(goalClient); ok {
					g, e := gc.Goal(ctx, goal)
					if e != nil {
						return p, mapAPIError(e)
					}
					name = g.GoalManagement().Details.Name
				}
				goals[goal] = name
			}
			p.Entries = append(p.Entries, workbench.Entry{ID: "page:study-run/" + item.String("run_id"), Label: name + " · " + item.String("kind") + " · " + item.String("status") + " · " + item.String("updated_at")})
		}
	case "study-run":
		d, err := call("run", api.StudyQuery{Run: first}, nil)
		if err != nil {
			return p, err
		}
		if req.Action != "" {
			body := map[string]any{"operation_id": req.Operation, "expected_version": req.Version, "kind": req.Action}
			if req.Action == "respond" {
				body["interaction_id"] = req.Basis
				body["answer"] = req.Text
			}
			if req.Action == "continue_budget" || req.Action == "retry_start" {
				requests, e := strconv.Atoi(req.Values["requests"])
				if e != nil {
					return p, fmt.Errorf("请求预算须为正整数")
				}
				tokens, e := strconv.Atoi(req.Values["tokens"])
				if e != nil {
					return p, fmt.Errorf("Token 预算须为正整数")
				}
				body["request_budget"], body["token_budget"] = requests, tokens
			}
			if _, err = call("run-command", api.StudyQuery{Run: first}, body); err != nil {
				return p, err
			}
			p.Redirect = "study-run/" + first
			return p, nil
		}
		p.Version, p.Basis = d.Number("version"), d.Object("interaction").String("id")
		p.Content += studyText("run", d) + fmt.Sprintf("\n剩余请求 %d · 剩余 Token %d · 结果未知=%t\n", d.Number("requests_left"), d.Number("tokens_left"), d.Bool("result_unknown"))
		if i := d.Object("interaction"); i != nil {
			p.Content += "\n" + i.String("question") + "\n可选回答：" + string(i["choices"])
			if !i.Bool("reference_selection") && i.String("memory_candidate_id") == "" {
				p.Actions = append(p.Actions, workbench.Action{ID: "respond", Label: "回答当前服务端交互", Input: "输入明确答案", Confirmation: "将回答提交给此运行中的具体交互？"})
			} else {
				p.Content += "\n此专用审批请回原 Web 入口处理；CLI 不代作决定。"
			}
		}
		if d.Bool("body_available") {
			p.Actions = append(p.Actions, workbench.Action{ID: "clear", Label: "清除此运行保存的正文", Confirmation: "清除原运行正文并停止运行？已发布的知识、课堂和答案仍按原规则保留。"})
		}
		if studyRunWaiting(d.String("status")) || d.String("status") == "waiting_input" || d.String("status") == "waiting_approval" || d.String("status") == "paused_budget" {
			p.Actions = append(p.Actions, workbench.Action{ID: "stop", Label: "停止此运行", Confirmation: "请求服务端停止此运行？已完成结果仍保留。"})
		}
		budget := []workbench.Field{{ID: "requests", Label: "新增请求预算", Value: "10"}, {ID: "tokens", Label: "新增 Token 预算", Value: "20000"}}
		if d.String("status") == "paused_budget" {
			p.Actions = append(p.Actions, workbench.Action{ID: "continue_budget", Label: "明确增加预算继续", Fields: budget, Confirmation: "允许原运行使用新增预算继续模型请求？"})
		}
		start := d.Object("start_learning")
		if result := start.Object("result"); result != nil {
			p.Entries = append(p.Entries, workbench.Entry{ID: "page:session/" + result.String("session_id"), Label: "继续此运行创建的正式课堂"}, workbench.Entry{ID: "page:study-content/" + result.String("artifact_id"), Label: "阅读本课堂正式内容"})
		} else if d.String("kind") == "start_learning" && (d.String("status") == "partial" || d.String("status") == "failed") && d.Bool("body_available") {
			p.Actions = append(p.Actions, workbench.Action{ID: "retry_start", Label: "按新预算恢复开学", Fields: budget, Confirmation: "授权恢复原开学运行？复用已有来源，失败步骤可能重新请求。"})
		}
		if d.String("kind") == "research" || d.String("kind") == "start_learning" {
			p.Entries = append(p.Entries, workbench.Entry{ID: "page:study-sources/" + first, Label: "查看研究来源及采用状态"})
		}
		p.Entries = append(p.Entries, workbench.Entry{ID: "page:study-changes/" + d.String("goal_id"), Label: "查看此目标的正式变更"})
	case "study-sources":
		d, err := call("sources", api.StudyQuery{Run: first}, nil)
		if err != nil {
			return p, err
		}
		for _, s := range d.Items("items") {
			p.Entries = append(p.Entries, workbench.Entry{ID: "page:study-source/" + first + "/" + s.String("id"), Label: s.String("title") + " · " + s.String("status")})
		}
	case "study-source":
		d, err := call("source", api.StudyQuery{Run: first, Source: second}, nil)
		if err != nil {
			return p, err
		}
		run, err := call("run", api.StudyQuery{Run: first}, nil)
		if err != nil {
			return p, err
		}
		if req.Action != "" {
			_, err = call("source-decision", api.StudyQuery{Run: first, Source: second}, map[string]any{"operation_id": req.Operation, "expected_version": req.Version, "kind": req.Action, "accept_partial": true})
			if err != nil {
				return p, err
			}
			p.Redirect = "study-source/" + req.Resource
			return p, nil
		}
		p.Version = run.Number("version")
		p.Content += studyText("source", d)
		p.Actions = []workbench.Action{{ID: "adopt", Label: "采用已读来源", Confirmation: "采用此来源进入本目标知识参考？部分解析只覆盖页面已显示的片段。"}, {ID: "reject", Label: "拒绝此来源", Confirmation: "拒绝此运行中的来源？已发布历史不由此撤销。"}}
	case "study-library":
		d, err := call("library", api.StudyQuery{Goal: first, Cursor: req.Cursor, Limit: 20}, nil)
		if err != nil {
			return p, err
		}
		p.NextCursor = d.String("next_cursor")
		for _, item := range d.Items("items") {
			p.Entries = append(p.Entries, workbench.Entry{ID: "page:study-content/" + item.String("artifact_id"), Label: item.String("title") + " · " + item.String("kind") + fmt.Sprintf(" · v%d", item.Number("version"))})
		}
	case "study-current":
		view, err := refetchSession(ctx, client, first)
		if err != nil {
			return p, mapAPIError(err)
		}
		if view.WorkItem == nil || view.WorkItem.Activity == nil {
			p.Content += "此课堂暂无活动正文。"
			return p, nil
		}
		d, err := call("ensure", api.StudyQuery{Session: first}, map[string]any{"protocol_version": 1, "activity_id": view.WorkItem.Activity.ActivityID})
		if err != nil {
			return p, err
		}
		p.Redirect = "study-content/" + d.String("artifact_id")
	case "study-content":
		version := int64(0)
		if second != "" {
			var err error
			version, err = strconv.ParseInt(second, 10, 64)
			if err != nil {
				return p, err
			}
		}
		d, err := call("content", api.StudyQuery{Artifact: first, Version: version}, nil)
		if err != nil {
			return p, err
		}
		view, err := refetchSession(ctx, client, d.String("session_id"))
		if err != nil {
			return p, mapAPIError(err)
		}
		if strings.HasPrefix(req.Action, "answer:") {
			contentVersion, e := strconv.ParseInt(req.Basis, 10, 64)
			if e != nil {
				return p, e
			}
			body := map[string]any{"operation_id": req.Operation, "payload_schema_version": 1, "aggregate_type": "session", "aggregate_id": d.String("session_id"), "expected_version": req.Version, "action": "submit_attempt", "content_version": contentVersion, "answer": req.Text, "help": strings.TrimPrefix(req.Action, "answer:")}
			if _, err = call("answer", api.StudyQuery{Artifact: first}, body); err != nil {
				return p, err
			}
			p.Redirect = "session/" + d.String("session_id")
			return p, nil
		}
		p.Version, p.Basis = view.Session.AggregateVersion, strconv.FormatInt(d.Number("version"), 10)
		p.Content += fmt.Sprintf("正文版本 %d · 正式版本 %d\n", d.Number("version"), d.Number("committed_version")) + api.ContentText(d)
		kind := d.Object("body").Object("interaction").String("kind")
		if view.WorkItem != nil && view.WorkItem.Activity != nil && view.WorkItem.Activity.ActivityID == d.String("activity_id") && allowed(view.WorkItem.AllowedActions, "submit_attempt") && d.Number("version") == d.Number("committed_version") && (kind == "text" || kind == "single_choice") {
			for _, help := range view.WorkItem.Activity.AllowedHelp {
				p.Actions = append(p.Actions, workbench.Action{ID: "answer:" + help, Label: "提交本题答案 · " + help, Input: "文本答案；选择题填写显示的选项值", DraftKey: "/" + d.String("session_id") + "/" + d.String("activity_id")})
			}
		}
		p.Entries = append(p.Entries, workbench.Entry{ID: "page:session/" + d.String("session_id"), Label: "返回原课堂继续（已提交答案共享）"}, workbench.Entry{ID: "page:study-history/" + first, Label: "选择内容历史版本"}, workbench.Entry{ID: "page:study-context/" + d.String("session_id"), Label: "查看原课堂知识上下文"})
		for i, ref := range d.Object("body").Items("references") {
			p.Entries = append(p.Entries, workbench.Entry{ID: fmt.Sprintf("page:study-citation/%s/%d/%s", first, d.Number("version"), ref.String("node_revision_id")), Label: fmt.Sprintf("引用 %d · %s", i+1, ref.String("slice"))})
		}
	case "study-context":
		d, err := call("context", api.StudyQuery{Session: first}, nil)
		if err != nil {
			return p, err
		}
		p.Content += studyText("context", d)
	case "study-citation":
		versionText, source, ok := strings.Cut(second, "/")
		version, err := strconv.ParseInt(versionText, 10, 64)
		if !ok || err != nil {
			return p, fmt.Errorf("引用须绑定具体正文版本")
		}
		d, err := call("citation", api.StudyQuery{Artifact: first, Source: source, Version: version}, nil)
		if err != nil {
			return p, err
		}
		p.Content += studyText("citation", d)
	case "study-history":
		d, err := call("history", api.StudyQuery{Artifact: first}, nil)
		if err != nil {
			return p, err
		}
		for _, item := range d.Items("items") {
			p.Entries = append(p.Entries, workbench.Entry{ID: fmt.Sprintf("page:study-content/%s/%d", first, item.Number("version")), Label: fmt.Sprintf("版本 %d · %s · %s", item.Number("version"), item.String("status"), item.String("created_at"))})
		}
	case "study-changes":
		d, err := call("changes", api.StudyQuery{Goal: first}, nil)
		if err != nil {
			return p, err
		}
		for _, item := range d.Items("items") {
			p.Entries = append(p.Entries, workbench.Entry{ID: "page:study-change/" + first + "/" + item.String("id"), Label: item.String("impact") + " · " + item.String("status") + fmt.Sprintf(" · 修订 %d", item.Number("revision"))})
		}
	case "study-change":
		d, err := call("change", api.StudyQuery{Goal: first, Change: second}, nil)
		if err != nil {
			return p, err
		}
		if req.Action != "" {
			var body map[string]any
			if json.Unmarshal([]byte(req.Basis), &body) != nil {
				return p, fmt.Errorf("审阅依据无效，请刷新")
			}
			body["operation_id"], body["action"], body["immediate"] = req.Operation, req.Action, false
			if _, err = call("change-command", api.StudyQuery{Goal: first, Change: second}, body); err != nil {
				return p, err
			}
			p.Redirect = "study-change/" + req.Resource
			return p, nil
		}
		p.Version = d.Number("revision")
		p.Basis = string(studyJSON(map[string]any{"session_id": d.String("session_id"), "expected_revision": d.Number("revision"), "hash": d.String("hash"), "interaction_id": d.String("interaction_id")}))
		p.Content += "批准、排队和生效分别显示；默认等当前活动处理后安全接入。\n" + studyText("change", d)
		for _, action := range []struct{ id, label string }{{"approve", "批准具体候选，按安全边界接入"}, {"reject", "拒绝具体候选"}, {"cancel", "取消尚未生效候选"}, {"apply_now", "立即应用并保存原焦点"}, {"compensate", "创建补偿候选"}, {"restore_focus", "恢复保留的原焦点"}} {
			p.Actions = append(p.Actions, workbench.Action{ID: action.id, Label: action.label, Confirmation: "确认对当前展示的具体版本执行“" + action.label + "”？服务端会重新检查版本与安全边界。"})
		}
	default:
		return p, fmt.Errorf("未知学习服务页面")
	}
	p.Content = studySafeLines(p.Content)
	return p, nil
}
