package agentui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func (p *localArtifactPanel) header(width int) []string {
	lines := []string{"完整差异 / 逐项回执 · 当前 Session", "diff不代表修改已执行", "receipt：逐项事实，不承诺全部成功"}
	if strings.HasPrefix(p.id, "b_") {
		lines[2] = "追加日志：plan非执行；pending无actual为未知"
	}
	if p.catalogError != "" {
		lines = append(lines, wrapDisplayLines("目录读取失败："+p.catalogError, width, 2)...)
	} else if !p.catalogReady {
		lines = append(lines, "产物目录尚未读取完成")
	} else if len(p.items) == 0 {
		lines = append(lines, "当前 Session 无可访问产物")
	} else {
		selected := 0
		for i, item := range p.items {
			if item.ID == p.id {
				selected = i
				break
			}
		}
		// Compact terminals keep one list row so metadata and actual content
		// remain visible. Larger terminals show neighbors around the selection.
		count := p.listRows
		start := max(0, selected-count/2)
		for i := start; i < min(len(p.items), start+count); i++ {
			item := p.items[i]
			prefix := "  "
			if i == selected {
				prefix = "› "
			}
			lines = append(lines, truncateDisplayWidth(fmt.Sprintf("%s%d/%d %s %s", prefix, i+1, len(p.items), safeSingleLineTerminalText(item.Kind), safeSingleLineTerminalText(item.ID)), width))
		}
		info := p.items[selected]
		lines = append(lines, wrapDisplayLines("kind="+safeSingleLineTerminalText(info.Kind)+" id="+safeSingleLineTerminalText(info.ID), width, 3)...)
		saved := "仅内存；退出不可恢复"
		if strings.HasPrefix(info.ID, "b_") {
			saved = "未完整保存（内存或部分保存）；恢复仅限认证范围"
		}
		if info.Saved {
			saved = "saved · 已加密保存"
		}
		lines = append(lines, wrapDisplayLines(fmt.Sprintf("完整字节=%d · %s", info.Bytes, saved), width, 2)...)
		lines = append(lines, wrapDisplayLines("hash="+safeSingleLineTerminalText(info.Hash), width, 3)...)
	}
	if p.id != "" && p.catalogReady {
		lines = append(lines, wrapDisplayLines(fmt.Sprintf("offset=%d next_offset=%d", p.page.Offset, p.page.NextOffset), width, 2)...)
		if p.pageReady {
			lines = append(lines, fmt.Sprintf("more=%t · 原始字节（4096/页）", p.page.More))
		} else {
			lines = append(lines, "more=未知 · 本页尚不可用")
		}
	}
	query := p.cursor().query
	if p.queryMode {
		query = p.queryDraft
	}
	if query != "" || p.queryMode {
		lines = append(lines, truncateDisplayWidth("检索："+safeSingleLineTerminalText(query), width))
	}
	if p.cursor().searched {
		lines = append(lines, truncateDisplayWidth(fmt.Sprintf("检索next=%d more=%t", p.cursor().searchNext, p.cursor().more), width))
	}
	if p.notice != "" {
		lines = append(lines, wrapDisplayLines(safeTerminalText(p.notice), width, 2)...)
	} else if p.loading {
		lines = append(lines, "正在读取原始字节页")
	}
	for i, line := range lines {
		lines[i] = truncateDisplayWidth(line, width)
	}
	return lines
}

func localArtifactFooter(width int) []string {
	return []string{
		truncateDisplayWidth("↑↓选择 PgUp/PgDn页 Home首字节 r刷新", width),
		truncateDisplayWidth("Ctrl↑↓滚动 /检索 n继续 Esc/F6返回", width),
	}
}

func (p *localArtifactPanel) resize(width, height int) {
	p.output.Width = max(1, width-4)
	// Use a size-only list choice: rendering must not change the viewport's
	// effective height, otherwise scrolling can stop before the visible EOF.
	p.listRows = 1
	if height >= 24 {
		p.listRows = 3
	}
	header := p.header(p.output.Width)
	p.output.Height = max(1, height-len(header)-len(localArtifactFooter(p.output.Width)))
	content := strings.Join(wrapDisplayLines(safeTerminalText(string(p.page.Data)), p.output.Width, 8192), "\n")
	p.output.SetContent(content)
}

func (p *localArtifactPanel) render(width int) string {
	width = max(1, width-4)
	lines := p.header(width)
	lines = append(lines, p.output.View())
	lines = append(lines, localArtifactFooter(width)...)
	return lipgloss.NewStyle().Width(width).Render(strings.Join(lines, "\n"))
}

// fileMutationPlanSummary is shared by the selector and transcript confirmation;
// neither representation makes reading the complete plan an approval gate.
func fileMutationPlanSummary(id string, bytes int64, saved bool) string {
	state := "仅内存；退出不可恢复"
	if saved {
		state = "saved · 已加密保存"
	}
	return fmt.Sprintf("F6 完整复制清单：%s · %d字节 · %s\n清单不代表已经执行；浏览不批准、不取消，也不增加末页审批门槛。", safeSingleLineTerminalText(id), bytes, state)
}

func fileMutationArtifactSummary(pendingID string, bytes int64, saved bool) string {
	state := "仅内存；退出不可恢复"
	if saved {
		state = "saved · 已加密保存"
	}
	return fmt.Sprintf("F6 完整差异：%s · %d字节 · %s\ndiff不代表修改已执行；浏览不改变授权。", safeSingleLineTerminalText(pendingID), bytes, state)
}
