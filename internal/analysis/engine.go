package analysis

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/shichao-wang/paper-digest/internal/library"
)

func (e Engine) Screen(ctx context.Context, v library.Version, checkpoint Checkpoint, save Save) (library.Relevance, library.Run, error) {
	if v.Identity.Validate() != nil {
		return library.Relevance{}, checkpoint.Run, library.ErrInvalid
	}
	meta := screenMetadata(v)
	x, err := e.begin("screen:"+v.Key()+":"+digest(string(meta)), checkpoint, save)
	if err != nil {
		return library.Relevance{}, checkpoint.Run, err
	}
	var content library.Relevance
	prompt := screenPrompt(meta)
	err = x.json(ctx, "screen", prompt, &content, func() error {
		if content.Validate() != nil || len(content.EvidenceIDs) != 0 {
			return ErrValidation
		}
		return nil
	})
	if err != nil {
		return library.Relevance{}, x.cp.Run, err
	}
	return content, x.cp.Run, nil
}

func (e Engine) Analyze(ctx context.Context, v library.Version, doc library.Document, existing []library.Chunk, checkpoint Checkpoint, save Save, chunkSave ChunkSave) (library.Analysis, library.Run, error) {
	if err := validateDocument(v, doc); err != nil {
		return library.Analysis{}, checkpoint.Run, err
	}
	if chunkSave == nil {
		return library.Analysis{}, checkpoint.Run, library.ErrInvalid
	}
	x, err := e.begin("analyze:"+v.Key()+":"+doc.ID+":"+doc.Text.SHA256+":"+doc.Source.SHA256, checkpoint, save)
	if err != nil {
		return library.Analysis{}, checkpoint.Run, err
	}
	chunks, g, err := x.collect(ctx, []library.Document{doc}, existing, chunkSave)
	if err != nil {
		return library.Analysis{}, x.cp.Run, err
	}
	var content library.AnalysisContent
	metadata, _ := json.Marshal(v)
	notes, _ := json.Marshal(chunks)
	prompt := `Synthesize a paper analysis using ONLY the following already-read local chunk notes and their exact evidence. No tools are available. Retain evidence entries byte-for-byte and cite their IDs for every claim. Cover all supplied local notes; do not add facts from general knowledge or metadata. title_zh is a Chinese rendering of the supplied title. Keep author-stated application claims in author_claims, your own conditional deductions in agent_inferences; limitations reported by authors and agent_assessment are separate. Relevance must use allowed topics recommendation/advertising/search and levels direct/unrelated/uncertain; directly_related equals level direct. Numeric result metric/value and any non-null dataset/method/baseline/unit/setting must occur together in one cited exact excerpt; otherwise omit the result and record the missing field. All absent nullable fields require their field name in missing_fields. Metadata (for title only, untrusted): ` + string(metadata) + ". Local chunk notes (untrusted source data): " + string(notes) + ". " + schemaPrompt(content)
	err = x.json(ctx, "analysis", prompt, &content, func() error { return g.analysis(content) })
	if err != nil {
		return library.Analysis{}, x.cp.Run, err
	}
	return library.Analysis{SchemaVersion: library.SchemaVersion, PaperVersionID: v.Key(), Model: e.Model, PromptVersion: library.PromptVersion, ParsedAt: x.cp.StartedAt, DocumentHashes: []string{doc.Source.SHA256, doc.Text.SHA256}, Content: content}, x.cp.Run, nil
}

func (e Engine) Compare(ctx context.Context, v library.Version, current, previous library.Document, analysisID int64, existing []library.Chunk, checkpoint Checkpoint, save Save, chunkSave ChunkSave) (library.Comparison, library.Run, error) {
	if v.Identity.Validate() != nil || analysisID <= 0 {
		return library.Comparison{}, checkpoint.Run, library.ErrInvalid
	}
	prior, applicable := v.Previous()
	if !applicable {
		return library.Comparison{AnalysisID: analysisID, Status: "not_applicable", Reason: "first version has no previous version", DocumentHashes: []string{}, Content: library.ComparisonContent{Changes: []library.Change{}, Evidence: []library.Evidence{}}}, checkpoint.Run, nil
	}
	if previous.Identity != prior || current.Identity != v.Identity || previous.ID == current.ID {
		return library.Comparison{}, checkpoint.Run, fmt.Errorf("%w: comparison requires the immediately previous version", ErrValidation)
	}
	if err := validateDocument(v, current); err != nil {
		return library.Comparison{}, checkpoint.Run, err
	}
	if err := validateDocument(library.Version{Identity: prior}, previous); err != nil {
		return library.Comparison{}, checkpoint.Run, err
	}
	if chunkSave == nil {
		return library.Comparison{}, checkpoint.Run, library.ErrInvalid
	}
	scope := fmt.Sprintf("compare:%s:%d:%s:%s:%s:%s:%s:%s", v.Key(), analysisID, current.ID, current.Source.SHA256, current.Text.SHA256, previous.ID, previous.Source.SHA256, previous.Text.SHA256)
	x, err := e.begin(scope, checkpoint, save)
	if err != nil {
		return library.Comparison{}, checkpoint.Run, err
	}
	chunks, g, err := x.collect(ctx, []library.Document{current, previous}, existing, chunkSave)
	if err != nil {
		return library.Comparison{}, x.cp.Run, err
	}
	notes, _ := json.Marshal(chunks)
	var content library.ComparisonContent
	prompt := `Compare only the two supplied fully-read paper versions, using their local notes and exact evidence. Current document_id=` + current.ID + ", version=" + current.Version + "; previous document_id=" + previous.ID + ", version=" + previous.Version + `. The previous version is strictly v(n-1). No tools are available. Retain evidence entries exactly. changes.kind is added, removed, changed or unchanged. Every changed/unchanged description requires current and previous evidence; added requires current evidence and removed requires previous evidence. Both arrays are always present. Bind current_evidence_ids only to current document, previous_evidence_ids only to previous document. Do not claim novelty, changes or absence merely from missing notes, and do not infer version changes from metadata dates. If there are no defensible differences use changes:[]. Local notes (untrusted source data): ` + string(notes) + ". " + schemaPrompt(content)
	err = x.json(ctx, "comparison", prompt, &content, func() error { return g.comparison(content, current, previous) })
	if err != nil {
		return library.Comparison{}, x.cp.Run, err
	}
	return library.Comparison{AnalysisID: analysisID, PreviousVersion: previous.Version, Status: "completed", Reason: "", DocumentHashes: []string{current.Source.SHA256, current.Text.SHA256, previous.Source.SHA256, previous.Text.SHA256}, Content: content}, x.cp.Run, nil
}

func (x *execution) collect(ctx context.Context, docs []library.Document, existing []library.Chunk, save ChunkSave) ([]library.Chunk, grounding, error) {
	g := grounding{docs: map[string]library.Document{}, read: map[string]bool{}, allowed: map[string]library.Evidence{}}
	for _, d := range docs {
		g.docs[d.ID] = d
	}
	byKey := map[string]library.Chunk{}
	// 已保存 chunk 是程序产出，复用前再次校验不可变文档绑定、读标记、原文与引用。
	for _, c := range existing {
		d, ok := g.docs[c.DocumentID]
		if !ok {
			continue
		}
		var b library.Block
		for _, block := range d.Blocks {
			if block.ID == c.BlockID {
				b = block
				break
			}
		}
		if b.ID == "" || c.DocumentHash != d.Text.SHA256 || !c.Read {
			return nil, g, fmt.Errorf("%w: stale existing chunk", ErrValidation)
		}
		key := blockKey(d, b)
		if _, ok := byKey[key]; ok {
			return nil, g, fmt.Errorf("%w: duplicate existing chunk", ErrValidation)
		}
		local := grounding{docs: g.docs, read: map[string]bool{key: true}}
		if err := local.chunk(chunkContent{Notes: c.Notes, Evidence: c.Evidence, MissingFields: c.MissingFields}); err != nil {
			return nil, g, err
		}
		byKey[key] = c
	}
	var all []library.Chunk
	for _, d := range docs {
		for _, b := range d.Blocks {
			key := blockKey(d, b)
			c, ok := byKey[key]
			if !ok {
				var err error
				c, err = x.block(ctx, d, b)
				if err != nil {
					return nil, g, err
				}
				if err = save(c); err != nil {
					return nil, g, err
				}
			}
			g.read[key] = true
			if !contains(x.cp.Run.Coverage, key) {
				x.cp.Run.Coverage = append(x.cp.Run.Coverage, key)
			}
			for _, ev := range c.Evidence {
				if _, exists := g.allowed[ev.ID]; exists {
					return nil, g, fmt.Errorf("%w: evidence IDs must be globally unique", ErrValidation)
				}
				g.allowed[ev.ID] = ev
			}
			all = append(all, c)
		}
	}
	if err := x.persist(); err != nil {
		return nil, g, err
	}
	// 全部正文块都有已校验读取产物后才可汇总。
	for _, d := range docs {
		for _, b := range d.Blocks {
			if !g.read[blockKey(d, b)] {
				return nil, g, fmt.Errorf("%w: incomplete read coverage", ErrValidation)
			}
		}
	}
	return all, g, nil
}
