//go:build windows

package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	conpty "github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

type windowsOutputEvent struct {
	data []byte
	err  error
	done bool
}

type windowsConPTYProcess struct {
	console *conpty.ConPty
	events  chan windowsOutputEvent
	closed  bool
}

func runNativeSecretProbe(secret string) ([]byte, string, error) {
	const method = "windows-conpty+xterm-readpassword+input-echo-probe+final-fragment-rejection"
	process, err := startWindowsConPTYTestHelper(nativeSecretHelperEnvironment, "1", "TestPlatformPairSecretInput")
	if err != nil {
		return nil, method, err
	}
	defer process.close()

	var output bytes.Buffer
	if err := process.readUntil(&output, []byte("Pairing code:"), 10*time.Second); err != nil {
		return nil, method, err
	}
	probe, remainder := splitNativeSecretFixture(secret)
	if _, err := io.WriteString(process, probe); err != nil {
		return nil, method, fmt.Errorf("write ConPTY no-echo probe: %w", err)
	}
	checkpoint := output.Len()
	if err := process.requireQuietInput(&output, checkpoint, 250*time.Millisecond); err != nil {
		return nil, method, err
	}
	if _, err := io.WriteString(process, remainder+"\r"); err != nil {
		return nil, method, fmt.Errorf("write ConPTY secret remainder: %w", err)
	}
	if err := process.readUntil(&output, []byte(nativeSecretSuccessMarker), 10*time.Second); err != nil {
		return nil, method, err
	}
	if err := process.waitAndDrain(&output, 10*time.Second); err != nil {
		return nil, method, err
	}
	return output.Bytes(), method, nil
}

func startWindowsConPTYTestHelper(helperEnvironment, helperValue, testName string) (*windowsConPTYProcess, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve native helper executable: %w", err)
	}
	helperCommand := windows.ComposeCommandLine([]string{
		executable,
		"-test.run=" + testName,
		"-test.count=1",
	})
	commandInterpreter := os.Getenv("ComSpec")
	if commandInterpreter == "" {
		commandInterpreter = `C:\Windows\System32\cmd.exe`
	}
	commandLine := windows.ComposeCommandLine([]string{commandInterpreter}) + ` /d /s /c "` + helperCommand + `"`

	previousHelper, helperWasSet := os.LookupEnv(helperEnvironment)
	if err := os.Setenv(helperEnvironment, helperValue); err != nil {
		return nil, fmt.Errorf("set native helper environment: %w", err)
	}
	restoreEnvironment := func() error {
		if helperWasSet {
			return os.Setenv(helperEnvironment, previousHelper)
		}
		return os.Unsetenv(helperEnvironment)
	}
	console, startErr := conpty.Start(commandLine, conpty.ConPtyDimensions(80, 25))
	restoreErr := restoreEnvironment()
	if startErr != nil {
		return nil, fmt.Errorf("start ConPTY helper: %w", startErr)
	}
	if restoreErr != nil {
		_ = console.Close()
		return nil, fmt.Errorf("restore native helper environment: %w", restoreErr)
	}

	process := &windowsConPTYProcess{
		console: console,
		events:  make(chan windowsOutputEvent, 256),
	}
	go streamWindowsNativeOutput(console, process.events)
	return process, nil
}

func (p *windowsConPTYProcess) Write(data []byte) (int, error) {
	if p.closed || p.console == nil {
		return 0, errors.New("ConPTY helper is closed")
	}
	return p.console.Write(data)
}

func (p *windowsConPTYProcess) readUntil(output *bytes.Buffer, marker []byte, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for !bytes.Contains(output.Bytes(), marker) {
		select {
		case event := <-p.events:
			output.Write(event.data)
			if event.done {
				return windowsConPTYOutputEnded("wait for ConPTY helper output", event.err)
			}
		case <-deadline.C:
			visible := bytes.TrimSpace(windowsVisibleConPTYText(output.Bytes()))
			if len(visible) > 240 {
				visible = visible[len(visible)-240:]
			}
			return fmt.Errorf("timed out waiting for ConPTY helper marker %q after output %q", marker, visible)
		}
	}
	return nil
}

func (p *windowsConPTYProcess) requireQuietInput(output *bytes.Buffer, checkpoint int, timeout time.Duration) error {
	quiet := time.NewTimer(timeout)
	defer quiet.Stop()
	for {
		select {
		case event := <-p.events:
			output.Write(event.data)
			if len(bytes.TrimSpace(windowsVisibleConPTYText(output.Bytes()[checkpoint:]))) != 0 {
				return errors.New("ConPTY emitted printable input before no-echo state")
			}
			if event.done {
				return windowsConPTYOutputEnded("wait for ConPTY no-echo state", event.err)
			}
		case <-quiet.C:
			return nil
		}
	}
}

func (p *windowsConPTYProcess) waitAndDrain(output *bytes.Buffer, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	exitCode, waitErr := p.console.Wait(ctx)
	if waitErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("ConPTY helper timed out")
		}
		return fmt.Errorf("wait for ConPTY helper: %w", waitErr)
	}
	closeErr := p.close()
	if err := p.drainOutput(output, 5*time.Second); err != nil {
		return err
	}
	if closeErr != nil && !isExpectedWindowsPipeClose(closeErr) {
		return fmt.Errorf("close ConPTY helper: %w", closeErr)
	}
	if exitCode != 0 {
		return fmt.Errorf("ConPTY helper exited with code %d", exitCode)
	}
	return nil
}

func (p *windowsConPTYProcess) drainOutput(output *bytes.Buffer, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case event := <-p.events:
			output.Write(event.data)
			if event.done {
				if event.err != nil && !isExpectedWindowsPipeClose(event.err) {
					return fmt.Errorf("read ConPTY helper output: %w", event.err)
				}
				return nil
			}
		case <-deadline.C:
			return errors.New("timed out draining ConPTY helper output")
		}
	}
}

func (p *windowsConPTYProcess) close() error {
	if p.closed || p.console == nil {
		return nil
	}
	p.closed = true
	return p.console.Close()
}

func windowsConPTYOutputEnded(stage string, err error) error {
	if err == nil || isExpectedWindowsPipeClose(err) {
		return fmt.Errorf("%s: output pipe closed", stage)
	}
	return fmt.Errorf("%s: %w", stage, err)
}

func isExpectedWindowsPipeClose(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, windows.ERROR_BROKEN_PIPE) ||
		errors.Is(err, windows.ERROR_OPERATION_ABORTED) ||
		errors.Is(err, windows.ERROR_INVALID_HANDLE)
}

func windowsVisibleConPTYText(input []byte) []byte {
	visible := make([]byte, 0, len(input))
	for index := 0; index < len(input); {
		current := input[index]
		if current == 0x1b {
			index++
			if index >= len(input) {
				break
			}
			switch input[index] {
			case '[':
				index++
				for index < len(input) {
					value := input[index]
					index++
					if value >= 0x40 && value <= 0x7e {
						break
					}
				}
			case ']':
				index++
				for index < len(input) {
					if input[index] == 0x07 {
						index++
						break
					}
					if input[index] == 0x1b && index+1 < len(input) && input[index+1] == '\\' {
						index += 2
						break
					}
					index++
				}
			default:
				index++
			}
			continue
		}
		index++
		if current < 0x20 || current == 0x7f || (current >= 0x80 && current <= 0x9f) {
			continue
		}
		visible = append(visible, current)
	}
	return visible
}

func streamWindowsNativeOutput(reader io.Reader, events chan<- windowsOutputEvent) {
	buffer := make([]byte, 4096)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			data := append([]byte(nil), buffer[:count]...)
			events <- windowsOutputEvent{data: data}
		}
		if err != nil {
			events <- windowsOutputEvent{err: err, done: true}
			return
		}
	}
}
