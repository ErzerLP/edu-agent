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
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func runNativeSecretProbe(secret string) ([]byte, string, error) {
	const method = "unix-pty+xterm-readpassword+termios-echo-check"
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
	if err := waitForNativeEchoDisabled(terminalFile, 2*time.Second); err != nil {
		return nil, method, err
	}
	if _, err := io.WriteString(terminalFile, secret+"\n"); err != nil {
		return nil, method, fmt.Errorf("write native secret: %w", err)
	}
	if err := terminalFile.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, method, fmt.Errorf("reset PTY read deadline: %w", err)
	}
	remainder, readErr := io.ReadAll(reader)
	output.Write(remainder)
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

func waitForNativeEchoDisabled(file *os.File, timeout time.Duration) error {
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
