package papers

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const defaultBaseURL = "https://export.arxiv.org/api/query"

// rasSearchQuery stays on title, abstract, and cs.IR. arXiv's all: field also
// matches the PDF, so a citation or the verb "recommend" was enough to crowd
// out the lookback window.
const rasSearchQuery = `(cat:cs.IR OR ti:recommender OR ti:recommendation OR abs:recommender OR abs:"collaborative filtering" OR ti:advertising OR ti:advertisement OR ti:advertiser OR abs:advertising OR abs:"click-through" OR abs:"sponsored search" OR abs:"ad auction" OR abs:"ad allocation" OR abs:"ad ranking" OR abs:"ad targeting" OR ti:retrieval OR abs:"information retrieval" OR abs:"document retrieval" OR abs:"dense retrieval" OR abs:"query understanding" OR abs:"query rewriting" OR abs:"learning to rank" OR ti:"web search" OR ti:"search engine" OR ti:"conversational search" OR abs:"search ranking" OR abs:"sequential recommendation")`

var (
	arxivPageSize  = 100
	arxivMaxPages  = 16
	arxivPageDelay = 3 * time.Second
)

var arxivIDPattern = regexp.MustCompile(`(?i)^((?:\d{4}\.\d{4,5})|(?:[a-z][a-z.-]*/\d{7}))(?:v([1-9]\d*))?$`)

type Paper struct {
	ID        string
	Version   string
	Title     string
	Authors   []string
	Published time.Time
	Updated   time.Time
	Abstract  string
	URL       string

	categories []string
}

type atomFeed struct {
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID         string       `xml:"id"`
	Title      string       `xml:"title"`
	Published  string       `xml:"published"`
	Updated    string       `xml:"updated"`
	Summary    string       `xml:"summary"`
	Authors    []atomAuthor `xml:"author"`
	Categories []struct {
		Term string `xml:"term,attr"`
	} `xml:"category"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

func Fetch(ctx context.Context, client *http.Client, baseURL string, limit int) ([]Paper, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("arXiv result limit must be positive")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return fetchPage(ctx, client, baseURL, 0, limit)
}

// FetchSince pages the RAS query, newest first, until a page ends before since
// or the page cap is reached. since is the start of the lookback window.
func FetchSince(ctx context.Context, client *http.Client, baseURL string, since time.Time) ([]Paper, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if since.IsZero() {
		return Fetch(ctx, client, baseURL, arxivPageSize)
	}
	var all []Paper
	for page := 0; page < arxivMaxPages; page++ {
		if page > 0 && arxivPageDelay > 0 {
			timer := time.NewTimer(arxivPageDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		batch, err := fetchPage(ctx, client, baseURL, page*arxivPageSize, arxivPageSize)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		if len(batch) < arxivPageSize || !submittedInWindow(batch[len(batch)-1], since) {
			break
		}
	}
	return all, nil
}

func submittedInWindow(paper Paper, since time.Time) bool {
	updated := paper.Updated
	if updated.IsZero() {
		updated = paper.Published
	}
	return !updated.Before(since) || !paper.Published.Before(since)
}

func fetchPage(ctx context.Context, client *http.Client, baseURL string, start, limit int) ([]Paper, error) {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, fmt.Errorf("invalid arXiv API base URL %q", baseURL)
	}
	query := endpoint.Query()
	query.Set("search_query", rasSearchQuery)
	query.Set("start", fmt.Sprint(start))
	query.Set("max_results", fmt.Sprint(limit))
	query.Set("sortBy", "submittedDate")
	query.Set("sortOrder", "descending")
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create arXiv request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch arXiv feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("arXiv API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var feed atomFeed
	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, fmt.Errorf("decode arXiv Atom feed: %w", err)
	}
	papers := make([]Paper, 0, len(feed.Entries))
	for i, entry := range feed.Entries {
		paper, err := parseEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("parse arXiv entry %d: %w", i, err)
		}
		papers = append(papers, paper)
	}
	return papers, nil
}

func parseEntry(entry atomEntry) (Paper, error) {
	id, version, err := parseID(entry.ID)
	if err != nil {
		return Paper{}, err
	}
	published, err := time.Parse(time.RFC3339, strings.TrimSpace(entry.Published))
	if err != nil {
		return Paper{}, fmt.Errorf("parse published time: %w", err)
	}
	updated := published
	if strings.TrimSpace(entry.Updated) != "" {
		updated, err = time.Parse(time.RFC3339, strings.TrimSpace(entry.Updated))
		if err != nil {
			return Paper{}, fmt.Errorf("parse updated time: %w", err)
		}
	}

	authors := make([]string, 0, len(entry.Authors))
	for _, author := range entry.Authors {
		if name := normalizeSpace(author.Name); name != "" {
			authors = append(authors, name)
		}
	}
	categories := make([]string, 0, len(entry.Categories))
	for _, category := range entry.Categories {
		if term := strings.TrimSpace(category.Term); term != "" {
			categories = append(categories, term)
		}
	}

	versionSuffix := version
	return Paper{
		ID:         id,
		Version:    version,
		Title:      normalizeSpace(entry.Title),
		Authors:    authors,
		Published:  published,
		Updated:    updated,
		Abstract:   normalizeSpace(entry.Summary),
		URL:        "https://arxiv.org/abs/" + strings.TrimPrefix(id, "arxiv:") + versionSuffix,
		categories: categories,
	}, nil
}

func parseID(raw string) (string, string, error) {
	candidate := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(candidate), "arxiv:") {
		candidate = candidate[len("arxiv:"):]
	}
	if parsed, err := url.Parse(candidate); err == nil && parsed.Path != "" {
		candidate = strings.TrimPrefix(parsed.Path, "/abs/")
		if candidate == parsed.Path {
			candidate = path.Base(parsed.Path)
		}
		candidate = strings.Trim(candidate, "/")
	}
	match := arxivIDPattern.FindStringSubmatch(candidate)
	if match == nil {
		return "", "", fmt.Errorf("invalid arXiv ID %q", raw)
	}
	version := ""
	if match[2] != "" {
		version = "v" + match[2]
	}
	return "arxiv:" + match[1], version, nil
}

func normalizeSpace(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}
