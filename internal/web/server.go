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
	"github.com/shichao-wang/paper-digest/internal/document"
	"github.com/shichao-wang/paper-digest/internal/job"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/state"
)

type Options struct {
	DeliveryEnabled bool
	Topics          []config.Topic
}

type topicRecord struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	DeliveryEnabled bool   `json:"deliveryEnabled"`
}

// New 使用共享 Store 与静态资源目录；调用方管理生命周期，创建 handler 不打开数据库。
func New(store *state.Store, staticFS fs.FS, options ...Options) (http.Handler, error) {
	return NewWithDocuments(store, staticFS, nil, options...)
}

// NewWithDocuments 为生产服务注入文档仓库，证据读取会核对磁盘文件、hash 和持久化文档。
// repository 为 nil 时保持 New 的兼容行为，仅核对 SQLite 中的 page/block 内容。
// 两种构造方式都只读取已有文档，不下载、提取或启动模型。
func NewWithDocuments(store *state.Store, staticFS fs.FS, repository *document.Repository, options ...Options) (http.Handler, error) {
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
	s := &server{store: store, staticFS: staticFS, files: http.FileServer(http.FS(staticFS)), topics: make([]topicRecord, 0)}
	if repository != nil {
		s.documents = repository
	}
	// 旧调用方省略 Options 时保留 RAS；显式传入空目录则不提供默认主题。
	settings := Options{Topics: []config.Topic{{ID: job.Topic}}}
	if len(options) > 0 {
		settings = options[0]
	}
	for _, topic := range settings.Topics {
		name := topic.ID
		if topic.ID == job.Topic {
			name = "推荐 / 广告 / 搜索"
		}
		s.topics = append(s.topics, topicRecord{ID: topic.ID, Name: name, DeliveryEnabled: settings.DeliveryEnabled && topic.ID == job.Topic})
		if s.defaultTopic == "" || topic.ID == job.Topic {
			s.defaultTopic = topic.ID
		}
	}
	return http.HandlerFunc(s.serveHTTP), nil
}

const evidenceTimeout = 2 * time.Minute

type evidenceRepository interface {
	BlockText(context.Context, library.Document, string) (library.Block, error)
}

type server struct {
	store        *state.Store
	staticFS     fs.FS
	files        http.Handler
	documents    evidenceRepository
	topics       []topicRecord
	defaultTopic string
}

func (s *server) selectTopic(w http.ResponseWriter, r *http.Request) (topicRecord, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid query parameters")
		return topicRecord{}, false
	}
	id := s.defaultTopic
	if requested, exists := values["topic"]; exists {
		if len(requested) != 1 || strings.TrimSpace(requested[0]) == "" {
			writeError(w, http.StatusBadRequest, "invalid topic parameter")
			return topicRecord{}, false
		}
		id = requested[0]
	}
	for _, topic := range s.topics {
		if topic.ID == id {
			return topic, true
		}
	}
	writeError(w, http.StatusNotFound, "topic not found")
	return topicRecord{}, false
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
		// 证据读取会重新校验完整文档，单独给出文件核查预算。
		timeout := 5 * time.Second
		if r.URL.Path == "/api/library/evidence" {
			timeout = evidenceTimeout
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		if r.URL.Path == "/api/library/evidence" {
			deadline, _ := ctx.Deadline()
			if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
				writeError(w, http.StatusServiceUnavailable, "service unavailable")
				return
			}
		}
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
	var topic topicRecord
	digestDetail := strings.HasPrefix(r.URL.Path, "/api/digests/") && !strings.Contains(strings.TrimPrefix(r.URL.Path, "/api/digests/"), "/")
	if r.URL.Path == "/api/papers" || r.URL.Path == "/api/papers/detail" || r.URL.Path == "/api/digests" || digestDetail {
		var ok bool
		topic, ok = s.selectTopic(w, r)
		if !ok {
			return
		}
	}
	switch r.URL.Path {
	case "/api/topics":
		value = struct {
			Items []topicRecord `json:"items"`
		}{s.topics}
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
			value, err = s.store.BrowsePapers(r.Context(), topic.ID, state.PaperQuery{PageQuery: page, Q: r.URL.Query().Get("q"), Date: r.URL.Query().Get("date"), Summary: r.URL.Query().Get("summary")})
		}
	case "/api/papers/detail":
		value, err = s.store.PaperDetail(r.Context(), topic.ID, r.URL.Query().Get("id"), r.URL.Query().Get("date"))
	case "/api/digests":
		var page state.PageQuery
		page, err = pagination(r.URL.Query())
		if err == nil {
			value, err = s.store.BrowseDigests(r.Context(), topic.ID, page)
		}
	default:
		if digestDetail {
			value, err = s.store.DigestDetail(r.Context(), topic.ID, strings.TrimPrefix(r.URL.Path, "/api/digests/"))
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

const maxWebhookBody = 16 << 10

func (s *server) webhookSettings(w http.ResponseWriter, r *http.Request) {
	topic, ok := s.selectTopic(w, r)
	if !ok {
		return
	}
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
		err = s.store.SetWebhook(r.Context(), topic.ID, webhook)
	} else {
		webhook, err = s.store.Webhook(r.Context(), topic.ID)
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Topic           string `json:"topic"`
		Configured      bool   `json:"configured"`
		DeliveryEnabled bool   `json:"deliveryEnabled"`
	}{topic.ID, webhook != "", topic.DeliveryEnabled})
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
