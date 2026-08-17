package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultMinimumContextWindow = 4096

type Config struct {
	ListenAddr                 string
	PublicBaseURL              *url.URL
	DatabaseURL                string
	MigrateOnStart             bool
	AllowInsecureNonLoopback   bool
	InsecureNonLoopbackWarning bool
	ShutdownTimeout            time.Duration
	PairingCodeTTL             time.Duration
	PairingCodeMaxAttempts     int
	TokenLastUsedTouchInterval time.Duration
	PairingRateLimitPerMinute  int
	AuthFailureLimitPerMinute  int
	Model                      ModelConfig
}

type ModelConfig struct {
	Enabled        bool
	Required       bool
	BaseURL        *url.URL
	Name           string
	APIKey         string
	ContextWindow  int
	MinimumContext int
	Timeout        time.Duration
	ProbeCacheTTL  time.Duration
}

type envReader func(string) (string, bool)

func Load() (Config, error) {
	return load(os.LookupEnv)
}

func load(lookup envReader) (Config, error) {
	cfg := Config{
		ListenAddr:                 "127.0.0.1:8080",
		MigrateOnStart:             true,
		ShutdownTimeout:            10 * time.Second,
		PairingCodeTTL:             10 * time.Minute,
		PairingCodeMaxAttempts:     5,
		TokenLastUsedTouchInterval: 5 * time.Minute,
		PairingRateLimitPerMinute:  10,
		AuthFailureLimitPerMinute:  20,
		Model: ModelConfig{
			MinimumContext: DefaultMinimumContextWindow,
			Timeout:        30 * time.Second,
			ProbeCacheTTL:  15 * time.Minute,
		},
	}

	var err error
	if cfg.ListenAddr, err = stringValue(lookup, "LISTEN_ADDR", cfg.ListenAddr); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseURL, err = requiredString(lookup, "DATABASE_URL"); err != nil {
		return Config{}, err
	}
	if err := validatePostgresURL(cfg.DatabaseURL); err != nil {
		return Config{}, fmt.Errorf("DATABASE_URL: %w", err)
	}
	if cfg.MigrateOnStart, err = boolValue(lookup, "MIGRATE_ON_START", cfg.MigrateOnStart); err != nil {
		return Config{}, err
	}
	if cfg.AllowInsecureNonLoopback, err = boolValue(lookup, "ALLOW_INSECURE_NON_LOOPBACK", false); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = durationValue(lookup, "SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.PairingCodeTTL, err = durationValue(lookup, "PAIRING_CODE_TTL", cfg.PairingCodeTTL); err != nil {
		return Config{}, err
	}
	if cfg.PairingCodeMaxAttempts, err = intValue(lookup, "PAIRING_CODE_MAX_ATTEMPTS", cfg.PairingCodeMaxAttempts); err != nil {
		return Config{}, err
	}
	if cfg.TokenLastUsedTouchInterval, err = durationValue(lookup, "TOKEN_LAST_USED_TOUCH_INTERVAL", cfg.TokenLastUsedTouchInterval); err != nil {
		return Config{}, err
	}
	if cfg.PairingRateLimitPerMinute, err = intValue(lookup, "PAIRING_RATE_LIMIT_PER_MINUTE", cfg.PairingRateLimitPerMinute); err != nil {
		return Config{}, err
	}
	if cfg.AuthFailureLimitPerMinute, err = intValue(lookup, "AUTH_FAILURE_LIMIT_PER_MINUTE", cfg.AuthFailureLimitPerMinute); err != nil {
		return Config{}, err
	}

	publicRaw, publicSet := lookup("PUBLIC_BASE_URL")
	if !publicSet || strings.TrimSpace(publicRaw) == "" {
		publicRaw = (&url.URL{Scheme: "http", Host: cfg.ListenAddr}).String()
	}
	cfg.PublicBaseURL, err = parseHTTPURL("PUBLIC_BASE_URL", publicRaw)
	if err != nil {
		return Config{}, err
	}
	if err := validateListeningPolicy(&cfg); err != nil {
		return Config{}, err
	}

	if cfg.Model.Required, err = boolValue(lookup, "MODEL_REQUIRED", false); err != nil {
		return Config{}, err
	}
	baseRaw := optionalTrimmed(lookup, "MODEL_BASE_URL")
	cfg.Model.Name = optionalTrimmed(lookup, "MODEL_NAME")
	cfg.Model.APIKey = optionalTrimmed(lookup, "MODEL_API_KEY")
	if cfg.Model.MinimumContext, err = intValue(lookup, "MODEL_MIN_CONTEXT_WINDOW", cfg.Model.MinimumContext); err != nil {
		return Config{}, err
	}
	if cfg.Model.Timeout, err = durationValue(lookup, "MODEL_TIMEOUT", cfg.Model.Timeout); err != nil {
		return Config{}, err
	}
	if cfg.Model.ProbeCacheTTL, err = durationValue(lookup, "MODEL_PROBE_CACHE_TTL", cfg.Model.ProbeCacheTTL); err != nil {
		return Config{}, err
	}
	contextRaw := optionalTrimmed(lookup, "MODEL_CONTEXT_WINDOW")
	modelValues := []string{baseRaw, cfg.Model.Name, cfg.Model.APIKey, contextRaw}
	present := 0
	for _, value := range modelValues {
		if value != "" {
			present++
		}
	}
	if present != 0 && present != len(modelValues) {
		return Config{}, errors.New("MODEL_BASE_URL, MODEL_NAME, MODEL_API_KEY, and MODEL_CONTEXT_WINDOW must be configured together")
	}
	if cfg.Model.Required && present == 0 {
		return Config{}, errors.New("model configuration is required when MODEL_REQUIRED=true")
	}
	if present == len(modelValues) {
		cfg.Model.Enabled = true
		cfg.Model.BaseURL, err = parseHTTPURL("MODEL_BASE_URL", baseRaw)
		if err != nil {
			return Config{}, err
		}
		cfg.Model.ContextWindow, err = strconv.Atoi(contextRaw)
		if err != nil || cfg.Model.ContextWindow <= 0 {
			return Config{}, errors.New("MODEL_CONTEXT_WINDOW must be a positive integer")
		}
		if cfg.Model.ContextWindow < cfg.Model.MinimumContext {
			return Config{}, fmt.Errorf("MODEL_CONTEXT_WINDOW must be at least %d", cfg.Model.MinimumContext)
		}
	}

	if cfg.ShutdownTimeout <= 0 || cfg.PairingCodeTTL <= 0 || cfg.TokenLastUsedTouchInterval <= 0 || cfg.Model.Timeout <= 0 || cfg.Model.ProbeCacheTTL <= 0 {
		return Config{}, errors.New("duration settings must be positive")
	}
	if cfg.PairingCodeMaxAttempts <= 0 || cfg.PairingRateLimitPerMinute <= 0 || cfg.AuthFailureLimitPerMinute <= 0 || cfg.Model.MinimumContext <= 0 {
		return Config{}, errors.New("numeric limits must be positive")
	}
	return cfg, nil
}

func validateListeningPolicy(cfg *Config) error {
	host, _, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("LISTEN_ADDR: expected host:port: %w", err)
	}
	if isLoopbackHost(host) {
		return nil
	}
	if cfg.PublicBaseURL.Scheme == "https" {
		return nil
	}
	if !cfg.AllowInsecureNonLoopback {
		return errors.New("non-loopback LISTEN_ADDR requires an HTTPS PUBLIC_BASE_URL or ALLOW_INSECURE_NON_LOOPBACK=true")
	}
	cfg.InsecureNonLoopbackWarning = true
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func parseHTTPURL(name, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%s must be an absolute HTTP(S) URL", name)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%s must use http or https", name)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s must not include credentials, query, or fragment", name)
	}
	return u, nil
}

func validatePostgresURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return errors.New("must be an absolute postgres or postgresql URL with a host")
	}
	return nil
}

func requiredString(lookup envReader, name string) (string, error) {
	value := optionalTrimmed(lookup, name)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func stringValue(lookup envReader, name, fallback string) (string, error) {
	value, ok := lookup(name)
	if !ok {
		return fallback, nil
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s must not be empty", name)
	}
	return value, nil
}

func optionalTrimmed(lookup envReader, name string) string {
	value, _ := lookup(name)
	return strings.TrimSpace(value)
}

func boolValue(lookup envReader, name string, fallback bool) (bool, error) {
	value, ok := lookup(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return parsed, nil
}

func durationValue(lookup envReader, name string, fallback time.Duration) (time.Duration, error) {
	value, ok := lookup(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", name)
	}
	return parsed, nil
}

func intValue(lookup envReader, name string, fallback int) (int, error) {
	value, ok := lookup(name)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}
