import { useRequest } from './hooks'
import type { Topic } from './types'

type Settings = { topic: string; configured: boolean; deliveryEnabled: boolean }

function TopicRow({ topic, onEdit, onRead }: { topic: Topic; onEdit: (id: string) => void; onRead: (id: string) => void }) {
  const settings = useRequest<Settings>(`/api/settings/webhook?${new URLSearchParams({ topic: topic.id })}`)
  return <tr>
    <th scope="row"><button className="topic-name" onClick={() => onRead(topic.id)}>{topic.name}</button><span className="topic-id">{topic.id}</span></th>
    <td data-label="Webhook">{settings.data ? <span className={`status ${settings.data.configured ? 'status-ready' : ''}`}>{settings.data.configured ? '已配置' : '未配置'}</span> : settings.error ? <span className="row-error">加载失败 <button className="text-button" onClick={settings.retry}>重试</button></span> : <span className="topic-id" role="status">加载中…</span>}</td>
    <td data-label="自动任务"><span className={`status ${topic.deliveryEnabled ? 'status-ready' : ''}`}>{topic.deliveryEnabled ? '已开启' : '未启用'}</span></td>
    <td><button className="secondary" aria-label={`编辑${topic.name}的推送配置`} onClick={() => onEdit(topic.id)}>编辑推送配置 <span aria-hidden="true">→</span></button></td>
  </tr>
}

export default function TopicManagement({ topics, onEdit, onRead }: { topics: Topic[]; onEdit: (id: string) => void; onRead: (id: string) => void }) {
  return <section className="topic-management" aria-labelledby="topic-list-heading">
    <div className="management-heading"><h3 id="topic-list-heading">已登记主题</h3><span>{topics.length} 个主题</span></div>
    {topics.length ? <table className="topic-table"><caption className="sr-only">各主题的 Webhook 配置和自动任务状态</caption><thead><tr><th scope="col">主题</th><th scope="col">Webhook</th><th scope="col">自动任务</th><th scope="col"><span className="sr-only">操作</span></th></tr></thead><tbody>{topics.map(topic => <TopicRow key={topic.id} topic={topic} onEdit={onEdit} onRead={onRead} />)}</tbody></table> : <p className="management-empty">尚未登记主题，请在运行配置的 topics 中添加主题后重新启动服务。</p>}
    <div className="management-notes"><p>每个主题独立保存推送地址。点击主题名称可进入阅读，点击「编辑推送配置」可保存、替换或清除 Webhook。</p><p>保存 Webhook 不会开启自动任务。主题登记和自动任务启用仍通过运行配置管理。</p></div>
  </section>
}
