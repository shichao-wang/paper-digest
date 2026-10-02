// Package document 保存精确版本的 arXiv PDF 与完整逐页文本提取结果。
package document

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shichao-wang/paper-digest/internal/arxivclient"
	"github.com/shichao-wang/paper-digest/internal/library"
)

var ErrQuality = library.ErrQuality
var ErrIntegrity = errors.New("document: integrity check failed")

const extractor = "poppler-pdftotext-layout-utf8-v1"
const syntheticExtractor = "synthetic-fixture-v1"
const defaultMaxBytes int64 = 64 << 20
const defaultBlockBytes = 12000
const maxPages = 10000

var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var pageCountPattern = regexp.MustCompile(`(?m)^Pages:\s+([0-9]+)\s*$`)

// PDFBase 是显式测试入口，可指定 /pdf 前缀或站点根地址，生产默认使用官方地址。
// PDFInfo 与 PDFText 只指定可执行文件，不接受 shell 命令。
type Repository struct {
	Root       string
	Client     *http.Client
	PDFBase    string
	PDFInfo    string
	PDFText    string
	MaxBytes   int64
	BlockBytes int
}

type manifest struct {
	Format     int              `json:"format"`
	Document   library.Document `json:"document"`
	BlockBytes int              `json:"block_bytes"`
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func canonical(i library.Identity) (library.Identity, error) {
	i.PaperID = strings.TrimPrefix(i.PaperID, "arxiv:")
	return i, i.Validate()
}
func identityID(i library.Identity) string { return digest([]byte(i.Key())) }
func documentID(i library.Identity, pdfHash, textHash string, blockBytes int) string {
	return digest([]byte(i.Key() + "\n" + pdfHash + "\n" + textHash + "\n" + strconv.Itoa(blockBytes)))
}
func (r *Repository) limits() (int64, int, error) {
	m, b := r.MaxBytes, r.BlockBytes
	if m == 0 {
		m = defaultMaxBytes
	}
	if b == 0 {
		b = defaultBlockBytes
	}
	if m < 5 || m > 1<<30 || b < utf8.UTFMax || b > 16<<20 {
		return 0, 0, library.ErrInvalid
	}
	return m, b, nil
}

// root 使用 os.Root 限定持久化边界，并拒绝受管路径中的符号链接。
func (r *Repository) root() (*os.Root, error) {
	if r.Root == "" {
		return nil, library.ErrInvalid
	}
	abs, err := filepath.Abs(r.Root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return nil, ErrIntegrity
	}
	return os.OpenRoot(abs)
}
func safePath(p string) bool {
	return p != "" && !filepath.IsAbs(p) && filepath.ToSlash(p) == p && !strings.Contains(p, "\\") && filepath.Clean(p) == p && p != "." && !strings.HasPrefix(p, "../")
}
func noSymlink(root *os.Root, p string) error {
	if !safePath(p) {
		return ErrIntegrity
	}
	parts := strings.Split(p, "/")
	for n := range parts {
		fi, err := root.Lstat(strings.Join(parts[:n+1], "/"))
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return ErrIntegrity
		}
		if n < len(parts)-1 && !fi.IsDir() {
			return ErrIntegrity
		}
	}
	return nil
}
func readFile(root *os.Root, p string, max int64) ([]byte, error) {
	if err := noSymlink(root, p); err != nil {
		return nil, err
	}
	f, err := root.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > max {
		return nil, ErrIntegrity
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if int64(len(b)) > max {
		return nil, ErrIntegrity
	}
	return b, err
}
func writeFile(root *os.Root, p string, b []byte) error {
	f, err := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
func syncDirectory(root *os.Root, p string) error {
	f, err := root.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func mkdir(root *os.Root, p string) error {
	if err := root.Mkdir(p, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return noSymlink(root, p)
}

// Ensure 复用已验证的完整文档，否则下载其精确 arXiv 版本。
func (r *Repository) Ensure(ctx context.Context, i library.Identity) (library.Document, error) {
	i, err := canonical(i)
	if err != nil {
		return library.Document{}, err
	}
	d, err := r.Load(ctx, i)
	if !errors.Is(err, library.ErrNotFound) {
		return d, err
	}
	max, b, err := r.limits()
	if err != nil {
		return library.Document{}, err
	}
	pdf, sourceURL, err := r.download(ctx, i, max)
	if err != nil {
		return library.Document{}, err
	}
	root, err := r.root()
	if err != nil {
		return library.Document{}, err
	}
	defer root.Close()
	if err = mkdir(root, ".staging"); err != nil {
		return library.Document{}, err
	}
	tmp, err := os.MkdirTemp(filepath.Join(root.Name(), ".staging"), "extract-")
	if err != nil {
		return library.Document{}, err
	}
	rel, err := filepath.Rel(root.Name(), tmp)
	if err != nil {
		return library.Document{}, err
	}
	defer root.RemoveAll(rel)
	if err = writeFile(root, rel+"/original.pdf", pdf); err != nil {
		return library.Document{}, err
	}
	pages, err := r.extract(ctx, filepath.Join(tmp, "original.pdf"))
	if err != nil {
		return library.Document{}, err
	}
	return r.publish(ctx, root, i, pdf, sourceURL, pages, b, false)
}

func (r *Repository) download(ctx context.Context, i library.Identity, max int64) ([]byte, string, error) {
	base := r.PDFBase
	if base == "" {
		base = "https://arxiv.org/pdf"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", library.ErrInvalid
	}
	if u.Path != "" && u.Path != "/" && u.Path != "/pdf" && u.Path != "/pdf/" {
		return nil, "", library.ErrInvalid
	}
	path := "/pdf/" + i.PaperID + i.Version
	u.Path = path
	sourceURL := u.String()
	injected := r.PDFBase != "" && !officialHost(u)
	if injected {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, "", library.ErrInvalid
		}
	}
	if !injected && !officialHost(u) {
		return nil, "", library.ErrInvalid
	}
	valid := func(v *url.URL) bool {
		if v == nil || v.User != nil || v.RawQuery != "" || v.Fragment != "" || v.RawPath != "" || (v.Path != path && v.Path != path+".pdf") {
			return false
		}
		if injected {
			return v.Scheme == u.Scheme && v.Host == u.Host
		}
		return officialHost(v)
	}
	client := http.Client{Timeout: 2 * time.Minute}
	if r.Client != nil {
		client = *r.Client
	}
	oldRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 6 || !valid(req.URL) {
			return fmt.Errorf("document: disallowed PDF redirect")
		}
		if oldRedirect != nil {
			return oldRedirect(req, via)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/pdf")
	resp, err := arxivclient.Client(&client).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.Request == nil || !valid(resp.Request.URL) {
		return nil, "", fmt.Errorf("document: disallowed PDF response URL")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("document: PDF HTTP status %d", resp.StatusCode)
	}
	if resp.ContentLength > max {
		return nil, "", fmt.Errorf("document: PDF exceeds %d bytes", max)
	}
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, parseErr := mime.ParseMediaType(contentType)
		if parseErr != nil || (mediaType != "application/pdf" && mediaType != "application/octet-stream") {
			return nil, "", fmt.Errorf("document: unexpected PDF content type")
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, "", err
	}
	if err = ctx.Err(); err != nil {
		return nil, "", err
	}
	if int64(len(body)) > max {
		return nil, "", fmt.Errorf("document: PDF exceeds %d bytes", max)
	}
	if !bytes.HasPrefix(body, []byte("%PDF-")) {
		return nil, "", fmt.Errorf("document: response lacks PDF magic")
	}
	return body, i.PDFURL(), nil
}
func officialHost(u *url.URL) bool {
	return u.Scheme == "https" && (u.Host == "arxiv.org" || u.Host == "export.arxiv.org" || u.Host == "static.arxiv.org")
}

func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	var out, stderr limitedBuffer
	out.max = 64 << 20
	stderr.max = 64 << 10
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("document: %s: %w (%s)", filepath.Base(name), err, strings.TrimSpace(stderr.String()))
	}
	// Poppler 有时在诊断 PDF 语法损坏后仍返回成功，不能据此生成可用证据。
	diagnostic := strings.ToLower(stderr.String())
	if strings.Contains(diagnostic, "syntax error") || strings.Contains(diagnostic, "couldn't read xref") {
		return nil, fmt.Errorf("%w: Poppler reported damaged PDF syntax", ErrQuality)
	}
	return out.Bytes(), nil
}

// 限制子进程输出，避免异常 PDF 消耗无限内存。
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.max-b.Len() {
		return 0, fmt.Errorf("document: extractor output limit exceeded")
	}
	return b.Buffer.Write(p)
}
func (r *Repository) extract(ctx context.Context, path string) ([]library.Page, error) {
	info, text := r.PDFInfo, r.PDFText
	if info == "" {
		info = "pdfinfo"
	}
	if text == "" {
		text = "pdftotext"
	}
	out, err := command(ctx, info, "-enc", "UTF-8", path)
	if err != nil {
		return nil, err
	}
	m := pageCountPattern.FindSubmatch(out)
	if len(m) != 2 {
		return nil, fmt.Errorf("document: pdfinfo missing page count")
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil || n < 1 || n > maxPages {
		return nil, fmt.Errorf("document: invalid page count")
	}
	pages := make([]library.Page, 0, n)
	total := 0
	for p := 1; p <= n; p++ {
		raw, err := command(ctx, text, "-layout", "-enc", "UTF-8", "-f", strconv.Itoa(p), "-l", strconv.Itoa(p), path, "-")
		if err != nil {
			return nil, err
		}
		raw = bytes.TrimSuffix(raw, []byte("\f")) // 去除 Poppler 页分隔符，保留页面正文。
		total += len(raw)
		if total > 64<<20 {
			return nil, fmt.Errorf("document: extracted text limit exceeded")
		}
		pages = append(pages, library.Page{Number: p, Text: string(raw), SHA256: digest(raw)})
	}
	return pages, nil
}

// 提示与阻断分开：ready 只表示已提取全部页面文本且通过基础完整性检查。
// 这不是 PDF 视觉等价证明，表格单元格对应关系和公式含义仍需分析阶段核实。
const visualFidelityWarning = "warning: PDF visual fidelity unverified; text may omit images or formula symbols and may not preserve table cells or reading order"

func qualityDiagnostics(pages []library.Page, synthetic bool) ([]string, string) {
	issues := quality(pages)
	status := "ready"
	if len(issues) > 0 {
		status = "blocked"
	}
	if !synthetic {
		issues = append(issues, visualFidelityWarning)
	}
	return issues, status
}

func quality(pages []library.Page) []string {
	var issues []string
	total := 0
	if len(pages) == 0 {
		return []string{"no pages"}
	}
	for _, p := range pages {
		meaningful, bad, private, nonspace := 0, 0, 0, 0
		if !utf8.ValidString(p.Text) {
			issues = append(issues, fmt.Sprintf("page %d: invalid UTF-8", p.Number))
		}
		for _, c := range p.Text {
			if !unicode.IsSpace(c) {
				nonspace++
			}
			if unicode.Is(unicode.Co, c) {
				private++
			}
			if unicode.IsLetter(c) || unicode.IsNumber(c) {
				meaningful++
			}
			if c == utf8.RuneError || (unicode.IsControl(c) && c != '\n' && c != '\r' && c != '\t') {
				bad++
			}
		}
		// 少数私用区符号可能是数学字体；仅大量不可解释字形才判定提取损坏。
		if private >= 32 && private*4 > nonspace {
			issues = append(issues, fmt.Sprintf("page %d: excessive unmapped private-use glyphs", p.Number))
		}
		if meaningful < 16 {
			issues = append(issues, fmt.Sprintf("page %d: empty or insufficient text", p.Number))
		}
		if bad > 0 {
			issues = append(issues, fmt.Sprintf("page %d: garbled text", p.Number))
		}
		total += meaningful
	}
	if total < 80 {
		issues = append(issues, "document: insufficient text")
	}
	return issues
}
func blocks(pages []library.Page, size int) []library.Block {
	var result []library.Block
	for _, p := range pages {
		for start := 0; start < len(p.Text); {
			end := start + size
			if end > len(p.Text) {
				end = len(p.Text)
			}
			for end < len(p.Text) && end > start && !utf8.RuneStart(p.Text[end]) {
				end--
			}
			if end == start {
				end = start + 1
				for end < len(p.Text) && !utf8.RuneStart(p.Text[end]) {
					end++
				}
			}
			text := p.Text[start:end]
			hash := digest([]byte(text))
			id := fmt.Sprintf("p%d:%d:%d:%s", p.Number, start, end, hash)
			result = append(result, library.Block{ID: id, Page: p.Number, Start: start, End: end, Text: text, SHA256: hash})
			start = end
		}
	}
	return result
}

func (r *Repository) publish(ctx context.Context, root *os.Root, i library.Identity, pdf []byte, sourceURL string, pages []library.Page, size int, synthetic bool) (library.Document, error) {
	if err := ctx.Err(); err != nil {
		return library.Document{}, err
	}
	if len(pages) == 0 || len(pages) > maxPages {
		return library.Document{Identity: i, Quality: "blocked", Issues: []string{"no pages or excessive page count"}}, fmt.Errorf("%w: invalid page count", ErrQuality)
	}
	for n := range pages {
		pages[n].Number = n + 1
		pages[n].SHA256 = digest([]byte(pages[n].Text))
		// JSON 无法无损保存非法 UTF-8；不能替换原字节后生成看似可用的证据坐标。
		if !utf8.ValidString(pages[n].Text) {
			d := library.Document{Identity: i, Quality: "blocked", Issues: []string{fmt.Sprintf("page %d: invalid UTF-8", n+1)}}
			return d, fmt.Errorf("%w: %s", ErrQuality, d.Issues[0])
		}
	}
	text, err := json.Marshal(pages)
	if err != nil {
		return library.Document{}, err
	}
	pdfHash, textHash := digest(pdf), digest(text)
	id := documentID(i, pdfHash, textHash, size)
	prefix := "documents/" + id
	now := library.Timestamp(time.Now())
	name, kind, extractorName := "original.pdf", "pdf", extractor
	if synthetic {
		name, kind, extractorName = "synthetic.txt", "synthetic", syntheticExtractor
	}
	d := library.Document{ID: id, Identity: i, Source: library.Artifact{Path: prefix + "/" + name, SHA256: pdfHash, URL: sourceURL, CapturedAt: now, Kind: kind}, Text: library.Artifact{Path: prefix + "/pages.json", SHA256: textHash, CapturedAt: now, Kind: "page-text-json"}, Extractor: extractorName, Pages: pages, Blocks: blocks(pages, size)}
	d.Issues, d.Quality = qualityDiagnostics(pages, synthetic)
	m, err := json.Marshal(manifest{Format: 1, Document: d, BlockBytes: size})
	if err != nil {
		return library.Document{}, err
	}
	for _, dir := range []string{"documents", "identities", ".staging"} {
		if err = mkdir(root, dir); err != nil {
			return library.Document{}, err
		}
	}
	tmp, err := os.MkdirTemp(filepath.Join(root.Name(), ".staging"), "publish-")
	if err != nil {
		return library.Document{}, err
	}
	rel, err := filepath.Rel(root.Name(), tmp)
	if err != nil {
		return library.Document{}, err
	}
	defer root.RemoveAll(rel)
	for _, f := range []struct {
		name string
		body []byte
	}{{name, pdf}, {"pages.json", text}, {"manifest.json", m}} {
		if err = writeFile(root, rel+"/"+f.name, f.body); err != nil {
			return library.Document{}, err
		}
	}
	if err = syncDirectory(root, rel); err != nil {
		return library.Document{}, err
	}
	if err = ctx.Err(); err != nil {
		return library.Document{}, err
	}
	if err = root.Rename(rel, prefix); err != nil {
		// 并发发布可能已经完成，不能覆盖已有不可变目录。
		existing, loadErr := r.loadID(ctx, root, id, i)
		if loadErr != nil && !errors.Is(loadErr, ErrQuality) {
			return library.Document{}, err
		}
		d = existing
	}
	if err = syncDirectory(root, "documents"); err != nil {
		return library.Document{}, err
	}
	pointer := []byte(id)
	index := "identities/" + identityID(i) + ".json"
	// 排他硬链接发布，防止同一论文版本静默切换到另一份原文。
	pointerTmp := rel + "-index"
	if err = writeFile(root, pointerTmp, pointer); err != nil {
		return library.Document{}, err
	}
	defer root.Remove(pointerTmp)
	if err = root.Link(pointerTmp, index); err != nil {
		existing, readErr := readFile(root, index, 128)
		if readErr != nil || string(existing) != id {
			return library.Document{}, fmt.Errorf("%w: identity already points to another document", ErrIntegrity)
		}
	}
	if err = syncDirectory(root, "identities"); err != nil {
		return library.Document{}, err
	}
	if err = syncDirectory(root, "."); err != nil {
		return library.Document{}, err
	}
	if d.Quality == "blocked" {
		return d, fmt.Errorf("%w: %s", ErrQuality, strings.Join(d.Issues, "; "))
	}
	return d, nil
}

// SaveFixture 仅保存明确标记的合成演示或测试数据，不冒充 PDF 提取成功。
func (r *Repository) SaveFixture(i library.Identity, texts []string) (library.Document, error) {
	i, err := canonical(i)
	if err != nil {
		return library.Document{}, err
	}
	_, size, err := r.limits()
	if err != nil {
		return library.Document{}, err
	}
	pages := make([]library.Page, len(texts))
	for n, t := range texts {
		pages[n] = library.Page{Number: n + 1, Text: t}
	}
	root, err := r.root()
	if err != nil {
		return library.Document{}, err
	}
	defer root.Close()
	original, err := json.Marshal(struct {
		Synthetic bool             `json:"synthetic"`
		Identity  library.Identity `json:"identity"`
		Pages     []string         `json:"pages"`
	}{true, i, texts})
	if err != nil {
		return library.Document{}, err
	}
	return r.publish(context.Background(), root, i, original, "", pages, size, true)
}

// Load 验证两个不可变产物、论文身份、全部页面和完整分块覆盖。
// ref 支持 Identity、Document、*Document 或 64 字符文档 ID。
func (r *Repository) Load(ctx context.Context, ref any) (library.Document, error) {
	if err := ctx.Err(); err != nil {
		return library.Document{}, err
	}
	root, err := r.root()
	if err != nil {
		return library.Document{}, err
	}
	defer root.Close()
	var id string
	var identity library.Identity
	var expected *library.Document
	switch v := ref.(type) {
	case library.Identity:
		identity, err = canonical(v)
		if err != nil {
			return library.Document{}, err
		}
		b, e := readFile(root, "identities/"+identityID(identity)+".json", 128)
		if errors.Is(e, os.ErrNotExist) {
			return library.Document{}, library.ErrNotFound
		}
		if e != nil {
			return library.Document{}, e
		}
		id = string(b)
	case library.Document:
		id = v.ID
		identity, err = canonical(v.Identity)
		expected = &v
	case *library.Document:
		if v == nil {
			return library.Document{}, library.ErrInvalid
		}
		id = v.ID
		identity, err = canonical(v.Identity)
		expected = v
	case string:
		id = v
	default:
		return library.Document{}, library.ErrInvalid
	}
	if err != nil {
		return library.Document{}, err
	}
	d, err := r.loadID(ctx, root, id, identity)
	if errors.Is(err, library.ErrNotFound) && identity.Source != "" {
		return library.Document{}, fmt.Errorf("%w: indexed document is incomplete", ErrIntegrity)
	}
	if err != nil && !errors.Is(err, ErrQuality) {
		return d, err
	}
	if expected != nil {
		a, _ := json.Marshal(expected)
		b, _ := json.Marshal(d)
		if !bytes.Equal(a, b) {
			return library.Document{}, fmt.Errorf("%w: document disagrees with manifest", ErrIntegrity)
		}
	}
	return d, err
}
func (r *Repository) loadID(ctx context.Context, root *os.Root, id string, want library.Identity) (library.Document, error) {
	fail := func(reason string) (library.Document, error) {
		return library.Document{}, fmt.Errorf("%w: %s", ErrIntegrity, reason)
	}
	if !hashPattern.MatchString(id) {
		return fail("invalid document ID")
	}
	prefix := "documents/" + id
	raw, err := readFile(root, prefix+"/manifest.json", 256<<20)
	if errors.Is(err, os.ErrNotExist) {
		return library.Document{}, library.ErrNotFound
	}
	if err != nil {
		return library.Document{}, err
	}
	var m manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&m); err != nil {
		return fail("manifest JSON")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return fail("manifest trailing data")
	}
	d := m.Document
	i, e := canonical(d.Identity)
	if e != nil || i != d.Identity || m.Format != 1 || d.ID != id || m.BlockBytes < 4 || m.BlockBytes > 16<<20 {
		return fail("manifest identity or format")
	}
	if want.Source != "" && i != want {
		return fail("identity mismatch")
	}
	name := "original.pdf"
	switch d.Extractor {
	case extractor:
		if d.Source.Kind != "pdf" || d.Source.URL != i.PDFURL() {
			return fail("PDF provenance")
		}
	case syntheticExtractor:
		name = "synthetic.txt"
		if d.Source.Kind != "synthetic" || d.Source.URL != "" {
			return fail("synthetic provenance")
		}
	default:
		return fail("unknown extractor")
	}
	if d.Source.Path != prefix+"/"+name || d.Text.Path != prefix+"/pages.json" || d.Text.Kind != "page-text-json" || !hashPattern.MatchString(d.Source.SHA256) || !hashPattern.MatchString(d.Text.SHA256) {
		return fail("artifact paths or hashes")
	}
	max, _, err := r.limits()
	if err != nil {
		return library.Document{}, err
	}
	if d.Extractor == syntheticExtractor && max < 64<<20 {
		max = 64 << 20
	}
	pdf, err := readFile(root, d.Source.Path, max)
	if err != nil {
		return library.Document{}, err
	}
	if digest(pdf) != d.Source.SHA256 || (name == "original.pdf" && !bytes.HasPrefix(pdf, []byte("%PDF-"))) {
		return fail("original hash or PDF magic")
	}
	text, err := readFile(root, d.Text.Path, 128<<20)
	if err != nil {
		return library.Document{}, err
	}
	if digest(text) != d.Text.SHA256 {
		return fail("text hash")
	}
	var pages []library.Page
	if err = json.Unmarshal(text, &pages); err != nil {
		return fail("page text JSON")
	}
	same := func(a, b any) bool { x, _ := json.Marshal(a); y, _ := json.Marshal(b); return bytes.Equal(x, y) }
	if len(pages) < 1 || len(pages) > maxPages || !same(pages, d.Pages) {
		return fail("manifest page text disagreement")
	}
	for n, p := range pages {
		if p.Number != n+1 || p.SHA256 != digest([]byte(p.Text)) {
			return fail("page number or hash")
		}
	}
	if documentID(i, d.Source.SHA256, d.Text.SHA256, m.BlockBytes) != id {
		return fail("document ID mismatch")
	}
	if !same(blocks(pages, m.BlockBytes), d.Blocks) {
		return fail("block hash, offset, or coverage")
	}
	issues, q := qualityDiagnostics(pages, d.Extractor == syntheticExtractor)
	if q != d.Quality || !same(issues, d.Issues) {
		return fail("quality disagreement")
	}
	if err = ctx.Err(); err != nil {
		return library.Document{}, err
	}
	if q == "blocked" {
		return d, fmt.Errorf("%w: %s", ErrQuality, strings.Join(issues, "; "))
	}
	return d, nil
}
