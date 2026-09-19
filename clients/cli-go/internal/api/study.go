package api

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

// StudyDocument 保留新协议的扩展字段；旧教学与离线 DTO 仍严格解码。
// 每个入口另行核对版本和归属，展示字段不参与本地领域决策。
type StudyDocument map[string]json.RawMessage

func (d StudyDocument) String(key string) string {
	var value string
	_ = json.Unmarshal(d[key], &value)
	return value
}
func (d StudyDocument) Number(key string) int64 {
	var value int64
	_ = json.Unmarshal(d[key], &value)
	return value
}
func (d StudyDocument) Bool(key string) bool {
	var value bool
	_ = json.Unmarshal(d[key], &value)
	return value
}
func (d StudyDocument) Object(key string) StudyDocument {
	var value StudyDocument
	_ = json.Unmarshal(d[key], &value)
	return value
}
func (d StudyDocument) Items(key string) []StudyDocument {
	var value []StudyDocument
	_ = json.Unmarshal(d[key], &value)
	return value
}

type StudyQuery struct {
	Goal, Session, Run, Artifact, Change, Source, Operation string
	Kind, Status, Cursor                                    string
	Version                                                 int64
	Limit                                                   int
}

func studyUnsupported() error {
	return &APIError{Status: 501, Code: "learning_services_upgrade_required"}
}
func studyInvalid() error { return &APIError{Status: 400, Code: "invalid_study_request"} }

func (c *Client) studyCapability(ctx context.Context, name string) (StudyDocument, error) {
	path, version := "/v1/capabilities", "schema_version"
	if name != "runtime" {
		path, version = "/v1/learning/"+name+"/capabilities", "protocol_version"
	}
	var result StudyDocument
	status, err := c.doJSONStatus(ctx, "GET", path, true, nil, map[int]bool{200: true}, true, &result)
	if status == 404 || status == 405 || status == 501 {
		return nil, studyUnsupported()
	}
	if err != nil {
		return nil, err
	}
	if result.Number(version) != 1 {
		return nil, studyUnsupported()
	}
	return result, nil
}

// Study 只映射明确的正式命令，不允许任意 URL、Shell 或 SQL。
// 所有写请求只发送一次；调用方保留原 operation_id 查询或显式重试。
func (c *Client) Study(ctx context.Context, action string, q StudyQuery, input json.RawMessage) (StudyDocument, error) {
	for _, id := range []string{q.Goal, q.Session, q.Run, q.Artifact, q.Change, q.Source, q.Operation} {
		if id != "" && !validLearningUUID(id) {
			return nil, studyInvalid()
		}
	}
	if q.Version < 0 || q.Limit < 0 || q.Limit > 100 {
		return nil, studyInvalid()
	}
	if action == "capabilities" {
		result := StudyDocument{}
		for _, name := range []string{"runtime", "start", "content", "changes"} {
			capability, err := c.studyCapability(ctx, name)
			if err != nil {
				return nil, err
			}
			result[name], _ = json.Marshal(capability)
		}
		return result, nil
	}
	method, path, capability, feature := "GET", "", "runtime", ""
	query := url.Values{}
	need := func(ids ...string) bool {
		for _, id := range ids {
			if id == "" {
				return false
			}
		}
		return true
	}
	limit := q.Limit
	if limit == 0 {
		limit = 20
	}
	switch action {
	case "current":
		end := map[string]string{"research": "research", "start_learning": "start", "mentor": "runs", "content_edit": "content-edits"}[q.Kind]
		if !need(q.Goal) || end == "" {
			return nil, studyInvalid()
		}
		path = "/v1/learning/goals/" + q.Goal + "/" + end
	case "research", "start", "mentor":
		if !need(q.Goal) {
			return nil, studyInvalid()
		}
		method, path, feature = "POST", "/v1/learning/goals/"+q.Goal+"/runs", "research"
		if action == "mentor" {
			feature = "web_mentor"
		}
		if action == "start" {
			capability, feature = "start", "available"
		}
	case "runs":
		path = "/v1/learning/runs"
		query = url.Values{"limit": {strconv.Itoa(limit)}, "cursor": {q.Cursor}, "task_kind": {q.Kind}, "status": {q.Status}}
	case "run", "run-command", "sources", "source", "source-decision":
		if !need(q.Run) {
			return nil, studyInvalid()
		}
		path = "/v1/learning/runs/" + q.Run
		switch action {
		case "run-command":
			method, path = "POST", path+"/commands"
		case "sources":
			path += "/sources"
		case "source", "source-decision":
			if !need(q.Source) {
				return nil, studyInvalid()
			}
			path += "/sources/" + q.Source
			if action == "source-decision" {
				method, path = "POST", path+"/decisions"
			}
		}
	case "operation":
		if !need(q.Operation) {
			return nil, studyInvalid()
		}
		path = "/v1/learning/operations/" + q.Operation
	case "session-operation":
		if !need(q.Session, q.Operation) {
			return nil, studyInvalid()
		}
		path = "/v1/tutoring/sessions/" + q.Session + "/operations/" + q.Operation
	case "context", "ensure":
		if !need(q.Session) {
			return nil, studyInvalid()
		}
		path = "/v1/tutoring/sessions/" + q.Session + "/knowledge-context"
		if action == "ensure" {
			method, path, capability = "POST", "/v1/tutoring/sessions/"+q.Session+"/content", "content"
		}
	case "library":
		path, capability = "/v1/learning/content", "content"
		query = url.Values{"goal_id": {q.Goal}, "kind": {q.Kind}, "limit": {strconv.Itoa(limit)}, "cursor": {q.Cursor}}
	case "content", "history", "citation", "answer":
		if !need(q.Artifact) {
			return nil, studyInvalid()
		}
		path, capability = "/v1/learning/content/"+q.Artifact, "content"
		switch action {
		case "content":
			if q.Version > 0 {
				query.Set("version", strconv.FormatInt(q.Version, 10))
			}
		case "history":
			path += "/revisions"
		case "citation":
			if !need(q.Source) || q.Version < 1 {
				return nil, studyInvalid()
			}
			path += "/sources/" + q.Source
			query.Set("version", strconv.FormatInt(q.Version, 10))
		case "answer":
			method, path = "POST", path+"/answers"
		}
	case "changes", "change", "change-context", "change-command":
		if !need(q.Goal) {
			return nil, studyInvalid()
		}
		path, capability = "/v1/learning/goals/"+q.Goal+"/changes", "changes"
		if action == "change-context" {
			if !need(q.Session) {
				return nil, studyInvalid()
			}
			path = "/v1/learning/goals/" + q.Goal + "/change-context"
			query.Set("session_id", q.Session)
		} else if action != "changes" {
			if !need(q.Change) {
				return nil, studyInvalid()
			}
			path += "/" + q.Change
			if action == "change-command" {
				method, feature = "POST", "available"
			} else if q.Version > 0 {
				query.Set("revision", strconv.FormatInt(q.Version, 10))
			}
		}
	default:
		return nil, studyInvalid()
	}
	var body StudyDocument
	if method == "POST" {
		if len(input) == 0 || len(input) > 256<<10 || decodeStrict(input, &body) != nil || body == nil {
			return nil, studyInvalid()
		}
		if action != "ensure" && !validLearningUUID(body.String("operation_id")) {
			return nil, studyInvalid()
		}
		if err := validateStudyInput(action, body); err != nil {
			return nil, err
		}
	} else if len(input) != 0 {
		return nil, studyInvalid()
	}
	if action == "source-decision" && body.String("kind") == "adopt" {
		feature = "research"
	}
	if action == "run-command" && body.String("kind") != "stop" && body.String("kind") != "clear" {
		run, err := c.Study(ctx, "run", StudyQuery{Run: q.Run}, nil)
		if err != nil {
			return nil, err
		}
		feature = "web_mentor"
		if run.String("kind") == "research" || run.String("kind") == "start_learning" {
			feature = "research"
		}
	}
	cap, err := c.studyCapability(ctx, capability)
	if err != nil {
		return nil, err
	}
	if feature != "" {
		available := cap.Bool("available")
		if feature != "available" {
			available = cap.Object(feature).Bool("available")
		}
		if !available {
			return nil, &APIError{Status: 503, Code: "learning_services_disabled"}
		}
	}
	if action == "answer" {
		content, e := c.Study(ctx, "content", StudyQuery{Artifact: q.Artifact, Version: body.Number("content_version")}, nil)
		if e != nil {
			return nil, e
		}
		if content.String("session_id") != body.String("aggregate_id") || content.Number("version") != body.Number("content_version") || content.String("status") != "committed" {
			return nil, studyInvalid()
		}
		if err := CheckContentAnswer(content, body.String("answer")); err != nil {
			return nil, err
		}
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var result StudyDocument
	var request any
	if method == "POST" {
		request = input
	}
	status, err := c.doJSONStatus(ctx, method, path, true, request, map[int]bool{200: true, 201: true, 202: true}, method == "GET", &result)
	if status == 405 || status == 501 {
		return nil, studyUnsupported()
	}
	if err != nil {
		return nil, err
	}
	if err = c.validateStudyResult(action, q, body, result); err != nil {
		return nil, err
	}
	return result, nil
}

func validateStudyInput(action string, b StudyDocument) error {
	switch action {
	case "research", "start", "mentor":
		if !validLearningUUID(b.String("session_id")) || b.Number("expected_version") < 1 || len(b["save"]) == 0 {
			return studyInvalid()
		}
		if action == "mentor" {
			if !validLearningUUID(b.String("teaching_session_id")) || b.Object("research") != nil || b.Object("start_learning") != nil {
				return studyInvalid()
			}
		} else {
			r := b.Object("research")
			if !r.Bool("external_consent") || strings.TrimSpace(r.String("topic")) == "" {
				return studyInvalid()
			}
			if action == "start" && (!b.Object("start_learning").Bool("new_session") || !b.Object("start_learning").Bool("model_consent") || !r.Bool("auto_adopt") || !b.Bool("save")) {
				return studyInvalid()
			}
			if action == "research" && b.Object("start_learning") != nil {
				return studyInvalid()
			}
		}
	case "run-command", "source-decision":
		if b.Number("expected_version") < 1 {
			return studyInvalid()
		}
	case "ensure":
		if b.Number("protocol_version") != 1 || !validLearningUUID(b.String("activity_id")) {
			return studyInvalid()
		}
	case "answer":
		if b.Number("content_version") < 1 || b.Number("expected_version") < 1 || !validLearningUUID(b.String("aggregate_id")) || b.String("action") != "submit_attempt" {
			return studyInvalid()
		}
	case "change-command":
		if !validLearningUUID(b.String("session_id")) {
			return studyInvalid()
		}
		if b.String("action") == "propose" {
			if b.Object("base") == nil || b.Object("candidate") == nil {
				return studyInvalid()
			}
		} else if b.Number("expected_revision") < 1 || !validSHA256(b.String("hash")) || !validLearningUUID(b.String("interaction_id")) {
			return studyInvalid()
		}
	}
	return nil
}

func (c *Client) validateStudyResult(action string, q StudyQuery, b, r StudyDocument) error {
	invalid := func() error { return &ProtocolError{Category: "study_context_mismatch"} }
	space := c.learningSpace
	if space == "" {
		space = DefaultLearningSpaceID
	}
	for _, field := range []string{"space_id", "learning_space_id"} {
		if _, ok := r[field]; ok && r.String(field) != space {
			return invalid()
		}
	}
	for key, want := range map[string]string{"goal_id": q.Goal, "run_id": q.Run, "artifact_id": q.Artifact} {
		if _, ok := r[key]; ok && want != "" && r.String(key) != want {
			return invalid()
		}
	}
	switch action {
	case "current":
		var run StudyDocument
		if json.Unmarshal(r["run"], &run) != nil {
			return invalid()
		}
		if run != nil {
			if run.String("kind") != q.Kind {
				return invalid()
			}
			return c.validateStudyResult("run", q, nil, run)
		}
	case "answer":
		if r.String("aggregate_type") != "session" || r.String("aggregate_id") != b.String("aggregate_id") || r.Object("result").String("session_id") != b.String("aggregate_id") {
			return invalid()
		}
	case "session-operation":
		if r.String("session_id") != q.Session || r.String("operation_id") != q.Operation {
			return invalid()
		}
	case "runs", "library", "changes", "sources":
		var items []StudyDocument
		if json.Unmarshal(r["items"], &items) != nil || items == nil {
			return invalid()
		}
		child := map[string]string{"runs": "run", "library": "library-item", "changes": "change", "sources": "source"}[action]
		for _, item := range items {
			if err := c.validateStudyResult(child, q, nil, item); err != nil {
				return err
			}
		}
	case "run":
		if r.String("space_id") != space || !validLearningUUID(r.String("run_id")) || !validLearningUUID(r.String("goal_id")) || !validLearningUUID(r.String("session_id")) || r.Number("version") < 1 {
			return invalid()
		}
	case "research", "start", "mentor", "run-command", "source-decision", "operation":
		if !validLearningUUID(r.String("run_id")) || !validLearningUUID(r.String("session_id")) || r.Number("version") < 1 {
			return invalid()
		}
		want := b.String("operation_id")
		if action == "operation" {
			want = q.Operation
		}
		if r.String("operation_id") != want {
			return invalid()
		}
		if (action == "research" || action == "start" || action == "mentor") && r.String("session_id") != b.String("session_id") {
			return invalid()
		}
	case "content", "ensure":
		if r.Number("protocol_version") != 1 {
			return studyUnsupported()
		}
		if r.String("learning_space_id") != space || !validLearningUUID(r.String("artifact_id")) || !validLearningUUID(r.String("session_id")) || !validLearningUUID(r.String("activity_id")) || r.Number("version") < 1 || r.Object("body") == nil {
			return invalid()
		}
		if action == "ensure" && (r.String("session_id") != q.Session || r.String("activity_id") != b.String("activity_id")) || action == "content" && q.Version > 0 && r.Number("version") != q.Version {
			return invalid()
		}
	case "change", "change-command":
		if r.String("learning_space_id") != space || r.String("goal_id") != q.Goal || !validLearningUUID(r.String("id")) || r.Number("revision") < 1 {
			return invalid()
		}
		if q.Change != "" && r.String("id") != q.Change {
			return invalid()
		}
		if action == "change" && q.Version > 0 && r.Number("revision") != q.Version {
			return invalid()
		}
		if action == "change-command" && r.String("session_id") != b.String("session_id") {
			return invalid()
		}
	case "change-context":
		if r.Object("session").String("session_id") != q.Session || r.Object("goal").String("goal_id") != q.Goal {
			return invalid()
		}
	case "source":
		if r.String("space_id") != space || !validLearningUUID(r.String("id")) || q.Source != "" && r.String("id") != q.Source {
			return invalid()
		}
	case "library-item":
		if !validLearningUUID(r.String("artifact_id")) || !validLearningUUID(r.String("session_id")) || !validLearningUUID(r.String("goal_id")) || r.Number("version") < 1 {
			return invalid()
		}
	}
	return nil
}

func CheckContentAnswer(content StudyDocument, answer string) error {
	if content.String("status") != "committed" || content.Number("version") != content.Number("committed_version") {
		return &APIError{Status: 409, Code: "learning_content_conflict"}
	}
	i := content.Object("body").Object("interaction")
	switch i.String("kind") {
	case "text":
		if strings.TrimSpace(answer) != "" {
			return nil
		}
	case "single_choice":
		for _, choice := range i.Items("choices") {
			if answer == choice.String("value") {
				return nil
			}
		}
	default:
		return &APIError{Status: 422, Code: "learning_content_upgrade_required"}
	}
	return studyInvalid()
}

// ContentText 只投影显示，不从未知块或选项标签推测答案含义。
func ContentText(content StudyDocument) string {
	var out strings.Builder
	var walk func([]StudyDocument, int)
	walk = func(blocks []StudyDocument, depth int) {
		if depth > 8 {
			return
		}
		for _, b := range blocks {
			text := b.String("fallback")
			switch b.String("kind") {
			case "markdown", "code", "math", "question", "callout":
				if b.String("text") != "" {
					text = b.String("text")
				}
			case "group":
				walk(b.Items("children"), depth+1)
				continue
			case "table":
				var rows [][]string
				if json.Unmarshal(b["rows"], &rows) == nil {
					for _, row := range rows {
						out.WriteString(strings.Join(row, " | ") + "\n")
					}
					continue
				}
			}
			out.WriteString(text + "\n")
		}
	}
	walk(content.Object("body").Items("blocks"), 0)
	i := content.Object("body").Object("interaction")
	if i.String("kind") == "single_choice" {
		for _, choice := range i.Items("choices") {
			out.WriteString(choice.String("value") + ". " + choice.String("label") + "\n")
		}
	} else if i.String("kind") != "text" && i.String("kind") != "none" {
		out.WriteString("此交互需要更新客户端或使用支持的图形界面；CLI 禁止提交答案。\n")
	}
	return out.String()
}
