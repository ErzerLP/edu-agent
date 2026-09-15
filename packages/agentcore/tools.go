package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/packages/agentcore/agentlimits"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
)

// ToolRegistration 只能由宿主建立。Execute 闭包绑定真实应用主体和 scope，
// 严格解码参数后调用正式命令；模型不能通过字段选择另一执行器或提升权限。
type ToolRegistration struct {
	Definition modelclient.Tool
	Execute    func(context.Context, string) (ToolOutput, error)
}

// ToolOutput 的内容只能作为 role=tool 的不可信数据加入历史。
// 等待交互时，Resume 是宿主创建的续行函数，不能从模型 JSON 反序列化。
type ToolOutput struct {
	Content  string
	Question *PendingQuestion
	Resume   func(context.Context, QuestionAnswer) (string, error) `json:"-"`
}

type ToolSet struct {
	mu      sync.Mutex
	tools   map[string]ToolRegistration
	order   []string
	seen    map[string]struct{}
	pending map[string]ToolOutput
	timeout time.Duration
}

// NewToolSet 的空集合没有任何能力。工具集合属于单个会话，超时由宿主显式设置。
func NewToolSet(timeout time.Duration, registrations ...ToolRegistration) (*ToolSet, error) {
	if timeout <= 0 {
		return nil, errors.New("工具超时必须为正数")
	}
	s := &ToolSet{tools: map[string]ToolRegistration{}, seen: map[string]struct{}{}, pending: map[string]ToolOutput{}, timeout: timeout}
	for _, registration := range registrations {
		name := registration.Definition.Function.Name
		if name == "" || registration.Definition.Type != "function" || registration.Execute == nil || !json.Valid(registration.Definition.Function.Parameters) {
			return nil, errors.New("工具注册无效")
		}
		if _, exists := s.tools[name]; exists {
			return nil, errors.New("工具注册重复")
		}
		registration.Definition.Function.Parameters = append(json.RawMessage(nil), registration.Definition.Function.Parameters...)
		s.tools[name] = registration
		s.order = append(s.order, name)
	}
	return s, nil
}

func (s *ToolSet) Definitions() []modelclient.Tool {
	result := make([]modelclient.Tool, 0, len(s.order))
	for _, name := range s.order {
		definition := s.tools[name].Definition
		definition.Function.Parameters = append(json.RawMessage(nil), definition.Function.Parameters...)
		result = append(result, definition)
	}
	return result
}

// ReserveCallIDs 在恢复时预留已验证检查点中的历史身份，不执行任何工具。
// 宿主在开放新输入前调用；历史正文的新鲜度、授权重置及主体校验由存储适配器负责。
func (s *ToolSet) ReserveCallIDs(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) != 0 {
		return errors.New("历史身份只能在工具运行前恢复")
	}
	reserved := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" || len(id) > 512 || !utf8.ValidString(id) {
			return errors.New("历史工具身份无效")
		}
		if _, duplicate := reserved[id]; duplicate {
			return errors.New("历史工具身份重复")
		}
		reserved[id] = struct{}{}
	}
	s.seen = reserved
	return nil
}

func (s *ToolSet) Invoke(ctx context.Context, call modelclient.ToolCall) (ToolOutput, error) {
	if err := ctx.Err(); err != nil {
		return ToolOutput{}, err
	}
	if err := ValidateModelMessage(modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{call}}); err != nil {
		return ToolOutput{}, err
	}
	registration, ok := s.tools[call.Function.Name]
	if !ok {
		return ToolOutput{}, errors.New("tool_not_allowed")
	}
	s.mu.Lock()
	if _, duplicate := s.seen[call.ID]; duplicate {
		s.mu.Unlock()
		return ToolOutput{}, errors.New("duplicate_tool_call")
	}
	s.seen[call.ID] = struct{}{}
	s.mu.Unlock()
	toolCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	output, err := registration.Execute(toolCtx, call.Function.Arguments)
	if toolCtx.Err() != nil {
		return ToolOutput{}, toolCtx.Err()
	}
	if err != nil {
		return ToolOutput{}, err
	}
	if err := validateToolContent(output.Content); err != nil {
		return ToolOutput{}, err
	}
	if output.Question != nil {
		if output.Resume == nil || output.Question.ID == "" {
			return ToolOutput{}, errors.New("交互续行无效")
		}
		question := *output.Question
		question.Options = append([]QuestionOption(nil), question.Options...)
		output.Question = &question
		s.mu.Lock()
		s.pending[call.ID] = output
		s.mu.Unlock()
		// 返回展示数据与执行闭包隔离，外部不能绕过 Resolve 的身份校验。
		output.Resume = nil
		displayQuestion := *output.Question
		displayQuestion.Options = append([]QuestionOption(nil), displayQuestion.Options...)
		output.Question = &displayQuestion
	} else if output.Resume != nil {
		return ToolOutput{}, errors.New("缺少待处理问询")
	}
	return output, nil
}

// Resolve 只接受真实交互入口传入的回答。执行前消费待处理状态，重复或迟到
// 回答不能重放副作用；发生未知结果时应由正式应用命令的幂等协议核对。
func (s *ToolSet) Resolve(ctx context.Context, callID string, answer QuestionAnswer) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	pending, exists := s.pending[callID]
	if !exists {
		s.mu.Unlock()
		return "", errors.New("interaction_not_pending")
	}
	if err := validateAnswer(pending.Question, answer); err != nil {
		s.mu.Unlock()
		return "", err
	}
	delete(s.pending, callID)
	s.mu.Unlock()
	if answer.Status != QuestionAnswered {
		data, _ := json.Marshal(map[string]string{"question_id": answer.QuestionID, "status": string(answer.Status)})
		return string(data), nil
	}
	toolCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	result, err := pending.Resume(toolCtx, answer)
	if toolCtx.Err() != nil {
		return "", toolCtx.Err()
	}
	if err != nil {
		return "", err
	}
	return result, validateToolContent(result)
}

func validateToolContent(value string) error {
	if len(value) > 8<<10 || !utf8.ValidString(value) {
		return errors.New("tool_result_too_large_or_invalid")
	}
	return nil
}

func validateAnswer(question *PendingQuestion, answer QuestionAnswer) error {
	if question == nil || question.ID != answer.QuestionID {
		return errors.New("问题ID与当前待处理问题不匹配")
	}
	if !utf8.ValidString(answer.Custom) || len(answer.Custom) > 8<<10 {
		return errors.New("自定义回答无效")
	}
	if answer.Status == QuestionCancelled || answer.Status == QuestionUnavailable {
		if len(answer.OptionIDs) != 0 || answer.Custom != "" {
			return errors.New("取消或不可用回答不能包含选项或自定义内容")
		}
		return nil
	}
	if answer.Status != QuestionAnswered {
		return errors.New("问题回答状态无效")
	}
	allowed := map[string]bool{}
	for _, option := range question.Options {
		allowed[option.ID] = true
	}
	for _, id := range answer.OptionIDs {
		if !allowed[id] {
			return errors.New("问题回答包含未知或重复选项")
		}
		delete(allowed, id)
	}
	custom := strings.TrimSpace(answer.Custom) != ""
	if custom && !question.AllowCustom {
		return errors.New("当前问题不接受自定义回答")
	}
	if question.Mode == QuestionSingle {
		if custom && len(answer.OptionIDs) != 0 || !custom && len(answer.OptionIDs) != 1 {
			return errors.New("单选回答必须选择一个选项或只提供自定义内容")
		}
	} else if question.Mode != QuestionMultiple || len(answer.OptionIDs) == 0 && !custom {
		return errors.New("多选回答至少需要一个选项或自定义内容")
	}
	return nil
}

// DecodeArguments 供正式命令适配器解码对象，拒绝未知、重复字段和尾随载荷。
// 必填、枚举与字段间约束仍由该命令的真实参数校验器执行。
func DecodeArguments(raw string, target any) error {
	if len(raw) > agentlimits.MaxFileMutationArgumentsBytes || !utf8.ValidString(raw) {
		return errors.New("工具参数无效")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	if err := checkJSONObject(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("工具参数包含尾随载荷")
	}
	decoder = json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func checkJSONObject(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("工具参数必须是对象")
	}
	return checkObjectMembers(decoder)
}

func checkObjectMembers(decoder *json.Decoder) error {
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return errors.New("工具参数字段重复")
		}
		seen[name] = true
		if err := checkJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
}

func checkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return errors.New("工具参数不接受 null")
	}
	if token == json.Delim('{') {
		return checkObjectMembers(decoder)
	}
	if token == json.Delim('[') {
		for decoder.More() {
			if err := checkJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
	}
	return err
}
