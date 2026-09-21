package tutormodel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/learning"
)

func TestAdapterSendsStrictRecursiveSchemasAndThreeRoles(t *testing.T) {
	outputs := map[learning.ProposalType]string{
		learning.ProposalType("planning"): `{"details":{"name":"并发","priority":"normal","expected_outcome":"","scope":"","exclusions":"","self_assessment":"","purpose":"","completion_criteria":"","scope_snapshot_id":"","timezone":"","deadline":null,"weekly_minutes":null},"steps":[],"questions":[],"gaps":["资料尚待核对"]}`,
		learning.ProposalRoute:            `{"route":[{"node_revision_id":"node","teaching_intent":"teach","completion_condition":"pass"}]}`,
		learning.ProposalActivity:         `{"activity":{"prompt":"Question","type":"open","rubric":{"rubric_revision":"r1","items":[{"rubric_item_id":"i1","criterion":"correct","required_reference_ids":null}],"objective_rule":null},"difficulty":1,"allowed_help":["none"],"knowledge_references":[{"node_revision_id":"node","slice_sha256":null,"range":null}]}}`,
		learning.ProposalAssessment:       `{"assessment":{"items":[],"rubric_complete":false,"confidence":0,"risk_flags":[]}}`,
		learning.ProposalFreeAnswer:       `{"text":{"text":"Answer","knowledge_references":[{"node_revision_id":"node","slice_sha256":null,"range":null}]}}`,
		learning.ProposalExplanation:      `{"text":{"text":"Explanation","knowledge_references":[{"node_revision_id":"node","slice_sha256":null,"range":null}]}}`,
	}
	for kind, output := range outputs {
		t.Run(string(kind), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages       []llm.Message `json:"messages"`
					ResponseFormat struct {
						Type       string `json:"type"`
						JSONSchema struct {
							Name   string         `json:"name"`
							Strict bool           `json:"strict"`
							Schema map[string]any `json:"schema"`
						} `json:"json_schema"`
					} `json:"response_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if body.ResponseFormat.Type != "json_schema" || !body.ResponseFormat.JSONSchema.Strict {
					t.Errorf("response format is not strict: %#v", body.ResponseFormat)
				}
				roles := map[llm.Role]bool{}
				for _, message := range body.Messages {
					roles[message.Role] = true
				}
				if !roles[llm.RoleSystem] || !roles[llm.RoleAssistant] || !roles[llm.RoleUser] {
					t.Errorf("missing role: %#v", roles)
				}
				if err := validateStrictSchema(body.ResponseFormat.JSONSchema.Schema, "$"); err != nil {
					t.Logf("严格端点拒绝请求：%v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				content := output
				if body.ResponseFormat.JSONSchema.Name == "capability_probe" {
					content = `{"capability_probe":true}`
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
			}))
			defer server.Close()
			client := newAdapterTestClient(t, server.URL)
			if capabilities := client.Probe(context.Background()); !capabilities.Compatible || !capabilities.NativeJSONSchema {
				t.Fatalf("严格能力探测失败：%+v", capabilities)
			}
			adapter := New(client)
			result, err := adapter.Generate(context.Background(), learning.ProposalRequest{Type: kind, Input: json.RawMessage(`{"context":true}`)})
			if err != nil || !json.Valid(result) {
				t.Fatalf("Generate = %s, %v", result, err)
			}
		})
	}
}

// 模拟严格提供商的请求校验，不能仅检查 schema 是否出现过 required。
func validateStrictSchema(schema map[string]any, path string) error {
	if properties, ok := schema["properties"].(map[string]any); ok {
		if schema["additionalProperties"] != false {
			return fmt.Errorf("%s 未关闭额外属性", path)
		}
		required, _ := schema["required"].([]any)
		seen := map[string]bool{}
		for _, value := range required {
			name, ok := value.(string)
			if !ok || seen[name] || properties[name] == nil {
				return fmt.Errorf("%s 的 required 无效", path)
			}
			seen[name] = true
		}
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if !seen[name] {
				return fmt.Errorf("%s.%s 未列入 required", path, name)
			}
			if err := validateStrictSchema(properties[name].(map[string]any), path+"."+name); err != nil {
				return err
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		return validateStrictSchema(items, path+"[]")
	}
	return nil
}

func TestAdapterValidatesNullableActivityFields(t *testing.T) {
	const output = `{"activity":{"prompt":"哪些数是偶数？","type":"open","rubric":{"rubric_revision":"r1","items":[{"rubric_item_id":"i1","criterion":"正确识别","required_reference_ids":null}],"objective_rule":null},"difficulty":1,"allowed_help":["none"],"knowledge_references":[{"node_revision_id":"node","slice_sha256":null,"range":null}]}}`
	for _, test := range []struct {
		name  string
		from  string
		to    string
		valid bool
	}{
		{name: "可选值为空", valid: true},
		{name: "引用数组非空", from: `"required_reference_ids":null`, to: `"required_reference_ids":["node"]`, valid: true},
		{name: "客观题规则非空", from: `"objective_rule":null`, to: `"objective_rule":{"accepted_answers":["2"],"case_sensitive":false,"trim_space":true}`, valid: true},
		{name: "引用范围非空", from: `"range":null`, to: `"range":{"start":0,"end":5}`, valid: true},
		{name: "引用摘要非空", from: `"slice_sha256":null`, to: `"slice_sha256":"摘要由领域层核验"`, valid: true},
		{name: "缺少可空属性", from: `,"objective_rule":null`, to: ``},
		{name: "引用数组类型错误", from: `"required_reference_ids":null`, to: `"required_reference_ids":false`},
		{name: "引用数组元素类型错误", from: `"required_reference_ids":null`, to: `"required_reference_ids":[null]`},
		{name: "客观题规则类型错误", from: `"objective_rule":null`, to: `"objective_rule":[]`},
		{name: "客观题规则不完整", from: `"objective_rule":null`, to: `"objective_rule":{"accepted_answers":["2"]}`},
		{name: "引用范围类型错误", from: `"range":null`, to: `"range":{"start":"0","end":5}`},
		{name: "引用范围含未知属性", from: `"range":null`, to: `"range":{"start":0,"end":5,"extra":true}`},
		{name: "引用摘要类型错误", from: `"slice_sha256":null`, to: `"slice_sha256":42`},
		{name: "必填文本不允许为空值", from: `"prompt":"哪些数是偶数？"`, to: `"prompt":null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := output
			if test.from != "" {
				content = strings.Replace(output, test.from, test.to, 1)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
			}))
			defer server.Close()
			raw, err := New(newAdapterTestClient(t, server.URL)).Generate(context.Background(), learning.ProposalRequest{Type: learning.ProposalActivity})
			if !test.valid {
				if llm.Category(err) != llm.ErrorSchemaMismatch {
					t.Fatalf("非法响应未被拒绝：%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Activity learning.ActivityProposal `json:"activity"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("可空输出无法解码为领域类型：%v", err)
			}
			if test.from == "" && (decoded.Activity.Rubric.ObjectiveRule != nil || len(decoded.Activity.Rubric.Items[0].RequiredReferenceIDs) != 0 || decoded.Activity.References[0].SliceSHA256 != "" || decoded.Activity.References[0].Range != (learning.SourceRange{})) {
				t.Fatalf("空值改变了可选字段语义：%+v", decoded.Activity)
			}
		})
	}
}

func TestAdapterPreservesModelFailureCategory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer server.Close()
	_, err := New(newAdapterTestClient(t, server.URL)).Generate(context.Background(), learning.ProposalRequest{Type: learning.ProposalRoute, Input: json.RawMessage(`{}`)})
	failure, ok := err.(interface{ ModelCategory() string })
	if !ok || failure.ModelCategory() != "rate_limited" {
		t.Fatalf("category = %T %v", err, err)
	}
}

func newAdapterTestClient(t *testing.T, rawURL string) *llm.Client {
	t.Helper()
	base, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := llm.New(llm.Options{BaseURL: base, Model: "strict-fake", APIKey: "test-key", ContextWindow: 8192, MinimumContext: 4096, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
