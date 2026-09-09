//go:build !windows

package dashboard

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestWorkbenchPTYKeepsAlternateScreenAcrossSubmit(t *testing.T) {
	primary, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	defer terminal.Close()
	if err := pty.Setsize(primary, &pty.Winsize{Rows: 30, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	runner := &Runner{In: terminal, Out: terminal, Workbench: workbenchStub{}}
	done := make(chan error, 1)
	go func() { _, _, err := runner.Run(ctx, Snapshot{LocalState: LocalStatePaired}); done <- err }()
	chunks := make(chan string, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := primary.Read(buf)
			if n > 0 {
				select {
				case chunks <- string(buf[:n]):
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	var output strings.Builder
	wait := func(target string) {
		t.Helper()
		var recent strings.Builder
		for !strings.Contains(recent.String(), target) {
			select {
			case chunk := <-chunks:
				recent.WriteString(chunk)
				output.WriteString(chunk)
			case <-ctx.Done():
				t.Fatalf("未显示 %q，输出：%s", target, output.String())
			}
		}
	}
	send := func(value string) {
		t.Helper()
		if _, err := io.WriteString(primary, value); err != nil {
			t.Fatal(err)
		}
	}
	wait("edu-agent")
	send("b")
	wait("overview")
	send("3")
	wait("goals")
	send(strings.Repeat("\x1b[B", 8) + "\r")
	wait("目标草稿")
	send("\x1b[200~中文目标\n第二行\x1b[201~")
	wait("第二行")
	send("\x13")
	wait("操作")
	if strings.Contains(output.String(), "\x1b[?1049l") {
		t.Fatal("提交目标时退出了全屏")
	}
	send("5")
	wait("reviews")
	send("\x03")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("工作台无法退出")
	}
}
