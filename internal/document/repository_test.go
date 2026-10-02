package document

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shichao-wang/paper-digest/internal/library"
)

var fixtureIdentity = library.Identity{Source: "arxiv", PaperID: "2501.01234", Version: "v2"}
var fixturePages = []string{
	"Introduction: recommendation research includes all original page text. 中文字节坐标 preserved without truncation.\nTable 1: Method baseline accuracy 99.1 and appendix reference.\n",
	"References and Appendix: every page is retained. Additional experimental settings and citation evidence remain searchable. 中文参考文献\n",
}

// 以测试程序本身注入 Poppler 命令，不安装本机依赖或使用 shell。
func TestMain(m *testing.M) {
	if os.Getenv("DOCUMENT_COMMAND_HELPER") == "1" {
		if os.Getenv("DOCUMENT_COMMAND_FAIL") == "1" {
			fmt.Fprint(os.Stderr, "fixture extractor failure")
			os.Exit(7)
		}
		if os.Getenv("DOCUMENT_COMMAND_WARNING") == "1" {
			fmt.Fprint(os.Stderr, "Syntax Error: damaged object stream")
		}
		if os.Getenv("DOCUMENT_COMMAND_SLEEP") == "1" {
			time.Sleep(20 * time.Second)
		}
		args := os.Args[1:]
		var pages []string
		if err := json.Unmarshal([]byte(os.Getenv("DOCUMENT_HELPER_PAGES")), &pages); err != nil {
			os.Exit(8)
		}
		if len(args) == 3 && args[0] == "-enc" && args[1] == "UTF-8" {
			fmt.Printf("Pages: %d\n", len(pages))
			os.Exit(0)
		}
		if len(args) != 9 || args[0] != "-layout" || args[1] != "-enc" || args[2] != "UTF-8" || args[3] != "-f" || args[5] != "-l" || args[4] != args[6] || args[8] != "-" {
			os.Exit(9)
		}
		page, err := strconv.Atoi(args[4])
		if err != nil || page < 1 || page > len(pages) {
			os.Exit(10)
		}
		fmt.Print(pages[page-1] + "\f")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// 替身验证 pdftotext 的九个参数，以真实子进程检查 CommandContext。
func helperRepository(t *testing.T, pages []string) *Repository {
	t.Helper()
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pages)
	t.Setenv("DOCUMENT_COMMAND_HELPER", "1")
	t.Setenv("DOCUMENT_HELPER_PAGES", string(raw))
	return &Repository{Root: t.TempDir(), PDFInfo: helper, PDFText: helper, BlockBytes: 37}
}
func servePDF(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s
}
func tinyPDF(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/minimal.pdf")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEnsureRoundTripAndAllUTF8Coverage(t *testing.T) {
	r := helperRepository(t, fixturePages)
	var requests atomic.Int32
	s := servePDF(t, func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if req.URL.Path != "/pdf/2501.01234v2" {
			t.Errorf("wrong exact path %q", req.URL.Path)
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(tinyPDF(t))
	})
	r.PDFBase = s.URL
	d, err := r.Ensure(context.Background(), fixtureIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if d.Source.Kind != "pdf" || d.Extractor != extractor || d.Source.URL != fixtureIdentity.PDFURL() {
		t.Fatalf("bad provenance %#v", d.Source)
	}
	if d.Quality != "ready" || !reflect.DeepEqual(d.Issues, []string{visualFidelityWarning}) {
		t.Fatalf("ready must retain visual fidelity limitation: %#v", d.Issues)
	}
	for _, ref := range []any{fixtureIdentity, d, &d, d.ID} {
		loaded, err := r.Load(context.Background(), ref)
		if err != nil || !reflect.DeepEqual(loaded, d) {
			t.Fatalf("load %T: %v", ref, err)
		}
	}
	again, err := r.Ensure(context.Background(), fixtureIdentity)
	if err != nil || again.ID != d.ID || requests.Load() != 1 {
		t.Fatalf("cache reuse: %v requests=%d", err, requests.Load())
	}
	for _, p := range d.Pages {
		var covered strings.Builder
		next := 0
		for _, b := range d.Blocks {
			if b.Page != p.Number {
				continue
			}
			if b.Start != next || b.End-b.Start > r.BlockBytes || !utf8.ValidString(b.Text) {
				t.Fatalf("coverage %#v", b)
			}
			covered.WriteString(b.Text)
			next = b.End
		}
		if covered.String() != p.Text || p.Text != fixturePages[p.Number-1] {
			t.Fatal("lost page text")
		}
	}
	hits, err := Search(d, "Appendix")
	if err != nil || len(hits) != 1 || hits[0].Page != 2 {
		t.Fatalf("appendix search %v %v", hits, err)
	}
	hits, err = Search(d, "中文")
	if err != nil || len(hits) != 2 {
		t.Fatalf("UTF8 search %v %v", hits, err)
	}
	tables, err := TablePages(d, "")
	if err != nil || len(tables) != 1 || tables[0].Text != d.Pages[0].Text {
		t.Fatalf("tables %v %v", tables, err)
	}
	e, err := ReadRange(d, 1, 0, len(d.Pages[0].Text))
	if err != nil || e.Quote != fixturePages[0] {
		t.Fatal("full-page read truncated")
	}
	at := strings.Index(d.Pages[0].Text, "中文")
	if _, err = ReadRange(d, 1, at+1, at+3); !errors.Is(err, library.ErrInvalid) {
		t.Fatal("accepted mid-rune range")
	}
	for _, b := range d.Blocks {
		got, err := BlockText(d, b.ID)
		if err != nil || got != b {
			t.Fatalf("block read %v", err)
		}
	}
}

func TestDownloadFailuresDoNotPublish(t *testing.T) {
	cases := []struct {
		name    string
		max     int64
		handler http.HandlerFunc
	}{
		{"status", 0, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }},
		{"wrong-mime", 0, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "%PDF-disguised")
		}},
		{"wrong-magic", 0, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/pdf")
			fmt.Fprint(w, "not a PDF")
		}},
		{"HTML", 0, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "<html>not a PDF</html>") }},
		{"content-length", 8, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "%PDF-01234567890123456789") }},
		{"stream-limit", 8, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/pdf")
			w.(http.Flusher).Flush()
			fmt.Fprint(w, "%PDF-01234567890123456789")
		}},
		{"redirect-path", 0, func(w http.ResponseWriter, req *http.Request) { http.Redirect(w, req, "/pdf/2501.01234v3", 302) }},
		{"redirect-host", 0, func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "http://example.invalid/pdf/2501.01234v2", 302)
		}},
		{"redirect-query", 0, func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "/pdf/2501.01234v2?download=1", 302)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := helperRepository(t, fixturePages)
			r.MaxBytes = tc.max
			s := servePDF(t, tc.handler)
			r.PDFBase = s.URL
			if _, err := r.Ensure(context.Background(), fixtureIdentity); err == nil {
				t.Fatal("expected download error")
			}
			if _, err := r.Load(context.Background(), fixtureIdentity); !errors.Is(err, library.ErrNotFound) {
				t.Fatalf("published failed document: %v", err)
			}
		})
	}
}
func TestDownloadContextAndCommandFailure(t *testing.T) {
	t.Run("cancel-download", func(t *testing.T) {
		r := helperRepository(t, fixturePages)
		s := servePDF(t, func(w http.ResponseWriter, req *http.Request) { <-req.Context().Done() })
		r.PDFBase = s.URL
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if _, err := r.Ensure(ctx, fixtureIdentity); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%v", err)
		}
	})
	for _, key := range []string{"DOCUMENT_COMMAND_FAIL", "DOCUMENT_COMMAND_SLEEP"} {
		t.Run(key, func(t *testing.T) {
			r := helperRepository(t, fixturePages)
			t.Setenv(key, "1")
			s := servePDF(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(tinyPDF(t)) })
			r.PDFBase = s.URL
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if _, err := r.Ensure(ctx, fixtureIdentity); err == nil {
				t.Fatal("expected command error")
			}
			if _, err := r.Load(context.Background(), fixtureIdentity); !errors.Is(err, library.ErrNotFound) {
				t.Fatalf("partial publication %v", err)
			}
		})
	}
}
func TestExactIdentityAndRedirectPolicy(t *testing.T) {
	for _, i := range []library.Identity{{Source: "arxiv", PaperID: "../../etc/passwd", Version: "v1"}, {Source: "arxiv", PaperID: "2501.01234", Version: ""}, {Source: "arxiv", PaperID: "2501.01234", Version: "v0"}, {Source: "other", PaperID: "2501.01234", Version: "v1"}, {Source: "arxiv", PaperID: "2501.01234?x=1", Version: "v1"}} {
		r := &Repository{Root: t.TempDir()}
		if _, err := r.Ensure(context.Background(), i); !errors.Is(err, library.ErrInvalid) {
			t.Fatalf("accepted %+v %v", i, err)
		}
	}
	for _, raw := range []string{"https://arxiv.org/pdf/2501.01234v1", "https://arxiv.org/?x=1", "https://user@arxiv.org/pdf", "https://arxiv.org/pdf#x"} {
		r := &Repository{Root: t.TempDir(), PDFBase: raw}
		if _, _, err := r.download(context.Background(), fixtureIdentity, 1024); err == nil {
			t.Fatalf("accepted base %s", raw)
		}
	}
	for _, raw := range []string{"https://arxiv.org/pdf/x", "https://export.arxiv.org/pdf/x", "https://static.arxiv.org/pdf/x"} {
		u, _ := url.Parse(raw)
		if !officialHost(u) {
			t.Fatalf("official host rejected %s", raw)
		}
	}
	for _, raw := range []string{"http://arxiv.org/pdf/x", "https://arxiv.org.evil.test/pdf/x", "https://arxiv.org:444/pdf/x", "https://cdn.evil.test/pdf/x"} {
		u, _ := url.Parse(raw)
		if officialHost(u) {
			t.Fatalf("unsafe host accepted %s", raw)
		}
	}
	t.Run("same-host-pdf-suffix", func(t *testing.T) {
		r := helperRepository(t, fixturePages)
		s := servePDF(t, func(w http.ResponseWriter, req *http.Request) {
			if strings.HasSuffix(req.URL.Path, ".pdf") {
				w.Write(tinyPDF(t))
				return
			}
			http.Redirect(w, req, req.URL.Path+".pdf", 302)
		})
		r.PDFBase = s.URL
		if _, err := r.Ensure(context.Background(), fixtureIdentity); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("old-id", func(t *testing.T) {
		r := helperRepository(t, fixturePages)
		i := library.Identity{Source: "arxiv", PaperID: "arxiv:hep-th/9901001", Version: "v3"}
		s := servePDF(t, func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path != "/pdf/hep-th/9901001v3" {
				t.Errorf("%s", req.URL.Path)
			}
			w.Write(tinyPDF(t))
		})
		r.PDFBase = s.URL
		if _, err := r.Ensure(context.Background(), i); err != nil {
			t.Fatal(err)
		}
	})
}
func TestQualityBlockedPersistsButNeverReady(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pages []string
	}{{"empty", []string{fixturePages[0], ""}}, {"low-text", []string{"Hello world"}}, {"replacement", []string{fixturePages[0] + "�"}}, {"invalid-utf8", []string{fixturePages[0] + string([]byte{0xff})}}, {"control", []string{fixturePages[0] + "\x01"}}} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Repository{Root: t.TempDir()}
			d, err := r.SaveFixture(fixtureIdentity, tc.pages)
			if !errors.Is(err, ErrQuality) || d.Quality != "blocked" {
				t.Fatalf("expected quality blocked %v %+v", err, d)
			}
			loaded, err := r.Load(context.Background(), fixtureIdentity)
			if tc.name == "invalid-utf8" {
				if !errors.Is(err, library.ErrNotFound) {
					t.Fatalf("published invalid UTF8: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrQuality) || loaded.ID != d.ID {
				t.Fatalf("blocked reload %v", err)
			}
		})
	}
}
func synthetic(t *testing.T) (*Repository, library.Document) {
	t.Helper()
	r := &Repository{Root: t.TempDir(), BlockBytes: 37}
	d, err := r.SaveFixture(fixtureIdentity, fixturePages)
	if err != nil {
		t.Fatal(err)
	}
	if d.Source.Kind != "synthetic" || d.Extractor != syntheticExtractor || strings.HasSuffix(d.Source.Path, ".pdf") {
		t.Fatal("fixture pretends to be PDF")
	}
	return r, d
}
func TestIntegrityTamper(t *testing.T) {
	for _, which := range []string{"original", "text", "manifest-offset", "manifest-page", "manifest-path", "manifest-identity", "manifest-block-remove", "manifest-hash", "manifest-ID", "manifest-quality"} {
		t.Run(which, func(t *testing.T) {
			r, d := synthetic(t)
			path := filepath.Join(r.Root, "documents", d.ID, "manifest.json")
			switch which {
			case "original":
				os.WriteFile(filepath.Join(r.Root, d.Source.Path), []byte("tampered"), 0600)
			case "text":
				os.WriteFile(filepath.Join(r.Root, d.Text.Path), []byte("tampered"), 0600)
			default:
				raw, _ := os.ReadFile(path)
				var m manifest
				json.Unmarshal(raw, &m)
				switch which {
				case "manifest-offset":
					m.Document.Blocks[0].Start = 1
				case "manifest-page":
					m.Document.Pages[0].Text += "tamper"
				case "manifest-path":
					m.Document.Source.Path = "../outside"
				case "manifest-identity":
					m.Document.Version = "v3"
				case "manifest-block-remove":
					m.Document.Blocks = m.Document.Blocks[1:]
				case "manifest-hash":
					m.Document.Text.SHA256 = strings.Repeat("0", 64)
				case "manifest-ID":
					m.Document.ID = strings.Repeat("0", 64)
				case "manifest-quality":
					m.Document.Quality = "blocked"
				}
				raw, _ = json.Marshal(m)
				os.WriteFile(path, raw, 0600)
			}
			if _, err := r.Load(context.Background(), d.ID); err == nil || errors.Is(err, ErrQuality) {
				t.Fatalf("accepted tamper %v", err)
			}
		})
	}
}
func TestPathsSymlinksAndExpectedDocument(t *testing.T) {
	for _, id := range []string{"../x", "/tmp/x", "documents/x", "a\\b", "not-a-hash"} {
		r := &Repository{Root: t.TempDir()}
		if _, err := r.Load(context.Background(), id); err == nil {
			t.Fatalf("accepted path %q", id)
		}
	}
	for _, which := range []string{"source", "text", "manifest", "directory", "index"} {
		t.Run(which, func(t *testing.T) {
			r, d := synthetic(t)
			target := d.Source.Path
			switch which {
			case "text":
				target = d.Text.Path
			case "manifest":
				target = "documents/" + d.ID + "/manifest.json"
			case "directory":
				target = "documents/" + d.ID
			case "index":
				target = "identities/" + identityID(fixtureIdentity) + ".json"
			}
			path := filepath.Join(r.Root, target)
			copy := path + "-original"
			if err := os.Rename(path, copy); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(copy, path); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Load(context.Background(), fixtureIdentity); err == nil {
				t.Fatal("accepted symlink")
			}
		})
	}
	r, d := synthetic(t)
	d.Pages[0].Text += "tamper"
	if _, err := r.Load(context.Background(), d); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("accepted external doc %v", err)
	}
}
func TestFixtureImmutableIdentity(t *testing.T) {
	r, d := synthetic(t)
	again, err := r.SaveFixture(fixtureIdentity, fixturePages)
	if err != nil || again.ID != d.ID {
		t.Fatalf("replay: %v", err)
	}
	if _, err = r.SaveFixture(fixtureIdentity, []string{fixturePages[0] + "different"}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("replaced identity: %v", err)
	}
	loaded, err := r.Load(context.Background(), fixtureIdentity)
	if err != nil || loaded.ID != d.ID {
		t.Fatalf("lost original: %v", err)
	}
}

func TestExplicitDamageAndMathematicsBoundary(t *testing.T) {
	t.Run("poppler-diagnostic", func(t *testing.T) {
		r := helperRepository(t, fixturePages)
		t.Setenv("DOCUMENT_COMMAND_WARNING", "1")
		s := servePDF(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(tinyPDF(t)) })
		r.PDFBase = s.URL
		if _, err := r.Ensure(context.Background(), fixtureIdentity); !errors.Is(err, ErrQuality) {
			t.Fatalf("accepted syntax damage: %v", err)
		}
		if _, err := r.Load(context.Background(), fixtureIdentity); !errors.Is(err, library.ErrNotFound) {
			t.Fatalf("published syntax damage: %v", err)
		}
	})
	if issues := quality([]library.Page{{Number: 1, Text: fixturePages[0] + " E=mc² ∑ α β ∫ ≤ ≥ " + strings.Repeat("", 8)}}); len(issues) > 0 {
		t.Fatalf("blocked mathematical text: %v", issues)
	}
	if issues := quality([]library.Page{{Number: 1, Text: fixturePages[0] + strings.Repeat("", 200)}}); len(issues) == 0 {
		t.Fatal("accepted excessive unmapped glyphs")
	}
}

func TestConcurrentImmutablePublication(t *testing.T) {
	r := &Repository{Root: t.TempDir(), BlockBytes: 37}
	type result struct {
		d   library.Document
		err error
	}
	results := make(chan result, 12)
	for n := 0; n < 12; n++ {
		go func() { d, err := r.SaveFixture(fixtureIdentity, fixturePages); results <- result{d, err} }()
	}
	var id string
	for n := 0; n < 12; n++ {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if id == "" {
			id = got.d.ID
		}
		if got.d.ID != id {
			t.Fatal("concurrent differing document")
		}
	}
	if _, err := r.Load(context.Background(), fixtureIdentity); err != nil {
		t.Fatal(err)
	}
}
func TestIncompleteIndexedDocumentFailsClosed(t *testing.T) {
	r, d := synthetic(t)
	if err := os.Remove(filepath.Join(r.Root, "documents", d.ID, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Load(context.Background(), fixtureIdentity); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("incomplete indexed document %v", err)
	}
	if _, err := r.Ensure(context.Background(), fixtureIdentity); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("re-downloaded corrupt index %v", err)
	}
}
func TestSearchOverlappingOccurrences(t *testing.T) {
	r := &Repository{Root: t.TempDir()}
	d, err := r.SaveFixture(fixtureIdentity, []string{fixturePages[0] + "aaaa"})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := Search(d, "aaa")
	if err != nil || len(hits) != 2 {
		t.Fatalf("overlapping search %v %v", hits, err)
	}
}

// 可选真实 Poppler 验收在运行镜像内执行，不访问网络或生产卷。
// 使用仓库中结构有效的 PDF 检查真实提取。
func TestPopplerFixture(t *testing.T) {
	if _, err := exec.LookPath("pdfinfo"); err != nil {
		t.Skip("Poppler acceptance requires runtime image")
	}
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("Poppler acceptance requires runtime image")
	}
	r := &Repository{Root: t.TempDir()}
	pages, err := r.extract(context.Background(), "testdata/minimal.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || !strings.Contains(pages[0].Text, "Table 1") || !strings.Contains(pages[1].Text, "Appendix") {
		t.Fatalf("unexpected extraction: %#v", pages)
	}
	if issues := quality(pages); len(issues) > 0 {
		t.Fatalf("fixture quality %v", issues)
	}
	root, err := r.root()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	d, err := r.publish(context.Background(), root, fixtureIdentity, tinyPDF(t), fixtureIdentity.PDFURL(), pages, 37, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Load(context.Background(), d.ID); err != nil {
		t.Fatal(err)
	}
}
