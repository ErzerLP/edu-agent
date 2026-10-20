package command

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/credentials"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/id"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelgateway"
)

type agentSettings struct {
	config.Config
	Connection   modelgateway.Connection
	Capabilities modelgateway.Configuration
}

func (a *App) gatewayBinding() (config.Config, credentials.Record, error) {
	value, err := a.loadMutableClientConfig()
	if err != nil {
		return config.Config{}, credentials.Record{}, commandError("not_paired", "设备尚未配对或本地绑定不可用", "请先运行 edu-agent pair 配对设备", ExitAuth)
	}
	record, err := a.Credentials.Load()
	if err != nil {
		return config.Config{}, record, commandError("device_unauthorized", "设备凭据不可用", "请重新配对", ExitAuth)
	}
	if value.Agent == nil {
		v := config.DefaultAgentConfig()
		value.Agent = &v
	}
	return value, record, nil
}
func (a *App) ensureGatewayNotice(value *config.Config) error {
	if g := value.Gateway; g != nil && g.ServerURL == value.ServerURL && g.DeviceID == value.DeviceID && g.NoticeVersion == modelgateway.PrivacyVersion {
		return nil
	}
	if !a.interactiveTerminalAvailable() || a.Terminal == nil {
		return commandError("privacy_confirmation_required", modelgateway.PrivacyNotice, "请在交互终端运行 edu-agent model list，阅读并确认告知", ExitInput)
	}
	if _, err := fmt.Fprintln(a.Out, modelgateway.PrivacyNotice); err != nil {
		return err
	}
	yes, err := a.Terminal.Confirm("是否同意经此服务端使用所选模型？")
	if err != nil || !yes {
		return commandError("privacy_confirmation_required", "尚未确认隐私告知，未发送任何请求", "确认后才能读取网关配置和调用模型", ExitInput)
	}
	value.Gateway = &config.GatewayState{ServerURL: value.ServerURL, DeviceID: value.DeviceID, NoticeVersion: modelgateway.PrivacyVersion}
	return a.Config.Save(*value)
}
func (a *App) gatewayClient(value config.Config, record credentials.Record) (*modelgateway.Client, error) {
	return modelgateway.New(modelgateway.Options{ServerURL: value.ServerURL, DeviceID: record.DeviceID, Token: record.Token, AllowInsecureHTTP: value.AllowInsecureHTTP, Transport: a.GatewayTransport,
		Select: func(ctx context.Context, cfg modelgateway.Configuration) (modelgateway.Connection, error) {
			current, err := a.Config.Load()
			if err != nil {
				return modelgateway.Connection{}, err
			}
			g := current.Gateway
			if g == nil || g.ServerURL != value.ServerURL || g.DeviceID != record.DeviceID || current.ServerURL != value.ServerURL || current.DeviceID != record.DeviceID || g.NoticeVersion != modelgateway.PrivacyVersion {
				return modelgateway.Connection{}, modelgateway.ErrorFor("device_unauthorized")
			}
			if g.SelectedTarget == "" {
				return modelgateway.Connection{}, modelgateway.ErrorFor("model_selection_required")
			}
			selected, ok := cfg.Find(g.SelectedTarget)
			if !ok {
				return selected, modelgateway.ErrorFor("model_unavailable")
			}
			if selected.DestinationRevision != g.DestinationRevision {
				return selected, modelgateway.ErrorFor("destination_confirmation_required")
			}
			return selected, nil
		}})
}
func (a *App) gatewaySetup(ctx context.Context) (config.Config, credentials.Record, *modelgateway.Client, modelgateway.Configuration, error) {
	value, record, err := a.gatewayBinding()
	if err != nil {
		return value, record, nil, modelgateway.Configuration{}, err
	}
	if err = a.ensureGatewayNotice(&value); err != nil {
		return value, record, nil, modelgateway.Configuration{}, err
	}
	client, err := a.gatewayClient(value, record)
	if err != nil {
		return value, record, nil, modelgateway.Configuration{}, gatewayCommandError(err)
	}
	cfg, err := client.Config(ctx)
	return value, record, client, cfg, gatewayCommandError(err)
}
func (a *App) selectGateway(value *config.Config, selected modelgateway.Connection) error {
	g := *value.Gateway
	if g.SelectedTarget == selected.Target && g.DestinationRevision != "" && g.DestinationRevision != selected.DestinationRevision {
		if !a.interactiveTerminalAvailable() {
			return gatewayCommandError(modelgateway.ErrorFor("destination_confirmation_required"))
		}
		_, _ = fmt.Fprintln(a.Out, modelgateway.DestinationNotice)
		yes, err := a.Terminal.Confirm("确认使用更改后的模型服务？")
		if err != nil || !yes {
			return gatewayCommandError(modelgateway.ErrorFor("destination_confirmation_required"))
		}
	}
	g.SelectedTarget = selected.Target
	g.DestinationRevision = selected.DestinationRevision
	value.Gateway = &g
	return a.Config.Save(*value)
}
func (a *App) modelDependencies(ctx context.Context) (agentSettings, agentloop.Model, error) {
	value, _, client, cfg, err := a.gatewaySetup(ctx)
	if err != nil {
		return agentSettings{}, nil, err
	}
	if err = cfg.Ready(); err != nil {
		return agentSettings{}, nil, gatewayCommandError(err)
	}
	if value.Gateway.SelectedTarget == "" && len(cfg.Models) == 1 {
		if err = a.selectGateway(&value, cfg.Models[0]); err != nil {
			return agentSettings{}, nil, err
		}
	}
	// Bind uses this already-fetched configuration at startup. Every user turn
	// refreshes through Prepare before planning context or issuing model requests.
	selected, err := client.Selection(ctx, cfg)
	if err != nil {
		return agentSettings{}, nil, gatewayCommandError(err)
	}
	model := client.Model(cfg, selected, modelgateway.Identity{})
	var result agentloop.Model = model
	if a.NewModel != nil {
		result, err = a.NewModel(model)
	}
	return agentSettings{Config: value, Connection: selected, Capabilities: cfg}, result, err
}
func (a *App) agentDependencies(ctx context.Context) (agentSettings, credentials.Record, agentloop.Model, error) {
	value, model, err := a.modelDependencies(ctx)
	if err != nil {
		return agentSettings{}, credentials.Record{}, nil, err
	}
	record, err := a.Credentials.Load()
	return value, record, model, err
}
func (a *App) runModel(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return modelUsage("模型命令需要 list、select、show、set 或 test")
	}
	if args[0] == "set" {
		return a.runModelSet(args[1:])
	}
	switch args[0] {
	case "list", "show", "test":
		if len(args) != 1 {
			return modelUsage("此模型命令不接受额外参数")
		}
	case "select":
		if len(args) != 2 || (args[1] != "teaching" && args[1] != "mentor") {
			return modelUsage("请使用 model select teaching 或 model select mentor")
		}
	default:
		return modelUsage("未知模型命令；模型连接请在网页配置")
	}
	if args[0] == "test" {
		value, model, err := a.modelDependencies(ctx)
		if err != nil {
			return err
		}
		if gateway, ok := model.(*modelgateway.Model); ok {
			session, err := id.NewUUID()
			if err != nil {
				return err
			}
			turn, err := id.NewUUID()
			if err != nil {
				return err
			}
			copy := *gateway
			copy.Identity = modelgateway.Identity{SessionID: session, TurnID: turn}
			model = &copy
		}
		response, err := model.Complete(modelgateway.WithStage(ctx, "probe"), modelclient.Request{Messages: []modelclient.Message{{Role: "user", Content: "仅回复：连接正常"}}, MaxTokens: min(64, value.Connection.OutputTokens), ReasoningEffort: modelclient.ReasoningEffort(value.Agent.ReasoningEffort)})
		if err != nil {
			return gatewayCommandError(err)
		}
		if strings.TrimSpace(response.Message.Content) == "" {
			return commandError("model_protocol_error", "模型没有返回文本", "请检查服务端模型配置", ExitUnavailable)
		}
		_, err = fmt.Fprintf(a.Out, "模型连接正常：%s / %s\n", safeText(value.Connection.DisplayName), safeText(value.Connection.ModelName))
		return err
	}
	value, _, _, cfg, err := a.gatewaySetup(ctx)
	if err != nil {
		return err
	}
	if args[0] == "select" {
		if err = cfg.Ready(); err != nil {
			return gatewayCommandError(err)
		}
		selected, ok := cfg.Find(args[1])
		if !ok {
			return gatewayCommandError(modelgateway.ErrorFor("model_unavailable"))
		}
		if err = a.selectGateway(&value, selected); err != nil {
			return err
		}
		_, err = fmt.Fprintf(a.Out, "已选择：%s / %s（%s）\n", safeText(selected.DisplayName), safeText(selected.ModelName), selected.Target)
		return err
	}
	if args[0] == "show" {
		selected, ok := cfg.Find(value.Gateway.SelectedTarget)
		if !ok {
			return gatewayCommandError(modelgateway.ErrorFor("model_unavailable"))
		}
		_, err = fmt.Fprintf(a.Out, "所选连接：%s（%s）\n模型：%s\n上下文窗口：%d\n最大输出 tokens：%d\n用量归属：%s / %s\n", safeText(selected.DisplayName), selected.Target, safeText(selected.ModelName), selected.ContextTokens, selected.OutputTokens, safeText(value.ServerURL), safeText(value.DeviceID))
		return err
	}
	for _, m := range cfg.Models {
		_, err = fmt.Fprintf(a.Out, "%s\t%s\t%s\t上下文 %d / 输出 %d\n", m.Target, safeText(m.DisplayName), safeText(m.ModelName), m.ContextTokens, m.OutputTokens)
		if err != nil {
			return err
		}
	}
	if err = cfg.Ready(); err != nil {
		return gatewayCommandError(err)
	}
	return nil
}
func (a *App) runModelSet(args []string) error {
	set := newFlagSet("model set")
	var compaction, effort, history string
	var rounds int
	set.StringVar(&compaction, "context-compaction", "", "auto、recent-only 或 off")
	set.StringVar(&effort, "reasoning-effort", "", "默认推理强度")
	set.StringVar(&history, "session-history", "", "auto 或 off")
	set.IntVar(&rounds, "max-tool-rounds", -1, "最大工具轮数；0 表示不限制")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return modelUsage("模型参数格式无效；连接和容量由网页配置")
	}
	value, err := a.loadModelConfig()
	if err != nil {
		return err
	}
	candidate := config.DefaultAgentConfig()
	if value.Agent != nil {
		candidate = *value.Agent
	}
	if compaction != "" {
		candidate.ContextCompaction = compaction
	}
	if effort != "" {
		candidate.ReasoningEffort = effort
	}
	if history != "" {
		candidate.SessionHistory = history
	}
	roundsSet := false
	set.Visit(func(f *flag.Flag) {
		if f.Name == "max-tool-rounds" {
			roundsSet = true
		}
	})
	if roundsSet {
		if rounds < 0 {
			return commandError("invalid_configuration", "最大工具轮数无效", "请输入0或正整数；0表示不限制", ExitInput)
		}
		candidate.MaxToolRounds = rounds
	}
	if err = candidate.Validate(); err != nil {
		return commandError("invalid_configuration", "客户端模型偏好无效", "检查上下文压缩、推理强度和会话历史选项", ExitInput)
	}
	value.Agent = &candidate
	if err = a.Config.Save(value); err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.Out, "客户端模型偏好已更新；连接与容量由服务端管理。")
	return err
}
func gatewayCommandError(err error) error {
	if err == nil {
		return nil
	}
	var gateway *modelgateway.Error
	if errors.As(err, &gateway) {
		code := ExitUnavailable
		if gateway.Code == "device_unauthorized" || gateway.Code == "model_scope_required" {
			code = ExitAuth
		}
		mapped := commandError(gateway.Code, gateway.Message, "请按提示操作，不会自动重发请求", code)
		var requestErr *modelgateway.RequestError
		if errors.As(err, &requestErr) {
			mapped.RequestID = requestErr.RequestID
		}
		return mapped
	}
	return commandError("gateway_unavailable", "服务连接失败或请求未完成", "请稍后重试，已发送请求可能产生用量", ExitUnavailable)
}
