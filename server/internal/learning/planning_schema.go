package learning

import "encoding/json"

// PlanningSchema 只允许建议字段；服务端还会验证字段长度、关系和资料范围。
func PlanningSchema() json.RawMessage {
	text := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer"}
	array := func(item any) any { return map[string]any{"type": "array", "items": item} }
	object := func(p map[string]any, required []string) any {
		return map[string]any{"type": "object", "properties": p, "required": required, "additionalProperties": false}
	}
	details := map[string]any{}
	for _, k := range []string{"name", "expected_outcome", "scope", "exclusions", "self_assessment", "purpose", "completion_criteria", "priority", "scope_snapshot_id", "timezone"} {
		details[k] = text
	}
	details["deadline"] = map[string]any{"type": []string{"string", "null"}}
	details["weekly_minutes"] = map[string]any{"type": []string{"integer", "null"}}
	step := object(map[string]any{"name": text, "content": text, "reason": text, "exercise": text, "completion": text, "minutes": integer, "prerequisites": array(integer), "node_revision_id": text}, []string{"name", "content", "reason", "exercise", "completion", "minutes", "prerequisites", "node_revision_id"})
	question := object(map[string]any{"field": text, "question": text, "answer": text}, []string{"field", "question", "answer"})
	raw, _ := json.Marshal(object(map[string]any{"details": object(details, []string{"name", "expected_outcome", "scope", "exclusions", "self_assessment", "purpose", "completion_criteria", "priority", "scope_snapshot_id", "timezone", "deadline", "weekly_minutes"}), "steps": array(step), "questions": array(question), "gaps": array(text)}, []string{"details", "steps", "questions", "gaps"}))
	return raw
}
