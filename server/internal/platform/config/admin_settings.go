package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	adminSettingsVersion  = 1
	maxAdminSettingsBytes = 64 << 10
)

// NotesyncAdminSettings contains the server-owned NoteSync connection profile.
// APIToken is persisted only in the protected settings file and is never returned
// by the administration API.
type NotesyncAdminSettings struct {
	Enabled    bool      `json:"enabled"`
	BaseURL    string    `json:"base_url,omitempty"`
	APIToken   string    `json:"api_token,omitempty"`
	Vault      string    `json:"vault,omitempty"`
	PathPrefix string    `json:"path_prefix,omitempty"`
	SavedAt    time.Time `json:"saved_at"`
}

type adminSettingsFile struct {
	Version  int                   `json:"version"`
	Notesync NotesyncAdminSettings `json:"notesync"`
}

func NotesyncAdminSettingsFromConfig(cfg NotesyncConfig) NotesyncAdminSettings {
	baseURL := ""
	if cfg.BaseURL != nil {
		baseURL = cfg.BaseURL.String()
	}
	return NotesyncAdminSettings{
		Enabled: cfg.Enabled, BaseURL: baseURL, APIToken: cfg.APIToken,
		Vault: cfg.Vault, PathPrefix: cfg.PathPrefix,
	}
}

func ApplyNotesyncAdminSettings(base NotesyncConfig, settings NotesyncAdminSettings) (NotesyncConfig, error) {
	candidate := base
	candidate.Enabled = settings.Enabled
	candidate.BaseURL = nil
	candidate.APIToken = ""
	candidate.Vault = ""
	candidate.PathPrefix = "edu-agent"
	if !settings.Enabled {
		if err := validateNotesyncLimits(candidate); err != nil {
			return NotesyncConfig{}, err
		}
		return candidate, nil
	}

	baseRaw := strings.TrimSpace(settings.BaseURL)
	candidate.APIToken = settings.APIToken
	candidate.Vault = strings.TrimSpace(settings.Vault)
	candidate.PathPrefix = strings.TrimSpace(settings.PathPrefix)
	if baseRaw == "" || candidate.APIToken == "" || candidate.Vault == "" || candidate.PathPrefix == "" {
		return NotesyncConfig{}, errors.New("NoteSync enabled configuration requires base URL, API token, vault, and managed path prefix")
	}
	parsed, err := parseHTTPURL("NOTESYNC_BASE_URL", baseRaw)
	if err != nil {
		return NotesyncConfig{}, err
	}
	if parsed.RawPath != "" {
		return NotesyncConfig{}, errors.New("NOTESYNC_BASE_URL must not contain percent-encoded path segments")
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) && !base.AllowInsecureNonLoopback {
		return NotesyncConfig{}, errors.New("non-loopback NOTESYNC_BASE_URL requires HTTPS or NOTESYNC_ALLOW_INSECURE_NON_LOOPBACK=true")
	}
	if !validNotesyncToken(candidate.APIToken) {
		return NotesyncConfig{}, errors.New("NOTESYNC_API_TOKEN must contain at least 32 visible ASCII characters")
	}
	if !validNotesyncName(candidate.Vault, 255) {
		return NotesyncConfig{}, errors.New("NOTESYNC_VAULT is invalid")
	}
	if !validManagedPrefix(candidate.PathPrefix) {
		return NotesyncConfig{}, errors.New("NOTESYNC_PATH_PREFIX must be a canonical relative path")
	}
	if err := validateNotesyncLimits(candidate); err != nil {
		return NotesyncConfig{}, err
	}
	candidate.BaseURL = parsed
	return candidate, nil
}

func NotesyncConfigsEqual(left, right NotesyncConfig) bool {
	return left.Enabled == right.Enabled && notesyncURLString(left.BaseURL) == notesyncURLString(right.BaseURL) &&
		left.APIToken == right.APIToken && left.Vault == right.Vault && left.PathPrefix == right.PathPrefix &&
		left.HTTPTimeout == right.HTTPTimeout && left.WorkerInterval == right.WorkerInterval &&
		left.WorkerBatch == right.WorkerBatch && left.ScanPageSize == right.ScanPageSize && left.ScanMaxPages == right.ScanMaxPages
}

func notesyncURLString(value *url.URL) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func LoadNotesyncAdminSettings(path string) (NotesyncAdminSettings, bool, error) {
	if strings.TrimSpace(path) == "" {
		return NotesyncAdminSettings{}, false, nil
	}
	clean, err := validateAdminSettingsPath(path)
	if err != nil {
		return NotesyncAdminSettings{}, false, err
	}
	info, err := os.Lstat(clean)
	if errors.Is(err, os.ErrNotExist) {
		return NotesyncAdminSettings{}, false, nil
	}
	if err != nil {
		return NotesyncAdminSettings{}, false, fmt.Errorf("inspect admin settings: %w", err)
	}
	if err := validateAdminSettingsDirectory(filepath.Dir(clean)); err != nil {
		return NotesyncAdminSettings{}, false, err
	}
	if !info.Mode().IsRegular() {
		return NotesyncAdminSettings{}, false, errors.New("admin settings file must be a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return NotesyncAdminSettings{}, false, errors.New("admin settings file mode must be 0600")
	}
	file, err := os.Open(clean)
	if err != nil {
		return NotesyncAdminSettings{}, false, fmt.Errorf("open admin settings: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return NotesyncAdminSettings{}, false, fmt.Errorf("inspect opened admin settings: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Mode().Perm() != 0o600 || !os.SameFile(info, openedInfo) {
		return NotesyncAdminSettings{}, false, errors.New("admin settings file changed while it was being opened")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maxAdminSettingsBytes+1))
	if err != nil {
		return NotesyncAdminSettings{}, false, fmt.Errorf("read admin settings: %w", err)
	}
	if len(payload) > maxAdminSettingsBytes {
		return NotesyncAdminSettings{}, false, errors.New("admin settings file exceeds the supported size")
	}
	var stored adminSettingsFile
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return NotesyncAdminSettings{}, false, errors.New("admin settings file is invalid")
	}
	if err := ensureJSONEOF(decoder); err != nil || stored.Version != adminSettingsVersion {
		return NotesyncAdminSettings{}, false, errors.New("admin settings file is invalid")
	}
	return stored.Notesync, true, nil
}

func SaveNotesyncAdminSettings(path string, settings NotesyncAdminSettings) error {
	clean, err := validateAdminSettingsPath(path)
	if err != nil {
		return err
	}
	directory := filepath.Dir(clean)
	if err := ensureAdminSettingsDirectory(directory); err != nil {
		return err
	}
	if info, statErr := os.Lstat(clean); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return errors.New("admin settings destination must be a regular file with mode 0600")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect admin settings destination: %w", statErr)
	}

	settings.SavedAt = settings.SavedAt.UTC().Truncate(time.Microsecond)
	payload, err := json.MarshalIndent(adminSettingsFile{Version: adminSettingsVersion, Notesync: settings}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode admin settings: %w", err)
	}
	payload = append(payload, '\n')
	temporary, err := os.CreateTemp(directory, ".admin-settings-*")
	if err != nil {
		return fmt.Errorf("create admin settings temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect admin settings temporary file: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write admin settings: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync admin settings: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close admin settings: %w", err)
	}
	if err := os.Rename(temporaryPath, clean); err != nil {
		return fmt.Errorf("replace admin settings: %w", err)
	}
	removeTemporary = false
	dir, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open admin settings directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync admin settings directory: %w", err)
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON content")
	}
	return nil
}

func validateAdminSettingsPath(value string) (string, error) {
	if strings.TrimSpace(value) != value || value == "" || !filepath.IsAbs(value) {
		return "", errors.New("ADMIN_UI_SETTINGS_FILE must be an absolute canonical path")
	}
	clean := filepath.Clean(value)
	if clean != value || filepath.Base(clean) == "." || filepath.Base(clean) == string(filepath.Separator) {
		return "", errors.New("ADMIN_UI_SETTINGS_FILE must be an absolute canonical path")
	}
	return clean, nil
}

func ensureAdminSettingsDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create admin settings directory: %w", err)
	}
	return validateAdminSettingsDirectory(directory)
}

func validateAdminSettingsDirectory(directory string) error {
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("resolve admin settings directory: %w", err)
	}
	if resolved != directory {
		return errors.New("admin settings directory must not contain symbolic links")
	}
	info, err := os.Stat(directory)
	if err != nil {
		return fmt.Errorf("inspect admin settings directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("admin settings parent must be a directory")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("admin settings directory must not be writable by group or other users")
	}
	return nil
}
