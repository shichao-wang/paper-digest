package digest

import (
	"context"
	"strings"
	"testing"

	"github.com/shichao-wang/paper-digest/internal/papers"
)

func TestParseDecisionsRequiresEveryPaperOnce(t *testing.T) {
	text := "```json\n{\"decisions\":[{\"id\":\"arxiv:a\",\"relevant\":true,\"score\":0.9,\"reason\":\"检索排序\"},{\"id\":\"arxiv:b\",\"relevant\":false,\"score\":0.2,\"reason\":\"只是普通用词\"}]}\n```"
	got, err := ParseDecisions(text, []string{"arxiv:b", "arxiv:a"})
	if err != nil || len(got) != 2 || !got[0].Relevant || got[1].Relevant || got[1].Reason != "只是普通用词" {
		t.Fatalf("ParseDecisions() = %+v, %v", got, err)
	}
	for _, text := range []string{
		`{"decisions":[{"id":"arxiv:a","relevant":true,"score":0.9,"reason":"检索"}]}`,
		`{"decisions":[{"id":"arxiv:a","relevant":true,"score":1.2,"reason":"检索"},{"id":"arxiv:b","relevant":false,"score":0,"reason":"无关"}]}`,
		`{"decisions":[{"id":"arxiv:a","relevant":true,"score":0.4,"reason":""},{"id":"arxiv:b","relevant":false,"score":0,"reason":"无关"}]}`,
		`{"decisions":[{"id":"arxiv:a","score":0.4,"reason":"检索"},{"id":"arxiv:b","relevant":false,"score":0,"reason":"无关"}]}`,
		`{"decisions":[{"id":"arxiv:a","relevant":true,"score":0.4,"reason":"检索","note":"x"},{"id":"arxiv:b","relevant":false,"score":0,"reason":"无关"}]}`,
		`{"extra":1,"decisions":[{"id":"arxiv:a","relevant":true,"score":0.4,"reason":"检索"},{"id":"arxiv:b","relevant":false,"score":0,"reason":"无关"}]}`,
		"not json",
	} {
		if _, err := ParseDecisions(text, []string{"arxiv:a", "arxiv:b"}); err == nil {
			t.Fatalf("accepted %s", text)
		}
	}
}

func TestScreenRetriesInvalidJSONThenFailsClosed(t *testing.T) {
	list := []papers.Paper{{ID: "arxiv:a", Title: "Search ranking"}, {ID: "arxiv:b", Title: "Ads auction"}}
	calls := 0
	screener := Screener{
		Model: "deepseek-flash", Attempts: 3,
		request: func(context.Context, string, string, string, string) (string, error) {
			calls++
			return "not json", nil
		},
	}
	if _, err := screener.Screen(context.Background(), list); err == nil || calls != 3 || !strings.Contains(err.Error(), "3 次") {
		t.Fatalf("err = %v, calls = %d", err, calls)
	}

	calls = 0
	screener.request = func(context.Context, string, string, string, string) (string, error) {
		calls++
		if calls < 3 {
			return `{"decisions":[]}`, nil
		}
		return `{"decisions":[{"id":"arxiv:a","relevant":false,"score":0.1,"reason":"不是检索"},{"id":"arxiv:b","relevant":true,"score":0.8,"reason":"广告拍卖"}]}`, nil
	}
	got, err := screener.Screen(context.Background(), list)
	if err != nil || calls != 3 || len(got) != 2 || got[0].Skill == "" || !strings.HasPrefix(got[0].Skill, "paper-screening@") {
		t.Fatalf("got %+v err %v calls %d", got, err, calls)
	}
}

func TestScreenStopsWhenTheProcessIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	screener := Screener{
		request: func(context.Context, string, string, string, string) (string, error) {
			calls++
			return "", nil
		},
	}
	_, err := screener.Screen(ctx, []papers.Paper{{ID: "arxiv:a"}})
	if err == nil || calls != 0 {
		t.Fatalf("err = %v, calls = %d", err, calls)
	}
}

func TestScreenBatchesAndKeepsTheInstruction(t *testing.T) {
	var prompts []string
	screener := Screener{
		Instruction: "只收检索。", BatchSize: 1, Skill: "draft@abc",
		request: func(_ context.Context, _, _, _, prompt string) (string, error) {
			prompts = append(prompts, prompt)
			id := "arxiv:one"
			if strings.Contains(prompt, "arxiv:two") {
				id = "arxiv:two"
			}
			return `{"decisions":[{"id":"` + id + `","relevant":true,"score":0.5,"reason":"检索"}]}`, nil
		},
	}
	got, err := screener.Screen(context.Background(), []papers.Paper{{ID: "arxiv:one", Title: "One"}, {ID: "arxiv:two", Title: "Two"}})
	if err != nil || len(got) != 2 || len(prompts) != 2 || !strings.Contains(prompts[0], "只收检索") || got[0].Skill != "draft@abc" {
		t.Fatalf("got %+v prompts %d err %v", got, len(prompts), err)
	}
}
