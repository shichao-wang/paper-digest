package papers

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"
)

type rasFixture struct {
	ID         string    `json:"id"`
	Role       string    `json:"role"`
	Set        string    `json:"set"`
	Title      string    `json:"title"`
	Abstract   string    `json:"abstract"`
	Published  time.Time `json:"published"`
	Categories []string  `json:"categories"`
}

func (f rasFixture) paper() Paper {
	return Paper{
		ID:         f.ID,
		Title:      f.Title,
		Abstract:   f.Abstract,
		Published:  f.Published,
		Categories: f.Categories,
	}
}

func loadRASFixtures(t *testing.T) []rasFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/2026-10-09-ras.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []rasFixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("empty RAS fixtures")
	}
	return fixtures
}

func TestOctober9PapersMatchTopic(t *testing.T) {
	for _, fixture := range loadRASFixtures(t) {
		t.Run(fixture.ID, func(t *testing.T) {
			tier := topicTier(fixture.paper())
			switch fixture.Role {
			case "positive":
				if tier != tierStrong {
					t.Fatalf("tier = %d, want strong match for %q", tier, fixture.Title)
				}
			case "negative":
				if tier != tierOut {
					t.Fatalf("tier = %d, want rejected for %q", tier, fixture.Title)
				}
			default:
				t.Fatalf("unknown role %q", fixture.Role)
			}
		})
	}
}

func TestDNAStoragePIRStaysOutEvenWithCSIR(t *testing.T) {
	var dna rasFixture
	for _, fixture := range loadRASFixtures(t) {
		if fixture.ID == "arxiv:2610.10211" {
			dna = fixture
		}
	}
	if dna.ID == "" {
		t.Fatal("missing DNA-storage PIR fixture")
	}
	paper := dna.paper()
	paper.Categories = append(append([]string(nil), paper.Categories...), "cs.IR")
	if tier := topicTier(paper); tier != tierOut {
		t.Fatalf("cs.IR DNA-storage PIR tier = %d, want rejected", tier)
	}
}

func TestOctober9ExclusionReasons(t *testing.T) {
	want := map[string]string{
		"arxiv:2610.10483": "cs.IR with a technique signal",
		"arxiv:2610.10441": "cs.IR alone is not a topic match",
		"arxiv:2610.10256": "recommendation appears only as an ordinary word",
		"arxiv:2610.10224": "recommendation appears only as an ordinary word",
		"arxiv:2610.10211": "private information retrieval is not search",
	}
	for _, fixture := range loadRASFixtures(t) {
		if fixture.Set != "2026-10-09" {
			continue
		}
		got := Explain(fixture.paper(), DefaultRules())
		if got.Reason != want[fixture.ID] || got.Signals == nil {
			t.Fatalf("%s reason=%q signals=%v", fixture.ID, got.Reason, got.Signals)
		}
	}
}

func TestLooseBaselineKeepsOctober9WeakDay(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	var day []Paper
	for _, fixture := range loadRASFixtures(t) {
		if fixture.Set == "2026-10-09" {
			day = append(day, fixture.paper())
		}
	}
	got := SelectLoose(day, now, 7, 5, nil)
	want := []string{"arxiv:2610.10483", "arxiv:2610.10441", "arxiv:2610.10256", "arxiv:2610.10224", "arxiv:2610.10211"}
	if len(got) != len(want) {
		t.Fatalf("SelectLoose() = %d papers, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("loose position %d = %s, want %s", i, got[i].ID, id)
		}
	}
	skipped := SelectLoose(day, now, 7, 5, func(id string) bool { return id == "arxiv:2610.10483" })
	if len(skipped) != 4 || skipped[0].ID != "arxiv:2610.10441" {
		t.Fatalf("seen paper was not dropped: %+v", skipped)
	}
}

func TestOctober9WeakDayDoesNotPad(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	var day []Paper
	for _, fixture := range loadRASFixtures(t) {
		if fixture.Set == "2026-10-09" {
			day = append(day, fixture.paper())
		}
	}
	if len(day) != 5 {
		t.Fatalf("weak-day fixtures = %d, want the five 2026-10-09 digest papers", len(day))
	}
	byDate := append([]Paper(nil), day...)
	sort.Slice(byDate, func(i, j int) bool { return byDate[i].Published.After(byDate[j].Published) })
	if byDate[0].ID != "arxiv:2610.10483" {
		t.Fatalf("newest weak-day paper = %s, want the on-topic softmax paper", byDate[0].ID)
	}

	got := Select(day, now, 7, DefaultRules(), nil)
	if len(got) != 1 || got[0].ID != "arxiv:2610.10483" {
		ids := make([]string, len(got))
		for i, paper := range got {
			ids[i] = paper.ID
		}
		t.Fatalf("Select() = %v, want only arxiv:2610.10483", ids)
	}
}

func TestNewerOffTopicPapersDoNotCrowdOutOlderMatches(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	fixtures := loadRASFixtures(t)
	all := make([]Paper, 0, len(fixtures))
	positive := make([]Paper, 0, len(fixtures))
	negative := map[string]bool{}
	for _, fixture := range fixtures {
		paper := fixture.paper()
		all = append(all, paper)
		switch fixture.Role {
		case "positive":
			positive = append(positive, paper)
		case "negative":
			negative[paper.ID] = true
		}
	}
	byDate := append([]Paper(nil), all...)
	sort.Slice(byDate, func(i, j int) bool {
		if byDate[i].Published.Equal(byDate[j].Published) {
			return byDate[i].ID < byDate[j].ID
		}
		return byDate[i].Published.After(byDate[j].Published)
	})
	dateTopIncludesNegative := false
	for _, paper := range byDate[:maxDailyPapers] {
		if negative[paper.ID] {
			dateTopIncludesNegative = true
		}
	}
	if !dateTopIncludesNegative {
		t.Fatal("fixture no longer puts an off-topic paper in the newest five")
	}

	sort.Slice(positive, func(i, j int) bool {
		if positive[i].Published.Equal(positive[j].Published) {
			return positive[i].ID < positive[j].ID
		}
		return positive[i].Published.After(positive[j].Published)
	})
	want := positive[:maxDailyPapers]
	got := Select(all, now, 7, DefaultRules(), nil)
	if len(got) != len(want) {
		t.Fatalf("Select() returned %d papers, want %d", len(got), len(want))
	}
	for i, paper := range got {
		if negative[paper.ID] {
			t.Fatalf("selected off-topic paper %s", paper.ID)
		}
		if paper.ID != want[i].ID {
			t.Fatalf("position %d = %s, want %s", i, paper.ID, want[i].ID)
		}
	}
}

func TestSelectDropsIncidentalWordsAndRanksRelevance(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	got := Select([]Paper{
		{ID: "new-abstract", Published: now.Add(-time.Hour), Title: "Notes on libraries", Abstract: "We evaluate a document retrieval model on two corpora."},
		{ID: "old-title", Published: now.Add(-48 * time.Hour), Title: "Web Search Ranking for Product Queries"},
		{ID: "pulsar", Published: now.Add(-time.Minute), Title: "A Coherent Harmonic Summing Pulsar Search Code", Abstract: "We search the parameter space. The ads in the appendix list file sizes.", Categories: []string{"astro-ph.IM"}},
		{ID: "suggestion", Published: now.Add(-2 * time.Minute), Title: "A methods note", Abstract: "We offer different recommendations on the experimental setup and recommend further search."},
		{ID: "category-only", Published: now.Add(-3 * time.Minute), Title: "Bridging online communities", Abstract: "A dual-pane social interface for civic discourse.", Categories: []string{"cs.HC", "cs.IR"}},
		{ID: "abstract-recsys", Published: now.Add(-3 * time.Hour), Title: "Calibrated pruning", Abstract: "The method is an application to sequential recommendation on session logs."},
	}, now, 7, DefaultRules(), nil)
	want := []string{"old-title", "new-abstract", "abstract-recsys"}
	if len(got) != len(want) {
		t.Fatalf("Select() returned %d papers, want %d: %#v", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("position %d = %s, want %s", i, got[i].ID, id)
		}
	}
}
