package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/integrations/nocturne"
	"golang.org/x/text/unicode/norm"
)

const DefaultMinimumContextWindow = 4096

type Config struct {
	ListenAddr                 string
	PublicBaseURL              *url.URL
	DatabaseURL                string
	MigrateOnStart             bool
	AllowInsecureNonLoopback   bool
	InsecureNonLoopbackWarning bool
	AdminUI                    AdminUIConfig
	ShutdownTimeout            time.Duration
	PairingCodeTTL             time.Duration
	PairingCodeMaxAttempts     int
	TokenLastUsedTouchInterval time.Duration
	PairingRateLimitPerMinute  int
	AuthFailureLimitPerMinute  int
	DeviceRateLimitPerMinute   int
	Model                      ModelConfig
	Offline                    OfflineConfig
	Nocturne                   NocturneConfig
	Notesync                   NotesyncConfig
	Privacy                    PrivacyConfig
}

type AdminUIConfig struct {
	Enabled                 bool
	Token                   string
	TrustedLoopbackProxy    bool
	SettingsFile            string
	NotesyncSource          string
	NotesyncSettingsSavedAt time.Time
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

type OfflineConfig struct {
	SignerKeyID         string
	SignerPrivateKey    ed25519.PrivateKey
	SignerIssuedAt      time.Time
	SignerNotAfter      time.Time
	SignerManifestChain []json.RawMessage
}

func (c OfflineConfig) SignerEnabled() bool {
	return c.SignerKeyID != "" && len(c.SignerPrivateKey) == ed25519.PrivateKeySize
}

type NocturneConfig struct {
	Enabled                  bool
	BaseURL                  *url.URL
	APIToken                 string
	MaintenanceToken         string
	Namespace                string
	Domain                   string
	HTTPTimeout              time.Duration
	ReconciliationInterval   time.Duration
	WorkerPollInterval       time.Duration
	WorkerLeaseDuration      time.Duration
	WorkerBatchSize          int
	DeliveryTTL              time.Duration
	CandidateSweepInterval   time.Duration
	DeliverySweepInterval    time.Duration
	BackupRoot               string
	BackupControllerInterval time.Duration
	BackupRetention          time.Duration
	MasterWrappingKey        []byte
	PGDumpDSN                string
	ImageLockReference       string
}

type NotesyncConfig struct {
	Enabled                  bool
	AllowInsecureNonLoopback bool
	BaseURL                  *url.URL
	APIToken                 string
	Vault                    string
	PathPrefix               string
	HTTPTimeout              time.Duration
	WorkerInterval           time.Duration
	WorkerBatch              int
	ScanPageSize             int
	ScanMaxPages             int
}

func (c NotesyncConfig) String() string {
	baseURL := ""
	if c.BaseURL != nil {
		baseURL = c.BaseURL.String()
	}
	return fmt.Sprintf("{Enabled:%t BaseURL:%s APIToken:<redacted> Vault:%s PathPrefix:%s HTTPTimeout:%s WorkerInterval:%s WorkerBatch:%d ScanPageSize:%d ScanMaxPages:%d}",
		c.Enabled, baseURL, c.Vault, c.PathPrefix, c.HTTPTimeout, c.WorkerInterval, c.WorkerBatch, c.ScanPageSize, c.ScanMaxPages)
}

type PrivacyConfig struct {
	ErasureGrantTTL         time.Duration
	ErasureGrantMaxAttempts int
	OfflineChallengeKeys    map[int][]byte
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
		DeviceRateLimitPerMinute:   600,
		Model: ModelConfig{
			MinimumContext: DefaultMinimumContextWindow,
			Timeout:        30 * time.Second,
			ProbeCacheTTL:  15 * time.Minute,
		},
		Nocturne: NocturneConfig{
			HTTPTimeout:              10 * time.Second,
			ReconciliationInterval:   30 * time.Second,
			WorkerPollInterval:       time.Second,
			WorkerLeaseDuration:      2 * time.Minute,
			WorkerBatchSize:          50,
			DeliveryTTL:              24 * time.Hour,
			CandidateSweepInterval:   5 * time.Minute,
			DeliverySweepInterval:    5 * time.Minute,
			BackupControllerInterval: 24 * time.Hour,
			BackupRetention:          30 * 24 * time.Hour,
		},
		Notesync: NotesyncConfig{
			PathPrefix: "edu-agent", HTTPTimeout: 10 * time.Second, WorkerInterval: 3 * time.Second,
			WorkerBatch: 20, ScanPageSize: 100, ScanMaxPages: 20,
		},
		Privacy: PrivacyConfig{ErasureGrantTTL: 10 * time.Minute, ErasureGrantMaxAttempts: 5},
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
	if cfg.AdminUI.Enabled, err = boolValue(lookup, "ADMIN_UI_ENABLED", false); err != nil {
		return Config{}, err
	}
	if cfg.AdminUI.TrustedLoopbackProxy, err = boolValue(lookup, "ADMIN_UI_TRUSTED_LOOPBACK_PROXY", false); err != nil {
		return Config{}, err
	}
	if cfg.AdminUI.SettingsFile, err = stringValue(lookup, "ADMIN_UI_SETTINGS_FILE", ""); err != nil {
		return Config{}, err
	}
	if cfg.AdminUI.SettingsFile != "" {
		if _, err := validateAdminSettingsPath(cfg.AdminUI.SettingsFile); err != nil {
			return Config{}, err
		}
	}
	if cfg.AdminUI.Enabled {
		if cfg.AdminUI.Token, err = requiredString(lookup, "ADMIN_UI_TOKEN"); err != nil {
			return Config{}, err
		}
		if err := validateAdminUIToken(cfg.AdminUI.Token); err != nil {
			return Config{}, err
		}
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
	if cfg.DeviceRateLimitPerMinute, err = intValue(lookup, "DEVICE_RATE_LIMIT_PER_MINUTE", cfg.DeviceRateLimitPerMinute); err != nil {
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
	if cfg.AdminUI.Enabled && !isLoopbackHost(cfg.PublicBaseURL.Hostname()) {
		return Config{}, errors.New("ADMIN_UI_ENABLED requires a loopback PUBLIC_BASE_URL")
	}
	listenHost, _, _ := net.SplitHostPort(cfg.ListenAddr)
	if cfg.AdminUI.Enabled && !isLoopbackHost(listenHost) && !cfg.AdminUI.TrustedLoopbackProxy {
		return Config{}, errors.New("ADMIN_UI_ENABLED on a non-loopback LISTEN_ADDR requires ADMIN_UI_TRUSTED_LOOPBACK_PROXY=true and an external loopback-only transport boundary")
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

	if err := loadOffline(lookup, &cfg.Offline); err != nil {
		return Config{}, err
	}
	if err := loadNocturne(lookup, &cfg.Nocturne); err != nil {
		return Config{}, err
	}
	if err := loadNotesync(lookup, &cfg.Notesync); err != nil {
		return Config{}, err
	}
	cfg.AdminUI.NotesyncSource = "environment"
	if cfg.AdminUI.SettingsFile != "" {
		settings, found, settingsErr := LoadNotesyncAdminSettings(cfg.AdminUI.SettingsFile)
		if settingsErr != nil {
			return Config{}, settingsErr
		}
		if found {
			cfg.Notesync, settingsErr = ApplyNotesyncAdminSettings(cfg.Notesync, settings)
			if settingsErr != nil {
				return Config{}, fmt.Errorf("admin NoteSync settings: %w", settingsErr)
			}
			cfg.AdminUI.NotesyncSource = "admin_settings"
			cfg.AdminUI.NotesyncSettingsSavedAt = settings.SavedAt
		}
	}
	if cfg.Privacy.ErasureGrantTTL, err = durationValue(lookup, "PRIVACY_ERASURE_GRANT_TTL", cfg.Privacy.ErasureGrantTTL); err != nil {
		return Config{}, err
	}
	if cfg.Privacy.ErasureGrantMaxAttempts, err = intValue(lookup, "PRIVACY_ERASURE_GRANT_MAX_ATTEMPTS", cfg.Privacy.ErasureGrantMaxAttempts); err != nil {
		return Config{}, err
	}
	if cfg.Privacy.OfflineChallengeKeys, err = offlineChallengeKeys(lookup); err != nil {
		return Config{}, err
	}

	if cfg.ShutdownTimeout <= 0 || cfg.PairingCodeTTL <= 0 || cfg.TokenLastUsedTouchInterval <= 0 || cfg.Model.Timeout <= 0 || cfg.Model.ProbeCacheTTL <= 0 || cfg.Privacy.ErasureGrantTTL <= 0 {
		return Config{}, errors.New("duration settings must be positive")
	}
	if cfg.PairingCodeMaxAttempts <= 0 || cfg.PairingRateLimitPerMinute <= 0 || cfg.AuthFailureLimitPerMinute <= 0 || cfg.DeviceRateLimitPerMinute <= 0 || cfg.Model.MinimumContext <= 0 || cfg.Privacy.ErasureGrantMaxAttempts <= 0 {
		return Config{}, errors.New("numeric limits must be positive")
	}
	if err := validateAdminUISecretSeparation(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateAdminUISecretSeparation(cfg Config) error {
	if !cfg.AdminUI.Enabled {
		return nil
	}
	secrets := []struct {
		name  string
		value string
	}{
		{name: "MODEL_API_KEY", value: cfg.Model.APIKey},
		{name: "NOCTURNE_API_TOKEN", value: cfg.Nocturne.APIToken},
		{name: "NOCTURNE_MAINTENANCE_TOKEN", value: cfg.Nocturne.MaintenanceToken},
		{name: "NOTESYNC_API_TOKEN", value: cfg.Notesync.APIToken},
	}
	for _, candidate := range secrets {
		if candidate.value == "" || len(candidate.value) != len(cfg.AdminUI.Token) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(candidate.value), []byte(cfg.AdminUI.Token)) == 1 {
			return fmt.Errorf("ADMIN_UI_TOKEN must differ from %s", candidate.name)
		}
	}
	return nil
}

func offlineChallengeKeys(lookup envReader) (map[int][]byte, error) {
	raw, err := optionalSecret(lookup, "PRIVACY_OFFLINE_CHALLENGE_KEYS")
	if err != nil || raw == "" {
		return nil, err
	}
	keys := make(map[int][]byte)
	for _, entry := range strings.Split(raw, ",") {
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 {
			return nil, errors.New("PRIVACY_OFFLINE_CHALLENGE_KEYS must use version:base64url entries")
		}
		version, parseErr := strconv.Atoi(parts[0])
		if parseErr != nil || version < 2 {
			return nil, errors.New("PRIVACY_OFFLINE_CHALLENGE_KEYS versions must be integers greater than or equal to 2")
		}
		key, decodeErr := base64.RawURLEncoding.DecodeString(parts[1])
		if decodeErr != nil || len(key) != sha256.Size || base64.RawURLEncoding.EncodeToString(key) != parts[1] {
			return nil, errors.New("PRIVACY_OFFLINE_CHALLENGE_KEYS values must be canonical unpadded Base64url containing 32 bytes")
		}
		if _, exists := keys[version]; exists {
			return nil, errors.New("PRIVACY_OFFLINE_CHALLENGE_KEYS versions must be unique")
		}
		keys[version] = key
	}
	return keys, nil
}

func loadOffline(lookup envReader, cfg *OfflineConfig) error {
	keyID := optionalTrimmed(lookup, "OFFLINE_SIGNER_KEY_ID")
	privateKeyRaw, err := optionalSecret(lookup, "OFFLINE_SIGNER_PRIVATE_KEY")
	if err != nil {
		return err
	}
	issuedAtRaw := optionalTrimmed(lookup, "OFFLINE_SIGNER_ISSUED_AT")
	notAfterRaw := optionalTrimmed(lookup, "OFFLINE_SIGNER_NOT_AFTER")
	manifestChainRaw, err := optionalSecret(lookup, "OFFLINE_SIGNER_MANIFEST_CHAIN")
	if err != nil {
		return err
	}
	values := []string{keyID, privateKeyRaw, issuedAtRaw, notAfterRaw}
	present := 0
	for _, value := range values {
		if value != "" {
			present++
		}
	}
	if present == 0 {
		if manifestChainRaw != "" {
			return errors.New("OFFLINE_SIGNER_MANIFEST_CHAIN requires the signer key profile")
		}
		return nil
	}
	if present != len(values) {
		return errors.New("offline signer configuration requires key ID, private key, issued-at, and not-after")
	}
	if len(keyID) > 128 || strings.TrimSpace(keyID) != keyID {
		return errors.New("OFFLINE_SIGNER_KEY_ID is invalid")
	}
	privateKey, err := base64.RawURLEncoding.DecodeString(privateKeyRaw)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize || base64.RawURLEncoding.EncodeToString(privateKey) != privateKeyRaw {
		return errors.New("OFFLINE_SIGNER_PRIVATE_KEY must be unpadded Base64url containing 64 bytes")
	}
	expectedKey := ed25519.NewKeyFromSeed(privateKey[:ed25519.SeedSize])
	if subtle.ConstantTimeCompare(privateKey, expectedKey) != 1 {
		return errors.New("OFFLINE_SIGNER_PRIVATE_KEY is not a canonical Ed25519 private key")
	}
	issuedAt, err := time.Parse(time.RFC3339, issuedAtRaw)
	if err != nil {
		return errors.New("OFFLINE_SIGNER_ISSUED_AT must be RFC3339")
	}
	notAfter, err := time.Parse(time.RFC3339, notAfterRaw)
	if err != nil {
		return errors.New("OFFLINE_SIGNER_NOT_AFTER must be RFC3339")
	}
	if _, offset := issuedAt.Zone(); offset != 0 {
		return errors.New("OFFLINE_SIGNER_ISSUED_AT must use UTC")
	}
	if _, offset := notAfter.Zone(); offset != 0 {
		return errors.New("OFFLINE_SIGNER_NOT_AFTER must use UTC")
	}
	if !notAfter.After(issuedAt) {
		return errors.New("OFFLINE_SIGNER_NOT_AFTER must be after OFFLINE_SIGNER_ISSUED_AT")
	}
	cfg.SignerKeyID = keyID
	cfg.SignerPrivateKey = append(ed25519.PrivateKey(nil), privateKey...)
	cfg.SignerIssuedAt = issuedAt.UTC()
	cfg.SignerNotAfter = notAfter.UTC()
	if manifestChainRaw != "" {
		decoder := json.NewDecoder(bytes.NewBufferString(manifestChainRaw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg.SignerManifestChain); err != nil || len(cfg.SignerManifestChain) == 0 || len(cfg.SignerManifestChain) > 16 {
			return errors.New("OFFLINE_SIGNER_MANIFEST_CHAIN must be a JSON array containing 1 to 16 manifest envelopes")
		}
		for _, manifest := range cfg.SignerManifestChain {
			trimmed := bytes.TrimSpace(manifest)
			if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' || !json.Valid(trimmed) {
				return errors.New("OFFLINE_SIGNER_MANIFEST_CHAIN contains an invalid manifest envelope")
			}
		}
	}
	return nil
}

func loadNocturne(lookup envReader, cfg *NocturneConfig) error {
	var err error
	if cfg.Enabled, err = boolValue(lookup, "NOCTURNE_ENABLED", false); err != nil {
		return err
	}
	if cfg.HTTPTimeout, err = durationValue(lookup, "NOCTURNE_HTTP_TIMEOUT", cfg.HTTPTimeout); err != nil {
		return err
	}
	if cfg.ReconciliationInterval, err = durationValue(lookup, "NOCTURNE_RECONCILIATION_INTERVAL", cfg.ReconciliationInterval); err != nil {
		return err
	}
	if cfg.WorkerPollInterval, err = durationValue(lookup, "NOCTURNE_WORKER_POLL_INTERVAL", cfg.WorkerPollInterval); err != nil {
		return err
	}
	if cfg.WorkerLeaseDuration, err = durationValue(lookup, "NOCTURNE_WORKER_LEASE_DURATION", cfg.WorkerLeaseDuration); err != nil {
		return err
	}
	if cfg.WorkerBatchSize, err = intValue(lookup, "NOCTURNE_WORKER_BATCH_SIZE", cfg.WorkerBatchSize); err != nil {
		return err
	}
	if cfg.DeliveryTTL, err = durationValue(lookup, "NOCTURNE_DELIVERY_TTL", cfg.DeliveryTTL); err != nil {
		return err
	}
	if cfg.CandidateSweepInterval, err = durationValue(lookup, "NOCTURNE_CANDIDATE_SWEEP_INTERVAL", cfg.CandidateSweepInterval); err != nil {
		return err
	}
	if cfg.DeliverySweepInterval, err = durationValue(lookup, "NOCTURNE_DELIVERY_SWEEP_INTERVAL", cfg.DeliverySweepInterval); err != nil {
		return err
	}
	if cfg.BackupControllerInterval, err = durationValue(lookup, "NOCTURNE_BACKUP_CONTROLLER_INTERVAL", cfg.BackupControllerInterval); err != nil {
		return err
	}
	if cfg.BackupRetention, err = durationValue(lookup, "NOCTURNE_BACKUP_RETENTION", cfg.BackupRetention); err != nil {
		return err
	}

	baseRaw := optionalTrimmed(lookup, "NOCTURNE_BASE_URL")
	cfg.Namespace = optionalTrimmed(lookup, "NOCTURNE_NAMESPACE")
	cfg.Domain = optionalTrimmed(lookup, "NOCTURNE_DOMAIN")
	cfg.BackupRoot = optionalTrimmed(lookup, "NOCTURNE_BACKUP_ROOT")
	cfg.PGDumpDSN = optionalTrimmed(lookup, "NOCTURNE_PG_DUMP_DSN")
	cfg.ImageLockReference = optionalTrimmed(lookup, "NOCTURNE_IMAGE_LOCK_REFERENCE")
	if cfg.APIToken, err = optionalSecret(lookup, "NOCTURNE_API_TOKEN"); err != nil {
		return err
	}
	if cfg.MaintenanceToken, err = optionalSecret(lookup, "NOCTURNE_MAINTENANCE_TOKEN"); err != nil {
		return err
	}
	wrappingKeyRaw, err := optionalSecret(lookup, "NOCTURNE_BACKUP_MASTER_WRAPPING_KEY")
	if err != nil {
		return err
	}
	profileValues := []string{
		baseRaw, cfg.APIToken, cfg.MaintenanceToken, cfg.Namespace, cfg.Domain, cfg.BackupRoot,
		wrappingKeyRaw, cfg.PGDumpDSN, cfg.ImageLockReference,
	}
	present := 0
	for _, value := range profileValues {
		if value != "" {
			present++
		}
	}
	if !cfg.Enabled {
		if present != 0 {
			return errors.New("NOCTURNE_ENABLED must be true when Nocturne connection or backup settings are configured")
		}
		return validateNocturneIntervals(*cfg)
	}
	if present != len(profileValues) {
		return errors.New("Nocturne enabled configuration requires base URL, API and maintenance tokens, namespace, domain, backup root, master wrapping key, pg_dump DSN, and image lock reference")
	}
	cfg.BaseURL, err = parseHTTPURL("NOCTURNE_BASE_URL", baseRaw)
	if err != nil {
		return err
	}
	if !utf8.ValidString(cfg.APIToken) || utf8.RuneCountInString(cfg.APIToken) < 32 {
		return errors.New("NOCTURNE_API_TOKEN must contain at least 32 characters")
	}
	if _, err := decodeSecret32("NOCTURNE_MAINTENANCE_TOKEN", cfg.MaintenanceToken); err != nil {
		return err
	}
	if secretsEqual(cfg.APIToken, cfg.MaintenanceToken) {
		return errors.New("Nocturne API and maintenance tokens must differ")
	}
	if !validFixedName(cfg.Namespace) || !validFixedName(cfg.Domain) {
		return errors.New("NOCTURNE_NAMESPACE and NOCTURNE_DOMAIN must be fixed lowercase names")
	}
	if !filepath.IsAbs(cfg.BackupRoot) || filepath.Clean(cfg.BackupRoot) != cfg.BackupRoot || cfg.BackupRoot == string(filepath.Separator) {
		return errors.New("NOCTURNE_BACKUP_ROOT must be a canonical absolute non-root path")
	}
	if err := validatePostgresURL(cfg.PGDumpDSN); err != nil {
		return fmt.Errorf("NOCTURNE_PG_DUMP_DSN: %w", err)
	}
	if !validImageDigestReference(cfg.ImageLockReference) {
		return errors.New("NOCTURNE_IMAGE_LOCK_REFERENCE must be repository@sha256:<64 lowercase hex characters>")
	}
	_, imageDigest, _ := strings.Cut(cfg.ImageLockReference, "@")
	if imageDigest != nocturne.ImagePlatformManifestDigest {
		return errors.New("NOCTURNE_IMAGE_LOCK_REFERENCE digest does not match the fixed Nocturne platform manifest")
	}
	cfg.MasterWrappingKey, err = decodeSecret32("NOCTURNE_BACKUP_MASTER_WRAPPING_KEY", wrappingKeyRaw)
	if err != nil {
		return err
	}
	return validateNocturneIntervals(*cfg)
}

// pi-lens-ignore: go-bare-error
func loadNotesync(lookup envReader, cfg *NotesyncConfig) error {
	var err error
	if cfg.Enabled, err = boolValue(lookup, "NOTESYNC_ENABLED", false); err != nil {
		return fmt.Errorf("NOTESYNC_ENABLED: %w", err)
	}
	if cfg.AllowInsecureNonLoopback, err = boolValue(lookup, "NOTESYNC_ALLOW_INSECURE_NON_LOOPBACK", false); err != nil {
		return fmt.Errorf("NOTESYNC_ALLOW_INSECURE_NON_LOOPBACK: %w", err)
	}
	if cfg.HTTPTimeout, err = durationValue(lookup, "NOTESYNC_HTTP_TIMEOUT", cfg.HTTPTimeout); err != nil {
		return fmt.Errorf("NOTESYNC_HTTP_TIMEOUT: %w", err)
	}
	if cfg.WorkerInterval, err = durationValue(lookup, "NOTESYNC_WORKER_INTERVAL", cfg.WorkerInterval); err != nil {
		return fmt.Errorf("NOTESYNC_WORKER_INTERVAL: %w", err)
	}
	if cfg.WorkerBatch, err = intValue(lookup, "NOTESYNC_WORKER_BATCH", cfg.WorkerBatch); err != nil {
		return fmt.Errorf("NOTESYNC_WORKER_BATCH: %w", err)
	}
	if cfg.ScanPageSize, err = intValue(lookup, "NOTESYNC_SCAN_PAGE_SIZE", cfg.ScanPageSize); err != nil {
		return fmt.Errorf("NOTESYNC_SCAN_PAGE_SIZE: %w", err)
	}
	if cfg.ScanMaxPages, err = intValue(lookup, "NOTESYNC_SCAN_MAX_PAGES", cfg.ScanMaxPages); err != nil {
		return fmt.Errorf("NOTESYNC_SCAN_MAX_PAGES: %w", err)
	}
	baseRaw := optionalTrimmed(lookup, "NOTESYNC_BASE_URL")
	cfg.Vault = optionalTrimmed(lookup, "NOTESYNC_VAULT")
	if prefix, ok := lookup("NOTESYNC_PATH_PREFIX"); ok {
		cfg.PathPrefix = strings.TrimSpace(prefix)
	}
	if cfg.APIToken, err = optionalSecret(lookup, "NOTESYNC_API_TOKEN"); err != nil {
		return err
	}
	configured := baseRaw != "" || cfg.APIToken != "" || cfg.Vault != ""
	if !cfg.Enabled {
		if configured {
			return errors.New("NOTESYNC_ENABLED must be true when NoteSync connection settings are configured")
		}
		return validateNotesyncLimits(*cfg)
	}
	if baseRaw == "" || cfg.APIToken == "" || cfg.Vault == "" || cfg.PathPrefix == "" {
		return errors.New("NoteSync enabled configuration requires base URL, API token, vault, and managed path prefix")
	}
	cfg.BaseURL, err = parseHTTPURL("NOTESYNC_BASE_URL", baseRaw)
	if err != nil {
		return fmt.Errorf("NOTESYNC_BASE_URL: %w", err)
	}
	if cfg.BaseURL.RawPath != "" {
		return errors.New("NOTESYNC_BASE_URL must not contain percent-encoded path segments")
	}
	if cfg.BaseURL.Scheme == "http" && !isLoopbackHost(cfg.BaseURL.Hostname()) && !cfg.AllowInsecureNonLoopback {
		return errors.New("non-loopback NOTESYNC_BASE_URL requires HTTPS or NOTESYNC_ALLOW_INSECURE_NON_LOOPBACK=true")
	}
	if !validNotesyncToken(cfg.APIToken) {
		return errors.New("NOTESYNC_API_TOKEN must contain at least 32 visible ASCII characters")
	}
	if !validNotesyncName(cfg.Vault, 255) {
		return errors.New("NOTESYNC_VAULT is invalid")
	}
	if !validManagedPrefix(cfg.PathPrefix) {
		return errors.New("NOTESYNC_PATH_PREFIX must be a canonical relative path")
	}
	return validateNotesyncLimits(*cfg)
}

func validateNotesyncLimits(cfg NotesyncConfig) error {
	if cfg.HTTPTimeout <= 0 || cfg.HTTPTimeout > time.Minute || cfg.WorkerInterval <= 0 || cfg.WorkerInterval > time.Hour {
		return errors.New("NoteSync timing settings are outside supported bounds")
	}
	if cfg.WorkerBatch <= 0 || cfg.WorkerBatch > 1000 || cfg.ScanPageSize <= 0 || cfg.ScanPageSize > 100 || cfg.ScanMaxPages <= 0 || cfg.ScanMaxPages > 1000 {
		return errors.New("NoteSync batch and scan limits are outside supported bounds")
	}
	return nil
}

func validNotesyncToken(value string) bool {
	if !utf8.ValidString(value) || len(value) < 32 {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func validNotesyncName(value string, max int) bool {
	if value == "" || len(value) > max || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validManagedPrefix(value string) bool {
	if !validNotesyncName(value, 512) || !norm.NFKC.IsNormalString(value) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") ||
		strings.HasSuffix(value, "/") || path.Clean(value) != value || value == "." {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." || segment == "" {
			return false
		}
	}
	return true
}

func validateNocturneIntervals(cfg NocturneConfig) error {
	if cfg.HTTPTimeout <= 0 || cfg.ReconciliationInterval <= 0 || cfg.WorkerPollInterval <= 0 || cfg.WorkerLeaseDuration <= 0 || cfg.DeliveryTTL <= 0 || cfg.CandidateSweepInterval <= 0 || cfg.DeliverySweepInterval <= 0 || cfg.BackupControllerInterval <= 0 || cfg.BackupRetention <= 0 {
		return errors.New("Nocturne duration settings must be positive")
	}
	if cfg.WorkerBatchSize <= 0 {
		return errors.New("NOCTURNE_WORKER_BATCH_SIZE must be positive")
	}
	if cfg.HTTPTimeout > cfg.WorkerLeaseDuration/12 {
		return errors.New("NOCTURNE_WORKER_LEASE_DURATION must be at least 12 times NOCTURNE_HTTP_TIMEOUT")
	}
	if cfg.CandidateSweepInterval > 5*time.Minute || cfg.DeliverySweepInterval > 5*time.Minute {
		return errors.New("Nocturne candidate and delivery sweep intervals must not exceed 5m")
	}
	if cfg.BackupControllerInterval > 24*time.Hour {
		return errors.New("NOCTURNE_BACKUP_CONTROLLER_INTERVAL must not exceed 24h")
	}
	if cfg.BackupRetention > 30*24*time.Hour {
		return errors.New("NOCTURNE_BACKUP_RETENTION must not exceed 30d")
	}
	return nil
}

func optionalSecret(lookup envReader, name string) (string, error) {
	value, ok := lookup(name)
	if !ok || value == "" {
		return "", nil
	}
	if strings.TrimSpace(value) != value {
		return "", fmt.Errorf("%s must not contain surrounding whitespace", name)
	}
	return value, nil
}

func decodeSecret32(name, encoded string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return nil, fmt.Errorf("%s must be canonical unpadded base64url for exactly 32 bytes", name)
	}
	return decoded, nil
}

func secretsEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func validFixedName(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value {
		if character < 'a' || character > 'z' {
			if character < '0' || character > '9' {
				if character != '-' && character != '_' {
					return false
				}
			}
		}
	}
	return true
}

func validImageDigestReference(value string) bool {
	if strings.Count(value, "@") != 1 {
		return false
	}
	repository, digest, found := strings.Cut(value, "@sha256:")
	if !found || repository == "" || len(digest) != 64 || strings.ToLower(repository) != repository {
		return false
	}
	if strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/") || strings.Contains(repository, "//") {
		return false
	}
	lastSlash := strings.LastIndexByte(repository, '/')
	if colon := strings.LastIndexByte(repository, ':'); colon > lastSlash {
		return false
	}
	for _, character := range repository {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') &&
			character != '.' && character != '_' && character != '-' && character != '/' && character != ':' {
			return false
		}
	}
	for _, character := range digest {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validateAdminUIToken(token string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != token {
		return errors.New("ADMIN_UI_TOKEN must be the canonical unpadded base64url encoding of 32 random bytes")
	}
	return nil
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
