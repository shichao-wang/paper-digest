import { useCallback, useEffect, useState } from 'react'

export function useLocation() {
  const [search, setSearch] = useState(window.location.search)
  useEffect(() => {
    const update = () => setSearch(window.location.search)
    window.addEventListener('popstate', update)
    window.addEventListener('app-location-change', update)
    return () => {
      window.removeEventListener('popstate', update)
      window.removeEventListener('app-location-change', update)
    }
  }, [])
  const navigate = useCallback((updates: Record<string, string | null>, replace = false) => {
    const params = new URLSearchParams(window.location.search)
    for (const [key, value] of Object.entries(updates)) {
      if (value === null || value === '') params.delete(key)
      else params.set(key, value)
    }
    const query = params.toString()
    const url = `${window.location.pathname}${query ? `?${query}` : ''}`
    if (replace) window.history.replaceState(null, '', url)
    else window.history.pushState(null, '', url)
    setSearch(window.location.search)
    window.dispatchEvent(new Event('app-location-change'))
  }, [])
  return { params: new URLSearchParams(search), navigate }
}

export function useRequest<T>(url: string | null) {
  const [attempt, setAttempt] = useState(0)
  const [result, setResult] = useState<{ url: string; data?: T; error?: string; loading: boolean }>()
  useEffect(() => {
    if (!url) return
    const controller = new AbortController()
    let active = true
    setResult({ url, loading: true })
    fetch(url, { signal: controller.signal })
      .then(async response => {
        if (!response.ok) throw new Error(response.status === 404 ? '这条记录不存在，请返回列表选择其他内容。' : '暂时无法加载，请重试。')
        return response.json() as Promise<T>
      })
      .then(data => { if (active) setResult({ url, data, loading: false }) })
      .catch(error => { if (active) setResult({ url, error: error instanceof Error && error.message.startsWith('这条记录不存在') ? error.message : '暂时无法加载这份内容。请确认服务正在运行，然后重试。', loading: false }) })
    return () => { active = false; controller.abort() }
  }, [url, attempt])
  return {
    data: result?.url === url ? result.data : undefined,
    error: result?.url === url ? result.error : undefined,
    loading: !!url && (result?.url !== url || result.loading),
    retry: () => setAttempt(value => value + 1),
  }
}
