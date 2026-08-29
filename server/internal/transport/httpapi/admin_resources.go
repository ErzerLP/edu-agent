package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/notesync"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
)

type adminNotesyncSettingsRequest struct {
	Enabled    bool   `json:"enabled"`
	BaseURL    string `json:"base_url"`
	APIToken   string `json:"api_token"`
	Vault      string `json:"vault"`
	PathPrefix string `json:"path_prefix"`
}

type adminNotesyncConnectionView struct {
	Enabled             bool      `json:"enabled"`
	BaseURL             string    `json:"base_url"`
	APIKeyConfigured    bool      `json:"api_key_configured"`
	Vault               string    `json:"vault"`
	PathPrefix          string    `json:"path_prefix"`
	SavedAt             time.Time `json:"saved_at,omitempty"`
	ConfigurationSource string    `json:"configuration_source"`
}

type adminNotesyncView struct {
	Active           adminNotesyncConnectionView `json:"active"`
	Saved            adminNotesyncConnectionView `json:"saved"`
	Runtime          notesync.ReviewStatus       `json:"runtime"`
	RestartRequired  bool                        `json:"restart_required"`
	SettingsWritable bool                        `json:"settings_writable"`
}

func (a *API) adminMemory(w http.ResponseWriter, r *http.Request) {
	if a.memoryExporter == nil {
		writeError(w, r, http.StatusServiceUnavailable, "memory_unavailable", "Memory service is unavailable")
		return
	}
	limit := 200
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		parsed, parseErr := strconv.Atoi(rawLimit)
		if parseErr != nil || parsed < 1 || parsed > 200 {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Memory page limit must be between 1 and 200")
			return
		}
		limit = parsed
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if len(cursor) > 4096 {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Memory cursor is too long")
		return
	}
	page, err := a.memoryExporter.Export(r.Context(), memory.PageRequest{Cursor: cursor, Limit: limit})
	if err != nil {
		a.logger.ErrorContext(r.Context(), "admin memory export failed", "error", err)
		writeError(w, r, http.StatusServiceUnavailable, "memory_unavailable", "Memory tree could not be loaded")
		return
	}
	if page.Items == nil {
		page.Items = []memory.ExportItem{}
	}
	if page.ReasonCodes == nil {
		page.ReasonCodes = []string{}
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *API) adminKnowledge(w http.ResponseWriter, r *http.Request) {
	if a.knowledge == nil {
		writeError(w, r, http.StatusServiceUnavailable, "knowledge_unavailable", "Knowledge service is unavailable")
		return
	}
	head, err := a.knowledge.Head(r.Context())
	if err != nil {
		a.writeAdminKnowledgeFailure(w, r, "head", err)
		return
	}
	if head == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"head": nil, "tree": nil, "export": knowledge.ExportResult{Documents: []knowledge.ExportDocument{}},
		})
		return
	}
	tree, err := a.knowledge.Tree(r.Context(), head.ID)
	if err != nil {
		a.writeAdminKnowledgeFailure(w, r, "tree", err)
		return
	}
	exported, err := a.knowledge.Export(r.Context(), head.ID)
	if err != nil {
		a.writeAdminKnowledgeFailure(w, r, "export", err)
		return
	}
	if exported.Documents == nil {
		exported.Documents = []knowledge.ExportDocument{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"head": head, "tree": tree, "export": exported})
}

func (a *API) writeAdminKnowledgeFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	code := knowledge.ErrorCode(err)
	if code == knowledge.CodeContentRedacted {
		writeError(w, r, http.StatusServiceUnavailable, code, "Knowledge content is unavailable")
		return
	}
	a.logger.ErrorContext(r.Context(), "admin knowledge read failed", "operation", operation, "error", err)
	writeError(w, r, http.StatusInternalServerError, "internal_error", "Knowledge library could not be loaded")
}

func (a *API) adminNotesync(w http.ResponseWriter, r *http.Request) {
	view, err := a.adminNotesyncSettingsView(r.Context())
	if err != nil {
		a.logger.ErrorContext(r.Context(), "admin NoteSync settings read failed", "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "NoteSync settings could not be loaded")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) adminUpdateNotesync(w http.ResponseWriter, r *http.Request) {
	if !a.adminUI.WriteLimiter.Allow("admin-write:" + clientIP(r)) {
		writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many admin changes")
		return
	}
	if a.adminUI.SettingsFile == "" {
		writeError(w, r, http.StatusServiceUnavailable, "settings_unavailable", "Server settings persistence is not configured")
		return
	}
	var request adminNotesyncSettingsRequest
	if err := decodeJSON(w, r, 8<<10, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "NoteSync settings are invalid")
		return
	}
	if len(request.APIToken) > 4096 {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "NoteSync settings are invalid")
		return
	}

	a.adminSettingsMu.Lock()
	defer a.adminSettingsMu.Unlock()

	previous, found, err := config.LoadNotesyncAdminSettings(a.adminUI.SettingsFile)
	if err != nil {
		a.logger.ErrorContext(r.Context(), "admin NoteSync settings read before write failed", "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "NoteSync settings could not be saved")
		return
	}
	var preserved config.NotesyncAdminSettings
	if found && previous.APIToken != "" {
		preserved = previous
	} else if a.adminUI.Notesync.APIToken != "" && a.adminUI.Notesync.BaseURL != nil {
		preserved = config.NotesyncAdminSettings{
			APIToken: a.adminUI.Notesync.APIToken,
			BaseURL:  a.adminUI.Notesync.BaseURL.String(),
		}
	}
	settings := config.NotesyncAdminSettings{
		Enabled: request.Enabled, BaseURL: strings.TrimSpace(request.BaseURL),
		APIToken: request.APIToken, Vault: strings.TrimSpace(request.Vault),
		PathPrefix: strings.TrimSpace(request.PathPrefix), SavedAt: a.now().UTC(),
	}
	if !settings.Enabled {
		settings.BaseURL, settings.APIToken, settings.Vault, settings.PathPrefix = "", "", "", ""
	} else if settings.APIToken == "" && settings.BaseURL == preserved.BaseURL {
		settings.APIToken = preserved.APIToken
	} else if settings.APIToken == "" && preserved.APIToken != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "a new NoteSync API token is required when changing the service address")
		return
	}
	if sameAdminSecret(settings.APIToken, a.adminUI.Token) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "NoteSync API token must differ from the management password")
		return
	}
	if _, err := config.ApplyNotesyncAdminSettings(a.adminUI.Notesync, settings); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := config.SaveNotesyncAdminSettings(a.adminUI.SettingsFile, settings); err != nil {
		a.logger.ErrorContext(r.Context(), "admin NoteSync settings write failed", "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "NoteSync settings could not be saved")
		return
	}
	view, err := a.adminNotesyncSettingsViewLocked(r.Context())
	if err != nil {
		a.logger.ErrorContext(r.Context(), "admin NoteSync settings reload failed", "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "NoteSync settings were saved but could not be reloaded")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) adminNotesyncPreview(w http.ResponseWriter, r *http.Request) {
	if !a.adminUI.WriteLimiter.Allow("admin-write:" + clientIP(r)) {
		writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many admin changes")
		return
	}
	if a.notesync == nil {
		writeError(w, r, http.StatusConflict, notesyncNotConfigured, "NoteSync is not active; restart the server after saving an enabled profile")
		return
	}
	var request notesyncPreviewRequest
	if err := decodeJSON(w, r, a.maxKnowledgeRequestBody, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, notesync.CodeReviewInvalidRequest, "NoteSync preview request is invalid")
		return
	}
	pageSize := request.PageSize
	maxPageSize := notesync.MaxPreviewPageSize
	if configured := a.adminUI.Notesync.ScanPageSize; configured > 0 && configured < maxPageSize {
		maxPageSize = configured
	}
	if pageSize <= 0 || pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	result, err := a.notesync.Preview(r.Context(), notesync.PreviewCommand{Path: request.Path, Page: request.Page, PageSize: pageSize})
	if err != nil {
		a.writeNotesyncFailure(w, r, "admin_preview", err)
		return
	}
	if result.Items == nil {
		result.Items = []notesync.PreviewItem{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) adminNotesyncReviews(w http.ResponseWriter, r *http.Request) {
	if a.notesync == nil {
		writeJSON(w, http.StatusOK, notesync.ReviewPage{Items: []notesync.ReviewSummary{}})
		return
	}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if len(cursor) > 4096 {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "NoteSync review cursor is too long")
		return
	}
	result, err := a.notesync.ListReviews(r.Context(), notesync.ReviewListCommand{
		Status: r.URL.Query().Get("status"), Cursor: cursor, Limit: notesync.MaxReviewPageSize,
	})
	if err != nil {
		a.writeNotesyncFailure(w, r, "admin_reviews", err)
		return
	}
	if result.Items == nil {
		result.Items = []notesync.ReviewSummary{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) adminNotesyncSettingsView(ctx context.Context) (adminNotesyncView, error) {
	a.adminSettingsMu.Lock()
	defer a.adminSettingsMu.Unlock()
	return a.adminNotesyncSettingsViewLocked(ctx)
}

func (a *API) adminNotesyncSettingsViewLocked(ctx context.Context) (adminNotesyncView, error) {
	activeSettings := config.NotesyncAdminSettingsFromConfig(a.adminUI.Notesync)
	activeSettings.SavedAt = a.adminUI.NotesyncSettingsSavedAt
	activeSource := a.adminUI.NotesyncSource
	if activeSource == "" {
		activeSource = "environment"
	}
	active := adminNotesyncConnection(activeSettings, activeSource)
	saved := active
	restartRequired := false
	if a.adminUI.SettingsFile != "" {
		pending, found, err := config.LoadNotesyncAdminSettings(a.adminUI.SettingsFile)
		if err != nil {
			return adminNotesyncView{}, err
		}
		if found {
			pendingConfig, err := config.ApplyNotesyncAdminSettings(a.adminUI.Notesync, pending)
			if err != nil {
				return adminNotesyncView{}, err
			}
			saved = adminNotesyncConnection(pending, "admin_settings")
			restartRequired = !config.NotesyncConfigsEqual(a.adminUI.Notesync, pendingConfig)
		}
	}
	runtimeStatus := notesync.ReviewStatus{Configured: false, Compatible: false, Reason: "not_configured", ExternalCleanupRequired: true}
	if a.notesync != nil {
		runtimeStatus = a.notesync.Status(ctx)
	}
	return adminNotesyncView{
		Active: active, Saved: saved, Runtime: runtimeStatus,
		RestartRequired: restartRequired, SettingsWritable: a.adminUI.SettingsFile != "",
	}, nil
}

func adminNotesyncConnection(settings config.NotesyncAdminSettings, source string) adminNotesyncConnectionView {
	return adminNotesyncConnectionView{
		Enabled: settings.Enabled, BaseURL: settings.BaseURL, APIKeyConfigured: settings.APIToken != "",
		Vault: settings.Vault, PathPrefix: settings.PathPrefix, SavedAt: settings.SavedAt,
		ConfigurationSource: source,
	}
}

func sameAdminSecret(left, right string) bool {
	return left != "" && len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
