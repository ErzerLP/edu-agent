//go:build !linux && !darwin

package localexec

import (
	"os"
	"os/exec"
)

func openPTY(*exec.Cmd, int, int) (*processPipes, error) { return nil, failure("unsupported_platform") }
func resizeTerminal(*os.File, int, int) error            { return failure("unsupported_platform") }
func terminalControlByte(*os.File, string) (byte, error) { return 0, failure("unsupported_platform") }
func terminalEOF(error) bool                             { return false }
func sessionHasMembers(int, bool) (bool, error)          { return false, failure("unsupported_platform") }

func platformSupported() bool               { return false }
func configureProcessGroup(*exec.Cmd)       {}
func observeExit(int) error                 { return failure("unsupported_platform") }
func groupHasLiveMembers(int) (bool, error) { return false, failure("unsupported_platform") }
func signalProcessGroup(int, bool) error    { return failure("unsupported_platform") }
func processGroupExists(int) (bool, error)  { return false, failure("unsupported_platform") }
