//go:build windows

package terminal

import (
	"os"
	"testing"
)

func TestPlatformClear(t *testing.T) {
	console, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer console.Close()
	if err := clearScreen(console); err != nil {
		t.Fatal(err)
	}
}
