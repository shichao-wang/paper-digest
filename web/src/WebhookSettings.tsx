import { useEffect, useRef, useState } from 'react'
import { useRequest } from './hooks'
import type { Topic } from './types'

type Settings = { topic: string; configured: boolean; deliveryEnabled: boolean }

export default function WebhookSettings({ topic }: { topic: Topic }) {
  const endpoint = `/api/settings/webhook?${new URLSearchParams({ topic: topic.id })}`
  const request = useRequest<Settings>(endpoint)
  const [saved, setSaved] = useState<Settings>()
  const settings = saved || request.data
  const [webhookURL, setWebhookURL] = useState('')
  const [settingsToken, setSettingsToken] = useState('')
  const [saving, setSaving] = useState(false)
  const [confirmClear, setConfirmClear] = useState(false)
  const [feedback, setFeedback] = useState<{ error: boolean; text: string }>()
  const pending = useRef<AbortController | null>(null)
  useEffect(() => () => { pending.current?.abort() }, [])

  async function save(value: string) {
    if (pending.current) return
    const controller = new AbortController()
    pending.current = controller
    setSaving(true)
    setFeedback(undefined)
    let failure = '未能确认保存结果，请重新打开设置核对后再试。'
    try {
      const headers: Record<string, string> = { 'Content-Type': 'application/json' }
      if (settingsToken) headers.Authorization = `Bearer ${settingsToken}`
      const response = await fetch(endpoint, {
        method: 'PUT',
        headers,
        body: JSON.stringify({ webhookURL: value }),
        signal: controller.signal,
      })
      if (!response.ok) {
        failure = response.status === 400
          ? '地址格式不正确，请填写完整的 HTTPS Webhook 地址。'
          : response.status === 403
            ? '无法保存，请从本机地址打开设置；容器或代理连接还需填写正确的管理令牌。'
            : '保存失败，请确认服务正在运行后重试。'
        throw new Error('save failed')
      }
      const data = await response.json() as Settings
      if (controller.signal.aborted) return
      setSaved(data)
      setWebhookURL('')
      setConfirmClear(false)
      setFeedback({ error: false, text: data.configured ? `${topic.name}的 Webhook 已保存，后续推送将使用新地址。` : `${topic.name}的 Webhook 已清除，该主题暂停推送。` })
    } catch {
      if (controller.signal.aborted) return
      setFeedback({ error: true, text: failure })
    } finally {
      if (!controller.signal.aborted) {
        pending.current = null
        setSaving(false)
      }
    }
  }

  if (!settings) return <div className="settings-panel message" role="status">
    <h3>{request.loading ? '正在加载设置…' : '无法加载设置'}</h3>
    {request.error && <><p>{request.error}</p><button className="secondary" onClick={request.retry}>重试</button></>}
  </div>

  return <section className="settings-panel" aria-labelledby="webhook-heading">
    <div className="settings-heading"><div><h3 id="webhook-heading">飞书群机器人 · {topic.name}</h3><p>仅管理当前主题的日报推送目标。主题 ID：{settings.topic}</p></div><span className={`status ${settings.configured ? 'status-ready' : ''}`}>{settings.configured ? '已配置' : '未配置'}</span></div>
    <form onSubmit={event => { event.preventDefault(); const value = webhookURL.trim(); if (value) void save(value) }} aria-busy={saving}>
      <label className="webhook-label" htmlFor="webhook-url">{settings.configured ? '替换 Webhook 地址' : 'Webhook 地址'}</label>
      <p className="field-help" id="webhook-help">从飞书群机器人的设置中复制完整 HTTPS 地址。已保存的地址不会显示。</p>
      <input id="webhook-url" className="webhook-input" type="password" inputMode="url" autoComplete="off" spellCheck={false} maxLength={4096} placeholder="https://open.feishu.cn/open-apis/bot/v2/hook/…" aria-describedby="webhook-help" value={webhookURL} disabled={saving} onChange={event => { setWebhookURL(event.target.value); setFeedback(undefined); setConfirmClear(false) }} />
      <label className="webhook-label" htmlFor="settings-token">管理令牌（服务启用授权时必填）</label>
      <p className="field-help" id="settings-token-help">使用服务启动时配置的管理令牌。仅在当前设置页内存中保留，离开页面后需重新填写；服务未配置令牌且直接连接本机时可留空。</p>
      <input id="settings-token" className="webhook-input" type="password" autoComplete="off" spellCheck={false} maxLength={4096} aria-describedby="settings-token-help" value={settingsToken} disabled={saving} onChange={event => { setSettingsToken(event.target.value); setFeedback(undefined) }} />
      <div className="settings-actions"><button className="primary" type="submit" disabled={saving || !webhookURL.trim()}>{saving ? '正在保存…' : '保存 Webhook'}</button>{settings.configured && <button className="secondary" type="button" disabled={saving} onClick={() => { setConfirmClear(true); setFeedback(undefined) }}>清除配置</button>}</div>
    </form>
    {confirmClear && <div className="clear-confirm" role="group" aria-label="确认清除 Webhook"><p>确定清除{topic.name}的 Webhook？只会暂停该主题的推送，其他主题不受影响。</p><div className="settings-actions"><button className="secondary" disabled={saving} onClick={() => void save('')}>确认清除</button><button className="text-button" disabled={saving} onClick={() => setConfirmClear(false)}>取消</button></div></div>}
    {feedback && <p className={`settings-feedback ${feedback.error ? 'notice' : ''}`} role={feedback.error ? 'alert' : 'status'}>{feedback.text}</p>}
    <div className="settings-notes"><p>保存立即生效，不会发送测试消息或补发历史日报。</p><p>{settings.deliveryEnabled ? '当前主题自动运行已开启：每天北京时间 09:00 推送已生成的日报。' : '当前主题没有启用自动任务。保存 Webhook 不会启动采集、生成或推送。'}</p></div>
  </section>
}
