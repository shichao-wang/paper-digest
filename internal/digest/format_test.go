package digest

import "testing"

func TestFormatSummaryLabelsAndPreserveContent(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"plain", "- 研究问题：正文\n\n- NPS/TTF 实现：验证 $x:y$\n- 意义与局限：更多", "- **研究问题**：正文\n\n- **NPS/TTF 实现**：验证 $x:y$\n- **意义与局限**：更多"},
		{"mixed bold", "- **方法**：正文\n- **主要结果：**有效\n1. 局限: 未讨论", "- **方法**：正文\n- **主要结果**：有效\n1. **局限**： 未讨论"},
		{"paragraph label", "研究问题：正文\n以下要点仅依据给定摘要信息：\n正文中的冒号：保持", "**研究问题**：正文\n以下要点仅依据给定摘要信息：\n正文中的冒号：保持"},
		{"links and math", "- [原文](https://arxiv.org/abs/2610.01234)\n- https://example.com\n- $x:y$ 表达式\n- `code: value`", "- [原文](https://arxiv.org/abs/2610.01234)\n- https://example.com\n- $x:y$ 表达式\n- `code: value`"},
		{"fenced code", "````text\n```\n- 方法：原样\n````\n- 方法：格式化", "````text\n```\n- 方法：原样\n````\n- **方法**：格式化"},
		{"math block", "$$\n- 方法：原样\n$$\n\\[\n- 结果：原样\n\\]\n- 结果：格式化", "$$\n- 方法：原样\n$$\n\\[\n- 结果：原样\n\\]\n- **结果**：格式化"},
		{"no label", "- 全句正文没有标签\n", "- 全句正文没有标签\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatSummary(tc.input)
			if got != tc.want {
				t.Fatalf("got %q; want %q", got, tc.want)
			}
			if twice := FormatSummary(got); twice != got {
				t.Fatalf("not idempotent: %q", twice)
			}
		})
	}
}
