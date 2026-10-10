import { useEffect, useRef, useState, type FormEvent } from 'react'

interface EvalCandidate {
  id: string
  url: string
  title: string
  authors: string[]
  abstract: string
  published: string
  categories: string[]
  tier: number
  signals: string[]
  reason: string
  selected: boolean
  position: number
  looseSelected: boolean
  loosePosition: number
  inDigest: boolean
  label: string
  draftSelected: boolean
  draftPosition: number
  draftTier: number
  draftSignals: string[]
  draftReason: string
}

interface DraftComparison {
  selected: EvalCandidate[]
  onlyDraft: string[]
  onlyActive: string[]
  score: EvalScore
}

interface EvalScore {
  selected: number
  labeled: number
  relevant: number
  notRelevant: number
  precision: number | null
}

interface EvalDay {
  date: string
  lookbackDays: number
  selected: EvalCandidate[]
  loose: EvalCandidate[]
  sent: { id: string; title: string }[]
  candidates: EvalCandidate[]
  onlyCurrent: string[]
  onlyLoose: string[]
  onlySent: string[]
  score: EvalScore
  draft?: DraftComparison
}

interface EvalReport {
  topic: string
  days: EvalDay[]
}

const reasons: Record<string, string> = {
  'title match': '标题命中',
  'cs.IR with a technique signal': 'cs.IR 且有技术信号',
  'abstract phrase only': '仅摘要中的强短语',
  'private information retrieval is not search': '私有信息检索，不是搜索',
  'recommendation appears only as an ordinary word': 'recommendation 只是普通用词',
  'cs.IR alone is not a topic match': '仅有 cs.IR，不是主题命中',
  'search or ads appears only as an ordinary word': 'search 或 ads 只是普通用词',
  'no topic signal': '没有主题信号',
}

function shanghaiToday() {
  return new Intl.DateTimeFormat('en-CA', { timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date())
}

function reasonText(reason: string) {
  return reasons[reason] || reason
}

function scoreOf(selected: EvalCandidate[]): EvalScore {
  let labeled = 0
  let relevant = 0
  let notRelevant = 0
  for (const item of selected) {
    if (item.label === 'relevant') {
      labeled += 1
      relevant += 1
    } else if (item.label === 'not_relevant') {
      labeled += 1
      notRelevant += 1
    }
  }
  return { selected: selected.length, labeled, relevant, notRelevant, precision: labeled > 0 ? relevant / labeled : null }
}

function withLabel(report: EvalReport, id: string, label: string): EvalReport {
  const apply = (item: EvalCandidate) => item.id === id ? { ...item, label } : item
  return {
    ...report,
    days: report.days.map(day => {
      const selected = day.selected.map(apply)
      const draft = day.draft ? { ...day.draft, selected: day.draft.selected.map(apply), score: scoreOf(day.draft.selected.map(apply)) } : undefined
      return { ...day, selected, loose: day.loose.map(apply), candidates: day.candidates.map(apply), draft, score: scoreOf(selected) }
    }),
  }
}

function titleOf(day: EvalDay, id: string) {
  return day.candidates.find(item => item.id === id)?.title || day.sent.find(item => item.id === id)?.title || id
}

function arxivURL(item: EvalCandidate) {
  return item.url || `https://arxiv.org/abs/${item.id.replace(/^arxiv:/, '')}`
}

export default function EvalReview({ topicID }: { topicID: string }) {
  const [date, setDate] = useState(shanghaiToday)
  const [end, setEnd] = useState('')
  const [report, setReport] = useState<EvalReport | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [labelError, setLabelError] = useState('')
  const [pendingID, setPendingID] = useState('')
  const [rulesText, setRulesText] = useState('')
  const [activeRules, setActiveRules] = useState('')
  const [hasRules, setHasRules] = useState<boolean | null>(null)
  const [rulesError, setRulesError] = useState('')
  const abort = useRef<AbortController | null>(null)
  useEffect(() => () => abort.current?.abort(), [])
  useEffect(() => {
    const controller = new AbortController()
    setHasRules(null)
    setRulesError('')
    setReport(null)
    fetch(`/api/eval/rules?${new URLSearchParams({ topic: topicID })}`, { signal: controller.signal })
      .then(async response => {
        if (response.status === 400) {
          setHasRules(false)
          return
        }
        if (!response.ok) throw new Error('无法读取当前筛选规则。')
        const text = JSON.stringify(await response.json(), null, 2)
        setActiveRules(text)
        setRulesText(text)
        setHasRules(true)
      })
      .catch(caught => {
        if (caught instanceof DOMException && caught.name === 'AbortError') return
        setHasRules(false)
        setRulesError(caught instanceof Error ? caught.message : '无法读取当前筛选规则。')
      })
    return () => controller.abort()
  }, [topicID])

  async function preview(mode: 'active' | 'draft') {
    abort.current?.abort()
    const controller = new AbortController()
    abort.current = controller
    setLoading(true)
    setError('')
    setLabelError('')
    try {
      let response: Response
      if (mode === 'draft') {
        let rules: unknown
        try {
          rules = JSON.parse(rulesText)
        } catch {
          throw new Error('草稿不是合法的 JSON。')
        }
        response = await fetch(`/api/eval?${new URLSearchParams({ topic: topicID })}`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ date, to: end, rules }),
          signal: controller.signal,
        })
      } else {
        const query = new URLSearchParams({ date, topic: topicID })
        if (end) query.set('to', end)
        response = await fetch(`/api/eval?${query}`, { signal: controller.signal })
      }
      if (!response.ok) {
        const payload = await response.json().catch(() => null) as { error?: string } | null
        if (payload?.error && payload.error !== 'invalid query parameters') throw new Error(payload.error)
        throw new Error(response.status === 400 ? '日期无效，或一次最多预览 14 天。' : '暂时无法完成预览。请确认本机可以访问 arXiv 后重试。')
      }
      setReport(await response.json() as EvalReport)
    } catch (caught) {
      if (caught instanceof DOMException && caught.name === 'AbortError') return
      setError(caught instanceof Error ? caught.message : '暂时无法完成预览。')
    } finally {
      if (abort.current === controller) setLoading(false)
    }
  }

  function runPreview(event: FormEvent) {
    event.preventDefault()
    void preview('active')
  }

  async function label(item: EvalCandidate, value: 'relevant' | 'not_relevant' | 'clear') {
    setPendingID(item.id)
    setLabelError('')
    const snapshot = {
      ID: item.id,
      Title: item.title,
      Abstract: item.abstract,
      Authors: item.authors,
      Published: item.published,
      URL: item.url,
      categories: item.categories,
    }
    try {
      const response = await fetch(`/api/eval/labels?${new URLSearchParams({ topic: topicID })}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ paperID: item.id, label: value, snapshot: value === 'clear' ? null : snapshot }),
      })
      if (!response.ok) throw new Error(response.status === 403 ? '请从本机地址打开页面后再标注。' : '没能保存这条判断。')
      setReport(current => current ? withLabel(current, item.id, value === 'clear' ? '' : value) : current)
    } catch (caught) {
      setLabelError(caught instanceof Error ? caught.message : '没能保存这条判断。')
    } finally {
      setPendingID('')
    }
  }

  if (hasRules === null) {
    return <div className="message" role="status"><h3>正在读取筛选规则</h3><p>只读取本机配置，不会抓取 arXiv。</p></div>
  }
  if (!hasRules) {
    return <div className="message" role="status"><h3>当前主题没有筛选规则</h3><p>{rulesError || '请从主题导航切换到已配置筛选规则的主题。联合主题在省略 selection 时使用内置规则。'}</p></div>
  }

  return <div className="eval-review">
    <p className="eval-note">按当天北京时间 09:00 的截止时刻运行规则。不生成摘要，不发送飞书，不把论文标为已读，也不写入日报任务。草稿只用于这次对照，不会改 config.json。打开本页不会自动抓取。</p>
    <div className="rules-editor">
      <label className="filter-field"><span>筛选规则 JSON</span><textarea value={rulesText} spellCheck={false} onChange={event => setRulesText(event.target.value)} aria-label="筛选规则 JSON" /></label>
      <div className="rules-actions">
        <button type="button" className="secondary" disabled={rulesText === activeRules} onClick={() => setRulesText(activeRules)}>恢复为当前规则</button>
      </div>
    </div>
    <form className="eval-form" onSubmit={runPreview}>
      <label className="filter-field"><span>开始日期</span><input type="date" required value={date} onChange={event => setDate(event.target.value)} /></label>
      <label className="filter-field"><span>结束日期</span><input type="date" value={end} onChange={event => setEnd(event.target.value)} /></label>
      <button type="submit" disabled={loading || !date}>{loading ? '正在预览…' : '运行当前规则'}</button>
      <button type="button" disabled={loading || !date} onClick={() => void preview('draft')}>用草稿对照</button>
      <a href={`/api/eval/fixtures?${new URLSearchParams({ topic: topicID })}`}>导出已标注样本</a>
    </form>
    {loading && <div className="message" role="status"><h3>正在按当前规则预览</h3><p>只读取 arXiv 公开元数据和本机标注，不会生成摘要或发送飞书。</p></div>}
    {error && <div className="message" role="status"><h3>无法完成预览</h3><p>{error}</p></div>}
    {labelError && <p className="row-error" role="status">{labelError}</p>}
    {report?.days.map(day => <section key={day.date} className="eval-day">
      <div className="eval-score">
        <strong>{day.date}</strong> 回看 {day.lookbackDays} 天。当前入选 {day.score.selected} 篇，宽松基线 {day.loose.length} 篇，当日已保存 {day.sent.length} 篇。
        {day.score.labeled === 0 ? ' 已标注入选 0 篇，精确率暂无。' : ` 已标注入选 ${day.score.labeled} 篇，其中相关 ${day.score.relevant}、不相关 ${day.score.notRelevant}，精确率 ${day.score.precision?.toFixed(2)}。`}
        {day.draft ? (day.draft.score.labeled === 0 ? ` 草稿入选 ${day.draft.score.selected} 篇，已标注 0 篇，精确率暂无。` : ` 草稿入选 ${day.draft.score.selected} 篇，已标注 ${day.draft.score.labeled} 篇，其中相关 ${day.draft.score.relevant}、不相关 ${day.draft.score.notRelevant}，精确率 ${day.draft.score.precision?.toFixed(2)}。`) : ''}
      </div>
      <h3>当前入选</h3>
      {day.selected.length === 0 ? <p className="eval-note">这一天没有入选论文。</p> : <ol className="eval-selected">{day.selected.map(item => <li key={item.id}><a href={arxivURL(item)} target="_blank" rel="noreferrer">{item.title}</a> <span className="eval-meta">#{item.position} · tier {item.tier} · {item.id}</span></li>)}</ol>}
      {day.draft && <><h3>草稿入选</h3>{day.draft.selected.length === 0 ? <p className="eval-note">草稿没有入选论文。</p> : <ol className="eval-selected">{day.draft.selected.map(item => <li key={item.id}><a href={arxivURL(item)} target="_blank" rel="noreferrer">{item.title}</a> <span className="eval-meta">#{item.draftPosition} · tier {item.draftTier} · {item.id}</span></li>)}</ol>}</>}
      <div className="eval-diff">
        <section><h3>仅当前规则</h3>{idList(day, day.onlyCurrent)}</section>
        <section><h3>仅宽松基线</h3>{idList(day, day.onlyLoose)}</section>
        <section><h3>仅当日已保存</h3>{idList(day, day.onlySent)}</section>
        {day.draft && <section><h3>仅草稿</h3>{idList(day, day.draft.onlyDraft)}</section>}
        {day.draft && <section><h3>仅当前规则（草稿未入选）</h3>{idList(day, day.draft.onlyActive)}</section>}
      </div>
      <h3>全部候选</h3>
      {day.candidates.map(item => <article className="eval-candidate" key={item.id}>
        <header><h3><a href={arxivURL(item)} target="_blank" rel="noreferrer">{item.title}</a></h3><span className="eval-meta">{item.selected ? `入选 #${item.position}` : '未入选'} · tier {item.tier}</span></header>
        <p className="eval-meta">{item.id} · {(item.categories || []).join(' ') || '无分类'}{item.looseSelected ? ` · 宽松 #${item.loosePosition}` : ''}{item.inDigest ? ' · 当日已保存' : ''}</p>
        <p className="eval-meta">信号：{(item.signals || []).join(', ') || '无'}</p>
        <p className="eval-meta">原因：{reasonText(item.reason)}</p>
        {day.draft && <p className="eval-meta">草稿：{item.draftSelected ? `入选 #${item.draftPosition}` : '未入选'} · tier {item.draftTier} · {reasonText(item.draftReason)}{(item.draftSignals || []).length ? ` · ${(item.draftSignals || []).join(', ')}` : ''}</p>}
        <div className="eval-actions">
          <button type="button" className="secondary" aria-pressed={item.label === 'relevant'} disabled={pendingID === item.id} onClick={() => label(item, 'relevant')}>相关</button>
          <button type="button" className="secondary" aria-pressed={item.label === 'not_relevant'} disabled={pendingID === item.id} onClick={() => label(item, 'not_relevant')}>不相关</button>
          <button type="button" className="secondary" disabled={pendingID === item.id || !item.label} onClick={() => label(item, 'clear')}>清除</button>
        </div>
      </article>)}
    </section>)}
  </div>
}

function idList(day: EvalDay, ids: string[]) {
  if (!ids?.length) return <p className="eval-note">无</p>
  return <ul>{ids.map(id => <li key={id}>{titleOf(day, id)}</li>)}</ul>
}
