package digest

import (
	"regexp"
	"strings"
)

var referenceDestination = regexp.MustCompile(`^ {0,3}\[[^\]\n]+\]:[ \t]*\S`)

// Mask Markdown regions whose contents are literal, preserving line boundaries.
// Link labels remain visible to the math checker; destinations do not.
func mathMarkdownText(text string) string {
	masked := []byte(text)
	mask := func(start, end int) {
		for i := start; i < end; i++ {
			if masked[i] != '\n' {
				masked[i] = ' '
			}
		}
	}
	var fence byte
	var fenceLength int
	for start := 0; start < len(text); {
		end := start + strings.IndexByte(text[start:], '\n')
		if end < start {
			end = len(text)
		}
		line := text[start:end]
		trimmed := strings.TrimLeft(line, " \t")
		run := 0
		if len(trimmed) > 0 && (trimmed[0] == '`' || trimmed[0] == '~') {
			for run < len(trimmed) && trimmed[run] == trimmed[0] {
				run++
			}
		}
		if fence != 0 {
			mask(start, end)
			if run >= fenceLength && trimmed[0] == fence && strings.TrimSpace(trimmed[run:]) == "" {
				fence = 0
			}
		} else if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") || referenceDestination.MatchString(line) {
			mask(start, end)
		} else if run >= 3 && (trimmed[0] == '~' || !strings.Contains(trimmed[run:], "`")) {
			fence, fenceLength = trimmed[0], run
			mask(start, end)
		}
		start = end + 1
	}
	// Operate on the masked text so fenced content cannot start an inline span.
	text = string(masked)
	for i := 0; i < len(text); {
		if text[i] == '\\' {
			i += 2
			continue
		}
		if text[i] == '`' {
			run := 1
			for i+run < len(text) && text[i+run] == '`' {
				run++
			}
			end := i + run
			for end < len(text) {
				if text[end] != '`' {
					end++
					continue
				}
				count := 1
				for end+count < len(text) && text[end+count] == '`' {
					count++
				}
				if count == run {
					mask(i, end+count)
					i = end + count
					break
				}
				end += count
			}
			if end >= len(text) {
				i += run // An unmatched backtick is ordinary text.
			}
			continue
		}
		if strings.HasPrefix(text[i:], "](") {
			depth, end := 1, i+2
			for end < len(text) && depth > 0 {
				switch text[end] {
				case '\\':
					end++
				case '(':
					depth++
				case ')':
					depth--
				}
				end++
			}
			if depth == 0 {
				mask(i+2, end-1)
				i = end
				continue
			}
		}
		if text[i] == '<' && (strings.HasPrefix(text[i+1:], "https://") || strings.HasPrefix(text[i+1:], "http://") || strings.HasPrefix(text[i+1:], "mailto:")) {
			if end := strings.IndexByte(text[i:], '>'); end >= 0 {
				mask(i, i+end+1)
				i += end + 1
				continue
			}
		}
		i++
	}
	return string(masked)
}
