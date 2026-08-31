package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentui"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/credentials"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelsecret"
)

type defaultAgentUIRunner struct {
	in  io.Reader
	out io.Writer
}

func (r defaultAgentUIRunner) Run(ctx context.Context, conversation agentui.Conversation, modelName string) error {
	return agentui.Runner{In: r.in, Out: r.out, Session: conversation, ModelName: modelName}.Run(ctx)
}

func (a *App) runAgent(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return commandError("usage", "AI学习助手不接受额外参数", "在交互终端运行 edu-agent agent", ExitInput)
	}
	if !a.interactiveTerminalAvailable() || a.AgentUI == nil {
		return commandError("not_a_terminal", "AI学习助手需要交互终端", "请在TTY终端运行 edu-agent agent", ExitInput)
	}
	value, record, model, err := a.agentDependencies()
	if err != nil {
		return err
	}
	requestTimeout, err := config.ParseTimeout(value.Timeout)
	if err != nil {
		return commandError("invalid_configuration", "客户端请求超时配置无效", "在设置中修复客户端配置", ExitInput)
	}
	modelTimeout, err := config.ParseTimeout(value.Agent.Timeout)
	if err != nil {
		return commandError("invalid_configuration", "模型请求超时配置无效", "在设置中修复模型配置", ExitInput)
	}
	server := a.NewClient(value.ServerURL, record.Token, requestTimeout)
	session, err := agentloop.New(model, server, agentloop.Options{
		ContextWindow: value.Agent.ContextWindow, MaxToolRounds: value.Agent.MaxToolRounds,
		ContextCompaction: value.Agent.ContextCompaction,
		ReasoningEffort:   modelclient.ReasoningEffort(value.Agent.ReasoningEffort),
		ModelTimeout:      modelTimeout,
		ToolTimeout:       requestTimeout,
		NewUUID:           a.NewUUID,
	})
	if err != nil {
		return commandError("agent_configuration_invalid", "AI学习助手配置无效", "检查模型参数后重试", ExitInput)
	}
	defer session.Close()
	if err := a.AgentUI.Run(ctx, session, value.Agent.Model); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return commandError("terminal_error", "AI学习助手界面无法运行", "检查终端能力后重试", ExitInternal)
	}
	return nil
}

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
		_, err = fmt.Fprintf(a.Out, "提供商：%s\nBase URL：%s\n模型：%s\n上下文窗口：%d\n上下文压缩：%s\n默认推理强度：%s\n请求超时：%s\n最大工具轮数：%d\nAPI Key：%s\n",
			safeText(value.Agent.Provider), safeText(value.Agent.BaseURL), safeText(value.Agent.Model), value.Agent.ContextWindow,
			safeText(value.Agent.ContextCompaction), safeText(value.Agent.ReasoningEffort), safeText(value.Agent.Timeout), value.Agent.MaxToolRounds, keyStatus)
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
	var provider, baseURL, modelName, timeout, contextCompaction, reasoningEffort string
	var contextWindow, maxToolRounds int
	set.StringVar(&provider, "provider", "", "model provider")
	set.StringVar(&baseURL, "base-url", "", "OpenAI-compatible base URL")
	set.StringVar(&modelName, "model", "", "model name")
	set.IntVar(&contextWindow, "context-window", 0, "context window")
	set.StringVar(&contextCompaction, "context-compaction", "", "auto, recent-only, or off")
	set.StringVar(&reasoningEffort, "reasoning-effort", "", "auto, none, minimal, low, medium, high, xhigh, or max")
	set.StringVar(&timeout, "timeout", "", "model timeout")
	set.IntVar(&maxToolRounds, "max-tool-rounds", 0, "maximum tool rounds")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return modelUsage("模型参数格式无效")
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
		candidate = config.DefaultAgentConfig(provider)
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
	if strings.TrimSpace(timeout) != "" {
		candidate.Timeout = strings.TrimSpace(timeout)
	}
	if maxToolRounds != 0 {
		candidate.MaxToolRounds = maxToolRounds
	}
	if err := candidate.Validate(); err != nil {
		return commandError("invalid_configuration", "AI模型参数无效", "检查地址、模型、上下文窗口、压缩模式、推理强度、超时和工具轮数", ExitInput)
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
