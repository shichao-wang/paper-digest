import type { ReactNode } from 'react'
import type { LibraryIdentity, LibraryVersion } from './types'

export const topicLabels: Record<string, string> = { recommendation: '推荐', advertising: '广告', search: '搜索' }
export const relevanceLabels: Record<string, string> = { direct: '直接相关', unrelated: '不相关', uncertain: '待判断', pending: '尚未筛选', all: '全部候选' }
export const taskLabels: Record<string, string> = {
  pending: '待处理', running: '处理中', succeeded: '已完成', retry: '等待重试', failed: '失败',
  paused: '已暂停', blocked: '受阻', cancelled: '已取消', not_applicable: '不适用', ready: '已完成',
  queued: '待处理', retry_wait: '等待重试', completed: '已完成',
}
export const stageLabels: Record<string, string> = { metadata: '版本元数据', relevance: '相关性筛选', document: '全文提取', analyze: '全文分析', compare: '版本比较' }
export const qualityLabels: Record<string, string> = { ready: '可用于全文分析', good: '可用', usable: '可用', verified: '已核验', passed: '已通过', ok: '可用', poor: '质量不足', blocked: '质量受阻', partial: '部分可用', failed: '提取失败', missing: '未获取' }
export const completenessLabels: Record<string, string> = { complete: '完整', partial: '不完整', incomplete: '不完整', failed: '获取失败', empty: '空批次', uncertain: '完整性未确认', unknown: '完整性未确认' }
export function dateLabel(value: string) {
  if (!value) return '未记录'
  if (/^\d{4}-\d{2}-\d{2}$/.test(value)) return value
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : new Intl.DateTimeFormat('zh-CN', { timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }).format(date)
}
export function positivePage(value: string | null) {
  const n = Number(value)
  return /^\d+$/.test(value || '') && n > 0 && Number.isSafeInteger(n) ? n : 1
}
export const identityKey = (identity: LibraryIdentity) => `${identity.source}/${identity.paper_id}/${identity.version}`
export const apiID = (identity: LibraryIdentity) => identity.paper_id.startsWith(`${identity.source}:`) ? identity.paper_id : `${identity.source}:${identity.paper_id}`
export const isSynthetic = (version: LibraryVersion) => /synthetic|demo/i.test(version.origin)
export function safeURL(value: string) {
  try { const url = new URL(value); return ['https:', 'http:'].includes(url.protocol) ? url.href : undefined } catch { return undefined }
}
export function LibraryBadge({ value, labels = taskLabels }: { value: string; labels?: Record<string, string> }) {
  return <span className={`status status-${value}`}>{labels[value] || value || '未记录'}</span>
}
export function LibraryMessage({ title, children, retry }: { title: string; children?: ReactNode; retry?: () => void }) {
  return <div className="message" role="status"><h3>{title}</h3>{children && <p>{children}</p>}{retry && <button className="secondary" onClick={retry}>重试</button>}</div>
}
export function LibraryPagination({ page, total, pageSize, onChange }: { page: number; total: number; pageSize: number; onChange: (page: number) => void }) {
  const pages = Math.max(1, Math.ceil(total / Math.max(1, pageSize)))
  return <div className="pagination"><span>第 {page} 页，共 {pages} 页</span><div><button disabled={page <= 1} onClick={() => onChange(page - 1)}>上一页</button><button disabled={page >= pages} onClick={() => onChange(page + 1)}>下一页</button></div></div>
}
