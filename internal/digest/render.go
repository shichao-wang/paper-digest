package digest

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/papers"
)

type Item struct {
	Paper   papers.Paper
	Summary Summary
}

func Render(date time.Time, items []Item) string {
	var output strings.Builder
	fmt.Fprintf(&output, "# arXiv 论文日报 · %s\n\n基于论文公开摘要整理，非全文解读。\n\n", date.Format("2006-01-02"))
	if len(items) == 0 {
		output.WriteString("今日未发现符合筛选条件的论文。\n")
		return output.String()
	}

	for i, item := range items {
		paper := item.Paper
		title := oneLine(paper.Title)
		if title == "" {
			title = "（无标题）"
		}
		fmt.Fprintf(&output, "## %d. %s\n\n", i+1, escapeHeading(title))

		id := paper.ID + paper.Version
		link := paper.URL
		if link == "" && paper.ID != "" {
			link = "https://arxiv.org/abs/" + strings.TrimPrefix(paper.ID, "arxiv:") + paper.Version
		}
		if id != "" && link != "" {
			fmt.Fprintf(&output, "- 原文：[%s](%s)\n", escapeLinkText(id), escapeURL(link))
		} else if link != "" {
			fmt.Fprintf(&output, "- 原文：[%s](%s)\n", escapeLinkText(link), escapeURL(link))
		} else {
			output.WriteString("- 原文：链接不可用\n")
		}

		authors := "未提供"
		if len(paper.Authors) > 0 {
			clean := make([]string, 0, len(paper.Authors))
			for _, author := range paper.Authors {
				if author = oneLine(author); author != "" {
					clean = append(clean, author)
				}
			}
			if len(clean) > 0 {
				authors = strings.Join(clean, "、")
			}
		}
		fmt.Fprintf(&output, "- 作者：%s\n", escapeInline(authors))
		if !paper.Published.IsZero() {
			fmt.Fprintf(&output, "- 发布：%s\n", paper.Published.Format("2006-01-02"))
		}
		if !paper.Updated.IsZero() && !paper.Updated.Equal(paper.Published) {
			fmt.Fprintf(&output, "- 更新：%s\n", paper.Updated.Format("2006-01-02"))
		}
		if item.Summary.Model != "" || item.Summary.PromptVersion != "" {
			fmt.Fprintf(&output, "- 摘要生成：%s（提示版本：%s）\n", escapeInline(item.Summary.Model), escapeInline(item.Summary.PromptVersion))
		}
		output.WriteByte('\n')
		if text := strings.TrimSpace(item.Summary.Text); text != "" {
			output.WriteString(text)
			output.WriteString("\n\n")
		} else {
			output.WriteString("摘要暂不可用。\n\n")
		}
	}
	return strings.TrimRight(output.String(), "\n") + "\n"
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func escapeHeading(value string) string {
	return escapeInline(value)
}

func escapeInline(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	for _, char := range []string{"`", "*", "_", "[", "]", "<", ">", "|", "#"} {
		value = strings.ReplaceAll(value, char, "\\"+char)
	}
	return value
}

func escapeLinkText(value string) string {
	return escapeInline(value)
}

func escapeURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err == nil {
		value = parsed.String()
	}
	value = strings.ReplaceAll(value, "\\", "%5C")
	value = strings.ReplaceAll(value, "(", "%28")
	value = strings.ReplaceAll(value, ")", "%29")
	return value
}
