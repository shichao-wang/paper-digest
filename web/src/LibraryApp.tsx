import { useEffect, useRef, useState } from 'react'
import { useLocation, useRequest } from './hooks'
import LibraryDetail from './LibraryDetail'
import { apiID, dateLabel, identityKey, isSynthetic, LibraryMessage, LibraryPagination, positivePage, topicLabels } from './libraryUI'
import type { LibraryDetailData, LibraryIdentity, LibraryListItem, LibraryResultPage } from './types'

export default function LibraryApp() {
  const { params, navigate } = useLocation()
  const query = params.get('q') || ''
  const topic = params.get('topic') || ''
  const relevance = params.get('relevance') === 'all' ? 'all' : 'direct'
  const page = positivePage(params.get('page'))
  const [input, setInput] = useState(query)
  const listRef = useRef<HTMLDivElement>(null)
  const detailRef = useRef<HTMLDivElement>(null)
  const listPosition = useRef({ pane: 0, window: 0 })
  const returnFocus = useRef<HTMLButtonElement | null>(null)
  const wasDetail = useRef(false)
  useEffect(() => { setInput(query) }, [query])
  const listQuery = new URLSearchParams({ q: query, topic, relevance, page: String(page), pageSize: '20' })
  const papers = useRequest<LibraryResultPage>(`/api/library/papers?${listQuery}`)
  const selectedID = params.get('paper') || params.get('id') || ''
  const selectedVersion = params.get('version') || ''
  const explicitPaper = !!selectedID && !!selectedVersion
  const selected = explicitPaper ? { source: 'arxiv', paper_id: selectedID, version: selectedVersion } : selectedID ? undefined : papers.data?.items[0]?.version
  const detailQuery = selected ? new URLSearchParams({ id: apiID(selected), version: selected.version }) : null
  const detail = useRequest<LibraryDetailData>(detailQuery ? `/api/library/papers/detail?${detailQuery}` : null)
  const selectedKey = selected ? identityKey({ ...selected, paper_id: apiID(selected) }) : ''
  useEffect(() => {
    if (wasDetail.current && !explicitPaper) requestAnimationFrame(() => {
      listRef.current?.scrollTo(0, listPosition.current.pane)
      if (window.matchMedia('(max-width: 767px)').matches) window.scrollTo(0, listPosition.current.window)
      returnFocus.current?.focus({ preventScroll: true })
    })
    wasDetail.current = explicitPaper
  }, [explicitPaper])
  useEffect(() => { detailRef.current?.scrollTo(0, 0) }, [selectedKey])
  function filter(updates: Record<string, string | null>) {
    navigate({ ...updates, batch: null, status: null, page: null, paper: null, id: null, version: null })
    listPosition.current = { pane: 0, window: 0 }
    listRef.current?.scrollTo(0, 0)
  }
  function selectPaper(item: LibraryListItem) {
    returnFocus.current = document.activeElement instanceof HTMLButtonElement ? document.activeElement : null
    listPosition.current = { pane: listRef.current?.scrollTop || 0, window: window.scrollY }
    selectVersion(item.version)
  }
  function selectVersion(identity: LibraryIdentity) {
    navigate({ paper: apiID(identity), id: null, version: identity.version })
    detailRef.current?.scrollTo(0, 0)
    if (window.matchMedia('(max-width: 767px)').matches) {
      window.scrollTo(0, 0)
      requestAnimationFrame(() => detailRef.current?.focus({ preventScroll: true }))
    }
  }
  function back() { navigate({ paper: null, id: null, version: null }) }
  function legacy(view: string) {
    navigate({ view, page: null, paper: null, id: null, version: null, paperDate: null, digest: null, digestTopic: null, q: null, date: null, summary: null, batch: null, topic: null, relevance: null, status: null })
  }
  const hasFilters = !!query || !!topic || relevance !== 'direct'
  return <div className="app-shell">
    <a className="skip-link" href="#main-content">跳到论文内容</a>
    <header className="header"><div className="brand"><div className="brand-mark" aria-hidden="true"><i /><i /><i /></div><div><h1>论文日报</h1><p>Recommendation / Advertising / Search</p></div></div><nav aria-label="主导航"><button aria-current="page" onClick={() => filter({ q: null, topic: null, relevance: null })}>版本论文库</button><button onClick={() => legacy('legacy')}>日报精选快照</button><button onClick={() => legacy('digests')}>日报历史</button><button onClick={() => legacy('topics')}>日报主题管理</button></nav></header>
    <main id="main-content"><div className="page-heading"><div><h2>版本论文库</h2><p>阅读推荐、广告与搜索领域的研究，了解方法、实验与版本变化。</p></div><span className="local-label"><span aria-hidden="true" />本机阅读</span></div>
      <form className="filters library-filters" onSubmit={event => { event.preventDefault(); filter({ q: input.trim() }) }}>
        <label className="search-field"><span className="sr-only">搜索标题、作者或摘要</span><svg width="19" height="19" viewBox="0 0 24 24" fill="none" aria-hidden="true"><circle cx="10.5" cy="10.5" r="6.5" stroke="currentColor" strokeWidth="1.7" /><path d="m16 16 5 5" stroke="currentColor" strokeWidth="1.7" /></svg><input type="search" placeholder="搜索标题、作者或摘要" value={input} onChange={event => setInput(event.target.value)} /><button type="submit">搜索</button></label>
        <label className="filter-field"><span>范围</span><select value={relevance} onChange={event => filter({ relevance: event.target.value === 'direct' ? null : 'all' })}><option value="direct">精选论文</option><option value="all">全部论文</option></select></label>
        <label className="filter-field"><span>主题</span><select value={topic} onChange={event => filter({ topic: event.target.value })}><option value="">全部主题</option>{topic && !Object.hasOwn(topicLabels, topic) && <option value={topic}>{topic}</option>}{Object.entries(topicLabels).map(([value, label]) => <option value={value} key={value}>{label}</option>)}</select></label>
        {hasFilters && <button className="clear-filters" type="button" onClick={() => filter({ q: null, topic: null, relevance: null })}>恢复默认</button>}
      </form>
      <div className={`workspace library-workspace ${explicitPaper ? 'show-detail' : ''}`}><div className="list-pane" ref={listRef}><div className="list-heading"><span>{relevance === 'all' ? '全部论文' : '精选论文'}</span><span>{papers.data ? `${papers.data.total} 个版本` : ''}</span></div>
        {papers.loading && <LibraryMessage title="正在加载论文…" />}{papers.error && <LibraryMessage title="无法加载论文库" retry={papers.retry}>{papers.error}</LibraryMessage>}
        {papers.data?.items.length === 0 && <LibraryMessage title={hasFilters ? '没有符合条件的论文' : '暂无精选论文'}>试试其他关键词，或浏览全部论文。</LibraryMessage>}
        {(papers.data?.items || []).map(item => { const version = item.version; const key = identityKey({ ...version, paper_id: apiID(version) }); return <button className="paper-row library-row" key={key} aria-current={key === selectedKey ? 'true' : undefined} onClick={() => selectPaper(item)}><div className="row-meta"><span>{dateLabel(version.updated_at || version.published_at)} · {version.primary_category || version.source}</span><span>{version.version}</span></div><h3 className="paper-title">{version.title}</h3><p className="row-authors">{version.authors?.join(', ') || '未提供作者'}</p><div className="inline-tags">{(item.relevance?.topics || []).map(value => <span className="topic-tag" key={value}>{topicLabels[value] || value}</span>)}{isSynthetic(version) && <span className="demo-tag">合成演示</span>}</div><p className="row-preview">{version.abstract || '暂无摘要。'}</p><div className="row-footer"><span>{version.paper_id} · {version.version}</span></div></button> })}
        {papers.data && papers.data.total > 0 && <LibraryPagination {...papers.data} onChange={value => { navigate({ page: String(value), paper: null, id: null, version: null }); listPosition.current = { pane: 0, window: 0 }; listRef.current?.scrollTo(0, 0) }} />}
      </div><div className="detail-pane" ref={detailRef} aria-label="版本论文详情" tabIndex={-1}>
        {explicitPaper && !detail.data && <button className="mobile-back" onClick={back}>‹ 返回版本列表</button>}{detail.loading && <LibraryMessage title="正在读取此版本…" />}{detail.error && <LibraryMessage title="无法打开此版本" retry={detail.retry}>{detail.error}</LibraryMessage>}
        {detail.data && <LibraryDetail key={selectedKey} detail={detail.data} onBack={back} onVersion={selectVersion} />}
        {selectedID && !selectedVersion && <LibraryMessage title="请选择具体版本">从列表选择一个版本，即可阅读分析与原文证据。</LibraryMessage>}
        {!selected && !selectedID && !papers.loading && <LibraryMessage title="选择一篇论文">查看研究分析、原文证据与版本差异。</LibraryMessage>}
      </div></div>
      <footer className="footer"><span>推荐 / 广告 / 搜索</span><span>分析与 Agent 推断由模型生成，请结合引用原文判断。</span></footer>
    </main>
  </div>
}
