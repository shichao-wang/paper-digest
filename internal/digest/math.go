package digest

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var mathDelimiters = [][2]string{{"$$", "$$"}, {"$", "$"}, {`\(`, `\)`}, {`\[`, `\]`}}
var currencyAmount = regexp.MustCompile(`^\$[+-]?[0-9]+(?:[.,][0-9]+)*(?:[kKmMbBtT])?`)

// Recognize amounts, magnitude suffixes and numeric ranges. A following math
// closer or operator still identifies a formula; ambiguous math can use \(...\).
func isCurrency(text string, index int) bool {
	amount := currencyAmount.FindString(text[index:])
	if amount == "" {
		return false
	}
	next := index + len(amount)
	if next == len(text) {
		return true
	}
	char, _ := utf8.DecodeRuneInString(text[next:])
	if strings.ContainsRune("-–—~～", char) {
		_, width := utf8.DecodeRuneInString(text[next:])
		remainder := strings.TrimLeftFunc(text[next+width:], unicode.IsSpace)
		if !strings.HasPrefix(remainder, "$") {
			remainder = "$" + remainder
		}
		if other := currencyAmount.FindString(remainder); other != "" {
			if len(other) == len(remainder) {
				return true
			}
			following, _ := utf8.DecodeRuneInString(remainder[len(other):])
			return unicode.IsSpace(following) || strings.ContainsRune(".,;:!?，。；：！？、", following)
		}
	}
	return unicode.IsSpace(char) || strings.ContainsRune(".,;:!?，。；：！？、", char)
}

func mathDelimiterAt(text string, index int) (string, string) {
	for _, pair := range mathDelimiters {
		if strings.HasPrefix(text[index:], pair[0]) {
			return pair[0], pair[1]
		}
	}
	return "", ""
}

func hasCompleteMath(text string) bool {
	text = mathMarkdownText(text)
	for index := 0; index < len(text); {
		left, right := mathDelimiterAt(text, index)
		if left == "" || (left == "$" && isCurrency(text, index)) {
			if strings.HasPrefix(text[index:], `\)`) || strings.HasPrefix(text[index:], `\]`) {
				return false
			}
			if text[index] == '\\' {
				index += 2
			} else {
				index++
			}
			continue
		}
		end := index + len(left)
		braces := 0
		closed := false
		for end < len(text) {
			if braces == 0 {
				if strings.HasPrefix(text[end:], right) && (right != "$" || !strings.HasPrefix(text[end:], "$$")) {
					index = end + len(right)
					closed = true
					break
				}
				if next, _ := mathDelimiterAt(text, end); next != "" {
					return false
				}
				if strings.HasPrefix(text[end:], `\)`) || strings.HasPrefix(text[end:], `\]`) {
					return false
				}
			}
			if text[end] == '\\' {
				end += 2
			} else {
				if text[end] == '{' {
					braces++
				}
				if text[end] == '}' && braces > 0 {
					braces--
				}
				end++
			}
		}
		if !closed {
			return false
		}
	}
	return true
}
