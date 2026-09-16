// Package learningcontent 拥有学习正文与版本，不拥有教学动作或评分规则。
package learningcontent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const Protocol = 1
const MaxBody = 256 << 10

var (
	ErrInvalid     = errors.New("invalid_learning_content")
	ErrNotFound    = errors.New("learning_content_not_found")
	ErrConflict    = errors.New("learning_content_conflict")
	ErrForbidden   = errors.New("learning_content_forbidden")
	ErrUnavailable = errors.New("learning_content_key_unavailable")
	ErrUnsupported = errors.New("learning_content_upgrade_required")
)

type Block struct {
	ID          string     `json:"block_id"`
	Kind        string     `json:"kind"`
	Text        string     `json:"text,omitempty"`
	Fallback    string     `json:"fallback"`
	Language    string     `json:"language,omitempty"`
	Rows        [][]string `json:"rows,omitempty"`
	ReferenceID string     `json:"reference_id,omitempty"`
	Children    []Block    `json:"children,omitempty"`
}

type Choice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// 作答能力独立于块样式；值仍传给原字符串答案合同，rubric 始终由 learning 读取。
type Interaction struct {
	Kind    string   `json:"kind"`
	Choices []Choice `json:"choices,omitempty"`
}

type Body struct {
	Blocks              []Block                       `json:"blocks"`
	Interaction         Interaction                   `json:"interaction"`
	References          []learning.KnowledgeReference `json:"references"`
	ModelID             string                        `json:"model_id"`
	InputFingerprint    string                        `json:"input_fingerprint"`
	SemanticFingerprint string                        `json:"semantic_fingerprint"`
}

type Revision struct {
	ProtocolVersion  int       `json:"protocol_version"`
	ArtifactID       string    `json:"artifact_id"`
	Version          int64     `json:"version"`
	CommittedVersion int64     `json:"committed_version"`
	SpaceID          string    `json:"learning_space_id"`
	GoalID           string    `json:"goal_id"`
	GoalRevisionID   string    `json:"goal_revision_id"`
	SessionID        string    `json:"session_id"`
	ActivityID       string    `json:"activity_id"`
	ActivityRevision int64     `json:"activity_revision"`
	Generation       int64     `json:"privacy_generation"`
	ActorDeviceID    string    `json:"actor_device_id"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"created_at"`
	Body             Body      `json:"body"`
}

type RevisionInfo struct {
	Version   int64     `json:"version"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type Source struct {
	SpaceID          string
	GoalID           string
	Activity         learning.Activity
	ModelID          string
	InputFingerprint string
}

type SourceReader interface {
	LearningContentSource(context.Context, pgx.Tx, string, string) (Source, error)
}

type Commit struct {
	ProtocolVersion int         `json:"protocol_version"`
	OperationID     string      `json:"operation_id"`
	ExpectedVersion int64       `json:"expected_version"`
	Status          string      `json:"status"`
	Blocks          []Block     `json:"blocks"`
	Interaction     Interaction `json:"interaction"`
}

var namespace = uuid.MustParse("282c78c0-48d9-48e9-999d-dcf13c059466")

// 仅适配原题独立行中的显式选项；不猜测自由文本或从 rubric 推导映射。
var choiceLine = regexp.MustCompile(`(?m)^[\t ]*([A-Za-z0-9]{1,8})[.)、:：][\t ]*([^\r\n]+?)[\t ]*\r?$`)

func ArtifactID(activity learning.Activity) string {
	return uuid.NewSHA1(namespace, []byte(fmt.Sprintf("activity:%s:%d", activity.ID, activity.Revision))).String()
}

func blockID(activity learning.Activity, role string) string {
	return uuid.NewSHA1(namespace, []byte(ArtifactID(activity)+":"+role)).String()
}

func fingerprint(value any) string {
	raw, _ := json.Marshal(value)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func SemanticFingerprint(a learning.Activity) string {
	return fingerprint(struct {
		Prompt string
		Type   learning.ActivityType
		Rubric learning.Rubric
		Help   []learning.HelpLevel
	}{a.Prompt, a.Type, a.Rubric, a.AllowedHelp})
}

// 适配是纯函数，不改旧 ID、事件或 rubric，也不从正确答案猜选项。
func Adapt(source Source) Body {
	a := source.Activity
	kind, interaction := "question", "text"
	if a.Type == learning.ActivityExplanation {
		kind, interaction = "markdown", "none"
	}
	blocks := []Block{{ID: blockID(a, "prompt"), Kind: kind, Text: a.Prompt, Fallback: a.Prompt}}
	if interaction != "none" {
		blocks = append(blocks, Block{ID: blockID(a, "answer"), Kind: "answer_input", Fallback: "请使用独立的正式答案控件作答。"})
	}
	refs := append([]learning.KnowledgeReference{}, a.References...)
	for _, ref := range refs {
		fallback := ref.Slice
		if strings.TrimSpace(fallback) == "" {
			fallback = "原资料节点版本：" + ref.NodeRevisionID + "。此旧引用未保存正文片段。"
		}
		blocks = append(blocks, Block{ID: blockID(a, "reference:"+ref.NodeRevisionID), Kind: "citation", ReferenceID: ref.NodeRevisionID, Fallback: fallback})
	}
	input := source.InputFingerprint
	if input == "" {
		input = fingerprint(a)
	}
	model := source.ModelID
	if model == "" {
		model = "legacy-activity"
	}
	return Body{Blocks: blocks, Interaction: Interaction{Kind: interaction}, References: refs, ModelID: model, InputFingerprint: input, SemanticFingerprint: SemanticFingerprint(a)}
}

func Validate(body Body, source Source, status string) error {
	if status != "committed" && status != "draft" && status != "failed" {
		return ErrInvalid
	}
	raw, err := json.Marshal(body)
	if err != nil || len(raw) > MaxBody || len(body.Blocks) == 0 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	count, questions, inputs := 0, 0, 0
	reading := false
	var walk func([]Block, int) error
	walk = func(blocks []Block, depth int) error {
		if depth > 8 {
			return ErrInvalid
		}
		for _, b := range blocks {
			count++
			if count > 256 || uuid.Validate(b.ID) != nil || seen[b.ID] || len(b.Kind) > 64 || strings.TrimSpace(b.Fallback) == "" || !utf8.ValidString(b.Fallback+b.Text) {
				return ErrInvalid
			}
			seen[b.ID] = true
			if source.Activity.Type == learning.ActivityExplanation && b.ID == blockID(source.Activity, "prompt") && b.Text == source.Activity.Prompt && b.Fallback == b.Text {
				reading = true
			}
			switch b.Kind {
			case "question":
				questions++
				if b.ID != blockID(source.Activity, "prompt") || b.Text != source.Activity.Prompt || b.Fallback != b.Text {
					return ErrInvalid
				}
			case "answer_input":
				inputs++
				if b.ID != blockID(source.Activity, "answer") {
					return ErrInvalid
				}
			case "table":
				if len(b.Rows) == 0 || len(b.Rows) > 100 {
					return ErrInvalid
				}
				for _, row := range b.Rows {
					if len(row) == 0 || len(row) > 20 {
						return ErrInvalid
					}
				}
			case "citation":
				found := false
				for _, ref := range body.References {
					if ref.NodeRevisionID == b.ReferenceID {
						found = true
					}
				}
				if !found {
					return ErrInvalid
				}
			case "group":
				if len(b.Children) == 0 {
					return ErrInvalid
				}
			case "markdown", "code", "math", "callout":
				if strings.TrimSpace(b.Text) == "" {
					return ErrInvalid
				}
			default:
				if b.Kind == "" {
					return ErrInvalid
				}
			}
			if b.Kind != "group" && len(b.Children) > 0 {
				return ErrInvalid
			}
			if err := walk(b.Children, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(body.Blocks, 0); err != nil {
		return err
	}
	if status != "committed" {
		return nil
	}
	if source.Activity.Type == learning.ActivityExplanation {
		if body.Interaction.Kind != "none" || inputs != 0 || questions != 0 {
			return ErrInvalid
		}
		// 阅读正文必须保留正规原文；附加展示不能悄悄替换原资料。
		if !reading {
			return ErrInvalid
		}
		return nil
	}
	if questions != 1 || inputs != 1 {
		return ErrInvalid
	}
	switch body.Interaction.Kind {
	case "text":
		if len(body.Interaction.Choices) > 0 {
			return ErrInvalid
		}
	case "single_choice":
		if len(body.Interaction.Choices) < 2 || len(body.Interaction.Choices) > 20 {
			return ErrInvalid
		}
		original := map[string]string{}
		for _, match := range choiceLine.FindAllStringSubmatch(source.Activity.Prompt, -1) {
			if _, duplicate := original[match[1]]; duplicate {
				return ErrInvalid
			}
			original[match[1]] = strings.TrimSpace(match[2])
		}
		if len(original) != len(body.Interaction.Choices) {
			return ErrInvalid
		}
		values := map[string]bool{}
		for _, c := range body.Interaction.Choices {
			// 标签、答案值与完整选项集必须匹配；展示不能交换答案含义或隐藏选项。
			if strings.TrimSpace(c.Label) == "" || values[c.Value] || original[c.Value] != c.Label {
				return ErrInvalid
			}
			values[c.Value] = true
		}
	default:
		// 未知交互允许有完整阅读回退，任何提交入口都必须拒绝。
		if strings.TrimSpace(body.Interaction.Kind) == "" || len(body.Interaction.Kind) > 64 {
			return ErrInvalid
		}
	}
	return nil
}

func CanAnswer(r Revision, answer string) error {
	if r.Status != "committed" || r.Version != r.CommittedVersion {
		return ErrConflict
	}
	if len(answer) > learning.MaxAnswerBytes || !utf8.ValidString(answer) || strings.TrimSpace(answer) == "" {
		return ErrInvalid
	}
	switch r.Body.Interaction.Kind {
	case "text":
		return nil
	case "single_choice":
		for _, c := range r.Body.Interaction.Choices {
			if answer == c.Value {
				return nil
			}
		}
		return ErrInvalid
	default:
		return ErrUnsupported
	}
}
