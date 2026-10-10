package papers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const legacyQuery = `(cat:cs.IR OR ti:recommender OR ti:recommendation OR abs:recommender OR abs:"collaborative filtering" OR ti:advertising OR ti:advertisement OR ti:advertiser OR abs:advertising OR abs:"click-through" OR abs:"sponsored search" OR abs:"ad auction" OR abs:"ad allocation" OR abs:"ad ranking" OR abs:"ad targeting" OR ti:retrieval OR abs:"information retrieval" OR abs:"document retrieval" OR abs:"dense retrieval" OR abs:"query understanding" OR abs:"query rewriting" OR abs:"learning to rank" OR ti:"web search" OR ti:"search engine" OR ti:"conversational search" OR abs:"search ranking" OR abs:"sequential recommendation")`

func TestDefaultRulesMatchTheLegacyQueryAndOctober9(t *testing.T) {
	if DefaultRules().Query != legacyQuery || DefaultRules().Query != legacyRASSearchQuery {
		t.Fatalf("default query = %s", DefaultRules().Query)
	}
	if DefaultRules().MaxPapers() != maxDailyPapers || DefaultRules().MinTier() != 1 {
		t.Fatalf("max=%d min=%d", DefaultRules().MaxPapers(), DefaultRules().MinTier())
	}
	encoded, err := json.Marshal(DefaultSelection())
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ParseSelection(encoded)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.Query != legacyQuery {
		t.Fatalf("round trip query = %s", roundTrip.Query)
	}
	for _, fixture := range loadRASFixtures(t) {
		direct := Explain(fixture.paper(), DefaultRules())
		again := Explain(fixture.paper(), roundTrip)
		if direct.Tier != again.Tier || direct.Reason != again.Reason || strings.Join(direct.Signals, ",") != strings.Join(again.Signals, ",") {
			t.Fatalf("%s direct=%+v roundTrip=%+v", fixture.ID, direct, again)
		}
	}
}

func TestSelectHonorsConfiguredMaxPapers(t *testing.T) {
	spec := DefaultSelection()
	spec.MaxPapers = 8
	rules, err := Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	papers := make([]Paper, 0, 6)
	for i := 0; i < 6; i++ {
		papers = append(papers, Paper{
			ID:        string(rune('a' + i)),
			Title:     "Web Search Ranking",
			Published: now.Add(-time.Duration(i+1) * time.Hour),
		})
	}
	if got := Select(papers, now, 7, DefaultRules(), nil); len(got) != maxDailyPapers {
		t.Fatalf("default cap = %d, want %d", len(got), maxDailyPapers)
	}
	if got := Select(papers, now, 7, rules, nil); len(got) != 6 {
		t.Fatalf("configured cap = %d, want 6", len(got))
	}
	if Select(papers, now, 7, Rules{}, nil) != nil {
		t.Fatal("inactive rules selected papers")
	}
}

func TestSelectionValidation(t *testing.T) {
	valid := minimalSelection()
	if _, err := Compile(valid); err != nil {
		t.Fatal(err)
	}
	secret := "super-secret-pattern"
	cases := []struct {
		name    string
		mutate  func(*Selection)
		want    string
		rawJSON string
	}{
		{name: "min tier", mutate: func(spec *Selection) { spec.MinTier = 0 }, want: "min_tier"},
		{name: "bad category", mutate: func(spec *Selection) { spec.Query.Categories = []string{"not a category"} }, want: "arXiv"},
		{name: "unknown signal", mutate: func(spec *Selection) {
			spec.Decisions[0].AnySignals = []string{"missing"}
			spec.Decisions[0].MinSignalTier = nil
		}, want: "不存在"},
		{name: "bad regex", mutate: func(spec *Selection) { spec.Signals[0].Pattern = "(" + secret }, want: "正则"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := minimalSelection()
			tc.mutate(&spec)
			_, err := Compile(spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), secret) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	raw := `{"max_papers":5,"min_tier":1,"query":{"categories":["cs.IR"]},"signals":[{"name":"title:hit","pattern":"search","fields":["title"],"tier":2,"` + secret + `":true}],"decisions":[{"tier":2,"reason":"title match","min_signal_tier":2}],"fallback_reason":"no topic signal"}`
	if _, err := ParseSelection([]byte(raw)); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unknown field err = %v", err)
	}
	if _, err := ParseSelection([]byte(raw + raw)); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("trailing value err = %v", err)
	}
}

func TestOrQueryKeepsASingleSearch(t *testing.T) {
	if OrQuery(" (cat:cs.IR) ", "") != " (cat:cs.IR) " || OrQuery("(a)", "(a)") != "(a)" {
		t.Fatal(OrQuery("(a)", "(a)"))
	}
	if got := OrQuery("(cat:cs.IR)", "(ti:search)"); got != "((cat:cs.IR)) OR ((ti:search))" {
		t.Fatal(got)
	}
}

func minimalSelection() Selection {
	tier := 2
	return Selection{
		MaxPapers:      5,
		MinTier:        1,
		Query:          Query{Categories: []string{"cs.IR"}},
		Signals:        []Signal{{Name: "title:hit", Pattern: `\bsearch\b`, Fields: []string{"title"}, Tier: 2}},
		Decisions:      []Decision{{Tier: 2, Reason: "title match", MinSignalTier: &tier}},
		FallbackReason: "no topic signal",
	}
}
