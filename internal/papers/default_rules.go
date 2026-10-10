package papers

import "sync"

// DefaultSelection is the recommendation, advertising, and search filter used
// when that topic's config omits a selection block.
func DefaultSelection() Selection {
	atLeast := func(tier int) *int { return &tier }
	return Selection{
		MaxPapers: 5,
		MinTier:   1,
		Query: Query{
			Categories: []string{"cs.IR"},
			Terms: []QueryTerm{
				{Field: "title", Text: "recommender"},
				{Field: "title", Text: "recommendation"},
				{Field: "abstract", Text: "recommender"},
				{Field: "abstract", Text: "collaborative filtering"},
				{Field: "title", Text: "advertising"},
				{Field: "title", Text: "advertisement"},
				{Field: "title", Text: "advertiser"},
				{Field: "abstract", Text: "advertising"},
				{Field: "abstract", Text: "click-through"},
				{Field: "abstract", Text: "sponsored search"},
				{Field: "abstract", Text: "ad auction"},
				{Field: "abstract", Text: "ad allocation"},
				{Field: "abstract", Text: "ad ranking"},
				{Field: "abstract", Text: "ad targeting"},
				{Field: "title", Text: "retrieval"},
				{Field: "abstract", Text: "information retrieval"},
				{Field: "abstract", Text: "document retrieval"},
				{Field: "abstract", Text: "dense retrieval"},
				{Field: "abstract", Text: "query understanding"},
				{Field: "abstract", Text: "query rewriting"},
				{Field: "abstract", Text: "learning to rank"},
				{Field: "title", Text: "web search"},
				{Field: "title", Text: "search engine"},
				{Field: "title", Text: "conversational search"},
				{Field: "abstract", Text: "search ranking"},
				{Field: "abstract", Text: "sequential recommendation"},
			},
		},
		Masks: []Mask{
			{Name: "excluded:private-information-retrieval", Remove: "private information retrieval"},
			{
				Name:   "excluded:dna-storage",
				Remove: "data retrieval",
				When: &MaskWhen{All: []MaskWhen{
					{Contains: "dna"},
					{Any: []MaskWhen{{Contains: "data storage"}, {Pattern: `\bpir\b`}}},
				}},
			},
		},
		Signals: []Signal{
			{Name: "title:recommendation", Pattern: `\brecommender(s)?\b|\brecommendation(s)?\b|\bcollaborative filtering\b`, Fields: []string{"title"}, Tier: 2},
			{Name: "title:advertising", Pattern: `\badvertising\b|\badvertisement(s)?\b|\badvertiser(s)?\b|\bclick[- ]through\b|\bsponsored (search|ad|ads|advertisement|advertisements)\b|\bonline ads?\b|\bad (click|auction|auctions|allocation|ranking|targeting|placement)s?\b`, Fields: []string{"title"}, Tier: 2},
			{Name: "title:search", Pattern: `\binformation retrieval\b|\b(document|dense|sparse|passage|neural|generative|multimodal|visual|semantic|cross-modal|cross modal) retrieval\b|\b(web|conversational|sponsored|semantic|neural|personalized|product|e-commerce|ecommerce) search\b|\bsearch (engine|engines|ranking|result|results|query|queries|system|systems)\b|\bquery (rewriting|understanding|expansion|processing|reformulation)\b|\blearning to rank\b`, Fields: []string{"title"}, Tier: 2},
			{Name: "title:retrieval", Pattern: `\bretrieval\b`, Fields: []string{"title"}, Tier: 2},
			{Name: "abstract:recommendation", Pattern: `\brecommender(s)?\b|\bcollaborative filtering\b|\b(sequential|session-based|session based|personalized|generative|conversational) recommendations?\b|\brecommendation (system|systems|model|models|algorithm|algorithms|task|tasks|framework|frameworks|method|methods|engine|engines|list|lists|quality|accuracy|diversity|performance)\b`, Fields: []string{"abstract"}, Tier: 1},
			{Name: "abstract:advertising", Pattern: `\badvertising\b|\badvertisement(s)?\b|\badvertiser(s)?\b|\bclick[- ]through\b|\bsponsored (search|ad|ads|advertisement|advertisements)\b|\bonline ads?\b|\bad (click|auction|auctions|allocation|ranking|targeting|placement)s?\b`, Fields: []string{"abstract"}, Tier: 1},
			{Name: "abstract:search", Pattern: `\binformation retrieval\b|\b(document|dense|sparse|passage|neural|generative|multimodal|visual|semantic|cross-modal|cross modal) retrieval\b|\b(web|conversational|sponsored|semantic|neural|personalized|product|e-commerce|ecommerce) search\b|\bsearch (engine|engines|ranking|result|results|query|queries|system|systems)\b|\bquery (rewriting|understanding|expansion|processing|reformulation)\b|\blearning to rank\b`, Fields: []string{"abstract"}, Tier: 1},
			{Name: "category:cs.IR", Categories: []string{"cs.IR"}},
			{Name: "technique:retrieval", Pattern: `\bretrieval\b|\breranking\b|\branking\b|\bbm25\b|\bndcg\b|\bcollaborative filtering\b|\brecommender(s)?\b|\blearning to rank\b|\bquery (rewriting|understanding|expansion|processing)\b|\bdual[- ]encoders?\b|\btwo[- ]towers?\b|\bnegative sampling\b|\bapproximate nearest\b|\binverted index\b`, Fields: []string{"title", "abstract"}},
			{Name: "technique:softmax-sampling", Fields: []string{"title", "abstract"}, Near: &Near{Term: "softmax", Within: 80, Contains: "sampl"}},
			{Name: "ignored:recommendation", Pattern: `\brecommendations?\b`, Fields: []string{"title", "abstract"}, Raw: true, Unless: []string{"title:recommendation", "abstract:recommendation"}},
			{Name: "ignored:search", Pattern: `\bsearch\b`, Fields: []string{"title", "abstract"}, Raw: true, Unless: []string{"title:search", "abstract:search", "title:retrieval"}},
			{Name: "ignored:ads", Pattern: `\bads\b`, Fields: []string{"title", "abstract"}, Raw: true, Unless: []string{"title:advertising", "abstract:advertising"}},
		},
		Decisions: []Decision{
			{Tier: 2, Reason: "title match", MinSignalTier: atLeast(2)},
			{Tier: 2, Reason: "cs.IR with a technique signal", AllSignals: []string{"category:cs.IR"}, AnySignals: []string{"technique:retrieval", "technique:softmax-sampling"}},
			{Tier: 1, Reason: "abstract phrase only", MinSignalTier: atLeast(1)},
			{Tier: 0, Reason: "private information retrieval is not search", AnySignals: []string{"excluded:private-information-retrieval", "excluded:dna-storage"}},
			{Tier: 0, Reason: "recommendation appears only as an ordinary word", AnySignals: []string{"ignored:recommendation"}},
			{Tier: 0, Reason: "cs.IR alone is not a topic match", AllSignals: []string{"category:cs.IR"}},
			{Tier: 0, Reason: "search or ads appears only as an ordinary word", AnySignals: []string{"ignored:search", "ignored:ads"}},
		},
		FallbackReason: "no topic signal",
	}
}

var (
	builtinRules Rules
	builtinOnce  sync.Once
)

// DefaultRules is the compiled built-in selection. It panics if that spec is invalid.
func DefaultRules() Rules {
	builtinOnce.Do(func() {
		compiled, err := Compile(DefaultSelection())
		if err != nil {
			panic(err)
		}
		builtinRules = compiled
	})
	return builtinRules
}
