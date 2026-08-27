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
	if report.Status != StatusDegraded || report.Components["postgresql"].Status != StatusHealthy || report.Components["open_evaluation_worker"].Reason != "not_configured" || report.Components["offline_signer"].Reason != "not_configured" || report.Components["offline_protocol"].Reason != "unavailable" || report.Components["nocturne"].Reason != "not_configured" || report.Components["notesync"].Reason != "not_configured" || len(report.Warnings) != 1 {
		t.Fatalf("unexpected optional model report: %+v", report)
	}

	checker = New(Options{Database: fakePinger{err: errors.New("secret connection detail")}, ModelEnabled: false, ModelRequired: true})
	report = checker.Ready(context.Background())
	if report.Status != StatusNotReady || report.Components["postgresql"].Reason != "unavailable" || report.Components["model"].Reason != "not_configured" {
		t.Fatalf("unexpected required report: %+v", report)
	}
}

func TestOfflineReadinessComponentsDistinguishHealthyDegradedAndFatal(t *testing.T) {
	healthyOptions := Options{
		Database: fakePinger{}, ModelEnabled: true,
		ModelProbe:                func(context.Context) (bool, string) { return true, "" },
		OpenEvaluationWorkerProbe: func(context.Context) error { return nil },
		OfflineSignerAvailable:    true, OfflineProtocolAvailable: true,
		NocturneEnabled: true, NocturneProbe: func(context.Context) error { return nil },
		NotesyncEnabled: true, NotesyncProbe: func(context.Context) (bool, string) { return true, "" },
	}
	report := New(healthyOptions).Ready(context.Background())
	if report.Status != StatusHealthy || report.Components["open_evaluation_worker"] != (Component{Status: StatusHealthy}) || report.Components["offline_protocol"] != (Component{Status: StatusHealthy}) {
		t.Fatalf("unexpected healthy offline readiness: %+v", report)
	}

	degradedOptions := healthyOptions
	degradedOptions.ModelEnabled = false
	degradedOptions.ModelProbe = nil
	report = New(degradedOptions).Ready(context.Background())
	if report.Status != StatusDegraded || report.Components["open_evaluation_worker"] != (Component{Status: StatusDegraded, Reason: "model_unavailable"}) || report.Components["offline_protocol"] != (Component{Status: StatusHealthy}) {
		t.Fatalf("unexpected no-model degradation: %+v", report)
	}

	degradedOptions = healthyOptions
	degradedOptions.OpenEvaluationWorkerProbe = func(context.Context) error { return errors.New("private worker detail") }
	report = New(degradedOptions).Ready(context.Background())
	if report.Status != StatusDegraded || report.Components["open_evaluation_worker"] != (Component{Status: StatusDegraded, Reason: "unavailable"}) {
		t.Fatalf("unexpected worker-loop degradation: %+v", report)
	}

	fatalOptions := healthyOptions
	fatalOptions.Database = fakePinger{err: errors.New("private database detail")}
	report = New(fatalOptions).Ready(context.Background())
	if report.Status != StatusNotReady || report.Components["postgresql"] != (Component{Status: StatusNotReady, Reason: "unavailable"}) || report.Components["open_evaluation_worker"].Status != StatusHealthy || report.Components["offline_protocol"].Status != StatusHealthy {
		t.Fatalf("unexpected fatal readiness: %+v", report)
	}
}

func TestOfflineSignerReadinessIsReportedWithoutMakingOnlineTeachingNotReady(t *testing.T) {
	checker := New(Options{Database: fakePinger{}, OfflineSignerAvailable: true})
	report := checker.Ready(context.Background())
	if report.Components["offline_signer"] != (Component{Status: StatusHealthy}) || report.Status != StatusDegraded {
		t.Fatalf("unexpected configured signer report: %+v", report)
	}
	checker = New(Options{Database: fakePinger{}})
	report = checker.Ready(context.Background())
	if report.Components["offline_signer"] != (Component{Status: StatusDegraded, Reason: "not_configured"}) || report.Status != StatusDegraded {
		t.Fatalf("unexpected absent signer report: %+v", report)
	}
}

func TestNotesyncReadinessIsIndependentAndReasoned(t *testing.T) {
	base := Options{Database: fakePinger{}, ModelEnabled: true, ModelProbe: func(context.Context) (bool, string) { return true, "" }, NotesyncEnabled: true}
	base.NotesyncProbe = func(context.Context) (bool, string) { return true, "" }
	report := New(base).Ready(context.Background())
	if report.Components["notesync"] != (Component{Status: StatusHealthy}) {
		t.Fatalf("compatible NoteSync was not healthy: %+v", report)
	}

	for _, reason := range []string{"version_unsupported", "version_untested", "version_unavailable", "capability_unavailable"} {
		options := base
		options.NotesyncProbe = func(context.Context) (bool, string) { return false, reason }
		report = New(options).Ready(context.Background())
		if report.Status != StatusDegraded || report.Components["postgresql"].Status != StatusHealthy || report.Components["notesync"] != (Component{Status: StatusDegraded, Reason: reason}) {
			t.Fatalf("NoteSync failure escaped optional component for %s: %+v", reason, report)
		}
	}

	base.NotesyncProbe = nil
	report = New(base).Ready(context.Background())
	if report.Components["notesync"] != (Component{Status: StatusDegraded, Reason: "probe_unavailable"}) {
		t.Fatalf("missing NoteSync probe was not explicit: %+v", report)
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
