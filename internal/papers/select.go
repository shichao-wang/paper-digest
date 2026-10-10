package papers

import (
	"regexp"
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

var (
	titleRecommendation = regexp.MustCompile(`\brecommender(s)?\b|\brecommendation(s)?\b|\bcollaborative filtering\b`)
	// Abstract "recommendation" is too weak: policy notes and postmortems use it
	// for ordinary suggestions. Require the recommender-systems sense.
	abstractRecommendation = regexp.MustCompile(`\brecommender(s)?\b|\bcollaborative filtering\b|\b(sequential|session-based|session based|personalized|generative|conversational) recommendations?\b|\brecommendation (system|systems|model|models|algorithm|algorithms|task|tasks|framework|frameworks|method|methods|engine|engines|list|lists|quality|accuracy|diversity|performance)\b`)
	advertisingSignal      = regexp.MustCompile(`\badvertising\b|\badvertisement(s)?\b|\badvertiser(s)?\b|\bclick[- ]through\b|\bsponsored (search|ad|ads|advertisement|advertisements)\b|\bonline ads?\b|\bad (click|auction|auctions|allocation|ranking|targeting|placement)s?\b`)
	searchSignal           = regexp.MustCompile(`\binformation retrieval\b|\b(document|dense|sparse|passage|neural|generative|multimodal|visual|semantic|cross-modal|cross modal) retrieval\b|\b(web|conversational|sponsored|semantic|neural|personalized|product|e-commerce|ecommerce) search\b|\bsearch (engine|engines|ranking|result|results|query|queries|system|systems)\b|\bquery (rewriting|understanding|expansion|processing|reformulation)\b|\blearning to rank\b`)
	titleRetrieval         = regexp.MustCompile(`\bretrieval\b`)
	irTechnique            = regexp.MustCompile(`\bretrieval\b|\breranking\b|\branking\b|\bbm25\b|\bndcg\b|\bcollaborative filtering\b|\brecommender(s)?\b|\blearning to rank\b|\bquery (rewriting|understanding|expansion|processing)\b|\bdual[- ]encoders?\b|\btwo[- ]towers?\b|\bnegative sampling\b|\bapproximate nearest\b|\binverted index\b`)
	pirTerm                = regexp.MustCompile(`\bpir\b`)
	bareRecommendation     = regexp.MustCompile(`\brecommendations?\b`)
	bareSearch             = regexp.MustCompile(`\bsearch\b`)
	bareAds                = regexp.MustCompile(`\bads\b`)
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
		if topicTier(paper) == tierOut {
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
		left, right := topicTier(candidates[i]), topicTier(candidates[j])
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
// and a short reason. Tier 0 is not selected.
type Explanation struct {
	Tier    int      `json:"tier"`
	Signals []string `json:"signals"`
	Reason  string   `json:"reason"`
}

func Explain(paper Paper) Explanation {
	rawTitle := strings.ToLower(paper.Title)
	rawAbstract := strings.ToLower(paper.Abstract)
	context := rawTitle + " " + rawAbstract
	var signals []string
	if strings.Contains(context, "private information retrieval") {
		signals = append(signals, "excluded:private-information-retrieval")
	}
	if strings.Contains(context, "dna") && (strings.Contains(context, "data storage") || pirTerm.MatchString(context)) {
		signals = append(signals, "excluded:dna-storage")
	}
	title := maskOffTopicRetrieval(rawTitle, context)
	abstract := maskOffTopicRetrieval(rawAbstract, context)
	if titleRecommendation.MatchString(title) {
		signals = append(signals, "title:recommendation")
	}
	if advertisingSignal.MatchString(title) {
		signals = append(signals, "title:advertising")
	}
	if searchSignal.MatchString(title) {
		signals = append(signals, "title:search")
	}
	if titleRetrieval.MatchString(title) {
		signals = append(signals, "title:retrieval")
	}
	if abstractRecommendation.MatchString(abstract) {
		signals = append(signals, "abstract:recommendation")
	}
	if advertisingSignal.MatchString(abstract) {
		signals = append(signals, "abstract:advertising")
	}
	if searchSignal.MatchString(abstract) {
		signals = append(signals, "abstract:search")
	}
	if hasCategory(paper, "cs.IR") {
		signals = append(signals, "category:cs.IR")
	}
	body := title + " " + abstract
	if irTechnique.MatchString(body) {
		signals = append(signals, "technique:retrieval")
	}
	if softmaxSampling(body) {
		signals = append(signals, "technique:softmax-sampling")
	}
	if bareRecommendation.MatchString(context) && !titleRecommendation.MatchString(title) && !abstractRecommendation.MatchString(abstract) {
		signals = append(signals, "ignored:recommendation")
	}
	if bareSearch.MatchString(context) && !searchSignal.MatchString(title) && !searchSignal.MatchString(abstract) && !titleRetrieval.MatchString(title) {
		signals = append(signals, "ignored:search")
	}
	if bareAds.MatchString(context) && !advertisingSignal.MatchString(title) && !advertisingSignal.MatchString(abstract) {
		signals = append(signals, "ignored:ads")
	}
	if signals == nil {
		signals = []string{}
	}

	switch {
	case titleSignal(title):
		return Explanation{Tier: tierStrong, Signals: signals, Reason: "title match"}
	case hasCategory(paper, "cs.IR") && techniqueSignal(body):
		return Explanation{Tier: tierStrong, Signals: signals, Reason: "cs.IR with a technique signal"}
	case abstractSignal(abstract):
		return Explanation{Tier: tierAbstract, Signals: signals, Reason: "abstract phrase only"}
	case signal(signals, "excluded:private-information-retrieval") || signal(signals, "excluded:dna-storage"):
		return Explanation{Tier: tierOut, Signals: signals, Reason: "private information retrieval is not search"}
	case signal(signals, "ignored:recommendation"):
		return Explanation{Tier: tierOut, Signals: signals, Reason: "recommendation appears only as an ordinary word"}
	case hasCategory(paper, "cs.IR"):
		return Explanation{Tier: tierOut, Signals: signals, Reason: "cs.IR alone is not a topic match"}
	case signal(signals, "ignored:search") || signal(signals, "ignored:ads"):
		return Explanation{Tier: tierOut, Signals: signals, Reason: "search or ads appears only as an ordinary word"}
	default:
		return Explanation{Tier: tierOut, Signals: signals, Reason: "no topic signal"}
	}
}

func signal(signals []string, want string) bool {
	for _, item := range signals {
		if item == want {
			return true
		}
	}
	return false
}

func topicTier(paper Paper) int {
	return Explain(paper).Tier
}

func maskOffTopicRetrieval(text, context string) string {
	text = strings.ReplaceAll(text, "private information retrieval", " ")
	// DNA-storage papers describe reading a strand as "data retrieval". That is
	// not document or web retrieval, even when the paper is also tagged cs.IR.
	if strings.Contains(context, "dna") && (strings.Contains(context, "data storage") || pirTerm.MatchString(context)) {
		text = strings.ReplaceAll(text, "data retrieval", " ")
	}
	return text
}

func titleSignal(title string) bool {
	return titleRecommendation.MatchString(title) || advertisingSignal.MatchString(title) || searchSignal.MatchString(title) || titleRetrieval.MatchString(title)
}

func abstractSignal(abstract string) bool {
	return abstractRecommendation.MatchString(abstract) || advertisingSignal.MatchString(abstract) || searchSignal.MatchString(abstract)
}

func techniqueSignal(text string) bool {
	return irTechnique.MatchString(text) || softmaxSampling(text)
}

func softmaxSampling(text string) bool {
	const window = 80
	needle := "softmax"
	for start := 0; ; {
		rel := strings.Index(text[start:], needle)
		if rel < 0 {
			return false
		}
		at := start + rel
		from := at - window
		if from < 0 {
			from = 0
		}
		to := at + len(needle) + window
		if to > len(text) {
			to = len(text)
		}
		if strings.Contains(text[from:to], "sampl") {
			return true
		}
		start = at + len(needle)
	}
}

func hasCategory(paper Paper, category string) bool {
	for _, candidate := range paper.Categories {
		if strings.EqualFold(candidate, category) {
			return true
		}
	}
	return false
}
