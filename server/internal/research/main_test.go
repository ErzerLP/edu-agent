package research

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
)

func TestMain(m *testing.M) {
	// 与服务启动一致，可信 WASM 冷编译不占用网页抓取和 PDF 的处理预算。
	if err := pdfsource.Prepare(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "初始化 PDF 解析器失败:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
