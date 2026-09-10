package workbench

import (
	"context"
	"testing"
)

func TestAgentEntryKeepsSelectedSpaceGoalAndTeaching(t *testing.T) {
	m := New(t.Context(), serviceFunc(func(context.Context, Request) (Page, error) { return Page{}, nil }), "space-A")
	cmd := m.choose("select:agent:goal-A/teaching-A")
	if cmd == nil {
		t.Fatal("没有生成聊天入口")
	}
	selection, ok := cmd().(AgentMsg)
	if !ok || selection.Space != "space-A" || selection.Goal != "goal-A" || selection.Session != "teaching-A" {
		t.Fatalf("上下文丢失：%+v", selection)
	}
}
