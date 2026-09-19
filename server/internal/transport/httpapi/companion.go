package httpapi

import (
	"net/http"
	"strconv"

	"github.com/edu-agent/edu-agent/packages/agentcore/companion"
	"github.com/edu-agent/edu-agent/server/internal/identity"
)

func companionFailure(w http.ResponseWriter, r *http.Request, err error) {
	status := 403
	if err == companion.ErrLimit {
		status = 429
	}
	if err == companion.ErrConflict {
		status = 409
	}
	writeError(w, r, status, "companion_unavailable", "本地连接不可用、未授权、操作冲突或容量不足；请查询原操作，不要重复执行")
}

func (a *API) companionOrigin(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if !a.webOriginOK(r) || r.Header.Get("Authorization") != "" || r.URL.RawQuery != "" {
		companionFailure(w, r, companion.ErrDenied)
		return false
	}
	return true
}

func (a *API) companionAttach(w http.ResponseWriter, r *http.Request) {
	if !a.companionOrigin(w, r) {
		return
	}
	if len(r.Cookies()) != 0 || !a.pairLimiter.Allow("companion:"+clientIP(r)) {
		companionFailure(w, r, companion.ErrDenied)
		return
	}
	var command companion.Attach
	if decodeJSON(w, r, 8192, &command) != nil {
		companionFailure(w, r, companion.ErrDenied)
		return
	}
	result, err := a.companion.Attach(command)
	if err != nil {
		companionFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) companionChannel(w http.ResponseWriter, r *http.Request) {
	if !a.companionOrigin(w, r) {
		return
	}
	if len(r.Cookies()) != 0 || len(r.Header.Values("X-Companion-ID")) != 1 || len(r.Header.Values("X-Companion-Sequence")) != 1 || len(r.Header.Values("X-Companion-MAC")) != 1 {
		companionFailure(w, r, companion.ErrDenied)
		return
	}
	id := r.Header.Get("X-Companion-ID")
	sequence, err := strconv.ParseUint(r.Header.Get("X-Companion-Sequence"), 10, 64)
	if err != nil {
		companionFailure(w, r, companion.ErrDenied)
		return
	}
	body, err := readJSONBody(w, r, companion.MaxPayload)
	if err != nil {
		companionFailure(w, r, companion.ErrDenied)
		return
	}
	if !a.companion.Verify(id, sequence, r.Header.Get("X-Companion-MAC"), body) {
		companionFailure(w, r, companion.ErrDenied)
		return
	}
	owner, device, grant, err := a.companion.Principal(id)
	if err != nil || a.mentorRuns == nil {
		companionFailure(w, r, companion.ErrDenied)
		return
	}
	if err = a.mentorRuns.CheckCompanion(r.Context(), identity.Credential{TokenID: owner, Device: identity.Device{ID: device}}, grant); err != nil {
		a.companion.RevokeChannel(id)
		companionFailure(w, r, companion.ErrDenied)
		return
	}
	result, err := a.companion.Exchange(id, sequence, r.Header.Get("X-Companion-MAC"), body)
	if err != nil {
		companionFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}

type companionBrowserRequest struct {
	Action      string               `json:"action"`
	ID          string               `json:"id,omitempty"`
	Secret      string               `json:"secret,omitempty"`
	Device      string               `json:"device,omitempty"`
	Grant       *companion.Grant     `json:"grant,omitempty"`
	Operation   *companion.Operation `json:"operation,omitempty"`
	OperationID string               `json:"operation_id,omitempty"`
}

func (a *API) companionBrowser(w http.ResponseWriter, r *http.Request) {
	if !a.companionOrigin(w, r) {
		return
	}
	if _, err := r.Cookie(a.webCookieName()); err != nil {
		companionFailure(w, r, companion.ErrDenied)
		return
	}
	var c companionBrowserRequest
	if decodeJSON(w, r, companion.MaxPayload, &c) != nil {
		companionFailure(w, r, companion.ErrDenied)
		return
	}
	if c.Action == "capabilities" {
		writeJSON(w, 200, map[string]bool{"enabled": a.companion != nil})
		return
	}
	if a.companion == nil {
		writeError(w, r, 404, "companion_disabled", "服务器未启用本地连接；线上学习、参考资料和导师对话仍可使用")
		return
	}
	actor, _ := credentialFromContext(r.Context())
	var result any
	var err error
	if c.Action != "revoke" {
		if a.mentorRuns == nil {
			companionFailure(w, r, companion.ErrDenied)
			return
		}
		var grant *companion.Grant
		if c.Action != "pair" {
			view, e := a.companion.View(actor.TokenID, c.ID, c.Secret)
			if e != nil {
				companionFailure(w, r, e)
				return
			}
			grant = view.Grant
		}
		if e := a.mentorRuns.CheckCompanion(r.Context(), actor, grant); e != nil {
			if c.ID != "" {
				_ = a.companion.Revoke(actor.TokenID, c.ID, c.Secret)
			}
			companionFailure(w, r, companion.ErrDenied)
			return
		}
	}
	switch c.Action {
	case "pair":
		result, err = a.companion.Create(actor.TokenID, actor.Device.ID)
	case "status":
		result, err = a.companion.View(actor.TokenID, c.ID, c.Secret)
	case "revoke":
		err = a.companion.Revoke(actor.TokenID, c.ID, c.Secret)
		result = map[string]bool{"revoked": err == nil}
	case "grant":
		if c.Grant == nil || a.mentorRuns == nil {
			err = companion.ErrDenied
			break
		}
		err = a.mentorRuns.CheckCompanion(r.Context(), actor, c.Grant)
		if err == nil {
			err = a.companion.Authorize(actor.TokenID, c.ID, c.Secret, c.Device, *c.Grant)
		}
		result = map[string]bool{"authorized": err == nil}
	case "submit":
		if c.Operation == nil || a.mentorRuns == nil {
			err = companion.ErrDenied
			break
		}
		var view companion.View
		view, err = a.companion.View(actor.TokenID, c.ID, c.Secret)
		if err != nil {
			break
		}
		if view.Grant == nil {
			err = companion.ErrDenied
			break
		}
		err = a.mentorRuns.CheckCompanion(r.Context(), actor, view.Grant)
		if err != nil {
			break
		}
		// 手动操作使用自己的回执身份，客户端不能冒充另一个模型运行。
		c.Operation.Run = "manual:" + c.Operation.ID
		result, err = a.companion.Submit(actor.TokenID, c.ID, c.Secret, *c.Operation)
	case "receipt":
		result, err = a.companion.Receipt(actor.TokenID, c.ID, c.Secret, c.OperationID)
	default:
		err = companion.ErrDenied
	}
	if err != nil {
		companionFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
