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
	}
	for _, test := range cases {
		t.Run(test.text, func(t *testing.T) {
			if got := hasCompleteMath(test.text); got != test.complete {
				t.Fatalf("hasCompleteMath=%v, want %v", got, test.complete)
			}
		})
	}
}
