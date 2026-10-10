package papers

import (
	"sort"
	"strings"
	"time"
)

const maxDailyPapers = 5

// topicTier is how strongly a paper belongs in the recommendation, computational
// advertising, or search-and-retrieval digest.
//
// Tier 2 is a title hit, or cs.IR plus a retrieval technique. Tier 1 is a strong
// phrase that appears only in the abstract. Tier 0 is left out. cs.IR by itself
// is not enough, because social-computing papers use that category. A lone
// "recommendation", "search", or "ads" in the abstract is ordinary prose, not a
// topic. Private information retrieval, including DNA-storage PIR, is a
// different field and does not count as search.
const (
	tierOut      = 0
	tierAbstract = 1
	tierStrong   = 2
)

func Select(all []Paper, now time.Time, lookbackDays int, rules Rules, seen func(string) bool) []Paper {
	if !rules.active || lookbackDays <= 0 || rules.maxPapers <= 0 {
		return nil
	}
	max := rules.maxPapers
	cutoff := now.Add(-time.Duration(lookbackDays) * 24 * time.Hour)

	candidateByID := make(map[string]Paper, len(all))
	seenState := make(map[string]bool, len(all))
	checkedSeen := make(map[string]bool, len(all))
	for _, paper := range all {
		if paper.ID == "" || paper.Published.IsZero() || paper.Published.Before(cutoff) || paper.Published.After(now) {
			continue
		}
		if rules.explain(paper).Tier < rules.minTier {
			continue
		}
		if seen != nil {
			if !checkedSeen[paper.ID] {
				seenState[paper.ID] = seen(paper.ID)
				checkedSeen[paper.ID] = true
			}
			if seenState[paper.ID] {
				continue
			}
		}
		if current, exists := candidateByID[paper.ID]; !exists || paper.Published.After(current.Published) {
			candidateByID[paper.ID] = paper
		}
	}

	candidates := make([]Paper, 0, len(candidateByID))
	for _, paper := range candidateByID {
		candidates = append(candidates, paper)
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := rules.explain(candidates[i]).Tier, rules.explain(candidates[j]).Tier
		if left != right {
			return left > right
		}
		if !candidates[i].Published.Equal(candidates[j].Published) {
			return candidates[i].Published.After(candidates[j].Published)
		}
		return candidates[i].ID < candidates[j].ID
	})
	if len(candidates) > max {
		candidates = candidates[:max]
	}
	return candidates
}

// Explanation is the topic decision for one paper: tier, the signals that fired,
// and a short reason. Tier 0 is not selected when it is below the rules' minimum.
type Explanation struct {
	Tier    int      `json:"tier"`
	Signals []string `json:"signals"`
	Reason  string   `json:"reason"`
}

func Explain(paper Paper, rules Rules) Explanation {
	return rules.explain(paper)
}

func topicTier(paper Paper) int {
	return Explain(paper, DefaultRules()).Tier
}

func hasCategory(paper Paper, category string) bool {
	for _, candidate := range paper.Categories {
		if strings.EqualFold(candidate, category) {
			return true
		}
	}
	return false
}
