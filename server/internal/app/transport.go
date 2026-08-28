package app

import (
	"fmt"
	"net/http"

	"github.com/edu-agent/edu-agent/server/internal/transport/httpapi"
	mcptransport "github.com/edu-agent/edu-agent/server/internal/transport/mcp"
)

func composeTransportHandler(options httpapi.Options) (http.Handler, error) {
	mcpHandler, err := mcptransport.New(mcptransport.Options{
		Identity: options.Identity, Knowledge: options.Knowledge, Learning: options.Learning,
		Memory: options.Memory, MemoryExporter: options.MemoryExporter,
		ReadPermits: options.ReadPermits, Logger: options.Logger,
		AuthLimiter: options.AuthLimiter, DeviceLimiter: options.DeviceLimiter,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize MCP transport: %w", err)
	}
	options.MCP = mcpHandler
	handler, err := httpapi.New(options)
	if err != nil {
		return nil, fmt.Errorf("initialize HTTP API: %w", err)
	}
	return handler, nil
}
