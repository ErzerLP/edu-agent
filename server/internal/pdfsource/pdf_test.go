package pdfsource

import (
	"bytes"
	"context"
	"encoding/base64"
	"image/png"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/pdffixture"
)

func TestStaticAndEncryptedPDF(t *testing.T) {
	data, err := os.ReadFile("testdata/bilingual-mixed.pdf")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(context.Background(), data)
	if err != nil || r.PageCount != 3 || !strings.Contains(r.Pages[1].Text, "第二物理页") || r.Pages[2].Status != "no_text" {
		t.Fatal("真实浏览器夹具不可解析", r, err)
	}
	if !slices.Contains(r.Pages[1].Gaps, "font_not_embedded") {
		t.Fatal("未嵌入字体的显示风险被隐藏")
	}
	embedded, err := os.ReadFile("testdata/bilingual-embedded.pdf.base64")
	if err != nil {
		t.Fatal(err)
	}
	data, err = base64.StdEncoding.DecodeString(string(embedded))
	if err != nil {
		t.Fatal(err)
	}
	r, err = Parse(context.Background(), data)
	if err != nil || r.PageCount != 3 || !strings.Contains(r.Pages[1].Text, "第二物理页") || r.Pages[1].Status != "text" {
		t.Fatal("嵌入字体夹具错误", r, err)
	}
	encoded, err := os.ReadFile("testdata/encrypted.pdf.base64")
	if err != nil {
		t.Fatal(err)
	}
	data, err = base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Parse(context.Background(), data); Code(err) != "pdf_encrypted" {
		t.Fatal("密码 PDF 未明确拒绝", err)
	}
}

func TestPDFConcurrentCancellation(t *testing.T) {
	data := pdffixture.Build("不应执行已取消任务")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := Parse(ctx, data); Code(err) != "pdf_timeout" {
				t.Errorf("取消未释放并发槽：%v", err)
			}
		})
	}
	wg.Wait()
	if len(slots) != 0 {
		t.Fatal("取消遗留了活动解析槽")
	}
}

func TestPDFExecutionDeadline(t *testing.T) {
	// 先完成可信 WASM 编译，随后中止实际文件解析，不以编译耗时冒充 CPU 上限验证。
	if _, err := Parse(context.Background(), pdffixture.Build("预热")); err != nil {
		t.Fatal(err)
	}
	data := pdffixture.BuildWithOptions(true, "", strings.Repeat("A", 500000))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Parse(ctx, data); Code(err) != "pdf_timeout" {
		t.Fatal("CPU 超时没有明确中止", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("超时未及时释放执行实例")
	}
}

func TestPDFPreparationContextDoesNotOwnFileInstances(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := Prepare(ctx); err != nil {
		t.Fatal("预编译失败", err)
	}
	cancel()
	if err := Prepare(ctx); Code(err) != "pdf_timeout" {
		t.Fatal("已取消的预编译未拒绝", err)
	}
	r, err := Parse(t.Context(), pdffixture.Build("独立文件实例"))
	if err != nil || r.Text() != "独立文件实例" {
		t.Fatal("预编译上下文取消影响后续文件实例", r, err)
	}
}

func TestPDFCompressedMemoryLimit(t *testing.T) {
	data := pdffixture.CompressedPadding((MemoryMiB + 32) << 20)
	if len(data) > MaxBytes {
		t.Fatal("压缩攻击夹具超出了输入限制")
	}
	r, err := Parse(context.Background(), data)
	if err == nil && (r.Text() != "" || r.Coverage != "partial_pdf") {
		t.Fatal("解压超过 WASM 内存上限仍被视为有效正文", r)
	}
	t.Logf("压缩输入 %d 字节，解析错误 %v，覆盖 %s", len(data), err, r.Coverage)
}

func TestRealPDFTextCoverageAndRendering(t *testing.T) {
	data := pdffixture.BuildWithOptions(true, "", "中文第一页\nEnglish first page", "第二页 source evidence", "", "中文第一页\nEnglish first page")
	r, err := Parse(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	if r.PageCount != 4 || r.Coverage != "partial_pdf" || !strings.Contains(r.Pages[0].Text, "中文第一页") || !strings.Contains(r.Pages[1].Text, "source evidence") || r.Pages[2].Status != "no_text" || r.Pages[3].DuplicateOf != 1 {
		t.Fatalf("逐页解析错误：%+v", r)
	}
	for _, p := range r.Pages {
		if p.Text != "" && r.Text()[p.Start:p.End] != p.Text {
			t.Fatal("页码偏移不对应原文")
		}
	}
	image, err := Render(context.Background(), data, 2)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(image))
	if err != nil || cfg.Width > 1000 || cfg.Height > 1400 {
		t.Fatal("页视图没有像素上限", cfg, err)
	}
}

func TestPDFLimitsAndUntrustedActions(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		code string
	}{
		{"损坏", []byte("%PDF-1.7\ninvalid\n%%EOF"), "pdf_invalid_or_resource_limit"},
		{"体积", bytes.Repeat([]byte{'x'}, MaxBytes+1), "pdf_file_limit"},
		{"页数", pdffixture.Build(make([]string, MaxPages+1)...), "pdf_page_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(context.Background(), tc.data)
			if err == nil || Code(err) != tc.code {
				t.Fatalf("错误类别：%v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Parse(ctx, pdffixture.Build("text")); Code(err) != "pdf_timeout" {
		t.Fatal("取消没有中止解析", err)
	}
	data := pdffixture.BuildWithOptions(false, `/OpenAction << /S /JavaScript /JS (while\(true\){}; app.launchURL\('http://127.0.0.1'\)) >>`, "真实正文")
	r, err := Parse(context.Background(), data)
	if err != nil || r.Pages[0].Text != "真实正文" {
		t.Fatal("脚本影响了原文解析", r, err)
	}
	r, err = Parse(context.Background(), pdffixture.BuildWithOptions(true, "", strings.Repeat("A", MaxText*4), "下一页"))
	if err != nil || len(r.Text()) > MaxText || r.Coverage != "partial_pdf" || r.Pages[1].Text != "" {
		t.Fatal("压缩文本预算未生效", err)
	}
}
