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
	cutoff, err := Cutoff("2026-10-09")
	if err != nil {
		t.Fatal(err)
	}
	strong := papers.Paper{
		ID: "arxiv:strong", Title: "Sequential Recommendation",
		Published: cutoff.Add(-48 * time.Hour), Categories: []string{"cs.IR"},
		URL: "https://arxiv.org/abs/strong",
	}
	weak := papers.Paper{
		ID: "arxiv:weak", Title: "Cross community", Abstract: "social",
		Published: cutoff.Add(-time.Hour), Categories: []string{"cs.IR"},
		URL: "https://arxiv.org/abs/weak",
	}
	plainNew := papers.Paper{ID: "arxiv:plain-new", Title: "Unrelated physics", Published: cutoff.Add(-30 * time.Minute)}
	plainOld := papers.Paper{ID: "arxiv:plain-old", Title: "Older physics", Published: cutoff.Add(-72 * time.Hour)}
	outside := papers.Paper{ID: "arxiv:outside", Title: "Web Search Ranking", Published: cutoff.Add(-8 * 24 * time.Hour)}
	fetched := []papers.Paper{plainOld, weak, strong, plainNew, outside}
	sent := map[string][]SentPaper{
		"2026-10-09": {
			{ID: weak.ID, Title: weak.Title},
			{ID: strong.ID, Title: strong.Title},
			{ID: "arxiv:missing", Title: "saved but not fetched"},
		},
	}
	labels := map[string]string{strong.ID: LabelRelevant, weak.ID: LabelNotRelevant}

	report, err := Build("recommendation-advertising-search", []string{"2026-10-09"}, 7, papers.DefaultRules(), nil, fetched, sent, labels)
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

	unlabeled, err := Build("recommendation-advertising-search", []string{"2026-10-09"}, 7, papers.DefaultRules(), nil, fetched, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unlabeled.Days[0].Score.Precision != nil || len(unlabeled.Days[0].Sent) != 0 || len(unlabeled.Days[0].OnlySent) != 0 {
		t.Fatalf("unlabeled day = %+v", unlabeled.Days[0].Score)
	}
	if !strings.Contains(Format(unlabeled), "精确率暂无") {
		t.Fatal(Format(unlabeled))
	}
	if _, err := Build("topic", []string{"2026-10-09"}, 0, papers.DefaultRules(), nil, nil, nil, nil); err == nil {
		t.Fatal("lookback 0 was accepted")
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
