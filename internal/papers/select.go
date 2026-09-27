package papers

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxDailyPapers = 5

var (
	searchWordPattern = regexp.MustCompile(`\bsearch\b`)
	adsWordPattern    = regexp.MustCompile(`\bads\b`)
)

func Select(all []Paper, now time.Time, lookbackDays, max int, seen func(string) bool) []Paper {
	if lookbackDays <= 0 || max <= 0 {
		return nil
	}
	if max > maxDailyPapers {
		max = maxDailyPapers
	}
	cutoff := now.Add(-time.Duration(lookbackDays) * 24 * time.Hour)

	candidateByID := make(map[string]Paper, len(all))
	seenState := make(map[string]bool, len(all))
	checkedSeen := make(map[string]bool, len(all))
	for _, paper := range all {
		if paper.ID == "" || paper.Published.IsZero() || paper.Published.Before(cutoff) || paper.Published.After(now) {
			continue
		}
		if !isRASJointTopic(paper) {
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
		if candidates[i].Published.Equal(candidates[j].Published) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].Published.After(candidates[j].Published)
	})
	if len(candidates) > max {
		candidates = candidates[:max]
	}
	return candidates
}

func isRASJointTopic(paper Paper) bool {
	var search bool
	for _, category := range paper.categories {
		if strings.EqualFold(category, "cs.IR") {
			search = true
		}
	}

	text := strings.ToLower(paper.Title + " " + paper.Abstract)
	recommendation := containsAny(text, "recommendation", "recommender", "collaborative filtering", "personalized recommendation")
	advertising := containsAny(text, "advertising", "advertisement", "advertiser", "online ad", "ad click", "ad auction", "ad allocation", "ad placement", "ad targeting", "ad ranking", "click-through", "click through", "sponsored ad") || adsWordPattern.MatchString(text)
	search = search || containsAny(text, "information retrieval", "retrieval system", "document retrieval", "query processing", "query understanding", "search engine", "web search", "search ranking", "search query", "search result") || searchWordPattern.MatchString(text)

	return recommendation || advertising || search
}

func containsAny(text string, terms ...string) bool {
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}
