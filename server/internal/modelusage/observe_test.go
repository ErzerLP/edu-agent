package modelusage

import "testing"

func TestNullableChatUsage(t *testing.T) {
	u := ParseChatUsage([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":7,"completion_tokens_details":{"reasoning_tokens":3},"total_tokens":7}}`))
	if u.Input == nil || *u.Input != 0 || u.Output == nil || *u.Output != 7 || u.Reasoning == nil || *u.Reasoning != 3 || u.Total() == nil || *u.Total() != 7 {
		t.Fatalf("明确零、推理子项或合计错误: %+v", u)
	}
	u = ParseChatUsage([]byte(`{"usage":{"total_tokens":12}}`))
	if u.Input != nil || u.Output != nil || u.Reasoning != nil || u.Total() != nil {
		t.Fatalf("缺字段被推算: %+v", u)
	}
	u = ParseChatUsage([]byte(`{"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":99}}`))
	if !u.Inconsistent || u.Total() != nil || u.ProviderTotal == nil || *u.ProviderTotal != 99 {
		t.Fatalf("矛盾报告未保留审计值: %+v", u)
	}
}
