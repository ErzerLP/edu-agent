package health

import (
	"context"
	"strings"
	"time"
)

type Status string

const (
	StatusHealthy  Status = "healthy"
	StatusDegraded Status = "degraded"
	StatusNotReady Status = "not_ready"
)

type Component struct {
	Status Status `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type Report struct {
	Status     Status               `json:"status"`
	Components map[string]Component `json:"components"`
	Warnings   []string             `json:"warnings,omitempty"`
}

type Pinger interface {
	Ping(context.Context) error
}

type ModelProbe func(context.Context) (compatible bool, reason string)
type NotesyncProbe func(context.Context) (compatible bool, reason string)
type OptionalProbe func(context.Context) error

type Checker struct {
	database                  Pinger
	modelEnabled              bool
	modelRequired             bool
	modelProbe                ModelProbe
	openEvaluationWorkerProbe OptionalProbe
	offlineSignerAvailable    bool
	offlineProtocolAvailable  bool
	nocturneEnabled           bool
	nocturneProbe             OptionalProbe
	notesyncEnabled           bool
	notesyncProbe             NotesyncProbe
	insecureWarning           bool
	timeout                   time.Duration
}

type Options struct {
	Database                  Pinger
	ModelEnabled              bool
	ModelRequired             bool
	ModelProbe                ModelProbe
	OpenEvaluationWorkerProbe OptionalProbe
	OfflineSignerAvailable    bool
	OfflineProtocolAvailable  bool
	NocturneEnabled           bool
	NocturneProbe             OptionalProbe
	NotesyncEnabled           bool
	NotesyncProbe             NotesyncProbe
	InsecureWarning           bool
	Timeout                   time.Duration
}

func New(options Options) *Checker {
	if options.Timeout <= 0 {
		options.Timeout = 2 * time.Second
	}
	return &Checker{
		database: options.Database, modelEnabled: options.ModelEnabled, modelRequired: options.ModelRequired,
		modelProbe: options.ModelProbe, openEvaluationWorkerProbe: options.OpenEvaluationWorkerProbe,
		offlineSignerAvailable: options.OfflineSignerAvailable, offlineProtocolAvailable: options.OfflineProtocolAvailable,
		nocturneEnabled: options.NocturneEnabled, nocturneProbe: options.NocturneProbe,
		notesyncEnabled: options.NotesyncEnabled, notesyncProbe: options.NotesyncProbe,
		insecureWarning: options.InsecureWarning, timeout: options.Timeout,
	}
}

func (c *Checker) Ready(ctx context.Context) Report {
	report := Report{Status: StatusHealthy, Components: map[string]Component{}}
	if c.insecureWarning {
		report.Warnings = []string{"insecure_non_loopback_http"}
	}
	if c.database == nil {
		setComponent(&report, "postgresql", StatusNotReady, "not_configured")
	} else {
		checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
		err := c.database.Ping(checkCtx)
		cancel()
		if err != nil {
			setComponent(&report, "postgresql", StatusNotReady, "unavailable")
		} else {
			setComponent(&report, "postgresql", StatusHealthy, "")
		}
	}

	modelStatus := StatusDegraded
	if c.modelRequired {
		modelStatus = StatusNotReady
	}
	modelCompatible := false
	modelReason := "not_configured"
	switch {
	case !c.modelEnabled:
	case c.modelProbe == nil:
		modelReason = "probe_unavailable"
	default:
		checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
		modelCompatible, modelReason = c.modelProbe(checkCtx)
		cancel()
		if !modelCompatible && strings.TrimSpace(modelReason) == "" {
			modelReason = "incompatible"
		}
	}
	if modelCompatible {
		setComponent(&report, "model", StatusHealthy, "")
	} else {
		setComponent(&report, "model", modelStatus, modelReason)
	}

	switch {
	case c.openEvaluationWorkerProbe == nil:
		setComponent(&report, "open_evaluation_worker", StatusDegraded, "not_configured")
	default:
		checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
		err := c.openEvaluationWorkerProbe(checkCtx)
		cancel()
		if err != nil {
			setComponent(&report, "open_evaluation_worker", StatusDegraded, "unavailable")
		} else if !modelCompatible {
			setComponent(&report, "open_evaluation_worker", StatusDegraded, "model_unavailable")
		} else {
			setComponent(&report, "open_evaluation_worker", StatusHealthy, "")
		}
	}

	if c.offlineSignerAvailable {
		setComponent(&report, "offline_signer", StatusHealthy, "")
	} else {
		setComponent(&report, "offline_signer", StatusDegraded, "not_configured")
	}
	if c.offlineProtocolAvailable {
		setComponent(&report, "offline_protocol", StatusHealthy, "")
	} else {
		setComponent(&report, "offline_protocol", StatusDegraded, "unavailable")
	}

	switch {
	case !c.nocturneEnabled:
		setComponent(&report, "nocturne", StatusDegraded, "not_configured")
	case c.nocturneProbe == nil:
		setComponent(&report, "nocturne", StatusDegraded, "probe_unavailable")
	default:
		checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
		err := c.nocturneProbe(checkCtx)
		cancel()
		if err != nil {
			setComponent(&report, "nocturne", StatusDegraded, "unavailable")
		} else {
			setComponent(&report, "nocturne", StatusHealthy, "")
		}
	}
	switch {
	case !c.notesyncEnabled:
		setComponent(&report, "notesync", StatusDegraded, "not_configured")
	case c.notesyncProbe == nil:
		setComponent(&report, "notesync", StatusDegraded, "probe_unavailable")
	default:
		checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
		compatible, reason := c.notesyncProbe(checkCtx)
		cancel()
		if compatible {
			setComponent(&report, "notesync", StatusHealthy, "")
		} else {
			if strings.TrimSpace(reason) == "" {
				reason = "capability_unavailable"
			}
			setComponent(&report, "notesync", StatusDegraded, reason)
		}
	}
	return report
}

func setComponent(report *Report, name string, status Status, reason string) {
	report.Components[name] = Component{Status: status, Reason: reason}
	report.Status = worse(report.Status, status)
}

func worse(left, right Status) Status {
	weight := map[Status]int{StatusHealthy: 0, StatusDegraded: 1, StatusNotReady: 2}
	if weight[right] > weight[left] {
		return right
	}
	return left
}
