package localexec

import (
	"os"
	"os/exec"
	"time"
)

type exitResult struct {
	state *os.ProcessState
	err   error
}

// The leader is observed WITHOUT reaping (WNOWAIT). Until all group signals are
// finished its PID therefore remains reserved, even if the shell has exited.
// This avoids the killpg-after-Wait PID-reuse race. No signal is sent after reap.
func (m *Manager) supervise(t *task, cmd *exec.Cmd, pipes *processPipes, timeout time.Duration) {
	stdoutDone, stderrDone := make(chan struct{}), make(chan struct{})
	go m.capture(t, &t.stdout, "stdout", pipes.stdoutReader, stdoutDone)
	if t.snapshot.PTY {
		close(stderrDone) // One merged terminal stream, never a fake stderr pipe.
	} else {
		go m.capture(t, &t.stderr, "stderr", pipes.stderrReader, stderrDone)
	}
	pid := cmd.Process.Pid
	observations := make(chan error, 1)
	reap := make(chan struct{})
	reaped := make(chan exitResult, 1)
	go func() {
		observations <- observeExit(pid)
		<-reap
		err := cmd.Wait() // Only files were passed to exec; no inherited-pipe waits.
		reaped <- exitResult{state: cmd.ProcessState, err: err}
		m.mu.Lock()
		t.reaped = true
		if t.terminal {
			m.releaseActiveLocked(t)
		}
		m.mu.Unlock()
	}()

	observed := false
	var observationError error
	markObserved := func(err error) {
		observed, observationError = true, err
		m.mu.Lock()
		if t.stopReason == "" {
			t.snapshot.State = StateFinishing
		}
		m.mu.Unlock()
	}
	checkObserved := func() {
		if !observed {
			select {
			case err := <-observations:
				markObserved(err)
			default:
			}
		}
	}
	var timeoutC <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}
	executionReason := ""
	select {
	case err := <-observations:
		markObserved(err)
	case <-t.stop:
		checkObserved()
		if !observed {
			m.mu.Lock()
			executionReason = t.stopReason
			m.mu.Unlock()
		}
	case <-timeoutC:
		checkObserved()
		if !observed {
			m.mu.Lock()
			m.requestStopLocked(t, "execution_timeout")
			executionReason = t.stopReason
			m.mu.Unlock()
		}
	}
	closeTaskInput(t)

	// An anchored, exited leader plus no LIVE group members needs no signal.
	// Zombies are checked separately after reap, and are never called clean.
	groupSettled := func() bool {
		checkObserved()
		if !observed || observationError != nil {
			return false
		}
		if t.snapshot.PTY {
			live, err := sessionHasMembers(pid, true)
			return err == nil && !live
		}
		live, err := groupHasLiveMembers(pid)
		return err == nil && !live
	}
	awaitSettlement := func(budget time.Duration) bool {
		deadline := time.NewTimer(budget)
		defer deadline.Stop()
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			if groupSettled() {
				return true
			}
			if observationError != nil {
				return false
			}
			select {
			case <-deadline.C:
				return false
			case <-ticker.C:
			}
		}
	}

	settled := groupSettled()
	signalFailed := false
	if !settled && observationError == nil {
		if err := signalProcessGroup(pid, false); err != nil {
			signalFailed = true
		}
		settled = awaitSettlement(m.options.StopGrace)
		if !settled && observationError == nil {
			if t.snapshot.PTY {
				// Last-master close invokes the kernel's terminal hangup, which
				// targets its actual foreground group without a numeric PID race.
				// Only escalate after the normal output/TERM grace; forced close
				// cannot establish EOF and capture reports incomplete honestly.
				closeFile(pipes.stdoutReader)
			}
			if err := signalProcessGroup(pid, true); err != nil {
				signalFailed = true
			}
			settled = awaitSettlement(killSettleLimit)
		}
	}
	// Query the whole terminal session (including zombies) BEFORE releasing
	// the leader. Do not chase old numeric identities after reap. A detached
	// session is outside this contract; a remaining job-control pgrp is not.
	sessionIncomplete := false
	if t.snapshot.PTY {
		exists, err := sessionHasMembers(pid, false)
		sessionIncomplete = exists || err != nil
	}
	// Ownership of the leader ends here. Even if it is stuck in kernel I/O,
	// retirement never signals a later incarnation of its numeric PID/PGID.
	close(reap)
	var result exitResult
	haveResult := false
	if observed {
		timer := time.NewTimer(killSettleLimit)
		select {
		case result = <-reaped:
			haveResult = true
		case <-timer.C:
		}
		timer.Stop()
	} else {
		select {
		case result = <-reaped:
			haveResult = true
		default:
		}
	}
	cleanupIncomplete := !settled || signalFailed || !haveResult || observationError != nil || sessionIncomplete
	if haveResult && !t.snapshot.PTY {
		exists, err := processGroupExists(pid)
		cleanupIncomplete = cleanupIncomplete || err != nil || exists
	}

	// Bound pipe waiting independently from journal I/O. Each capture spends
	// at most outputDrainLimit actually reading after this point; synchronous
	// storage has its own bounded settlement window and per-call deadlines.
	m.mu.Lock()
	t.drainStarted = time.Now()
	deadline := t.drainStarted.Add(outputDrainLimit)
	_ = pipes.stdoutReader.SetReadDeadline(deadline)
	if pipes.stderrReader != nil {
		_ = pipes.stderrReader.SetReadDeadline(deadline)
	}
	m.mu.Unlock()
	<-stdoutDone
	<-stderrDone

	m.mu.Lock()
	t.settling = true
	final := t.snapshot
	final.CleanupIncomplete = cleanupIncomplete
	final.FinishedAt = time.Now()
	final.State, final.Reason = StateUnknown, "exit_unknown"
	if haveResult && result.state != nil && observationError == nil {
		final.State, final.Reason = StateExited, "completed"
		exitCode := result.state.ExitCode()
		if exitCode >= 0 {
			final.ExitCode = &exitCode
			if exitCode != 0 {
				final.Reason = "nonzero_exit"
			}
		} else {
			final.Reason = "signaled"
		}
		if executionReason == "execution_timeout" {
			final.State, final.Reason = StateTimedOut, executionReason
		} else if executionReason != "" {
			final.State, final.Reason = StateCanceled, executionReason
		}
	}
	m.mu.Unlock()
	m.finalizeArchive(t, final)
	m.mu.Lock()
	defer m.mu.Unlock()
	applyFinalLocked(t, final)
	t.terminal = true
	// An unkillable kernel task keeps its concurrency reservation until the
	// one existing reaper eventually returns. Repeated calls create no goroutines.
	if haveResult || t.reaped {
		m.releaseActiveLocked(t)
	}
	close(t.done)
}
