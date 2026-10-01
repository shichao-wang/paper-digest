import { useEffect, useRef, useState } from 'react'
import { useLocation, useRequest } from './hooks'
import type { Digest, DigestDetail, Page, Paper } from './types'
import WebhookSettings from './WebhookSettings'

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
  const listQuery = new URLSearchParams({ q: query, date, summary, page: String(page), pageSize: '20' })
  const papers = useRequest<Page<Paper>>(historyView || settingsView ? null : `/api/papers?${listQuery}`)
  const digests = useRequest<Page<Digest>>(historyView ? `/api/digests?page=${page}&pageSize=20` : null)
  const selectedID = params.get('paper') || ''
  const selectedDate = params.get('paperDate') || ''
  const explicitPaper = !!selectedID && !!selectedDate
  const selectedPaper = explicitPaper ? { id: selectedID, digestDate: selectedDate } : papers.data?.items[0]
  const detailQuery = selectedPaper ? new URLSearchParams({ id: selectedPaper.id, date: selectedPaper.digestDate }) : null
  const paperDetail = useRequest<Paper>(!historyView && !settingsView && detailQuery ? `/api/papers/detail?${detailQuery}` : null)
  const selectedDigest = params.get('digest') || digests.data?.items[0]?.date || ''
  const digestDetail = useRequest<DigestDetail>(historyView && selectedDigest ? `/api/digests/${encodeURIComponent(selectedDigest)}` : null)
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
  const selectedKey = selectedPaper ? `${selectedPaper.id}/${selectedPaper.digestDate}` : ''

  return <div className="app-shell">
    <a className="skip-link" href="#main-content">跳到主要内容</a>
    <header className="header">
      <div className="brand"><div className="brand-mark" aria-hidden="true"><i /><i /><i /></div><div><h1>论文日报</h1><p>Recommendation / Advertising / Search</p></div></div>
      <nav aria-label="主导航"><button aria-current={!historyView && !settingsView ? 'page' : undefined} onClick={() => navigate({ view: null, page: null, paper: null, paperDate: null, digest: null })}>论文库</button><button aria-current={historyView ? 'page' : undefined} onClick={() => navigate({ view: 'digests', page: null, paper: null, paperDate: null, digest: null })}>日报历史</button><button aria-current={settingsView ? 'page' : undefined} onClick={() => navigate({ view: 'settings', page: null, paper: null, paperDate: null, digest: null })}>设置</button></nav>
    </header>
    <main id="main-content">
      <div className="page-heading"><div><h2>{settingsView ? '设置' : historyView ? '日报历史' : '论文库'}</h2><p>{settingsView ? '管理日报的飞书推送地址。' : historyView ? '按日期回看每天的精选论文与日报。' : '每日精选，留待细读。按最近入选日期排列。'}</p></div><span className="local-label"><span aria-hidden="true" />{settingsView ? '本机配置' : '本机阅读'}</span></div>
      {settingsView ? <WebhookSettings /> : <>
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
            {digests.data?.items.map(digest => <button className="digest-row" key={digest.date} aria-current={selectedDigest === digest.date ? 'true' : undefined} onClick={() => { rememberList(); navigate({ digest: digest.date }); detailRef.current?.scrollTo(0, 0); if (window.matchMedia('(max-width: 767px)').matches) window.scrollTo(0, 0) }}><div><h3>{digest.date}</h3><Status value={digest.status} /></div><p>{digest.paperCount} 篇入选论文，{digest.summaryCount} 篇已有中文要点</p></button>)}
            {digests.data && digests.data.total > 0 && <Pagination {...digests.data} onChange={value => { navigate({ page: String(value), digest: null }); listRef.current?.scrollTo(0, 0) }} />}
          </> : <>
            {papers.loading && <Message title="正在加载论文…" />}
            {papers.error && <Message title="无法加载论文" retry={papers.retry}>{papers.error}</Message>}
            {papers.data?.items.length === 0 && <Message title={query || date || summary !== 'all' ? '没有符合筛选条件的论文' : '暂无已保存的精选论文'}>{query || date || summary !== 'all' ? '试试其他关键词，或清空筛选。' : '这里只展示已经入选日报并保存的论文。自动运行关闭时不会新增论文。'}</Message>}
            {papers.data?.items.map(paper => <button className="paper-row" key={`${paper.id}/${paper.digestDate}`} aria-current={selectedKey === `${paper.id}/${paper.digestDate}` ? 'true' : undefined} onClick={() => selectPaper(paper)}>
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
      </>}
    </main>
  </div>
}
