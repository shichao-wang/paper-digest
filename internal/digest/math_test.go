package digest

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/shichao-wang/paper-digest/internal/papers"
)

func TestClaudeAnalyzerMathDelimiters(t *testing.T) {
	data, err := os.ReadFile("../../testdata/math-delimiters.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Text     string
		Complete bool
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		t.Run(test.Text, func(t *testing.T) {
			analyzer := ClaudeAnalyzer{request: func(context.Context, string, string, string, string) (string, error) {
				return test.Text, nil
			}}
			summary, err := analyzer.Analyze(context.Background(), papers.Paper{})
			if test.Complete {
				if err != nil || summary.Text != test.Text {
					t.Fatalf("完整公式或金额应原样通过: summary=%#v, err=%v", summary, err)
				}
			} else if err == nil || summary.Text != "" || !strings.Contains(err.Error(), "unclosed math") {
				t.Fatalf("未闭合公式不应保存: summary=%#v, err=%v", summary, err)
			}
		})
	}
}

func TestMathIgnoresMarkdownLiteralRegions(t *testing.T) {
	cases := []struct {
		text     string
		complete bool
	}{
		{"- **方法**：运行 `echo $HOME`", true},
		{"使用 ``echo `$HOME` \\( \\[``，公式 $x$", true},
		{"```sh\necho $HOME \\(\n```\n结果 $x$", true},
		{"~~~~sh\necho $HOME\n~~~\n\\[\n~~~~\n结果 $x$", true},
		{"```sh\necho $HOME", true},
		{"    echo $HOME \\[\n\techo $PATH", true},
		{`[代码](https://example.com/$HOME/a(b)/\[file "title")，结果 $x$`, true},
		{`![图](<https://example.com/$HOME/\[file>)`, true},
		{"[代码][ref]\n[ref]: https://example.com/$HOME/\\[file", true},
		{`<https://example.com/$HOME/\[file>`, true},
		{"`echo $HOME` 结果 $x", false},
		{"```sh\necho $HOME\n```\n结果 \\[x", false},
		{`[$x](https://example.com/$HOME)`, false},
		{"未闭合 ` 文本 $x", false},
		{`- **资源**：https://api.example/items?$filter=x`, true},
		{`http://api.example/\[file?$filter=x，结果 $x$`, true},
		{`https://api.example/items?$filter=x 结果 $x`, false},
		{`https://api.example/items?$filter=x，结果 \[x`, false},
		{"- **结果**：\n    公式为 $x", false},
		{"- **结果**：\n\n    公式为 $x", false},
		{"1. **结果**：\n    公式为 $x", false},
		{"- **结果**：\n  - 子项\n      公式为 $x", false},
		{"- **结果**：\n    公式为 $x$", true},
		{"- **方法**：\n\n      echo $HOME", true},
		{"1. **方法**：\n\n       echo $HOME", true},
		{"    - echo $HOME", true},
		{"- 方法\n\n      - echo $HOME", true},
		{"- 方法\n  ```sh\n  echo $HOME\n  ```\n    公式为 $x", false},
		{"- 方法\n    续行\n\n普通段落\n\n    echo $HOME", true},
		{"- 方法\n    续行\n\n普通段落\n\n    echo $HOME\n公式 $x", false},
	}
	for _, test := range cases {
		t.Run(test.text, func(t *testing.T) {
			if got := hasCompleteMath(test.text); got != test.complete {
				t.Fatalf("hasCompleteMath=%v, want %v", got, test.complete)
			}
		})
	}
}

func TestMathRecognizesCompactCurrency(t *testing.T) {
	cases := []struct {
		text     string
		complete bool
	}{
		{"成本 $5M", true},
		{"成本 $10k", true},
		{"成本 $5M、$10k、$2.5B、$1t。", true},
		{"预算 $5-$10", true},
		{"预算 $5M–$10M，结果 $x$", true},
		{"预算 $5 - $10 或 $5～10", true},
		{"预算 $5M，结果 $x", false},
		{"$5M$ 与 $10k$", true},
		{"$5M+x", false},
		{"$5-x", false},
	}
	for _, test := range cases {
		t.Run(test.text, func(t *testing.T) {
			if got := hasCompleteMath(test.text); got != test.complete {
				t.Fatalf("hasCompleteMath=%v, want %v", got, test.complete)
			}
		})
	}
}
