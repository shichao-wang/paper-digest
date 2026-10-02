import { useState } from 'react'
import type { ReactNode } from 'react'
import { useRequest } from './hooks'
import { apiID, dateLabel, isSynthetic } from './libraryUI'
import type { LibraryApplication, LibraryBlock, LibraryClaim, LibraryComparison, LibraryDetailData, LibraryEvidence, LibraryIdentity } from './types'

function Section({ title, children, note }: { title: string; children: ReactNode; note?: string }) {
  return <section className="library-section"><div className="section-heading"><h3>{title}</h3>{note && <span>{note}</span>}</div>{children}</section>
}
function Empty({ children = '本次分析未提供此项。' }: { children?: ReactNode }) { return <p className="summary-missing">{children}</p> }
function EvidenceBlock({ evidence, identity }: { evidence: LibraryEvidence; identity: LibraryIdentity }) {
  const [open, setOpen] = useState(false)
  const params = new URLSearchParams({ id: apiID(identity), version: evidence.version || identity.version, document: evidence.document_id, block: evidence.block_id })
  const block = useRequest<LibraryBlock>(open ? `/api/library/evidence?${params}` : null)
  return <details className="original-block" onToggle={event => setOpen(event.currentTarget.open)}><summary>展开原文</summary>
    {open && <>{block.loading && <p role="status">正在读取原文…</p>}{block.error && <p role="alert">暂时无法读取原文。 <button className="text-button" onClick={block.retry}>重试</button></p>}{block.data && <><p className="evidence-location">第 {block.data.page || '未记录'} 页</p><div className="original-text" lang="en">{block.data.text}</div></>}</>}
  </details>
}
function EvidenceCard({ evidence, identity }: { evidence: LibraryEvidence; identity: LibraryIdentity }) {
  return <div className="evidence-card"><p className="evidence-location">{evidence.version || identity.version} · {evidence.page > 0 ? `第 ${evidence.page} 页` : '页码未记录'} · {evidence.section || '章节未记录'}</p><blockquote>{evidence.quote || '未保存引文。'}</blockquote><EvidenceBlock evidence={evidence} identity={identity} /></div>
}
function EvidenceRefs({ ids, evidence, identity }: { ids: string[]; evidence: LibraryEvidence[]; identity: LibraryIdentity }) {
  if (!ids?.length) return <span className="evidence-missing">未关联原文证据</span>
  return <details className="claim-evidence"><summary>查看证据（{ids.length}）</summary>{ids.map((id, index) => {
    const match = (evidence || []).find(item => item.id === id && (item.version || identity.version) === identity.version)
    return match ? <EvidenceCard key={`${id}/${index}`} evidence={match} identity={identity} /> : <p className="summary-missing" key={`${id}/${index}`}>此处引文暂不可用。</p>
  })}</details>
}
function Claims({ claims, evidence, identity }: { claims: LibraryClaim[]; evidence: LibraryEvidence[]; identity: LibraryIdentity }) {
  return claims?.length ? <ul className="claim-list">{claims.map((claim, index) => <li key={index}><div className="claim-text">{claim.text}</div><EvidenceRefs ids={claim.evidence_ids} evidence={evidence} identity={identity} /></li>)}</ul> : <Empty />
}
function Applications({ applications, evidence, identity }: { applications: LibraryApplication[]; evidence: LibraryEvidence[]; identity: LibraryIdentity }) {
  return applications?.length ? <div className="application-list">{applications.map((item, index) => <div className="application-card" key={index}><div className="claim-text">{item.scenario}</div><dl className="reading-facts"><div><dt>适用条件</dt><dd>{item.conditions || '未提供'}</dd></div><div><dt>成本与代价</dt><dd>{item.cost || '未提供'}</dd></div></dl><EvidenceRefs ids={item.evidence_ids} evidence={evidence} identity={identity} /></div>)}</div> : <Empty />
}
function ComparisonSection({ comparison, identity }: { comparison: LibraryComparison | null; identity: LibraryIdentity }) {
  const number = Number(identity.version.replace(/^v/, ''))
  if (number === 1) return null
  const expected = Number.isSafeInteger(number) && number > 1 ? `v${number - 1}` : ''
  const completed = !!expected && comparison?.previous_version === expected && comparison?.status === 'completed'
  if (!completed) return <Section title="版本比较"><Empty>暂时没有可阅读的版本差异。</Empty></Section>
  const evidence = comparison.content?.evidence || []
  return <Section title="版本比较" note={`${expected} → ${identity.version}`}>
    {comparison.content?.changes?.length ? <div className="change-list">{comparison.content.changes.map((change, index) => <div className="change-card" key={index}><div className="claim-text">{change.description}</div><p className="small-copy">当前版本 {identity.version}</p><EvidenceRefs ids={change.current_evidence_ids} evidence={evidence} identity={identity} /><p className="small-copy">前一版本 {expected}</p><EvidenceRefs ids={change.previous_evidence_ids} evidence={evidence} identity={{ ...identity, version: expected }} /></div>)}</div> : <Empty>未发现已记录的版本变化。</Empty>}
  </Section>
}

export default function LibraryDetail({ detail, onBack, onVersion }: { detail: LibraryDetailData; onBack: () => void; onVersion: (identity: LibraryIdentity) => void }) {
  const version = detail.version
  const content = detail.analysis?.content
  const evidence = content?.evidence || []
  const identity = { source: version.source, paper_id: version.paper_id, version: version.version }
  const claimProps = { evidence, identity }
  const identifier = version.paper_id.replace(/^arxiv:/, '') + version.version
  const synthetic = isSynthetic(version)
  const exactSource = !synthetic && version.source === 'arxiv' && /^v[1-9]\d*$/.test(version.version)
  return <article className="paper-detail library-detail">
    <button className="mobile-back" onClick={onBack}>‹ 返回版本列表</button>
    <div className="detail-meta"><span>{version.source} · {version.paper_id} · {version.version}</span></div>
    {synthetic && <p className="notice demo-notice">合成演示数据，非真实论文。</p>}
    <h2 className="paper-title">{version.title}</h2>{content?.title_zh && content.title_zh !== version.title && <p className="translated-title">{content.title_zh}</p>}
    <p className="detail-authors">{version.authors?.join(', ') || '未提供作者'}</p>
    <dl className="dates"><div><dt>首发</dt><dd>{dateLabel(version.published_at)}</dd></div><div><dt>版本更新</dt><dd>{dateLabel(version.updated_at)}</dd></div></dl>
    <label className="version-picker">版本<select value={version.version} onChange={event => { const match = (detail.versions || []).find(item => item.version === event.target.value); if (match) onVersion(match) }}>{!(detail.versions || []).some(item => item.version === version.version) && <option value={version.version}>{version.version}</option>}{(detail.versions || []).map(item => <option key={item.version} value={item.version}>{item.version}</option>)}</select></label>
    {exactSource && <div className="source-links"><a href={`https://arxiv.org/abs/${identifier}`} target="_blank" rel="noreferrer">arXiv {version.version} ↗</a><a href={`https://arxiv.org/pdf/${identifier}`} target="_blank" rel="noreferrer">PDF {version.version} ↗</a></div>}
    {content ? <>
      <Section title="中文总结"><Claims claims={[content.summary_zh].filter(Boolean)} {...claimProps} /></Section>
      <Section title="关键要点"><Claims claims={content.key_points} {...claimProps} /></Section>
      <Section title="研究问题"><Claims claims={content.problem ? [content.problem] : []} {...claimProps} /></Section>
      <Section title="研究动机"><Claims claims={content.motivation ? [content.motivation] : []} {...claimProps} /></Section>
      <Section title="方法与技术方案"><Claims claims={content.method ? [content.method] : []} {...claimProps} /></Section>
      <Section title="主要贡献"><Claims claims={content.contributions} {...claimProps} /></Section>
      <Section title="实验数据集"><Claims claims={content.datasets} {...claimProps} /></Section>
      <Section title="对比基线"><Claims claims={content.baselines} {...claimProps} /></Section>
      <Section title="实验结果">{content.results?.length ? <div className="result-list">{content.results.map((result, index) => <div className="result-card" key={index}><h4>{result.metric || '指标未记录'}：{result.value} {result.unit || ''}</h4><dl className="reading-facts"><div><dt>数据集</dt><dd>{result.dataset || '未提供'}</dd></div><div><dt>方法</dt><dd>{result.method || '未提供'}</dd></div><div><dt>基线</dt><dd>{result.baseline || '未提供'}</dd></div><div><dt>实验设置</dt><dd>{result.setting || '未提供'}</dd></div></dl><EvidenceRefs ids={result.evidence_ids} {...claimProps} /></div>)}</div> : <Empty />}</Section>
      <Section title="论文局限" note="作者在原文中的说明"><Claims claims={content.limitations} {...claimProps} /></Section>
      <Section title="Agent 评估" note="分析判断，需结合原文核对"><Claims claims={content.agent_assessment} {...claimProps} /></Section>
      <Section title="业务应用 · 作者主张" note="原文支持的应用描述"><Applications applications={content.author_claims} {...claimProps} /></Section>
      <Section title="业务应用 · Agent 推断" note="推断，不等同于作者结论"><Applications applications={content.agent_inferences} {...claimProps} /></Section>
      <Section title="关键词与研究资源"><h4>作者关键词</h4><Claims claims={content.author_keywords} {...claimProps} /><h4>提取关键词</h4><p className="claim-text">{content.extracted_keywords?.join('、') || '未提供'}</p><h4>原文中的资源</h4><Claims claims={content.resource_links} {...claimProps} /></Section>
    </> : <Section title="论文解读"><Empty>此版本的解读暂未提供，可先阅读原始摘要。</Empty></Section>}
    <ComparisonSection comparison={detail.comparison} identity={identity} />
    <Section title="原始摘要"><p lang="en" className="abstract-text">{version.abstract || '未提供原始摘要。'}</p></Section>
    <details className="provenance"><summary>论文信息</summary><dl className="reading-facts"><div><dt>分类</dt><dd>{version.categories?.join('、') || '未提供'}</dd></div><div><dt>DOI</dt><dd>{version.doi || '未提供'}</dd></div><div><dt>期刊</dt><dd>{version.journal_ref || '未提供'}</dd></div><div><dt>作者备注</dt><dd>{version.comment || '未提供'}</dd></div></dl></details>
  </article>
}
