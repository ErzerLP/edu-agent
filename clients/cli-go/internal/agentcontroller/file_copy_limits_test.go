package agentcontroller

import (
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func TestRecursiveCopyCurrentBudgetsAcrossResumeAndSwitch(t *testing.T) {
	for _, kind := range []string{"bytes", "journal_memory"} {
		t.Run(kind, func(t *testing.T) {
			f, old, model := recursiveCopyFixture(t, false)
			owner, provider := old.SessionID(), old.provider
			if err := old.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			deps := controllerDependencies(f.openStore(t), model, f.server, f.workspace, provider)
			deps.LoopOptions.ContextWindow = 32768
			want := "file_too_large"
			if kind == "bytes" {
				deps.LoopOptions.WorkspaceCopyBytes = 8
			} else {
				deps.LoopOptions.FileBatchMemoryBytes = 1
				want = "file_batch_limit"
			}
			c, err := Resume(t.Context(), deps, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			check := func() {
				t.Helper()
				model.result = nil
				result, err := c.Send(t.Context(), "copy with current budget")
				if err == nil && result.PendingFileMutation != nil {
					_, err = c.ResolveFileMutation(t.Context(), result.PendingFileMutation.CallID, agentloop.FileMutationApprove)
				}
				if err != nil {
					t.Fatal(err)
				}
				if model.result["code"] != want {
					t.Fatalf("current %s budget lost: %+v", kind, model.result)
				}
			}
			check()
			if _, err := c.NewSession(t.Context()); err != nil {
				t.Fatal(err)
			}
			check()
			if _, err := c.CommitSwitch(t.Context(), c.PlanSwitch(leaseSummary(t, c, owner)), SwitchConfirmation{}); err != nil {
				t.Fatal(err)
			}
			// Call IDs are opaque operation identities, so each new user
			// request uses a new one even when returning to the original owner.
			model.callID = "copy-current-budget-after-switch"
			check()
		})
	}
}
