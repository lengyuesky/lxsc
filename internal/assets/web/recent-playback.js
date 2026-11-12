// 最近播放与听歌汇总分别读取，翻页和详情不会重建日期控件。
;(function (root) {
  class RecentLogState {
    constructor(api, Gate) {
      this.api = api; this.gate = new Gate(); this.detailRequests = new Set(); this.revision = 0
      this.reset()
    }
    reset() {
      this.gate.cancel(); this.revision++
      for (const controller of this.detailRequests) controller.abort()
      this.detailRequests.clear()
      this.params = {}; this.signature = ''; this.items = []; this.cursor = ''; this.sources = []; this.clients = []
      this.loading = false; this.loaded = false; this.paged = false; this.error = ''; this.generatedAt = 0
    }
    select(params) {
      const signature = JSON.stringify(Object.entries(params).sort(([a], [b]) => a.localeCompare(b)))
      if (signature === this.signature) return false
      this.reset(); this.params = { ...params }; this.signature = signature
      return true
    }
    async load({ more = false, preserve = this.paged } = {}) {
      if (more && (!this.cursor || this.loading)) return false
      const request = this.gate.begin()
      this.loading = true; this.error = ''
      const oldCursor = this.cursor, params = new URLSearchParams(this.params)
      params.set('limit', '50')
      if (more) params.set('cursor', oldCursor)
      try {
        const data = await this.api('/listening/recent?' + params, { signal: request.signal })
        if (!request.current()) return false
        if (more || preserve) {
          const cutoff = data.generatedAt - Math.min(Number(this.params.days || 30), 30) * 86400000
          const items = new Map(this.items.filter(item => item.startedAt >= cutoff).map(item => [item.id, item]))
          for (const item of data.items) items.set(item.id, item)
          this.items = [...items.values()].sort((a, b) => b.startedAt - a.startedAt || b.id - a.id)
        } else this.items = data.items
        this.cursor = preserve && !more ? oldCursor : data.nextCursor
        this.sources = data.sources; this.clients = data.clients; this.generatedAt = data.generatedAt
        this.loaded = true; this.paged ||= more
        return true
      } catch (error) {
        if (!request.current() || error.name === 'AbortError') return false
        this.error = error.message
        return false
      } finally { if (request.current()) this.loading = false }
    }
    async detail(id) {
      const controller = new AbortController(), revision = this.revision
      this.detailRequests.add(controller)
      try {
        const data = await this.api('/listening/recent/' + encodeURIComponent(id) + '?' + new URLSearchParams({ scope: this.params.scope || 'me', ...(this.params.userId ? { userId: this.params.userId } : {}) }), { signal: controller.signal })
        return revision === this.revision && !controller.signal.aborted ? data : null
      } finally { this.detailRequests.delete(controller) }
    }
  }

  const stageNames = { metadata: '读取歌曲', resolve: '音源取链', source_fallback: '尝试备用音源', refresh: '刷新失效直链', url_check: '验证音频', cache_check: '验证缓存音频', proxy_response: '代理响应验证', cross_source: '跨平台兜底', redirect: '交付直链', proxy_copy: '代理传输' }
  const errorNames = { timeout: '超时', cancelled: '已取消', busy: '服务繁忙', invalid_response: '无效响应', non_audio: '返回内容不是音频', non_audio_response: '返回内容不是音频', invalid_audio: '音频验证失败', decode_failed: '音频解码失败', decoder_unavailable: '解码器不可用', expired: '直链已失效', rate_limited: '上游限流', source_auth: '音源认证失效', source_error: '音源不可用', script: '脚本调用失败', upstream_status: '上游响应被拒绝', unsupported_quality: '不支持此音质', not_found: '歌曲不存在' }
  function outcome(item) {
    if (item.outcome === 'success') return { tone: 'ok', label: item.delivery === '302' ? '直链已交付' : '传输完成' }
    return { pending: { tone: 'pri', label: '准备／传输中' }, failed: { tone: 'err', label: '交付失败' }, interrupted: { tone: 'warn', label: '传输中断' }, reported: { tone: 'off', label: '开始播放上报' }, unknown: { tone: 'warn', label: '结束结果未确认' } }[item.outcome] || { tone: 'off', label: '结果未知' }
  }
  if (typeof module === 'object' && module.exports) { module.exports = { RecentLogState, outcome }; return }

  const state = new RecentLogState((...args) => playlistAPI(...args), root.LXSCMusic.RequestGate)
  const timeFormatter = new Intl.DateTimeFormat('zh-CN', { timeZone: 'Asia/Shanghai', dateStyle: 'short', timeStyle: 'medium' })
  const timeText = value => Number.isFinite(Number(value)) && Number(value) > 0 ? timeFormatter.format(new Date(value)) : '—'
  const platformText = value => platName[value] || value || '未确认'
  let panel = null
  function scopeParams() {
    const scope = $('#listeningScope').value
    return { scope: scope.startsWith('user:') ? 'user' : scope, ...(scope.startsWith('user:') ? { userId: scope.slice(5) } : {}), days: $('#listeningRange').value }
  }
  function filters() {
    const values = Object.fromEntries(new FormData(panel.querySelector('form')))
    return Object.fromEntries(Object.entries({ ...scopeParams(), ...values }).filter(([, value]) => value !== ''))
  }
  function ensurePanel() {
    if (panel) return
    panel = document.createElement('section'); panel.id = 'recentPlaybackPanel'; panel.className = 'card recent-playback'; panel.setAttribute('aria-label', '最近播放')
    panel.innerHTML = `<div class="section-title recent-playback-head"><div><h3>最近播放</h3><p>查看歌曲、音源与客户端的交付情况</p></div><button type="button" class="sec sm" id="recentPlaybackRefresh"><svg class="icon"><use href="#i-refresh"/></svg>刷新</button></div>
      <form class="recent-playback-filters" aria-label="播放日志筛选"><label class="recent-query">歌曲／歌手<input name="query" id="recentPlaybackQuery" type="search" maxlength="200" placeholder="搜索最近播放" autocomplete="off"></label><label>音源<select name="sourceId" id="recentPlaybackSource"><option value="">全部音源</option></select></label><label>客户端<select name="client" id="recentPlaybackClient"><option value="">全部客户端</option></select></label><label>交付方式<select name="delivery" id="recentPlaybackDelivery"><option value="">全部方式</option><option value="302">302 直链</option><option value="proxy">代理</option></select></label><label>结果<select name="outcome" id="recentPlaybackOutcome"><option value="">全部结果</option><option value="success">交付成功</option><option value="failed">交付失败</option><option value="interrupted">传输中断</option><option value="pending">准备／传输中</option><option value="reported">开始播放上报</option><option value="unknown">结束结果未确认</option></select></label><button type="submit" class="sec">筛选</button></form>
      <div id="recentPlaybackStatus" class="recent-playback-status muted hint" role="status"></div><div class="table-wrap recent-playback-table"><table><thead><tr><th>时间</th><th>歌曲</th><th>用户</th><th>音源／取链平台</th><th>客户端</th><th>交付</th><th>结果</th><th>详情</th></tr></thead><tbody id="recentPlaybackRows"></tbody></table></div><div class="recent-playback-more"><button type="button" class="sec sm hidden" id="recentPlaybackMore">加载更多</button></div><p class="recent-playback-note">日志保留最近 30 天，按北京时间显示。302 表示服务端交付了直链，代理完成表示服务器传输结束；实际收听量以听歌统计的口径为准。</p>`
    panel.querySelector('form').onsubmit = event => { event.preventDefault(); state.select(filters()); load() }
    panel.querySelector('form').onchange = event => { if (event.target.matches('select')) { state.select(filters()); load() } }
    panel.querySelector('#recentPlaybackRefresh').onclick = () => load({ preserve: state.paged })
    panel.querySelector('#recentPlaybackMore').onclick = () => load({ more: true })
    panel.addEventListener('click', event => { if (event.target.closest('[data-recent-retry]')) load({ preserve: state.paged }) })
    panel.addEventListener('toggle', event => {
      const details = event.target
      if (details.matches('details[data-playback-details]')) {
        const row = panel.querySelector(`[data-playback-detail-row="${details.dataset.playbackDetails}"]`)
        if (row) row.hidden = !details.open
        if (details.open) loadDetail(details)
      }
    }, true)
  }
  function setOptions(selector, options, emptyLabel, selected) {
    const select = panel.querySelector(selector)
    if (document.activeElement === select) return
    const values = options.map(option => typeof option === 'string' ? { value: option, label: option } : option)
    if (selected && !values.some(option => String(option.value) === selected)) values.push({ value: selected, label: selector.includes('Source') ? '音源 #' + selected : selected })
    select.innerHTML = `<option value="">${emptyLabel}</option>` + values.map(option => `<option value="${esc(option.value)}">${esc(option.label)}</option>`).join('')
    select.value = selected
  }
  function anchor() {
    const row = [...(panel?.querySelectorAll('[data-playback-row],[data-playback-detail-row]') || [])].find(row => { const box = row.getBoundingClientRect(); return box.height > 0 && box.bottom > 0 && box.top < innerHeight })
    return row ? { id: row.dataset.playbackRow || row.dataset.playbackDetailRow, detail: !!row.dataset.playbackDetailRow, y: row.getBoundingClientRect().top } : null
  }
  function render(readAnchor = null) {
    if (!panel?.isConnected) return
    const body = panel.querySelector('#recentPlaybackRows'), oldRows = new Map([...body.querySelectorAll('[data-playback-row]')].map(row => [Number(row.dataset.playbackRow), row])), oldDetails = new Map([...body.querySelectorAll('[data-playback-detail-row]')].map(row => [Number(row.dataset.playbackDetailRow), row]))
    const focus = panel.contains(document.activeElement) ? document.activeElement : null
    const rows = state.items.flatMap(item => {
      const row = oldRows.get(item.id) || document.createElement('tr'), previousDetails = row.querySelector('details')
      row.dataset.playbackRow = item.id
      const result = outcome(item)
      row.innerHTML = `<td data-label="时间"><time datetime="${new Date(item.startedAt).toISOString()}">${esc(timeText(item.startedAt))}</time><small>${item.kind === 'playback' ? '播放尝试' : '媒体请求'}</small></td><td data-label="歌曲" class="recent-song"><strong title="${esc(item.name)}">${esc(item.name || '未知歌曲')}</strong><small>${esc(item.singer || '未知歌手')}</small></td><td data-label="用户">${esc(item.userName)}</td><td data-label="音源／平台"><strong>${esc(item.sourceName || '未确认音源')}</strong><small>${esc(platformText(item.platform))}${item.quality ? ' · ' + esc(item.quality) : ''}${item.cached ? ' · 缓存命中' : ''}</small></td><td data-label="客户端">${esc(item.client)}<small>${esc({ web: '网页', fnmusic: '飞牛协议', subsonic: 'Subsonic' }[item.protocol] || item.protocol)}</small></td><td data-label="交付">${esc({ '302': '302 直链', proxy: '代理' }[item.delivery] || '未观测到交付')}</td><td data-label="结果"><span class="badge ${result.tone}">${result.label}</span>${item.status ? `<small>HTTP ${item.status}</small>` : ''}</td><td class="recent-details-cell"><details data-playback-details="${esc(item.id)}"><summary aria-controls="recentPlaybackDetail-${esc(item.id)}">详情</summary></details></td>`
      if (previousDetails) row.querySelector('details').replaceWith(previousDetails)
      const detailRow = oldDetails.get(item.id) || document.createElement('tr')
      detailRow.dataset.playbackDetailRow = item.id; detailRow.className = 'recent-detail-row'; detailRow.hidden = !row.querySelector('details').open
      if (!oldDetails.has(item.id)) detailRow.innerHTML = `<td colspan="8"><div id="recentPlaybackDetail-${esc(item.id)}" class="recent-playback-detail"></div></td>`
      return [row, detailRow]
    })
    if (rows.length) body.replaceChildren(...rows)
    else body.innerHTML = `<tr><td colspan="8" class="center muted">${state.loading ? '正在读取最近播放…' : '暂无符合条件的最近播放记录'}</td></tr>`
    panel.querySelector('#recentPlaybackRefresh').disabled = state.loading
    const more = panel.querySelector('#recentPlaybackMore')
    more.disabled = state.loading; more.classList.toggle('hidden', !state.cursor); more.textContent = state.loading ? '正在读取…' : '加载更多'
    const status = panel.querySelector('#recentPlaybackStatus')
    status.innerHTML = state.error ? `${esc(state.error)}${state.items.length ? '；保留上次记录。' : ''} <button type="button" class="ghost sm" data-recent-retry>重试</button>` : state.loading ? '正在读取最近播放…' : `更新于 ${esc(timeText(state.generatedAt))} · 已显示 ${state.items.length} 条${state.paged ? ' · 正在查看已加载记录，可手动刷新' : ''}`
    status.classList.toggle('err', !!state.error)
    setOptions('#recentPlaybackSource', state.sources.map(source => ({ value: source.id, label: source.name })), '全部音源', String(state.params.sourceId || ''))
    setOptions('#recentPlaybackClient', state.clients, '全部客户端', state.params.client || '')
    if (focus?.isConnected && document.activeElement !== focus) focus.focus({ preventScroll: true })
    if (readAnchor) {
      const row = panel.querySelector(`[data-playback-${readAnchor.detail ? 'detail-row' : 'row'}="${readAnchor.id}"]`)
      if (row) scrollBy(0, row.getBoundingClientRect().top - readAnchor.y)
    }
    for (const details of panel.querySelectorAll('details[open]')) {
      const item = state.items.find(item => String(item.id) === details.dataset.playbackDetails)
      if (item && details.dataset.loadedAt && details.dataset.loadedAt !== String(item.updatedAt)) loadDetail(details, true)
    }
  }
  async function load(options = {}) {
    if (!sessionState.me || !panel?.isConnected) return
    const revision = state.revision, currentPanel = panel, readAnchor = anchor()
    const promise = state.load(options)
    render(readAnchor)
    await promise
    if (revision === state.revision && currentPanel === panel) render(anchor())
  }
  async function loadDetail(details, force = false) {
    if (details.dataset.loading || (!force && details.dataset.loadedAt)) return
    const id = details.dataset.playbackDetails, body = panel.querySelector(`[data-playback-detail-row="${id}"] .recent-playback-detail`), revision = state.revision
    details.dataset.loading = 'true'; body.textContent = '正在读取播放详情…'
    try {
      const data = await state.detail(id)
      if (!data || revision !== state.revision || !details.isConnected) return
      const result = outcome(data)
      body.innerHTML = `<div class="recent-detail-summary"><strong>${esc(data.name || '未知歌曲')} · ${result.label}</strong><span>${esc(data.client)} · ${data.requests} 次媒体请求</span><span>歌曲平台：${esc(platformText(data.requestedPlatform))} · 取链平台：${esc(platformText(data.platform))}</span><span>请求音质：${esc(data.requestedQuality || '—')} · 采用音质：${esc(data.quality || '—')} · 播放设置：${esc({ redirect: '优先 302', force_redirect: '强制 302', proxy: '代理' }[data.mode] || '—')}</span></div>${data.events.length ? `<ol class="recent-playback-events">${data.events.map(event => `<li><div><b>${esc(stageNames[event.stage] || '播放请求')}</b><small>请求 ${event.request} · ${esc(timeText(event.at))}</small></div><div><span>${esc(event.sourceName || (event.sourceId ? '音源 #' + event.sourceId : ''))}</span><small>${[event.platform && platformText(event.platform), event.quality, event.cached && '缓存命中', event.status && 'HTTP ' + event.status, event.elapsedMs > 0 && event.elapsedMs + ' ms', event.error && event.error !== 'none' && (errorNames[event.error] || '请求失败')].filter(Boolean).map(esc).join(' · ') || '—'}</small></div></li>`).join('')}</ol>` : '<p class="muted hint">当前没有观测到媒体交付明细，开始播放上报本身不能确定音源或交付方式。</p>'}<p class="muted hint">${data.detailCount > data.events.length ? `共 ${data.detailCount} 条明细，保留最近 ${data.events.length} 条。` : ''}取链平台是服务端最终选中的平台，脚本内部的实际上游无法单独验证。</p>`
      details.dataset.loadedAt = String(data.updatedAt)
    } catch (error) {
      if (error.name !== 'AbortError' && revision === state.revision && details.isConnected) {
        body.innerHTML = `<span class="err">${esc(error.message)}</span> <button type="button" class="sec sm">重试</button>`
        body.querySelector('button').onclick = () => loadDetail(details, true)
      }
    } finally { delete details.dataset.loading }
  }
  const api = {
    state,
    get panel() { return panel },
    mount(target) {
      ensurePanel(); target.replaceChildren(panel)
      // 未提交的搜索输入不因汇总静默刷新而自动生效。
      const params = { ...state.params, ...scopeParams() }
      const changed = state.select(Object.fromEntries(Object.entries(params).filter(([, value]) => value !== '')))
      if (changed || !state.loaded) load()
    },
    refresh() { return load({ preserve: state.paged }) },
    reset() { state.reset(); panel?.remove(); panel = null },
  }
  setInterval(() => {
    if (document.hidden || !sessionState.me || location.hash !== '#listening' || !panel?.isConnected || state.loading || state.paged || panel.contains(document.activeElement) || panel.querySelector('details[open]')) return
    const box = panel.getBoundingClientRect()
    if (box.top < 0 && box.bottom > 0) return
    load()
  }, 30000)
  root.LXSCRecent = api
})(globalThis)
