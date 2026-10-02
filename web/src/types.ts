// Historical digest contracts are kept separate from the version library.
export interface Summary { text: string; model: string; promptVersion: string }
export interface Paper {
  id: string; version: string; title: string; authors: string[]
  publishedAt: string; updatedAt: string; abstract: string; url: string
  digestDate: string; position: number; status: string; summary: Summary | null
}
export interface Digest { date: string; status: string; paperCount: number; summaryCount: number }
export interface DigestDetail extends Digest { message: string; items: Paper[] }
export interface Page<T> { items: T[]; total: number; page: number; pageSize: number }

// Mirrors internal/library/types.go; the wire names intentionally stay snake_case.
export interface LibraryIdentity { source: string; paper_id: string; version: string }
export interface LibraryArtifact { path: string; sha256: string; url: string; captured_at: string; kind: string }
export interface LibraryVersion extends LibraryIdentity {
  title: string; authors: string[]; abstract: string; primary_category: string; categories: string[]
  published_at: string; updated_at: string; doi: string; journal_ref: string; comment: string
  author_keywords: string[]; origin: string; announcement_date: string; captured_at: string
  metadata_verified: boolean; metadata_artifacts: LibraryArtifact[]
}
export interface LibraryRelevance {
  topics: string[]; directly_related: boolean; relevance_level: string; rationale: string
  extracted_keywords: string[]; evidence_ids: string[]
}
export interface LibraryBlock { id: string; page: number; start: number; end: number; text: string; sha256: string }
export interface LibraryDocument {
  id: string; source: string; paper_id: string; version: string
  source_file: LibraryArtifact; text_file: LibraryArtifact; extractor: string; quality: string; issues: string[]
  pages: { number: number; text?: string; sha256: string }[]; blocks: Omit<LibraryBlock, 'text'>[]
}
export interface LibraryEvidence {
  id: string; document_id: string; version: string; block_id: string
  section: string; page: number; start: number; end: number; quote: string
}
export interface LibraryClaim { text: string; evidence_ids: string[] }
export interface LibraryNumericResult {
  dataset: string | null; metric: string; method: string | null; baseline: string | null
  value: string; unit: string | null; setting: string | null; evidence_ids: string[]
}
export interface LibraryApplication { scenario: string; conditions: string | null; cost: string | null; evidence_ids: string[] }
export interface LibraryAnalysisContent {
  title_zh: string; author_keywords: LibraryClaim[]; resource_links: LibraryClaim[]; extracted_keywords: string[]
  relevance: LibraryRelevance; problem: LibraryClaim | null; motivation: LibraryClaim | null; method: LibraryClaim | null
  contributions: LibraryClaim[]; datasets: LibraryClaim[]; baselines: LibraryClaim[]; results: LibraryNumericResult[]
  limitations: LibraryClaim[]; agent_assessment: LibraryClaim[]; author_claims: LibraryApplication[]
  agent_inferences: LibraryApplication[]; summary_zh: LibraryClaim; key_points: LibraryClaim[]
  evidence: LibraryEvidence[]; missing_fields: string[]
}
export interface LibraryAnalysis {
  schema_version: string; paper_version_id: string; model: string; prompt_version: string
  parsed_at: string; document_hashes: string[]; content: LibraryAnalysisContent
}
export interface LibraryChange {
  kind: string; description: string; current_evidence_ids: string[]; previous_evidence_ids: string[]
}
export interface LibraryComparison {
  analysis_id: number; previous_version: string; status: string; reason: string; document_hashes: string[]
  content: { changes: LibraryChange[]; evidence: LibraryEvidence[] }
}
export interface LibraryTask extends LibraryIdentity {
  id: number; stage: string; generation: number; status: string; attempt: number
  next_attempt_at: string; lease_until: string; error: string; checkpoint?: unknown
}
export interface LibraryRun {
  requests: number; prompt_tokens: number; completion_tokens: number; reserved_tokens: number
  duration_ms: number; model: string; prompt_version: string; coverage: string[]
}
export interface LibraryListItem {
  version: LibraryVersion; relevance: LibraryRelevance | null; analysis_id: number; tasks: LibraryTask[]
}
export interface LibraryDetailData extends LibraryListItem {
  versions: LibraryIdentity[]; relevance_history: LibraryRelevance[]; documents: LibraryDocument[]
  analysis: LibraryAnalysis | null; comparison: LibraryComparison | null; runs: LibraryRun[]
}
export interface LibraryAnnouncement extends LibraryIdentity { category: string; type: string; date: string }
export interface LibraryBatch {
  category: string; date: string; built_at: string; captured_at: string; completeness: string; reason: string
  counts: Record<string, number>; artifacts: LibraryArtifact[]; events: LibraryAnnouncement[]; versions: LibraryVersion[]
}
export interface LibraryStatus {
  versions: number; tasks: Record<string, number>; batches: LibraryBatch[]
  gaps: { category: string; after: string; before: string; reason: string }[]
}
export type LibraryResultPage = Page<LibraryListItem>
