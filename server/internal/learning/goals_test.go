package learning

import (
	"strings"
	"testing"
	"time"
)

func TestLegacyGoalLongTextRemainsEditable(t *testing.T) {
	g := GoalRevision{Text: strings.Repeat("学习并发", 200)}
	d := g.GoalManagement().Details
	d.Priority = "high"
	if len([]rune(d.Name)) != 120 || g.GoalManagement().Status != "active" {
		t.Fatal("旧目标摘要或状态不兼容")
	}
	m, err := nextGoalManagement(&g, GoalCommand{Text: g.Text, Details: &d}, "device", time.Now())
	if err != nil || m.Details.Priority != "high" || len([]rune(g.Text)) != 800 {
		t.Fatalf("旧目标不能补充结构化信息或原正文被截断：%v", err)
	}
}

func TestGoalLifecycleAndCompletionDoNotAssertMastery(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	command := GoalCommand{Text: "掌握并发"}
	m, err := nextGoalManagement(nil, command, "device", now)
	if err != nil || m.Status != "draft" || m.Details.Deadline != nil || m.Details.WeeklyMinutes != nil {
		t.Fatalf("草稿错误：%+v %v", m, err)
	}
	g := GoalRevision{Text: command.Text, Management: m}
	for _, action := range []string{"start", "pause", "resume", "archive", "restore", "complete", "archive", "restore"} {
		command.Action = action
		command.CompletionReason = ""
		if action == "complete" {
			command.CompletionReason = "面试准备完成"
		}
		m, err = nextGoalManagement(&g, command, "device", now)
		if err != nil {
			t.Fatal(action, err)
		}
		g.Management = m
	}
	if m.Status != "completed" || m.Completion == nil || m.Completion.Kind != "manual" || m.CriteriaVerification != "unverified" || m.Completion.Reason != "面试准备完成" {
		t.Fatalf("手动完成语义错误：%+v", m)
	}
	command.Action = "resume"
	command.CompletionReason = ""
	if _, err = nextGoalManagement(&g, command, "device", now); ErrorCode(err) != CodeInvalidTransition {
		t.Fatalf("已完成目标不应被暂停恢复隐式重启：%v", err)
	}
}

func TestGoalDetailsTimezoneAndRevisionChanges(t *testing.T) {
	d := GoalDetails{Name: "Go", Priority: "normal"}
	minutes := 60
	d.WeeklyMinutes = &minutes
	if d.Validate() == nil {
		t.Fatal("时间预算缺少明确时区仍被接受")
	}
	d.Timezone = "Asia/Shanghai"
	deadline, _ := time.Parse(time.RFC3339, "2026-10-01T10:00:00+08:00")
	d.Deadline = &deadline
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	wrong, _ := time.Parse(time.RFC3339, "2026-10-01T10:00:00Z")
	d.Deadline = &wrong
	if d.Validate() == nil {
		t.Fatal("截止日期偏移与时区不符仍被接受")
	}
	d.Deadline = &deadline
	g := GoalRevision{Text: "Go", Management: &GoalManagement{Details: GoalDetails{Name: "Go"}, Status: "active", CriteriaVerification: "unverified"}}
	d.Scope = "并发"
	d.SelfAssessment = "自述精通全部知识"
	m, err := nextGoalManagement(&g, GoalCommand{Text: g.Text, Details: &d}, "device", time.Now())
	if err != nil || !m.RouteAdjustmentNeeded || m.CriteriaVerification != "unverified" || m.Completion != nil {
		t.Fatalf("修订错误：%+v %v", m, err)
	}
	if g.Management.Details.Scope != "" {
		t.Fatal("修订覆盖了旧版本")
	}
	if _, err = nextGoalManagement(&g, GoalCommand{Text: g.Text, Details: &d, Action: "archive"}, "device", time.Now()); err == nil {
		t.Fatal("状态操作混合内容替换")
	}
}
