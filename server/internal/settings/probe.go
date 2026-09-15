package settings

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
)

type countingTransport struct {
	base  http.RoundTripper
	count atomic.Int32
}

func (t *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.count.Add(1)
	return t.base.RoundTrip(r)
}

func (s *Service) probeConnection(ctx context.Context, target Target, slot secretSlot, limits Limits) Probe {
	result := Probe{Status: "failed", CostEstimate: "unknown"}
	if target == Search {
		transport := &countingTransport{base: searchTransport()}
		var adapter websearch.Adapter = websearch.NewBrave(slot.Endpoint, slot.Key, &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: rejectRedirect})
		_, err := adapter.Search(ctx, websearch.Request{Query: "education", Limit: 1})
		result.Requests = int(transport.count.Load())
		if err != nil {
			result.Reason = websearch.Category(err)
			return result
		}
		result.Status = "ready"
		return result
	}
	transport := &countingTransport{base: modelTransport()}
	client, err := modelClient(slot, limits, true, transport)
	if err != nil {
		result.Reason = "invalid_request"
		return result
	}
	capability := client.Probe(ctx)
	result.Requests = int(transport.count.Load())
	result.StructuredJSON = capability.StructuredJSON
	result.NativeSchema = capability.NativeJSONSchema
	if capability.Compatible {
		result.Status = "ready"
	} else if len(capability.IncompatibilityReasons) > 0 {
		result.Reason = capability.IncompatibilityReasons[0]
	} else {
		result.Reason = string(llm.ErrorIncompatible)
	}
	return result
}
