package document

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/shichao-wang/paper-digest/internal/library"
)

// BlockText 返回完整块，坐标为页内 UTF-8 字节偏移。
func BlockText(doc library.Document, blockID string) (library.Block, error) {
	for _, b := range doc.Blocks {
		if b.ID == blockID {
			e, err := ReadRange(doc, b.Page, b.Start, b.End)
			if err != nil {
				return library.Block{}, err
			}
			if e.Quote != b.Text || digest([]byte(b.Text)) != b.SHA256 {
				return library.Block{}, ErrIntegrity
			}
			return b, nil
		}
	}
	return library.Block{}, library.ErrNotFound
}

// ReadRange 不截断内容，start/end 是指定页内的字节偏移。
// 跨块证据不指定 BlockID。
func ReadRange(doc library.Document, page, start, end int) (library.Evidence, error) {
	if page < 1 || page > len(doc.Pages) || doc.Pages[page-1].Number != page {
		return library.Evidence{}, library.ErrInvalid
	}
	p := doc.Pages[page-1]
	if !utf8.ValidString(p.Text) || digest([]byte(p.Text)) != p.SHA256 {
		return library.Evidence{}, ErrIntegrity
	}
	if start < 0 || end <= start || end > len(p.Text) || !boundary(p.Text, start) || !boundary(p.Text, end) {
		return library.Evidence{}, library.ErrInvalid
	}
	quote := p.Text[start:end]
	e := library.Evidence{ID: fmt.Sprintf("%s:p%d:%d:%d:%s", doc.ID, page, start, end, digest([]byte(quote))), DocumentID: doc.ID, Version: doc.Version, Page: page, Start: start, End: end, Quote: quote}
	for _, b := range doc.Blocks {
		if b.Page == page && b.Start <= start && b.End >= end {
			e.BlockID = b.ID
			break
		}
	}
	return e, nil
}
func boundary(s string, n int) bool {
	return n == len(s) || (n >= 0 && n < len(s) && utf8.RuneStart(s[n]))
}

// Search 返回所有页面中的精确匹配，区分大小写并包含重叠结果。
// 搜索覆盖参考文献与附录，无隐式结果数量限制。
func Search(doc library.Document, query string) ([]library.Evidence, error) {
	if query == "" || !utf8.ValidString(query) {
		return nil, library.ErrInvalid
	}
	hits := make([]library.Evidence, 0)
	for _, p := range doc.Pages {
		if !utf8.ValidString(p.Text) || digest([]byte(p.Text)) != p.SHA256 {
			return nil, ErrIntegrity
		}
		for start := 0; start < len(p.Text); {
			match := strings.Index(p.Text[start:], query)
			if match < 0 {
				break
			}
			offset := start + match
			e, err := ReadRange(doc, p.Number, offset, offset+len(query))
			if err != nil {
				return nil, err
			}
			hits = append(hits, e)
			_, width := utf8.DecodeRuneInString(p.Text[offset:])
			start = offset + width
		}
	}
	return hits, nil
}

// TablePages 返回含查询文字的完整页面，空查询匹配 Table 或表。
// 本方法不推断表格结构，也不执行 OCR。
func TablePages(doc library.Document, query string) ([]library.Page, error) {
	if !utf8.ValidString(query) {
		return nil, library.ErrInvalid
	}
	pages := make([]library.Page, 0)
	for _, p := range doc.Pages {
		if !utf8.ValidString(p.Text) || digest([]byte(p.Text)) != p.SHA256 {
			return nil, ErrIntegrity
		}
		match := strings.Contains(p.Text, query)
		if query == "" {
			match = strings.Contains(strings.ToLower(p.Text), "table") || strings.Contains(p.Text, "表")
		}
		if match {
			pages = append(pages, p)
		}
	}
	return pages, nil
}
func (r *Repository) BlockText(ctx context.Context, doc library.Document, blockID string) (library.Block, error) {
	verified, err := r.Load(ctx, doc)
	if err != nil {
		return library.Block{}, err
	}
	return BlockText(verified, blockID)
}
func (r *Repository) ReadRange(ctx context.Context, doc library.Document, page, start, end int) (library.Evidence, error) {
	verified, err := r.Load(ctx, doc)
	if err != nil {
		return library.Evidence{}, err
	}
	return ReadRange(verified, page, start, end)
}
func (r *Repository) Search(ctx context.Context, doc library.Document, query string) ([]library.Evidence, error) {
	verified, err := r.Load(ctx, doc)
	if err != nil {
		return nil, err
	}
	return Search(verified, query)
}
func (r *Repository) TablePages(ctx context.Context, doc library.Document, query string) ([]library.Page, error) {
	verified, err := r.Load(ctx, doc)
	if err != nil {
		return nil, err
	}
	return TablePages(verified, query)
}
