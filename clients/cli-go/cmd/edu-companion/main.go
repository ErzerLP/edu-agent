// edu-companion 是显式启动的独立适配器，不加载 CLI 配置、历史或钥匙串。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/companion"
	wire "github.com/edu-agent/edu-agent/packages/agentcore/companion"
)

func run() error {
	server := flag.String("server", "", "已配对学习 Web 的完整 HTTPS Origin")
	root := flag.String("workspace", "", "结构化文件工具的工作区绝对路径；不约束 Shell")
	allow := flag.Bool("allow-loopback-http", false, "仅回环 IP 的 HTTP 开发模式")
	flag.Parse()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return fmt.Errorf("当前平台没有原生 companion 支持")
	}
	if !filepath.IsAbs(*root) {
		return fmt.Errorf("必须显式指定 --workspace 绝对路径")
	}
	c, err := companion.NewClient(*server, *allow)
	if err != nil {
		return err
	}
	u, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		return fmt.Errorf("无法确认 OS 用户")
	}
	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("无法确认设备名称")
	}
	d := wire.Device{ID: wire.Secret(), Host: host, User: u.Username + " (uid=" + u.Uid + ")", OS: runtime.GOOS, Workspace: filepath.Clean(*root)}
	fmt.Printf("本地连接站点：%q\n设备：%q\nOS 用户：%q\n文件工作区：%q\n", c.Origin, d.Host, d.User, d.Workspace)
	fmt.Println("Shell 使用此 OS 用户原生权限，不受文件工作区限制；本地内容经该服务器发到浏览器，模型外发另行授权。不会读取 CLI 历史或钥匙串。退出将停止受管任务，但不承诺清理脱离进程组的进程。")
	fmt.Print("请粘贴 Web 本地连接页的一次性配对码（不要放入命令参数）：")
	scan := bufio.NewScanner(os.Stdin)
	if !scan.Scan() {
		return fmt.Errorf("未输入配对码")
	}
	parts := strings.Split(strings.TrimSpace(scan.Text()), ".")
	if len(parts) != 2 {
		return fmt.Errorf("配对码无效")
	}
	fmt.Print("输入“连接”明确允许此站点申请本机权限：")
	if !scan.Scan() || strings.TrimSpace(scan.Text()) != "连接" {
		return fmt.Errorf("未授权连接")
	}
	p, err := companion.NewProvider(d)
	if err != nil {
		return fmt.Errorf("工作区不可用")
	}
	defer func() {
		if err := p.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "受管任务清理未能全部确认，请在本机核查；不会继续控制旧 PID。")
		}
	}()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	channel, err := c.Attach(ctx, wire.Attach{ID: parts[0], Code: parts[1], Device: d})
	if err != nil {
		return err
	}
	fmt.Printf("设备身份：%s\n已连接，等待浏览器核对设备并授权。Ctrl+C 撤销本次本机连接。\n", d.ID)
	return c.Run(ctx, parts[0], channel.Token, p)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "本地连接结束：", err)
		os.Exit(1)
	}
}
