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

type Checker struct {
	database        Pinger
	modelEnabled    bool
	modelRequired   bool
	modelProbe      ModelProbe
	insecureWarning bool
	timeout         time.Duration
}

type Options struct {
	Database        Pinger
	ModelEnabled    bool
	ModelRequired   bool
	ModelProbe      ModelProbe
	InsecureWarning bool
	Timeout         time.Duration
}

func New(options Options) *Checker {
	if options.Timeout <= 0 {
		options.Timeout = 2 * time.Second
	}
	return &Checker{
		database: options.Database, modelEnabled: options.ModelEnabled, modelRequired: options.ModelRequired,
		modelProbe: options.ModelProbe, insecureWarning: options.InsecureWarning, timeout: options.Timeout,
	}
}

func (c *Checker) Ready(ctx context.Context) Report {
	report := Report{Status: StatusHealthy, Components: map[string]Component{}}
	if c.insecureWarning {
		report.Warnings = []string{"insecure_non_loopback_http"}
	}
	if c.database == nil {
		report.Components["postgresql"] = Component{Status: StatusNotReady, Reason: "not_configured"}
		report.Status = StatusNotReady
	} else {
		checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
		err := c.database.Ping(checkCtx)
		cancel()
		if err != nil {
			report.Components["postgresql"] = Component{Status: StatusNotReady, Reason: "unavailable"}
			report.Status = StatusNotReady
		} else {
			report.Components["postgresql"] = Component{Status: StatusHealthy}
		}
	}
	if !c.modelEnabled {
		status := StatusDegraded
		if c.modelRequired {
			status = StatusNotReady
		}
		report.Components["model"] = Component{Status: status, Reason: "not_configured"}
		report.Status = worse(report.Status, status)
		return report
	}
	if c.modelProbe == nil {
		report.Components["model"] = Component{Status: StatusNotReady, Reason: "probe_unavailable"}
		report.Status = StatusNotReady
		return report
	}
	checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
	compatible, reason := c.modelProbe(checkCtx)
	cancel()
	if compatible {
		report.Components["model"] = Component{Status: StatusHealthy}
		return report
	}
	status := StatusDegraded
	if c.modelRequired {
		status = StatusNotReady
	}
	if strings.TrimSpace(reason) == "" {
		reason = "incompatible"
	}
	report.Components["model"] = Component{Status: status, Reason: reason}
	report.Status = worse(report.Status, status)
	return report
}

func worse(left, right Status) Status {
	weight := map[Status]int{StatusHealthy: 0, StatusDegraded: 1, StatusNotReady: 2}
	if weight[right] > weight[left] {
		return right
	}
	return left
}
