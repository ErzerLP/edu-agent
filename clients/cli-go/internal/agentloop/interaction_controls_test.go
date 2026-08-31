package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func TestQuestionArgumentsRejectMalformedOrSecretRequests(t *testing.T) {
	valid := map[string]any{
		"question_id": "next-step",
		"header":      "下一步",
		"question":    "你希望接下来学习什么？",
		"mode":        "single",
		"options": []map[string]any{
			{"id": "graphs", "label": "图论", "description": "继续学习图论"},
			{"id": "algebra", "label": "代数", "description": "切换到代数"},
		},
	}
	encode := func(value map[string]any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if _, err := decodeQuestionArgs(encode(valid)); err != nil {
		t.Fatalf("valid question rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown field", mutate: func(value map[string]any) { value["unexpected"] = true }},
		{name: "too few options", mutate: func(value map[string]any) { value["options"] = value["options"].([]map[string]any)[:1] }},
		{name: "too many options", mutate: func(value map[string]any) {
			value["options"] = []map[string]any{
				{"id": "a", "label": "A", "description": "A"}, {"id": "b", "label": "B", "description": "B"},
				{"id": "c", "label": "C", "description": "C"}, {"id": "d", "label": "D", "description": "D"},
				{"id": "e", "label": "E", "description": "E"},
			}
		}},
		{name: "duplicate IDs", mutate: func(value map[string]any) { value["options"].([]map[string]any)[1]["id"] = "graphs" }},
		{name: "reserved custom ID", mutate: func(value map[string]any) { value["options"].([]map[string]any)[0]["id"] = "custom_input" }},
		{name: "reserved custom label", mutate: func(value map[string]any) { value["options"].([]map[string]any)[0]["label"] = "Type something" }},
		{name: "question display width", mutate: func(value map[string]any) { value["question"] = strings.Repeat("问", 37) }},
		{name: "label display width", mutate: func(value map[string]any) {
			value["options"].([]map[string]any)[0]["label"] = strings.Repeat("宽", 17)
		}},
		{name: "description display width", mutate: func(value map[string]any) {
			value["options"].([]map[string]any)[0]["description"] = strings.Repeat("详", 31)
		}},
		{name: "secret question", mutate: func(value map[string]any) { value["question"] = "请输入你的 API Key" }},
		{name: "secret option", mutate: func(value map[string]any) {
			value["options"].([]map[string]any)[0]["description"] = "粘贴访问令牌"
		}},
		{name: "bidi option", mutate: func(value map[string]any) { value["options"].([]map[string]any)[0]["label"] = "图\u202e论" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(valid)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			options := value["options"].([]any)
			converted := make([]map[string]any, len(options))
			for index := range options {
				converted[index] = options[index].(map[string]any)
			}
			value["options"] = converted
			test.mutate(value)
			if _, err := decodeQuestionArgs(encode(value)); err == nil {
				t.Fatal("invalid question accepted")
			}
		})
	}
}

func TestQuestionDisplayWidthLimitsFitMinimumSelector(t *testing.T) {
	if !validQuestionText(strings.Repeat("问", 36), 160, 72) || validQuestionText(strings.Repeat("问", 37), 160, 72) {
		t.Fatal("question display-width boundary is incorrect")
	}
	if !validQuestionText(strings.Repeat("宽", 15)+"尾", 48, 32) || validQuestionText(strings.Repeat("宽", 17), 48, 32) {
		t.Fatal("option label display-width boundary is incorrect")
	}
	if !validQuestionText(strings.Repeat("详", 29)+"尾", 120, 60) || validQuestionText(strings.Repeat("详", 31), 120, 60) {
		t.Fatal("option description display-width boundary is incorrect")
	}
}

func TestAsksForSecretRequiresSolicitationAndCredentialObject(t *testing.T) {
	for _, safe := range []string{
		"解释 token 与 tokenizer 的区别",
		"请讲解 API Key 的权限模型",
		"密钥和私钥在密码学中有什么区别？",
		"如何安全轮换访问令牌？",
		"输入密码学概念时应该注意什么？",
		"Please provide an explanation of password hashing",
		"Please provide a password hashing explanation",
		"Please explain how access tokens expire",
		"Describe private key cryptography",
		"How should you protect your password?",
	} {
		if asksForSecret(safe) {
			t.Fatalf("safe teaching question rejected: %q", safe)
		}
	}
	for _, unsafe := range []string{
		"请粘贴你的 API Key",
		"请输入密码",
		"请输入 token",
		"提供恢复码给我",
		"你的 API 密钥是什么？",
		"Please provide your access token",
		"Please enter your password",
		"Enter passcode",
		"Please provide me with your password",
		"Provide me your password",
		"Give us your token",
		"Share your authentication token",
		"Post your API key",
		"Your password?",
		"Can I have your token?",
		"Password please",
		"What is your API key?",
		"Paste the private key here",
	} {
		if !asksForSecret(unsafe) {
			t.Fatalf("credential solicitation accepted: %q", unsafe)
		}
	}
}

func TestQuestionAnswerAllowsBoundedMultilineConceptTextButRejectsUnsafeCharacters(t *testing.T) {
	pending := &PendingQuestion{
		ID: "concept", Mode: QuestionMultiple,
		Options: []QuestionOption{{ID: "a", Label: "A", Description: "A"}, {ID: "b", Label: "B", Description: "B"}},
	}
	custom := "我想讨论 API key 和 token 的概念，\n\t但不会提交任何秘密值。"
	value, err := validateQuestionAnswer(pending, QuestionAnswer{QuestionID: pending.ID, Status: QuestionAnswered, Custom: custom})
	if err != nil {
		t.Fatalf("safe multiline conceptual answer rejected: %v", err)
	}
	if value["custom"] != custom {
		t.Fatalf("custom answer changed: %#v", value["custom"])
	}
	for _, unsafe := range []string{"line\x01break", "left\u202eright"} {
		if _, err := validateQuestionAnswer(pending, QuestionAnswer{QuestionID: pending.ID, Status: QuestionAnswered, Custom: unsafe}); err == nil {
			t.Fatalf("unsafe custom answer accepted: %q", unsafe)
		}
	}
	if _, err := validateQuestionAnswer(pending, QuestionAnswer{QuestionID: pending.ID, Status: QuestionAnswered}); err == nil {
		t.Fatal("empty multiple answer accepted")
	}
	if _, err := validateQuestionAnswer(pending, QuestionAnswer{QuestionID: pending.ID, Status: QuestionAnswered, OptionIDs: []string{"a", "a"}}); err == nil {
		t.Fatal("duplicate selected IDs accepted")
	}
}

func TestQuestionSinglePausesSiblingsPreservesCallIDAndClonesResult(t *testing.T) {
	server := &fakeServer{}
	question := questionToolCall(t, "question-call", "choose-topic", QuestionSingle)
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolCallsMessage(
			question,
			modelclient.ToolCall{ID: "progress-call", Type: "function", Function: modelclient.ToolFunction{Name: "get_learning_progress", Arguments: `{}`}},
		)},
		{Message: modelclient.Message{Role: "assistant", Content: "已按选择继续"}},
	}}
	session := newTestSession(t, model, server)
	pendingResult, err := session.Send(t.Context(), "帮我选择下一步")
	if err != nil {
		t.Fatal(err)
	}
	if pendingResult.PendingQuestion == nil || pendingResult.PendingQuestion.ID != "choose-topic" {
		t.Fatalf("pending=%+v", pendingResult.PendingQuestion)
	}
	if server.currentCalls != 0 {
		t.Fatalf("sibling ran before answer: currentCalls=%d", server.currentCalls)
	}
	pendingResult.PendingQuestion.ID = "externally-mutated"
	pendingResult.PendingQuestion.Options[0].ID = "externally-mutated"
	result, err := session.ResolveQuestion(t.Context(), QuestionAnswer{QuestionID: "choose-topic", Status: QuestionAnswered, OptionIDs: []string{"first"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "已按选择继续" || server.currentCalls != 1 {
		t.Fatalf("result=%+v currentCalls=%d", result, server.currentCalls)
	}
	if _, err := session.ResolveQuestion(t.Context(), QuestionAnswer{QuestionID: "choose-topic", Status: QuestionAnswered, OptionIDs: []string{"first"}}); err == nil {
		t.Fatal("late question resolution accepted")
	}
	if server.createCalls != 0 || server.decisionCalls != 0 {
		t.Fatalf("ordinary question wrote preferences: create=%d decision=%d", server.createCalls, server.decisionCalls)
	}
	if len(model.requests) != 2 {
		t.Fatalf("requests=%d", len(model.requests))
	}
	answer := decodedToolResult(t, model.requests[1].Messages, "question-call")
	if answer["question_id"] != "choose-topic" || answer["status"] != string(QuestionAnswered) {
		t.Fatalf("answer=%+v", answer)
	}
	ids := answer["option_ids"].([]any)
	if len(ids) != 1 || ids[0] != "first" {
		t.Fatalf("option_ids=%+v", ids)
	}
	if _, present := decodedToolResult(t, model.requests[1].Messages, "progress-call")["active"]; !present {
		t.Fatalf("sibling tool result missing: %+v", model.requests[1].Messages)
	}
}

func TestQuestionMultipleCustomCancelledAndUnavailableOutcomes(t *testing.T) {
	tests := []struct {
		name   string
		mode   QuestionMode
		answer QuestionAnswer
		check  func(*testing.T, map[string]any)
	}{
		{
			name: "multiple", mode: QuestionMultiple,
			answer: QuestionAnswer{QuestionID: "question", Status: QuestionAnswered, OptionIDs: []string{"second", "first"}},
			check: func(t *testing.T, value map[string]any) {
				ids := value["option_ids"].([]any)
				if !equalAnyStrings(ids, []string{"first", "second"}) {
					t.Fatalf("ordered option IDs=%+v", ids)
				}
			},
		},
		{
			name: "custom multiline concepts", mode: QuestionSingle,
			answer: QuestionAnswer{QuestionID: "question", Status: QuestionAnswered, Custom: "讨论 API Key 与 token 概念\n\t不提交秘密"},
			check: func(t *testing.T, value map[string]any) {
				if value["custom"] != "讨论 API Key 与 token 概念\n\t不提交秘密" {
					t.Fatalf("custom=%#v", value["custom"])
				}
			},
		},
		{
			name: "cancelled", mode: QuestionSingle,
			answer: QuestionAnswer{QuestionID: "question", Status: QuestionCancelled},
			check: func(t *testing.T, value map[string]any) {
				if value["status"] != string(QuestionCancelled) {
					t.Fatalf("value=%+v", value)
				}
			},
		},
		{
			name: "unavailable", mode: QuestionSingle,
			answer: QuestionAnswer{QuestionID: "question", Status: QuestionUnavailable},
			check: func(t *testing.T, value map[string]any) {
				if value["status"] != string(QuestionUnavailable) {
					t.Fatalf("value=%+v", value)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := &fakeServer{}
			model := &fakeModel{responses: []modelclient.Response{
				{Message: toolCallsMessage(questionToolCall(t, "question-call", "question", test.mode))},
				{Message: modelclient.Message{Role: "assistant", Content: "继续"}},
			}}
			session := newTestSession(t, model, server)
			if _, err := session.Send(t.Context(), "需要选择"); err != nil {
				t.Fatal(err)
			}
			if _, err := session.ResolveQuestion(t.Context(), test.answer); err != nil {
				t.Fatal(err)
			}
			value := decodedToolResult(t, model.requests[1].Messages, "question-call")
			test.check(t, value)
			if server.createCalls != 0 || server.decisionCalls != 0 {
				t.Fatalf("question wrote preference server: create=%d decision=%d", server.createCalls, server.decisionCalls)
			}
		})
	}
}

func TestQuestionRejectsConcurrentAndLateResolution(t *testing.T) {
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	model := &scriptedCompleteModel{steps: []completeStep{
		func(context.Context, modelclient.Request) (modelclient.Response, error) {
			return modelclient.Response{Message: toolCallsMessage(questionToolCall(t, "question-call", "question", QuestionSingle))}, nil
		},
		func(context.Context, modelclient.Request) (modelclient.Response, error) {
			close(secondStarted)
			<-releaseSecond
			return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "完成"}}, nil
		},
	}}
	session := newTestSession(t, model, &fakeServer{})
	if _, err := session.Send(t.Context(), "提问"); err != nil {
		t.Fatal(err)
	}
	firstResult := make(chan error, 1)
	go func() {
		_, err := session.ResolveQuestion(t.Context(), QuestionAnswer{QuestionID: "question", Status: QuestionAnswered, OptionIDs: []string{"first"}})
		firstResult <- err
	}()
	<-secondStarted
	if _, err := session.ResolveQuestion(t.Context(), QuestionAnswer{QuestionID: "question", Status: QuestionAnswered, OptionIDs: []string{"first"}}); err == nil {
		t.Fatal("concurrent duplicate resolution accepted")
	}
	close(releaseSecond)
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
	if _, err := session.ResolveQuestion(t.Context(), QuestionAnswer{QuestionID: "question", Status: QuestionAnswered, OptionIDs: []string{"first"}}); err == nil {
		t.Fatal("late duplicate resolution accepted")
	}
}

func TestQuestionLimitIsFourPerTurn(t *testing.T) {
	responses := make([]modelclient.Response, 0, 6)
	for index := 1; index <= 5; index++ {
		responses = append(responses, modelclient.Response{Message: toolCallsMessage(questionToolCall(t, fmt.Sprintf("call-%d", index), fmt.Sprintf("question-%d", index), QuestionSingle))})
	}
	responses = append(responses, modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "四次后继续"}})
	model := &fakeModel{responses: responses}
	session := newTestSession(t, model, &fakeServer{})
	result, err := session.Send(t.Context(), "连续问题")
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 4; index++ {
		if result.PendingQuestion == nil || result.PendingQuestion.ID != fmt.Sprintf("question-%d", index) {
			t.Fatalf("pending at %d=%+v", index, result.PendingQuestion)
		}
		result, err = session.ResolveQuestion(t.Context(), QuestionAnswer{
			QuestionID: fmt.Sprintf("question-%d", index), Status: QuestionAnswered, OptionIDs: []string{"first"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if result.PendingQuestion != nil || result.Text != "四次后继续" {
		t.Fatalf("fifth ask was not rejected and continued: %+v", result)
	}
	if len(model.requests) != 6 {
		t.Fatalf("model requests=%d", len(model.requests))
	}
	fifth := decodedToolResult(t, model.requests[5].Messages, "call-5")
	if fifth["error"] != "question_limit_exceeded" {
		t.Fatalf("fifth question result=%+v", fifth)
	}
}

func TestQuestionAndPreferenceResolversRemainIsolated(t *testing.T) {
	t.Run("question cannot be saved as preference", func(t *testing.T) {
		server := &fakeServer{}
		model := &fakeModel{responses: []modelclient.Response{
			{Message: toolCallsMessage(questionToolCall(t, "question-call", "question", QuestionSingle))},
			{Message: modelclient.Message{Role: "assistant", Content: "完成"}},
		}}
		session := newTestSession(t, model, server)
		if _, err := session.Send(t.Context(), "问题"); err != nil {
			t.Fatal(err)
		}
		if _, err := session.ResolvePreference(t.Context(), PreferenceSave); err == nil {
			t.Fatal("preference resolver accepted a question")
		}
		if _, err := session.ResolveQuestion(t.Context(), QuestionAnswer{QuestionID: "question", Status: QuestionAnswered, OptionIDs: []string{"first"}}); err != nil {
			t.Fatal(err)
		}
		if server.createCalls != 0 || server.decisionCalls != 0 {
			t.Fatalf("question resolution wrote preferences: %+v", server)
		}
	})

	t.Run("preference cannot be answered as question", func(t *testing.T) {
		server := &fakeServer{}
		model := &fakeModel{responses: []modelclient.Response{
			{Message: toolMessage("preference-call", "remember_preference", `{"content":"回答保持简洁","reason":"用户明确要求","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`)},
			{Message: modelclient.Message{Role: "assistant", Content: "完成"}},
		}}
		session := newTestSession(t, model, server)
		if _, err := session.Send(t.Context(), "记住偏好"); err != nil {
			t.Fatal(err)
		}
		if _, err := session.ResolveQuestion(t.Context(), QuestionAnswer{QuestionID: "preference-call", Status: QuestionAnswered, OptionIDs: []string{"first"}}); err == nil {
			t.Fatal("question resolver accepted a preference")
		}
		if _, err := session.ResolvePreference(t.Context(), PreferenceDecline); err != nil {
			t.Fatal(err)
		}
		if server.createCalls != 0 || server.decisionCalls != 0 {
			t.Fatalf("decline wrote preference server: %+v", server)
		}
	})
}

func TestPreferenceResolutionSaveSessionOnlyAndDecline(t *testing.T) {
	for _, test := range []struct {
		name           string
		resolution     PreferenceResolution
		wantCreate     int
		wantDecision   int
		resultProperty string
		resultValue    any
	}{
		{name: "save", resolution: PreferenceSave, wantCreate: 1, wantDecision: 1, resultProperty: "saved", resultValue: true},
		{name: "session only", resolution: PreferenceSessionOnly, resultProperty: "session_only", resultValue: true},
		{name: "decline", resolution: PreferenceDecline, resultProperty: "reason", resultValue: "user_declined"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &fakeServer{}
			model := &fakeModel{responses: []modelclient.Response{
				{Message: toolMessage("preference-call", "remember_preference", `{"content":"回答保持简洁","reason":"用户明确要求","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`)},
				{Message: modelclient.Message{Role: "assistant", Content: "偏好处理完成"}},
			}}
			session := newPreferenceTestSession(t, model, server)
			if _, err := session.Send(t.Context(), "偏好"); err != nil {
				t.Fatal(err)
			}
			if _, err := session.ResolvePreference(t.Context(), test.resolution); err != nil {
				t.Fatal(err)
			}
			if server.createCalls != test.wantCreate || server.decisionCalls != test.wantDecision {
				t.Fatalf("writes create=%d decision=%d", server.createCalls, server.decisionCalls)
			}
			value := decodedToolResult(t, model.requests[1].Messages, "preference-call")
			if value[test.resultProperty] != test.resultValue {
				t.Fatalf("preference result=%+v", value)
			}
		})
	}
}

func TestQuestionPublicContractIsDocumentedWithoutGrantingWrites(t *testing.T) {
	model := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: "完成"}}}}
	session := newTestSession(t, model, &fakeServer{})
	if _, err := session.Send(t.Context(), "检查提示"); err != nil {
		t.Fatal(err)
	}
	prompt := model.requests[0].Messages[0].Content
	for _, required := range []string{"单选", "多选", "自定义回答", "不构成长期记忆、外部写入、删除或发布授权"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("system prompt lacks %q: %s", required, prompt)
		}
	}
	var description, parameters string
	for _, tool := range Tools() {
		if tool.Function.Name == "ask_user_question" {
			description = tool.Function.Description
			parameters = string(tool.Function.Parameters)
			break
		}
	}
	for _, required := range []string{"single", "multiple", "终端显示宽度", "普通问询答案", "不构成长期记忆、外部写入、删除或发布授权"} {
		if !strings.Contains(description, required) {
			t.Fatalf("tool description lacks %q: %s", required, description)
		}
	}
	for _, required := range []string{"最多36个终端显示列", "最多72个终端显示列", "最多32个终端显示列", "最多60个终端显示列"} {
		if !strings.Contains(parameters, required) {
			t.Fatalf("tool schema lacks %q: %s", required, parameters)
		}
	}
}

func questionToolCall(t *testing.T, callID, questionID string, mode QuestionMode) modelclient.ToolCall {
	t.Helper()
	arguments, err := json.Marshal(map[string]any{
		"question_id": questionID,
		"header":      "下一步",
		"question":    "你希望如何继续？",
		"mode":        mode,
		"options": []map[string]any{
			{"id": "first", "label": "第一项", "description": "选择第一项"},
			{"id": "second", "label": "第二项", "description": "选择第二项"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return modelclient.ToolCall{ID: callID, Type: "function", Function: modelclient.ToolFunction{Name: "ask_user_question", Arguments: string(arguments)}}
}

func decodedToolResult(t *testing.T, messages []modelclient.Message, callID string) map[string]any {
	t.Helper()
	for _, message := range messages {
		if message.Role != "tool" || message.ToolCallID != callID {
			continue
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(message.Content), &value); err != nil {
			t.Fatalf("decode tool result %q: %v", message.Content, err)
		}
		return value
	}
	t.Fatalf("tool result %q not found in %+v", callID, messages)
	return nil
}

func equalAnyStrings(values []any, expected []string) bool {
	if len(values) != len(expected) {
		return false
	}
	for index := range values {
		if values[index] != expected[index] {
			return false
		}
	}
	return true
}

func newPreferenceTestSession(t *testing.T, model Model, server Server) *Session {
	t.Helper()
	var mu sync.Mutex
	next := 0
	ids := []string{"70000000-0000-4000-8000-000000000001", "70000000-0000-4000-8000-000000000002"}
	session, err := New(model, server, Options{
		ContextWindow: 32768,
		MaxToolRounds: 8,
		Now:           func() time.Time { return time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC) },
		NewUUID: func() (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if next >= len(ids) {
				return "", errors.New("no UUID left")
			}
			value := ids[next]
			next++
			return value, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}
