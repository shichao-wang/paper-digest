import { useEffect, useRef, useState } from 'react'
import { useLocation, useRequest } from './hooks'
import type { Digest, DigestDetail, Page, Paper, Topic } from './types'
import WebhookSettings from './WebhookSettings'
import TopicManagement from './TopicManagement'

const statusLabels: Record<string, string> = {
  new: '待生成', processing: '生成中', ready: '已生成', sending: '发送中',
  sent: '已推送', unknown: '发送待核对', missed: '错过发送',
}
const day = (value: string) => value ? new Intl.DateTimeFormat('zh-CN', { timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date(value)) : '未提供'
const positivePage = (value: string | null) => /^\d+$/.test(value || '') && Number(value) > 0 && Number.isSafeInteger(Number(value)) ? Number(value) : 1

function Status({ value }: { value: string }) {
  return <span className={`status status-${value}`}>{statusLabels[value] || value}</span>
}
function Message({ title, children, retry }: { title: string; children?: React.ReactNode; retry?: () => void }) {
  return <div className="message" role="status"><h3>{title}</h3>{children && <p>{children}</p>}{retry && <button className="secondary" onClick={retry}>重试</button>}</div>
}
function Pagination({ page, total, pageSize, onChange }: { page: number; total: number; pageSize: number; onChange: (page: number) => void }) {
  const pages = Math.max(1, Math.ceil(total / pageSize))
  return <div className="pagination"><span>第 {page} 页，共 {pages} 页</span><div><button disabled={page <= 1} onClick={() => onChange(page - 1)}>上一页</button><button disabled={page >= pages} onClick={() => onChange(page + 1)}>下一页</button></div></div>
}
function PaperDetail({ paper, onBack }: { paper: Paper; onBack: () => void }) {
  const version = /^v\d+$/.test(paper.version) ? paper.version : ''
  const identifier = paper.id.replace(/^arxiv:/, '') + version
  const arxivURL = `https://arxiv.org/abs/${identifier}`
  const pdfURL = `https://arxiv.org/pdf/${identifier}`
  return <article className="paper-detail">
    <button className="mobile-back" onClick={onBack}>‹ 返回论文列表</button>
    <div className="detail-meta"><span>arXiv {paper.id}{version}</span><Status value={paper.status} /></div>
    <h2 className="paper-title">{paper.title}</h2>
    <p className="detail-authors">{paper.authors.join(', ') || '未提供作者'}</p>
    <dl className="dates"><div><dt>发表</dt><dd>{day(paper.publishedAt)}</dd></div><div><dt>更新</dt><dd>{day(paper.updatedAt)}</dd></div><div><dt>入选日报</dt><dd>{paper.digestDate}</dd></div></dl>
    <div className="source-links"><a href={arxivURL} target="_blank" rel="noreferrer">打开 arXiv <span aria-hidden="true">↗</span></a><a href={pdfURL} target="_blank" rel="noreferrer">查看 PDF <span aria-hidden="true">↗</span></a></div>
    <section className="summary-section"><div className="section-heading"><h3>中文要点</h3><span>基于公开摘要生成</span></div>
      {paper.summary ? <><div className="summary-text">{paper.summary.text}</div><details className="provenance"><summary>生成来源</summary><p>模型：{paper.summary.model || '未记录'}<br />提示版本：{paper.summary.promptVersion || '未记录'}</p></details></> : <p className="summary-missing">暂无中文要点，可查看下面的原始摘要。</p>}
    </section>
    <section className="abstract-section"><h3>原始摘要</h3><p lang="en" className="abstract-text">{paper.abstract || '未提供原始摘要。'}</p></section>
  </article>
}

export default function App() {
  const { params, navigate } = useLocation()
  const historyView = params.get('view') === 'digests'
  const settingsView = params.get('view') === 'settings'
  const managementView = params.get('view') === 'topics'
  const adminView = settingsView || managementView
  const [navigationOpen, setNavigationOpen] = useState(false)
  const menuButton = useRef<HTMLButtonElement>(null)
  useEffect(() => {
    setNavigationOpen(false)
  }, [params.toString()])
  function closeNavigation() {
    setNavigationOpen(false)
    menuButton.current?.focus()
  }
  const topics = useRequest<{ items: Topic[] }>('/api/topics')
  const requestedTopic = params.get('topic')
  const topic = requestedTopic !== null
    ? topics.data?.items.find(item => item.id === requestedTopic)
    : topics.data?.items.find(item => item.id === 'recommendation-advertising-search') || topics.data?.items[0]
  const topicID = topic?.id || ''
  useEffect(() => {
    if (requestedTopic === null && topicID) navigate({ topic: topicID }, true)
  }, [requestedTopic, topicID, navigate])
  const query = params.get('q') || ''
  const date = params.get('date') || ''
  const summary = ['available', 'missing'].includes(params.get('summary') || '') ? params.get('summary')! : 'all'
  const page = positivePage(params.get('page'))
  const [input, setInput] = useState(query)
  const listRef = useRef<HTMLDivElement>(null)
  const detailRef = useRef<HTMLDivElement>(null)
  const listPosition = useRef({ pane: 0, window: 0 })
  const returnFocus = useRef<HTMLButtonElement | null>(null)
  useEffect(() => { setInput(query) }, [query])
  const listQuery = new URLSearchParams({ topic: topicID, q: query, date, summary, page: String(page), pageSize: '20' })
  const papers = useRequest<Page<Paper>>(!topic || historyView || adminView ? null : `/api/papers?${listQuery}`)
  const digestQuery = new URLSearchParams({ topic: topicID, page: String(page), pageSize: '20' })
  const digests = useRequest<Page<Digest>>(topic && historyView ? `/api/digests?${digestQuery}` : null)
  const selectedID = params.get('paper') || ''
  const selectedDate = params.get('paperDate') || ''
  const explicitPaper = !!selectedID && !!selectedDate
  const selectedPaper = explicitPaper ? { id: selectedID, digestDate: selectedDate } : papers.data?.items[0]
  const detailQuery = selectedPaper ? new URLSearchParams({ topic: topicID, id: selectedPaper.id, date: selectedPaper.digestDate }) : null
  const paperDetail = useRequest<Paper>(topic && !historyView && !adminView && detailQuery ? `/api/papers/detail?${detailQuery}` : null)
  const selectedDigest = params.get('digest') || digests.data?.items[0]?.date || ''
  const digestDetail = useRequest<DigestDetail>(topic && historyView && selectedDigest ? `/api/digests/${encodeURIComponent(selectedDigest)}?${new URLSearchParams({ topic: topicID })}` : null)
  const explicitDigest = historyView && !!params.get('digest')
  const mobileDetail = explicitPaper || explicitDigest
  const wasDetail = useRef(false)
  useEffect(() => {
    if (wasDetail.current && !mobileDetail && window.matchMedia('(max-width: 767px)').matches) {
      requestAnimationFrame(() => window.scrollTo(0, listPosition.current.window))
    }
    wasDetail.current = mobileDetail
  }, [mobileDetail])
  function rememberList() {
    returnFocus.current = document.activeElement instanceof HTMLButtonElement ? document.activeElement : null
    listPosition.current = { pane: listRef.current?.scrollTop || 0, window: window.scrollY }
  }

  function filter(updates: Record<string, string | null>) {
    navigate({ ...updates, page: null, paper: null, paperDate: null })
    listRef.current?.scrollTo(0, 0)
  }
  function selectPaper(paper: Paper) {
    rememberList()
    navigate({ paper: paper.id, paperDate: paper.digestDate })
    detailRef.current?.scrollTo(0, 0)
    if (window.matchMedia('(max-width: 767px)').matches) window.scrollTo(0, 0)
  }
  function back() {
    navigate({ paper: null, paperDate: null, digest: null })
    requestAnimationFrame(() => {
      listRef.current?.scrollTo(0, listPosition.current.pane)
      returnFocus.current?.focus({ preventScroll: true })
    })
  }
  function selectTopic(id: string) {
    if (navigationOpen) closeNavigation()
    navigate({ topic: id, view: historyView ? 'digests' : null, q: null, date: null, summary: null, page: null, paper: null, paperDate: null, digest: null })
    listPosition.current = { pane: 0, window: 0 }
    returnFocus.current = null
    listRef.current?.scrollTo(0, 0)
    detailRef.current?.scrollTo(0, 0)
  }
  function manageTopics() {
    if (navigationOpen) closeNavigation()
    navigate({ view: 'topics', page: null, paper: null, paperDate: null, digest: null })
  }
  function editTopic(id: string) {
    navigate({ view: 'settings', topic: id, page: null, paper: null, paperDate: null, digest: null })
  }
  const selectedKey = selectedPaper ? `${topicID}/${selectedPaper.id}/${selectedPaper.digestDate}` : ''

  return <div className="app-shell">
    <a className="skip-link" href="#main-content">跳到主要内容</a>
    <header className="header">
      <div className="brand"><div className="brand-mark" aria-hidden="true"><i /><i /><i /></div><div><h1>论文日报</h1><p>每日精选，留待细读</p></div></div>
      <button className="navigation-toggle secondary" ref={menuButton} aria-expanded={navigationOpen} aria-controls="topic-navigation" onClick={() => setNavigationOpen(value => !value)}>{navigationOpen ? '收起导航' : '主题导航'} <span aria-hidden="true">☰</span></button>
    </header>
    <div className="app-layout">
    <aside id="topic-navigation" className={`sidebar ${navigationOpen ? 'is-open' : ''}`} onKeyDown={event => { if (event.key === 'Escape') closeNavigation() }}>
      <nav aria-label="主题导航"><h2 className="sidebar-heading">阅读主题</h2><div className="sidebar-topics">{topics.data?.items.map(item => <button className="sidebar-link" key={item.id} aria-current={!adminView && topicID === item.id ? 'page' : undefined} onClick={() => selectTopic(item.id)}><span className="topic-dot" aria-hidden="true" /><span>{item.name}</span></button>)}</div>
      {topics.loading && <p className="sidebar-note" role="status">正在加载主题…</p>}
      {topics.error && <p className="sidebar-note">主题加载失败 <button className="text-button" onClick={topics.retry}>重试</button></p>}
      {topics.data?.items.length === 0 && <p className="sidebar-note">尚未登记主题</p>}
      <div className="sidebar-management"><button className="sidebar-link" aria-current={adminView ? 'page' : undefined} onClick={manageTopics}><span aria-hidden="true">⚙</span><span>主题管理</span></button></div></nav>
    </aside>
    <main id="main-content">
      {settingsView && <button className="back-to-management" onClick={manageTopics}>‹ 返回主题管理</button>}
      <div className="page-heading"><div>{!adminView && <p className="eyebrow">{historyView ? '日报历史' : '论文库'}</p>}<h2>{managementView ? '主题管理' : settingsView ? '推送配置' : topic?.name || '论文阅读'}</h2><p>{managementView ? '集中查看各主题的推送配置与自动任务状态。' : settingsView ? topic?.name || '选择一个主题以编辑推送配置。' : historyView ? '按日期回看当前主题的精选论文与日报。' : '阅读当前主题的每日精选，按最近入选日期排列。'}</p></div><span className="local-label"><span aria-hidden="true" />{adminView ? '本机配置' : '本机阅读'}</span></div>
      {topics.loading && <Message title="正在加载主题…" />}
      {topics.error && <Message title="无法加载主题" retry={topics.retry}>{topics.error}</Message>}
      {topics.data && managementView && <TopicManagement topics={topics.data.items} onEdit={editTopic} onRead={selectTopic} />}
      {topics.data && !managementView && !topic && <Message title={topics.data.items.length ? '主题不存在' : '尚未配置主题'}>{topics.data.items.length ? '请从主题导航中选择，当前链接不会读写其他主题。' : '请先在运行配置的 topics 中登记主题，再重新启动服务。'}</Message>}
      {topic && !managementView && (settingsView ? <WebhookSettings key={topic.id} topic={topic} /> : <>
      <nav className="reading-tabs" aria-label="主题内容"><button aria-current={!historyView ? 'page' : undefined} onClick={() => navigate({ view: null, page: null, paper: null, paperDate: null, digest: null })}>论文库</button><button aria-current={historyView ? 'page' : undefined} onClick={() => navigate({ view: 'digests', page: null, paper: null, paperDate: null, digest: null })}>日报历史</button></nav>
      {!historyView && <form className="filters" onSubmit={event => { event.preventDefault(); filter({ q: input.trim() }) }}>
        <label className="search-field"><span className="sr-only">搜索标题、作者或摘要</span><svg width="19" height="19" viewBox="0 0 24 24" fill="none" aria-hidden="true"><circle cx="10.5" cy="10.5" r="6.5" stroke="currentColor" strokeWidth="1.7" /><path d="m16 16 5 5" stroke="currentColor" strokeWidth="1.7" /></svg><input type="search" placeholder="搜索标题、作者或摘要" value={input} onChange={event => setInput(event.target.value)} /><button type="submit">搜索</button></label>
        <label className="filter-field"><span>日报日期</span><input type="date" value={date} onChange={event => filter({ date: event.target.value })} /></label>
        <label className="filter-field"><span>中文要点</span><select value={summary} onChange={event => filter({ summary: event.target.value === 'all' ? null : event.target.value })}><option value="all">全部</option><option value="available">已有要点</option><option value="missing">暂无要点</option></select></label>
        {(query || date || summary !== 'all') && <button className="clear-filters" type="button" onClick={() => filter({ q: null, date: null, summary: null })}>清空筛选</button>}
      </form>}
      <div className={`workspace ${mobileDetail ? 'show-detail' : ''}`}>
        <div className="list-pane" ref={listRef}>
          <div className="list-heading"><span>{historyView ? '按日期浏览' : date || '全部精选'}</span><span>{historyView ? (digests.data ? `${digests.data.total} 份日报` : '') : (papers.data ? `${papers.data.total} 篇论文` : '')}</span></div>
          {historyView ? <>
            {digests.loading && <Message title="正在加载日报…" />}
            {digests.error && <Message title="无法加载日报" retry={digests.retry}>{digests.error}</Message>}
            {digests.data?.items.length === 0 && <Message title="暂无日报记录">日报任务开始运行后，历史记录会显示在这里。</Message>}
            {digests.data?.items.map(digest => <button className="digest-row" key={`${topicID}/${digest.date}`} aria-current={selectedDigest === digest.date ? 'true' : undefined} onClick={() => { rememberList(); navigate({ digest: digest.date }); detailRef.current?.scrollTo(0, 0); if (window.matchMedia('(max-width: 767px)').matches) window.scrollTo(0, 0) }}><div><h3>{digest.date}</h3><Status value={digest.status} /></div><p>{digest.paperCount} 篇入选论文，{digest.summaryCount} 篇已有中文要点</p></button>)}
            {digests.data && digests.data.total > 0 && <Pagination {...digests.data} onChange={value => { navigate({ page: String(value), digest: null }); listRef.current?.scrollTo(0, 0) }} />}
          </> : <>
            {papers.loading && <Message title="正在加载论文…" />}
            {papers.error && <Message title="无法加载论文" retry={papers.retry}>{papers.error}</Message>}
            {papers.data?.items.length === 0 && <Message title={query || date || summary !== 'all' ? '没有符合筛选条件的论文' : '暂无已保存的精选论文'}>{query || date || summary !== 'all' ? '试试其他关键词，或清空筛选。' : '这里只展示已经入选日报并保存的论文。自动运行关闭时不会新增论文。'}</Message>}
            {papers.data?.items.map(paper => <button className="paper-row" key={`${topicID}/${paper.id}/${paper.digestDate}`} aria-current={selectedKey === `${topicID}/${paper.id}/${paper.digestDate}` ? 'true' : undefined} onClick={() => selectPaper(paper)}>
              <div className="row-meta"><span>{paper.digestDate} 日报</span><span>{paper.summary ? '中文要点' : '原始摘要'}</span></div><h3 className="paper-title">{paper.title}</h3><p className="row-authors">{paper.authors.join(', ') || '未提供作者'}</p><p className="row-preview">{paper.summary?.text || paper.abstract}</p><div className="row-footer"><span>发表于 {day(paper.publishedAt)}</span><span>arXiv {paper.id}</span></div>
            </button>)}
            {papers.data && papers.data.total > 0 && <Pagination {...papers.data} onChange={value => { navigate({ page: String(value), paper: null, paperDate: null }); listRef.current?.scrollTo(0, 0) }} />}
          </>}
        </div>
        <div className="detail-pane" ref={detailRef} aria-label={historyView ? '日报详情' : '论文详情'}>
          {historyView ? <>
            {explicitDigest && <button className="mobile-back" onClick={back}>‹ 返回日报列表</button>}
            {digestDetail.loading && <Message title="正在加载日报内容…" />}
            {digestDetail.error && <Message title="无法加载日报内容" retry={digestDetail.retry}>{digestDetail.error}</Message>}
            {!selectedDigest && !digests.loading && <Message title="选择一份日报">查看当天的论文与中文要点。</Message>}
            {digestDetail.data && <article className="digest-detail"><div className="detail-meta"><span>北京时间</span><Status value={digestDetail.data.status} /></div><h2>{digestDetail.data.date} 日报</h2><p className="digest-intro">{digestDetail.data.paperCount} 篇入选，{digestDetail.data.summaryCount} 篇已有中文要点</p>
              {digestDetail.data.status === 'unknown' && <p className="notice">发送结果尚未确认。请在飞书群核对送达情况。</p>}
              {digestDetail.data.items.length === 0 && <p className="summary-missing">当天没有保存的入选论文。</p>}
              <div className="digest-papers">{digestDetail.data.items.map(paper => <button key={paper.id} onClick={() => { navigate({ view: null, date: paper.digestDate, q: null, summary: null, page: null, digest: null, paper: paper.id, paperDate: paper.digestDate }); detailRef.current?.scrollTo(0, 0) }}><h3 className="paper-title">{paper.title}</h3><p>{paper.summary ? '阅读中文要点与原始摘要' : '阅读原始摘要'}</p></button>)}</div>
              <section className="digest-message"><h3>日报正文</h3>{digestDetail.data.message ? <div className="summary-text">{digestDetail.data.message}</div> : <p className="summary-missing">暂无完整日报正文。</p>}</section>
            </article>}
          </> : <>
            {explicitPaper && !paperDetail.data && <button className="mobile-back" onClick={back}>‹ 返回论文列表</button>}
            {paperDetail.loading && <Message title="正在打开论文…" />}
            {paperDetail.error && <Message title="无法打开论文" retry={paperDetail.retry}>{paperDetail.error}</Message>}
            {paperDetail.data && <PaperDetail paper={paperDetail.data} onBack={back} />}
            {!selectedPaper && !papers.loading && <Message title="留一点时间，读一篇论文。">从左侧列表选择论文，查看中文要点和原始摘要。</Message>}
          </>}
        </div>
      </div>
      <footer className="footer"><span>内容来自 arXiv 公开摘要</span><span>中文要点由模型生成，请结合原文判断。</span></footer>
      </>)}
    </main>
    </div>
  </div>
}
