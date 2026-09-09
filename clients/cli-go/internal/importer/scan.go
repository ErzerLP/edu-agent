package importer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"golang.org/x/text/cases"
)

const MaxScanEntries = 5000

type ScanOptions struct {
	Job     bool     `json:"job,omitempty"`
	Path    string   `json:"path"`
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}
type ScanItem struct {
	Path     string              `json:"path"`
	Bytes    int64               `json:"bytes"`
	Status   string              `json:"status"`
	Reason   string              `json:"reason,omitempty"`
	Selected bool                `json:"selected"`
	Document *api.ImportDocument `json:"document,omitempty"`
}
type ScanReport struct {
	Items   []ScanItem `json:"items"`
	Stopped string     `json:"stopped,omitempty"`
}

func matches(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if strings.HasSuffix(pattern, "/**") && strings.HasPrefix(name, strings.TrimSuffix(pattern, "**")) {
			return true
		}
		if ok, _ := path.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

func Scan(ctx context.Context, options ScanOptions) ScanReport {
	report := ScanReport{Items: []ScanItem{}}
	for _, pattern := range append(append([]string{}, options.Include...), options.Exclude...) {
		if _, err := path.Match(pattern, ""); err != nil {
			report.Stopped = "包含/排除规则无效：" + pattern
			return report
		}
	}
	info, err := os.Lstat(options.Path)
	if err != nil {
		report.Stopped = err.Error()
		return report
	}
	if info.Mode()&os.ModeSymlink != 0 {
		report.Stopped = "不能扫描符号链接"
		return report
	}
	rootPath := options.Path
	if !info.IsDir() {
		rootPath = filepath.Dir(options.Path)
	}
	root, err := securefile.OpenRoot(rootPath)
	if err != nil {
		report.Stopped = err.Error()
		return report
	}
	defer root.Close()
	seen := map[string]int{}
	used, count, entries := 0, 0, 0
	budget := 12 << 20
	if options.Job {
		budget = 128 << 20
	}
	visit := func(current string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > MaxScanEntries {
			return fmt.Errorf("扫描达到 %d 项上限，请缩小目录", MaxScanEntries)
		}
		relative, relErr := filepath.Rel(rootPath, current)
		if relErr != nil {
			return relErr
		}
		name := filepath.ToSlash(relative)
		if name == "." && walkErr == nil && entry.IsDir() {
			return nil
		}
		item := ScanItem{Path: name, Status: "error"}
		if walkErr != nil {
			item.Reason = walkErr.Error()
			report.Items = append(report.Items, item)
			return nil
		}
		if info, err := entry.Info(); err == nil {
			item.Bytes = info.Size()
		}
		if matches(options.Exclude, name) || (entry.IsDir() && matches(options.Exclude, name+"/")) {
			item.Status, item.Reason = "excluded", "被排除规则匹配"
			report.Items = append(report.Items, item)
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if len(options.Include) > 0 && !matches(options.Include, name) {
			item.Status, item.Reason = "excluded", "未匹配包含规则"
		} else if entry.Type()&os.ModeSymlink != 0 {
			item.Reason = "符号链接不可导入"
		} else if err := validatePath(name); err != nil {
			item.Reason = err.Error()
		} else if ext := strings.ToLower(path.Ext(name)); ext != ".md" && ext != ".txt" {
			item.Status, item.Reason = "unsupported", "只支持 Markdown 与 UTF-8 纯文本"
		} else {
			data, readErr := root.ReadLimit(name, MaxDocumentSize, false)
			if readErr == nil {
				item.Bytes = int64(len(data))
			}
			if readErr != nil {
				item.Reason = readErr.Error()
			} else if !utf8.Valid(data) {
				item.Reason = "编码错误：仅接受 UTF-8，请转换后重新扫描"
			} else {
				document := api.ImportDocument{Path: name, Markdown: string(data)}
				if strings.EqualFold(path.Ext(name), ".txt") {
					document = TextDocument(name, string(data), "text/plain; charset=utf-8")
				}
				if len(document.Markdown) > MaxDocumentSize {
					item.Reason = "转换后的内容超过单文件4 MiB预算"
					report.Items = append(report.Items, item)
					return nil
				}
				if err := validatePath(document.Path); err != nil {
					item.Reason = "转换后的目标路径无效：" + err.Error()
					report.Items = append(report.Items, item)
					return nil
				}
				key := cases.Fold().String(document.Path)
				if previous, duplicate := seen[key]; duplicate {
					item.Reason = "规范路径与另一文件冲突"
					report.Items[previous].Status, report.Items[previous].Reason, report.Items[previous].Selected = "error", item.Reason, false
				} else if count >= MaxDocuments || used+len(document.Markdown) > budget {
					item.Reason = fmt.Sprintf("超出1000篇或%d MiB内容预算，请缩小选择", budget>>20)
				} else {
					seen[key] = len(report.Items)
					used += len(document.Markdown)
					count++
					item.Status, item.Selected, item.Document = "ready", true, &document
				}
			}
		}
		report.Items = append(report.Items, item)
		return nil
	}
	// 目录枚举也遵守预算，不能先把任意大目录全部读入内存再计数。
	enumerated := 0
	var walk func(string, fs.FileInfo, int) error
	walk = func(current string, info fs.FileInfo, depth int) error {
		if depth > 64 {
			return fmt.Errorf("目录深度超过64层：%s", current)
		}
		if err := visit(current, fs.FileInfoToDirEntry(info), nil); err != nil {
			if err == filepath.SkipDir {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(rootPath, current)
		if err != nil {
			return err
		}
		remaining := MaxScanEntries - enumerated
		if remaining < 1 {
			return fmt.Errorf("目录枚举超过 %d 项，请缩小来源", MaxScanEntries)
		}
		names, skipped, complete, err := root.ReadDir(filepath.ToSlash(relative), remaining)
		if err != nil {
			return visit(current, fs.FileInfoToDirEntry(info), err)
		}
		enumerated += len(names) + skipped
		if skipped > 0 {
			report.Items = append(report.Items, ScanItem{Path: filepath.ToSlash(relative), Status: "error", Reason: fmt.Sprintf("枚举时有 %d 项已消失或无法读取，请重新扫描", skipped)})
		}
		sort.Slice(names, func(i, j int) bool { return names[i].Name < names[j].Name })
		for _, name := range names {
			child := filepath.Join(current, name.Name)
			childInfo, err := os.Lstat(child)
			if err != nil {
				if err = visit(child, nil, err); err != nil {
					return err
				}
				continue
			}
			if err = walk(child, childInfo, depth+1); err != nil {
				return err
			}
		}
		if !complete {
			return fmt.Errorf("目录枚举超过 %d 项，请缩小来源", MaxScanEntries)
		}
		return nil
	}
	if err := walk(options.Path, info, 0); err != nil {
		report.Stopped = err.Error()
	}
	return report
}

// 纯文本以可逆代码块保存，来源只包含相对名称、编码及原始字节摘要。
func TextDocument(name, text, kind string) api.ImportDocument {
	sum := sha256.Sum256([]byte(text))
	meta, _ := json.Marshal(map[string]string{"name": name, "type": kind, "sha256": hex.EncodeToString(sum[:])})
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return api.ImportDocument{Path: name + ".md", Markdown: "<!-- import-source-v1 " + base64.RawURLEncoding.EncodeToString(meta) + " -->\n" + fence + "text\n" + text + "\n" + fence + "\n"}
}

func (r ScanReport) Documents() []api.ImportDocument {
	result := []api.ImportDocument{}
	for _, item := range r.Items {
		if item.Selected && item.Status == "ready" && item.Document != nil {
			result = append(result, *item.Document)
		}
	}
	return result
}
