package mcpadmin

import (
	"context"
	"net/http"
	"time"
)

// Service is the bounded management contract exposed by the live MCP handler.
type Service interface {
	http.Handler
	Snapshot(limit int) Snapshot
	Probe(ctx context.Context, token, host string) ProbeResult
}

type Descriptor struct {
	Kind            string   `json:"kind"`
	Name            string   `json:"name"`
	URI             string   `json:"uri,omitempty"`
	URITemplate     string   `json:"uri_template,omitempty"`
	Description     string   `json:"description"`
	RequiredScope   string   `json:"required_scope"`
	PrivacyOwners   []string `json:"privacy_owners"`
	ReadOnly        bool     `json:"read_only"`
	InputLimit      int64    `json:"input_limit_bytes,omitempty"`
	OutputLimit     int64    `json:"output_limit_bytes"`
	AuditName       string   `json:"audit_name"`
	HTTPOperationID string   `json:"http_operation_id"`
}

type Invocation struct {
	CompletedAt time.Time `json:"completed_at"`
	RequestID   string    `json:"request_id"`
	Descriptor  string    `json:"descriptor"`
	DeviceID    string    `json:"device_id,omitempty"`
	Result      string    `json:"result"`
	ErrorCode   string    `json:"error_code,omitempty"`
	DurationMS  int64     `json:"duration_ms"`
	Peer        string    `json:"peer"`
}

type Snapshot struct {
	ImplementationName    string       `json:"implementation_name"`
	ImplementationVersion string       `json:"implementation_version"`
	Transport             string       `json:"transport"`
	Stateless             bool         `json:"stateless"`
	JSONResponse          bool         `json:"json_response"`
	MaxRequestBodyBytes   int64        `json:"max_request_body_bytes"`
	StaticResourceCount   int          `json:"static_resource_count"`
	ResourceTemplateCount int          `json:"resource_template_count"`
	ResourceCount         int          `json:"resource_count"`
	ToolCount             int          `json:"tool_count"`
	Descriptors           []Descriptor `json:"descriptors"`
	RecentInvocations     []Invocation `json:"recent_invocations"`
}

type ProbeResult struct {
	OK         bool   `json:"ok"`
	HTTPStatus int    `json:"http_status"`
	RequestID  string `json:"request_id,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	ToolCount  int    `json:"tool_count"`
	DurationMS int64  `json:"duration_ms"`
}
