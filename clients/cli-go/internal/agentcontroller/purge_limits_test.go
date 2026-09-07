package agentcontroller

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArchivePurgeCurrentPlanBudgetsAcrossResumeAndSwitch(t *testing.T) {
	for _, kind := range []string{"entries", "plan_bytes"} {
		t.Run(kind, func(t *testing.T) {
			f, old, model := purgeControllerFixture(t, false)
			owner, provider := old.SessionID(), old.provider
			if err := old.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			deps := controllerDependencies(f.openStore(t), model, f.server, f.workspace, provider)
			deps.LoopOptions.ContextWindow = 32768
			if kind == "entries" {
				deps.LoopOptions.WorkspaceCopyEntries = 1
			} else {
				deps.LoopOptions.WorkspaceCopyPlanBytes = 1
			}
			c, err := Resume(t.Context(), deps, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			check := func(callID string) {
				t.Helper()
				model.mu.Lock()
				model.callID, model.result = callID, nil
				model.mu.Unlock()
				result, err := c.Send(t.Context(), "按当前预算清理")
				if err != nil || result.PendingFileMutation != nil || model.result["code"] != "file_too_large" {
					t.Fatal("old budget reused or scope narrowed", result, model.result, err)
				}
				if _, err := os.Stat(filepath.Join(f.workspace, model.path)); err != nil {
					t.Fatal("budget failure deleted scope", err)
				}
			}
			check("purge-current-resume")
			if _, err := c.NewSession(t.Context()); err != nil {
				t.Fatal(err)
			}
			check("purge-current-new")
			if _, err := c.CommitSwitch(t.Context(), c.PlanSwitch(leaseSummary(t, c, owner)), SwitchConfirmation{}); err != nil {
				t.Fatal(err)
			}
			check("purge-current-switch")
		})
	}
}
