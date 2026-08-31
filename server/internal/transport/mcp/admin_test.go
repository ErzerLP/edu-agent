package mcp

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/transport/mcpadmin"
)

func TestManagementSnapshotAndProbeUseLiveCatalogWithoutCredentials(t *testing.T) {
	handler, _, _, _, _, _ := newProtocolFixture(t, []string{"knowledge:read", "knowledge:write", "learning:read", "learning:write", "memory:read"})

	snapshot := handler.Snapshot(50)
	if snapshot.ImplementationName != implementationName || snapshot.ImplementationVersion != implementationVersion {
		t.Fatalf("implementation = %q/%q", snapshot.ImplementationName, snapshot.ImplementationVersion)
	}
	if snapshot.Transport != "streamable_http" || !snapshot.Stateless || !snapshot.JSONResponse || snapshot.MaxRequestBodyBytes != DefaultMaxRequestBodyBytes {
		t.Fatalf("runtime snapshot = %+v", snapshot)
	}
	if snapshot.StaticResourceCount != 4 || snapshot.ResourceTemplateCount != 5 || snapshot.ResourceCount != 9 || snapshot.ToolCount != 15 || len(snapshot.Descriptors) != 24 {
		t.Fatalf("catalog counts = static:%d templates:%d resources:%d tools:%d descriptors:%d", snapshot.StaticResourceCount, snapshot.ResourceTemplateCount, snapshot.ResourceCount, snapshot.ToolCount, len(snapshot.Descriptors))
	}
	for _, descriptor := range snapshot.Descriptors {
		if descriptor.Name == "" || descriptor.Kind == "" || descriptor.RequiredScope == "" || len(descriptor.PrivacyOwners) == 0 || descriptor.OutputLimit <= 0 || descriptor.AuditName == "" {
			t.Fatalf("incomplete management descriptor: %+v", descriptor)
		}
	}

	probe := handler.Probe(context.Background(), testToken, "localhost")
	if !probe.OK || probe.HTTPStatus != http.StatusOK || probe.ToolCount != 15 || probe.RequestID == "" || probe.ErrorCode != "" {
		t.Fatalf("successful probe = %+v", probe)
	}
	failed := handler.Probe(context.Background(), "invalid-device-token", "localhost")
	if failed.OK || failed.HTTPStatus != http.StatusUnauthorized || failed.ErrorCode != "authentication_failed" || failed.RequestID == "" {
		t.Fatalf("failed probe = %+v", failed)
	}

	recent := handler.Snapshot(2).RecentInvocations
	if len(recent) != 2 || recent[0].RequestID != failed.RequestID || recent[1].RequestID != probe.RequestID {
		t.Fatalf("recent invocations = %+v", recent)
	}
	if recent[0].Descriptor != "tools/list" || recent[0].DeviceID != "" || recent[1].DeviceID != testDeviceID {
		t.Fatalf("recent invocation metadata = %+v", recent)
	}
}

func TestRecentInvocationSnapshotIsConcurrencySafeBoundedAndNewestFirst(t *testing.T) {
	handler, _, _, _, _, _ := newProtocolFixture(t, []string{"knowledge:read"})
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		worker := worker
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := 0; index < 25; index++ {
				handler.recordInvocation(mcpadmin.Invocation{
					CompletedAt: time.Unix(int64(worker*25+index), 0).UTC(),
					RequestID:   fmt.Sprintf("%d-%d", worker, index), Descriptor: "tools/list",
					Result: "success", Peer: " 127.0.0.1 ",
				})
			}
		}()
	}
	workers.Wait()

	recent := handler.Snapshot(maxRecentInvocationCount + 1).RecentInvocations
	if len(recent) != maxRecentInvocationCount {
		t.Fatalf("recent count = %d, want %d", len(recent), maxRecentInvocationCount)
	}
	for _, invocation := range recent {
		if invocation.RequestID == "" || invocation.Peer != "127.0.0.1" {
			t.Fatalf("invalid recent invocation: %+v", invocation)
		}
	}
}
