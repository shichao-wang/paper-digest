package eval

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/paper-digest/internal/papers"
)

func TestDatesRejectsInvertedAndLongRanges(t *testing.T) {
	dates, err := Dates("2026-10-01", "2026-10-14")
	if err != nil || len(dates) != 14 || dates[0] != "2026-10-01" || dates[13] != "2026-10-14" {
		t.Fatalf("Dates() = %v, %v", dates, err)
	}
	if _, err := Dates("2026-10-09", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := Dates("2026-10-10", "2026-10-09"); err == nil {
		t.Fatal("end before start was accepted")
	}
	if _, err := Dates("2026-10-01", "2026-10-15"); err == nil {
		t.Fatal("15-day range was accepted")
	}
	if _, err := Dates("2026-02-30", ""); err == nil {
		t.Fatal("invalid date was accepted")
	}
}

func TestBuildComparesCurrentLooseAndSentWithoutUsingLabelsOutsideSelection(t *testing.T) {
	strong := papers.Paper{
		ID: "arxiv:strong", Title: "Sequential Recommendation",
		Published: time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC), Categories: []string{"cs.IR"},
		URL: "https://arxiv.org/abs/strong",
	}
	weak := papers.Paper{
		ID: "arxiv:weak", Title: "Cross community", Abstract: "social",
		Published: time.Date(2026, 10, 7, 17, 30, 0, 0, time.UTC), Categories: []string{"cs.IR"},
		URL: "https://arxiv.org/abs/weak",
	}
	plainNew := papers.Paper{ID: "arxiv:plain-new", Title: "Unrelated physics", Published: time.Date(2026, 10, 7, 17, 0, 0, 0, time.UTC)}
	plainOld := papers.Paper{ID: "arxiv:plain-old", Title: "Older physics", Published: time.Date(2026, 10, 7, 16, 0, 0, 0, time.UTC)}
	outside := papers.Paper{ID: "arxiv:outside", Title: "Web Search Ranking", Published: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	fetched := []papers.Paper{plainOld, weak, strong, plainNew, outside}
	sent := map[string][]SentPaper{
		"2026-10-09": {
			{ID: weak.ID, Title: weak.Title},
			{ID: strong.ID, Title: strong.Title},
			{ID: "arxiv:missing", Title: "saved but not fetched"},
		},
	}
	labels := map[string]string{strong.ID: LabelRelevant, weak.ID: LabelNotRelevant}

	report, err := Build("recommendation-advertising-search", []string{"2026-10-09"}, papers.DefaultRules(), nil, fetched, sent, labels)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Days) != 1 {
		t.Fatalf("days = %d", len(report.Days))
	}
	day := report.Days[0]
	if len(day.Selected) != 1 || day.Selected[0].ID != strong.ID || day.Selected[0].Position != 1 || day.Selected[0].Tier != 2 {
		t.Fatalf("selected = %+v", day.Selected)
	}
	if day.Selected[0].Reason != "title match" || !contains(day.Selected[0].Signals, "title:recommendation") {
		t.Fatalf("explanation = %+v", day.Selected[0])
	}
	if len(day.Loose) != 2 || day.Loose[0].ID != weak.ID || day.Loose[1].ID != strong.ID {
		t.Fatalf("loose = %+v", idsOf(day.Loose))
	}
	if len(day.OnlyCurrent) != 0 || !equalIDs(day.OnlyLoose, []string{weak.ID}) || !equalIDs(day.OnlySent, []string{weak.ID, "arxiv:missing"}) {
		t.Fatalf("only current=%v loose=%v sent=%v", day.OnlyCurrent, day.OnlyLoose, day.OnlySent)
	}
	gotIDs := idsOf(day.Candidates)
	wantIDs := []string{strong.ID, weak.ID, plainNew.ID, plainOld.ID}
	if !equalIDs(gotIDs, wantIDs) {
		t.Fatalf("candidates = %v, want %v", gotIDs, wantIDs)
	}
	if !day.Candidates[0].InDigest || !day.Candidates[1].InDigest || day.Candidates[1].Reason != "cs.IR alone is not a topic match" {
		t.Fatalf("candidate flags = %+v / %+v", day.Candidates[0], day.Candidates[1])
	}
	if day.Score.Selected != 1 || day.Score.Labeled != 1 || day.Score.Relevant != 1 || day.Score.Precision == nil || *day.Score.Precision != 1 {
		t.Fatalf("score = %+v", day.Score)
	}
	text := Format(report)
	if !strings.Contains(text, "不调用模型，不发送飞书，不写入日报或去重记录。") || !strings.Contains(text, "精确率 1.00") || !strings.Contains(text, "标题命中") {
		t.Fatalf("format = %s", text)
	}

	unlabeled, err := Build("recommendation-advertising-search", []string{"2026-10-09"}, papers.DefaultRules(), nil, fetched, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unlabeled.Days[0].Score.Precision != nil || len(unlabeled.Days[0].Sent) != 0 || len(unlabeled.Days[0].OnlySent) != 0 {
		t.Fatalf("unlabeled day = %+v", unlabeled.Days[0].Score)
	}
	if !strings.Contains(Format(unlabeled), "精确率暂无") {
		t.Fatal(Format(unlabeled))
	}
	sunday, err := Build("recommendation-advertising-search", []string{"2026-10-11"}, papers.DefaultRules(), nil, fetched, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sunday.Days[0].HasBatch || len(sunday.Days[0].Selected) != 0 || !strings.Contains(Format(sunday), "没有新的 arXiv 公告") {
		t.Fatalf("sunday = %+v", sunday.Days[0])
	}
}

func TestCollectedPapersStayVisibleAboveThePushCap(t *testing.T) {
	batch, ok, err := papers.DigestBatch("2026-10-09")
	if err != nil || !ok {
		t.Fatalf("batch ok=%v err=%v", ok, err)
	}
	spec := papers.DefaultSelection()
	spec.MaxPapers = 2
	rules, err := papers.Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	fetched := make([]papers.Paper, 4)
	for i := range fetched {
		fetched[i] = papers.Paper{
			ID:        "arxiv:p" + string(rune('0'+i)),
			Title:     "Web Search Ranking",
			Published: batch.Window.Through.Add(-time.Duration(i+1) * time.Hour),
			URL:       "https://arxiv.org/abs/example",
		}
	}
	report, err := Build("recommendation-advertising-search", []string{"2026-10-09"}, rules, nil, fetched, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	day := report.Days[0]
	if len(day.Selected) != 4 || len(day.Pushed) != 2 || day.PushLimit != 2 || day.Score.Selected != 2 {
		t.Fatalf("collected=%d pushed=%d limit=%d score=%+v", len(day.Selected), len(day.Pushed), day.PushLimit, day.Score)
	}
	if !day.Selected[0].Pushed || !day.Selected[1].Pushed || day.Selected[2].Pushed || day.Candidates[2].Pushed {
		t.Fatalf("push flags = %#v", day.Selected)
	}
	if !strings.Contains(Format(report), "收集 4 篇，将推送 2 篇（上限 2）") || !strings.Contains(Format(report), "超出推送上限") {
		t.Fatal(Format(report))
	}
}

func TestFixturesSkipLabelsWithoutPaperText(t *testing.T) {
	published := time.Date(2026, 10, 7, 17, 38, 35, 0, time.UTC)
	paper := papers.Paper{ID: "arxiv:strong", Title: "Sequential Recommendation", Abstract: "session logs", Published: published, Categories: []string{"cs.IR"}}
	encoded, err := json.Marshal(paper)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := string(encoded)
	fixtures, skipped := Fixtures([]StoredLabel{
		{PaperID: "arxiv:weak", Label: LabelNotRelevant, Snapshot: `{"ID":"arxiv:weak","Title":"Cross community","Abstract":"social","categories":["cs.IR"]}`},
		{PaperID: "arxiv:strong", Label: LabelRelevant, Snapshot: snapshot},
		{PaperID: "arxiv:empty", Label: LabelRelevant, Snapshot: ""},
		{PaperID: "arxiv:bad", Label: LabelRelevant, Snapshot: "{"},
	})
	if len(fixtures) != 2 || fixtures[0].ID != "arxiv:strong" || fixtures[0].Role != "positive" || fixtures[0].Title != paper.Title || !fixtures[0].Published.Equal(published) {
		t.Fatalf("fixtures = %+v", fixtures)
	}
	if fixtures[1].Role != "negative" || fixtures[1].ID != "arxiv:weak" || len(fixtures[1].Categories) != 1 {
		t.Fatalf("second fixture = %+v", fixtures[1])
	}
	if !equalIDs(skipped, []string{"arxiv:bad", "arxiv:empty"}) {
		t.Fatalf("skipped = %v", skipped)
	}
}

func idsOf(list []Candidate) []string {
	out := make([]string, len(list))
	for i, item := range list {
		out[i] = item.ID
	}
	return out
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
