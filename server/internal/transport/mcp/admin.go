package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/transport/mcpadmin"
)

const (
	implementationName       = "edu-agent"
	implementationVersion    = "mcp-surface-v1"
	maxRecentInvocationCount = 100
)

func (h *Handler) Snapshot(limit int) mcpadmin.Snapshot {
	descriptors := make([]mcpadmin.Descriptor, 0, len(descriptorCatalog))
	staticResources, resourceTemplates, tools := 0, 0, 0
	for _, descriptor := range Catalog() {
		owners := make([]string, len(descriptor.PrivacyOwners))
		for index, owner := range descriptor.PrivacyOwners {
			owners[index] = string(owner)
		}
		descriptors = append(descriptors, mcpadmin.Descriptor{
			Kind: string(descriptor.Kind), Name: descriptor.Name, URI: descriptor.URI,
			URITemplate: descriptor.URITemplate, Description: descriptor.Description,
			RequiredScope: descriptor.RequiredScope, PrivacyOwners: owners, ReadOnly: descriptor.ReadOnly,
			InputLimit: descriptor.InputLimit, OutputLimit: descriptor.OutputLimit,
			AuditName: descriptor.AuditName, HTTPOperationID: descriptor.HTTPOperationID,
		})
		switch descriptor.Kind {
		case DescriptorResource:
			staticResources++
		case DescriptorResourceTemplate:
			resourceTemplates++
		case DescriptorTool:
			tools++
		}
	}
	return mcpadmin.Snapshot{
		ImplementationName: implementationName, ImplementationVersion: implementationVersion,
		Transport: "streamable_http", Stateless: true, JSONResponse: true,
		MaxRequestBodyBytes: h.maxRequestBody,
		StaticResourceCount: staticResources, ResourceTemplateCount: resourceTemplates,
		ResourceCount: staticResources + resourceTemplates, ToolCount: tools,
		Descriptors: descriptors, RecentInvocations: h.recentInvocationSnapshot(limit),
	}
}

func (h *Handler) Probe(ctx context.Context, token, host string) mcpadmin.ProbeResult {
	started := time.Now()
	result := mcpadmin.ProbeResult{}
	body := []byte(`{"jsonrpc":"2.0","id":"admin-probe","method":"tools/list","params":{}}`)
	target := "http://" + host + "/mcp"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		result.ErrorCode = "invalid_endpoint"
		result.DurationMS = time.Since(started).Milliseconds()
		return result
	}
	request.Host = host
	request.RemoteAddr = "127.0.0.1:0"
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 80}))

	response := newBufferedResponse()
	h.ServeHTTP(response, request)
	result.HTTPStatus = response.statusCode()
	result.RequestID = response.Header().Get("X-Request-Id")
	result.DurationMS = time.Since(started).Milliseconds()

	var envelope struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
		Error *struct {
			Code json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.body.Bytes(), &envelope); err != nil {
		result.ErrorCode = "invalid_mcp_response"
		return result
	}
	if envelope.Error != nil {
		result.ErrorCode = probeErrorCode(envelope.Error.Code)
		return result
	}
	if result.HTTPStatus < http.StatusOK || result.HTTPStatus >= http.StatusMultipleChoices {
		result.ErrorCode = "protocol_error"
		return result
	}
	result.OK = true
	result.ToolCount = len(envelope.Result.Tools)
	return result
}

func probeErrorCode(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil && value != "" {
		return value
	}
	if len(raw) != 0 {
		return "protocol_error"
	}
	return "unknown_error"
}

func (h *Handler) recordInvocation(invocation mcpadmin.Invocation) {
	h.recentMu.Lock()
	defer h.recentMu.Unlock()
	if len(h.recentInvocations) == maxRecentInvocationCount {
		copy(h.recentInvocations, h.recentInvocations[1:])
		h.recentInvocations[len(h.recentInvocations)-1] = invocation
		return
	}
	h.recentInvocations = append(h.recentInvocations, invocation)
}

func (h *Handler) recentInvocationSnapshot(limit int) []mcpadmin.Invocation {
	if limit <= 0 || limit > maxRecentInvocationCount {
		limit = maxRecentInvocationCount
	}
	h.recentMu.Lock()
	defer h.recentMu.Unlock()
	if limit > len(h.recentInvocations) {
		limit = len(h.recentInvocations)
	}
	result := make([]mcpadmin.Invocation, limit)
	for index := 0; index < limit; index++ {
		result[index] = h.recentInvocations[len(h.recentInvocations)-1-index]
		result[index].Peer = strings.TrimSpace(result[index].Peer)
	}
	return result
}
