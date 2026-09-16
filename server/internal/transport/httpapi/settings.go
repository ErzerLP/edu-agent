package httpapi

import (
	"errors"
	"net/http"

	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/go-chi/chi/v5"
)

func (a *API) mountSettings(router chi.Router) {
	router.With(a.requireScope("learning:read")).Get("/v1/settings", a.readSettings)
	router.With(a.requireScope("learning:read")).Get("/v1/capabilities", a.readCapabilities)
	router.With(a.requireScope("settings:write"), a.settingsBrowser).Put("/v1/settings", a.updateSettings)
	router.With(a.requireScope("settings:probe"), a.settingsBrowser).Post("/v1/settings/probes", a.probeSettings)
}

// 敏感设置只通过受 Origin/CSRF 保护的学习会话操作，Bearer/admin Cookie 不替代它。
func (a *API) settingsBrowser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.webUI.Enabled {
			writeError(w, r, 403, "forbidden", "设置修改需要浏览器学习身份")
			return
		}
		if _, err := r.Cookie(a.webCookieName()); err != nil {
			writeError(w, r, 403, "forbidden", "设置修改需要浏览器学习身份")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) settingsService() *settings.Service {
	if a.settings != nil {
		return a.settings
	}
	s, _ := settings.Open(settings.Options{})
	return s
}
func (a *API) readSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, a.settingsService().View())
}
func (a *API) readCapabilities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	capabilities := a.settingsService().Capabilities()
	if a.mentorRuns != nil {
		mentor := a.settingsService().View().EffectiveMentor
		capabilities.WebMentor = settings.Capability{Available: mentor.Enabled && mentor.Configured, Reason: mentor.Reason}
		if capabilities.WebMentor.Available {
			capabilities.WebMentor.Reason = ""
		}
		search := a.settingsService().View().Search
		capabilities.Research = settings.Capability{Available: capabilities.WebMentor.Available && search.Enabled && search.Configured, Reason: "research_configuration_required"}
		if capabilities.Research.Available {
			capabilities.Research.Reason = ""
		}
	}
	if !a.webUI.Enabled {
		capabilities.Rendering = settings.Capability{Reason: "not_enabled"}
	}
	if a.learning == nil {
		capabilities.Persistence = settings.Capability{Reason: "not_enabled"}
	}
	writeJSON(w, 200, capabilities)
}
func (a *API) updateSettings(w http.ResponseWriter, r *http.Request) {
	var body settings.Update
	if decodeJSON(w, r, 16<<10, &body) != nil {
		writeError(w, r, 400, "invalid_settings", "配置格式或范围无效")
		return
	}
	view, err := a.settingsService().Update(body)
	if err != nil {
		a.settingsError(w, r, err)
		return
	}
	writeJSON(w, 200, view)
}
func (a *API) probeSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target           settings.Target `json:"target"`
		ExpectedRevision int64           `json:"expected_revision"`
		Consent          bool            `json:"consent"`
	}
	if decodeJSON(w, r, 1024, &body) != nil {
		writeError(w, r, 400, "invalid_settings", "探测请求无效")
		return
	}
	view, err := a.settingsService().Probe(r.Context(), body.Target, body.ExpectedRevision, body.Consent)
	if err != nil {
		a.settingsError(w, r, err)
		return
	}
	writeJSON(w, 200, view)
}
func (a *API) settingsError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := 503, "settings_storage_unavailable", "受保护的设置存储不可用，请联系服务器操作者"
	switch {
	case errors.Is(err, settings.ErrInvalid):
		status, code, message = 400, "invalid_settings", "配置格式、端点许可或预算范围无效"
	case errors.Is(err, settings.ErrConsent):
		status, code, message = 400, "probe_consent_required", "请先确认探测将发送外部请求且可能计费"
	case errors.Is(err, settings.ErrConflict):
		status, code, message = 409, "settings_conflict", "配置已变化，请重新读取后检查"
	case errors.Is(err, settings.ErrBusy):
		status, code, message = 429, "probe_busy", "已有连接探测正在进行"
	}
	writeError(w, r, status, code, message)
}
