package notesync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	SupportedVersion     = "3.6.1"
	ClientType           = "CLI"
	ClientName           = "edu-agent-notesync"
	ClientVersion        = "1"
	UserAgent            = "edu-agent-notesync-bridge/1"
	DefaultBodyLimit     = int64(32 << 20)
	businessSuccess      = 1
	businessRateLimit    = 303
	businessAPINotFound  = 304
	businessNotFound     = 430
	businessNoteExists   = 431
	businessVaultDenied  = 315
	businessInvalidPath  = 444
	capabilityProbePath  = "../.edu-agent-notesync-capability-probe.md"
	capabilityProbeVault = "__edu_agent_notesync_forbidden_probe__"
)

type ErrorCategory string

const (
	CategoryAuth             ErrorCategory = "auth"
	CategoryNotFound         ErrorCategory = "not_found"
	CategoryConflict         ErrorCategory = "conflict"
	CategoryValidation       ErrorCategory = "validation"
	CategoryRateLimited      ErrorCategory = "rate_limited"
	CategoryUpstream         ErrorCategory = "upstream"
	CategoryTimeout          ErrorCategory = "timeout"
	CategoryTransport        ErrorCategory = "transport"
	CategoryContractMismatch ErrorCategory = "contract_mismatch"
)

type Error struct {
	category  ErrorCategory
	operation string
	status    int
	code      int
	cause     error
}

func (e *Error) Error() string {
	return fmt.Sprintf("notesync operation failed: operation=%s category=%s", e.operation, e.category)
}

func (e *Error) Unwrap() error           { return e.cause }
func (e *Error) Category() ErrorCategory { return e.category }
func (e *Error) HTTPStatus() int         { return e.status }
func (e *Error) BusinessCode() int       { return e.code }

func Category(err error) ErrorCategory {
	var target *Error
	if errors.As(err, &target) {
		return target.category
	}
	return CategoryTransport
}

func IsNotFound(err error) bool { return Category(err) == CategoryNotFound }

type Options struct {
	BaseURL    *url.URL
	APIToken   string
	Timeout    time.Duration
	BodyLimit  int64
	HTTPClient *http.Client
}

type Client struct {
	baseURL    url.URL
	apiToken   string
	httpClient *http.Client
	bodyLimit  int64
}

func New(options Options) (*Client, error) {
	if options.BaseURL == nil || (options.BaseURL.Scheme != "http" && options.BaseURL.Scheme != "https") ||
		options.BaseURL.Host == "" || options.BaseURL.User != nil || options.BaseURL.RawPath != "" || options.BaseURL.RawQuery != "" || options.BaseURL.Fragment != "" {
		return nil, errors.New("notesync base URL must be absolute HTTP(S) without credentials, encoded path, query, or fragment")
	}
	if !validAPIToken(options.APIToken) {
		return nil, errors.New("notesync API token must contain at least 32 visible ASCII characters")
	}
	if options.Timeout <= 0 {
		return nil, errors.New("notesync HTTP timeout must be positive")
	}
	if options.BodyLimit == 0 {
		options.BodyLimit = DefaultBodyLimit
	}
	if options.BodyLimit <= 0 {
		return nil, errors.New("notesync response body limit must be positive")
	}
	base := *options.BaseURL
	base.Path = strings.TrimRight(base.Path, "/")
	base.RawPath = ""
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	clone := *httpClient
	clone.Timeout = options.Timeout
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: base, apiToken: options.APIToken, httpClient: &clone, bodyLimit: options.BodyLimit}, nil
}

func validAPIToken(value string) bool {
	if len(value) < 32 {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

type Version struct {
	Version string
}

type Health struct {
	Status   string
	Version  string
	Database string
}

type Vault struct {
	ID   int64
	Name string
}

type Note struct {
	Vault       string
	Path        string
	Content     string
	ContentHash string
	Version     int64
	Ctime       int64
	Mtime       int64
	LastTime    int64
}

type NoteWrite struct {
	Vault      string `json:"vault"`
	Path       string `json:"path"`
	Content    string `json:"content"`
	Ctime      int64  `json:"ctime"`
	Mtime      int64  `json:"mtime"`
	CreateOnly bool   `json:"createOnly"`
}

type NotePage struct {
	Notes     []Note
	Page      int
	PageSize  int
	TotalRows int
}

type Capability struct {
	Compatible bool
	Reason     string
	Version    string
	Vault      string
}

func (c *Client) Version(ctx context.Context) (Version, error) {
	var wire versionWire
	if err := c.call(ctx, http.MethodGet, "/api/version", nil, nil, "version", &wire); err != nil {
		return Version{}, err
	}
	if strings.TrimSpace(wire.Version) == "" {
		return Version{}, contract("version", errors.New("version is missing"))
	}
	return Version{Version: wire.Version}, nil
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	var wire healthWire
	if err := c.call(ctx, http.MethodGet, "/api/health", nil, nil, "health", &wire); err != nil {
		return Health{}, err
	}
	if wire.Status == "" || wire.Version == "" || wire.Database == "" || wire.Uptime < 0 {
		return Health{}, contract("health", errors.New("health data is incomplete"))
	}
	return Health{Status: wire.Status, Version: wire.Version, Database: wire.Database}, nil
}

func (c *Client) Vaults(ctx context.Context) ([]Vault, error) {
	var wire []vaultWire
	if err := c.call(ctx, http.MethodGet, "/api/vault", nil, nil, "vaults", &wire); err != nil {
		return nil, err
	}
	if wire == nil {
		return nil, contract("vaults", errors.New("vault list is missing"))
	}
	result := make([]Vault, 0, len(wire))
	for _, item := range wire {
		if item.ID <= 0 || strings.TrimSpace(item.Name) == "" {
			return nil, contract("vaults", errors.New("vault identity is incomplete"))
		}
		result = append(result, Vault{ID: item.ID, Name: item.Name})
	}
	return result, nil
}

func (c *Client) GetNote(ctx context.Context, vault, path string) (Note, error) {
	if strings.TrimSpace(vault) == "" || strings.TrimSpace(path) == "" {
		return Note{}, requestError("get_note", errors.New("vault and path are required"))
	}
	query := url.Values{"vault": {vault}, "path": {path}}
	var wire noteReadWire
	if err := c.call(ctx, http.MethodGet, "/api/note", query, nil, "get_note", &wire); err != nil {
		return Note{}, err
	}
	return validateNote("get_note", vault, path, true, wire.noteWire)
}

func (c *Client) ListNotes(ctx context.Context, vault string, page, pageSize int) (NotePage, error) {
	if strings.TrimSpace(vault) == "" || page <= 0 || pageSize <= 0 || pageSize > 100 {
		return NotePage{}, requestError("list_notes", errors.New("vault and bounded pagination are required"))
	}
	query := url.Values{"vault": {vault}, "page": {strconv.Itoa(page)}, "pageSize": {strconv.Itoa(pageSize)}}
	var wire noteListWire
	if err := c.call(ctx, http.MethodGet, "/api/notes", query, nil, "list_notes", &wire); err != nil {
		return NotePage{}, err
	}
	if wire.Pager.Page != page || wire.Pager.PageSize != pageSize || wire.Pager.TotalRows < 0 || wire.List == nil ||
		len(wire.List) > pageSize || len(wire.List) > wire.Pager.TotalRows {
		return NotePage{}, contract("list_notes", errors.New("pagination data is incomplete or mismatched"))
	}
	result := NotePage{Page: wire.Pager.Page, PageSize: wire.Pager.PageSize, TotalRows: wire.Pager.TotalRows, Notes: make([]Note, 0, len(wire.List))}
	for _, item := range wire.List {
		note, err := validateNote("list_notes", vault, item.Path, false, item)
		if err != nil {
			return NotePage{}, err
		}
		result.Notes = append(result.Notes, note)
	}
	return result, nil
}

func (c *Client) CreateOrUpdateNote(ctx context.Context, input NoteWrite) (Note, error) {
	if strings.TrimSpace(input.Vault) == "" || strings.TrimSpace(input.Path) == "" || input.Ctime <= 0 || input.Mtime <= 0 {
		return Note{}, requestError("write_note", errors.New("vault, path, ctime, and mtime are required"))
	}
	var wire noteWire
	if err := c.call(ctx, http.MethodPost, "/api/note", nil, input, "write_note", &wire); err != nil {
		return Note{}, err
	}
	return validateNote("write_note", input.Vault, input.Path, true, wire)
}

func (c *Client) Probe(ctx context.Context, configuredVault string) Capability {
	version, err := c.Version(ctx)
	if err != nil {
		return Capability{Reason: "version_unavailable", Vault: configuredVault}
	}
	capability := Capability{Version: version.Version, Vault: configuredVault}
	if version.Version != SupportedVersion {
		capability.Reason = versionReason(version.Version)
		return capability
	}
	health, err := c.Health(ctx)
	if err != nil || health.Status != "healthy" || health.Database != "connected" || health.Version != SupportedVersion {
		capability.Reason = "capability_unavailable"
		return capability
	}
	vaults, err := c.Vaults(ctx)
	if err != nil || len(vaults) != 1 || vaults[0].Name != configuredVault {
		capability.Reason = "capability_unavailable"
		return capability
	}
	if !c.probeWriteCapability(ctx, configuredVault) || !c.probeVaultRestriction(ctx, configuredVault) {
		capability.Reason = "capability_unavailable"
		return capability
	}
	capability.Compatible = true
	return capability
}

func (c *Client) probeWriteCapability(ctx context.Context, configuredVault string) bool {
	_, err := c.CreateOrUpdateNote(ctx, NoteWrite{
		Vault: configuredVault, Path: capabilityProbePath, Ctime: 1, Mtime: 1, CreateOnly: true,
	})
	return hasBusinessCode(err, businessInvalidPath)
}

func (c *Client) probeVaultRestriction(ctx context.Context, configuredVault string) bool {
	vault := capabilityProbeVault
	if vault == configuredVault {
		vault += "_other"
	}
	_, err := c.CreateOrUpdateNote(ctx, NoteWrite{
		Vault: vault, Path: capabilityProbePath, Ctime: 1, Mtime: 1, CreateOnly: true,
	})
	return hasBusinessCode(err, businessVaultDenied)
}

func hasBusinessCode(err error, code int) bool {
	var target *Error
	return errors.As(err, &target) && target.BusinessCode() == code
}

func versionReason(observed string) string {
	left, leftOK := parseVersion(observed)
	right, _ := parseVersion(SupportedVersion)
	if !leftOK {
		return "version_unavailable"
	}
	for i := range left {
		if left[i] < right[i] {
			return "version_unsupported"
		}
		if left[i] > right[i] {
			return "version_untested"
		}
	}
	return "version_untested"
}

func parseVersion(value string) ([3]int, bool) {
	var result [3]int
	parts := strings.Split(value, ".")
	if len(parts) != len(result) {
		return result, false
	}
	for i, part := range parts {
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 || strconv.Itoa(parsed) != part {
			return result, false
		}
		result[i] = parsed
	}
	return result, true
}

func validateNote(operation, expectedVault, expectedPath string, requireContent bool, wire noteWire) (Note, error) {
	if wire.Path == "" || wire.Path != expectedPath || wire.Version < 0 || wire.Ctime <= 0 || wire.Mtime <= 0 || wire.LastTime < 0 || len(wire.UpdatedAt) == 0 || len(wire.CreatedAt) == 0 || requireContent && wire.Content == nil {
		return Note{}, contract(operation, errors.New("note data is incomplete or mismatched"))
	}
	content := ""
	if wire.Content != nil {
		content = *wire.Content
	}
	return Note{Vault: expectedVault, Path: wire.Path, Content: content, ContentHash: wire.ContentHash, Version: wire.Version, Ctime: wire.Ctime, Mtime: wire.Mtime, LastTime: wire.LastTime}, nil
}

type envelopeWire struct {
	Code      *int            `json:"code"`
	Status    *bool           `json:"status"`
	Message   json.RawMessage `json:"message,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Details   json.RawMessage `json:"details,omitempty"`
	TraceID   json.RawMessage `json:"traceId,omitempty"`
	Timestamp json.RawMessage `json:"timestamp,omitempty"`
	Vault     json.RawMessage `json:"vault,omitempty"`
	Context   json.RawMessage `json:"context,omitempty"`
	Path      json.RawMessage `json:"path,omitempty"`
	PageIndex json.RawMessage `json:"pageIndex,omitempty"`
}

type versionWire struct {
	Version                          string              `json:"version"`
	GitTag                           string              `json:"gitTag"`
	BuildTime                        string              `json:"buildTime"`
	VersionIsNew                     bool                `json:"versionIsNew"`
	VersionNewName                   string              `json:"versionNewName"`
	VersionNewLink                   string              `json:"versionNewLink"`
	VersionNewChangelog              string              `json:"versionNewChangelog"`
	VersionNewChangelogContent       string              `json:"versionNewChangelogContent"`
	VersionHistory                   []historicalVersion `json:"versionHistory"`
	PluginVersionNewName             string              `json:"pluginVersionNewName"`
	PluginVersionNewLink             string              `json:"pluginVersionNewLink"`
	PluginVersionNewChangelog        string              `json:"pluginVersionNewChangelog"`
	PluginVersionNewChangelogContent string              `json:"pluginVersionNewChangelogContent"`
	PluginVersionHistory             []historicalVersion `json:"pluginVersionHistory"`
}

type historicalVersion struct {
	Version          string `json:"version"`
	ChangelogContent string `json:"changelogContent"`
}

type healthWire struct {
	Status   string  `json:"status"`
	Version  string  `json:"version"`
	Uptime   float64 `json:"uptime"`
	Database string  `json:"database"`
}

type vaultWire struct {
	ID        int64  `json:"id"`
	Name      string `json:"vault"`
	NoteCount int64  `json:"noteCount"`
	NoteSize  int64  `json:"noteSize"`
	FileCount int64  `json:"fileCount"`
	FileSize  int64  `json:"fileSize"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type noteWire struct {
	ID            int64           `json:"id"`
	Path          string          `json:"path"`
	PathHash      string          `json:"pathHash"`
	Content       *string         `json:"content"`
	ContentHash   string          `json:"contentHash"`
	Version       int64           `json:"version"`
	Ctime         int64           `json:"ctime"`
	Mtime         int64           `json:"mtime"`
	Size          int64           `json:"size"`
	ClientName    string          `json:"clientName"`
	ClientType    string          `json:"clientType"`
	ClientVersion string          `json:"clientVersion"`
	LastTime      int64           `json:"lastTime"`
	UpdatedAt     json.RawMessage `json:"updatedAt"`
	CreatedAt     json.RawMessage `json:"createdAt"`
}

type noteReadWire struct {
	noteWire
	FileLinks map[string]string `json:"fileLinks"`
}

type noteListWire struct {
	List  []noteWire `json:"list"`
	Pager pagerWire  `json:"pager"`
}

type pagerWire struct {
	Page      int `json:"page"`
	PageSize  int `json:"pageSize"`
	TotalRows int `json:"totalRows"`
}

func (c *Client) call(ctx context.Context, method, path string, query url.Values, body any, operation string, output any) error {
	endpoint := c.baseURL
	endpoint.Path = c.baseURL.Path + path
	if query == nil {
		query = make(url.Values)
	}
	query.Set("client", ClientType)
	endpoint.RawQuery = query.Encode()
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return requestError(operation, err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), requestBody)
	if err != nil {
		return requestError(operation, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.apiToken)
	request.Header.Set("X-Client", ClientType)
	request.Header.Set("X-Client-Name", ClientName)
	request.Header.Set("X-Client-Version", ClientVersion)
	request.Header.Set("User-Agent", UserAgent)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return transportError(ctx, operation, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, c.bodyLimit+1))
		return statusError(operation, response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return contract(operation, errors.New("response content type is not application/json"))
	}
	limited := io.LimitReader(response.Body, c.bodyLimit+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return transportError(ctx, operation, err)
	}
	if int64(len(payload)) > c.bodyLimit {
		return contract(operation, errors.New("response body exceeds limit"))
	}
	var envelope envelopeWire
	if err := decodeStrict(payload, &envelope); err != nil {
		return contract(operation, err)
	}
	if envelope.Code == nil {
		return contract(operation, errors.New("response code is required"))
	}
	if envelope.Status == nil {
		if err := validateAppErrorEnvelope(envelope); err != nil {
			return contract(operation, err)
		}
		return businessError(operation, *envelope.Code)
	}
	if len(envelope.TraceID) != 0 || len(envelope.Timestamp) != 0 {
		return contract(operation, errors.New("response mixes success and application-error envelope fields"))
	}
	if !*envelope.Status {
		return businessError(operation, *envelope.Code)
	}
	if *envelope.Code != businessSuccess || len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		return contract(operation, errors.New("success envelope is inconsistent or missing data"))
	}
	if err := decodeStrict(envelope.Data, output); err != nil {
		return contract(operation, err)
	}
	return nil
}

func validateAppErrorEnvelope(envelope envelopeWire) error {
	if len(envelope.Message) == 0 || len(envelope.Timestamp) == 0 || len(envelope.Data) != 0 || len(envelope.Vault) != 0 ||
		len(envelope.Context) != 0 || len(envelope.Path) != 0 || len(envelope.PageIndex) != 0 {
		return errors.New("application-error envelope is incomplete or mixed")
	}
	var message string
	if err := decodeStrict(envelope.Message, &message); err != nil || strings.TrimSpace(message) == "" {
		return errors.New("application-error message is invalid")
	}
	var timestamp time.Time
	if err := decodeStrict(envelope.Timestamp, &timestamp); err != nil || timestamp.IsZero() {
		return errors.New("application-error timestamp is invalid")
	}
	if len(envelope.TraceID) != 0 {
		var traceID string
		if err := decodeStrict(envelope.TraceID, &traceID); err != nil || strings.TrimSpace(traceID) == "" {
			return errors.New("application-error trace ID is invalid")
		}
	}
	if len(envelope.Details) != 0 {
		var details []string
		if err := decodeStrict(envelope.Details, &details); err != nil {
			return errors.New("application-error details are invalid")
		}
		for _, detail := range details {
			if strings.TrimSpace(detail) == "" {
				return errors.New("application-error details are invalid")
			}
		}
	}
	return nil
}

func decodeStrict(payload []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func businessError(operation string, code int) error {
	category := CategoryUpstream
	switch {
	case code == businessRateLimit:
		category = CategoryRateLimited
	case code == businessAPINotFound:
		category = CategoryContractMismatch
	case code == 420 || code == businessNotFound:
		category = CategoryNotFound
	case code == businessNoteExists || code == 438 || code == 441 || code == 530:
		category = CategoryConflict
	case code == 305 || code == 444:
		category = CategoryValidation
	case code >= 306 && code <= 315:
		category = CategoryAuth
	case code == 0 || (code >= 300 && code <= 530):
	default:
		return contract(operation, errors.New("unknown business code"))
	}
	return &Error{category: category, operation: operation, code: code}
}

func statusError(operation string, status int) error {
	category := CategoryContractMismatch
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		category = CategoryAuth
	case status == http.StatusNotFound:
		category = CategoryNotFound
	case status == http.StatusTooManyRequests:
		category = CategoryRateLimited
	case status >= 500:
		category = CategoryUpstream
	}
	return &Error{category: category, operation: operation, status: status}
}

func transportError(ctx context.Context, operation string, err error) error {
	category := CategoryTransport
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		category = CategoryTimeout
	}
	return &Error{category: category, operation: operation, cause: err}
}

func requestError(operation string, err error) error {
	return &Error{category: CategoryValidation, operation: operation, cause: err}
}

func contract(operation string, err error) error {
	return &Error{category: CategoryContractMismatch, operation: operation, cause: err}
}
