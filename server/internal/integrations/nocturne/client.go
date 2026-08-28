package nocturne

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/google/uuid"
)

type ErrorCategory string

const (
	CategoryAuth             ErrorCategory = "permanent_auth"
	CategoryNotFound         ErrorCategory = "not_found"
	CategoryActive           ErrorCategory = "active_conflict"
	CategoryValidation       ErrorCategory = "validation"
	CategoryRateLimited      ErrorCategory = "rate_limited"
	CategoryUpstream         ErrorCategory = "upstream"
	CategoryTimeout          ErrorCategory = "timeout"
	CategoryTransport        ErrorCategory = "transport"
	CategoryContractMismatch ErrorCategory = "contract_mismatch"
)

type Error struct {
	category           ErrorCategory
	status             int
	operation          string
	cause              error
	mutationDispatched bool
}

func (e *Error) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("nocturne operation failed: operation=%s category=%s status=%d", e.operation, e.category, e.status)
	}
	return fmt.Sprintf("nocturne operation failed: operation=%s category=%s", e.operation, e.category)
}
func (e *Error) Unwrap() error            { return e.cause }
func (e *Error) Category() string         { return string(e.category) }
func (e *Error) MutationDispatched() bool { return e.mutationDispatched }
func (e *Error) Permanent() bool {
	return e.category == CategoryAuth || e.category == CategoryValidation || e.category == CategoryContractMismatch
}
func Category(err error) ErrorCategory {
	var target *Error
	if errors.As(err, &target) {
		return target.category
	}
	return CategoryTransport
}
func IsNotFound(err error) bool { return Category(err) == CategoryNotFound }
func IsActive(err error) bool   { return Category(err) == CategoryActive }
func IsContractMismatch(err error) bool {
	var classified interface{ Category() string }
	return errors.As(err, &classified) && classified.Category() == string(CategoryContractMismatch)
}
func MutationDispatched(err error) bool {
	var target interface{ MutationDispatched() bool }
	return errors.As(err, &target) && target.MutationDispatched()
}

type Options struct {
	BaseURL          *url.URL
	APIToken         string
	MaintenanceToken string
	Timeout          time.Duration
	BodyLimit        int64
	Namespace        string
	Domain           string
	ParentPath       string
	Priority         int
	Disclosure       string
	HTTPClient       *http.Client
}

type Client struct {
	baseURL          url.URL
	apiToken         string
	maintenanceToken string
	httpClient       *http.Client
	bodyLimit        int64
	namespace        string
	domain           string
	parentPath       string
	priority         int
	disclosure       string
}

func New(options Options) (*Client, error) {
	if options.BaseURL == nil || (options.BaseURL.Scheme != "http" && options.BaseURL.Scheme != "https") ||
		options.BaseURL.Host == "" || options.BaseURL.User != nil || options.BaseURL.RawQuery != "" || options.BaseURL.Fragment != "" {
		return nil, errors.New("nocturne base URL must be absolute HTTP(S) without credentials, query, or fragment")
	}
	if len(options.APIToken) < 32 || strings.TrimSpace(options.APIToken) != options.APIToken {
		return nil, errors.New("nocturne API token must contain at least 32 serialized characters")
	}
	if len([]byte(options.MaintenanceToken)) < 32 || strings.TrimSpace(options.MaintenanceToken) != options.MaintenanceToken {
		return nil, errors.New("nocturne maintenance token must contain at least 256 bits")
	}
	if options.APIToken == options.MaintenanceToken {
		return nil, errors.New("nocturne API and maintenance tokens must differ")
	}
	if options.Timeout <= 0 || options.BodyLimit <= 0 {
		return nil, errors.New("nocturne timeout and response body limit must be positive")
	}
	if strings.TrimSpace(options.Namespace) == "" || strings.TrimSpace(options.Domain) == "" ||
		strings.Trim(options.ParentPath, "/") != options.ParentPath || options.ParentPath == "" ||
		options.Priority < 0 || strings.TrimSpace(options.Disclosure) == "" {
		return nil, errors.New("nocturne fixed namespace, domain, parent, priority, and disclosure are required")
	}
	base := *options.BaseURL
	base.Path = strings.TrimRight(base.Path, "/")
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	clone := *httpClient
	clone.Timeout = options.Timeout
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		baseURL: base, apiToken: options.APIToken, maintenanceToken: options.MaintenanceToken,
		httpClient: &clone, bodyLimit: options.BodyLimit, namespace: options.Namespace,
		domain: options.Domain, parentPath: options.ParentPath, priority: options.Priority,
		disclosure: options.Disclosure,
	}, nil
}

func (c *Client) Health(ctx context.Context) error {
	var wire healthWire
	err := c.call(ctx, http.MethodGet, "/health", nil, nil, false, "health", &wire)
	if IsNotFound(err) {
		err = c.call(ctx, http.MethodGet, "/api/health", nil, nil, false, "health", &wire)
	}
	if err != nil {
		return err
	}
	if wire.Status == nil || wire.Database == nil || *wire.Status != "ok" || *wire.Database != "connected" {
		return contract("health", errors.New("required health fields are missing or incompatible"))
	}
	return nil
}

func (c *Client) Capabilities(ctx context.Context) (memory.NocturneCapabilities, error) {
	var wire capabilitiesWire
	if err := c.call(ctx, http.MethodGet, "/internal/edu-agent/capabilities", nil, nil, true, "capabilities", &wire); err != nil {
		return memory.NocturneCapabilities{}, err
	}
	if wire.UpstreamCommit == nil || wire.CompatRevision == nil || wire.BootEpoch == nil ||
		*wire.UpstreamCommit != ImageUpstreamCommit || *wire.CompatRevision != ImageCompatibilityRevision || strings.TrimSpace(*wire.BootEpoch) == "" {
		return memory.NocturneCapabilities{}, contract("capabilities", errors.New("required capability fields are missing or incompatible"))
	}
	return memory.NocturneCapabilities{UpstreamCommit: *wire.UpstreamCommit, CompatRevision: *wire.CompatRevision, BootEpoch: *wire.BootEpoch}, nil
}

// Preflight verifies the immutable image capability and production route boundary
// without invoking any business or maintenance mutation.
func (c *Client) Preflight(ctx context.Context) error {
	if err := c.Health(ctx); err != nil {
		return preflightFailure("preflight_health", err)
	}
	if _, err := c.Capabilities(ctx); err != nil {
		return preflightFailure("preflight_capabilities", err)
	}
	probes := []struct {
		method string
		path   string
		token  string
		want   int
	}{
		{http.MethodGet, "/sse", c.apiToken, http.StatusNotFound},
		{http.MethodPost, "/messages", c.apiToken, http.StatusNotFound},
		{http.MethodPost, "/mcp", c.apiToken, http.StatusNotFound},
		{http.MethodGet, "/api/settings", c.apiToken, http.StatusNotFound},
		{http.MethodPost, "/api/browse/domains", c.apiToken, http.StatusNotFound},
		{http.MethodGet, "/internal/edu-agent/capabilities", "", http.StatusUnauthorized},
		{http.MethodGet, "/internal/edu-agent/capabilities", c.apiToken, http.StatusUnauthorized},
		{http.MethodGet, "/internal/edu-agent/backups", c.apiToken, http.StatusUnauthorized},
		{http.MethodGet, "/api/browse/node", c.maintenanceToken, http.StatusUnauthorized},
	}
	for _, probe := range probes {
		status, err := c.probeStatus(ctx, probe.method, probe.path, probe.token)
		if err != nil {
			return preflightFailure("preflight_route_boundary", err)
		}
		if status != probe.want {
			return contract("preflight_route_boundary", errors.New("route or authorization boundary mismatch"))
		}
	}
	return nil
}

func (c *Client) probeStatus(ctx context.Context, method, path, token string) (int, error) {
	endpoint := c.baseURL
	endpoint.Path = c.baseURL.Path + path
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), nil)
	if err != nil {
		return 0, requestError("preflight_route_boundary", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Namespace", c.namespace)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return 0, transportError(ctx, "preflight_route_boundary", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, c.bodyLimit+1))
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		return 0, statusError("preflight_route_boundary", response.StatusCode)
	}
	return response.StatusCode, nil
}

func preflightFailure(operation string, err error) error {
	switch Category(err) {
	case CategoryTransport, CategoryTimeout, CategoryRateLimited, CategoryUpstream:
		return err
	default:
		return contract(operation, err)
	}
}

func (c *Client) GetNode(ctx context.Context, path string) (memory.RemoteNode, error) {
	if err := validatePath(path); err != nil {
		return memory.RemoteNode{}, requestError("get_node", err)
	}
	query := url.Values{"domain": {c.domain}, "path": {path}}
	var wire nodeEnvelopeWire
	if err := c.call(ctx, http.MethodGet, "/api/browse/node", query, nil, false, "get_node", &wire); err != nil {
		return memory.RemoteNode{}, err
	}
	if err := validateNodeEnvelope(wire); err != nil {
		return memory.RemoteNode{}, contract("get_node", err)
	}
	node := wire.Node
	if *node.Path != path || *node.Domain != c.domain || *node.URI != c.domain+"://"+path || *node.IsVirtual ||
		uuid.Validate(*node.NodeUUID) != nil || node.Disclosure == nil {
		return memory.RemoteNode{}, contract("get_node", errors.New("node identity or fixed routing fields mismatch"))
	}
	return memory.RemoteNode{NodeID: *node.NodeUUID, Path: *node.Path, URI: *node.URI, Content: *node.Content, Priority: *node.Priority, Disclosure: *node.Disclosure}, nil
}

func (c *Client) EnsureParent(ctx context.Context) error {
	if _, err := c.GetNode(ctx, c.parentPath); err == nil {
		return nil
	} else if !IsNotFound(err) {
		return err
	}
	body := createNodeWire{
		ParentPath: "", Content: "edu-agent managed root", Priority: c.priority,
		Disclosure: "edu-agent infrastructure root", Title: c.parentPath, Domain: c.domain,
	}
	var wire mutationWire
	createErr := c.call(ctx, http.MethodPost, "/api/browse/node", nil, body, false, "ensure_parent", &wire)
	expectedURI := c.domain + "://" + c.parentPath
	if createErr == nil && (wire.Success == nil || !*wire.Success || wire.URI == nil || *wire.URI != expectedURI || wire.MemoryID == nil || *wire.MemoryID <= 0) {
		createErr = mutationContract("ensure_parent", errors.New("parent response is incomplete"))
	}
	if _, readErr := c.GetNode(ctx, c.parentPath); readErr == nil {
		return nil
	} else if createErr != nil {
		return createErr
	} else {
		return readErr
	}
}

func (c *Client) CreateNode(ctx context.Context, title, content string) (memory.RemoteMutation, error) {
	if uuid.Validate(title) != nil || strings.TrimSpace(content) == "" {
		return memory.RemoteMutation{}, requestError("create_node", errors.New("canonical UUID title and content are required"))
	}
	body := createNodeWire{ParentPath: c.parentPath, Content: content, Priority: c.priority, Disclosure: c.disclosure, Title: title, Domain: c.domain}
	var wire mutationWire
	if err := c.call(ctx, http.MethodPost, "/api/browse/node", nil, body, false, "create_node", &wire); err != nil {
		return memory.RemoteMutation{}, err
	}
	expectedURI := c.domain + "://" + c.parentPath + "/" + title
	if wire.Success == nil || !*wire.Success || wire.URI == nil || *wire.URI != expectedURI || wire.MemoryID == nil || *wire.MemoryID <= 0 {
		return memory.RemoteMutation{}, mutationContract("create_node", errors.New("create response is incomplete or mismatched"))
	}
	return memory.RemoteMutation{URI: *wire.URI, MemoryID: *wire.MemoryID}, nil
}

func (c *Client) UpdateNode(ctx context.Context, path, content string) (memory.RemoteMutation, error) {
	if err := validatePath(path); err != nil || strings.TrimSpace(content) == "" {
		return memory.RemoteMutation{}, requestError("update_node", errors.New("valid path and content are required"))
	}
	query := url.Values{"domain": {c.domain}, "path": {path}}
	body := updateNodeWire{Content: content, Priority: c.priority, Disclosure: c.disclosure}
	var wire mutationWire
	if err := c.call(ctx, http.MethodPut, "/api/browse/node", query, body, false, "update_node", &wire); err != nil {
		return memory.RemoteMutation{}, err
	}
	if wire.Success == nil || !*wire.Success || wire.URI != nil || wire.MemoryID == nil || *wire.MemoryID <= 0 {
		return memory.RemoteMutation{}, mutationContract("update_node", errors.New("update response is incomplete or mismatched"))
	}
	return memory.RemoteMutation{URI: c.domain + "://" + path, MemoryID: *wire.MemoryID}, nil
}

func (c *Client) DeletePath(ctx context.Context, path string) error {
	if err := validatePath(path); err != nil {
		return requestError("delete_path", err)
	}
	query := url.Values{"domain": {c.domain}, "path": {path}}
	var wire deleteWire
	if err := c.call(ctx, http.MethodDelete, "/api/browse/node", query, nil, false, "delete_path", &wire); err != nil {
		return err
	}
	if wire.Success == nil || !*wire.Success || wire.URI == nil || *wire.URI != c.domain+"://"+path {
		return mutationContract("delete_path", errors.New("delete response is incomplete or mismatched"))
	}
	return nil
}

func (c *Client) Search(ctx context.Context, q string) ([]memory.RemoteSearchResult, error) {
	if strings.TrimSpace(q) == "" {
		return nil, requestError("search", errors.New("search query is required"))
	}
	query := url.Values{"q": {q}, "domain": {c.domain}, "limit": {"100"}}
	var wire searchWire
	if err := c.call(ctx, http.MethodGet, "/api/browse/search", query, nil, false, "search", &wire); err != nil {
		return nil, err
	}
	if wire.Query == nil || *wire.Query != q || wire.Count == nil || wire.Results == nil || *wire.Count != len(wire.Results) {
		return nil, contract("search", errors.New("search response is incomplete or mismatched"))
	}
	results := make([]memory.RemoteSearchResult, 0, len(wire.Results))
	for _, item := range wire.Results {
		if item.Domain == nil || item.Path == nil || item.URI == nil || item.Name == nil || item.Snippet == nil || item.Priority == nil ||
			*item.Domain != c.domain || *item.URI != c.domain+"://"+*item.Path {
			return nil, contract("search", errors.New("search result is incomplete or mismatched"))
		}
		results = append(results, memory.RemoteSearchResult{Path: *item.Path, URI: *item.URI})
	}
	return results, nil
}

func (c *Client) ListOrphans(ctx context.Context) ([]memory.RemoteOrphan, error) {
	var wire []orphanWire
	if err := c.call(ctx, http.MethodGet, "/api/maintenance/orphans", nil, nil, false, "list_orphans", &wire); err != nil {
		return nil, err
	}
	if wire == nil {
		return nil, contract("list_orphans", errors.New("orphan list is missing"))
	}
	result := make([]memory.RemoteOrphan, 0, len(wire))
	for _, item := range wire {
		value, err := validateOrphan(item)
		if err != nil {
			return nil, contract("list_orphans", err)
		}
		result = append(result, value)
	}
	return result, nil
}

func (c *Client) OrphanDetail(ctx context.Context, id int64) (memory.RemoteOrphan, error) {
	if id <= 0 {
		return memory.RemoteOrphan{}, requestError("orphan_detail", errors.New("positive memory ID is required"))
	}
	var wire orphanWire
	path := fmt.Sprintf("/api/maintenance/orphans/%d", id)
	if err := c.call(ctx, http.MethodGet, path, nil, nil, false, "orphan_detail", &wire); err != nil {
		return memory.RemoteOrphan{}, err
	}
	value, err := validateOrphan(wire)
	if err != nil || value.MemoryID != id {
		return memory.RemoteOrphan{}, contract("orphan_detail", errors.New("orphan detail is incomplete or mismatched"))
	}
	return value, nil
}

func (c *Client) PermanentDelete(ctx context.Context, id int64) (memory.RemoteDeleteResult, error) {
	if id <= 0 {
		return memory.RemoteDeleteResult{}, requestError("permanent_delete", errors.New("positive memory ID is required"))
	}
	var wire permanentDeleteWire
	path := fmt.Sprintf("/api/maintenance/orphans/%d", id)
	if err := c.call(ctx, http.MethodDelete, path, nil, nil, false, "permanent_delete", &wire); err != nil {
		return memory.RemoteDeleteResult{}, err
	}
	if wire.DeletedMemoryID == nil || *wire.DeletedMemoryID != id || wire.RowsBefore == nil || wire.RowsAfter == nil {
		return memory.RemoteDeleteResult{}, mutationContract("permanent_delete", errors.New("permanent delete response is incomplete or mismatched"))
	}
	result := memory.RemoteDeleteResult{DeletedMemoryID: id}
	if wire.ChainRepairedTo != nil {
		result.ChainRepairedTo = *wire.ChainRepairedTo
	}
	return result, nil
}

func (c *Client) References(ctx context.Context, nodeID string) (memory.RemoteReferences, error) {
	if uuid.Validate(nodeID) != nil {
		return memory.RemoteReferences{}, requestError("references", errors.New("canonical node UUID is required"))
	}
	var wire referencesWire
	path := "/internal/edu-agent/nodes/" + nodeID + "/references"
	if err := c.call(ctx, http.MethodGet, path, nil, nil, true, "references", &wire); err != nil {
		return memory.RemoteReferences{}, err
	}
	if wire.NodeUUID == nil || *wire.NodeUUID != nodeID || wire.Complete == nil || !*wire.Complete || wire.ActiveMemoryID == nil ||
		wire.MemoryIDs == nil || wire.Paths == nil || wire.EdgeIDs == nil || wire.GlossaryKeywords == nil ||
		wire.SearchDocumentIDs == nil || wire.AccessLogIDs == nil || wire.BootURIs == nil || wire.ReviewReferences == nil {
		return memory.RemoteReferences{}, contract("references", errors.New("reference enumeration is incomplete"))
	}
	result := memory.RemoteReferences{NodeID: nodeID, Complete: *wire.Complete, ActiveMemoryID: *wire.ActiveMemoryID,
		MemoryIDs: append([]int64(nil), wire.MemoryIDs...), EdgeIDs: append([]string(nil), wire.EdgeIDs...),
		GlossaryKeywords: append([]string(nil), wire.GlossaryKeywords...), SearchDocumentIDs: append([]string(nil), wire.SearchDocumentIDs...),
		AccessLogIDs: append([]string(nil), wire.AccessLogIDs...), ReviewReferences: append([]string(nil), wire.ReviewReferences...)}
	for _, ref := range wire.Paths {
		if ref.Namespace == nil || ref.Domain == nil || ref.Path == nil || ref.URI == nil || ref.Alias == nil {
			return memory.RemoteReferences{}, contract("references", errors.New("path reference is incomplete"))
		}
		result.Paths = append(result.Paths, memory.RemotePathReference{Namespace: *ref.Namespace, Domain: *ref.Domain, Path: *ref.Path, URI: *ref.URI, Alias: *ref.Alias})
	}
	for _, ref := range wire.BootURIs {
		if ref.Preset == nil || ref.Namespace == nil || ref.URI == nil {
			return memory.RemoteReferences{}, contract("references", errors.New("boot reference is incomplete"))
		}
		result.BootURIs = append(result.BootURIs, memory.RemoteBootReference{Preset: *ref.Preset, Namespace: *ref.Namespace, URI: *ref.URI})
	}
	return result, nil
}

func (c *Client) ClearReviewReferences(ctx context.Context, nodeID string) error {
	if uuid.Validate(nodeID) != nil {
		return requestError("clear_review_references", errors.New("canonical node UUID is required"))
	}
	var wire successWire
	path := "/internal/edu-agent/nodes/" + nodeID + "/review-reference"
	if err := c.call(ctx, http.MethodDelete, path, nil, nil, true, "clear_review_references", &wire); err != nil {
		return err
	}
	if wire.Success == nil || !*wire.Success {
		return contract("clear_review_references", errors.New("review cleanup response is incomplete"))
	}
	return nil
}

func (c *Client) Backups(ctx context.Context) (memory.BackupInventory, error) {
	var wire backupsWire
	if err := c.call(ctx, http.MethodGet, "/internal/edu-agent/backups", nil, nil, true, "backups", &wire); err != nil {
		return memory.BackupInventory{}, err
	}
	if wire.Validated == nil || wire.ManifestSHA256 == nil || wire.Artifacts == nil || !validBackupDigest(*wire.ManifestSHA256) {
		return memory.BackupInventory{}, contract("backups", errors.New("backup inventory is incomplete"))
	}
	result := memory.BackupInventory{Validated: *wire.Validated, ManifestSHA256: *wire.ManifestSHA256}
	seen := make(map[string]struct{}, len(wire.Artifacts))
	for _, item := range wire.Artifacts {
		if item.Path == nil || item.CreatedAt == nil || item.Size == nil || item.SHA256 == nil || item.LearnerGeneration == nil || item.WrappedKeyID == nil ||
			!validBackupPath(*item.Path) || *item.Size < 0 || *item.LearnerGeneration < 1 || !canonicalUUID(*item.WrappedKeyID) || !validBackupDigest(*item.SHA256) {
			return memory.BackupInventory{}, contract("backups", errors.New("backup artifact is incomplete"))
		}
		if _, duplicate := seen[*item.Path]; duplicate {
			return memory.BackupInventory{}, contract("backups", errors.New("backup artifact path is duplicated"))
		}
		seen[*item.Path] = struct{}{}
		createdAt, err := time.Parse(time.RFC3339Nano, *item.CreatedAt)
		if err != nil || createdAt.Location() != time.UTC || createdAt.Format(time.RFC3339Nano) != *item.CreatedAt {
			return memory.BackupInventory{}, contract("backups", errors.New("backup timestamp is not canonical UTC"))
		}
		result.Artifacts = append(result.Artifacts, memory.ManagedBackup{Path: *item.Path, CreatedAt: createdAt, Size: *item.Size, SHA256: *item.SHA256, LearnerGeneration: *item.LearnerGeneration, WrappedKeyID: *item.WrappedKeyID})
	}
	return result, nil
}

func (c *Client) PruneBackups(ctx context.Context, request memory.BackupPruneRequest) (memory.BackupPruneResult, error) {
	if !canonicalUUID(request.OperationID) || request.Cutoff.IsZero() || request.Cutoff.Location() != time.UTC ||
		!validBackupDigest(request.ExpectedManifestSHA256) || validateBackupPathSet(request.Paths, true) != nil {
		return memory.BackupPruneResult{}, requestError("prune_backups", errors.New("precise backup prune request is invalid"))
	}
	body := backupPruneRequestWire{
		OperationID: request.OperationID, Cutoff: request.Cutoff.Format(time.RFC3339Nano),
		ExpectedManifestSHA256: request.ExpectedManifestSHA256, Paths: append([]string(nil), request.Paths...),
	}
	var wire backupPruneResultWire
	if err := c.call(ctx, http.MethodPost, "/internal/edu-agent/backups/prune", nil, body, true, "prune_backups", &wire); err != nil {
		return memory.BackupPruneResult{}, err
	}
	if wire.OperationID == nil || *wire.OperationID != request.OperationID || wire.DeletedPaths == nil || wire.ManifestSHA256 == nil ||
		validateBackupPathSet(*wire.DeletedPaths, false) != nil || !validBackupDigest(*wire.ManifestSHA256) {
		return memory.BackupPruneResult{}, mutationContract("prune_backups", errors.New("backup prune response is incomplete or mismatched"))
	}
	return memory.BackupPruneResult{
		OperationID: *wire.OperationID, DeletedPaths: append([]string(nil), (*wire.DeletedPaths)...), ManifestSHA256: *wire.ManifestSHA256,
	}, nil
}

func (c *Client) call(ctx context.Context, method, path string, query url.Values, body any, maintenance bool, operation string, target any) error {
	mutation := method == http.MethodPost || method == http.MethodPut || method == http.MethodDelete
	endpoint := c.baseURL
	endpoint.Path = c.baseURL.Path + path
	endpoint.RawQuery = query.Encode()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return requestError(operation, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return requestError(operation, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Namespace", c.namespace)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	token := c.apiToken
	if maintenance {
		token = c.maintenanceToken
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return markMutationDispatched(transportError(ctx, operation, err), mutation)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, c.bodyLimit+1))
		return markMutationDispatched(statusError(operation, response.StatusCode), mutation)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return markMutationDispatched(contract(operation, errors.New("response content type is not application/json")), mutation)
	}
	limited := io.LimitReader(response.Body, c.bodyLimit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return markMutationDispatched(transportError(ctx, operation, err), mutation)
	}
	if int64(len(data)) > c.bodyLimit {
		return markMutationDispatched(contract(operation, errors.New("response body exceeds configured limit")), mutation)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return markMutationDispatched(contract(operation, errors.New("response JSON does not match the fixed contract")), mutation)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return markMutationDispatched(contract(operation, errors.New("response contains trailing JSON")), mutation)
	}
	return nil
}

func statusError(operation string, status int) error {
	category := CategoryContractMismatch
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		category = CategoryAuth
	case status == http.StatusNotFound:
		category = CategoryNotFound
	case status == http.StatusConflict:
		category = CategoryActive
	case status == http.StatusUnprocessableEntity:
		category = CategoryValidation
	case status == http.StatusTooManyRequests:
		category = CategoryRateLimited
	case status >= 500:
		category = CategoryUpstream
	}
	return &Error{category: category, status: status, operation: operation}
}
func transportError(ctx context.Context, operation string, err error) error {
	category := CategoryTransport
	cause := error(nil)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		category, cause = CategoryTimeout, context.DeadlineExceeded
	} else {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			category, cause = CategoryTimeout, netErr
		}
	}
	return &Error{category: category, operation: operation, cause: cause}
}
func requestError(operation string, cause error) error {
	return &Error{category: CategoryValidation, operation: operation, cause: cause}
}
func contract(operation string, cause error) error {
	return &Error{category: CategoryContractMismatch, operation: operation, cause: cause}
}
func mutationContract(operation string, cause error) error {
	return &Error{category: CategoryContractMismatch, operation: operation, cause: cause, mutationDispatched: true}
}
func markMutationDispatched(err error, dispatched bool) error {
	if !dispatched || err == nil {
		return err
	}
	var target *Error
	if !errors.As(err, &target) {
		return err
	}
	copyValue := *target
	copyValue.mutationDispatched = true
	return &copyValue
}
func validatePath(path string) error {
	if path == "" || strings.Trim(path, "/") != path || strings.Contains(path, "//") || strings.ContainsAny(path, "?#") {
		return errors.New("canonical non-root path is required")
	}
	return nil
}

func canonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

func validBackupDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validBackupPath(path string) bool {
	return validFlatName(path) && path != backupInventoryName && path != backupLockName
}

func validateBackupPathSet(paths []string, requireNonEmpty bool) error {
	if requireNonEmpty && len(paths) == 0 {
		return errors.New("backup prune paths are required")
	}
	for index, path := range paths {
		if !validBackupPath(path) || index > 0 && paths[index-1] >= path {
			return errors.New("backup prune paths must be sorted, unique, and safe")
		}
	}
	return nil
}

type healthWire struct{ Status, Database *string }
type capabilitiesWire struct {
	UpstreamCommit *string `json:"upstream_commit"`
	CompatRevision *string `json:"compat_revision"`
	BootEpoch      *string `json:"boot_epoch"`
}
type nodeEnvelopeWire struct {
	Node        nodeWire         `json:"node"`
	Children    []nodeChildWire  `json:"children"`
	Breadcrumbs []breadcrumbWire `json:"breadcrumbs"`
}
type nodeWire struct {
	Path             *string           `json:"path"`
	Domain           *string           `json:"domain"`
	URI              *string           `json:"uri"`
	Name             *string           `json:"name"`
	Content          *string           `json:"content"`
	Priority         *int              `json:"priority"`
	Disclosure       *string           `json:"disclosure"`
	CreatedAt        *string           `json:"created_at"`
	IsVirtual        *bool             `json:"is_virtual"`
	Aliases          []string          `json:"aliases"`
	NodeUUID         *string           `json:"node_uuid"`
	GlossaryKeywords []string          `json:"glossary_keywords"`
	GlossaryMatches  []json.RawMessage `json:"glossary_matches"`
}
type nodeChildWire struct {
	Domain              *string `json:"domain"`
	Path                *string `json:"path"`
	URI                 *string `json:"uri"`
	Name                *string `json:"name"`
	Priority            *int    `json:"priority"`
	Disclosure          *string `json:"disclosure"`
	ContentSnippet      *string `json:"content_snippet"`
	ApproxChildrenCount *int    `json:"approx_children_count"`
}
type breadcrumbWire struct{ Path, Label *string }

func validateNodeEnvelope(w nodeEnvelopeWire) error {
	n := w.Node
	if n.Path == nil || n.Domain == nil || n.URI == nil || n.Name == nil || n.Content == nil || n.Priority == nil || n.IsVirtual == nil ||
		n.Aliases == nil || n.NodeUUID == nil || n.GlossaryKeywords == nil || n.GlossaryMatches == nil || w.Children == nil || w.Breadcrumbs == nil {
		return errors.New("required node fields are missing")
	}
	for _, child := range w.Children {
		if child.Domain == nil || child.Path == nil || child.URI == nil || child.Name == nil || child.Priority == nil || child.ContentSnippet == nil || child.ApproxChildrenCount == nil {
			return errors.New("required child fields are missing")
		}
	}
	for _, crumb := range w.Breadcrumbs {
		if crumb.Path == nil || crumb.Label == nil {
			return errors.New("required breadcrumb fields are missing")
		}
	}
	return nil
}

type createNodeWire struct {
	ParentPath string `json:"parent_path"`
	Content    string `json:"content"`
	Priority   int    `json:"priority"`
	Disclosure string `json:"disclosure"`
	Title      string `json:"title"`
	Domain     string `json:"domain"`
}
type updateNodeWire struct {
	Content    string `json:"content"`
	Priority   int    `json:"priority"`
	Disclosure string `json:"disclosure"`
}
type mutationWire struct {
	Success  *bool   `json:"success"`
	URI      *string `json:"uri,omitempty"`
	MemoryID *int64  `json:"memory_id"`
}
type deleteWire struct {
	Success *bool   `json:"success"`
	URI     *string `json:"uri"`
}
type searchWire struct {
	Query   *string            `json:"query"`
	Results []searchResultWire `json:"results"`
	Count   *int               `json:"count"`
}
type searchResultWire struct {
	Domain, Path, URI, Name, Snippet *string
	Priority                         *int
	Disclosure                       *string
}
type migrationTargetWire struct {
	ID             *int64   `json:"id"`
	Paths          []string `json:"paths"`
	ContentSnippet *string  `json:"content_snippet,omitempty"`
	Content        *string  `json:"content,omitempty"`
	CreatedAt      *string  `json:"created_at,omitempty"`
}
type orphanWire struct {
	ID              *int64               `json:"id"`
	NodeUUID        *string              `json:"node_uuid"`
	ContentSnippet  *string              `json:"content_snippet,omitempty"`
	Content         *string              `json:"content,omitempty"`
	CreatedAt       *string              `json:"created_at"`
	Deprecated      *bool                `json:"deprecated"`
	MigratedTo      *int64               `json:"migrated_to"`
	Category        *string              `json:"category"`
	MigrationTarget *migrationTargetWire `json:"migration_target"`
}

func validateOrphan(w orphanWire) (memory.RemoteOrphan, error) {
	if w.ID == nil || *w.ID <= 0 || w.NodeUUID == nil || uuid.Validate(*w.NodeUUID) != nil || w.CreatedAt == nil || w.Deprecated == nil || w.Category == nil {
		return memory.RemoteOrphan{}, errors.New("required orphan fields are missing")
	}
	value := memory.RemoteOrphan{MemoryID: *w.ID, NodeID: *w.NodeUUID, Deprecated: *w.Deprecated, Category: *w.Category}
	if w.MigratedTo != nil {
		value.MigratedTo = *w.MigratedTo
	}
	return value, nil
}

type permanentDeleteWire struct {
	DeletedMemoryID *int64                     `json:"deleted_memory_id"`
	ChainRepairedTo *int64                     `json:"chain_repaired_to"`
	RowsBefore      map[string]json.RawMessage `json:"rows_before"`
	RowsAfter       map[string]json.RawMessage `json:"rows_after"`
}
type referencePathWire struct {
	Namespace, Domain, Path, URI *string
	Alias                        *bool
}
type bootReferenceWire struct{ Preset, Namespace, URI *string }
type referencesWire struct {
	NodeUUID          *string             `json:"node_uuid"`
	Complete          *bool               `json:"complete"`
	ActiveMemoryID    *int64              `json:"active_memory_id"`
	MemoryIDs         []int64             `json:"memory_ids"`
	Paths             []referencePathWire `json:"paths"`
	EdgeIDs           []string            `json:"edge_ids"`
	GlossaryKeywords  []string            `json:"glossary_keywords"`
	SearchDocumentIDs []string            `json:"search_document_ids"`
	AccessLogIDs      []string            `json:"access_log_ids"`
	BootURIs          []bootReferenceWire `json:"boot_uris"`
	ReviewReferences  []string            `json:"review_references"`
}
type successWire struct {
	Success *bool `json:"success"`
}
type backupWire struct {
	Path              *string `json:"path"`
	CreatedAt         *string `json:"created_at"`
	SHA256            *string `json:"sha256"`
	WrappedKeyID      *string `json:"wrapped_key_id"`
	Size              *int64  `json:"size_bytes"`
	LearnerGeneration *int64  `json:"learner_generation"`
}
type backupsWire struct {
	Validated      *bool        `json:"validated"`
	ManifestSHA256 *string      `json:"manifest_sha256"`
	Artifacts      []backupWire `json:"artifacts"`
}
type backupPruneRequestWire struct {
	OperationID            string   `json:"operation_id"`
	Cutoff                 string   `json:"cutoff"`
	ExpectedManifestSHA256 string   `json:"expected_manifest_sha256"`
	Paths                  []string `json:"paths"`
}
type backupPruneResultWire struct {
	OperationID    *string   `json:"operation_id"`
	DeletedPaths   *[]string `json:"deleted_paths"`
	ManifestSHA256 *string   `json:"manifest_sha256"`
}

var _ memory.NocturneRemote = (*Client)(nil)
var _ = slices.Contains[[]string, string]
