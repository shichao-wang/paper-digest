// Package library 定义论文版本库的共享合同，不依赖存储或模型客户端。
package library

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const SchemaVersion = "paper-analysis-v1"
const PromptVersion = "library-v1"

var ErrNotFound = errors.New("library: not found")
var ErrLease = errors.New("library: task lease lost")
var ErrInvalid = errors.New("library: invalid input")
var ErrPaused = errors.New("library: resource budget exhausted")
var ErrQuality = errors.New("library: document quality blocked")
var idPattern = regexp.MustCompile(`^(?:\d{4}\.\d{4,5}|[a-z][a-z.-]*/\d{7})$`)
var versionPattern = regexp.MustCompile(`^v([1-9]\d*)$`)

type Identity struct {
	Source  string `json:"source"`
	PaperID string `json:"paper_id"`
	Version string `json:"version"`
}

func (i Identity) Validate() error {
	if i.Source != "arxiv" || !idPattern.MatchString(strings.TrimPrefix(i.PaperID, "arxiv:")) || i.Number() == 0 {
		return ErrInvalid
	}
	return nil
}
func (i Identity) Number() int {
	m := versionPattern.FindStringSubmatch(i.Version)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
func (i Identity) Key() string { return i.Source + "/" + i.PaperID + "/" + i.Version }
func (i Identity) Previous() (Identity, bool) {
	if i.Number() <= 1 {
		return Identity{}, false
	}
	return Identity{i.Source, i.PaperID, fmt.Sprintf("v%d", i.Number()-1)}, true
}
func (i Identity) AbsURL() string {
	return "https://arxiv.org/abs/" + strings.TrimPrefix(i.PaperID, "arxiv:") + i.Version
}
func (i Identity) PDFURL() string {
	return "https://arxiv.org/pdf/" + strings.TrimPrefix(i.PaperID, "arxiv:") + i.Version
}

type Version struct {
	Identity
	Title             string     `json:"title"`
	Authors           []string   `json:"authors"`
	Abstract          string     `json:"abstract"`
	PrimaryCategory   string     `json:"primary_category"`
	Categories        []string   `json:"categories"`
	PublishedAt       string     `json:"published_at"`
	UpdatedAt         string     `json:"updated_at"`
	DOI               string     `json:"doi"`
	JournalRef        string     `json:"journal_ref"`
	Comment           string     `json:"comment"`
	AuthorKeywords    []string   `json:"author_keywords"`
	Origin            string     `json:"origin"`
	AnnouncementDate  string     `json:"announcement_date"`
	CapturedAt        string     `json:"captured_at"`
	MetadataVerified  bool       `json:"metadata_verified"`
	MetadataArtifacts []Artifact `json:"metadata_artifacts"`
}

type Artifact struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	URL        string `json:"url"`
	CapturedAt string `json:"captured_at"`
	Kind       string `json:"kind"`
}
type Announcement struct {
	Identity
	Category string `json:"category"`
	Type     string `json:"type"`
	Date     string `json:"date"`
}
type CategoryBatch struct {
	Category     string         `json:"category"`
	Date         string         `json:"date"`
	BuiltAt      string         `json:"built_at"`
	CapturedAt   string         `json:"captured_at"`
	Completeness string         `json:"completeness"`
	Reason       string         `json:"reason"`
	Counts       map[string]int `json:"counts"`
	Artifacts    []Artifact     `json:"artifacts"`
	Events       []Announcement `json:"events"`
	Versions     []Version      `json:"versions"`
}
type Gap struct {
	Category string `json:"category"`
	After    string `json:"after"`
	Before   string `json:"before"`
	Reason   string `json:"reason"`
}

type Relevance struct {
	Topics            []string `json:"topics"`
	DirectlyRelated   bool     `json:"directly_related"`
	Level             string   `json:"relevance_level"`
	Rationale         string   `json:"rationale"`
	ExtractedKeywords []string `json:"extracted_keywords"`
	EvidenceIDs       []string `json:"evidence_ids"`
}

func (r Relevance) Validate() error {
	if r.Rationale == "" || (r.Level != "direct" && r.Level != "unrelated" && r.Level != "uncertain") || r.DirectlyRelated != (r.Level == "direct") {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, t := range r.Topics {
		if (t != "recommendation" && t != "advertising" && t != "search") || seen[t] {
			return ErrInvalid
		}
		seen[t] = true
	}
	if r.DirectlyRelated && len(r.Topics) == 0 {
		return ErrInvalid
	}
	return nil
}

type Page struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
	SHA256 string `json:"sha256"`
}
type Block struct {
	ID     string `json:"id"`
	Page   int    `json:"page"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Text   string `json:"text"`
	SHA256 string `json:"sha256"`
}
type Document struct {
	ID string `json:"id"`
	Identity
	Source    Artifact `json:"source_file"`
	Text      Artifact `json:"text_file"`
	Extractor string   `json:"extractor"`
	Quality   string   `json:"quality"`
	Issues    []string `json:"issues"`
	Pages     []Page   `json:"pages"`
	Blocks    []Block  `json:"blocks"`
}
type Evidence struct {
	ID         string `json:"id"`
	DocumentID string `json:"document_id"`
	Version    string `json:"version"`
	BlockID    string `json:"block_id"`
	Section    string `json:"section"`
	Page       int    `json:"page"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
	Quote      string `json:"quote"`
}
type Claim struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}
type NumericResult struct {
	Dataset     *string  `json:"dataset"`
	Metric      string   `json:"metric"`
	Method      *string  `json:"method"`
	Baseline    *string  `json:"baseline"`
	Value       string   `json:"value"`
	Unit        *string  `json:"unit"`
	Setting     *string  `json:"setting"`
	EvidenceIDs []string `json:"evidence_ids"`
}
type Application struct {
	Scenario    string   `json:"scenario"`
	Conditions  *string  `json:"conditions"`
	Cost        *string  `json:"cost"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// AnalysisContent 为模型生成部分，所有键必填；只有指针字段允许 null。
type AnalysisContent struct {
	TitleZH           string          `json:"title_zh"`
	AuthorKeywords    []Claim         `json:"author_keywords"`
	ResourceLinks     []Claim         `json:"resource_links"`
	ExtractedKeywords []string        `json:"extracted_keywords"`
	Relevance         Relevance       `json:"relevance"`
	Problem           *Claim          `json:"problem"`
	Motivation        *Claim          `json:"motivation"`
	Method            *Claim          `json:"method"`
	Contributions     []Claim         `json:"contributions"`
	Datasets          []Claim         `json:"datasets"`
	Baselines         []Claim         `json:"baselines"`
	Results           []NumericResult `json:"results"`
	Limitations       []Claim         `json:"limitations"`
	AgentAssessment   []Claim         `json:"agent_assessment"`
	AuthorClaims      []Application   `json:"author_claims"`
	AgentInferences   []Application   `json:"agent_inferences"`
	SummaryZH         Claim           `json:"summary_zh"`
	KeyPoints         []Claim         `json:"key_points"`
	Evidence          []Evidence      `json:"evidence"`
	MissingFields     []string        `json:"missing_fields"`
}
type Analysis struct {
	SchemaVersion  string          `json:"schema_version"`
	PaperVersionID string          `json:"paper_version_id"`
	Model          string          `json:"model"`
	PromptVersion  string          `json:"prompt_version"`
	ParsedAt       string          `json:"parsed_at"`
	DocumentHashes []string        `json:"document_hashes"`
	Content        AnalysisContent `json:"content"`
}
type Chunk struct {
	DocumentID    string     `json:"document_id"`
	BlockID       string     `json:"block_id"`
	DocumentHash  string     `json:"document_hash"`
	Read          bool       `json:"read"`
	Notes         []Claim    `json:"notes"`
	Evidence      []Evidence `json:"evidence"`
	MissingFields []string   `json:"missing_fields"`
}
type Change struct {
	Kind                string   `json:"kind"`
	Description         string   `json:"description"`
	CurrentEvidenceIDs  []string `json:"current_evidence_ids"`
	PreviousEvidenceIDs []string `json:"previous_evidence_ids"`
}
type ComparisonContent struct {
	Changes  []Change   `json:"changes"`
	Evidence []Evidence `json:"evidence"`
}
type Comparison struct {
	AnalysisID      int64             `json:"analysis_id"`
	PreviousVersion string            `json:"previous_version"`
	Status          string            `json:"status"`
	Reason          string            `json:"reason"`
	DocumentHashes  []string          `json:"document_hashes"`
	Content         ComparisonContent `json:"content"`
}
type Task struct {
	ID int64 `json:"id"`
	Identity
	Stage         string          `json:"stage"`
	Generation    int             `json:"generation"`
	Status        string          `json:"status"`
	Attempt       int             `json:"attempt"`
	NextAttemptAt string          `json:"next_attempt_at"`
	LeaseToken    string          `json:"-"`
	LeaseUntil    string          `json:"lease_until"`
	Error         string          `json:"error"`
	Checkpoint    json.RawMessage `json:"checkpoint"`
}
type Run struct {
	Requests         int      `json:"requests"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	ReservedTokens   int64    `json:"reserved_tokens"`
	DurationMS       int64    `json:"duration_ms"`
	Model            string   `json:"model"`
	PromptVersion    string   `json:"prompt_version"`
	Coverage         []string `json:"coverage"`
}

// Completion 的所有变更与任务完成在同一事务中提交。
type Completion struct {
	Version            *Version
	Relevance          *Relevance
	Document           *Document
	ReferenceDocuments []Document
	Analysis           *Analysis
	Comparison         *Comparison
	Run                Run
	Next               []string
}
type ListItem struct {
	Version    Version    `json:"version"`
	Relevance  *Relevance `json:"relevance"`
	AnalysisID int64      `json:"analysis_id"`
	Tasks      []Task     `json:"tasks"`
}
type Detail struct {
	ListItem
	Versions         []Identity  `json:"versions"`
	RelevanceHistory []Relevance `json:"relevance_history"`
	Documents        []Document  `json:"documents"`
	Analysis         *Analysis   `json:"analysis"`
	Comparison       *Comparison `json:"comparison"`
	Runs             []Run       `json:"runs"`
}
type Query struct {
	Page                               int
	PageSize                           int
	Q, Batch, Topic, Relevance, Status string
}
type ResultPage struct {
	Items    []ListItem `json:"items"`
	Total    int        `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"pageSize"`
}
type Status struct {
	Versions int             `json:"versions"`
	Tasks    map[string]int  `json:"tasks"`
	Batches  []CategoryBatch `json:"batches"`
	Gaps     []Gap           `json:"gaps"`
}

func Timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
