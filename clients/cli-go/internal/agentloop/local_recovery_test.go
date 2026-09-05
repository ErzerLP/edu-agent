package agentloop

import (
	"context"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

type recordedLocalSink struct{ durabilitySink }

func (recordedLocalSink) BeforeLocalExecution(context.Context, LocalExecutionIntent) error {
	return ErrLocalCallRecorded
}

func TestLocalRecoveryIdentityProjectsUnknownWithoutLaunching(t *testing.T) {
	s := newLocalExecutionSession(t, &fakeModel{}, func(options *Options) { options.Durability = &recordedLocalSink{} })
	result := s.executeLocalTool(t.Context(), localCall("old-call", "shell", map[string]any{"command": "printf must-not-run"}))
	value := result.value(4096, 1, false, false)
	if len(s.options.LocalExec.List(s.options.LocalExecOwner)) != 0 || value["state"] != "unknown" || value["operation_outcome"] != "unknown" || value["replay"] != false || value["availability"] != "unavailable" || result.NotSaved {
		t.Fatalf("recovered intent was launched/misclassified: %+v", value)
	}
	value = localToolResult{Code: "local_execution_outcome_unknown", Snapshot: &localexec.Snapshot{TaskID: "active", State: "running", Controllable: true}}.value(0, 0, true, false)
	if value["state"] != "running" || value["controllable"] != true || value["operation_outcome"] != "unknown" {
		t.Fatalf("old input outcome overwrote live process state: %+v", value)
	}
}
