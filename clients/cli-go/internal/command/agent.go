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

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontext"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontroller"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentui"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
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
	if len(args) > 0 && args[0] == "sessions" {
		return a.runAgentSessions(ctx, args[1:])
	}
	var err error
	if len(args) > 0 && args[0] == "resume" {
		err = a.runAgentResume(ctx, args[1:])
	} else {
		err = a.runNewAgent(ctx, args)
	}
	for {
		var navigation *agentui.NavigateLearningContext
		if !errors.As(err, &navigation) {
			return err
		}
		err = a.enterAgentBinding(ctx, navigation.Binding, navigation.NoSave, navigation.WorkspaceRoot)
	}
}

func (a *App) runNewAgent(ctx context.Context, args []string) error {
	args, localOptions, localErr := parseLocalExecutionOptions(args)
	if localErr != nil {
		return commandError("usage", "本地任务资源预算无效", "运行 edu-agent agent --help 查看任务资源参数", ExitInput)
	}
	args, fileReadBytes, fileReadErr := parseFileReadOptions(args)
	if fileReadErr != nil {
		return commandError("usage", "文件读取预算无效", "运行 edu-agent agent --help 查看 --file-read-limit", ExitInput)
	}
	args, fileEditBytes, fileEditErr := parseFileEditOptions(args)
	if fileEditErr != nil {
		return commandError("usage", "文件编辑预算无效", "运行 edu-agent agent --help 查看 --file-edit-limit", ExitInput)
	}
	args, fileResults, resultErr := parseFileResultOptions(args)
	if resultErr != nil {
		return commandError("usage", "差异/补丁/结果保留预算无效", "运行 edu-agent agent --help 查看文件结果资源参数", ExitInput)
	}
	args, fileQueries, queryErr := parseFileQueryOptions(args)
	if queryErr != nil {
		return commandError("usage", "查询保留预算无效", "运行 edu-agent agent --help 查看查询资源参数", ExitInput)
	}
	args, fileCopies, copyErr := parseFileCopyOptions(args)
	if copyErr != nil {
		return commandError("usage", "复制资源预算无效", "运行 edu-agent agent --help 查看复制资源参数", ExitInput)
	}
	set := newFlagSet("agent")
	var workspacePath string
	var noSave bool
	var goalID, teachingID string
	set.StringVar(&goalID, "goal", "", "明确绑定目标 ID")
	set.StringVar(&teachingID, "session", "", "明确绑定教学会话 ID（需同时指定 --goal）")
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
	value, record, model, err := a.agentDependencies(ctx)
	if err != nil {
		return err
	}
	requestTimeout, modelTimeout, err := agentTimeouts(value)
	if err != nil {
		return err
	}
	server := a.scopedClient(value.ServerURL, record.Token, requestTimeout)
	learningBinding := agentcontext.Binding{SpaceID: a.learningSpace, GoalID: goalID, SessionID: teachingID}.Normalize()
	if !learningBinding.Valid() {
		return commandError("invalid_agent_binding", "学习区/目标/教学绑定无效", "--session 需配合 --goal，所有 ID 使用规范 UUID", ExitInput)
	}
	workspaceStatus := workspace.Status{Code: workspace.CodeWorkspaceUnavailable}
	var workspaceExecutor *workspace.Workspace
	if workspacePathErr == nil {
		limits := workspace.DefaultLimits()
		limits.ReadFileBytes, limits.EditFileBytes = fileReadBytes, fileEditBytes
		limits.DiffBytes, limits.PatchBytes = fileResults.DiffBytes, fileResults.PatchBytes
		limits.QueryMemoryBytes, limits.QueryEntries, limits.QueryRecords = fileQueries.MemoryBytes, fileQueries.Entries, fileQueries.Records
		limits.CopyBytes, limits.CopyPlanBytes, limits.CopyEntries = fileCopies.Bytes, fileCopies.PlanBytes, fileCopies.Entries
		workspaceExecutor, err = workspace.OpenWithLimits(workspacePath, limits)
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
		Gateway:       agentcontroller.GatewayBinding{Target: value.Connection.Target, ServerURL: value.ServerURL, DeviceID: value.DeviceID, DestinationRevision: value.Connection.DestinationRevision, ModelName: value.Connection.ModelName},
		WorkspaceRoot: workspacePath,
		LoopOptions: agentloop.Options{
			LearningBinding: learningBinding,
			ContextWindow:   value.Connection.ContextTokens, MaxTokens: value.Connection.OutputTokens,
			ContextCompaction: value.Agent.ContextCompaction,
			ReasoningEffort:   modelclient.ReasoningEffort(value.Agent.ReasoningEffort),
			ModelTimeout:      modelTimeout, ToolTimeout: requestTimeout, NewUUID: a.NewUUID,
			Workspace: workspaceExecutor, WorkspaceStatus: workspaceStatus,
			WorkspaceCopyBytes: fileCopies.Bytes, WorkspaceCopyPlanBytes: fileCopies.PlanBytes, WorkspaceCopyEntries: fileCopies.Entries,
			FileBatchMemoryBytes: fileCopies.JournalMemoryBytes, FileBatchMaxRecords: fileCopies.JournalRecords,
			WorkspaceQueryMemoryBytes: fileQueries.MemoryBytes,
			WorkspaceQueryEntries:     fileQueries.Entries,
			WorkspaceQueryRecords:     fileQueries.Records,
			WorkspaceReadFileBytes:    fileReadBytes,
			WorkspaceEditFileBytes:    fileEditBytes,
			WorkspaceDiffBytes:        fileResults.DiffBytes,
			WorkspacePatchBytes:       fileResults.PatchBytes,
			ArtifactOptions:           fileResults.Artifacts,
			LocalExec:                 localexec.New(localOptions),
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
		_, _ = fmt.Fprintf(a.Err, "会话已启用自动加密保存：%s。自动标题会向所选服务端模型发送有界的对话摘要。\n", safeText(controller.SessionID()))
	} else if noSave {
		_, _ = fmt.Fprintln(a.Err, "当前新会话为不保存模式（--no-save），退出后不可恢复。")
	} else if value.Agent.SessionHistory == "off" {
		_, _ = fmt.Fprintln(a.Err, "当前新会话不保存：设置中的Agent Session历史为off；退出后不可恢复。")
	} else if status.DegradedReason != "" {
		_, _ = fmt.Fprintln(a.Err, safeText(status.DegradedReason))
	}
	return a.runAgentController(ctx, controller, value.Connection.ModelName)
}

func (a *App) runAgentResume(ctx context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(a.Out, agentHelpText+"\n")
		return err
	}
	args, localOptions, localErr := parseLocalExecutionOptions(args)
	if localErr != nil {
		return commandError("usage", "本地任务资源预算无效", "运行 edu-agent agent resume --help 查看任务资源参数", ExitInput)
	}
	args, fileReadBytes, fileReadErr := parseFileReadOptions(args)
	if fileReadErr != nil {
		return commandError("usage", "文件读取预算无效", "运行 edu-agent agent resume --help 查看 --file-read-limit", ExitInput)
	}
	args, fileEditBytes, fileEditErr := parseFileEditOptions(args)
	if fileEditErr != nil {
		return commandError("usage", "文件编辑预算无效", "运行 edu-agent agent resume --help 查看 --file-edit-limit", ExitInput)
	}
	args, fileResults, resultErr := parseFileResultOptions(args)
	if resultErr != nil {
		return commandError("usage", "差异/补丁/结果保留预算无效", "运行 edu-agent agent resume --help 查看文件结果资源参数", ExitInput)
	}
	args, fileQueries, queryErr := parseFileQueryOptions(args)
	if queryErr != nil {
		return commandError("usage", "查询保留预算无效", "运行 edu-agent agent resume --help 查看查询资源参数", ExitInput)
	}
	args, fileCopies, copyErr := parseFileCopyOptions(args)
	if copyErr != nil {
		return commandError("usage", "复制资源预算无效", "运行 edu-agent agent resume --help 查看复制资源参数", ExitInput)
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
	value, record, model, err := a.agentDependencies(ctx)
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
	if target == "" && !all {
		filtered := summaries[:0]
		space := (agentcontext.Binding{SpaceID: a.learningSpace}).Normalize().SpaceID
		for _, summary := range summaries {
			if summary.LearningBinding.Normalize().SpaceID == space {
				filtered = append(filtered, summary)
			}
		}
		summaries = filtered
	}
	workspaceBinding, _ := agentcontroller.BindWorkspace(workspacePath)
	workspaceID := agentcontroller.WorkspaceScopeID(workspaceBinding)
	provider := agentcontroller.GatewayBinding{Target: value.Connection.Target, ServerURL: value.ServerURL, DeviceID: value.DeviceID, DestinationRevision: value.Connection.DestinationRevision, ModelName: value.Connection.ModelName}
	var selected agentsession.Summary
	var pickerChoice agentui.PickerChoice
	if target == "" && !last {
		if picker, ok := a.AgentUI.(AgentSessionPickerRunner); ok {
			selector := agentcontroller.NewSelector(store, workspaceID, provider).InLearningSpace((agentcontext.Binding{SpaceID: a.learningSpace}).Normalize().SpaceID)
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
				launchArgs := append([]string{"--file-read-limit", strconv.FormatInt(fileReadBytes, 10), "--file-edit-limit", strconv.FormatInt(fileEditBytes, 10)}, fileResults.arguments()...)
				launchArgs = append(launchArgs, fileCopies.arguments()...)
				return a.runNewAgent(ctx, append(launchArgs, fileQueries.arguments()...))
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
	server := a.scopedClient(value.ServerURL, record.Token, requestTimeout)
	controller, err := agentcontroller.Resume(ctx, agentcontroller.Dependencies{
		Store: store, Model: model, Server: server,
		Gateway: provider,
		LoopOptions: agentloop.Options{
			ContextWindow: value.Connection.ContextTokens, MaxTokens: value.Connection.OutputTokens,
			ContextCompaction: value.Agent.ContextCompaction,
			ReasoningEffort:   modelclient.ReasoningEffort(value.Agent.ReasoningEffort),
			ModelTimeout:      modelTimeout, ToolTimeout: requestTimeout, NewUUID: a.NewUUID,
			WorkspaceCopyBytes: fileCopies.Bytes, WorkspaceCopyPlanBytes: fileCopies.PlanBytes, WorkspaceCopyEntries: fileCopies.Entries,
			FileBatchMemoryBytes: fileCopies.JournalMemoryBytes, FileBatchMaxRecords: fileCopies.JournalRecords,
			WorkspaceQueryMemoryBytes: fileQueries.MemoryBytes,
			WorkspaceQueryEntries:     fileQueries.Entries,
			WorkspaceQueryRecords:     fileQueries.Records,
			WorkspaceReadFileBytes:    fileReadBytes,
			WorkspaceEditFileBytes:    fileEditBytes,
			WorkspaceDiffBytes:        fileResults.DiffBytes,
			WorkspacePatchBytes:       fileResults.PatchBytes,
			ArtifactOptions:           fileResults.Artifacts,
			LocalExec:                 localexec.New(localOptions),
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
	if status.GatewayConfirmationRequired {
		confirmed := pickerChoice.Gateway
		var confirmErr error
		if !confirmed {
			confirmed, confirmErr = a.Terminal.Confirm("所选模型服务已更改。确认经服务端向所选模型发送历史会话文本？")
		}
		if confirmErr != nil {
			_ = controller.Shutdown(context.Background())
			return commandError("terminal_error", "无法读取模型目的地确认", "重试恢复命令", ExitInternal)
		}
		if confirmed {
			if err := controller.ConfirmGateway(ctx); err != nil {
				_ = controller.Shutdown(context.Background())
				return sessionCommandError(err)
			}
		}
	}
	return a.runAgentController(ctx, controller, value.Connection.ModelName)
}

func (a *App) runAgentController(ctx context.Context, controller *agentcontroller.Controller, modelName string) error {
	noSave := !controller.Status().Persistent
	uiErr := a.runAgentUI(ctx, controller, modelName)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	shutdownErr := controller.Shutdown(shutdownCtx)
	cancel()
	if shutdownErr != nil {
		var taskErr *localexec.Error
		if errors.As(shutdownErr, &taskErr) {
			return commandError(taskErr.Code, "本地任务清理未能确认完成", "检查相关进程；不要将此状态当作已全部停止，也不要自动重跑历史命令", ExitUnavailable)
		}
		return sessionCommandError(shutdownErr)
	}
	var navigation *agentui.NavigateLearningContext
	if errors.As(uiErr, &navigation) {
		navigation.NoSave = noSave
		navigation.WorkspaceRoot = controller.LearningWorkspaceRoot()
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
	run := a.AgentUI.Run
	if _, ok := a.AgentUI.(defaultAgentUIRunner); ok {
		run = func(ctx context.Context, c agentui.Conversation, name string) error {
			return (agentui.Runner{In: a.teachingInput, Out: a.teachingOutput, Session: c, ModelName: name, Workflow: a.runAgentWorkflow}).Run(ctx)
		}
	}
	if err := run(ctx, conversation, modelName); err != nil {
		var navigation *agentui.NavigateLearningContext
		if errors.As(err, &navigation) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return commandError("terminal_error", "AI学习助手界面无法运行", "检查终端能力后重试", ExitInternal)
	}
	return nil
}

func (a *App) enterAgentBinding(ctx context.Context, b agentcontext.Binding, noSave bool, workspaceRoot string) error {
	local := *a
	local.learningSpace = b.Normalize().SpaceID
	if !noSave {
		value, err := a.Config.Load()
		if err == nil {
			store, err := a.openAgentSessionStore(ctx, value.ServerURL)
			if err == nil {
				items, listErr := store.List(ctx)
				_ = store.Close()
				if listErr == nil {
					for _, item := range sortAgentSessionSummaries(items) {
						if item.LearningBinding.Normalize() == b.Normalize() && !item.Locked && !item.Corrupt && !item.Unavailable {
							return local.runAgentResume(ctx, []string{item.SessionID, "--all"})
						}
					}
				}
			}
		}
	}
	args := []string{"--workspace", workspaceRoot}
	if b.GoalID != "" {
		args = append(args, "--goal", b.GoalID)
	}
	if b.SessionID != "" {
		args = append(args, "--session", b.SessionID)
	}
	if noSave {
		args = append(args, "--no-save")
	}
	return local.runNewAgent(ctx, args)
}

func agentTimeouts(value agentSettings) (requestTimeout, modelTimeout time.Duration, err error) {
	requestTimeout, err = config.ParseTimeout(value.Timeout)
	if err != nil {
		return 0, 0, commandError("invalid_configuration", "客户端请求超时配置无效", "在设置中修复客户端配置", ExitInput)
	}
	modelTimeout = time.Duration(value.Capabilities.Limits.IdleTimeoutSeconds+5) * time.Second
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
	case "session_gateway_confirmation_required":
		return commandError("session_gateway_confirmation_required", "模型目的地变更尚未确认", "确认所选模型服务后再发送历史文本", ExitConflict)
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
	var remote *api.APIError
	var protocol *api.ProtocolError
	if errors.As(err, &remote) || errors.As(err, &protocol) {
		return mapAPIError(err)
	}
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
	case errors.Is(err, agentcontroller.ErrGatewayConfirmationRequired):
		return sessionCommandErrorForCode("session_gateway_confirmation_required")
	default:
		return commandError("session_store_unavailable", "Agent Session操作无法完成", "检查本地加密存储和系统钥匙串后重试", ExitUnavailable)
	}
}

const agentHelpText = `用法：
  edu-agent agent [--workspace PATH] [--no-save] [--space UUID] [--goal UUID] [--session UUID]
  edu-agent agent resume [SESSION] [--all]
  edu-agent agent resume --last
  edu-agent agent sessions delete SESSION|storage:<storage-id> --confirmed
  edu-agent agent sessions clear --confirmed

启动、恢复或管理自动加密保存的 AI 学习助手会话。无 SESSION 的 resume 打开与 TUI F2 相同的选择器；Session 不按时间自动删除，达到上限也不自动淘汰。

选项：
  --space UUID      绑定学习区，省略时使用当前区；F7 明确选择区/目标/教学并进入对应独立聊天
  --goal UUID       可选目标；省略可区内交流，需要具体目标时由用户选择，不取最近目标
  --session UUID    可选教学会话，必须同时 --goal；只读其冻结范围，不调用全局 current
  --workspace PATH  固定本 Session 的本地工作区；省略时使用 Agent 启动目录
  --no-save         仅当前新 Session 不保存；不会改变默认设置
  --last            恢复当前学习区及工作区内最近可恢复的聊天；不能与 SESSION 或 --all 合用
  --all             关闭学习区及工作区过滤；跨区恢复保留原绑定，不改标签或重放教学
  --file-read-limit BYTES      完整读取预算，默认67108864；适用于新建/resume/F2切换
  --file-edit-limit BYTES      局部edit原文/候选预算，默认67108864；独立于read/write
  --file-diff-limit BYTES      完整差异/单产物上限，默认134217728，最高1073741824
  --file-patch-limit BYTES     patch原文总量/候选总量各自上限，默认67108864
  --artifact-memory-limit B   仅内存长结果累计上限，默认268435456，最高1073741824
  --artifact-max-records N    每Session长结果记录数，默认256，最高8192；不自动淘汰
  --file-query-memory-limit B 查询保留总预算，默认67108864，最高1073741824
  --file-query-entry-limit N  每查询保留条目预算，默认100000，最高1000000
  --file-query-max-records N  工作区查询数，默认16，最高64
  --file-copy-limit BYTES    复制文件总字节预算，默认1073741824；上限为平台maxInt-1
  --file-copy-plan-limit BYTES  复制/归档清理完整清单预算，默认67108864，最高1073741824
  --file-copy-entry-limit N  复制/归档清理计划入口数，默认100000，最高1000000
  --file-copy-journal-limit BYTES  复制/归档清理追加日志内存预算，默认268435456，最高1073741824
  --file-copy-max-records N  复制/归档清理追加日志记录数，默认256，最高8192；不自动淘汰
  --task-max-records N         当前客户端保留的任务记录数，默认256
  --task-max-running N         同时运行的任务数，默认16
  --task-output-limit BYTES    每任务 stdout+stderr 保留上限，默认8388608
  --task-total-output-limit B  客户端总内存输出保留上限，默认67108864
  --task-saved-output-limit B  每任务加密输出保留上限，默认134217728；仅持久Session
  -h, --help        显示此帮助

Session picker：空闲时 F2 打开；Tab 切换当前/全部学习区及工作区，支持搜索、恢复、重命名、二次确认删除和新建。恢复或切换会重置 YOLO、旧文件授权和未完成交互。自动标题会向所选服务端模型 发送有界安全对话片段；恢复后的模型请求会发送历史上下文，模型目的地变化时先确认。旧工作区不可用时只恢复对话并禁用文件工具，不回退到当前目录。系统钥匙串不可用时不写明文，只明确降级为未保存。clear 只清除本地 Session store，不清除服务端、终端、Shell、模型服务或 OS 备份中的副本。

每轮工具调用上限与网页共用设置，默认不限；每轮开始和明确“继续”时在线读取，本轮内固定。按实际执行的工具调用计数，等待批准不计数；到限说明进展后暂停，输入“继续”重新计数并接着完成未执行的工具，不重跑已完成项。设置读取失败会停止并提示重试，不使用缓存；不按累计模型请求或 Token 数停止。

文件工具：stat、find、list、read、search、write、edit、apply_patch、mkdir、copy、move、archive、restore_archive、purge_archive。copy支持普通文件（含二进制）和完整递归目录；复制按总文件字节、完整计划入口和追加日志预算限制。目标根须不存在，不覆盖或合并；一次授权完整计划，失败/取消停止余项，已完成项保留。--no-save只在有界内存保留复制追加日志。artifact只读完整差异和逐项结果。副作用默认逐次确认；F4 可切换仅当前 Session 生效的 YOLO。
read在独立预算内支持大于1MiB文本及长行续读，始终返回原始完整文件hash；next_offset/next_byte_offset对应实际返回正文，并携带expected_hash续读。首版仍是全文读取，不是GB级固定内存/低IO引擎；超限可调读取预算或使用Shell。本参数不扩大write/edit、stat.hash、search或模型结果预算。
局部edit在独立预算内支持大于1MiB文本，保留精确唯一匹配、完整expected_hash、授权、WAL和原子发布；原文/候选仍全文处理，参数仍64KiB。短预览可截断且明确标记；write/edit/apply_patch授权前保留完整diff，无法完整保留就不发布。F6独立分页和检索diff/receipt，不调用模型，也不新增逐页审批；原copy/move/mkdir末页门槛不变。
apply_patch接受严格Add/Update/Delete文本及expected_hashes，最多16文件；更新/删除须携带完整hash。全部预检，一次授权，逐项WAL/复核/原子发布并结算；冲突、失败或取消停止余项，已完成项不回滚，删除仍归档。拒绝Move、模糊匹配和二进制patch。artifact list/read/search使用独立原始字节游标；diff不证明已执行，逐项结果与文件状态分开判断。
差异/结果在持久Session中独立加密，--no-save只在有界内存；恢复不自动应用历史修改，clear/delete遵守Session密钥撤销，保存失败如实报告。文件与结果资源预算随当前客户端的新建/resume/F2切换生效，不扩大write/stat.hash/search的1MiB预算。
list/find/search可复用完整原参数及next_cursor续页；可完整覆盖超过单目录2000项，不会从头重扫冒充续页。无正文进度页仍须继续；scan_finished/scan_complete与more分别报告扫描结束、完整性及未返回结果。查询仅进程内保留，空闲10分钟过期、容量紧张时回收最旧已结束查询，恢复/F2切换后旧游标过期；目录、已读正文或采用的忽略规则变化使cursor_stale，原生变更观察不可用则明确失败。查询预算适用于当前客户端新建/resume/F2，不扩大search单文件1MiB预算。
restore_archive要求确切归档source、显式destination和当前stat的expected_version；用list/find/read/stat分页定位归档，不从旧布局猜测原位置。只在同文件系统不覆盖移动，目标父须已存在，不合并、不创建父目录、不复制后删除或清理空容器。恢复确认可PgUp/PgDn查看完整预览，无需末页即可授权；重启仅恢复加密事实和不可重放身份，未知须核查两端。
purge_archive要求确切归档path及当前stat的expected_version，容器/文件/子目录均可，但不能选归档根、工作区根、归档外或通配全部。每次永久清理不可撤销，YOLO仍须明确批准；授权前完整冻结并保留清单，可F6/artifact独立续览而无需末页。按同挂载边界后序逐项意图/删除/结算，链接仅删入口不跟随，遇变化/失败/取消/保存失败即停余项；已删除项不回滚，不顺带清理父容器。logical_bytes_removed仅为普通文件逻辑字节，物理释放量未知；硬链接、稀疏、打开文件和快照影响实际释放。历史名称file-copy的计划/入口/日志/记录预算同时适用于purge，--file-copy-limit不限制清理大文件；--no-save清理历史仅内存，重启只读恢复日志与防重放身份，不恢复批准或自动续执行。
本地 Shell：正常 OS 用户权限，可访问工作区外路径和网络，不受文件确认模式限制。shell/task 支持长任务、stdin 和停止；默认不设置执行总超时。F5 查看内存输出、切换 stdout/stderr、翻页和停止，退出客户端会收尾受管任务。
持久Session的输出独立加密保存，可用task search或F5的/检索、n继续，并在重启后读取已保存范围；恢复不重跑旧命令、不恢复进程控制。--no-save输出仅内存保留，退出不可恢复；配额/保存失败和缺口明确显示。PTY交互：shell指定pty=true（默认24行80列，可用rows/cols设置1..4096）；合并流为stdout。task interrupt/eof发送终端控制字节，resize调整尺寸，close_input仍仅用于pipe。F5按i进入不回显草稿的行式输入，Enter送行，Ctrl+C中断，Ctrl+D终端EOF，Ctrl+Q退出；程序回显可能作为输出保存，不是全屏终端模拟器。上述任务资源参数同时适用于新建和 resume，不改变命令权限。`

func (a *App) loadModelConfig() (config.Config, error) {
	value, err := a.Config.Load()
	if errors.Is(err, config.ErrNotFound) {
		return config.Config{Timeout: "30s", Color: "never"}, nil
	}
	if err != nil {
		var unknown *config.UnknownFieldError
		if errors.As(err, &unknown) {
			return config.Config{}, commandError("invalid_configuration", unknown.Error(), "", ExitInput)
		}
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
