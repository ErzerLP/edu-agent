package agentcore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	core "github.com/edu-agent/edu-agent/packages/agentcore"
)

func TestToolSetDefaultDeniesEveryCapability(t *testing.T) {
	set, err := core.NewToolSet(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Definitions()) != 0 {
		t.Fatal("核心默认暴露工具")
	}
	for _, name := range []string{"shell", "sql", "task", "write", "approve"} {
		if _, err := set.Invoke(context.Background(), call(name, name, `{}`)); err == nil || err.Error() != "tool_not_allowed" {
			t.Fatalf("意外能力 %s：%v", name, err)
		}
	}
}

func TestToolArgumentsRejectAuthorityForgeryAndMalformedJSON(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"view":"progress","view":"admin"}`, `{"view":null}`, `{"view":"progress"}{}`, `{"view":"progress","scope":"admin"}`, `{"view":"progress","approved":true}`, `{"view":"progress","nested":{"x":1,"x":2}}`, `{"view":"progress"`, strings.Repeat(" ", 65537)} {
		var args struct {
			View string `json:"view"`
		}
		if err := core.DecodeArguments(raw, &args); err == nil {
			t.Fatalf("非法参数被接受：%.100s", raw)
		}
	}
	var args struct {
		View string `json:"view"`
	}
	if err := core.DecodeArguments(`{"view":"progress"}`, &args); err != nil || args.View != "progress" {
		t.Fatalf("合法参数失败：%+v，%v", args, err)
	}
}

func TestToolResultBoundaryAndCancelledInteraction(t *testing.T) {
	set, err := core.NewToolSet(time.Second, registration("large", func(context.Context, string) (core.ToolOutput, error) {
		return core.ToolOutput{Content: strings.Repeat("x", 8193)}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Invoke(context.Background(), call("large", "large", `{}`)); err == nil {
		t.Fatal("接受了无界工具结果")
	}
	executed := 0
	set, err = core.NewToolSet(time.Second, registration("ask", func(context.Context, string) (core.ToolOutput, error) {
		return core.ToolOutput{Question: &core.PendingQuestion{ID: "q"}, Resume: func(context.Context, core.QuestionAnswer) (string, error) { executed++; return `{}`, nil }}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	output, err := set.Invoke(context.Background(), call("ask", "ask", `{}`))
	if err != nil || output.Resume != nil {
		t.Fatalf("交互泄露了执行闭包：%v", err)
	}
	if _, err := set.Resolve(context.Background(), "ask", core.QuestionAnswer{QuestionID: "q", Status: core.QuestionCancelled}); err != nil || executed != 0 {
		t.Fatalf("取消仍执行：%d，%v", executed, err)
	}
}
