//go:build linux || darwin

package terminal

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func runNativeSecretProbe(secret string) ([]byte, string, error) {
	const noEchoProbeTimeout = 250 * time.Millisecond
	method := nativeSecretProbeMethod()
	command := exec.Command(os.Args[0], "-test.run=^TestPlatformPairSecretInput$")
	command.Env = append(os.Environ(), nativeSecretHelperEnvironment+"=1")
	terminalFile, err := pty.Start(command)
	if err != nil {
		return nil, method, fmt.Errorf("start PTY helper: %w", err)
	}
	defer terminalFile.Close()
	finished := false
	defer func() {
		if !finished && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()

	if err := terminalFile.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, method, fmt.Errorf("set PTY read deadline: %w", err)
	}
	reader := bufio.NewReader(terminalFile)
	var output bytes.Buffer
	for !bytes.Contains(output.Bytes(), []byte(nativeSecretPrompt)) {
		value, readErr := reader.ReadByte()
		if readErr != nil {
			return nil, method, fmt.Errorf("wait for native secret prompt: %w", readErr)
		}
		_ = output.WriteByte(value)
	}
	if err := waitForPlatformSecretReady(terminalFile, 2*time.Second); err != nil {
		return nil, method, err
	}
	probe, remainder := splitNativeSecretFixture(secret)
	if _, err := io.WriteString(terminalFile, probe); err != nil {
		return nil, method, fmt.Errorf("write native no-echo probe: %w", err)
	}
	if err := requireQuietPTYInput(reader, terminalFile, noEchoProbeTimeout); err != nil {
		return nil, method, err
	}
	if _, err := io.WriteString(terminalFile, remainder+"\n"); err != nil {
		return nil, method, fmt.Errorf("write native secret remainder: %w", err)
	}
	if err := terminalFile.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, method, fmt.Errorf("reset PTY read deadline: %w", err)
	}
	outputRemainder, readErr := io.ReadAll(reader)
	output.Write(outputRemainder)
	waitErr := command.Wait()
	finished = true
	if waitErr != nil {
		return nil, method, fmt.Errorf("native secret helper exit: %w", waitErr)
	}
	if readErr != nil && !errors.Is(readErr, unix.EIO) {
		return nil, method, fmt.Errorf("read native secret helper output: %w", readErr)
	}
	return output.Bytes(), method, nil
}

func nativeSecretProbeMethod() string {
	if runtime.GOOS == "linux" {
		return "linux-pty+xterm-readpassword+termios-echo-check+input-echo-probe+final-fragment-rejection"
	}
	return "darwin-pty+xterm-readpassword+input-echo-probe+final-fragment-rejection"
}

func waitForPlatformSecretReady(file *os.File, timeout time.Duration) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		disabled, err := nativeEchoDisabled(file)
		if err != nil {
			return fmt.Errorf("inspect native PTY echo mode: %w", err)
		}
		if disabled {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("native PTY did not disable input echo")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func requireQuietPTYInput(reader *bufio.Reader, file *os.File, timeout time.Duration) error {
	if reader.Buffered() != 0 {
		return errors.New("native PTY produced output before no-echo state")
	}
	deadline := time.Now().Add(timeout)
	pollDescriptors := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLIN}}
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		ready, err := unix.Poll(pollDescriptors, int(remaining.Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("poll native no-echo probe: %w", err)
		}
		if ready == 0 {
			return nil
		}
		return errors.New("native PTY produced output or closed before no-echo state")
	}
}
