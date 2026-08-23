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
type OptionalProbe func(context.Context) error

type Checker struct {
	database        Pinger
	modelEnabled    bool
	modelRequired   bool
	modelProbe      ModelProbe
	nocturneEnabled bool
	nocturneProbe   OptionalProbe
	insecureWarning bool
	timeout         time.Duration
}

type Options struct {
	Database        Pinger
	ModelEnabled    bool
	ModelRequired   bool
	ModelProbe      ModelProbe
	NocturneEnabled bool
	NocturneProbe   OptionalProbe
	InsecureWarning bool
	Timeout         time.Duration
}

func New(options Options) *Checker {
	if options.Timeout <= 0 {
		options.Timeout = 2 * time.Second
	}
	return &Checker{
		database: options.Database, modelEnabled: options.ModelEnabled, modelRequired: options.ModelRequired,
		modelProbe: options.ModelProbe, nocturneEnabled: options.NocturneEnabled,
		nocturneProbe: options.NocturneProbe, insecureWarning: options.InsecureWarning, timeout: options.Timeout,
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
	switch {
	case !c.modelEnabled:
		setComponent(&report, "model", modelStatus, "not_configured")
	case c.modelProbe == nil:
		setComponent(&report, "model", modelStatus, "probe_unavailable")
	default:
		checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
		compatible, reason := c.modelProbe(checkCtx)
		cancel()
		if compatible {
			setComponent(&report, "model", StatusHealthy, "")
		} else {
			if strings.TrimSpace(reason) == "" {
				reason = "incompatible"
			}
			setComponent(&report, "model", modelStatus, reason)
		}
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
