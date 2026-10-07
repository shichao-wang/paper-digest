package digest

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var summaryListPrefix = regexp.MustCompile(`^[ \t]*(?:[-+*]|[0-9]+[.)])[ \t]+`)
var summaryLabel = regexp.MustCompile(`^[\p{Han}\p{L}\p{N} /／与及和、—-]+$`)
var summarySection = regexp.MustCompile(`^(研究|目标|问题|方法|结果|主要结果|结论|局限|贡献|评估|实验|意义|实现|资源)`)

// FormatSummary formats only short labels at the start of summary points.
// Content, links, formulas and code remain unchanged; applying it twice is safe.
func FormatSummary(text string) string {
	lines := strings.Split(text, "\n")
	var fence string
	var mathEnd string
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, string(fence[0])) == "" {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			char := trimmed[0]
			count := 0
			for count < len(trimmed) && trimmed[count] == char {
				count++
			}
			fence = trimmed[:count]
			continue
		}
		if mathEnd != "" {
			if trimmed == mathEnd {
				mathEnd = ""
			}
			continue
		}
		if trimmed == "$$" || trimmed == `\[` {
			mathEnd = "$$"
			if trimmed == `\[` {
				mathEnd = `\]`
			}
			continue
		}
		prefix := summaryListPrefix.FindString(line)
		rest := strings.TrimPrefix(line, prefix)
		colon := strings.IndexAny(rest, "：:")
		if colon < 0 {
			continue
		}
		_, width := utf8.DecodeRuneInString(rest[colon:])
		label, body := strings.TrimSpace(rest[:colon]), rest[colon+width:]
		if strings.HasPrefix(body, "//") {
			continue // A bare URL's scheme is not a point label.
		}
		// Accept both **方法**：正文 and **方法：**正文.
		if strings.HasPrefix(label, "**") {
			if strings.HasSuffix(label, "**") && len(label) > 4 {
				label = label[2 : len(label)-2]
			} else if strings.HasPrefix(body, "**") {
				label, body = strings.TrimPrefix(label, "**"), strings.TrimPrefix(body, "**")
			}
		}
		label = strings.TrimSpace(label)
		if utf8.RuneCountInString(label) > 24 || !summaryLabel.MatchString(label) || (prefix == "" && !summarySection.MatchString(label)) {
			continue
		}
		lines[i] = prefix + "**" + label + "**：" + body
	}
	return strings.Join(lines, "\n")
}
