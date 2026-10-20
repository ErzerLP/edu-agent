package modelgateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return "[" + e.Code + "] " + e.Message }
func ErrorFor(code string) *Error {
	messages := map[string]string{
		"invalid_request": "请求无法发送，请调整后重试", "request_too_large": "内容太大，请缩减后重试",
		"device_unauthorized": "设备已失效或未配对，请重新配对", "model_scope_required": "请重新配对此 CLI，以取得模型调用权限",
		"model_not_allowed": "请使用网页设置的模型", "configuration_changed": "模型设置已改变，请刷新配置后重试",
		"privacy_generation_changed": "数据状态已改变，请重新开始一轮；不会自动重发旧请求",
		"request_already_accepted":   "此次请求已受理，请核对状态；不会再次发送",
		"turn_binding_conflict":      "此轮归属已变化，请开始新轮次",
		"context_limit_exceeded":     "内容超过模型上下文限制，请缩减后重试", "output_limit_exceeded": "输出设置超过模型限制，请刷新配置后重试",
		"gateway_busy": "模型服务繁忙，请稍后重试", "gateway_version_unsupported": "网关协议不兼容，请升级 CLI 或服务端",
		"model_unavailable": "所选模型不可用，请运行 model select 重新选择", "gateway_capacity_disabled": "模型网关不可用，请在网页将总并发数调至至少 2",
		"gateway_unavailable": "服务暂不可用，请稍后重试；已发送请求的结果和用量可能未知",
		"upstream_error":      "模型服务失败，请稍后重试", "upstream_incompatible": "模型服务不支持此请求，请检查网页模型设置",
		"upstream_rate_limited": "模型服务暂时限流，请稍后重试",
		"model_idle_timeout":    "模型长时间没有回应，可能已消耗用量；请自行决定是否重试",
		"model_request_timeout": "本次请求超时，可能已消耗用量；请自行决定是否重试",
		"device_revoked":        "设备已撤销，已停止输出，请重新配对", "request_cancelled": "已停止本次请求",
		"gateway_connection_invalid":        "服务连接不可用，请检查服务端部署和模型网关路径",
		"no_selectable_model":               "没有可选模型，请在网页配置并启用教学或 AI 助手模型",
		"model_selection_required":          "请先运行 model list 查看连接，再运行 model select teaching 或 model select mentor",
		"destination_confirmation_required": DestinationNotice + "；请运行 model select 重新选择并确认",
	}
	message, ok := messages[code]
	if !ok {
		return &Error{Code: "gateway_unavailable", Message: "模型请求无法完成，请检查服务端或稍后重试"}
	}
	return &Error{Code: code, Message: message}
}
func isHTML(h http.Header, b []byte) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/html") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(string(b))), "<")
}
func responseError(status int, h http.Header, b []byte) error {
	if status >= 300 && status < 400 || isHTML(h, b) {
		return ErrorFor("gateway_connection_invalid")
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &envelope) == nil && envelope.Error.Code != "" {
		return ErrorFor(envelope.Error.Code)
	}
	if status == 401 {
		return ErrorFor("device_unauthorized")
	}
	if status >= 500 {
		return ErrorFor("gateway_unavailable")
	}
	return ErrorFor("gateway_connection_invalid")
}

// RequestError preserves the local association even when no response headers
// arrive. Unwrap keeps cancellation and shared protocol errors recognizable.
type RequestError struct {
	RequestID string
	Err       error
}

func (e *RequestError) Error() string {
	return e.Err.Error() + "（请求标识：" + e.RequestID + "）"
}
func (e *RequestError) Unwrap() error { return e.Err }
