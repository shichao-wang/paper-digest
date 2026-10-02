package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shichao-wang/paper-digest/internal/config"
	"github.com/shichao-wang/paper-digest/internal/digest"
	"github.com/shichao-wang/paper-digest/internal/document"
	"github.com/shichao-wang/paper-digest/internal/library"
	"github.com/shichao-wang/paper-digest/internal/papers"
	"github.com/shichao-wang/paper-digest/internal/state"
)

const demoLabel = "合成演示（synthetic_demo，非真实论文）"
const demoDate = "2026-09-30"

// seedDemo creates an offline fixture in two previously nonexistent paths. It
// does not load credentials, construct a worker/client, fetch arXiv or send a
// message. The caller supplies and persists its isolated configuration.
func seedDemo(ctx context.Context, cfg config.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.Delivery.Enabled || cfg.Library.CollectEnabled || cfg.Library.ProcessEnabled {
		return errors.New("demo 要求 delivery.enabled、library.collect_enabled 和 library.process_enabled 全部为 false")
	}
	if cfg.Anthropic.APIKey != "" {
		return errors.New("demo 配置不能包含 API key")
	}
	for _, topic := range cfg.Topics {
		if topic.WebhookURL != "" {
			return errors.New("demo 配置不能包含任何 webhook")
		}
	}
	if len(cfg.Topics) == 0 || strings.TrimSpace(cfg.Topics[0].ID) == "" {
		return errors.New("demo 配置至少需要一个无 webhook 的主题")
	}
	dbPath, err := demoNewPath(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("demo 数据库: %w", err)
	}
	docPath, err := demoNewPath(cfg.Library.DocumentDir)
	if err != nil {
		return fmt.Errorf("demo 文档目录: %w", err)
	}
	if dbPath == docPath || demoWithin(dbPath, docPath) || demoWithin(docPath, dbPath) {
		return errors.New("demo 数据库与文档目录不能相同或互相包含")
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm", docPath} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("demo 目标已存在，拒绝覆写: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, path := range []string{filepath.Dir(dbPath), filepath.Dir(docPath)} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
	}
	// Reserve both destinations exclusively before opening SQLite. A concurrent
	// creator cannot make this seed silently reuse an existing database or root.
	if err := os.Mkdir(docPath, 0700); err != nil {
		return fmt.Errorf("创建独立 demo 文档目录: %w", err)
	}
	f, err := os.OpenFile(dbPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		_ = os.Remove(docPath) // succeeds only while the newly created directory is empty
		return fmt.Errorf("创建独立 demo 数据库: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	store, err := state.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	repo := &document.Repository{Root: docPath}
	if err := demoSeedLibrary(ctx, store, repo); err != nil {
		return fmt.Errorf("保存合成演示论文库: %w", err)
	}
	if err := demoSeedLegacy(ctx, store, cfg.Topics[0].ID); err != nil {
		return fmt.Errorf("保存合成演示旧日报: %w", err)
	}
	return nil
}

func demoWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func demoNewPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return "", errors.New("必须指定新的本地文件路径")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for current := abs; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("demo 路径不能包含符号链接")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return abs, nil
}

func demoVersions() []library.Version {
	var versions []library.Version
	add := func(n int, version string) {
		id := library.Identity{Source: "arxiv", PaperID: fmt.Sprintf("2609.%05d", n), Version: version}
		topics := []string{"长尾推荐的离线评估", "广告排序的校准实验", "检索候选集的覆盖分析"}
		title := fmt.Sprintf("%s · %02d · %s · %s", demoLabel, n, topics[(n-1)%len(topics)], version)
		versions = append(versions, library.Version{
			Identity: id, Title: title, Authors: []string{"演示作者甲（虚构）", "演示作者乙（虚构）"},
			Abstract:        "本条目完全由本机合成，只演示筛选、全文证据和版本比较。标题、作者、实验结果与正文均为虚构，不代表 arXiv 收录或真实研究结论。",
			PrimaryCategory: "cs.IR", Categories: []string{"cs.IR", "cs.LG"},
			PublishedAt: "2026-09-01T08:00:00Z", UpdatedAt: "2026-09-30T08:00:00Z",
			Comment:        demoLabel + "；文档是 SaveFixture 合成文本，不是下载或提取的 PDF。",
			AuthorKeywords: []string{"合成实验", "离线评估", topics[(n-1)%len(topics)]}, Origin: "synthetic_demo",
			AnnouncementDate: demoDate, CapturedAt: "2026-09-30T09:00:00Z", MetadataVerified: false,
			MetadataArtifacts: []library.Artifact{},
		})
	}
	for _, version := range []string{"v1", "v2", "v9", "v10"} {
		add(1, version)
	}
	for _, n := range []int{2, 3} {
		add(n, "v1")
		add(n, "v2")
	}
	for n := 4; n <= 22; n++ {
		add(n, "v1")
	}
	// Keep the queued analysis last so ClaimTask can demonstrate a task that has
	// never run without reaching into storage internals to select a specific ID.
	for _, n := range []int{24, 25, 23} {
		add(n, "v1")
	}
	return versions
}

func demoPages(v library.Version) []string {
	metric := "0.420"
	if v.Number() > 1 {
		metric = "0.460"
	}
	return []string{
		fmt.Sprintf("%s\n%s\n第一节：问题与动机\n研究问题：合成候选集中长尾内容曝光不足，需要同时考虑推荐、广告与检索的覆盖。研究动机：单一相关性分数可能掩盖低频内容，需要可复核的离线评估。\n作者关键词：合成实验、离线评估、候选覆盖。资源说明：本演示未发布代码、数据或外部资源链接。\n主题判定说明：推荐、广告和检索是合成研究的直接对象；控制条目仅讨论虚构材料分类，与这些业务无关；信息不足条目缺少候选生成或线上排序说明，相关性待确认。", demoLabel, v.Title),
		fmt.Sprintf("%s\n第二节：方法与实验\n方法：演示方法甲在合成候选集上组合相关性分数与覆盖奖励，固定随机种子划分训练、验证和测试集。贡献：公开完整的演示评估步骤，区分作者陈述与阅读者推断。\n结果行：数据集演示数据甲，指标归一化折损累计增益，方法演示方法甲，基线演示基线乙，数值%s，单位无量纲，设置固定候选集。所有数值均为合成样例，不能用作模型效果证据。\n本版本为%s，后续版本在演示候选集上增加覆盖奖励的消融实验；这只是界面比较样例。", demoLabel, metric, v.Version),
		demoLabel + "\n第三节：局限与应用\n局限：只评估合成数据，没有线上用户、真实广告收入或生产流量；尚未报告在线实验、统计显著性与部署成本。\n作者声称：可用于演示推荐候选的离线检查，前提是固定候选集与明确的覆盖指标，计算代价为演示级的小批量运行。\n阅读者推断：可作为广告和搜索候选覆盖检查的教学材料，必须重新验证真实业务数据，并由业务方评估训练与服务成本；不能直接推断线上收益。\n结论：此文档保留全部合成正文、分页与字节定位，仅用于验证阅读库展示、证据回跳、任务状态和备份恢复。",
	}
}

func demoEvidence(d library.Document) []library.Evidence {
	var evidence []library.Evidence
	for n, block := range d.Blocks {
		evidence = append(evidence, library.Evidence{
			ID: fmt.Sprintf("%s-演示证据-%d", d.Version, n+1), DocumentID: d.ID, Version: d.Version,
			BlockID: block.ID, Section: fmt.Sprintf("合成正文第%d节", block.Page),
			Page: block.Page, Start: block.Start, End: block.End, Quote: block.Text,
		})
	}
	return evidence
}

func demoRelevance(v library.Version, evidence []library.Evidence) library.Relevance {
	r := library.Relevance{Level: "direct", DirectlyRelated: true, Topics: []string{[]string{"recommendation", "advertising", "search"}[v.Number()%3]},
		Rationale: "合成正文直接讨论推荐、广告或检索候选的离线评估；该判断只用于演示业务主题筛选。", ExtractedKeywords: []string{"合成样例", "候选覆盖", "离线评估"}, EvidenceIDs: []string{evidence[0].ID}}
	if v.PaperID == "2609.00020" || v.PaperID == "2609.00021" {
		r.Level, r.DirectlyRelated, r.Topics = "unrelated", false, []string{}
		r.Rationale = "合成控制条目只讨论虚构材料分类，未涉及推荐、广告或搜索场景，故判定不相关。"
	}
	if v.PaperID == "2609.00018" || v.PaperID == "2609.00019" {
		r.Level, r.DirectlyRelated, r.Topics = "uncertain", false, []string{}
		r.Rationale = "合成信息不足条目未说明候选生成、排序或线上使用场景，现有文本不足以判断业务相关性。"
	}
	return r
}

func demoAnalysis(v library.Version, d library.Document, r library.Relevance) library.Analysis {
	ev := demoEvidence(d)
	claim := func(text string, page int) library.Claim {
		return library.Claim{Text: text, EvidenceIDs: []string{ev[page-1].ID}}
	}
	problem, motivation, method := claim("合成候选集存在长尾内容曝光不足的问题。", 1), claim("需要可复核的离线覆盖评估，避免单一相关性分数掩盖低频内容。", 1), claim("演示方法甲组合相关性与覆盖奖励，以固定划分开展离线实验。", 2)
	ptr := func(s string) *string { return &s }
	value := "0.420"
	if v.Number() > 1 {
		value = "0.460"
	}
	return library.Analysis{SchemaVersion: library.SchemaVersion, PaperVersionID: v.Key(), Model: "synthetic_demo（本机合成，无模型请求）", PromptVersion: library.PromptVersion,
		ParsedAt: "2026-09-30T10:00:00Z", DocumentHashes: []string{d.Source.SHA256, d.Text.SHA256}, Content: library.AnalysisContent{
			TitleZH: v.Title, AuthorKeywords: []library.Claim{claim("作者关键词：合成实验、离线评估、候选覆盖。", 1)},
			ResourceLinks:     []library.Claim{claim("未发布代码、数据或外部资源链接；本地完整文本为合成演示资料。", 1)},
			ExtractedKeywords: r.ExtractedKeywords, Relevance: r, Problem: &problem, Motivation: &motivation, Method: &method,
			Contributions: []library.Claim{claim("展示完整评估步骤，并区分作者陈述与阅读者推断。", 2)},
			Datasets:      []library.Claim{claim("演示数据甲为虚构固定候选集，没有真实用户数据。", 2)}, Baselines: []library.Claim{claim("演示基线乙作为离线比较基线。", 2)},
			Results:         []library.NumericResult{{Dataset: ptr("演示数据甲"), Metric: "归一化折损累计增益", Method: ptr("演示方法甲"), Baseline: ptr("演示基线乙"), Value: value, Unit: ptr("无量纲"), Setting: ptr("固定候选集"), EvidenceIDs: []string{ev[1].ID}}},
			Limitations:     []library.Claim{claim("只评估合成数据，未报告线上实验、统计显著性或部署成本。", 3)},
			AgentAssessment: []library.Claim{claim("阅读者评估：可复核界面流程与证据定位，合成结果不足以支持线上收益判断。", 3)},
			AuthorClaims:    []library.Application{{Scenario: "作者陈述：演示推荐候选的离线检查。", Conditions: ptr("固定候选集与明确的覆盖指标。"), Cost: ptr("演示级小批量运行。"), EvidenceIDs: []string{ev[2].ID}}},
			AgentInferences: []library.Application{{Scenario: "阅读者推断：广告和搜索候选覆盖检查的教学材料。", Conditions: ptr("必须重新验证真实业务数据。"), Cost: ptr("训练与服务成本需由业务方另行评估。"), EvidenceIDs: []string{ev[2].ID}}},
			SummaryZH:       claim(demoLabel+"；用完整合成正文演示问题、方法、离线指标、局限和应用条件，所有数值均无真实研究含义。", 3),
			KeyPoints:       []library.Claim{claim("业务主题、问题与动机保留明确理由。", 1), claim("实验数值可回跳到完整合成结果行。", 2), claim("作者声称与阅读者推断分别展示，不能直接推断生产收益。", 3)},
			Evidence:        ev, MissingFields: []string{"在线实验：合成正文未报告", "统计显著性：合成正文未报告", "真实部署成本：合成正文未报告"},
		}}
}

func demoSeedLibrary(ctx context.Context, store *state.Store, repo *document.Repository) error {
	versions := demoVersions()
	byKey, docs := map[string]library.Version{}, map[string]library.Document{}
	for _, v := range versions {
		if err := ctx.Err(); err != nil {
			return err
		}
		byKey[v.Key()] = v
		d, err := repo.SaveFixture(v.Identity, demoPages(v))
		if err != nil {
			return err
		}
		docs[v.Key()] = d
	}
	for _, batch := range []struct {
		category, completeness, reason string
		versions                       []library.Version
	}{{"cs.IR", "complete", "合成完整批次：30 个版本均已保留；不代表真实公告。", versions}, {"cs.LG", "incomplete", "合成不完整批次：模拟公告响应缺少 replacement 总数，需要人工核对。", versions[:4]}} {
		events := []library.Announcement{}
		counts := map[string]int{"new": 0, "replacement": 0}
		for _, v := range batch.versions {
			typ := "new"
			if v.Number() > 1 {
				typ = "replacement"
			}
			counts[typ]++
			events = append(events, library.Announcement{Identity: v.Identity, Category: batch.category, Type: typ, Date: demoDate})
		}
		if err := store.SaveCategoryBatch(ctx, library.CategoryBatch{Category: batch.category, Date: demoDate, BuiltAt: "2026-09-30T08:00:00Z", CapturedAt: "2026-09-30T09:00:00Z", Completeness: batch.completeness, Reason: batch.reason, Counts: counts, Versions: batch.versions, Events: events, Artifacts: []library.Artifact{}}); err != nil {
			return err
		}
	}
	if err := store.SaveGap(ctx, library.Gap{Category: "cs.IR", After: "2026-09-25", Before: demoDate, Reason: "合成缺口：模拟停机期间漏收公告；不是已有完整批次，也不会自动发起网络补抓。"}); err != nil {
		return err
	}
	now := time.Now().UTC()
	complete := func(task library.Task, c library.Completion) error {
		c.Run.Model, c.Run.PromptVersion = "synthetic_demo（无模型请求）", library.PromptVersion
		return store.CompleteTask(ctx, task, c, now)
	}
	for _, stage := range []string{"metadata", "relevance", "document"} {
		for range versions {
			task, err := store.ClaimTask(ctx, stage, now, time.Hour)
			if err != nil {
				return err
			}
			v, d := byKey[task.Key()], docs[task.Key()]
			var c library.Completion
			switch stage {
			case "metadata":
				c.Version, c.Next = &v, []string{"relevance"}
			case "relevance":
				r := demoRelevance(v, demoEvidence(d))
				c.Relevance, c.Next = &r, []string{"document"}
			case "document":
				c.Document, c.Next = &d, []string{"analyze"}
			}
			if err := complete(task, c); err != nil {
				return err
			}
		}
	}
	for n := 0; n < len(versions)-1; n++ {
		task, err := store.ClaimTask(ctx, "analyze", now, time.Hour)
		if err != nil {
			return err
		}
		v, d := byKey[task.Key()], docs[task.Key()]
		if v.PaperID == "2609.00022" || v.PaperID == "2609.00024" || v.PaperID == "2609.00025" {
			status, reason, next := "paused", "合成预算暂停：演示请求上限已用尽，已有全文与逐块阅读进度已保存。", time.Time{}
			if v.PaperID == "2609.00022" {
				status, reason, next = "retry_wait", "合成可重试错误：模拟网关暂时不可用；本机没有发起任何请求。", now.Add(time.Hour)
			}
			if v.PaperID == "2609.00025" {
				status, reason = "blocked", "合成证据检查阻塞：待核对结果行的单位，禁止发布不完整分析。"
			}
			checkpoint, err := json.Marshal(map[string]any{"synthetic_demo": true, "reason": reason, "run": library.Run{Requests: 0, Model: "synthetic_demo", PromptVersion: library.PromptVersion, Coverage: []string{d.Blocks[0].ID}}})
			if err != nil {
				return err
			}
			if err := store.SaveCheckpoint(ctx, task, checkpoint, now); err != nil {
				return err
			}
			evidence := demoEvidence(d)[0]
			if err := store.SaveChunk(ctx, task, library.Chunk{DocumentID: d.ID, DocumentHash: d.Text.SHA256, BlockID: d.Blocks[0].ID, Read: true, Notes: []library.Claim{{Text: "已阅读第一节合成正文；后续阶段保留暂停或阻塞状态。", EvidenceIDs: []string{evidence.ID}}}, Evidence: []library.Evidence{evidence}, MissingFields: []string{"其余页面尚未完成逐块分析"}}, now); err != nil {
				return err
			}
			if err := store.FailTask(ctx, task, status, reason, next, now); err != nil {
				return err
			}
			continue
		}
		a := demoAnalysis(v, d, demoRelevance(v, demoEvidence(d)))
		for index, block := range d.Blocks {
			evidence := a.Content.Evidence[index]
			if err := store.SaveChunk(ctx, task, library.Chunk{DocumentID: d.ID, DocumentHash: d.Text.SHA256, BlockID: block.ID, Read: true, Notes: []library.Claim{{Text: fmt.Sprintf("已阅读并保留合成正文第%d页的完整内容与证据。", block.Page), EvidenceIDs: []string{evidence.ID}}}, Evidence: []library.Evidence{evidence}, MissingFields: []string{}}, now); err != nil {
				return err
			}
		}
		cmp := library.Comparison{Status: "not_applicable", Reason: "首个合成版本 v1，没有上一版本，比较不适用。", DocumentHashes: []string{}, Content: library.ComparisonContent{Changes: []library.Change{}, Evidence: []library.Evidence{}}}
		var next []string
		if prev, ok := v.Previous(); ok {
			cmp.Status, cmp.PreviousVersion, cmp.Reason = "pending", prev.Version, "合成比较待处理：当前版本分析已完成，上一版本对照尚未运行。"
			if v.PaperID == "2609.00001" && (v.Version == "v2" || v.Version == "v10") {
				next = []string{"compare"}
			}
			if v.PaperID == "2609.00003" {
				cmp.Status, cmp.Reason, next = "blocked", "合成比较阻塞：模拟上一版本证据待核验，不据此声称版本变化。", []string{"compare"}
			}
		}
		if err := complete(task, library.Completion{Analysis: &a, Comparison: &cmp, Next: next, Run: library.Run{Coverage: []string{d.Blocks[0].ID, d.Blocks[1].ID, d.Blocks[2].ID}}}); err != nil {
			return err
		}
	}
	// Only two completed comparisons and one explicitly blocked task have been
	// enqueued. Pending comparisons are enqueued afterwards and remain queued.
	for n := 0; n < 3; n++ {
		task, err := store.ClaimTask(ctx, "compare", now, time.Hour)
		if err != nil {
			return err
		}
		if task.PaperID == "2609.00003" {
			if err := store.FailTask(ctx, task, "blocked", "合成比较阻塞：上一版本证据待核验。", time.Time{}, now); err != nil {
				return err
			}
			continue
		}
		prev, _ := task.Previous()
		current, previous := docs[task.Key()], docs[prev.Key()]
		currentAnalysis, aid, err := store.Analysis(ctx, task.Identity)
		if err != nil {
			return err
		}
		previousAnalysis, _, err := store.Analysis(ctx, prev)
		if err != nil {
			return err
		}
		currentValue, previousValue := currentAnalysis.Content.Results[0].Value, previousAnalysis.Content.Results[0].Value
		description := fmt.Sprintf("合成版本 %s → %s：示例结果由 %s 改为 %s，版本文本更新；均不代表真实实验。", prev.Version, task.Version, previousValue, currentValue)
		if currentValue == previousValue {
			description = fmt.Sprintf("合成版本 %s → %s：双方示例结果均为 %s，数值保持不变，版本文本更新；均不代表真实实验。", prev.Version, task.Version, currentValue)
		}
		curEvidence, prevEvidence := demoEvidence(current)[1], demoEvidence(previous)[1]
		cmp := library.Comparison{AnalysisID: aid, PreviousVersion: prev.Version, Status: "completed", Reason: "两个合成版本全文均已保留；比较仅展示虚构结果行与版本说明。", DocumentHashes: []string{current.Source.SHA256, current.Text.SHA256, previous.Source.SHA256, previous.Text.SHA256}, Content: library.ComparisonContent{Evidence: []library.Evidence{curEvidence, prevEvidence}, Changes: []library.Change{{Kind: "changed", Description: description, CurrentEvidenceIDs: []string{curEvidence.ID}, PreviousEvidenceIDs: []string{prevEvidence.ID}}}}}
		if err := complete(task, library.Completion{Comparison: &cmp, ReferenceDocuments: []library.Document{previous}}); err != nil {
			return err
		}
	}
	for _, id := range []library.Identity{{Source: "arxiv", PaperID: "2609.00001", Version: "v9"}, {Source: "arxiv", PaperID: "2609.00002", Version: "v2"}} {
		if err := store.EnqueueTask(ctx, id, "compare", 0); err != nil {
			return err
		}
	}
	return nil
}

func demoSeedLegacy(ctx context.Context, store *state.Store, topic string) error {
	for n, date := range []string{"2026-09-27", "2026-09-28", "2026-09-29"} {
		if _, err := store.ClaimDay(ctx, topic, date); err != nil {
			return err
		}
		var candidates []papers.Paper
		if n < 2 {
			v := demoVersions()[n]
			candidates = []papers.Paper{{ID: "arxiv:" + v.PaperID, Version: v.Version, Title: v.Title, Authors: v.Authors, Abstract: v.Abstract, Published: time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC), Updated: time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC), Comment: demoLabel, Categories: v.Categories, PrimaryCategory: v.PrimaryCategory}}
		}
		if err := store.SaveCandidates(ctx, topic, date, candidates); err != nil {
			return err
		}
		for _, paper := range candidates {
			if err := store.SaveSummary(ctx, topic, date, paper.ID, digest.Summary{Text: demoLabel + "；这是通过旧日报状态接口保存的中文摘要，未请求模型或发送消息。", Model: "synthetic_demo", PromptVersion: "synthetic-demo-v1"}); err != nil {
				return err
			}
		}
		message := demoLabel + "；仅模拟旧日报历史状态，未真实发送。"
		if n == 2 {
			message += "此日为合成空日报，没有论文条目。"
		}
		if err := store.Ready(ctx, topic, date, message); err != nil {
			return err
		}
		claimed, err := store.ClaimSend(ctx, topic, date)
		if err != nil {
			return err
		}
		if !claimed {
			return errors.New("合成旧日报状态无法进入 sending")
		}
		if n == 1 {
			if err := store.MarkUnknown(ctx, topic, date); err != nil {
				return err
			}
		} else if err := store.MarkSent(ctx, topic, date); err != nil {
			return err
		}
	}
	return nil
}
