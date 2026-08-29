package command

import (
	"io"
	"strings"
)

type dashboardOutputWriter struct {
	target io.Writer
}

func (w dashboardOutputWriter) Write(value []byte) (int, error) {
	localized := localizeDashboardOutput(string(value))
	written, err := io.WriteString(w.target, localized)
	if err != nil {
		return 0, err
	}
	if written != len(localized) {
		return 0, io.ErrShortWrite
	}
	return len(value), nil
}

func localizeDashboardOutput(value string) string {
	lines := strings.SplitAfter(value, "\n")
	for index, line := range lines {
		ending := ""
		if strings.HasSuffix(line, "\n") {
			line = strings.TrimSuffix(line, "\n")
			ending = "\n"
		}
		lines[index] = localizeDashboardLine(line) + ending
	}
	return strings.Join(lines, "")
}

func localizeDashboardLine(line string) string {
	exact := map[string]string{
		"Current: active session has no route yet":       "当前：活动会话尚未生成学习路线",
		"Current: answer (not scored)":                   "当前：回答（不计分）",
		"Current: no active session":                     "当前：没有活动学习会话",
		"Current: no route yet":                          "当前：尚未生成学习路线",
		"Current: proposed route":                        "当前：模型建议的学习路线",
		"Current: explanation (not scored)":              "当前：讲解（不计分）",
		"Enter answer lines; a single . ends the block.": "请输入多行答案；单独输入一行 . 结束。",
		"Identity review required.":                      "需要确认知识身份匹配。",
	}
	if localized, ok := exact[line]; ok {
		return localized
	}
	prefixes := []struct {
		plain     string
		localized string
	}{
		{"Component database: ", "组件 数据库："},
		{"Component knowledge: ", "组件 知识库："},
		{"Component learning: ", "组件 学习进度："},
		{"Component memory: ", "组件 长期记忆："},
		{"Component model: ", "组件 服务端模型："},
		{"Component nocturne: ", "组件 Nocturne："},
		{"Component notesync: ", "组件 NoteSync："},
		{"Component outbox: ", "组件 Outbox："},
		{"Component privacy: ", "组件 隐私服务："},
		{"Component tutoring: ", "组件 教学服务："},
		{"Knowledge revision: ", "知识库版本："},
		{"Revision number: ", "修订序号："},
		{"Client settings updated. ", "客户端设置已更新。"},
		{"Allowed decisions: ", "可选决定："},
		{"Allowed actions: ", "可选操作："},
		{"Allowed help: ", "可选帮助等级："},
		{"Commands: ", "可用命令："},
		{"Rubric items: ", "评分项："},
		{"Reachability: ", "连接状态："},
		{"Readiness: ", "服务就绪状态："},
		{"Device ID: ", "设备 ID："},
		{"Projection: ", "进度投影："},
		{"Misconception: ", "误区："},
		{"Candidate: ", "候选项："},
		{"Document: ", "文档："},
		{"Locator: ", "定位标识："},
		{"Question: ", "问题："},
		{"Evidence: ", "学习证据："},
		{"Reviews: ", "复习项目："},
		{"Current: ", "当前："},
		{"Server: ", "服务器："},
		{"Device: ", "设备："},
		{"Scopes: ", "权限范围："},
		{"Timeout: ", "请求超时："},
		{"Color: ", "输出颜色："},
		{"Model: ", "模型兼容性："},
		{"Goal: ", "学习目标："},
		{"Session: ", "学习会话："},
		{"State: ", "状态："},
		{"Route: ", "学习路线："},
		{"Event: ", "事件序号："},
		{"Node: ", "知识节点："},
		{"Review: ", "复习："},
		{"Result: ", "结果："},
		{"Next: ", "下一步："},
		{"Feedback: ", "反馈："},
		{"Reason: ", "原因："},
		{"warning[", "提示["},
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(line, prefix.plain) {
			return prefix.localized + strings.TrimPrefix(line, prefix.plain)
		}
	}
	return line
}

func formatDashboardError(err *Error) string {
	detail, next := dashboardErrorMessage(err.Code)
	text := "错误[" + err.Code + "]"
	if err.RequestID != "" {
		text += " request_id=" + err.RequestID
	}
	text += "：" + detail
	if next != "" {
		text += "；下一步：" + next
	}
	return text
}

func dashboardErrorMessage(code string) (string, string) {
	switch code {
	case "not_paired":
		return "客户端尚未与服务端配对", "打开设置完成配对后重试"
	case "local_state_orphaned", "local_state_pending", "local_state_invalid", "device_mismatch":
		return "本地配对状态不完整或不一致", "使用主控制台中的本地修复操作"
	case "authentication_failed", "pairing_failed":
		return "认证或配对凭据无效", "重新生成配对码并完成配对"
	case "forbidden":
		return "当前设备没有执行此操作所需的权限", "使用具备相应权限的用户设备"
	case "model_unavailable", "model_configuration_missing", "model_credential_missing":
		return "AI模型尚未配置或暂时不可用", "在AI助手设置中检查模型与API Key"
	case "service_unavailable", "dependency_unavailable", "temporarily_unavailable", "upstream_unavailable", "projection_unavailable":
		return "服务或依赖暂时不可用", "稍后重试并检查管理WebUI中的服务状态"
	case "invalid_goal", "invalid_input", "invalid_configuration", "invalid_markdown_input", "invalid_request", "usage":
		return "输入的操作或参数不符合要求", "返回主控制台并重新填写"
	case "version_conflict", "revision_conflict", "stale_cursor", "stale_proposal":
		return "服务端权威状态已发生变化", "刷新当前页面后按最新状态重试"
	case "rate_limited":
		return "请求过于频繁", "等待片刻后重试"
	case "content_redacted", "privacy_clear_in_progress":
		return "内容因隐私处理暂时不可用", "等待隐私处理完成后重新读取"
	case "terminal_error", "not_a_terminal":
		return "当前终端无法完成交互操作", "检查终端能力或改用显式子命令"
	default:
		return "操作未完成", "根据错误代码检查配置，或使用显式子命令获取详细诊断"
	}
}
