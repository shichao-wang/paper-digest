package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/document"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/state"
)

// New 使用共享 Store 与静态资源目录；调用方管理生命周期，创建 handler 不打开数据库。
func New(store *state.Store, staticFS fs.FS) (http.Handler, error) {
	return NewWithDocuments(store, staticFS, nil)
}

// NewWithDocuments 为生产服务注入文档仓库，证据读取会核对磁盘文件、hash 和持久化文档。
// repository 为 nil 时保持 New 的兼容行为，仅核对 SQLite 中的 page/block 内容。
// 两种构造方式都只读取已有文档，不下载、提取或启动模型。
func NewWithDocuments(store *state.Store, staticFS fs.FS, repository *document.Repository) (http.Handler, error) {
	if store == nil || staticFS == nil {
		return nil, errors.New("web: store and static filesystem are required")
	}
	info, err := fs.Stat(staticFS, "index.html")
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("web: static index.html is missing or invalid")
	}
	_, err = fs.ReadFile(staticFS, "index.html")
	if err != nil {
		return nil, fmt.Errorf("web: read static index.html: %w", err)
	}
	s := &server{store: store, staticFS: staticFS, files: http.FileServer(http.FS(staticFS)), documents: repository}
	return http.HandlerFunc(s.serveHTTP), nil
}

type server struct {
	store     *state.Store
	staticFS  fs.FS
	files     http.Handler
	documents *document.Repository
}

func (s *server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		s.api(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		s.serveIndex(w, r)
		return
	}
	// 拒绝非规范路径，防止路径清理越出资源根目录。
	if !fs.ValidPath(name) || path.Clean(name) != name {
		http.NotFound(w, r)
		return
	}
	info, err := fs.Stat(s.staticFS, name)
	if err == nil && info.Mode().IsRegular() {
		s.files.ServeHTTP(w, r)
		return
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	// 仅无扩展名的 HTML 导航回退至 SPA；缺失资源和 API 路径仍返回 404。
	if path.Ext(name) == "" && !strings.HasPrefix(name, "assets/") && acceptsHTML(r.Header.Get("Accept")) {
		s.serveIndex(w, r)
		return
	}
	http.NotFound(w, r)
}

func acceptsHTML(accept string) bool {
	for _, part := range strings.Split(accept, ",") {
		parts := strings.Split(strings.TrimSpace(part), ";")
		if parts[0] != "text/html" {
			continue
		}
		allowed := true
		for _, param := range parts[1:] {
			if strings.HasPrefix(strings.TrimSpace(param), "q=") {
				q, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(param), "q="), 64)
				allowed = err == nil && q > 0
			}
		}
		if allowed {
			return true
		}
	}
	return false
}

func (s *server) serveIndex(w http.ResponseWriter, r *http.Request) {
	// 构建可能替换带 hash 的资源，入口必须与磁盘上的当前构建保持一致。
	index, err := fs.ReadFile(s.staticFS, "index.html")
	if err != nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(index)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(index)
	}
}

func (s *server) api(w http.ResponseWriter, r *http.Request) {
	var value any
	var err error
	switch r.URL.Path {
	case "/api/health":
		if err = s.store.Health(r.Context()); err == nil {
			value = struct {
				Status string `json:"status"`
			}{"ok"}
		}
	case "/api/library/papers":
		var page state.PageQuery
		page, err = pagination(r.URL.Query())
		if err == nil {
			query := r.URL.Query()
			var result library.ResultPage
			result, err = s.store.BrowseLibrary(r.Context(), library.Query{
				Page: page.Page, PageSize: page.PageSize, Q: query.Get("q"),
				Batch: query.Get("batch"), Topic: query.Get("topic"),
				Relevance: query.Get("relevance"), Status: query.Get("status"),
			})
			if err == nil {
				value = libraryPageDTO(result)
			}
		}
	case "/api/library/papers/detail":
		var id library.Identity
		id, err = libraryIdentity(r.URL.Query())
		if err == nil {
			var detail library.Detail
			detail, err = s.store.LibraryDetail(r.Context(), id)
			if err == nil {
				value = libraryDetailDTO(detail)
			}
		}
	case "/api/library/status":
		value, err = s.store.LibraryStatus(r.Context())
	case "/api/library/evidence":
		value, err = s.libraryEvidence(r.Context(), r.URL.Query())
	case "/api/papers":
		var page state.PageQuery
		page, err = pagination(r.URL.Query())
		if err == nil {
			value, err = s.store.BrowsePapers(r.Context(), job.Topic, state.PaperQuery{PageQuery: page, Q: r.URL.Query().Get("q"), Date: r.URL.Query().Get("date"), Summary: r.URL.Query().Get("summary")})
		}
	case "/api/papers/detail":
		value, err = s.store.PaperDetail(r.Context(), job.Topic, r.URL.Query().Get("id"), r.URL.Query().Get("date"))
	case "/api/digests":
		var page state.PageQuery
		page, err = pagination(r.URL.Query())
		if err == nil {
			value, err = s.store.BrowseDigests(r.Context(), job.Topic, page)
		}
	default:
		if strings.HasPrefix(r.URL.Path, "/api/digests/") && !strings.Contains(strings.TrimPrefix(r.URL.Path, "/api/digests/"), "/") {
			value, err = s.store.DigestDetail(r.Context(), job.Topic, strings.TrimPrefix(r.URL.Path, "/api/digests/"))
		} else {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, state.ErrInvalidQuery), errors.Is(err, library.ErrInvalid):
			writeError(w, http.StatusBadRequest, "invalid query parameters")
		case errors.Is(err, state.ErrJobNotFound), errors.Is(err, state.ErrPaperNotFound), errors.Is(err, library.ErrNotFound):
			writeError(w, http.StatusNotFound, "not found")
		default:
			writeError(w, http.StatusServiceUnavailable, "service unavailable")
		}
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func pagination(values url.Values) (state.PageQuery, error) {
	page := state.PageQuery{Page: 1, PageSize: state.DefaultPageSize}
	for _, field := range []struct {
		name string
		dest *int
	}{{"page", &page.Page}, {"pageSize", &page.PageSize}} {
		if values.Has(field.name) {
			n, err := strconv.Atoi(values.Get(field.name))
			if err != nil || n < 1 {
				return state.PageQuery{}, state.ErrInvalidQuery
			}
			*field.dest = n
		}
	}
	if page.PageSize > state.MaxPageSize || page.Page-1 > int(^uint(0)>>1)/page.PageSize {
		return state.PageQuery{}, state.ErrInvalidQuery
	}
	return page, nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
