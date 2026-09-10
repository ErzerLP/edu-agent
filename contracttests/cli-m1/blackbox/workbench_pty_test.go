package blackbox

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// script 提供 Linux 原生 PTY；运行真实 CLI 主入口，测试失败不输出终端正文。
func (h *harness) workbenchPTY(t *testing.T, marker string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("此用例需要 Linux 原生 PTY")
	}
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("未找到 script，未验证原生 PTY")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "script", "-q", "-e", "-c", "stty rows 40 cols 160 && exec \"$EDU_AGENT_TEST_CLI\"", "/dev/null")
	cmd.Env = append(os.Environ(), "EDU_AGENT_TEST_CLI="+cliBin, "HOME="+filepath.Join(h.primaryHome, "home"), "XDG_CONFIG_HOME="+filepath.Join(h.primaryHome, "config"), "TERM=xterm-256color", "NO_COLOR=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	chunks := make(chan string, 64)
	go func() {
		defer close(chunks)
		buffer := make([]byte, 4096)
		for {
			n, err := stdout.Read(buffer)
			if n > 0 {
				select {
				case chunks <- string(buffer[:n]):
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	output := ""
	wait := func(want string) {
		t.Helper()
		for !strings.Contains(output, want) {
			select {
			case chunk, ok := <-chunks:
				if !ok {
					t.Fatalf("PTY 在显示 %s 前退出", want)
				}
				output += chunk
				if strings.Contains(output, "protocol_error") {
					t.Fatal("主入口工作台出现协议错误")
				}
			case <-ctx.Done():
				t.Fatalf("PTY 等待 %s 超时", want)
			}
		}
	}
	wait("学习工作台")
	io.WriteString(stdin, "b")
	wait("学习进度与待办")
	wait(marker)
	if !strings.Contains(output, "默认学习区") {
		t.Fatal("工作台没有呈现真实学习区名称")
	}
	io.WriteString(stdin, "7")
	wait("1–7 切页")
	io.WriteString(stdin, "\x1b")
	io.WriteString(stdin, "\x03")
}
