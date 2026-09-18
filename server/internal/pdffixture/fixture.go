// Package pdffixture 生成可由独立 PDF 阅读器打开的确定性测试文件。
package pdffixture

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"
	"unicode/utf16"
)

// Build 使用标准字体和明确的 ToUnicode 映射；空字符串生成仅含图片的扫描页。
func Build(pages ...string) []byte { return BuildWithOptions(false, "", pages...) }

func BuildWithOptions(compressed bool, catalogExtra string, pages ...string) []byte {
	return build(compressed, catalogExtra, 0, pages...)
}

// CompressedPadding 以小输入制造大解压流，用于校验解析器线性内存上限。
func CompressedPadding(size int) []byte { return build(true, "", size, "资源攻击正文") }

func build(compressed bool, catalogExtra string, padding int, pages ...string) []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R " + catalogExtra + " >>", "", "<< /Type /Font /Subtype /Type0 /BaseFont /STSong-Light /Encoding /UniGB-UCS2-H /DescendantFonts [4 0 R] /ToUnicode 5 0 R >>", "<< /Type /Font /Subtype /CIDFontType0 /BaseFont /STSong-Light /CIDSystemInfo << /Registry (Adobe) /Ordering (GB1) /Supplement 4 >> >>"}
	cmap := "/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n/CMapName /Fixture def\n/CMapType 2 def\n1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n1 beginbfrange\n<0000> <FFFF> <0000>\nendbfrange\nendcmap\nCMapName currentdict /CMap defineresource pop\nend\nend"
	objects = append(objects, stream(cmap, false))
	kids := []string{}
	for _, text := range pages {
		n := len(objects) + 1
		kids = append(kids, fmt.Sprintf("%d 0 R", n))
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", n+1))
		var content strings.Builder
		if text == "" {
			content.WriteString("q 100 0 0 100 72 600 cm BI /W 1 /H 1 /BPC 8 /CS /RGB ID \xff\x00\x00 EI Q")
		} else {
			content.WriteString("BT /F1 16 Tf 72 760 Td ")
			for i, line := range strings.Split(text, "\n") {
				if i > 0 {
					content.WriteString("0 -24 Td ")
				}
				content.WriteByte('<')
				for _, c := range utf16.Encode([]rune(line)) {
					fmt.Fprintf(&content, "%04X", c)
				}
				content.WriteString("> Tj\n")
			}
			content.WriteString("ET")
		}
		if padding > 0 {
			var compressed bytes.Buffer
			w := zlib.NewWriter(&compressed)
			chunk := bytes.Repeat([]byte{' '}, 1<<20)
			for left := padding; left > 0; left -= min(left, len(chunk)) {
				_, _ = w.Write(chunk[:min(left, len(chunk))])
			}
			_, _ = w.Write([]byte(content.String()))
			_ = w.Close()
			objects = append(objects, fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream", compressed.Len(), compressed.String()))
		} else {
			objects = append(objects, stream(content.String(), compressed))
		}
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] >>", len(pages), strings.Join(kids, " "))
	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n")
	offsets := []int{0}
	for i, o := range objects {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

func stream(text string, compressed bool) string {
	filter := ""
	if compressed {
		var out bytes.Buffer
		w := zlib.NewWriter(&out)
		_, _ = w.Write([]byte(text))
		_ = w.Close()
		text = out.String()
		filter = " /Filter /FlateDecode"
	}
	return fmt.Sprintf("<< /Length %d%s >>\nstream\n%s\nendstream", len(text), filter, text)
}
