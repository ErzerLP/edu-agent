package agentui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func renderMarkdown(value string, width int) string {
	value = safeTerminalText(value)
	if width < 16 {
		width = 16
	}
	lines := strings.Split(value, "\n")
	result := make([]string, 0, len(lines))
	codeLines := make([]string, 0)
	codeLanguage := ""
	inCode := false
	flushCode := func() {
		if len(codeLines) == 0 && !inCode {
			return
		}
		body := strings.Join(codeLines, "\n")
		if codeLanguage != "" {
			body = markdownCodeLanguageStyle.Render(codeLanguage) + "\n" + body
		}
		result = append(result, markdownCodeStyle.Width(max(16, width-2)).Render(body))
		codeLines = codeLines[:0]
		codeLanguage = ""
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inCode {
				inCode = false
				flushCode()
			} else {
				inCode = true
				codeLanguage = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			}
			continue
		}
		if inCode {
			codeLines = append(codeLines, line)
			continue
		}
		if trimmed == "" {
			result = append(result, "")
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "### "):
			result = append(result, markdownHeadingStyle.Width(width).Render(renderInlineMarkdown(strings.TrimSpace(trimmed[4:]))))
		case strings.HasPrefix(trimmed, "## "):
			result = append(result, markdownHeadingStyle.Width(width).Render(renderInlineMarkdown(strings.TrimSpace(trimmed[3:]))))
		case strings.HasPrefix(trimmed, "# "):
			result = append(result, markdownHeadingStyle.Width(width).Render(renderInlineMarkdown(strings.TrimSpace(trimmed[2:]))))
		case strings.HasPrefix(trimmed, "> "):
			result = append(result, markdownQuoteStyle.Width(max(16, width-2)).Render(renderInlineMarkdown(strings.TrimSpace(trimmed[2:]))))
		case hasListPrefix(trimmed):
			marker, content := splitListLine(trimmed)
			result = append(result, markdownListStyle.Width(width).Render(marker+" "+renderInlineMarkdown(content)))
		default:
			result = append(result, lipgloss.NewStyle().Width(width).Render(renderInlineMarkdown(line)))
		}
	}
	if inCode {
		flushCode()
	}
	return strings.TrimRight(strings.Join(result, "\n"), "\n")
}

func hasListPrefix(value string) bool {
	if strings.HasPrefix(value, "- ") || strings.HasPrefix(value, "* ") || strings.HasPrefix(value, "+ ") {
		return true
	}
	index := strings.IndexByte(value, '.')
	if index < 1 || index > 3 || index+1 >= len(value) || value[index+1] != ' ' {
		return false
	}
	for _, char := range value[:index] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func splitListLine(value string) (string, string) {
	if strings.HasPrefix(value, "- ") || strings.HasPrefix(value, "* ") || strings.HasPrefix(value, "+ ") {
		return "•", strings.TrimSpace(value[2:])
	}
	index := strings.IndexByte(value, '.')
	return value[:index+1], strings.TrimSpace(value[index+1:])
}

func renderInlineMarkdown(value string) string {
	parts := strings.Split(value, "`")
	for index := range parts {
		if index%2 == 1 {
			parts[index] = markdownInlineCodeStyle.Render(parts[index])
			continue
		}
		parts[index] = renderBoldMarkdown(parts[index])
	}
	return strings.Join(parts, "")
}

func renderBoldMarkdown(value string) string {
	parts := strings.Split(value, "**")
	if len(parts) < 3 {
		return value
	}
	for index := 1; index < len(parts); index += 2 {
		parts[index] = markdownBoldStyle.Render(parts[index])
	}
	return strings.Join(parts, "")
}

var (
	markdownHeadingStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	markdownListStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("7")).PaddingLeft(1)
	markdownQuoteStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(lipgloss.Color("8")).PaddingLeft(1)
	markdownCodeStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("7")).Background(lipgloss.Color("0")).BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(lipgloss.Color("8")).Padding(0, 1)
	markdownCodeLanguageStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("8"))
	markdownInlineCodeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	markdownBoldStyle         = lipgloss.NewStyle().Bold(true)
)
