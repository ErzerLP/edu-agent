package importer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanReportsEveryFileAndKeepsOnlySelection(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{"valid.md": "# Go\n", "plain.txt": "# literal\n```\n原文", "bad.md": string([]byte{255}), "skip.md": "hidden", "image.png": "image"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := Scan(t.Context(), ScanOptions{Path: root, Exclude: []string{"skip.md"}})
	if r.Stopped != "" || len(r.Items) != 5 || len(r.Documents()) != 2 {
		t.Fatalf("逐项报告：%+v", r)
	}
	statuses := map[string]string{}
	for _, i := range r.Items {
		statuses[i.Path] = i.Status
		if strings.Contains(i.Path, root) {
			t.Fatal("泄露绝对路径")
		}
	}
	if statuses["bad.md"] != "error" || statuses["skip.md"] != "excluded" || statuses["image.png"] != "unsupported" {
		t.Fatalf("状态：%v", statuses)
	}
	for i := range r.Items {
		if r.Items[i].Path == "valid.md" {
			r.Items[i].Selected = false
		}
	}
	if len(r.Documents()) != 1 || r.Documents()[0].Path != "plain.txt.md" {
		t.Fatal("取消选择仍在最终清单")
	}
	body := r.Documents()[0].Markdown
	if !strings.Contains(body, "````text\n# literal\n```\n原文\n````") {
		t.Fatalf("纯文本被当作 Markdown 改写：%s", body)
	}
	encoded := strings.TrimSuffix(strings.Split(body, "\n")[0], " -->")
	encoded = strings.TrimPrefix(encoded, "<!-- import-source-v1 ")
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]string
	if json.Unmarshal(raw, &metadata) != nil || metadata["name"] != "plain.txt" || len(metadata["sha256"]) != 64 {
		t.Fatalf("来源：%s", raw)
	}
}
func TestScanCancellationAndPathCollision(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := Scan(ctx, ScanOptions{Path: t.TempDir()}); r.Stopped == "" {
		t.Fatal("扫描未响应取消")
	}
	root := t.TempDir()
	for _, name := range []string{"file.txt", "file.txt.md"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("body"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := Scan(t.Context(), ScanOptions{Path: root})
	if len(r.Documents()) != 0 {
		t.Fatalf("转换后的路径冲突没有排除双方：%+v", r)
	}
}
