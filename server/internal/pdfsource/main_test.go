package pdfsource

import (
	"context"
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// 与服务启动一致，可信 WASM 冷编译不占用单份 PDF 的处理预算。
	if err := Prepare(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "初始化 PDF 解析器失败:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
