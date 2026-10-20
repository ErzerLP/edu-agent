package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

const (
	DefaultServerURL              = "http://127.0.0.1:8080"
	DefaultTimeout                = 30 * time.Second
	DefaultColor                  = "never"
	DefaultAgentContextCompaction = "auto"
	DefaultAgentReasoningEffort   = "auto"
	DefaultAgentSessionHistory    = "auto"
)

var (
	ErrNotFound        = errors.New("configuration was not found")
	ErrJournalNotFound = errors.New("pairing journal was not found")
)

type UnknownFieldError struct {
	ConfigPath string
	FieldPath  string
}

func (e *UnknownFieldError) Error() string {
	return fmt.Sprintf("配置文件 %q 包含未知字段 %q：该设置已移除，请从配置文件中删除这一项", e.ConfigPath, e.FieldPath)
}

type Config struct {
	ServerURL         string          `json:"server_url"`
	DeviceID          string          `json:"device_id"`
	DisplayName       string          `json:"display_name"`
	Timeout           string          `json:"timeout"`
	Color             string          `json:"color"`
	AllowInsecureHTTP bool            `json:"allow_insecure_http"`
	Offline           *OfflineBinding `json:"offline,omitempty"`
	Gateway           *GatewayState   `json:"gateway,omitempty"`
	Agent             *AgentConfig    `json:"agent,omitempty"`
}

type AgentConfig struct {
	ContextCompaction string `json:"context_compaction,omitempty"`
	ReasoningEffort   string `json:"reasoning_effort,omitempty"`
	SessionHistory    string `json:"session_history,omitempty"`
}

// GatewayState is bound to a paired server and device. Online limits are never cached.
type GatewayState struct {
	ServerURL           string `json:"server_url"`
	DeviceID            string `json:"device_id"`
	SelectedTarget      string `json:"selected_target,omitempty"`
	DestinationRevision string `json:"destination_revision,omitempty"`
	NoticeVersion       int    `json:"notice_version"`
}

type OfflineBinding struct {
	ProtocolVersion   int             `json:"protocol_version"`
	LearnerGeneration string          `json:"learner_generation"`
	ServerBaseURL     string          `json:"server_base_url"`
	SignerManifest    json.RawMessage `json:"signer_manifest"`
}

type PairingJournal struct {
	SchemaVersion int    `json:"schema_version"`
	ServerURL     string `json:"server_url,omitempty"`
	DeviceID      string `json:"device_id,omitempty"`
	DisplayName   string `json:"display_name,omitempty"`
}

type Store struct{ Path string }

func DefaultStore() (Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return Store{}, fmt.Errorf("locate user configuration: %w", err)
	}
	return Store{Path: filepath.Join(dir, "edu-agent", "config.json")}, nil
}

func (s Store) Load() (Config, error) {
	data, err := securefile.Read(s.Path, true)
	if errors.Is(err, securefile.ErrNotFound) {
		return Config{}, ErrNotFound
	}
	if err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}
	var value Config
	if err := decodeStrict(data, &value); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			if path := unknownFieldPath(data, reflect.TypeOf(value)); path != "" {
				return Config{}, &UnknownFieldError{ConfigPath: s.Path, FieldPath: path}
			}
		}
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	if err := value.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate configuration: %w", err)
	}
	return value, nil
}

func (s Store) Save(value Config) error {
	if err := value.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	data = append(data, '\n')
	if err := securefile.AtomicWrite(s.Path, data, true); err != nil {
		return fmt.Errorf("save configuration: %w", err)
	}
	return nil
}

func (s Store) Delete() error { return securefile.Delete(s.Path) }

func (s Store) LoadPairingJournal() (PairingJournal, error) {
	data, err := securefile.Read(s.pairingJournalPath(), true)
	if errors.Is(err, securefile.ErrNotFound) {
		return PairingJournal{}, ErrJournalNotFound
	}
	if err != nil {
		return PairingJournal{}, fmt.Errorf("read pairing journal: %w", err)
	}
	var value PairingJournal
	if err := decodeStrict(data, &value); err != nil || value.SchemaVersion != 1 {
		return PairingJournal{}, errors.New("pairing journal is invalid")
	}
	return value, nil
}

func (s Store) SavePairingJournal(value PairingJournal) error {
	value.SchemaVersion = 1
	data, err := json.Marshal(value)
	if err != nil {
		return errors.New("encode pairing journal")
	}
	if err := securefile.AtomicWrite(s.pairingJournalPath(), append(data, '\n'), true); err != nil {
		return fmt.Errorf("save pairing journal: %w", err)
	}
	return nil
}

func (s Store) DeletePairingJournal() error {
	path := s.pairingJournalPath()
	data, readErr := securefile.Read(path, true)
	if errors.Is(readErr, securefile.ErrNotFound) {
		return nil
	}
	if readErr != nil {
		return fmt.Errorf("read pairing journal before delete: %w", readErr)
	}
	if err := securefile.Delete(path); err != nil {
		if restoreErr := securefile.AtomicWrite(path, data, true); restoreErr != nil {
			return fmt.Errorf("delete pairing journal: %v; restore fail-closed marker: %w", err, restoreErr)
		}
		return fmt.Errorf("delete pairing journal: %w", err)
	}
	return nil
}

func (s Store) pairingJournalPath() string {
	return filepath.Join(filepath.Dir(s.Path), "pairing-pending.json")
}

func (c *Config) Validate() error {
	if c.HasPairingBinding() {
		if strings.TrimSpace(c.ServerURL) == "" || strings.TrimSpace(c.DeviceID) == "" || strings.TrimSpace(c.DisplayName) == "" {
			return errors.New("server URL, device ID, and display name must be configured together")
		}
		normalized, err := ValidateServerURL(c.ServerURL, c.AllowInsecureHTTP)
		if err != nil {
			return err
		}
		c.ServerURL = normalized
	} else if c.Offline != nil {
		return errors.New("offline binding requires a paired device")
	}
	if c.Timeout == "" {
		c.Timeout = DefaultTimeout.String()
	}
	if _, err := ParseTimeout(c.Timeout); err != nil {
		return err
	}
	if c.Color == "" {
		c.Color = DefaultColor
	}
	if !validColor(c.Color) {
		return errors.New("color must be never, auto, or always")
	}
	if c.Offline != nil {
		if err := c.Offline.Validate(c.ServerURL); err != nil {
			return err
		}
	}
	if c.Gateway != nil {
		g := c.Gateway
		if !c.HasPairingBinding() || g.ServerURL != c.ServerURL || g.DeviceID != c.DeviceID {
			return errors.New("gateway state does not match paired server and device")
		}
		if g.SelectedTarget != "" && g.SelectedTarget != "teaching" && g.SelectedTarget != "mentor" {
			return errors.New("invalid gateway target")
		}
		if g.NoticeVersion < 0 || len(g.DestinationRevision) > 512 || strings.ContainsAny(g.DestinationRevision, "\r\n\x00") {
			return errors.New("invalid gateway consent")
		}
	}
	if c.Agent != nil {
		if err := c.Agent.Validate(); err != nil {
			return fmt.Errorf("agent configuration: %w", err)
		}
	}
	return nil
}

// HasPairingBinding reports whether any server/device-owned binding field is present.
func (c Config) HasPairingBinding() bool {
	return strings.TrimSpace(c.ServerURL) != "" || strings.TrimSpace(c.DeviceID) != "" || strings.TrimSpace(c.DisplayName) != "" || c.Offline != nil
}

// WithoutPairing preserves local client preferences while removing server-owned binding state.
func (c Config) WithoutPairing() Config {
	c.ServerURL = ""
	c.DeviceID = ""
	c.DisplayName = ""
	c.AllowInsecureHTTP = false
	c.Offline = nil
	c.Gateway = nil
	return c
}

func DefaultAgentConfig() AgentConfig {
	return AgentConfig{ContextCompaction: DefaultAgentContextCompaction, ReasoningEffort: DefaultAgentReasoningEffort, SessionHistory: DefaultAgentSessionHistory}
}

func (c *AgentConfig) Validate() error {
	c.ContextCompaction = strings.ToLower(strings.TrimSpace(c.ContextCompaction))
	if c.ContextCompaction == "" {
		c.ContextCompaction = DefaultAgentContextCompaction
	}
	switch c.ContextCompaction {
	case "auto", "recent-only", "off":
	default:
		return errors.New("context compaction must be auto, recent-only, or off")
	}
	c.ReasoningEffort = strings.ToLower(strings.TrimSpace(c.ReasoningEffort))
	if c.ReasoningEffort == "" {
		c.ReasoningEffort = DefaultAgentReasoningEffort
	}
	switch c.ReasoningEffort {
	case "auto", "none", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return errors.New("reasoning effort must be auto, none, minimal, low, medium, high, xhigh, or max")
	}
	c.SessionHistory = strings.ToLower(strings.TrimSpace(c.SessionHistory))
	if c.SessionHistory == "" {
		c.SessionHistory = DefaultAgentSessionHistory
	}
	if c.SessionHistory != "auto" && c.SessionHistory != "off" {
		return errors.New("session history must be auto or off")
	}
	return nil
}

func (b *OfflineBinding) Validate(serverURL string) error {
	if b.ProtocolVersion != 1 {
		return errors.New("offline binding protocol version is unsupported")
	}
	if b.LearnerGeneration == "" || (len(b.LearnerGeneration) > 1 && b.LearnerGeneration[0] == '0') {
		return errors.New("offline learner generation is invalid")
	}
	generation, err := strconv.ParseUint(b.LearnerGeneration, 10, 63)
	if err != nil || generation == 0 {
		return errors.New("offline learner generation is invalid")
	}
	normalizedOrigin, err := ValidateServerURL(b.ServerBaseURL, strings.HasPrefix(serverURL, "http://"))
	if err != nil || normalizedOrigin != serverURL {
		return errors.New("offline server origin does not match the paired server")
	}
	b.ServerBaseURL = normalizedOrigin
	if len(b.SignerManifest) == 0 || !json.Valid(b.SignerManifest) {
		return errors.New("offline signer manifest is invalid")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(b.SignerManifest, &object); err != nil || len(object) == 0 {
		return errors.New("offline signer manifest is invalid")
	}
	return nil
}

func ParseTimeout(value string) (time.Duration, error) {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || duration <= 0 {
		return 0, errors.New("timeout must be a positive duration")
	}
	return duration, nil
}

func ValidateServerURL(raw string, allowInsecure bool) (string, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" {
		return "", errors.New("server URL must be an absolute HTTP or HTTPS URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("server URL scheme must be http or https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("server URL must not contain credentials, query, or fragment")
	}
	if strings.Contains(parsed.Host, "\\") {
		return "", errors.New("server URL host is invalid")
	}
	host := parsed.Hostname()
	if host == "" {
		return "", errors.New("server URL host is required")
	}
	if parsed.Scheme == "http" && !isLoopback(host) && !allowInsecure {
		return "", errors.New("non-loopback HTTP requires explicit insecure approval")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	parsed.RawPath = strings.TrimSuffix(parsed.RawPath, "/")
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type Overrides struct {
	ServerURL         string
	Timeout           string
	Color             string
	AllowInsecureHTTP *bool
}

func Resolve(base Config, explicit Overrides, getenv func(string) string) (Config, error) {
	resolved := base
	if value := strings.TrimSpace(getenv("EDU_AGENT_SERVER")); value != "" {
		resolved.ServerURL = value
	}
	if value := strings.TrimSpace(getenv("EDU_AGENT_TIMEOUT")); value != "" {
		resolved.Timeout = value
	}
	if value := strings.TrimSpace(getenv("EDU_AGENT_COLOR")); value != "" {
		resolved.Color = value
	}
	if value := strings.TrimSpace(getenv("EDU_AGENT_ALLOW_INSECURE_HTTP")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, errors.New("EDU_AGENT_ALLOW_INSECURE_HTTP must be true or false")
		}
		resolved.AllowInsecureHTTP = parsed
	}
	if explicit.ServerURL != "" {
		resolved.ServerURL = explicit.ServerURL
	}
	if explicit.Timeout != "" {
		resolved.Timeout = explicit.Timeout
	}
	if explicit.Color != "" {
		resolved.Color = explicit.Color
	}
	if explicit.AllowInsecureHTTP != nil {
		resolved.AllowInsecureHTTP = *explicit.AllowInsecureHTTP
	}
	if resolved.ServerURL == "" {
		resolved.ServerURL = DefaultServerURL
	}
	if resolved.Timeout == "" {
		resolved.Timeout = DefaultTimeout.String()
	}
	if resolved.Color == "" {
		resolved.Color = DefaultColor
	}
	if resolved.DeviceID == "" || resolved.DisplayName == "" {
		normalized, err := ValidateServerURL(resolved.ServerURL, resolved.AllowInsecureHTTP)
		if err != nil {
			return Config{}, err
		}
		resolved.ServerURL = normalized
		if _, err := ParseTimeout(resolved.Timeout); err != nil {
			return Config{}, err
		}
		if !validColor(resolved.Color) {
			return Config{}, errors.New("color must be never, auto, or always")
		}
		return resolved, nil
	}
	return resolved, resolved.Validate()
}

func InsecureWarning(value Config) string {
	parsed, err := url.Parse(value.ServerURL)
	if err == nil && parsed.Scheme == "http" && !isLoopback(parsed.Hostname()) && value.AllowInsecureHTTP {
		return "warning[insecure_http]: connection uses plaintext HTTP to a non-loopback server"
	}
	return ""
}

func validColor(value string) bool { return value == "never" || value == "auto" || value == "always" }

// Recover the full path omitted by encoding/json's unknown-field diagnostic.
// Walk object keys in file order using the same JSON tags and case folding.
func unknownFieldPath(data []byte, schema reflect.Type) string {
	for schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	if schema.Kind() != reflect.Struct {
		return ""
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return ""
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		key := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return ""
		}
		var fieldType reflect.Type
		for index := 0; index < schema.NumField(); index++ {
			field := schema.Field(index)
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if strings.EqualFold(key, name) {
				fieldType = field.Type
				break
			}
		}
		if fieldType == nil {
			return key
		}
		if nested := unknownFieldPath(value, fieldType); nested != "" {
			return key + "." + nested
		}
	}
	return ""
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("configuration contains multiple JSON values")
		}
		return err
	}
	return nil
}
