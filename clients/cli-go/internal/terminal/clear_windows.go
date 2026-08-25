//go:build windows

package terminal

import (
	"errors"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	fillConsoleOutputCharacter = kernel32.NewProc("FillConsoleOutputCharacterW")
	fillConsoleOutputAttribute = kernel32.NewProc("FillConsoleOutputAttribute")
	setConsoleModeForClear     = windows.SetConsoleMode
)

func clearScreen(out io.Writer) error {
	file, ok := out.(*os.File)
	if !ok {
		return errors.New("Windows terminal output is not a console file")
	}
	handle := windows.Handle(file.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err == nil {
		if err := setConsoleModeForClear(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err == nil {
			_, err = io.WriteString(file, "\x1b[2J\x1b[H> ")
			return err
		}
	}
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(handle, &info); err != nil {
		return err
	}
	width := uint32(info.Window.Right - info.Window.Left + 1)
	height := uint32(info.Window.Bottom - info.Window.Top + 1)
	count := width * height
	origin := windows.Coord{X: info.Window.Left, Y: info.Window.Top}
	packed := uintptr(uint16(origin.X)) | uintptr(uint16(origin.Y))<<16
	var written uint32
	if result, _, callErr := fillConsoleOutputCharacter.Call(uintptr(handle), uintptr(' '), uintptr(count), packed, uintptr(unsafe.Pointer(&written))); result == 0 {
		return callErr
	}
	if result, _, callErr := fillConsoleOutputAttribute.Call(uintptr(handle), uintptr(info.Attributes), uintptr(count), packed, uintptr(unsafe.Pointer(&written))); result == 0 {
		return callErr
	}
	if err := windows.SetConsoleCursorPosition(handle, origin); err != nil {
		return err
	}
	_, err := io.WriteString(file, "> ")
	return err
}
