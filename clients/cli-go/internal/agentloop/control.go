package agentloop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/mattn/go-runewidth"
)

type pendingInteractionKind string

const (
	pendingNone         pendingInteractionKind = ""
	pendingPreference   pendingInteractionKind = "preference"
	pendingQuestion     pendingInteractionKind = "question"
	pendingFileMutation pendingInteractionKind = "file_mutation"
)

type questionArgs struct {
	QuestionID string           `json:"question_id"`
	Header     string           `json:"header"`
	Question   string           `json:"question"`
	Mode       QuestionMode     `json:"mode"`
	Options    []QuestionOption `json:"options"`
}

func validReasoningEffort(value modelclient.ReasoningEffort) bool {
	switch value {
	case modelclient.ReasoningEffortAuto, modelclient.ReasoningEffortNone, modelclient.ReasoningEffortMinimal,
		modelclient.ReasoningEffortLow, modelclient.ReasoningEffortMedium, modelclient.ReasoningEffortHigh,
		modelclient.ReasoningEffortXHigh, modelclient.ReasoningEffortMax:
		return true
	default:
		return false
	}
}

// ReasoningEffort returns the effort that will be frozen into the next model request.
func (s *Session) ReasoningEffort() modelclient.ReasoningEffort {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.reasoningEffort
}

// SetReasoningEffort affects only future model requests. An in-flight request
// retains the value captured immediately before that request was started.
func (s *Session) SetReasoningEffort(value modelclient.ReasoningEffort) error {
	if value == "" {
		value = modelclient.ReasoningEffortAuto
	}
	if !validReasoningEffort(value) {
		return errors.New("Agent推理强度无效")
	}
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return ErrSessionClosed
	}
	s.reasoningEffort = value
	return nil
}

func (s *Session) frozenReasoningEffort() modelclient.ReasoningEffort {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.reasoningEffort
}

func (s *Session) FileAuthorizationMode() FileAuthorizationMode {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.fileAuthorizationMode == "" {
		return FileAuthorizationConfirm
	}
	return s.fileAuthorizationMode
}

func (s *Session) SetFileAuthorizationMode(mode FileAuthorizationMode) error {
	if mode != FileAuthorizationConfirm && mode != FileAuthorizationYOLO {
		return errors.New("文件授权模式无效")
	}
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return ErrSessionClosed
	}
	s.fileAuthorizationMode = mode
	return nil
}

type activityTurnContextKey struct{}

func withActivityTurn(ctx context.Context, turnID string) context.Context {
	if ctx == nil || turnID == "" {
		return ctx
	}
	return context.WithValue(ctx, activityTurnContextKey{}, turnID)
}

func (s *Session) publishActivity(ctx context.Context, activity Activity) {
	if ctx == nil {
		return
	}
	now := s.options.Now().UTC()
	turnID, _ := ctx.Value(activityTurnContextKey{}).(string)
	identity := strings.Join([]string{turnID, string(activity.Kind), activity.Event.Tool, activity.Event.ID}, "\x00")
	s.activityMu.Lock()
	started := s.activityStarts[identity]
	if started.IsZero() {
		started = now
		s.activityStarts[identity] = started
	}
	activity.StartedAt = started
	activity.UpdatedAt = now
	if activity.TimeoutBudget <= 0 {
		switch activity.Phase {
		case ActivityWaitingModel, ActivityReceivingStream:
			activity.TimeoutBudget = s.options.ModelTimeout
		case ActivityExecutingTool:
			activity.TimeoutBudget = s.options.ToolTimeout
		}
	}
	if activity.StableCode == "" {
		activity.StableCode = activity.Event.Detail
	}
	if activity.Progress != nil {
		progress := *activity.Progress
		activity.Progress = &progress
	}
	if activity.File != nil {
		detail := *activity.File
		activity.File = &detail
	}
	s.activityMu.Unlock()
	if reporter, exists := ctx.Value(activityReporterContextKey{}).(activityReporter); exists && reporter != nil {
		reportActivitySafely(reporter, activity)
	}
}

func preferContextError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func stableActivityCode(err error, fallback string) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "context_cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline_exceeded"
	}
	if code := modelclient.StableErrorCode(err); code != "" {
		return string(code)
	}
	return fallback
}

func safeActivityDelta(value string) string {
	if !utf8.ValidString(value) {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(value))
	for _, current := range value {
		if current == '\n' || current == '\t' {
			builder.WriteRune(current)
			continue
		}
		if unicode.IsControl(current) || unicode.In(current, unicode.Cf) {
			continue
		}
		builder.WriteRune(current)
	}
	return builder.String()
}

func decodeQuestionArgs(raw string) (questionArgs, error) {
	var args questionArgs
	if err := decodeArguments(raw, &args); err != nil {
		return args, err
	}
	args.QuestionID = strings.TrimSpace(args.QuestionID)
	args.Header = strings.TrimSpace(args.Header)
	args.Question = strings.TrimSpace(args.Question)
	if !validQuestionID(args.QuestionID) || !validQuestionText(args.Header, 48, 36) || !validQuestionText(args.Question, 160, 72) {
		return args, errors.New("question identity or text is invalid")
	}
	if args.Mode != QuestionSingle && args.Mode != QuestionMultiple {
		return args, errors.New("question mode is invalid")
	}
	if len(args.Options) < 2 || len(args.Options) > 4 {
		return args, errors.New("question options must contain 2 to 4 items")
	}
	seen := make(map[string]struct{}, len(args.Options))
	var secretText strings.Builder
	secretText.WriteString(args.Header)
	secretText.WriteByte('\n')
	secretText.WriteString(args.Question)
	for index := range args.Options {
		option := &args.Options[index]
		option.ID = strings.TrimSpace(option.ID)
		option.Label = strings.TrimSpace(option.Label)
		option.Description = strings.TrimSpace(option.Description)
		if !validQuestionID(option.ID) || !validQuestionText(option.Label, 48, 32) || !validQuestionText(option.Description, 120, 60) {
			return args, errors.New("question option is invalid")
		}
		if reservedCustomOption(option.ID, option.Label) {
			return args, errors.New("question option must not impersonate the client custom-input entry")
		}
		if _, duplicate := seen[option.ID]; duplicate {
			return args, errors.New("question option IDs must be unique")
		}
		seen[option.ID] = struct{}{}
		secretText.WriteByte('\n')
		secretText.WriteString(option.Label)
		secretText.WriteByte('\n')
		secretText.WriteString(option.Description)
	}
	if asksForSecret(secretText.String()) {
		return args, errors.New("question must not request credentials or recovery secrets")
	}
	return args, nil
}

func validQuestionID(value string) bool {
	if len(value) < 1 || len(value) > 64 || !utf8.ValidString(value) {
		return false
	}
	for index, current := range value {
		if current >= 'a' && current <= 'z' || current >= 'A' && current <= 'Z' || index > 0 && current >= '0' && current <= '9' || index > 0 && (current == '-' || current == '_') {
			continue
		}
		return false
	}
	return true
}

func reservedCustomOption(id, label string) bool {
	compactID := strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(id)))
	switch compactID {
	case "custom", "custominput", "other", "typesomething":
		return true
	}
	compactLabel := strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(label))), " ")
	switch compactLabel {
	case "custom", "custom input", "other", "type something", "自定义", "自定义输入", "其他", "其它", "其他输入", "其它输入":
		return true
	default:
		return false
	}
}

func validQuestionText(value string, maxRunes, maxDisplayWidth int) bool {
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes || questionDisplayWidth(value) > maxDisplayWidth {
		return false
	}
	for _, current := range value {
		if unicode.IsControl(current) || unicode.In(current, unicode.Cf) {
			return false
		}
	}
	return true
}

func questionDisplayWidth(value string) int {
	result := 0
	for _, current := range value {
		if unicode.In(current, unicode.Mn, unicode.Me) {
			continue
		}
		result += runewidth.RuneWidth(current)
	}
	return result
}

func validCustomAnswer(value string) bool {
	if value == "" || len(value) > maxUserInputBytes || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 2000 {
		return false
	}
	for _, current := range value {
		if current == '\n' || current == '\t' {
			continue
		}
		if unicode.IsControl(current) || unicode.In(current, unicode.Cf) {
			return false
		}
	}
	return true
}

func asksForSecret(value string) bool {
	lower := strings.ToLower(value)
	if englishCredentialSolicitation(lower) {
		return true
	}
	// Avoid treating ordinary teaching questions about cryptography as credential
	// solicitation merely because they contain words such as “密码”.
	objectText := strings.NewReplacer(
		"密码学", "", "密码算法", "", "密码技术", "",
	).Replace(lower)
	hasObject := containsEnglishCredentialObject(lower) || containsAny(objectText,
		"密码", "口令", "密钥", "令牌", "访问令牌", "刷新令牌", "认证令牌", "身份令牌", "私钥",
		"恢复码", "恢复代码", "备份码", "助记词", "种子短语", "凭据",
	)
	if !hasObject {
		return false
	}
	return containsAny(lower,
		"请粘贴", "请贴出", "请输入", "请填写", "请提供", "请发送", "请分享", "请提交", "请上传", "请出示",
		"粘贴", "告诉我", "给我", "发给我", "发送给我", "给我你的", "提供你的", "粘贴你的", "输入你的",
	) || (containsAny(lower, "你的", "您的") && containsAny(lower, "是什么", "是多少", "发来", "给出"))
}

var englishCredentialObjects = []string{
	"api key", "access key", "secret key", "client secret", "access token", "refresh token", "auth token", "authentication token", "bearer token",
	"api token", "session token", "id token", "password", "passcode", "private key", "recovery code", "backup code", "seed phrase", "mnemonic phrase", "token",
}

func englishCredentialSolicitation(value string) bool {
	normalized := " " + normalizeEnglishWords(value) + " "
	for _, object := range englishCredentialObjects {
		needle := " " + object + " "
		for searchStart := 0; searchStart < len(normalized); {
			relative := strings.Index(normalized[searchStart:], needle)
			if relative < 0 {
				break
			}
			index := searchStart + relative
			objectStart := index + 1
			objectEnd := objectStart + len(object)
			before := strings.TrimSpace(normalized[:objectStart])
			after := strings.TrimSpace(normalized[objectEnd:])
			if !credentialConceptContinuation(after) && (hasCredentialSolicitationPrefix(before) || hasUnsafeCredentialPossessive(before) || after == "please" || strings.HasPrefix(after, "please ") || strings.TrimSpace(normalized) == object && !containsNonASCIIWord(value)) {
				return true
			}
			searchStart = objectEnd
		}
	}
	return false
}

func containsNonASCIIWord(value string) bool {
	for _, current := range value {
		if current > unicode.MaxASCII && unicode.IsLetter(current) {
			return true
		}
	}
	return false
}

func hasCredentialSolicitationPrefix(value string) bool {
	actions := []string{
		"enter", "input", "paste", "provide", "provide me", "provide us", "provide me with", "provide us with", "send", "send me", "send us", "send to me", "send to us",
		"share", "share with me", "share with us", "submit", "upload", "type", "reveal", "disclose", "supply", "hand over", "hand me", "hand us",
		"post", "forward", "copy", "state", "write", "write down", "drop", "return", "include", "attach", "expose", "transmit",
		"show", "show me", "show us", "give", "give me", "give us", "tell", "tell me", "tell us", "reply with", "respond with",
	}
	for _, action := range actions {
		for _, determiner := range []string{"", "your", "the", "a", "an"} {
			prefix := strings.TrimSpace(action + " " + determiner)
			if value == prefix || strings.HasSuffix(value, " "+prefix) {
				return true
			}
		}
	}
	for _, prefix := range []string{
		"what is your", "what's your", "what is the", "what's the", "i need your", "i need the", "we need your", "we need the",
		"i want your", "i want the", "we want your", "we want the", "i require your", "i require the", "we require your", "we require the",
		"can i have your", "can i have the", "may i have your", "may i have the", "could i have your", "could i have the",
	} {
		if value == prefix || strings.HasSuffix(value, " "+prefix) {
			return true
		}
	}
	return false
}

func hasUnsafeCredentialPossessive(value string) bool {
	if value != "your" && !strings.HasSuffix(value, " your") {
		return false
	}
	for _, safe := range []string{
		"protect your", "secure your", "rotate your", "store your", "manage your", "hash your", "change your", "reset your",
		"choose your", "generate your", "revoke your", "delete your",
	} {
		if value == safe || strings.HasSuffix(value, " "+safe) {
			return false
		}
	}
	return true
}

func credentialConceptContinuation(value string) bool {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "algorithm", "algorithms", "authentication", "concept", "concepts", "cryptography", "derivation", "example", "examples", "explanation", "expiration", "expiry", "format", "formats", "hash", "hashing", "lifecycle", "management", "model", "models", "permission", "permissions", "policy", "policies", "rotation", "scope", "scopes", "security", "usage":
		return true
	default:
		return false
	}
}

func containsEnglishCredentialObject(value string) bool {
	normalized := " " + normalizeEnglishWords(value) + " "
	for _, object := range englishCredentialObjects {
		if strings.Contains(normalized, " "+object+" ") {
			return true
		}
	}
	return false
}

func normalizeEnglishWords(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, current := range value {
		switch {
		case current >= 'a' && current <= 'z', current >= '0' && current <= '9', current == '\'':
			builder.WriteRune(current)
		default:
			builder.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(builder.String()), " ")
}

func containsAny(value string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func pendingQuestionFromArgs(args questionArgs) *PendingQuestion {
	return &PendingQuestion{
		ID: args.QuestionID, Header: args.Header, Question: args.Question, Mode: args.Mode,
		Options: append([]QuestionOption(nil), args.Options...), AllowCustom: true,
	}
}

func clonePendingQuestion(value *PendingQuestion) *PendingQuestion {
	if value == nil {
		return nil
	}
	clone := *value
	clone.Options = append([]QuestionOption(nil), value.Options...)
	return &clone
}

func validateQuestionAnswer(pending *PendingQuestion, answer QuestionAnswer) (map[string]any, error) {
	if pending == nil || answer.QuestionID != pending.ID {
		return nil, errors.New("问题ID与当前待处理问题不匹配")
	}
	custom := strings.TrimSpace(answer.Custom)
	if custom != "" && !validCustomAnswer(custom) {
		return nil, errors.New("自定义回答无效")
	}
	switch answer.Status {
	case QuestionCancelled, QuestionUnavailable:
		if len(answer.OptionIDs) != 0 || custom != "" {
			return nil, errors.New("取消或不可用回答不能包含选项或自定义内容")
		}
		return map[string]any{"question_id": pending.ID, "status": string(answer.Status)}, nil
	case QuestionAnswered:
	default:
		return nil, errors.New("问题回答状态无效")
	}

	selected := make(map[string]struct{}, len(answer.OptionIDs))
	for _, id := range answer.OptionIDs {
		if _, duplicate := selected[id]; duplicate {
			return nil, errors.New("问题回答包含重复选项")
		}
		selected[id] = struct{}{}
	}
	ordered := make([]string, 0, len(selected))
	for _, option := range pending.Options {
		if _, ok := selected[option.ID]; ok {
			ordered = append(ordered, option.ID)
			delete(selected, option.ID)
		}
	}
	if len(selected) != 0 {
		return nil, errors.New("问题回答包含未知选项")
	}
	if pending.Mode == QuestionSingle {
		if custom != "" && len(ordered) != 0 || custom == "" && len(ordered) != 1 || len(ordered) > 1 {
			return nil, errors.New("单选回答必须选择一个选项或只提供自定义内容")
		}
	} else if len(ordered) == 0 && custom == "" {
		return nil, errors.New("多选回答至少需要一个选项或自定义内容")
	}
	value := map[string]any{
		"question_id": pending.ID,
		"status":      string(QuestionAnswered),
		"option_ids":  ordered,
	}
	if custom != "" {
		value["custom"] = custom
	}
	return value, nil
}

func questionResolutionSummary(status QuestionAnswerStatus) string {
	switch status {
	case QuestionAnswered:
		return "用户已回答问题"
	case QuestionCancelled:
		return "用户取消了问题"
	case QuestionUnavailable:
		return "当前无法回答问题"
	default:
		return fmt.Sprintf("问题状态：%s", status)
	}
}
