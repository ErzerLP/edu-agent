//go:build !windows

package dashboard

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestRunnerUsesFullScreenAndQuitsFromPTY(t *testing.T) {
	primary, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	defer terminal.Close()
	if err := pty.Setsize(primary, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}

	type result struct {
		args []string
		quit bool
		err  error
	}
	resultCh := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		args, quit, runErr := (Runner{In: terminal, Out: terminal}).Run(ctx, Snapshot{})
		resultCh <- result{args: args, quit: quit, err: runErr}
	}()

	outputCh := make(chan []byte, 1)
	go func() {
		var output bytes.Buffer
		buffer := make([]byte, 4096)
		for {
			count, readErr := primary.Read(buffer)
			if count > 0 {
				output.Write(buffer[:count])
				if strings.Contains(output.String(), "edu-agent") {
					outputCh <- output.Bytes()
					return
				}
			}
			if readErr != nil {
				outputCh <- output.Bytes()
				return
			}
		}
	}()

	select {
	case output := <-outputCh:
		if !bytes.Contains(output, []byte("\x1b[?1049h")) {
			t.Fatalf("alternate screen sequence missing from %q", output)
		}
	case <-ctx.Done():
		t.Fatal("dashboard did not render before timeout")
	}
	if _, err := io.WriteString(primary, "q"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resultCh:
		if got.err != nil || !got.quit || len(got.args) != 0 {
			t.Fatalf("result args=%#v quit=%t err=%v", got.args, got.quit, got.err)
		}
	case <-ctx.Done():
		t.Fatal("dashboard did not quit from q before timeout")
	}
}
