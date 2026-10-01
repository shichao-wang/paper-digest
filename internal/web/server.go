package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/state"
)

type Options struct {
	DeliveryEnabled bool
}

// New 使用共享 Store 与静态资源目录；调用方管理生命周期，创建 handler 不打开数据库。
func New(store *state.Store, staticFS fs.FS, options ...Options) (http.Handler, error) {
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
	s := &server{store: store, staticFS: staticFS, files: http.FileServer(http.FS(staticFS))}
	if len(options) > 0 {
		s.deliveryEnabled = options[0].DeliveryEnabled
	}
	return http.HandlerFunc(s.serveHTTP), nil
}

type server struct {
	store           *state.Store
	staticFS        fs.FS
	files           http.Handler
	deliveryEnabled bool
}

func (s *server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		allowed := "GET"
		settings := r.URL.Path == "/api/settings/webhook"
		if settings {
			allowed = "GET, PUT"
		}
		if r.Method != http.MethodGet && !(settings && r.Method == http.MethodPut) {
			w.Header().Set("Allow", allowed)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		if settings {
			s.webhookSettings(w, r)
			return
		}
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
		case errors.Is(err, state.ErrInvalidQuery):
			writeError(w, http.StatusBadRequest, "invalid query parameters")
		case errors.Is(err, state.ErrJobNotFound), errors.Is(err, state.ErrPaperNotFound):
			writeError(w, http.StatusNotFound, "not found")
		default:
			writeError(w, http.StatusServiceUnavailable, "service unavailable")
		}
		return
	}
	writeJSON(w, http.StatusOK, value)
}

const maxWebhookBody = 16 << 10

func (s *server) webhookSettings(w http.ResponseWriter, r *http.Request) {
	var webhook string
	var err error
	if r.Method == http.MethodPut {
		if !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "same-origin request required")
			return
		}
		mediaType, _, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if parseErr != nil || mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "application/json required")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
		webhook, err = decodeWebhook(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid webhook settings")
			return
		}
		webhook = strings.TrimSpace(webhook)
		if webhook != "" && config.ValidateWebhookURL(webhook) != nil {
			writeError(w, http.StatusBadRequest, "invalid webhook settings")
			return
		}
		err = s.store.SetWebhook(r.Context(), job.Topic, webhook)
	} else {
		webhook, err = s.store.Webhook(r.Context(), job.Topic)
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Topic           string `json:"topic"`
		Configured      bool   `json:"configured"`
		DeliveryEnabled bool   `json:"deliveryEnabled"`
	}{job.Topic, webhook != "", s.deliveryEnabled})
}

// 精确字段名、单个对象、无重复字段，避免宽松解码把误填字段当成清除。
func decodeWebhook(body io.Reader) (string, error) {
	invalid := errors.New("invalid webhook settings")
	decoder := json.NewDecoder(body)
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", invalid
	}
	if !decoder.More() {
		return "", invalid
	}
	key, err := decoder.Token()
	if err != nil || key != "webhookURL" {
		return "", invalid
	}
	var value *string
	if err := decoder.Decode(&value); err != nil || value == nil || decoder.More() {
		return "", invalid
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return "", invalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", invalid
	}
	return *value, nil
}

func localSettingsHost(authority string) bool {
	host := authority
	if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(authority, "["), "]")
	} else if strings.Contains(authority, ":") {
		var port string
		var err error
		host, port, err = net.SplitHostPort(authority)
		if err != nil {
			return false
		}
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil || number == 0 {
			return false
		}
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(r *http.Request) bool {
	// Host 必须独立限定为本机，不能只相信 Origin 与 Host 的相互一致。
	// 不做 DNS 解析，防止攻击者域名重绑定到回环地址后修改设置。
	if !localSettingsHost(r.Host) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	origins, exists := r.Header["Origin"]
	if !exists {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	return err == nil && (origin.Scheme == "http" || origin.Scheme == "https") && origin.Host != "" &&
		origin.User == nil && origin.Path == "" && origin.RawQuery == "" && !origin.ForceQuery && origin.Fragment == "" && !strings.Contains(origins[0], "#") &&
		strings.EqualFold(origin.Host, r.Host)
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
