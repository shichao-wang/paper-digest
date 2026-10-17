package digest

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shichao-wang/research-vault/internal/papers"
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
	var mu sync.Mutex
	var prompts []string
	screener := Screener{
		Instruction: "只收检索。", BatchSize: 1, Skill: "draft@abc",
		request: func(_ context.Context, _, _, _, prompt string) (string, error) {
			mu.Lock()
			prompts = append(prompts, prompt)
			mu.Unlock()
			id := "arxiv:one"
			if strings.Contains(prompt, "arxiv:two") {
				id = "arxiv:two"
			}
			return `{"decisions":[{"id":"` + id + `","relevant":true,"score":0.5,"reason":"检索"}]}`, nil
		},
	}
	got, err := screener.Screen(context.Background(), []papers.Paper{{ID: "arxiv:one", Title: "One"}, {ID: "arxiv:two", Title: "Two"}})
	if err != nil || len(got) != 2 || got[0].ID != "arxiv:one" || got[1].ID != "arxiv:two" || len(prompts) != 2 || !strings.Contains(prompts[0], "只收检索") || !strings.Contains(prompts[1], "只收检索") || got[0].Skill != "draft@abc" {
		t.Fatalf("got %+v prompts %d err %v", got, len(prompts), err)
	}
}

func TestScreenRunsBatchesTogetherAndKeepsOrder(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	screener := Screener{
		BatchSize: 1, Concurrency: 2, Timeout: 5 * time.Second, MaxTokens: DefaultScreenMaxTokens,
		request: func(ctx context.Context, _, _, _, prompt string) (string, error) {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			id := "arxiv:one"
			if strings.Contains(prompt, "arxiv:two") {
				id = "arxiv:two"
			}
			return `{"decisions":[{"id":"` + id + `","relevant":true,"score":0.5,"reason":"检索"}]}`, nil
		},
	}
	errCh := make(chan error, 1)
	var got []Decision
	go func() {
		var err error
		got, err = screener.Screen(context.Background(), []papers.Paper{{ID: "arxiv:one", Title: "One"}, {ID: "arxiv:two", Title: "Two"}})
		errCh <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("batches did not overlap")
		}
	}
	close(release)
	if err := <-errCh; err != nil || len(got) != 2 || got[0].ID != "arxiv:one" || got[1].ID != "arxiv:two" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestScreenFailureDoesNotKeepAPartialBatch(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	screener := Screener{
		BatchSize: 1, Concurrency: 2, Attempts: 3, Timeout: time.Second,
		request: func(ctx context.Context, _, _, _, prompt string) (string, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			if strings.Contains(prompt, "arxiv:bad") {
				return "not json", nil
			}
			<-ctx.Done()
			return "", ctx.Err()
		},
	}
	_, err := screener.Screen(context.Background(), []papers.Paper{{ID: "arxiv:bad", Title: "Bad"}, {ID: "arxiv:slow", Title: "Slow"}})
	mu.Lock()
	defer mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "3 次") || calls > 4 {
		t.Fatalf("err = %v, calls = %d", err, calls)
	}
}

func TestScreenRefusesWorkThatCannotFinishBeforeTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	list := make([]papers.Paper, 30)
	for i := range list {
		list[i] = papers.Paper{ID: "arxiv:p" + string(rune('a'+i%26)) + string(rune('0'+i/26))}
	}
	screener := Screener{
		Timeout: time.Minute, BatchSize: 8, Concurrency: 1, MaxTokens: 16384,
		request: func(context.Context, string, string, string, string) (string, error) {
			calls++
			return "", nil
		},
	}
	_, err := screener.Screen(ctx, list)
	if err == nil || calls != 0 || !strings.Contains(err.Error(), "max_tokens=16384") || !strings.Contains(err.Error(), "30 篇") {
		t.Fatalf("err = %v, calls = %d", err, calls)
	}
}

func TestScreenRefusesWhenRetriesCannotFitEvenIfOnePassCan(t *testing.T) {
	// Four batches at concurrency 2 are two waves. One pass plus a spare is 3s.
	// Three attempts on each batch is 6s, so 4s is not enough to start.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	calls := 0
	screener := Screener{
		Timeout: time.Second, BatchSize: 1, Concurrency: 2,
		request: func(context.Context, string, string, string, string) (string, error) {
			calls++
			return "", nil
		},
	}
	_, err := screener.Screen(ctx, []papers.Paper{{ID: "arxiv:a"}, {ID: "arxiv:b"}, {ID: "arxiv:c"}, {ID: "arxiv:d"}})
	if err == nil || calls != 0 || !strings.Contains(err.Error(), "每批最多 3 次") {
		t.Fatalf("err = %v, calls = %d", err, calls)
	}
}
