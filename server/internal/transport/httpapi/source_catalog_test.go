package httpapi

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type catalogHTTPFixture struct{ fakeKnowledge }

func (*catalogHTTPFixture) Sources(context.Context, knowledge.SourceQuery) (knowledge.SourcePage, error) {
	return knowledge.SourcePage{Items: []knowledge.SourceItem{{Path: "private-source"}}}, nil
}
func (*catalogHTTPFixture) Source(context.Context, string, string) (knowledge.SourceDetail, error) {
	return knowledge.SourceDetail{SourceItem: knowledge.SourceItem{Path: "private-source"}}, nil
}

func TestSourceCatalogReadScopeAndOwnerGate(t *testing.T) {
	for _, path := range []string{"/v1/knowledge/sources", "/v1/knowledge/sources/10000000-0000-4000-8000-000000000001/documents/10000000-0000-4000-8000-000000000002"} {
		for _, state := range []string{"allowed", "missing_scope", "closed_owner"} {
			t.Run(path+"/"+state, func(t *testing.T) {
				scopes := []string{"knowledge:read"}
				if state == "missing_scope" {
					scopes = []string{"learning:read"}
				}
				manager := privacy.NewReadPermitManager()
				if state == "closed_owner" {
					if err := manager.CloseAndDrain(t.Context(), 2, privacy.OwnerKnowledge); err != nil {
						t.Fatal(err)
					}
				}
				h, err := New(Options{Identity: &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: maintenanceDeviceID}, Scopes: scopes}}, Knowledge: &catalogHTTPFixture{}, ReadPermits: manager, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
				if err != nil {
					t.Fatal(err)
				}
				response := maintenanceRequest(h, "GET", path, "")
				if state == "allowed" {
					if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
						t.Fatal(response.Code, response.Body.String())
					}
					return
				}
				want := 403
				if state == "closed_owner" {
					want = 503
				}
				if response.Code != want || strings.Contains(response.Body.String(), "private-source") {
					t.Fatal("source escaped authority gate", response.Code, response.Body.String())
				}
			})
		}
	}
}
