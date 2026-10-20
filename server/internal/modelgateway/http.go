package modelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/modeladmission"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
)

var errorMessages = map[string]string{
	"invalid_request": "请求无法发送，请检查输入", "request_too_large": "请求内容太大，请缩小内容", "device_unauthorized": "设备已失效，请重新配对", "model_scope_required": "请重新配对此 CLI", "model_not_allowed": "请使用网页设置的模型", "configuration_changed": "模型设置已改变，请刷新配置", "privacy_generation_changed": "数据状态已改变，请开始新轮次", "request_already_accepted": "此次请求已受理，请核对状态", "turn_binding_conflict": "此轮归属已变化，请开始新轮次", "context_limit_exceeded": "内容超过模型上下文限制", "output_limit_exceeded": "输出设置超过模型限制", "gateway_busy": "模型服务繁忙，请稍后重试", "gateway_version_unsupported": "请升级 CLI 或服务端", "model_unavailable": "所选模型不可用，请重新选择", "gateway_capacity_disabled": "请将总并发数调至至少 2", "gateway_unavailable": "服务暂不可用，请核对请求状态", "upstream_error": "模型服务调用失败", "upstream_incompatible": "模型服务不支持此请求", "upstream_rate_limited": "模型服务暂时限流", "model_idle_timeout": "模型长时间没有回应", "model_request_timeout": "本次模型请求超时", "device_revoked": "设备已撤销", "request_cancelled": "已停止", "request_not_found": "未找到此请求",
}

func errorCode(err error) string {
	var g gatewayError
	var l llm.GatewayError
	if errors.As(err, &g) {
		if _, ok := errorMessages[string(g)]; ok {
			return string(g)
		}
	}
	if errors.As(err, &l) {
		if _, ok := errorMessages[string(l)]; ok {
			return string(l)
		}
	}
	if errors.Is(err, modeladmission.ErrBusy) {
		return "gateway_busy"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "model_request_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "request_cancelled"
	}
	return "gateway_unavailable"
}
func errorStatus(code string) int {
	switch code {
	case "invalid_request":
		return 400
	case "request_too_large":
		return 413
	case "device_unauthorized", "device_revoked":
		return 401
	case "model_scope_required", "model_not_allowed":
		return 403
	case "configuration_changed", "privacy_generation_changed", "request_already_accepted", "turn_binding_conflict", "request_cancelled":
		return 409
	case "context_limit_exceeded", "output_limit_exceeded":
		return 422
	case "gateway_busy":
		return 429
	case "gateway_version_unsupported":
		return 426
	case "model_unavailable", "gateway_capacity_disabled", "gateway_unavailable":
		return 503
	case "model_idle_timeout", "model_request_timeout":
		return 504
	case "request_not_found":
		return 404
	default:
		return 502
	}
}
func envelope(code string) any {
	return map[string]any{"error": map[string]string{"type": "gateway_error", "code": code, "message": errorMessages[code]}}
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeFailure(w http.ResponseWriter, err error) {
	code := errorCode(err)
	if code == "gateway_busy" {
		w.Header().Set("Retry-After", "2")
	}
	writeJSON(w, errorStatus(code), envelope(code))
}
func validID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}
func parseGeneration(r *http.Request) (int64, error) {
	if r.Header.Get("X-Model-Gateway-Version") != "1" {
		return 0, gatewayError("gateway_version_unsupported")
	}
	g, err := strconv.ParseInt(r.Header.Get("X-Learning-Generation"), 10, 64)
	if err != nil || g < 1 {
		return 0, gatewayError("invalid_request")
	}
	return g, nil
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Model-Gateway-Version", "1")
	for _, cookie := range r.Cookies() {
		if cookie.Name == "__Host-edu_web" || cookie.Name == "edu_web_dev" {
			writeFailure(w, gatewayError("device_unauthorized"))
			return
		}
	}
	if len(r.Header.Values("Authorization")) != 1 {
		writeFailure(w, gatewayError("device_unauthorized"))
		return
	}
	if r.URL.RawQuery != "" {
		writeFailure(w, gatewayError("invalid_request"))
		return
	}
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		writeFailure(w, gatewayError("device_unauthorized"))
		return
	}
	config := r.URL.Path == "/v1/model-gateway/config" && r.Method == "GET"
	scope := "model:invoke"
	if config {
		scope = ""
	}
	actor, err := s.Identity.Authenticate(r.Context(), strings.TrimPrefix(authorization, "Bearer "), scope)
	if err != nil {
		if errors.Is(err, identity.ErrForbidden) {
			err = gatewayError("model_scope_required")
		} else if errors.Is(err, identity.ErrUnauthenticated) {
			err = gatewayError("device_unauthorized")
		}
		writeFailure(w, err)
		return
	}
	if config {
		s.configHTTP(w, r, actor)
		return
	}
	g, err := parseGeneration(r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	if r.URL.Path == "/v1/model-gateway/chat/completions" && r.Method == "POST" {
		s.invokeHTTP(w, r, actor, g)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/model-gateway/requests/")
	id := strings.TrimSuffix(path, "/cancel")
	if validID(id) && r.URL.Path != "/v1/model-gateway/requests/" && (r.Method == "GET" && path == id || r.Method == "POST" && path == id+"/cancel") {
		s.stateHTTP(w, r, actor, g, id, r.Method == "POST")
		return
	}
	writeFailure(w, gatewayError("request_not_found"))
}
func (s *Service) configHTTP(w http.ResponseWriter, r *http.Request, actor identity.Credential) {
	if version := r.Header.Get("X-Model-Gateway-Version"); version != "" && version != "1" {
		writeFailure(w, gatewayError("gateway_version_unsupported"))
		return
	}
	var generation int64
	if r.Header.Get("X-Learning-Generation") != "" {
		var err error
		generation, err = parseGeneration(r)
		if err != nil {
			writeFailure(w, err)
			return
		}
	}
	s.controlResponse(w, r, actor, generation, false, func(g int64) any { return s.configuration(g, actor) })
}

func (s *Service) controlResponse(w http.ResponseWriter, r *http.Request, actor identity.Credential, g int64, invoke bool, value func(int64) any) {
	permit, err := s.Permits.Acquire(r.Context(), privacy.OwnerIdentity, privacy.OwnerLearning)
	if err != nil {
		writeFailure(w, gatewayError("privacy_generation_changed"))
		return
	}
	defer permit.Release()
	tx, current, err := s.guard(permit.Context(), actor, g, invoke)
	if err != nil {
		if invoke && errorCode(err) == "privacy_generation_changed" {
			err = gatewayError("request_not_found")
		}
		writeFailure(w, err)
		return
	}
	defer tx.Rollback(context.Background())
	err = permit.CommitResponse(func() {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
		writeJSON(w, 200, value(current))
	})
	if err != nil {
		writeFailure(w, gatewayError("privacy_generation_changed"))
	}
}
func (s *Service) stateHTTP(w http.ResponseWriter, r *http.Request, actor identity.Credential, g int64, id string, cancel bool) {
	w.Header().Set("X-CLI-Request-ID", id)
	if cancel {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1025))
		var object map[string]any
		if err != nil || len(body) > 1024 || json.Unmarshal(body, &object) != nil || object == nil || len(object) != 0 {
			writeFailure(w, gatewayError("invalid_request"))
			return
		}
	}
	tx, _, err := s.guard(r.Context(), actor, g, true)
	if err != nil {
		if errorCode(err) == "privacy_generation_changed" {
			err = gatewayError("request_not_found")
		}
		writeFailure(w, err)
		return
	}
	defer tx.Rollback(context.Background())
	if cancel {
		if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(210021)`); err == nil {
			_, err = tx.Exec(r.Context(), `INSERT INTO learning_cli_model_requests(privacy_generation,actor_device_id,request_id,state,reason,finished_at) VALUES($1,$2,$3,'cancelled','request_cancelled',clock_timestamp()) ON CONFLICT(privacy_generation,actor_device_id,request_id) DO UPDATE SET state='cancelled',reason='request_cancelled',finished_at=clock_timestamp() WHERE learning_cli_model_requests.state IN ('accepted','running')`, g, actor.Device.ID, id)
		}
		if err != nil {
			writeFailure(w, err)
			return
		}
	}
	state, err := s.state(r.Context(), tx, actor, g, id)
	if err != nil {
		writeFailure(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		writeFailure(w, err)
		return
	}
	if cancel {
		s.controlResponse(w, r, actor, g, true, func(int64) any { return map[string]string{"request_id": id, "state": state.State} })
		return
	}
	s.controlResponse(w, r, actor, g, true, func(int64) any { return state })
}

func (s *Service) invokeHTTP(w http.ResponseWriter, r *http.Request, actor identity.Credential, g int64) {
	m := metadata{Generation: g, SessionID: r.Header.Get("X-CLI-Session-ID"), TurnID: r.Header.Get("X-CLI-Turn-ID"), RequestID: r.Header.Get("X-CLI-Request-ID"), Stage: r.Header.Get("X-CLI-Model-Stage"), Configuration: r.Header.Get("X-Model-Configuration"), SpaceID: r.Header.Get("X-Learning-Space-ID")}
	if !validID(m.SessionID) || !validID(m.TurnID) || !validID(m.RequestID) || m.Configuration == "" || len(m.Configuration) > 128 || m.SpaceID != "" && !validID(m.SpaceID) {
		writeFailure(w, gatewayError("invalid_request"))
		return
	}
	w.Header().Set("X-CLI-Request-ID", m.RequestID)
	switch m.Stage {
	case "foreground", "context_observer", "context_reflector", "tool_limit_summary", "session_title", "probe":
	default:
		writeFailure(w, gatewayError("invalid_request"))
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = controller.SetWriteDeadline(time.Time{})
	limits := s.Settings.View().Limits
	request, err := llm.DecodeGatewayRequest(r.Body, limits.ContextTokens, limits.OutputTokens)
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		writeFailure(w, err)
		return
	}
	if request.Model != "teaching" && request.Model != "mentor" {
		writeFailure(w, gatewayError("model_not_allowed"))
		return
	}
	connection, err := s.Settings.ModelConnection(settings.Target(request.Model))
	if err != nil {
		writeFailure(w, gatewayError("model_unavailable"))
		return
	}
	if connection.Projection().ConfigurationRevision != m.Configuration {
		writeFailure(w, gatewayError("configuration_changed"))
		return
	}
	permit, err := s.Permits.Acquire(r.Context(), privacy.OwnerIdentity, privacy.OwnerLearning)
	if err != nil {
		writeFailure(w, gatewayError("privacy_generation_changed"))
		return
	}
	defer permit.Release()
	deadline, end := context.WithTimeout(permit.Context(), s.RequestTimeout)
	defer end()
	ctx, cancel := context.WithCancelCause(deadline)
	defer cancel(nil)
	e := &execution{service: s, actor: actor, meta: m, connection: connection}
	if err = s.register(e, cancel); err != nil {
		writeFailure(w, err)
		return
	}
	defer s.unregister(e)
	if err = s.accept(ctx, e); err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("X-Model-Usage-Task-ID", e.task)
	stopLease := modeladmission.Maintain(ctx, e.lease.Until, e.renew, cancel)
	client, err := connection.Client(func(next http.RoundTripper) http.RoundTripper { return upstreamTransport{execution: e, next: next} })
	if err != nil {
		stopLease()
		e.finish(ctx, "failed", "gateway_unavailable")
		writeFailure(w, err)
		return
	}
	type outcome struct {
		result llm.GatewayResult
		err    error
	}
	events := make(chan llm.GatewayChunk)
	result := make(chan outcome, 1)
	go func() {
		value, err := client.Gateway(ctx, request, func(c llm.GatewayChunk) error {
			select {
			case events <- c:
				return nil
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		})
		result <- outcome{value, err}
	}()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	streamStarted := false
	finalized := false
	write := func(data []byte) error {
		var writeErr error
		err := permit.CommitResponse(func() {
			tx, e2 := e.guardState(ctx, finalized)
			if e2 != nil {
				writeErr = e2
				return
			}
			defer tx.Rollback(context.Background())
			_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if request.Stream && !streamStarted {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(200)
				streamStarted = true
			}
			_, writeErr = w.Write(data)
			if writeErr == nil {
				writeErr = controller.Flush()
			}
		})
		if err != nil {
			return err
		}
		return writeErr
	}
	var out outcome
loop:
	for {
		select {
		case c := <-events:
			choices := []any{}
			if c.Usage == nil {
				choice := map[string]any{"index": 0, "delta": c.Delta}
				if c.FinishReason != "" {
					choice["finish_reason"] = c.FinishReason
				}
				choices = append(choices, choice)
			}
			frame := map[string]any{"choices": choices}
			if c.Usage != nil {
				frame["usage"] = c.Usage
			}
			raw, _ := json.Marshal(frame)
			if err = write(append(append([]byte("data: "), raw...), []byte("\n\n")...)); err != nil {
				cancel(err)
			}
		case out = <-result:
			break loop
		case <-heartbeat.C:
			if request.Stream {
				if err = write([]byte(": keepalive\n\n")); err != nil {
					cancel(err)
				}
			}
		case <-ctx.Done():
			out = <-result
			break loop
		}
	}
	stopLease()
	if cause := context.Cause(ctx); cause != nil {
		out.err = cause
	}
	if out.err == nil {
		// The upstream body is closed and its receipt settled before releasing
		// the device slot. A client receiving DONE may immediately start its next
		// tool round; final output still takes the device/privacy commit gates.
		completionState, completionReason := "succeeded", ""
		switch out.result.FinishReason {
		case "length":
			completionState, completionReason = "failed", "output_limit_exceeded"
		case "content_filter":
			completionState, completionReason = "failed", "upstream_error"
		}
		out.err = e.finish(ctx, completionState, completionReason)
		finalized = out.err == nil
	}
	if out.err == nil {
		if request.Stream {
			out.err = write([]byte("data: [DONE]\n\n"))
		} else {
			body := map[string]any{"choices": []any{map[string]any{"index": 0, "message": out.result.Message, "finish_reason": out.result.FinishReason}}}
			if len(out.result.Usage) > 0 {
				body["usage"] = out.result.Usage
			}
			raw, _ := json.Marshal(body)
			w.Header().Set("Content-Type", "application/json")
			out.err = write(raw)
		}
	}
	state, reason := "succeeded", ""
	if out.err != nil {
		reason = errorCode(out.err)
		state = "failed"
		switch reason {
		case "device_unauthorized":
			reason = "device_revoked"
			state = "cancelled"
		case "request_cancelled", "device_revoked", "privacy_generation_changed", "configuration_changed":
			state = "cancelled"
		case "gateway_unavailable", "model_idle_timeout", "model_request_timeout":
			state = "outcome_unknown"
		}
		if streamStarted {
			raw, _ := json.Marshal(envelope(reason))
			_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_, _ = w.Write(append(append([]byte("event: error\ndata: "), raw...), []byte("\n\n")...))
			_ = controller.Flush()
		} else {
			writeFailure(w, out.err)
		}
	}
	if !finalized {
		_ = e.finish(ctx, state, reason)
	}
}
