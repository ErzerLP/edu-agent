package health

import (
	"context"
	"errors"
	"testing"
)

type fakePinger struct{ err error }

func (p fakePinger) Ping(context.Context) error { return p.err }

func TestReadinessDistinguishesRequiredAndOptionalDependencies(t *testing.T) {
	checker := New(Options{Database: fakePinger{}, ModelEnabled: true, ModelProbe: func(context.Context) (bool, string) {
		return false, "rate_limited"
	}, InsecureWarning: true})
	report := checker.Ready(context.Background())
	if report.Status != StatusDegraded || report.Components["postgresql"].Status != StatusHealthy || report.Components["nocturne"].Reason != "not_configured" || len(report.Warnings) != 1 {
		t.Fatalf("unexpected optional model report: %+v", report)
	}

	checker = New(Options{Database: fakePinger{err: errors.New("secret connection detail")}, ModelEnabled: false, ModelRequired: true})
	report = checker.Ready(context.Background())
	if report.Status != StatusNotReady || report.Components["postgresql"].Reason != "unavailable" || report.Components["model"].Reason != "not_configured" {
		t.Fatalf("unexpected required report: %+v", report)
	}
}

func TestNocturneReadinessIsAlwaysOptionalAndRedactsProbeErrors(t *testing.T) {
	checker := New(Options{
		Database: fakePinger{}, ModelEnabled: true, ModelProbe: func(context.Context) (bool, string) { return true, "" },
		NocturneEnabled: true, NocturneProbe: func(context.Context) error { return errors.New("token=secret raw body") },
	})
	report := checker.Ready(context.Background())
	if report.Status != StatusDegraded || report.Components["nocturne"] != (Component{Status: StatusDegraded, Reason: "unavailable"}) {
		t.Fatalf("unexpected Nocturne degradation: %+v", report)
	}
	if report.Components["nocturne"].Reason == "token=secret raw body" {
		t.Fatalf("readiness leaked probe details: %+v", report)
	}

	checker = New(Options{Database: fakePinger{}, NocturneEnabled: true})
	report = checker.Ready(context.Background())
	if report.Status != StatusDegraded || report.Components["nocturne"].Reason != "probe_unavailable" {
		t.Fatalf("unconfigured enabled Nocturne became not-ready: %+v", report)
	}
}
