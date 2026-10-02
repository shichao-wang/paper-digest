package web

import (
	"context"
	"net/url"
	"strings"

	"github.com/shichao-wang/paper-digest/internal/document"
	"github.com/shichao-wang/paper-digest/internal/library"
)

// API 使用显式任务投影，避免恢复用 checkpoint 和 lease token 随领域类型泄露。
type libraryTask struct {
	ID int64 `json:"id"`
	library.Identity
	Stage         string `json:"stage"`
	Generation    int    `json:"generation"`
	Status        string `json:"status"`
	Attempt       int    `json:"attempt"`
	NextAttemptAt string `json:"next_attempt_at"`
	LeaseUntil    string `json:"lease_until"`
	Error         string `json:"error"`
}

type libraryItem struct {
	Version    library.Version    `json:"version"`
	Relevance  *library.Relevance `json:"relevance"`
	AnalysisID int64              `json:"analysis_id"`
	Tasks      []libraryTask      `json:"tasks"`
}

type libraryPage struct {
	Items    []libraryItem `json:"items"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"pageSize"`
}

// 文档定位和质量仍可浏览；整页与整块正文仅通过 evidence 按需读取。
type libraryDocument struct {
	ID string `json:"id"`
	library.Identity
	Source    library.Artifact       `json:"source_file"`
	Text      library.Artifact       `json:"text_file"`
	Extractor string                 `json:"extractor"`
	Quality   string                 `json:"quality"`
	Issues    []string               `json:"issues"`
	Pages     []libraryPageLocation  `json:"pages"`
	Blocks    []libraryBlockLocation `json:"blocks"`
}

type libraryPageLocation struct {
	Number int    `json:"number"`
	SHA256 string `json:"sha256"`
}

type libraryBlockLocation struct {
	ID     string `json:"id"`
	Page   int    `json:"page"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	SHA256 string `json:"sha256"`
}

type libraryDetail struct {
	libraryItem
	Versions         []library.Identity  `json:"versions"`
	RelevanceHistory []library.Relevance `json:"relevance_history"`
	Documents        []libraryDocument   `json:"documents"`
	Analysis         *library.Analysis   `json:"analysis"`
	Comparison       *library.Comparison `json:"comparison"`
	Runs             []library.Run       `json:"runs"`
}

func libraryItemDTO(item library.ListItem) libraryItem {
	result := libraryItem{Version: item.Version, Relevance: item.Relevance, AnalysisID: item.AnalysisID, Tasks: make([]libraryTask, 0, len(item.Tasks))}
	for _, task := range item.Tasks {
		result.Tasks = append(result.Tasks, libraryTask{
			ID: task.ID, Identity: task.Identity, Stage: task.Stage,
			Generation: task.Generation, Status: task.Status, Attempt: task.Attempt,
			NextAttemptAt: task.NextAttemptAt, LeaseUntil: task.LeaseUntil, Error: task.Error,
		})
	}
	return result
}

func libraryPageDTO(page library.ResultPage) libraryPage {
	result := libraryPage{Items: make([]libraryItem, 0, len(page.Items)), Total: page.Total, Page: page.Page, PageSize: page.PageSize}
	for _, item := range page.Items {
		result.Items = append(result.Items, libraryItemDTO(item))
	}
	return result
}

func libraryDetailDTO(detail library.Detail) libraryDetail {
	result := libraryDetail{
		libraryItem: libraryItemDTO(detail.ListItem), Versions: detail.Versions,
		RelevanceHistory: detail.RelevanceHistory, Documents: make([]libraryDocument, 0, len(detail.Documents)),
		Analysis: detail.Analysis, Comparison: detail.Comparison, Runs: detail.Runs,
	}
	for _, doc := range detail.Documents {
		projected := libraryDocument{
			ID: doc.ID, Identity: doc.Identity, Source: doc.Source, Text: doc.Text,
			Extractor: doc.Extractor, Quality: doc.Quality, Issues: doc.Issues,
			Pages: make([]libraryPageLocation, 0, len(doc.Pages)), Blocks: make([]libraryBlockLocation, 0, len(doc.Blocks)),
		}
		for _, page := range doc.Pages {
			projected.Pages = append(projected.Pages, libraryPageLocation{Number: page.Number, SHA256: page.SHA256})
		}
		for _, block := range doc.Blocks {
			projected.Blocks = append(projected.Blocks, libraryBlockLocation{ID: block.ID, Page: block.Page, Start: block.Start, End: block.End, SHA256: block.SHA256})
		}
		result.Documents = append(result.Documents, projected)
	}
	return result
}

func libraryIdentity(values url.Values) (library.Identity, error) {
	id := library.Identity{Source: "arxiv", PaperID: strings.TrimPrefix(values.Get("id"), "arxiv:"), Version: values.Get("version")}
	return id, id.Validate()
}

func (s *server) libraryEvidence(ctx context.Context, values url.Values) (library.Block, error) {
	id, err := libraryIdentity(values)
	if err != nil {
		return library.Block{}, err
	}
	documentID, blockID := values.Get("document"), values.Get("block")
	if strings.TrimSpace(documentID) == "" || strings.TrimSpace(blockID) == "" {
		return library.Block{}, library.ErrInvalid
	}
	docs, err := s.store.Documents(ctx, id)
	if err != nil {
		return library.Block{}, err
	}
	for _, doc := range docs {
		if doc.ID != documentID {
			continue
		}
		storedID := doc.Identity
		storedID.PaperID = strings.TrimPrefix(storedID.PaperID, "arxiv:")
		if storedID != id {
			return library.Block{}, document.ErrIntegrity
		}
		if s.documents != nil {
			return s.documents.BlockText(ctx, doc, blockID)
		}
		return document.BlockText(doc, blockID)
	}
	return library.Block{}, library.ErrNotFound
}
