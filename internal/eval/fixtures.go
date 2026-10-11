package eval

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/papers"
)

// StoredLabel is one persisted judgment. Snapshot is the JSON encoding of papers.Paper.
type StoredLabel struct {
	PaperID  string
	Label    string
	Snapshot string
}

// Fixture is a regression sample. Role is positive or negative, matching the
// selection testdata files.
type Fixture struct {
	ID         string    `json:"id"`
	Role       string    `json:"role"`
	Title      string    `json:"title"`
	Abstract   string    `json:"abstract"`
	Categories []string  `json:"categories"`
	Published  time.Time `json:"published"`
}

// Fixtures turns stored judgments into regression samples. Labels without a
// usable paper snapshot are listed in skipped and omitted from the result.
func Fixtures(labels []StoredLabel) ([]Fixture, []string) {
	ordered := append([]StoredLabel(nil), labels...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PaperID < ordered[j].PaperID })
	fixtures := []Fixture{}
	var skipped []string
	for _, label := range ordered {
		role := ""
		switch label.Label {
		case LabelRelevant:
			role = "positive"
		case LabelNotRelevant:
			role = "negative"
		default:
			skipped = append(skipped, label.PaperID)
			continue
		}
		paper, ok := snapshotPaper(label)
		if !ok {
			skipped = append(skipped, label.PaperID)
			continue
		}
		categories := paper.Categories
		if categories == nil {
			categories = []string{}
		}
		fixtures = append(fixtures, Fixture{
			ID:         paper.ID,
			Role:       role,
			Title:      paper.Title,
			Abstract:   paper.Abstract,
			Categories: categories,
			Published:  paper.Published,
		})
	}
	return fixtures, skipped
}

func snapshotPaper(label StoredLabel) (papers.Paper, bool) {
	if strings.TrimSpace(label.Snapshot) == "" {
		return papers.Paper{}, false
	}
	var paper papers.Paper
	if err := json.Unmarshal([]byte(label.Snapshot), &paper); err != nil {
		return papers.Paper{}, false
	}
	if paper.ID == "" {
		paper.ID = label.PaperID
	}
	if strings.TrimSpace(paper.Title) == "" && strings.TrimSpace(paper.Abstract) == "" {
		return papers.Paper{}, false
	}
	return paper, true
}
