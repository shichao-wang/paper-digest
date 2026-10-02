package papers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/arxivclient"
	"github.com/shichao-wang/paper-digest/internal/library"
)

// Source captures the public sources before interpreting them. Artifact paths are
// relative to ArtifactRoot, which can also be the document repository root.
// Empty bases use official endpoints; loopback bases permit offline tests.
type Source struct {
	Client       *http.Client
	FeedBase     string
	ListBase     string
	APIBase      string
	ArtifactRoot string
	Now          func() time.Time
}

const (
	atomNamespace          = "http://www.w3.org/2005/Atom"
	maxSourceBody          = 16 << 20
	maxSourceBytes         = 128 << 20
	maxListPages           = 256
	sourceRequestTimeout   = 30 * time.Second
	sourceOperationTimeout = 10 * time.Minute
)

// FetchAnnouncements retains every captured event, including replacements and
// cross listings. Only batches verified against the complete official same-date
// listing are marked complete. On transport/resource errors, partial batches and
// their artifacts are returned with the error so callers can preserve evidence.
func (s Source) FetchAnnouncements(ctx context.Context, categories []string) ([]library.CategoryBatch, error) {
	if s.ArtifactRoot == "" {
		return nil, fmt.Errorf("arXiv ArtifactRoot is required")
	}
	seen := map[string]bool{}
	for _, category := range categories {
		if !arxivclient.SupportsAnnouncementCategory(category) || seen[category] {
			return nil, fmt.Errorf("invalid or duplicate arXiv announcement category %q", category)
		}
		seen[category] = true
	}
	feedBase, err := sourceBase(s.FeedBase, "https://rss.arxiv.org/atom")
	if err != nil {
		return nil, err
	}
	listBase, err := sourceBase(s.ListBase, "https://arxiv.org/list")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, sourceOperationTimeout)
	defer cancel()
	batches := make([]library.CategoryBatch, 0, len(categories))
	budget := maxSourceBytes
	var failures []error
	for _, category := range categories {
		batch := library.CategoryBatch{Category: category, CapturedAt: library.Timestamp(s.now()), Completeness: "incomplete", Counts: map[string]int{"new": 0, "cross": 0, "replace": 0, "replace-cross": 0}}
		feedURL := appendSourcePath(feedBase, category)
		body, artifact, fetchErr := s.capture(ctx, feedURL, "announcement-atom", &budget)
		if artifact.Path != "" {
			batch.Artifacts = append(batch.Artifacts, artifact)
		}
		reasons := []string{}
		if fetchErr != nil {
			reasons = append(reasons, fetchErr.Error())
			failures = append(failures, fetchErr)
		}
		// Even a truncated body can contain complete candidate entries.
		feed, parseErr := decodeSourceFeed(body)
		if parseErr != nil {
			reasons = append(reasons, "Atom structure: "+parseErr.Error())
		}
		if fetchErr == nil && parseErr == nil && feed.Updated == "" {
			reasons = append(reasons, "Atom feed has no build timestamp")
		}
		if feed.Updated != "" {
			built, e := time.Parse(time.RFC3339Nano, strings.TrimSpace(feed.Updated))
			if e != nil {
				reasons = append(reasons, "invalid Atom build timestamp")
			} else {
				batch.BuiltAt = library.Timestamp(built)
			}
		}
		dates := map[string]bool{}
		if feed.Published != "" {
			date, e := announcementDate(feed.Published)
			if e != nil {
				reasons = append(reasons, e.Error())
			} else {
				dates[date] = true
			}
		}
		stableVersions := map[string]string{}
		for index, entry := range feed.Entries {
			id, v, e := parseID(entry.ID)
			if e != nil || v == "" {
				reasons = append(reasons, fmt.Sprintf("entry %d: invalid exact arXiv ID/version", index))
				continue
			}
			id = strings.TrimPrefix(id, "arxiv:")
			identity := library.Identity{Source: "arxiv", PaperID: id, Version: v}
			if identity.Validate() != nil {
				reasons = append(reasons, fmt.Sprintf("entry %d: invalid identity", index))
				continue
			}
			kind := strings.TrimSpace(entry.AnnounceType)
			if normalizeSpace(entry.Title) == "" || announcementAbstract(entry.Summary) == "" || len(entryCategories(entry)) == 0 {
				reasons = append(reasons, fmt.Sprintf("entry %d: announcement lacks title, abstract or categories", index))
			}
			if !containsString(entryCategories(entry), category) {
				reasons = append(reasons, fmt.Sprintf("entry %d: announcement does not include requested category", index))
			}
			if kind != "new" && kind != "cross" && kind != "replace" && kind != "replace-cross" {
				reasons = append(reasons, fmt.Sprintf("entry %d: unknown announcement type %q", index, kind))
			}
			date := ""
			if entry.Published != "" {
				date, e = announcementDate(entry.Published)
				if e != nil {
					reasons = append(reasons, fmt.Sprintf("entry %d: %v", index, e))
				} else {
					dates[date] = true
				}
			}
			if prior, exists := stableVersions[id]; exists {
				reasons = append(reasons, fmt.Sprintf("ambiguous duplicate stable ID %s (%s/%s)", id, prior, v))
			}
			stableVersions[id] = v
			batch.Counts[kind]++
			batch.Events = append(batch.Events, library.Announcement{Identity: identity, Category: category, Type: kind, Date: date})
			authors := entryAuthors(entry)
			cats := entryCategories(entry)
			batch.Versions = append(batch.Versions, library.Version{Identity: identity, Title: normalizeSpace(entry.Title), Abstract: announcementAbstract(entry.Summary), Authors: authors, Categories: cats, PrimaryCategory: strings.TrimSpace(entry.PrimaryCategory.Term), DOI: normalizeSpace(entry.DOI), JournalRef: normalizeSpace(entry.JournalRef), Comment: normalizeSpace(entry.Comment), Origin: "announcement", AnnouncementDate: date, CapturedAt: batch.CapturedAt})
		}
		if len(dates) == 1 {
			for date := range dates {
				batch.Date = date
			}
		} else if len(dates) > 1 {
			reasons = append(reasons, "Atom entries contain different announcement dates")
		}
		// A failed fetch still records candidates, but context/budget failures cannot
		// safely start more requests.
		if ctx.Err() == nil && budget > 0 {
			listing, artifacts, listErr := s.captureListing(ctx, appendSourcePath(listBase, category+"/new"), &budget)
			batch.Artifacts = append(batch.Artifacts, artifacts...)
			reasons = append(reasons, listing.Reasons...)
			if listErr != nil {
				reasons = append(reasons, listErr.Error())
				failures = append(failures, listErr)
			}
			if listing.Date == "" {
				reasons = append(reasons, "official list has no announcement date")
			} else {
				if batch.Date == "" && len(dates) == 0 {
					batch.Date = listing.Date
				}
				if len(dates) > 1 || batch.Date != listing.Date {
					reasons = append(reasons, "Atom and official list announcement dates differ")
				}
			}
			for i := range batch.Events {
				if batch.Events[i].Date == "" {
					batch.Events[i].Date = batch.Date
					batch.Versions[i].AnnouncementDate = batch.Date
				}
			}
			reasons = append(reasons, compareListing(batch, listing)...)
		} else {
			reasons = append(reasons, "official list verification interrupted by context or resource limit")
		}
		if len(reasons) == 0 {
			batch.Completeness = "complete"
		} else {
			batch.Reason = strings.Join(uniqueStrings(reasons), "; ")
		}
		batches = append(batches, batch)
		if ctx.Err() != nil || budget <= 0 {
			break
		}
	}
	for i := range batches {
		for j := range batches[i].Events {
			if batches[i].Events[j].Date == "" && batches[i].Date != "" {
				batches[i].Events[j].Date = batches[i].Date
				batches[i].Versions[j].AnnouncementDate = batches[i].Date
			}
		}
	}
	if ctx.Err() != nil {
		failures = append(failures, ctx.Err())
	}
	if budget <= 0 {
		failures = append(failures, fmt.Errorf("arXiv source operation resource byte limit exhausted"))
	}
	return batches, errors.Join(failures...)
}

// FetchVersionMetadata uses id_list with an exact version and rejects API
// substitution of a newer version or another paper. RSS dates never enter here.
func (s Source) FetchVersionMetadata(ctx context.Context, identity library.Identity) (library.Version, []library.Artifact, error) {
	if identity.Validate() != nil {
		return library.Version{}, nil, fmt.Errorf("invalid arXiv identity")
	}
	if s.ArtifactRoot == "" {
		return library.Version{}, nil, fmt.Errorf("arXiv ArtifactRoot is required")
	}
	endpoint, err := sourceBase(s.APIBase, defaultBaseURL)
	if err != nil {
		return library.Version{}, nil, err
	}
	q := endpoint.Query()
	for _, key := range []string{"search_query", "start", "sortBy", "sortOrder"} {
		q.Del(key)
	}
	q.Set("id_list", strings.TrimPrefix(identity.PaperID, "arxiv:")+identity.Version)
	q.Set("max_results", "1")
	endpoint.RawQuery = q.Encode()
	budget := maxSourceBytes
	body, artifact, err := s.capture(ctx, endpoint, "version-api", &budget)
	artifacts := []library.Artifact{}
	if artifact.Path != "" {
		artifacts = append(artifacts, artifact)
	}
	if err != nil {
		return library.Version{}, artifacts, err
	}
	feed, err := decodeSourceFeed(body)
	if err != nil {
		return library.Version{}, artifacts, fmt.Errorf("decode exact arXiv metadata: %w", err)
	}
	if len(feed.Entries) != 1 {
		return library.Version{}, artifacts, fmt.Errorf("exact arXiv metadata returned %d entries, want 1", len(feed.Entries))
	}
	entry := feed.Entries[0]
	id, v, err := parseID(entry.ID)
	if err != nil || id != canonicalPaperID(identity.PaperID) || v != identity.Version {
		return library.Version{}, artifacts, fmt.Errorf("arXiv API identity/version mismatch: wanted %s%s, received %q", identity.PaperID, identity.Version, entry.ID)
	}
	published, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(entry.Published))
	if err != nil {
		return library.Version{}, artifacts, fmt.Errorf("invalid API published submission time: %w", err)
	}
	updated, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(entry.Updated))
	if err != nil {
		return library.Version{}, artifacts, fmt.Errorf("invalid API updated submission time: %w", err)
	}
	if updated.Before(published) {
		return library.Version{}, artifacts, fmt.Errorf("API updated submission time precedes published")
	}
	if normalizeSpace(entry.Title) == "" || normalizeSpace(entry.Summary) == "" || len(entryAuthors(entry)) == 0 {
		return library.Version{}, artifacts, fmt.Errorf("API metadata lacks title, abstract or authors")
	}
	if len(entryCategories(entry)) == 0 || !containsString(entryCategories(entry), strings.TrimSpace(entry.PrimaryCategory.Term)) {
		return library.Version{}, artifacts, fmt.Errorf("API metadata lacks a primary category among its categories")
	}
	identity.PaperID = strings.TrimPrefix(id, "arxiv:")
	version := library.Version{Identity: identity, Title: normalizeSpace(entry.Title), Authors: entryAuthors(entry), Abstract: normalizeSpace(entry.Summary), Categories: entryCategories(entry), PrimaryCategory: strings.TrimSpace(entry.PrimaryCategory.Term), PublishedAt: library.Timestamp(published), UpdatedAt: library.Timestamp(updated), DOI: normalizeSpace(entry.DOI), JournalRef: normalizeSpace(entry.JournalRef), Comment: normalizeSpace(entry.Comment), Origin: "api", CapturedAt: artifact.CapturedAt, MetadataVerified: true}
	version.MetadataArtifacts = artifacts
	return version, artifacts, nil
}

func canonicalPaperID(id string) string { return "arxiv:" + strings.TrimPrefix(id, "arxiv:") }
func (s Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func sourceBase(raw, defaultURL string) (*url.URL, error) {
	if raw == "" {
		raw = defaultURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("invalid arXiv base URL %q", raw)
	}
	official, _ := url.Parse(defaultURL)
	host := u.Hostname()
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !loopback && (u.Host != official.Host || u.Scheme != "https" || strings.TrimRight(u.Path, "/") != official.Path) {
		return nil, fmt.Errorf("arXiv base must be the official endpoint or an injected loopback test server")
	}
	return u, nil
}
func appendSourcePath(base *url.URL, suffix string) *url.URL {
	u := *base
	u.Path = strings.TrimRight(u.Path, "/") + "/" + suffix
	u.RawPath = ""
	return &u
}

func (s Source) capture(ctx context.Context, endpoint *url.URL, kind string, budget *int) ([]byte, library.Artifact, error) {
	if *budget <= 0 {
		return nil, library.Artifact{}, fmt.Errorf("arXiv source resource byte limit exceeded")
	}
	ctx, cancel := context.WithTimeout(ctx, sourceRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, library.Artifact{}, err
	}
	req.Header.Set("User-Agent", "paper-digest/1.0 (public arXiv metadata)")
	req.Header.Set("Accept", "application/atom+xml, text/html;q=0.9")
	client := http.Client{Timeout: sourceRequestTimeout}
	if s.Client != nil {
		client = *s.Client
		if client.Timeout == 0 || client.Timeout > sourceRequestTimeout {
			client.Timeout = sourceRequestTimeout
		}
	}
	// Redirects are returned as responses so their original body is captured;
	// they never turn an official endpoint into an arbitrary download URL.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := arxivclient.Client(&client).Do(req)
	if err != nil {
		return nil, library.Artifact{}, fmt.Errorf("fetch arXiv source %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	limit := maxSourceBody
	if *budget < limit {
		limit = *budget
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	*budget -= len(body)
	artifact, saveErr := s.saveCapture(body, endpoint.String(), kind)
	if saveErr != nil {
		return body, artifact, saveErr
	}
	if len(body) > limit {
		return body, artifact, fmt.Errorf("arXiv response exceeds body/resource byte limit (%d bytes)", limit)
	}
	if readErr != nil {
		return body, artifact, fmt.Errorf("read arXiv source: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return body, artifact, fmt.Errorf("arXiv source returned HTTP %s", resp.Status)
	}
	return body, artifact, nil
}

func (s Source) saveCapture(body []byte, sourceURL, kind string) (library.Artifact, error) {
	return s.saveCaptureWithSync(body, sourceURL, kind, syncCaptureDirectory)
}

func (s Source) saveCaptureWithSync(body []byte, sourceURL, kind string, syncDir func(string) error) (library.Artifact, error) {
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	relative := filepath.Join("artifacts", "arxiv", hash[:2], hash+".bin")
	destination := filepath.Join(s.ArtifactRoot, relative)
	artifact := library.Artifact{Path: filepath.ToSlash(relative), SHA256: hash, URL: sourceURL, CapturedAt: library.Timestamp(s.now()), Kind: kind}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return library.Artifact{}, fmt.Errorf("create arXiv capture directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(destination), ".capture-*")
	if err != nil {
		return library.Artifact{}, err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(body); err == nil {
		err = temp.Chmod(0400)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return library.Artifact{}, err
	}
	// Linking publishes the complete file atomically, without overwriting a prior
	// capture. A reused path must contain the exact same bytes.
	if err = os.Link(temp.Name(), destination); err != nil {
		if !os.IsExist(err) {
			return library.Artifact{}, fmt.Errorf("publish arXiv capture: %w", err)
		}
		info, e := os.Lstat(destination)
		if e != nil || !info.Mode().IsRegular() {
			return library.Artifact{}, fmt.Errorf("invalid existing arXiv capture path")
		}
		existing, e := os.ReadFile(destination)
		if e != nil || !bytes.Equal(body, existing) {
			return library.Artifact{}, fmt.Errorf("immutable arXiv capture content mismatch")
		}
	}
	// Flush the publication first, then every ancestor entry that might have
	// been created by MkdirAll (including ArtifactRoot). Also flush reused paths:
	// another collector may have linked the file but not yet synced its parents.
	if err = syncCaptureHierarchy(filepath.Dir(destination), syncDir); err != nil {
		return library.Artifact{}, fmt.Errorf("sync arXiv capture directories: %w", err)
	}
	return artifact, nil
}

func syncCaptureHierarchy(directory string, syncDir func(string) error) error {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	for {
		if err := syncDir(directory); err != nil {
			return err
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return nil
		}
		directory = parent
	}
}

func syncCaptureDirectory(directory string) error {
	f, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(syncErr, closeErr)
}

type sourceFeed struct {
	Updated   string
	Published string
	Entries   []atomEntry
}

// Stream entries so complete candidates survive an interrupted/truncated XML
// response. Root and extension namespaces are checked by encoding/xml.
func decodeSourceFeed(body []byte) (sourceFeed, error) {
	var feed sourceFeed
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var root xml.StartElement
	for {
		token, err := decoder.Token()
		if err != nil {
			return feed, err
		}
		if start, ok := token.(xml.StartElement); ok {
			root = start
			break
		}
	}
	if root.Name.Space != atomNamespace || root.Name.Local != "feed" {
		return feed, fmt.Errorf("expected Atom feed root and namespace")
	}
	for {
		token, err := decoder.Token()
		if err != nil {
			return feed, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Space != atomNamespace {
				if value.Name.Local == "entry" || value.Name.Local == "updated" || value.Name.Local == "published" {
					return feed, fmt.Errorf("unexpected namespace for feed child %s", value.Name.Local)
				}
				if err = decoder.Skip(); err != nil {
					return feed, err
				}
				continue
			}
			switch value.Name.Local {
			case "entry":
				var entry atomEntry
				if err = decoder.DecodeElement(&entry, &value); err != nil {
					return feed, err
				}
				feed.Entries = append(feed.Entries, entry)
			case "updated":
				if err = decoder.DecodeElement(&feed.Updated, &value); err != nil {
					return feed, err
				}
			case "published":
				if err = decoder.DecodeElement(&feed.Published, &value); err != nil {
					return feed, err
				}
			default:
				if err = decoder.Skip(); err != nil {
					return feed, err
				}
			}
		case xml.EndElement:
			if value.Name == root.Name {
				for {
					extra, e := decoder.Token()
					if e == io.EOF {
						return feed, nil
					}
					if e != nil {
						return feed, e
					}
					switch x := extra.(type) {
					case xml.CharData:
						if len(bytes.TrimSpace(x)) != 0 {
							return feed, fmt.Errorf("trailing Atom content")
						}
					case xml.Comment, xml.ProcInst:
					default:
						return feed, fmt.Errorf("trailing XML element")
					}
				}
			}
		}
	}
}

func announcementDate(raw string) (string, error) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid announcement date %q", raw)
	}
	return t.Format("2006-01-02"), nil
}
func entryAuthors(entry atomEntry) []string {
	authors := []string{}
	for _, author := range entry.Authors {
		if name := normalizeSpace(author.Name); name != "" {
			authors = append(authors, name)
		}
	}
	if len(authors) == 0 {
		for _, creator := range entry.Creators {
			for _, name := range strings.Split(creator, ",") {
				if name = normalizeSpace(name); name != "" {
					authors = append(authors, name)
				}
			}
		}
	}
	return authors
}
func entryCategories(entry atomEntry) []string {
	cats := []string{}
	for _, category := range entry.Categories {
		if term := strings.TrimSpace(category.Term); term != "" {
			cats = append(cats, term)
		}
	}
	return uniqueStrings(cats)
}

var abstractPrefix = regexp.MustCompile(`(?is)^\s*arxiv:\S+\s+Announce\s+Type:\s*\S+\s+Abstract:\s*`)

func announcementAbstract(raw string) string {
	return normalizeSpace(abstractPrefix.ReplaceAllString(raw, ""))
}
func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range values {
		if !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	return out
}

var (
	headingPattern    = regexp.MustCompile(`(?is)<h[1-4]\b[^>]*>(.*?)</h[1-4]\s*>`)
	nonContentPattern = regexp.MustCompile(`(?is)<!--.*?-->|<script\b[^>]*>.*?</script\s*>|<style\b[^>]*>.*?</style\s*>`)
	tagPattern        = regexp.MustCompile(`(?s)<[^>]*>`)
	totalPattern      = regexp.MustCompile(`(?i)\bTotal of\s+(\d+)\s+entries\b`)
	listDatePattern   = regexp.MustCompile(`(?i)Showing new listings for\s+([A-Za-z]+,\s*\d+\s+[A-Za-z]+\s+\d{4})`)
	sectionPattern    = regexp.MustCompile(`(?i)^(New|Cross|Replacement) submissions\s*\(showing\s+(\d+)\s+of\s+(\d+)\s+entries\)`)
	itemStartPattern  = regexp.MustCompile(`(?i)<dt\b`)
	itemPattern       = regexp.MustCompile(`(?is)<dt\b[^>]*>(.*?)</dt\s*>`)
	anchorPattern     = regexp.MustCompile(`(?is)<a\b[^>]*\bhref\s*=\s*["']([^"']+)["'][^>]*>`)
)

type listSection struct {
	Showing, Total int
	IDs            []string
}
type officialListing struct {
	Date     string
	Total    int
	Sections map[string]listSection
	Reasons  []string
}

func htmlText(raw string) string {
	return normalizeSpace(html.UnescapeString(tagPattern.ReplaceAllString(raw, " ")))
}
func parseOfficialListing(body []byte) officialListing {
	source := nonContentPattern.ReplaceAllString(string(body), " ")
	plain := htmlText(source)
	listing := officialListing{Total: -1, Sections: map[string]listSection{}}
	dates := listDatePattern.FindAllStringSubmatch(plain, -1)
	for _, m := range dates {
		date, err := time.Parse("Monday, 2 January 2006", normalizeSpace(m[1]))
		if err != nil {
			listing.Reasons = append(listing.Reasons, "invalid official list date")
			continue
		}
		d := date.Format("2006-01-02")
		if listing.Date != "" && listing.Date != d {
			listing.Reasons = append(listing.Reasons, "conflicting official list dates")
		}
		listing.Date = d
	}
	totals := totalPattern.FindAllStringSubmatch(plain, -1)
	for _, m := range totals {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			listing.Reasons = append(listing.Reasons, "invalid official list total")
			continue
		}
		if listing.Total != -1 && listing.Total != n {
			listing.Reasons = append(listing.Reasons, "conflicting official list totals")
		}
		listing.Total = n
	}
	if listing.Total < 0 {
		listing.Reasons = append(listing.Reasons, "official list has no declared total")
	}
	headings := headingPattern.FindAllStringSubmatchIndex(source, -1)
	parsedItems := 0
	for i, index := range headings {
		title := htmlText(source[index[2]:index[3]])
		match := sectionPattern.FindStringSubmatch(title)
		if match == nil {
			continue
		}
		key := map[string]string{"new": "new", "cross": "cross", "replacement": "replacement"}[strings.ToLower(match[1])]
		showing, e1 := strconv.Atoi(match[2])
		total, e2 := strconv.Atoi(match[3])
		if e1 != nil || e2 != nil {
			listing.Reasons = append(listing.Reasons, "invalid official section count")
			continue
		}
		section := listSection{Showing: showing, Total: total}
		end := len(source)
		if i+1 < len(headings) {
			end = headings[i+1][0]
		}
		items := itemPattern.FindAllStringSubmatch(source[index[1]:end], -1)
		parsedItems += len(items)
		for _, item := range items {
			ids := []string{}
			for _, anchor := range anchorPattern.FindAllStringSubmatch(item[1], -1) {
				href := html.UnescapeString(anchor[1])
				u, err := url.Parse(href)
				if err != nil || !strings.HasPrefix(u.Path, "/abs/") {
					continue
				}
				if (u.Host != "" && u.Host != "arxiv.org") || (u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
					listing.Reasons = append(listing.Reasons, "non-official abstract URL in list")
					continue
				}
				if u.RawQuery != "" || u.Fragment != "" {
					listing.Reasons = append(listing.Reasons, "invalid abstract URL in list")
					continue
				}
				id, _, err := parseID(strings.TrimPrefix(u.Path, "/abs/"))
				if err != nil {
					listing.Reasons = append(listing.Reasons, "invalid stable ID in official list")
					continue
				}
				ids = append(ids, strings.TrimPrefix(id, "arxiv:"))
			}
			if len(ids) != 1 {
				listing.Reasons = append(listing.Reasons, "official list item lacks one unambiguous stable ID")
			} else {
				section.IDs = append(section.IDs, ids[0])
			}
		}
		if section.Showing != len(section.IDs) || section.Showing > section.Total {
			listing.Reasons = append(listing.Reasons, "official section showing count does not match captured items")
		}
		if _, exists := listing.Sections[key]; exists {
			listing.Reasons = append(listing.Reasons, "duplicate official section")
		}
		listing.Sections[key] = section
	}
	if parsedItems != len(itemPattern.FindAllString(source, -1)) {
		listing.Reasons = append(listing.Reasons, "official items outside recognized three-group structure")
	}
	openedItems := itemStartPattern.FindAllString(source, -1)
	if len(openedItems) != len(itemPattern.FindAllString(source, -1)) {
		listing.Reasons = append(listing.Reasons, "official list contains truncated item markup")
	}
	if len(listing.Sections) == 0 {
		listing.Reasons = append(listing.Reasons, "unknown official three-group structure")
	}
	return listing
}

func (s Source) captureListing(ctx context.Context, endpoint *url.URL, budget *int) (officialListing, []library.Artifact, error) {
	result := officialListing{Total: -1, Sections: map[string]listSection{}}
	artifacts := []library.Artifact{}
	skip := 0
	seenIDs := map[string]bool{}
	for page := 0; page < maxListPages; page++ {
		u := *endpoint
		q := u.Query()
		q.Set("show", "1000")
		q.Set("skip", strconv.Itoa(skip))
		u.RawQuery = q.Encode()
		body, artifact, err := s.capture(ctx, &u, "announcement-list", budget)
		if artifact.Path != "" {
			artifacts = append(artifacts, artifact)
		}
		current := parseOfficialListing(body)
		result.Reasons = append(result.Reasons, current.Reasons...)
		if page == 0 {
			result.Date = current.Date
			result.Total = current.Total
			for k, section := range current.Sections {
				result.Sections[k] = listSection{Total: section.Total}
			}
		} else {
			if result.Date != current.Date {
				result.Reasons = append(result.Reasons, "official pagination dates differ")
			}
			if result.Total != current.Total {
				result.Reasons = append(result.Reasons, "official pagination totals differ")
			}
		}
		pageItems := 0
		for k, section := range current.Sections {
			prior, exists := result.Sections[k]
			if !exists {
				prior.Total = section.Total
			}
			if prior.Total != section.Total {
				result.Reasons = append(result.Reasons, "official pagination section totals differ")
			}
			for _, id := range section.IDs {
				if seenIDs[id] {
					result.Reasons = append(result.Reasons, "duplicate stable ID across official list pages")
				}
				seenIDs[id] = true
			}
			prior.IDs = append(prior.IDs, section.IDs...)
			prior.Showing += section.Showing
			result.Sections[k] = prior
			pageItems += len(section.IDs)
		}
		if err != nil {
			return result, artifacts, err
		}
		skip += pageItems
		if skip >= result.Total || len(result.Reasons) > 0 {
			return result, artifacts, nil
		}
		if pageItems == 0 {
			result.Reasons = append(result.Reasons, "official list pagination made no progress")
			return result, artifacts, nil
		}
	}
	result.Reasons = append(result.Reasons, "official list pagination resource limit exceeded")
	return result, artifacts, fmt.Errorf("arXiv official list pagination resource limit exceeded (%d pages)", maxListPages)
}
func compareListing(batch library.CategoryBatch, listing officialListing) []string {
	reasons := []string{}
	expected := map[string]map[string]bool{"new": {}, "cross": {}, "replacement": {}}
	for _, event := range batch.Events {
		key := event.Type
		if key == "replace" || key == "replace-cross" {
			key = "replacement"
		}
		if group, ok := expected[key]; ok {
			group[event.PaperID] = true
		}
	}
	if len(listing.Sections) != 3 {
		reasons = append(reasons, "official list must declare all three submission groups")
	}
	total := 0
	for _, key := range []string{"new", "cross", "replacement"} {
		section, ok := listing.Sections[key]
		if !ok {
			continue
		}
		total += section.Total
		if section.Total != section.Showing || section.Total != len(section.IDs) {
			reasons = append(reasons, "official "+key+" section is truncated/incomplete")
		}
		got := map[string]bool{}
		for _, id := range section.IDs {
			got[id] = true
		}
		if len(got) != len(expected[key]) {
			reasons = append(reasons, "Atom and official "+key+" stable IDs differ")
		} else {
			for id := range got {
				if !expected[key][id] {
					reasons = append(reasons, "Atom and official "+key+" stable IDs differ")
					break
				}
			}
		}
	}
	if total != listing.Total || listing.Total != len(batch.Events) {
		reasons = append(reasons, "Atom/list declared total or three-group count mismatch")
	}
	if len(batch.Events) == 0 && (listing.Total != 0 || listing.Date == "") {
		reasons = append(reasons, "empty Atom lacks same-date official zero-count evidence")
	}
	return reasons
}
