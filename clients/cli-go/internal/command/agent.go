package command

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontroller"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentui"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/credentials"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelsecret"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type defaultAgentUIRunner struct {
	in  io.Reader
	out io.Writer
}

func (r defaultAgentUIRunner) Run(ctx context.Context, conversation agentui.Conversation, modelName string) error {
	return agentui.Runner{In: r.in, Out: r.out, Session: conversation, ModelName: modelName}.Run(ctx)
}

func (r defaultAgentUIRunner) Pick(ctx context.Context, source agentui.SessionPickerSource, all bool) (agentui.PickerChoice, error) {
	return agentui.RunSessionPicker(ctx, r.in, r.out, source, all)
}

func (a *App) runAgent(ctx context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(a.Out, agentHelpText+"\n")
		return err
	}
	if len(args) > 0 && args[0] == "resume" {
		return a.runAgentResume(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "sessions" {
		return a.runAgentSessions(ctx, args[1:])
	}
	return a.runNewAgent(ctx, args)
}

func (a *App) runNewAgent(ctx context.Context, args []string) error {
	set := newFlagSet("agent")
	var workspacePath string
	var noSave bool
	set.StringVar(&workspacePath, "workspace", "", "fixed workspace root")
	set.BoolVar(&noSave, "no-save", false, "disable persistence for this new session")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "AI学习助手参数格式无效", "在交互终端运行 edu-agent agent [--workspace PATH] [--no-save]", ExitInput)
	}
	workspaceProvided := false
	set.Visit(func(current *flag.Flag) {
		if current.Name == "workspace" {
			workspaceProvided = true
		}
	})
	var workspacePathErr error
	if !workspaceProvided {
		workspacePath, workspacePathErr = os.Getwd()
	}
	if !a.interactiveTerminalAvailable() || a.AgentUI == nil {
		return commandError("not_a_terminal", "AI学习助手需要交互终端", "请在TTY终端运行 edu-agent agent", ExitInput)
	}
	value, record, model, err := a.agentDependencies()
	if err != nil {
		return err
	}
	requestTimeout, modelTimeout, err := agentTimeouts(value)
	if err != nil {
		return err
	}
	server := a.NewClient(value.ServerURL, record.Token, requestTimeout)
	workspaceStatus := workspace.Status{Code: workspace.CodeWorkspaceUnavailable}
	var workspaceExecutor *workspace.Workspace
	if workspacePathErr == nil {
		workspaceExecutor, err = workspace.Open(workspacePath)
		if err == nil {
			workspaceStatus = workspaceExecutor.Status()
		}
	}
	effectiveNoSave := noSave || value.Agent.SessionHistory == "off"
	var store *agentsession.Store
	if !effectiveNoSave {
		store, err = a.openAgentSessionStore(ctx, value.ServerURL)
		if err != nil {
			_, _ = fmt.Fprintln(a.Err, "警告[session_store_unavailable]：加密会话存储不可用；当前会话将以不保存模式运行。")
			store = nil
		}
	}
	controller, err := agentcontroller.Start(ctx, agentcontroller.Dependencies{
		Store: store, Model: model, Server: server,
		Provider:      agentcontroller.Provider{Name: value.Agent.Provider, Endpoint: value.Agent.BaseURL, Model: value.Agent.Model},
		WorkspaceRoot: workspacePath,
		LoopOptions: agentloop.Options{
			ContextWindow: value.Agent.ContextWindow, MaxToolRounds: value.Agent.MaxToolRounds,
			ContextCompaction: value.Agent.ContextCompaction,
			ReasoningEffort:   modelclient.ReasoningEffort(value.Agent.ReasoningEffort),
			ModelTimeout:      modelTimeout, ToolTimeout: requestTimeout, NewUUID: a.NewUUID,
			Workspace: workspaceExecutor, WorkspaceStatus: workspaceStatus,
		},
	}, effectiveNoSave)
	if err != nil {
		if workspaceExecutor != nil {
			_ = workspaceExecutor.Close()
		}
		return sessionCommandError(err)
	}
	status := controller.Status()
	if status.Persistent {
		_, _ = fmt.Fprintf(a.Err, "会话已启用自动加密保存：%s。自动标题会向当前模型提供商发送有界的对话摘要。\n", safeText(controller.SessionID()))
	} else if noSave {
		_, _ = fmt.Fprintln(a.Err, "当前新会话为不保存模式（--no-save），退出后不可恢复。")
	} else if value.Agent.SessionHistory == "off" {
		_, _ = fmt.Fprintln(a.Err, "当前新会话不保存：设置中的Agent Session历史为off；退出后不可恢复。")
	} else if status.DegradedReason != "" {
		_, _ = fmt.Fprintln(a.Err, safeText(status.DegradedReason))
	}
	return a.runAgentController(ctx, controller, value.Agent.Model)
}

func (a *App) runAgentResume(ctx context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(a.Out, agentHelpText+"\n")
		return err
	}
	target, last, all, workspacePath, err := parseAgentResumeArgs(args)
	if err != nil {
		return commandError("usage", "Agent Session恢复参数格式无效", "运行 edu-agent agent resume [SESSION] [--all]，或 edu-agent agent resume --last", ExitInput)
	}
	if !a.interactiveTerminalAvailable() || a.AgentUI == nil {
		return commandError("not_a_terminal", "Agent Session恢复需要交互终端", "请在TTY终端运行 edu-agent agent resume", ExitInput)
	}
	if workspacePath == "" {
		workspacePath, err = os.Getwd()
		if err != nil {
			return commandError("workspace_unavailable", "当前工作区不可用", "切换到可用工作区后重试，或使用 --all 搜索全部工作区", ExitUnavailable)
		}
	}
	value, record, model, err := a.agentDependencies()
	if err != nil {
		return err
	}
	requestTimeout, modelTimeout, err := agentTimeouts(value)
	if err != nil {
		return err
	}
	store, err := a.openAgentSessionStore(ctx, value.ServerURL)
	if err != nil {
		return sessionCommandError(err)
	}
	summaries, err := store.List(ctx)
	if err != nil {
		_ = store.Close()
		return sessionCommandError(err)
	}
	workspaceBinding, _ := agentcontroller.BindWorkspace(workspacePath)
	workspaceID := agentcontroller.WorkspaceScopeID(workspaceBinding)
	provider := agentcontroller.Provider{Name: value.Agent.Provider, Endpoint: value.Agent.BaseURL, Model: value.Agent.Model}
	var selected agentsession.Summary
	var pickerChoice agentui.PickerChoice
	if target == "" && !last {
		if picker, ok := a.AgentUI.(AgentSessionPickerRunner); ok {
			selector := agentcontroller.NewSelector(store, workspaceID, provider)
			pickerChoice, err = picker.Pick(ctx, selector, all)
			if err != nil {
				_ = store.Close()
				return commandError("terminal_error", "Agent Session选择器无法运行", "检查终端能力后重试", ExitInternal)
			}
			if pickerChoice.Cancelled {
				_ = store.Close()
				return nil
			}
			if pickerChoice.New {
				_ = store.Close()
				return a.runNewAgent(ctx, nil)
			}
			summaries, err = store.List(ctx)
			if err == nil {
				selected, err = selectSummary(summaries, pickerChoice.SessionID, true, workspaceID)
			}
		} else {
			selected, err = a.selectAgentSession(summaries, target, last, all, workspaceID)
		}
	} else {
		selected, err = a.selectAgentSession(summaries, target, last, all, workspaceID)
	}
	if err != nil {
		_ = store.Close()
		return err
	}
	server := a.NewClient(value.ServerURL, record.Token, requestTimeout)
	controller, err := agentcontroller.Resume(ctx, agentcontroller.Dependencies{
		Store: store, Model: model, Server: server,
		Provider: provider,
		LoopOptions: agentloop.Options{
			ContextWindow: value.Agent.ContextWindow, MaxToolRounds: value.Agent.MaxToolRounds,
			ContextCompaction: value.Agent.ContextCompaction,
			ReasoningEffort:   modelclient.ReasoningEffort(value.Agent.ReasoningEffort),
			ModelTimeout:      modelTimeout, ToolTimeout: requestTimeout, NewUUID: a.NewUUID,
		},
	}, agentcontroller.ResumeOptions{
		SessionID: selected.SessionID, CurrentWorkspace: workspacePath,
		ConfirmWorkspace: func(binding agentcontroller.WorkspaceBinding) (bool, error) {
			if pickerChoice.Workspace {
				return true, nil
			}
			return a.Terminal.Confirm("此会话属于工作区 " + safeText(binding.Label) + "。确认恢复该工作区？")
		},
	})
	if err != nil {
		_ = store.Close()
		return sessionCommandError(err)
	}
	status := controller.Status()
	for _, notice := range status.Notices {
		_, _ = fmt.Fprintln(a.Err, safeText("提示："+notice))
	}
	if status.ProviderConfirmationRequired {
		confirmed := pickerChoice.Provider
		var confirmErr error
		if !confirmed {
			confirmed, confirmErr = a.Terminal.Confirm("模型提供商端点已变更。确认向当前端点发送历史会话文本？")
		}
		if confirmErr != nil {
			_ = controller.Shutdown(context.Background())
			return commandError("terminal_error", "无法读取提供商确认", "重试恢复命令", ExitInternal)
		}
		if confirmed {
			if err := controller.ConfirmProvider(ctx); err != nil {
				_ = controller.Shutdown(context.Background())
				return sessionCommandError(err)
			}
		}
	}
	return a.runAgentController(ctx, controller, value.Agent.Model)
}

func (a *App) runAgentController(ctx context.Context, controller *agentcontroller.Controller, modelName string) error {
	uiErr := a.runAgentUI(ctx, controller, modelName)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	shutdownErr := controller.Shutdown(shutdownCtx)
	cancel()
	if shutdownErr != nil {
		return sessionCommandError(shutdownErr)
	}
	return uiErr
}

func (a *App) runAgentSessions(ctx context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(a.Out, agentHelpText+"\n")
		return err
	}
	if len(args) == 0 {
		return commandError("usage", "sessions需要delete或clear", "运行 edu-agent agent sessions delete SESSION|storage:<storage-id> --confirmed，或 sessions clear --confirmed", ExitInput)
	}
	action, target := args[0], ""
	switch action {
	case "delete":
		if len(args) != 3 || args[2] != "--confirmed" || strings.TrimSpace(args[1]) == "" {
			return commandError("confirmation_required", "删除Agent Session需要明确确认", "运行 edu-agent agent sessions delete SESSION|storage:<storage-id> --confirmed", ExitInput)
		}
		target = args[1]
	case "clear":
		if len(args) != 2 || args[1] != "--confirmed" {
			return commandError("confirmation_required", "清空Agent Session需要明确确认", "运行 edu-agent agent sessions clear --confirmed", ExitInput)
		}
	default:
		return commandError("usage", "未知sessions命令", "运行 edu-agent agent sessions delete 或 clear", ExitInput)
	}
	value, err := a.loadModelConfig()
	if err != nil {
		return err
	}
	if strings.TrimSpace(value.ServerURL) == "" {
		return commandError("not_paired", "没有可用于定位Agent Session的服务端配置", "先完成设备配对", ExitAuth)
	}
	store, err := a.openAgentSessionStore(ctx, value.ServerURL)
	if err != nil {
		return sessionCommandError(err)
	}
	defer store.Close()
	if action == "delete" {
		summaries, listErr := store.List(ctx)
		if listErr != nil {
			return sessionCommandError(listErr)
		}
		selected, selectErr := selectDeleteSummary(summaries, target)
		if selectErr != nil {
			return selectErr
		}
		deleteTarget := agentsession.DeleteTarget{
			SessionID: selected.SessionID, StorageID: selected.StorageID,
			ExpectedRecordRevision: selected.RecordRevision,
		}
		if deleteErr := store.Delete(ctx, deleteTarget); deleteErr != nil {
			return sessionCommandError(deleteErr)
		}
		label := selected.SessionID
		if label == "" {
			label = "storage:" + selected.StorageID
		}
		_, err = fmt.Fprintf(a.Out, "已删除Agent Session：%s\n", safeText(label))
		return err
	}
	if clearErr := store.Clear(ctx); clearErr != nil {
		return sessionCommandError(clearErr)
	}
	_, err = fmt.Fprintln(a.Out, "已通过轮换加密密钥清空当前配置档案的全部Agent Session。")
	return err
}

func (a *App) runAgentUI(ctx context.Context, conversation agentui.Conversation, modelName string) error {
	if err := a.AgentUI.Run(ctx, conversation, modelName); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return commandError("terminal_error", "AI学习助手界面无法运行", "检查终端能力后重试", ExitInternal)
	}
	return nil
}

func agentTimeouts(value config.Config) (requestTimeout, modelTimeout time.Duration, err error) {
	requestTimeout, err = config.ParseTimeout(value.Timeout)
	if err != nil {
		return 0, 0, commandError("invalid_configuration", "客户端请求超时配置无效", "在设置中修复客户端配置", ExitInput)
	}
	modelTimeout, err = config.ParseTimeout(value.Agent.Timeout)
	if err != nil {
		return 0, 0, commandError("invalid_configuration", "模型请求超时配置无效", "在设置中修复模型配置", ExitInput)
	}
	return requestTimeout, modelTimeout, nil
}

func (a *App) openAgentSessionStore(ctx context.Context, serverURL string) (*agentsession.Store, error) {
	rootSource := a.AgentSessionRoot
	if rootSource == nil {
		return nil, agentsession.ErrKeyUnavailable
	}
	root, err := rootSource()
	if err != nil {
		return nil, err
	}
	profile, err := agentsession.ProfileFingerprint(serverURL)
	if err != nil {
		return nil, err
	}
	return agentsession.Open(ctx, agentsession.Options{Root: filepath.Join(root, profile), ProfileFingerprint: profile, Secrets: a.AgentSessionSecrets})
}

func parseAgentResumeArgs(args []string) (target string, last, all bool, workspacePath string, err error) {
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--last":
			if last {
				return "", false, false, "", errors.New("duplicate last")
			}
			last = true
		case "--all":
			if all {
				return "", false, false, "", errors.New("duplicate all")
			}
			all = true
		default:
			if strings.HasPrefix(args[index], "-") || target != "" {
				return "", false, false, "", errors.New("invalid target")
			}
			target = args[index]
		}
	}
	if target != "" && last {
		return "", false, false, "", errors.New("mutually exclusive selectors")
	}
	if last && all {
		return "", false, false, "", errors.New("last cannot be combined with all")
	}
	return target, last, all, workspacePath, nil
}

func (a *App) selectAgentSession(summaries []agentsession.Summary, target string, last, all bool, workspaceID string) (agentsession.Summary, error) {
	if target != "" {
		return selectSummary(summaries, target, all, workspaceID)
	}
	filtered := sortAgentSessionSummaries(filterSummaries(summaries, all, workspaceID))
	if last {
		recoverable := make([]agentsession.Summary, 0, len(filtered))
		for _, summary := range filtered {
			if summary.Corrupt || summary.Locked || summary.Unavailable {
				continue
			}
			recoverable = append(recoverable, summary)
		}
		if len(recoverable) == 0 {
			if len(filtered) == 0 {
				return agentsession.Summary{}, commandError("session_not_found", "当前工作区没有可恢复的Agent Session", "运行 edu-agent agent resume --all 打开全部工作区的选择器，或启动新会话", ExitInput)
			}
			return agentsession.Summary{}, commandError("session_not_found", "当前工作区的Agent Session均已损坏、被占用或不可用", "修复或删除问题Session，或运行 edu-agent agent resume 打开选择器", ExitInput)
		}
		return recoverable[0], nil
	}
	if len(filtered) == 0 {
		return agentsession.Summary{}, commandError("session_not_found", "没有符合当前工作区范围的Agent Session", "使用 --all 查看其他工作区，或启动新会话", ExitInput)
	}
	for index, summary := range filtered {
		workspacePart := ""
		if all {
			workspacePart = "  " + safeText(summary.WorkspaceLabel)
		}
		_, _ = fmt.Fprintf(a.Out, "%d. %s  %s%s  %s\n", index+1, safeText(summary.Title), safeText(shortSessionID(summary.SessionID)), workspacePart, safeText(summary.UpdatedAt.Local().Format("2006-01-02 15:04")))
	}
	answer, err := a.Terminal.ReadLine("选择要恢复的Session编号：")
	if err != nil {
		return agentsession.Summary{}, commandError("terminal_error", "无法读取Session选择", "重试恢复命令", ExitInternal)
	}
	selected, parseErr := strconv.Atoi(strings.TrimSpace(answer))
	if parseErr != nil || selected < 1 || selected > len(filtered) {
		return agentsession.Summary{}, commandError("session_selection_invalid", "Session选择无效", "输入列表中的编号", ExitInput)
	}
	return filtered[selected-1], nil
}

func selectDeleteSummary(summaries []agentsession.Summary, target string) (agentsession.Summary, error) {
	if strings.HasPrefix(strings.ToLower(target), "storage:") {
		storageID, ok := canonicalStorageSelector(target)
		if !ok {
			return agentsession.Summary{}, commandError("session_selection_invalid", "损坏Session定位符无效", "使用storage:加32位小写十六进制Storage ID", ExitInput)
		}
		for _, summary := range summaries {
			if summary.StorageID != storageID {
				continue
			}
			if !summary.Corrupt || summary.VersionUnsupported {
				return agentsession.Summary{}, commandError("session_selection_invalid", "storage定位符只能删除明确损坏的Session", "健康或未来版本Session请使用canonical Session UUID", ExitInput)
			}
			return summary, nil
		}
		return agentsession.Summary{}, commandError("session_not_found", "未找到指定Agent Session", "刷新列表并核对storage定位符", ExitInput)
	}
	return selectSummary(summaries, target, true, "")
}

func canonicalStorageSelector(value string) (string, bool) {
	if len(value) != len("storage:")+32 || value != strings.ToLower(value) || !strings.HasPrefix(value, "storage:") {
		return "", false
	}
	storageID := strings.TrimPrefix(value, "storage:")
	for _, current := range storageID {
		if current < '0' || current > '9' && current < 'a' || current > 'f' {
			return "", false
		}
	}
	return storageID, true
}

func selectSummary(summaries []agentsession.Summary, target string, all bool, workspaceID string) (agentsession.Summary, error) {
	filtered := filterSummaries(summaries, all, workspaceID)
	if canonicalSessionUUID(target) {
		for _, summary := range filtered {
			if summary.SessionID == target {
				return summary, nil
			}
		}
		return agentsession.Summary{}, commandError("session_not_found", "未找到指定Agent Session", "检查UUID和工作区范围，或使用 --all", ExitInput)
	}
	fold := cases.Fold()
	wanted := fold.String(norm.NFC.String(strings.TrimSpace(target)))
	matches := make([]agentsession.Summary, 0, 2)
	for _, summary := range filtered {
		if fold.String(norm.NFC.String(strings.TrimSpace(summary.Title))) == wanted {
			matches = append(matches, summary)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return agentsession.Summary{}, commandError("session_name_ambiguous", "多个Agent Session使用同一标题", "使用canonical Session UUID重试", ExitConflict)
	}
	return agentsession.Summary{}, commandError("session_not_found", "未找到指定Agent Session", "检查标题和工作区范围，或使用 --all", ExitInput)
}

func filterSummaries(summaries []agentsession.Summary, all bool, workspaceID string) []agentsession.Summary {
	result := make([]agentsession.Summary, 0, len(summaries))
	for _, summary := range summaries {
		if all || workspaceID != "" && summary.WorkspaceID == workspaceID {
			result = append(result, summary)
		}
	}
	return result
}

func sortAgentSessionSummaries(summaries []agentsession.Summary) []agentsession.Summary {
	result := append([]agentsession.Summary(nil), summaries...)
	sort.SliceStable(result, func(left, right int) bool {
		if result[left].UpdatedAt.Equal(result[right].UpdatedAt) {
			return result[left].SessionID < result[right].SessionID
		}
		return result[left].UpdatedAt.After(result[right].UpdatedAt)
	})
	return result
}

func canonicalSessionUUID(value string) bool {
	if len(value) != 36 || strings.ToLower(value) != value || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return false
	}
	for index, current := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if current < '0' || current > '9' && current < 'a' || current > 'f' {
			return false
		}
	}
	return value[19] == '8' || value[19] == '9' || value[19] == 'a' || value[19] == 'b'
}

func shortSessionID(value string) string {
	if len(value) < 8 {
		return value
	}
	return value[:8]
}

func sessionCommandErrorForCode(code string) *Error {
	switch code {
	case "session_store_unavailable":
		return commandError("session_store_unavailable", "Agent Session加密密钥服务不可用", "修复系统钥匙串后重试；新会话可使用 --no-save", ExitUnavailable)
	case "session_store_full":
		return commandError("session_store_full", "Agent Session加密存储已达到硬上限", "手动删除不再需要的Session后重试", ExitUnavailable)
	case "session_save_failed":
		return commandError("session_save_failed", "Agent Session稳定检查点未能安全发布", "保留当前进程并修复本地存储后重试", ExitUnavailable)
	case "session_not_found":
		return commandError("session_not_found", "Agent Session不存在", "刷新Session列表后重试", ExitInput)
	case "session_name_ambiguous":
		return commandError("session_name_ambiguous", "多个Agent Session使用同一标题", "使用canonical Session UUID重试", ExitConflict)
	case "session_in_use":
		return commandError("session_in_use", "Agent Session正在另一个进程中使用", "关闭其他进程中的Session后重试", ExitConflict)
	case "session_corrupt":
		return commandError("session_corrupt", "Agent Session无法安全解密或验证", "删除损坏Session或清空当前Session档案", ExitConflict)
	case "session_version_unsupported":
		return commandError("session_version_unsupported", "Agent Session版本与当前客户端不兼容", "升级客户端后重试；当前版本不会恢复、修改或删除该Session", ExitConflict)
	case "session_checkpoint_conflict":
		return commandError("session_checkpoint_conflict", "Agent Session已被其他写入更新", "刷新或重新恢复该Session", ExitConflict)
	case "session_interrupted":
		return commandError("session_interrupted", "Agent Session上次运行在稳定检查点之后中断", "继续前检查上次未完成操作；客户端不会自动重放", ExitConflict)
	case "session_workspace_unavailable":
		return commandError("session_workspace_unavailable", "历史工作区无法安全恢复；文件工具已禁用", "修复原工作区后重试，或启动新会话", ExitUnavailable)
	case "session_provider_confirmation_required":
		return commandError("session_provider_confirmation_required", "模型提供商端点变更尚未确认", "确认当前端点后再发送历史文本", ExitConflict)
	case "session_privacy_revalidation_pending":
		return commandError("session_privacy_revalidation_pending", "Agent Session历史隐私代际尚未核对", "等待服务端隐私代际核对后重试", ExitUnavailable)
	case "session_delete_failed":
		return commandError("session_delete_failed", "Agent Session密钥或密文未能完整清理；该Session仍按未删除处理", "关闭其他进程并重试相同删除或清空操作；不要假定任何数据已删除", ExitUnavailable)
	case "session_title_failed":
		return commandError("session_title_failed", "Agent Session自动标题生成失败；已保留旧标题或本地摘要", "继续使用当前标题或本地摘要，稍后重试标题更新", ExitUnavailable)
	case "session_privacy_invalidated":
		return commandError("session_privacy_invalidated", "Agent Session已被隐私清除失效", "刷新Session列表或启动新会话", ExitConflict)
	case "workspace_confirmation_required":
		return commandError("workspace_confirmation_required", "跨工作区恢复未获确认", "确认历史工作区后重试", ExitConflict)
	default:
		return nil
	}
}

func sessionCommandError(err error) error {
	var commandErr *Error
	if errors.As(err, &commandErr) {
		if mapped := sessionCommandErrorForCode(commandErr.Code); mapped != nil {
			return mapped
		}
	}
	switch agentsession.StableErrorCode(err) {
	case agentsession.ErrorCodeVersionUnsupported:
		return sessionCommandErrorForCode("session_version_unsupported")
	case agentsession.ErrorCodeCorrupt:
		return sessionCommandErrorForCode("session_corrupt")
	}
	switch {
	case errors.Is(err, agentloop.ErrCheckpointVersionUnsupported):
		return sessionCommandErrorForCode("session_version_unsupported")
	case errors.Is(err, agentloop.ErrCheckpointCorrupt):
		return sessionCommandErrorForCode("session_corrupt")
	case errors.Is(err, agentsession.ErrKeyUnavailable):
		return sessionCommandErrorForCode("session_store_unavailable")
	case errors.Is(err, agentsession.ErrNotFound):
		return sessionCommandErrorForCode("session_not_found")
	case errors.Is(err, agentsession.ErrInUse):
		return sessionCommandErrorForCode("session_in_use")
	case errors.Is(err, agentsession.ErrCheckpointSaveFailed), errors.Is(err, agentsession.ErrOutcomeUnknown):
		return sessionCommandErrorForCode("session_save_failed")
	case errors.Is(err, agentsession.ErrCheckpointConflict):
		return sessionCommandErrorForCode("session_checkpoint_conflict")
	case errors.Is(err, agentsession.ErrDeleteFailed):
		return sessionCommandErrorForCode("session_delete_failed")
	case errors.Is(err, agentsession.ErrStoreFull):
		return sessionCommandErrorForCode("session_store_full")
	case errors.Is(err, agentsession.ErrPrivacyInvalidated):
		return sessionCommandErrorForCode("session_privacy_invalidated")
	case errors.Is(err, agentcontroller.ErrWorkspaceConfirmationRequired):
		return sessionCommandErrorForCode("workspace_confirmation_required")
	case errors.Is(err, agentcontroller.ErrProviderConfirmationRequired):
		return sessionCommandErrorForCode("session_provider_confirmation_required")
	default:
		return commandError("session_store_unavailable", "Agent Session操作无法完成", "检查本地加密存储和系统钥匙串后重试", ExitUnavailable)
	}
}

const agentHelpText = `用法：
  edu-agent agent [--workspace PATH] [--no-save]
  edu-agent agent resume [SESSION] [--all]
  edu-agent agent resume --last
  edu-agent agent sessions delete SESSION|storage:<storage-id> --confirmed
  edu-agent agent sessions clear --confirmed

启动、恢复或管理自动加密保存的 AI 学习助手会话。无 SESSION 的 resume 打开与 TUI F2 相同的选择器；Session 不按时间自动删除，达到上限也不自动淘汰。

选项：
  --workspace PATH  固定本 Session 的本地工作区；省略时使用 Agent 启动目录
  --no-save         仅当前新 Session 不保存；不会改变默认设置
  --last            恢复当前工作区范围内最近更新且可恢复的 Session；不能与 SESSION 或 --all 合用
  --all             关闭当前工作区过滤；仅用于打开 picker 或配合显式 SESSION
  -h, --help        显示此帮助

Session picker：空闲时 F2 打开；Tab 切换当前/全部工作区，支持搜索、恢复、重命名、二次确认删除和新建。恢复或切换会重置 YOLO、旧文件授权和未完成交互。自动标题会向当前 provider 发送有界安全对话片段；恢复后的模型请求会发送历史上下文，provider 端点变化时先确认。旧工作区不可用时只恢复对话并禁用文件工具，不回退到当前目录。系统钥匙串不可用时不写明文，只明确降级为未保存。clear 只清除本地 Session store，不清除服务端、终端、Shell、provider 或 OS 备份中的副本。

文件工具：list、read、search、write、edit。write/edit 默认逐次确认；在 TUI 中按 F4 可切换仅当前 Session 生效的 YOLO 模式。`

func (a *App) runModel(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return modelUsage("模型配置需要 show、preset、set、test 或 key")
	}
	switch args[0] {
	case "show":
		if len(args) != 1 {
			return modelUsage("model show 不接受额外参数")
		}
		value, err := a.loadModelConfig()
		if err != nil {
			return err
		}
		if value.Agent == nil {
			_, err = fmt.Fprintln(a.Out, "AI模型：未配置")
			return err
		}
		keyStatus := "未配置"
		binding := modelsecret.Binding(value.Agent.Provider, value.Agent.BaseURL)
		if _, keyErr := a.ModelSecrets.Load(binding); keyErr == nil {
			keyStatus = "已存入系统钥匙串"
		}
		_, err = fmt.Fprintf(a.Out, "提供商：%s\nBase URL：%s\n模型：%s\n上下文窗口：%d\n上下文压缩：%s\n默认推理强度：%s\n会话历史：%s\n请求超时：%s\n最大工具轮数：%d\nAPI Key：%s\n",
			safeText(value.Agent.Provider), safeText(value.Agent.BaseURL), safeText(value.Agent.Model), value.Agent.ContextWindow,
			safeText(value.Agent.ContextCompaction), safeText(value.Agent.ReasoningEffort), safeText(value.Agent.SessionHistory), safeText(value.Agent.Timeout), value.Agent.MaxToolRounds, keyStatus)
		return err
	case "preset":
		if len(args) != 2 {
			return modelUsage("model preset 需要一个提供商")
		}
		value, err := a.loadModelConfig()
		if err != nil {
			return err
		}
		preset := config.DefaultAgentConfig(args[1])
		if value.Agent != nil && value.Agent.SessionHistory != "" {
			preset.SessionHistory = value.Agent.SessionHistory
		}
		if !strings.EqualFold(strings.TrimSpace(args[1]), preset.Provider) {
			return modelUsage("未知模型提供商")
		}
		value.Agent = &preset
		if err := a.Config.Save(value); err != nil {
			return commandError("configuration_write_failed", "AI模型预设无法保存", "检查配置目录权限后重试", ExitInternal)
		}
		_, err = fmt.Fprintf(a.Out, "已选择%s预设。请核对模型名称并配置API Key。\n", providerName(preset.Provider))
		return err
	case "set":
		return a.runModelSet(args[1:])
	case "test":
		if len(args) != 1 {
			return modelUsage("model test 不接受额外参数")
		}
		value, model, err := a.modelDependencies()
		if err != nil {
			return err
		}
		response, err := model.Complete(ctx, modelclient.Request{
			Messages: []modelclient.Message{
				{Role: "system", Content: "你正在执行连接检查。"},
				{Role: "user", Content: "仅回复：连接正常"},
			},
			ReasoningEffort: modelclient.ReasoningEffort(value.Agent.ReasoningEffort),
		})
		if err != nil {
			if modelclient.StableErrorCode(err) == modelclient.ErrorCodeReasoningEffortUnsupported {
				return commandError(string(modelclient.ErrorCodeReasoningEffortUnsupported), "模型不支持当前推理强度", "将--reasoning-effort改为auto或none，或选择兼容模型", ExitUnavailable)
			}
			return commandError("model_unavailable", "模型连接测试失败", "检查Base URL、模型名称、API Key和网络", ExitUnavailable)
		}
		if strings.TrimSpace(response.Message.Content) == "" {
			return commandError("model_protocol_error", "模型没有返回文本", "检查OpenAI兼容协议实现", ExitUnavailable)
		}
		_, err = fmt.Fprintf(a.Out, "模型连接正常：%s / %s\n", providerName(value.Agent.Provider), safeText(value.Agent.Model))
		return err
	case "key":
		if len(args) != 3 || args[1] != "delete" || args[2] != "--confirmed" {
			return modelUsage("请从TUI确认删除API Key")
		}
		value, err := a.loadModelConfig()
		if err != nil {
			return err
		}
		if value.Agent == nil {
			return commandError("agent_not_configured", "AI模型尚未配置", "在设置中选择提供商后重试", ExitInput)
		}
		binding := modelsecret.Binding(value.Agent.Provider, value.Agent.BaseURL)
		if err := a.ModelSecrets.Delete(binding); err != nil {
			return commandError("credential_delete_failed", "API Key无法从系统钥匙串删除", "检查系统钥匙串后重试", ExitInternal)
		}
		_, err = fmt.Fprintln(a.Out, "API Key已从系统钥匙串删除。")
		return err
	default:
		return modelUsage("未知模型配置命令")
	}
}

func (a *App) runModelSet(args []string) error {
	set := newFlagSet("model set")
	var provider, baseURL, modelName, timeout, contextCompaction, reasoningEffort, sessionHistory string
	var contextWindow, maxToolRounds int
	set.StringVar(&provider, "provider", "", "model provider")
	set.StringVar(&baseURL, "base-url", "", "OpenAI-compatible base URL")
	set.StringVar(&modelName, "model", "", "model name")
	set.IntVar(&contextWindow, "context-window", 0, "context window")
	set.StringVar(&contextCompaction, "context-compaction", "", "auto, recent-only, or off")
	set.StringVar(&reasoningEffort, "reasoning-effort", "", "auto, none, minimal, low, medium, high, xhigh, or max")
	set.StringVar(&sessionHistory, "session-history", "", "auto or off")
	set.StringVar(&timeout, "timeout", "", "model timeout")
	set.IntVar(&maxToolRounds, "max-tool-rounds", 0, fmt.Sprintf("maximum tool rounds (%d-%d)", config.MinAgentMaxToolRounds, config.MaxAgentMaxToolRounds))
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return modelUsage("模型参数格式无效")
	}
	maxToolRoundsSet := false
	set.Visit(func(current *flag.Flag) {
		if current.Name == "max-tool-rounds" {
			maxToolRoundsSet = true
		}
	})
	if maxToolRoundsSet && (maxToolRounds < config.MinAgentMaxToolRounds || maxToolRounds > config.MaxAgentMaxToolRounds) {
		return commandError("invalid_configuration", "最大工具轮数无效", fmt.Sprintf("请输入%d到%d之间的整数", config.MinAgentMaxToolRounds, config.MaxAgentMaxToolRounds), ExitInput)
	}
	value, err := a.loadModelConfig()
	if err != nil {
		return err
	}
	if value.Agent == nil {
		preset := config.DefaultAgentConfig(config.DefaultAgentProvider)
		value.Agent = &preset
	}
	candidate := *value.Agent
	if strings.TrimSpace(provider) != "" {
		preservedHistory := candidate.SessionHistory
		candidate = config.DefaultAgentConfig(provider)
		if preservedHistory != "" {
			candidate.SessionHistory = preservedHistory
		}
		if !strings.EqualFold(strings.TrimSpace(provider), candidate.Provider) {
			return modelUsage("未知模型提供商")
		}
	}
	if strings.TrimSpace(baseURL) != "" {
		candidate.BaseURL = strings.TrimSpace(baseURL)
	}
	if strings.TrimSpace(modelName) != "" {
		candidate.Model = strings.TrimSpace(modelName)
	}
	if contextWindow != 0 {
		candidate.ContextWindow = contextWindow
	}
	if strings.TrimSpace(contextCompaction) != "" {
		candidate.ContextCompaction = strings.TrimSpace(contextCompaction)
	}
	if strings.TrimSpace(reasoningEffort) != "" {
		candidate.ReasoningEffort = strings.TrimSpace(reasoningEffort)
	}
	if strings.TrimSpace(sessionHistory) != "" {
		candidate.SessionHistory = strings.TrimSpace(sessionHistory)
	}
	if strings.TrimSpace(timeout) != "" {
		candidate.Timeout = strings.TrimSpace(timeout)
	}
	if maxToolRoundsSet {
		candidate.MaxToolRounds = maxToolRounds
	}
	if err := candidate.Validate(); err != nil {
		return commandError("invalid_configuration", "AI模型参数无效", "检查地址、模型、上下文窗口、压缩模式、推理强度、会话历史、超时和工具轮数", ExitInput)
	}
	value.Agent = &candidate
	if err := a.Config.Save(value); err != nil {
		return commandError("configuration_write_failed", "AI模型配置无法保存", "检查配置目录权限后重试", ExitInternal)
	}
	_, err = fmt.Fprintln(a.Out, "AI模型配置已更新。API Key仍由系统钥匙串独立管理。")
	return err
}

func (a *App) saveDashboardAgentKey(value string) error {
	if strings.TrimSpace(value) == "" {
		return modelUsage("API Key不能为空")
	}
	configValue, err := a.loadModelConfig()
	if err != nil {
		return err
	}
	if configValue.Agent == nil {
		return commandError("agent_not_configured", "请先选择模型提供商", "在设置中选择提供商预设", ExitInput)
	}
	binding := modelsecret.Binding(configValue.Agent.Provider, configValue.Agent.BaseURL)
	if err := a.ModelSecrets.Save(binding, value); err != nil {
		return commandError("credential_write_failed", "API Key无法写入系统钥匙串", "确认系统钥匙串可用后重试", ExitInternal)
	}
	_, err = fmt.Fprintln(a.Out, "API Key已安全保存到系统钥匙串。")
	return err
}

func (a *App) modelDependencies() (config.Config, agentloop.Model, error) {
	value, err := a.loadModelConfig()
	if err != nil {
		return config.Config{}, nil, err
	}
	if value.Agent == nil {
		return config.Config{}, nil, commandError("agent_not_configured", "AI模型尚未配置", "在设置中选择提供商并填写模型参数", ExitInput)
	}
	apiKey := ""
	if !value.Agent.APIKeyOptional() {
		binding := modelsecret.Binding(value.Agent.Provider, value.Agent.BaseURL)
		apiKey, err = a.ModelSecrets.Load(binding)
		if err != nil {
			return config.Config{}, nil, commandError("model_key_unavailable", "模型API Key不可用", "在设置中将当前端点的API Key保存到系统钥匙串", ExitAuth)
		}
	} else if value.Agent.Provider == "custom" {
		binding := modelsecret.Binding(value.Agent.Provider, value.Agent.BaseURL)
		apiKey, err = a.ModelSecrets.Load(binding)
		if err != nil {
			apiKey = ""
		}
	}
	model, err := a.NewModel(*value.Agent, apiKey)
	if err != nil {
		return config.Config{}, nil, commandError("agent_configuration_invalid", "AI模型配置无法使用", "检查模型设置后重试", ExitInput)
	}
	return value, model, nil
}

func (a *App) agentDependencies() (config.Config, credentials.Record, agentloop.Model, error) {
	value, err := a.loadMutableClientConfig()
	if err != nil {
		return config.Config{}, credentials.Record{}, nil, err
	}
	record, err := a.Credentials.Load()
	if err != nil {
		return config.Config{}, credentials.Record{}, nil, commandError("authentication_failed", "设备凭据不可用", "重新配对设备", ExitAuth)
	}
	_, model, err := a.modelDependencies()
	if err != nil {
		return config.Config{}, credentials.Record{}, nil, err
	}
	return value, record, model, nil
}

func (a *App) loadModelConfig() (config.Config, error) {
	value, err := a.Config.Load()
	if errors.Is(err, config.ErrNotFound) {
		return config.Config{Timeout: "30s", Color: "never"}, nil
	}
	if err != nil {
		return config.Config{}, commandError("configuration_unavailable", "本地配置无法读取", "检查配置文件权限后重试", ExitInternal)
	}
	if err := value.Validate(); err != nil {
		return config.Config{}, commandError("invalid_configuration", "本地配置无效", "修复或删除损坏的本地配置后重试", ExitInput)
	}
	return value, nil
}

func modelUsage(message string) error {
	return commandError("usage", message, "请使用TUI中的设置 > AI助手与模型", ExitInput)
}

func providerName(provider string) string {
	switch provider {
	case "openai":
		return "OpenAI"
	case "deepseek":
		return "DeepSeek"
	case "openrouter":
		return "OpenRouter"
	case "ollama":
		return "Ollama"
	case "custom":
		return "自定义OpenAI兼容服务"
	default:
		return strconv.Quote(provider)
	}
}
