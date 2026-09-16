package learningcontent

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/google/uuid"
)

func fixtureSource() Source {
	return Source{Activity: learning.Activity{ID: uuid.NewString(), Revision: 1, SessionID: uuid.NewString(), Prompt: "哪个是偶数？\nA. 2\nB. 3", Type: learning.ActivityObjective, Rubric: learning.Rubric{Revision: "r1", ObjectiveRule: &learning.ObjectiveRule{AcceptedAnswers: []string{"A"}}}, AllowedHelp: []learning.HelpLevel{learning.HelpNone, learning.HelpHint}}}
}

func TestLegacyIdentityAndSemanticSeparation(t *testing.T) {
	source := fixtureSource()
	before := source.Activity
	body := Adapt(source)
	if !reflect.DeepEqual(body, Adapt(source)) || !reflect.DeepEqual(before, source.Activity) {
		t.Fatal("旧内容适配不确定或修改了活动")
	}
	if err := Validate(body, source, "committed"); err != nil {
		t.Fatal(err)
	}
	body.Blocks = append([]Block{{ID: uuid.NewString(), Kind: "future_chart", Fallback: "图示：2 可以分为两个 1。"}}, body.Blocks...)
	body.Interaction = Interaction{Kind: "single_choice", Choices: []Choice{{Value: "A", Label: "2"}, {Value: "B", Label: "3"}}}
	if err := Validate(body, source, "committed"); err != nil {
		t.Fatal(err)
	}
	if body.SemanticFingerprint != SemanticFingerprint(before) {
		t.Fatal("展示改变了评分语义")
	}
	body.Blocks[1].Text = "篡改原题"
	if Validate(body, source, "committed") == nil {
		t.Fatal("允许展示修改题意")
	}
}

func TestIncompleteAndUnknownCannotSubmit(t *testing.T) {
	source := fixtureSource()
	body := Adapt(source)
	body.Blocks = body.Blocks[:1]
	if Validate(body, source, "committed") == nil {
		t.Fatal("未完成题目可提交")
	}
	if err := Validate(body, source, "draft"); err != nil {
		t.Fatal(err)
	}
	r := Revision{Version: 1, CommittedVersion: 1, Status: "draft", Body: body}
	if CanAnswer(r, "A") == nil {
		t.Fatal("草稿可作答")
	}
	r.Status = "failed"
	if CanAnswer(r, "A") == nil {
		t.Fatal("失败输出可作答")
	}
	r.Status = "committed"
	r.Body.Interaction.Kind = "code_execution"
	if CanAnswer(r, "A") != ErrUnsupported {
		t.Fatal("未知交互未受限")
	}
	r.Body.Interaction = Interaction{Kind: "single_choice", Choices: []Choice{{Value: "A"}, {Value: "B"}}}
	if CanAnswer(r, "C") != ErrInvalid || CanAnswer(r, "A") != nil {
		t.Fatal("选项值校验错误")
	}
	r.CommittedVersion = 2
	if CanAnswer(r, "A") != ErrConflict {
		t.Fatal("旧正式版本可提交")
	}
}

func TestChoiceLabelsCannotSwapAnswerMeaning(t *testing.T) {
	source := fixtureSource()
	body := Adapt(source)
	body.Interaction = Interaction{Kind: "single_choice", Choices: []Choice{{Value: "A", Label: "3"}, {Value: "B", Label: "2"}}}
	if Validate(body, source, "committed") == nil {
		t.Fatal("标签和值虽都出现在原题中，但交换对应关系会改变作答语义")
	}
	body.Interaction.Choices = []Choice{{Value: "B", Label: "3"}, {Value: "A", Label: "2"}}
	if err := Validate(body, source, "committed"); err != nil {
		t.Fatal("仅调整完整选项的展示顺序不应被拒绝", err)
	}
	source.Activity.Prompt += "\nC. 4"
	body.Blocks = Adapt(source).Blocks
	if Validate(body, source, "committed") == nil {
		t.Fatal("允许隐藏原题中的选项")
	}
	source.Activity.Prompt = "请选择 A 或 B"
	body.Blocks = Adapt(source).Blocks
	if Validate(body, source, "committed") == nil {
		t.Fatal("没有确定选项对应关系时不应猜测")
	}
}

func TestBlockBoundsAndReading(t *testing.T) {
	source := fixtureSource()
	body := Adapt(source)
	body.Blocks = append(body.Blocks, body.Blocks[0])
	if Validate(body, source, "draft") == nil {
		t.Fatal("允许重复块 ID")
	}
	body = Adapt(source)
	body.Blocks[0].Fallback = ""
	if Validate(body, source, "committed") == nil {
		t.Fatal("缺少语义回退")
	}
	source.Activity.Type = learning.ActivityExplanation
	body = Adapt(source)
	if body.Interaction.Kind != "none" || len(body.Blocks) != 1 || Validate(body, source, "committed") != nil {
		t.Fatal("阅读强制答题")
	}
	body.Blocks = []Block{{ID: uuid.NewString(), Kind: "group", Fallback: source.Activity.Prompt, Children: body.Blocks}}
	if Validate(body, source, "committed") != nil {
		t.Fatal("正规阅读正文不能自由分组")
	}
	body.Blocks[0].Children[0].ID = uuid.NewString()
	if Validate(body, source, "committed") == nil {
		t.Fatal("正规正文块身份可随版本漂移")
	}
	source.Activity.References = []learning.KnowledgeReference{{NodeRevisionID: uuid.NewString()}}
	if Validate(Adapt(source), source, "committed") != nil {
		t.Fatal("没有正文片段的旧引用导致整个会话不可读")
	}
}

func TestCiphertextBindsVersionAndGeneration(t *testing.T) {
	s, err := New(nil, nil, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	r := Revision{ArtifactID: uuid.NewString(), ActivityID: uuid.NewString(), Version: 1, Generation: 1, Status: "committed", Body: Adapt(fixtureSource())}
	raw, err := s.seal(r)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("哪个是偶数")) {
		t.Fatal("正文落明文")
	}
	copy := r
	copy.Body = Body{}
	if err := s.open(&copy, raw); err != nil || !reflect.DeepEqual(copy.Body, r.Body) {
		t.Fatalf("解密失败：%v", err)
	}
	copy.Version++
	if s.open(&copy, raw) == nil {
		t.Fatal("密文可移植到别的版本")
	}
	copy = r
	copy.Generation++
	if s.open(&copy, raw) == nil {
		t.Fatal("密文可跨隐私代次复活")
	}
}
