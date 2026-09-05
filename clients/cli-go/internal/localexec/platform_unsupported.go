//go:build !linux && !darwin

package localexec

import "os/exec"

func platformSupported() bool               { return false }
func configureProcessGroup(*exec.Cmd)       {}
func observeExit(int) error                 { return failure("unsupported_platform") }
func groupHasLiveMembers(int) (bool, error) { return false, failure("unsupported_platform") }
func signalProcessGroup(int, bool) error    { return failure("unsupported_platform") }
func processGroupExists(int) (bool, error)  { return false, failure("unsupported_platform") }
