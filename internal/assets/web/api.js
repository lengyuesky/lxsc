// 统一请求与会话代际；页面模块共享取消逻辑，不在浏览器存储中持久化凭据。
const $ = s => document.querySelector(s)
const sessionState = { me: null, defaultPublic: false, epoch: 0, requests: new Set() }
const isAbort = error => error?.name === 'AbortError'
const requestAPI = async (base, path, opts = {}) => {
  const epoch = sessionState.epoch, controller = new AbortController()
  const abort = () => controller.abort()
  if (opts.signal?.aborted) abort()
  else opts.signal?.addEventListener('abort', abort, { once: true })
  sessionState.requests.add(controller)
  try {
    const o = { ...opts, headers: { ...opts.headers }, credentials: 'same-origin', signal: controller.signal }
    delete o.onEvent
    if (o.body && !(o.body instanceof FormData)) { o.headers['Content-Type'] = 'application/json'; o.body = JSON.stringify(o.body) }
    const r = await fetch(base + path, o)
    if (r.ok && opts.onEvent) {
      return await LXSCSearch.readEvents(r.body, event => {
        if (epoch !== sessionState.epoch || controller.signal.aborted) throw new DOMException('请求已取消', 'AbortError')
        opts.onEvent(event)
      })
    }
    const d = await r.json().catch(() => ({}))
    if (epoch !== sessionState.epoch || controller.signal.aborted) throw new DOMException('请求已取消', 'AbortError')
    if (r.status === 401 && path !== '/login') showLogin()
    if (!r.ok) { const error = new Error(d.error || r.statusText); error.status = r.status; throw error }
    return d
  } finally {
    sessionState.requests.delete(controller)
    opts.signal?.removeEventListener('abort', abort)
  }
}
const authAPI = (path, opts) => requestAPI('/api/auth', path, opts)
const adminAPI = (path, opts) => requestAPI('/api/admin', path, opts)
const playlistAPI = (path, opts) => requestAPI('/api/app', path, opts)
