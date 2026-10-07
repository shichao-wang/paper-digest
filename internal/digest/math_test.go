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
