//go:build windows

package terminal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsWaitTimeout = 258

type windowsOutputEvent struct {
	data []byte
	err  error
	done bool
}

func runNativeSecretProbe(secret string) ([]byte, string, error) {
	const method = "windows-conpty+xterm-readpassword"
	var inputRead, inputWrite windows.Handle
	var outputRead, outputWrite windows.Handle
	var pseudoConsole windows.Handle
	defer func() {
		for _, handle := range []windows.Handle{inputRead, inputWrite, outputRead, outputWrite} {
			if handle != 0 {
				_ = windows.CloseHandle(handle)
			}
		}
		if pseudoConsole != 0 {
			windows.ClosePseudoConsole(pseudoConsole)
		}
	}()

	if err := windows.CreatePipe(&inputRead, &inputWrite, nil, 0); err != nil {
		return nil, method, fmt.Errorf("create ConPTY input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outputRead, &outputWrite, nil, 0); err != nil {
		return nil, method, fmt.Errorf("create ConPTY output pipe: %w", err)
	}
	if err := windows.CreatePseudoConsole(windows.Coord{X: 80, Y: 25}, inputRead, outputWrite, 0, &pseudoConsole); err != nil {
		return nil, method, fmt.Errorf("create ConPTY: %w", err)
	}
	_ = windows.CloseHandle(inputRead)
	inputRead = 0
	_ = windows.CloseHandle(outputWrite)
	outputWrite = 0

	inputFile := os.NewFile(uintptr(inputWrite), "conpty-input")
	if inputFile == nil {
		return nil, method, errors.New("wrap ConPTY input handle")
	}
	inputWrite = 0
	defer inputFile.Close()
	outputFile := os.NewFile(uintptr(outputRead), "conpty-output")
	if outputFile == nil {
		return nil, method, errors.New("wrap ConPTY output handle")
	}
	outputRead = 0
	defer outputFile.Close()

	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, method, fmt.Errorf("allocate ConPTY process attributes: %w", err)
	}
	defer attributes.Delete()
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, unsafe.Pointer(&pseudoConsole), unsafe.Sizeof(pseudoConsole)); err != nil {
		return nil, method, fmt.Errorf("bind ConPTY process attribute: %w", err)
	}
	startup := windows.StartupInfoEx{
		StartupInfo:             windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{}))},
		ProcThreadAttributeList: attributes.List(),
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, method, fmt.Errorf("resolve native helper executable: %w", err)
	}
	executableUTF16, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return nil, method, err
	}
	commandLineUTF16, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{executable, "-test.run=^TestPlatformPairSecretInput$", "-test.count=1"}))
	if err != nil {
		return nil, method, err
	}

	previousHelper, helperWasSet := os.LookupEnv(nativeSecretHelperEnvironment)
	if err := os.Setenv(nativeSecretHelperEnvironment, "1"); err != nil {
		return nil, method, fmt.Errorf("set native helper environment: %w", err)
	}
	restoreEnvironment := func() error {
		if helperWasSet {
			return os.Setenv(nativeSecretHelperEnvironment, previousHelper)
		}
		return os.Unsetenv(nativeSecretHelperEnvironment)
	}
	processInfo := new(windows.ProcessInformation)
	createErr := windows.CreateProcess(
		executableUTF16,
		commandLineUTF16,
		nil,
		nil,
		false,
		windows.CREATE_DEFAULT_ERROR_MODE|windows.CREATE_UNICODE_ENVIRONMENT|windows.EXTENDED_STARTUPINFO_PRESENT,
		nil,
		nil,
		&startup.StartupInfo,
		processInfo,
	)
	restoreErr := restoreEnvironment()
	if createErr != nil {
		return nil, method, fmt.Errorf("start ConPTY helper: %w", createErr)
	}
	if restoreErr != nil {
		_ = windows.TerminateProcess(processInfo.Process, 1)
		_, _ = windows.WaitForSingleObject(processInfo.Process, 5000)
		_ = windows.CloseHandle(processInfo.Thread)
		_ = windows.CloseHandle(processInfo.Process)
		return nil, method, fmt.Errorf("restore native helper environment: %w", restoreErr)
	}
	defer windows.CloseHandle(processInfo.Process)
	_ = windows.CloseHandle(processInfo.Thread)
	processExited := false
	defer func() {
		if !processExited {
			_ = windows.TerminateProcess(processInfo.Process, 1)
			_, _ = windows.WaitForSingleObject(processInfo.Process, 5000)
		}
	}()

	events := make(chan windowsOutputEvent, 64)
	go streamWindowsNativeOutput(outputFile, events)
	var output bytes.Buffer
	promptDeadline := time.NewTimer(10 * time.Second)
	defer promptDeadline.Stop()
	for !bytes.Contains(output.Bytes(), []byte(nativeSecretPrompt)) {
		select {
		case event := <-events:
			output.Write(event.data)
			if event.done {
				return nil, method, fmt.Errorf("ConPTY helper ended before prompt: %w", event.err)
			}
		case <-promptDeadline.C:
			return nil, method, errors.New("timed out waiting for ConPTY secret prompt")
		}
	}

	// ReadPassword disables console echo immediately after printing the prompt.
	time.Sleep(100 * time.Millisecond)
	if _, err := io.WriteString(inputFile, secret+"\r\n"); err != nil {
		return nil, method, fmt.Errorf("write ConPTY secret: %w", err)
	}
	waitResult, err := windows.WaitForSingleObject(processInfo.Process, 10000)
	if err != nil {
		return nil, method, fmt.Errorf("wait for ConPTY helper: %w", err)
	}
	if waitResult == windowsWaitTimeout {
		return nil, method, errors.New("ConPTY helper timed out")
	}
	if waitResult != windows.WAIT_OBJECT_0 {
		return nil, method, fmt.Errorf("unexpected ConPTY wait result: %d", waitResult)
	}
	processExited = true
	var exitCode uint32
	if err := windows.GetExitCodeProcess(processInfo.Process, &exitCode); err != nil {
		return nil, method, fmt.Errorf("read ConPTY helper exit code: %w", err)
	}
	if exitCode != 0 {
		return nil, method, fmt.Errorf("ConPTY helper exited with code %d", exitCode)
	}

	windows.ClosePseudoConsole(pseudoConsole)
	pseudoConsole = 0
	_ = inputFile.Close()
	drainDeadline := time.NewTimer(5 * time.Second)
	defer drainDeadline.Stop()
	for {
		select {
		case event := <-events:
			output.Write(event.data)
			if event.done {
				if event.err != nil && !errors.Is(event.err, io.EOF) && !errors.Is(event.err, windows.ERROR_BROKEN_PIPE) && !errors.Is(event.err, windows.ERROR_OPERATION_ABORTED) {
					return nil, method, fmt.Errorf("read ConPTY helper output: %w", event.err)
				}
				return output.Bytes(), method, nil
			}
		case <-drainDeadline.C:
			return nil, method, errors.New("timed out draining ConPTY helper output")
		}
	}
}

func streamWindowsNativeOutput(file *os.File, events chan<- windowsOutputEvent) {
	buffer := make([]byte, 4096)
	for {
		count, err := file.Read(buffer)
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
