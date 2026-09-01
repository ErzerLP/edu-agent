package agentui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func TestAgentUIWorkspaceActivityDetailsAreStructuredSafeAndUpserted(t *testing.T) {
	searchRunning := agentloop.Activity{
		Kind: agentloop.ActivityTool, Phase: agentloop.ActivityExecutingTool, TimeoutBudget: 30 * time.Second,
		Event: agentloop.Event{ID: "search-call", Tool: "search", Summary: "正在搜索 src：已扫描 1 个文件", Status: agentloop.EventRunning},
		File:  &agentloop.FileActivityDetail{Path: "src", ScannedFiles: 1, ScannedBytes: 128, HasScanned: true, Matches: 1, HasMatches: true},
	}
	searchUpdated := searchRunning
	searchUpdated.Event.Summary = "正在搜索 src：已扫描 40 个文件"
	searchUpdated.File = &agentloop.FileActivityDetail{Path: "src", ScannedFiles: 40, ScannedBytes: 4096, HasScanned: true, Matches: 3, HasMatches: true}
	searchDone := searchUpdated
	searchDone.Event = agentloop.Event{ID: "search-call", Tool: "search", Summary: "搜索达到匹配上限", Status: agentloop.EventFailed, Detail: "match_limit"}
	searchDone.StableCode = "match_limit"
	searchDone.File.TruncationReason = "match_limit"
	writeDone := agentloop.Activity{
		Kind: agentloop.ActivityTool, Phase: agentloop.ActivityExecutingTool,
		Event: agentloop.Event{ID: "write-call", Tool: "write", Summary: "已完成 write_replace：notes.md", Status: agentloop.EventSucceeded},
		File: &agentloop.FileActivityDetail{
			Path: "notes.md", Operation: "write_replace", PreviewKind: "diff", Preview: "-old\n+new\n",
			FirstChangedLine: 1, PublicationOutcome: "completed",
		},
	}
	conversation := &fakeConversation{
		workspaceStatus: agentloop.WorkspaceStatus{Available: true, Label: "project"},
		activities:      []agentloop.Activity{searchRunning, searchUpdated, searchDone, writeDone},
		result: agentloop.Result{
			Text:   "文件检查已结束。",
			Events: []agentloop.Event{searchDone.Event, writeDone.Event},
		},
	}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("检查文件")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = runTurn(t, updated.(model), command)
	collapsed := value.viewport.View()
	if !strings.Contains(collapsed, "工具调用 · 2 项") || strings.Count(collapsed, "搜索文件") != 1 ||
		!strings.Contains(collapsed, "src") || !strings.Contains(collapsed, "notes.md") || !strings.Contains(collapsed, "完成") {
		t.Fatalf("collapsed workspace timeline=%s", collapsed)
	}
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	value = updated.(model)
	expanded := value.viewport.View()
	for _, expected := range []string{
		"路径：src", "扫描：40 个文件 · 4096 字节 · 匹配：3", "截断原因：match_limit", "代码：match_limit",
		"路径：notes.md", "操作：write_replace", "发布结果：completed", "首个变化行：1", "最终差异：", "-old", "+new",
	} {
		if !strings.Contains(expanded, expected) {
			t.Fatalf("expanded workspace timeline missing %q: %s", expected, expanded)
		}
	}
	for _, forbidden := range []string{"/home/private", `{"path"`, "expected_hash", "sha256:", "provider reasoning"} {
		if strings.Contains(expanded, forbidden) {
			t.Fatalf("expanded workspace timeline leaked %q: %s", forbidden, expanded)
		}
	}
}

func TestAgentUISlowWorkspaceSearchShowsProgressBudgetAndEscInOneActivity(t *testing.T) {
	conversation := &fakeConversation{workspaceStatus: agentloop.WorkspaceStatus{Available: true, Label: "project"}}
	value := newModel(t.Context(), conversation, "model")
	value.busy = true
	value.activeCancelable = true
	value.activeTurnID = 1
	value.activeStarted = time.Now().Add(-slowTurnThreshold - time.Second)
	started := time.Now().Add(-9 * time.Second)
	for _, detail := range []*agentloop.FileActivityDetail{
		{Path: "src", ScannedFiles: 1, ScannedBytes: 128, HasScanned: true, Matches: 0, HasMatches: true},
		{Path: "src", ScannedFiles: 12, ScannedBytes: 8192, HasScanned: true, Matches: 4, HasMatches: true},
	} {
		value.handleActivity(1, agentloop.Activity{
			Kind: agentloop.ActivityTool, Phase: agentloop.ActivityExecutingTool, StartedAt: started, UpdatedAt: time.Now(), TimeoutBudget: 30 * time.Second,
			Event: agentloop.Event{ID: "search-call", Tool: "search", Summary: "正在搜索 src", Status: agentloop.EventRunning}, File: detail,
		})
	}
	toolActivities := 0
	for _, entry := range value.entries {
		if entry.kind == entryTools {
			toolActivities += len(entry.activities)
		}
	}
	detail := value.slowTurnDetail()
	for _, expected := range []string{"已等待", "已扫描 12 个文件 / 8192 字节", "匹配 4", "超时预算 30s", "Esc"} {
		if !strings.Contains(detail, expected) {
			t.Fatalf("slow detail missing %q: %s", expected, detail)
		}
	}
	if toolActivities != 1 {
		t.Fatalf("progress appended duplicate activities: %d entries=%+v", toolActivities, value.entries)
	}
	value.toolsExpanded = true
	value.refreshTranscript(false)
	if transcript := value.viewport.View(); !strings.Contains(transcript, "扫描：12 个文件 · 8192 字节 · 匹配：4") {
		t.Fatalf("live search progress missing: %s", transcript)
	}
}

func TestAgentUISlowWorkspaceReadShowsPathProgressBudgetEscAndStoppedState(t *testing.T) {
	conversation := &fakeConversation{workspaceStatus: agentloop.WorkspaceStatus{Available: true, Label: "project"}}
	value := newModel(t.Context(), conversation, "model")
	value.busy = true
	value.activeCancelable = true
	value.activeTurnID = 1
	value.activeStarted = time.Now().Add(-slowTurnThreshold - time.Second)
	started := time.Now().Add(-9 * time.Second)
	running := agentloop.Activity{
		Kind: agentloop.ActivityTool, Phase: agentloop.ActivityExecutingTool, StartedAt: started, UpdatedAt: time.Now(), TimeoutBudget: 30 * time.Second,
		Event: agentloop.Event{ID: "read-call", Tool: "read", Summary: "正在读取 notes.md 第 3 行起", Status: agentloop.EventRunning},
		File:  &agentloop.FileActivityDetail{Path: "notes.md", StartLine: 3, HasRange: true, Bytes: 8192, HasBytes: true},
	}
	value.handleActivity(1, running)
	for _, expected := range []string{"已等待", "正在读取 notes.md", "从第 3 行开始", "已处理 8192 字节", "超时预算 30s", "Esc"} {
		if detail := value.slowTurnDetail(); !strings.Contains(detail, expected) {
			t.Fatalf("slow read detail missing %q: %s", expected, detail)
		}
	}
	stopped := running
	stopped.Phase = agentloop.ActivityStopped
	stopped.Event = agentloop.Event{ID: "read-call", Tool: "read", Summary: "工作区文件操作已取消", Status: agentloop.EventFailed, Detail: "cancelled"}
	stopped.StableCode = "cancelled"
	value.handleActivity(1, stopped)
	value.toolsExpanded = true
	value.refreshTranscript(false)
	transcript := value.viewport.View()
	for _, expected := range []string{"路径：notes.md", "范围：从第 3 行开始", "返回字节：8192", "代码：cancelled"} {
		if !strings.Contains(transcript, expected) {
			t.Fatalf("stopped read missing %q: %s", expected, transcript)
		}
	}
	if strings.Count(transcript, "读取文件") != 1 {
		t.Fatalf("read activity did not upsert: %s", transcript)
	}
}

func TestAgentUIStoppingAppliesTerminalWorkspaceActivityUnderOriginalCall(t *testing.T) {
	for _, test := range []struct {
		tool   string
		path   string
		detail *agentloop.FileActivityDetail
	}{
		{tool: "read", path: "notes.md", detail: &agentloop.FileActivityDetail{Path: "notes.md", StartLine: 3, HasRange: true, Bytes: 128, HasBytes: true}},
		{tool: "search", path: "src", detail: &agentloop.FileActivityDetail{Path: "src", ScannedFiles: 2, ScannedBytes: 256, HasScanned: true, Matches: 1, HasMatches: true}},
		{tool: "write", path: "notes.md", detail: &agentloop.FileActivityDetail{Path: "notes.md", Operation: "write_create", PreviewKind: "content", Preview: "hello\n"}},
		{tool: "edit", path: "notes.md", detail: &agentloop.FileActivityDetail{Path: "notes.md", Operation: "edit", PreviewKind: "diff", Preview: "-old\n+new\n"}},
	} {
		t.Run(test.tool, func(t *testing.T) {
			value := newModel(t.Context(), &fakeConversation{workspaceStatus: agentloop.WorkspaceStatus{Available: true, Label: "project"}}, "model")
			value.busy = true
			value.activeCancelable = true
			value.activeTurnID = 1
			value.activeTurnCancel = func() {}
			running := agentloop.Activity{
				Kind: agentloop.ActivityTool, Phase: agentloop.ActivityExecutingTool,
				Event: agentloop.Event{ID: test.tool + "-call", Tool: test.tool, Summary: "工作区文件操作进行中", Status: agentloop.EventRunning},
				File:  test.detail,
			}
			updated, _ := value.Update(turnMsg{turnID: 1, activity: &running})
			value = updated.(model)
			updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyEsc})
			value = updated.(model)
			if !value.stopping {
				t.Fatal("Esc did not enter stopping state")
			}

			late := running
			late.File = &agentloop.FileActivityDetail{Path: "late.md", Bytes: 999, HasBytes: true}
			updated, _ = value.Update(turnMsg{turnID: 1, activity: &late})
			value = updated.(model)
			if value.activeFileDetail == nil || value.activeFileDetail.Path != test.path {
				t.Fatalf("late running activity replaced active detail: %+v", value.activeFileDetail)
			}

			stopped := running
			stopped.Phase = agentloop.ActivityStopped
			stopped.Event = agentloop.Event{ID: test.tool + "-call", Tool: test.tool, Summary: "工作区文件操作已取消", Status: agentloop.EventFailed, Detail: "cancelled"}
			stopped.StableCode = "cancelled"
			updated, _ = value.Update(turnMsg{turnID: 1, activity: &stopped})
			value = updated.(model)
			value.toolsExpanded = true
			value.refreshTranscript(false)
			transcript := value.viewport.View()
			if !strings.Contains(transcript, "路径："+test.path) || !strings.Contains(transcript, "代码：cancelled") || strings.Contains(transcript, "late.md") {
				t.Fatalf("terminal workspace activity not applied safely: %s", transcript)
			}
			if strings.Count(transcript, "代码：cancelled") != 1 {
				t.Fatalf("terminal activity did not upsert original call: %s", transcript)
			}
		})
	}
}

func TestTurnStreamCarriesTerminalWorkspaceActivityAcrossEscCancellation(t *testing.T) {
	for _, test := range []struct {
		tool   string
		detail *agentloop.FileActivityDetail
	}{
		{tool: "read", detail: &agentloop.FileActivityDetail{Path: "notes.md", StartLine: 3, HasRange: true}},
		{tool: "search", detail: &agentloop.FileActivityDetail{Path: "src", ScannedFiles: 2, ScannedBytes: 256, HasScanned: true}},
		{tool: "write", detail: &agentloop.FileActivityDetail{Path: "notes.md", Operation: "write_create", PreviewKind: "content", Preview: "hello\n"}},
		{tool: "edit", detail: &agentloop.FileActivityDetail{Path: "notes.md", Operation: "edit", PreviewKind: "diff", Preview: "-old\n+new\n"}},
	} {
		t.Run(test.tool, func(t *testing.T) {
			for iteration := 0; iteration < 32; iteration++ {
				deliveryCtx, cancelDelivery := context.WithCancel(t.Context())
				turnCtx, cancelTurn := context.WithCancel(deliveryCtx)
				value := newModel(deliveryCtx, &fakeConversation{workspaceStatus: agentloop.WorkspaceStatus{Available: true, Label: "project"}}, "model")
				value.busy = true
				value.activeCancelable = true
				value.activeTurnID = 1
				value.activeTurnCancel = cancelTurn

				running := agentloop.Activity{
					Kind: agentloop.ActivityTool, Phase: agentloop.ActivityExecutingTool,
					Event: agentloop.Event{ID: test.tool + "-call", Tool: test.tool, Summary: "工作区文件操作进行中", Status: agentloop.EventRunning},
					File:  test.detail,
				}
				stopped := running
				stopped.Phase = agentloop.ActivityStopped
				stopped.Event = agentloop.Event{ID: test.tool + "-call", Tool: test.tool, Summary: "工作区文件操作已取消", Status: agentloop.EventFailed, Detail: "cancelled"}
				stopped.StableCode = "cancelled"

				start := startTurnCmd(deliveryCtx, turnCtx, 1, turnSend, func(ctx context.Context) (agentloop.Result, error) {
					agentloop.PublishActivity(ctx, running)
					<-ctx.Done()
					agentloop.PublishActivity(ctx, stopped)
					return agentloop.Result{}, ctx.Err()
				})
				initial := start().(turnMsg)
				updated, wait := value.Update(initial)
				value = updated.(model)
				if wait == nil {
					t.Fatalf("iteration %d did not start stream wait", iteration)
				}
				runningMessage := wait().(turnMsg)
				if runningMessage.activity == nil || runningMessage.activity.Event.Status != agentloop.EventRunning {
					t.Fatalf("iteration %d first stream message=%+v", iteration, runningMessage)
				}
				updated, wait = value.Update(runningMessage)
				value = updated.(model)
				updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyEsc})
				value = updated.(model)
				if !value.stopping || wait == nil {
					t.Fatalf("iteration %d did not enter stopping state", iteration)
				}

				terminalMessage := wait().(turnMsg)
				if terminalMessage.activity == nil || terminalMessage.activity.Phase != agentloop.ActivityStopped || terminalMessage.activity.StableCode != "cancelled" {
					t.Fatalf("iteration %d terminal stream message=%+v", iteration, terminalMessage)
				}
				updated, wait = value.Update(terminalMessage)
				value = updated.(model)
				if wait == nil {
					t.Fatalf("iteration %d terminal activity did not continue draining", iteration)
				}
				completion := wait().(turnMsg)
				if !completion.done {
					t.Fatalf("iteration %d completion=%+v", iteration, completion)
				}
				updated, _ = value.Update(completion)
				value = updated.(model)
				value.toolsExpanded = true
				value.refreshTranscript(false)
				transcript := value.viewport.View()
				if !strings.Contains(transcript, "代码：cancelled") || strings.Count(transcript, "代码：cancelled") != 1 || strings.Contains(transcript, "进行中") {
					t.Fatalf("iteration %d terminal activity was not delivered and upserted: %s", iteration, transcript)
				}
				cancelDelivery()
			}
		})
	}
}

func TestAgentUIWorkspaceFooterAtMinimumWidthKeepsModeAndSanitizedWorkspace(t *testing.T) {
	conversation := &fakeConversation{workspaceStatus: agentloop.WorkspaceStatus{
		Available: true, Label: "/home/private/project-with-a-very-long-workspace-label",
	}}
	value := newModel(t.Context(), conversation, "a-very-long-model-name")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: minimumWidth, Height: minimumHeight})
	value = updated.(model)
	for _, mode := range []agentloop.FileAuthorizationMode{agentloop.FileAuthorizationConfirm, agentloop.FileAuthorizationYOLO} {
		conversation.fileMode = mode
		footer := value.renderFooterStatus(value.viewport.Width)
		wantMode := "文件 确认"
		if mode == agentloop.FileAuthorizationYOLO {
			wantMode = "文件 YOLO"
		}
		if lipgloss.Width(footer) > value.viewport.Width || !strings.Contains(footer, wantMode) || !strings.Contains(footer, "工作区") || strings.Contains(footer, "/home/") {
			t.Fatalf("mode=%q width=%d limit=%d footer=%q", mode, lipgloss.Width(footer), value.viewport.Width, footer)
		}
	}
	for _, line := range strings.Split(value.View(), "\n") {
		if lipgloss.Width(line) > minimumWidth || strings.Contains(line, "/home/private") {
			t.Fatalf("minimum-width line width=%d line=%q", lipgloss.Width(line), line)
		}
	}
}
