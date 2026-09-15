package agentcore

// 问询 DTO 与 CLI 共用；它们只表达交互数据，不包含身份、scope 或执行凭据。
type QuestionMode string

const (
	QuestionSingle   QuestionMode = "single"
	QuestionMultiple QuestionMode = "multiple"
)

type QuestionOption struct {
	ID          string
	Label       string
	Description string
}

type PendingQuestion struct {
	ID          string
	Header      string
	Question    string
	Mode        QuestionMode
	Options     []QuestionOption
	AllowCustom bool
}

type QuestionAnswerStatus string

const (
	QuestionAnswered    QuestionAnswerStatus = "answered"
	QuestionCancelled   QuestionAnswerStatus = "cancelled"
	QuestionUnavailable QuestionAnswerStatus = "unavailable"
)

type QuestionAnswer struct {
	QuestionID string
	Status     QuestionAnswerStatus
	OptionIDs  []string
	Custom     string
}
