// 概览、音源与日志的事件和实现由本模块管理。
const sourceErrorNames = { timeout: '调用超时', busy: '服务繁忙', cancelled: '已取消', invalid_response: '返回数据无效', script: '脚本调用失败' }
const sourceActionNames = { musicUrl: '取链', lyric: '歌词', pic: '封面' }

export function summarizeSourceCalls(items) {
  const out = { calls: 0, success: 0, failed: 0, cancelled: 0, downgrades: 0, attempts: 0, durationMs: 0, maxMs: 0, errors: {} }
  for (const item of items) {
    if (!item) continue
    for (const key of ['calls', 'success', 'failed', 'cancelled', 'downgrades', 'attempts', 'durationMs']) out[key] += item[key] || 0
    out.maxMs = Math.max(out.maxMs, item.maxMs || 0)
    for (const key of Object.keys(sourceErrorNames)) out.errors[key] = (out.errors[key] || 0) + (item.errors?.[key] || 0)
  }
  out.averageMs = out.calls ? Math.floor(out.durationMs / out.calls) : 0
  return out
}

export function sourceCallHealth(summary, state = 'ready') {
  if (state === 'disabled') return { tone: 'off', label: '已停用' }
  if (state === 'loading') return { tone: 'pri', label: '加载中' }
  if (state !== 'ready') return { tone: 'err', label: '未就绪' }
  const completed = (summary?.success || 0) + (summary?.failed || 0)
  if (!completed) return { tone: 'off', label: '暂无有效样本' }
  const rate = summary.success / completed
  return rate >= .95 ? { tone: 'ok', label: '正常' } : rate >= .8 ? { tone: 'warn', label: '波动' } : { tone: 'err', label: '异常' }
}

export function createAdmin({ $, adminAPI, esc, platName, fmtDur, formatBytes, formatTime, toast, isAbort }) {
const countText = value => Number(value || 0).toLocaleString('zh-CN')
const rateText = summary => (summary?.success || 0) + (summary?.failed || 0) ? (summary.success / (summary.success + summary.failed) * 100).toFixed(1).replace(/\.0$/, '') + '%' : '—'
const healthBadge = (summary, state) => { const health = sourceCallHealth(summary, state); return `<span class="badge ${health.tone}">${health.label}</span>` }
const timeText = value => new Date(value).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
const callTimeFormatter = new Intl.DateTimeFormat('zh-CN', { dateStyle: 'short', timeStyle: 'medium' })
const callTimeText = value => callTimeFormatter.format(new Date(value))
let sourceStatsData = null, sourceStatsController = null, sourceStatsVersion = 0, sourceStatsTimer = null
let sourceListController = null, sourceListVersion = 0, periodLabels = []
const sourceStatsVisible = () => !document.hidden && !$('#app').classList.contains('hidden') && $('#tab-sources').classList.contains('active')

function cancelSourceStats() {
  sourceStatsVersion++
  sourceStatsController?.abort()
  sourceStatsController = null
  clearTimeout(sourceStatsTimer)
  $('#refreshSourceStats').disabled = false
}

function scheduleSourceStats() {
  clearTimeout(sourceStatsTimer)
  if (sourceStatsVisible()) sourceStatsTimer = setTimeout(() => loadSourceStatistics(), 30000)
}

async function loadSourceStatistics() {
  clearTimeout(sourceStatsTimer)
  sourceStatsController?.abort()
  const controller = new AbortController(), version = ++sourceStatsVersion
  sourceStatsController = controller
  const window = $('#sourceStatsWindow').value
  $('#refreshSourceStats').disabled = true
  if (!sourceStatsData || sourceStatsData.window !== window) {
    sourceStatsData = null
    $('#sourceStatsBody').innerHTML = '<p class="empty">正在读取调用统计…</p>'
    $('#sourceStatsStatus').textContent = ''
  }
  try {
    const data = await adminAPI('/sources/statistics?window=' + window, { signal: controller.signal })
    if (version !== sourceStatsVersion) return
    sourceStatsData = data
    const selected = $('#sourceStatsSource').value
    $('#sourceStatsSource').innerHTML = '<option value="">全部音源</option>' + data.sources.map(source => `<option value="${source.id}">${esc(source.name)}</option>`).join('')
    $('#sourceStatsSource').value = data.sources.some(source => String(source.id) === selected) ? selected : ''
    renderSourceStatistics()
  } catch (error) {
    if (version !== sourceStatsVersion || isAbort(error)) return
    $('#sourceStatsStatus').textContent = '统计读取失败：' + error.message + (sourceStatsData ? '；仍显示上次数据，可点击刷新重试。' : '；可点击刷新重试。')
    if (!sourceStatsData) $('#sourceStatsBody').innerHTML = '<p class="empty">暂时无法读取调用统计</p>'
  } finally {
    if (version === sourceStatsVersion) {
      sourceStatsController = null
      $('#refreshSourceStats').disabled = false
      scheduleSourceStats()
    }
  }
}

function renderSourceStatistics() {
  if (!sourceStatsData) return
  const selected = $('#sourceStatsSource').value
  const sources = sourceStatsData.sources.filter(source => !selected || String(source.id) === selected)
  const summary = summarizeSourceCalls(sources.map(source => source.statistics.summary))
  const total = summarizeSourceCalls(sources.map(source => source.statistics.total))
  const inFlight = sources.reduce((count, source) => count + source.statistics.inFlight, 0)
  $('#sourceStatsStatus').textContent = `更新于 ${callTimeText(sourceStatsData.generatedAt)} · 运行期间累计 ${countText(total.calls)} 次调用 · 当前进行中 ${inFlight} 次`
  if (!sources.length) {
    $('#sourceStatsBody').innerHTML = '<p class="empty">暂无音源，请在下方添加音源脚本。调用后会自动显示统计。</p>'
    return
  }
  const body = $('#sourceStatsBody')
  const opened = [...body.querySelectorAll('details[open]')].map(item => item.id)
  const scrollTop = body.querySelector('.source-call-recent')?.scrollTop || 0
  const focusedPeriod = document.activeElement?.dataset?.sourcePeriod
  const focusedSource = document.activeElement?.dataset?.sourceStat
  const focusedSummary = document.activeElement?.matches('summary') ? document.activeElement.parentElement.id : ''
  const points = sources[0].statistics.trend.map((point, index) => ({ at: point.at, ...summarizeSourceCalls(sources.map(source => source.statistics.trend[index])) }))
  const maxCalls = Math.max(1, ...points.map(point => point.calls))
  const bucketMinutes = sources[0].statistics.bucketMinutes
  periodLabels = points.map(point => `${timeText(point.at)}–${timeText(Math.min(new Date(point.at).getTime() + bucketMinutes * 60000, new Date(sourceStatsData.generatedAt).getTime()))}：调用 ${countText(point.calls)} 次，成功 ${countText(point.success)}，失败 ${countText(point.failed)}，取消 ${countText(point.cancelled)}，平均 ${countText(point.averageMs)} ms`)
  const groups = new Map()
  for (const source of sources) for (const group of source.statistics.groups) {
    const key = group.platform + ':' + group.action
    if (!groups.has(key)) groups.set(key, { platform: group.platform, action: group.action, items: [] })
    groups.get(key).items.push(group)
  }
  const recent = sources.flatMap(source => source.statistics.recent.map(call => ({ ...call, sourceId: source.id, sourceName: source.name }))).sort((a, b) => new Date(b.at) - new Date(a.at)).slice(0, sourceStatsData.recentLimit || 100)
  const metrics = [
    ['调用次数', countText(summary.calls), `${countText(summary.attempts)} 次尝试 · 降级 ${countText(summary.downgrades)} 次`, ''],
    ['成功率', rateText(summary), `成功 ${countText(summary.success)} 次`, sourceCallHealth(summary).tone],
    ['平均耗时', summary.calls ? countText(summary.averageMs) + ' ms' : '—', `最长 ${countText(summary.maxMs)} ms`, ''],
    ['失败次数', countText(summary.failed), `另有 ${countText(summary.cancelled)} 次取消`, summary.failed ? 'err' : '']
  ]
  body.innerHTML = `<div class="source-call-metrics">${metrics.map(([label, value, note, tone]) => `<div class="source-call-metric"><span>${label}</span><strong class="${tone}">${value}</strong><small>${note}</small></div>`).join('')}</div>
    <div class="source-stats-chart-head"><b>调用趋势 <span class="muted hint">每 ${bucketMinutes} 分钟</span></b><div class="source-call-legend"><span><i></i>成功</span><span><i class="failed"></i>失败</span><span><i class="cancelled"></i>取消</span></div></div>
    <div class="source-call-trend" aria-label="调用趋势">${points.map((point, index) => `<button type="button" class="source-call-bar ${point.calls ? '' : 'empty-bar'}" data-source-period="${index}" aria-label="${esc(periodLabels[index])}" title="${esc(periodLabels[index])}"><i style="height:${point.success / maxCalls * 100}%"></i><i class="failed" style="height:${point.failed / maxCalls * 100}%"></i><i class="cancelled" style="height:${point.cancelled / maxCalls * 100}%"></i></button>`).join('')}</div>
    <div class="source-call-axis"><span>${timeText(sources[0].statistics.from)}</span><span>${timeText(sourceStatsData.generatedAt)}</span></div><output id="sourceStatsPeriod" class="source-call-period">${summary.calls ? '悬停、点击或用键盘选择时间段，查看调用情况' : '该时间范围内暂无调用，播放或测试音源后会自动显示'}</output>
    <div class="table-wrap"><table id="sourceStatsTable"><thead><tr><th>音源</th><th>调用健康</th><th>调用次数</th><th>成功率</th><th>失败 / 取消</th><th>平均耗时</th><th>降级</th></tr></thead><tbody>${sources.map(source => { const s = source.statistics.summary; return `<tr><td><button class="ghost sm source-call-name" type="button" data-source-stat="${source.id}">${esc(source.name)}</button></td><td>${healthBadge(s, source.state)}</td><td>${countText(s.calls)}</td><td>${rateText(s)}</td><td>${countText(s.failed)} / ${countText(s.cancelled)}</td><td>${s.calls ? countText(s.averageMs) + ' ms' : '—'}</td><td>${countText(s.downgrades)}</td></tr>` }).join('')}</tbody></table></div>
    <div class="source-call-errors">${Object.entries(summary.errors).filter(([, count]) => count > 0).map(([key, count]) => `<span class="chip">${sourceErrorNames[key]} <b>${countText(count)}</b></span>`).join('') || '<span class="muted hint">该时间范围内暂无失败或取消记录</span>'}</div>
    <details id="sourceStatsGroups"><summary>按平台与调用类型查看（${groups.size} 项）</summary><div class="table-wrap"><table><thead><tr><th>平台</th><th>调用类型</th><th>次数</th><th>成功率</th><th>失败 / 取消</th><th>平均耗时</th></tr></thead><tbody>${[...groups.values()].map(group => { const s = summarizeSourceCalls(group.items); return `<tr><td>${esc(platName[group.platform] || group.platform)}</td><td>${esc(sourceActionNames[group.action] || group.action)}</td><td>${countText(s.calls)}</td><td>${rateText(s)}</td><td>${countText(s.failed)} / ${countText(s.cancelled)}</td><td>${countText(s.averageMs)} ms</td></tr>` }).join('') || '<tr><td colspan="6" class="empty">暂无平台调用</td></tr>'}</tbody></table></div></details>
    <details id="sourceStatsRecent"><summary>最近调用明细（${recent.length} 条，最多显示 100 条）</summary><div class="table-wrap source-call-recent"><table><thead><tr><th>时间</th><th>音源</th><th>平台</th><th>调用</th><th>歌曲</th><th>音质</th><th>尝试</th><th>结果</th><th>耗时</th></tr></thead><tbody>${recent.map(call => `<tr><td>${esc(callTimeText(call.at))}</td><td>${esc(call.sourceName)}</td><td>${esc(platName[call.platform] || call.platform)}</td><td>${esc(sourceActionNames[call.action] || call.action)}</td><td class="call-song">${esc(call.song || '—')}${call.singer ? `<small>${esc(call.singer)}</small>` : ''}</td><td>${esc(call.requestedQuality || '—')}${call.downgraded ? ` → ${esc(call.quality)}` : ''}</td><td>${countText(call.attempts)}</td><td><span class="badge ${call.outcome === 'success' ? 'ok' : call.outcome === 'cancelled' ? 'off' : 'err'}">${call.outcome === 'success' ? (call.downgraded ? '成功 · 已降级' : '成功') : esc(sourceErrorNames[call.errorCategory] || '调用失败')}</span></td><td>${countText(call.durationMs)} ms</td></tr>`).join('') || '<tr><td colspan="9" class="empty">该时间范围内暂无调用明细</td></tr>'}</tbody></table></div></details>`
  for (const id of opened) body.querySelector('#' + id)?.setAttribute('open', '')
  if (body.querySelector('.source-call-recent')) body.querySelector('.source-call-recent').scrollTop = scrollTop
  if (focusedPeriod !== undefined) body.querySelector(`[data-source-period="${focusedPeriod}"]`)?.focus({ preventScroll: true })
  else if (focusedSource !== undefined) body.querySelector(`[data-source-stat="${focusedSource}"]`)?.focus({ preventScroll: true })
  else if (focusedSummary) body.querySelector(`#${focusedSummary} > summary`)?.focus({ preventScroll: true })
}

function selectSourceStatistics(id) {
  $('#sourceStatsSource').value = String(id)
  renderSourceStatistics()
  $('#sourceStatsPanel').scrollIntoView({ block: 'start' })
}

// ---- 概览 ----
function sourceCard(s, brief) {
  const st = s.status
  const badge = !s.enabled ? '<span class="badge off">已停用</span>' : !st ? '<span class="badge off">未加载</span>' : st.state === 'ready' ? '<span class="badge ok">就绪</span>' : st.state === 'loading' ? '<span class="badge pri">加载中</span>' : '<span class="badge err">错误</span>'
  const chips = st && st.platforms ? Object.entries(st.platforms).map(([p, c]) => `<span class="chip"><b>${esc(platName[p] || p)}</b><span>${esc((c.qualitys || []).join('/'))}</span></span>`).join('') : ''
  const h = st?.health
  const health = h?.samples ? `<div class="note">近一小时最近 ${h.samples} 次解析：成功率 ${Math.round(h.success / h.samples * 100)}% · 平均 ${h.averageMs} ms · 降级 ${h.downgrades} 次<br><small>失败分类：${esc(Object.entries(h.errors || {}).map(([key, count]) => `${({ timeout: '超时', busy: '繁忙', cancelled: '取消', script: '脚本失败' })[key] || key} ${count}`).join(' · ') || '无')}。解析成功不代表客户端实际播放成功；重载后重置。</small></div>` : '<p class="muted hint">暂无近期解析样本</p>'
  const err = st && st.error ? `<div class="note err">${esc(st.error)}</div>` : ''
  const alert = st && st.alert ? `<div class="note warn">脚本提示更新：${esc(st.alert)}</div>` : ''
  const meta = [s.description, s.author, s.homepage ? `<a href="${esc(s.homepage)}" target="_blank" rel="noopener">主页</a>` : ''].map((x, i) => i === 2 ? x : esc(x || '')).filter(Boolean).join(' · ')
  const actions = brief ? '' : `<div class="actions">
      <button class="sec sm" data-source-action="toggle" data-source-id="${s.id}" data-enabled="${!s.enabled}">${s.enabled ? '停用' : '启用'}</button>
      <button class="sec sm" data-source-action="reload" data-source-id="${s.id}"><svg class="icon"><use href="#i-refresh"/></svg>重新加载</button>
      <button class="sec sm" data-source-action="test" data-source-id="${s.id}"><svg class="icon"><use href="#i-zap"/></svg>测试取链</button>
      <button class="sec sm" data-source-action="statistics" data-source-id="${s.id}">调用统计</button>
      <a class="btn sec sm" href="/api/admin/sources/${s.id}/script" download>下载脚本</a>
      <label class="prio">优先级<input type="number" value="${s.priority}" data-source-priority="${s.id}"></label>
      <button class="danger sm" data-source-action="delete" data-source-id="${s.id}">删除</button>
    </div><div id="test-${s.id}"></div>
    ${st && st.logs && st.logs.length ? `<details><summary>脚本日志（${st.logs.length}）</summary><pre>${esc(st.logs.slice(-50).join('\n'))}</pre></details>` : ''}`
  return `<div class="card src">
    <div class="src-head"><div>
      <div class="src-title">${esc(s.name)} ${badge} <span class="ver">v${esc(s.version || st?.version || '')}</span></div>
      ${meta ? `<div class="meta">${meta}</div>` : ''}
    </div></div>
    <div class="chips">${chips || '<span class="muted hint">无平台信息</span>'}</div>${err}${alert}${health}${actions}</div>`
}

async function loadDashboard() {
  $('#serverUrl').textContent = location.origin
  const [st, srcs, calls] = await Promise.all([adminAPI('/status'), adminAPI('/sources'), adminAPI('/sources/statistics?window=1h')])
  const callSources = new Map(calls.sources.map(source => [source.id, source]))
  const unstable = calls.sources.filter(source => source.state === 'ready' && ['warn', 'err'].includes(sourceCallHealth(source.statistics.summary).tone)).length
  const health = st.sourceHealth || {}, ready = health.ready || 0, total = health.total || 0, errors = health.error || 0
  const healthType = ready > 0 && errors === 0 && unstable === 0 ? 'ok' : ready > 0 ? 'warn' : 'err'
  const healthText = healthType === 'ok' ? '服务运行正常' : healthType === 'warn' ? '部分音源异常' : '暂无可用音源'
  $('#healthHero').innerHTML = `<div><span class="health-dot ${healthType}"></span><div><div class="health-title">${healthText}</div><div class="muted">lxsc ${esc(st.version)} · 已运行 ${esc(fmtDur(st.uptime))}</div></div></div><div class="health-platforms">${(st.platforms || []).map(p => `<span class="chip"><b>${esc(platName[p] || p)}</b></span>`).join('') || '<span class="muted">没有可用平台</span>'}<small>更新于 ${new Date().toLocaleTimeString('zh-CN')}</small></div>`
  const data = st.data || {}
  const cards = [
    ['用户', data.users ?? st.users ?? 0, '已创建的登录账号', 'users'],
    ['歌单', data.playlists ?? 0, `${data.tracks ?? 0} 首歌曲元数据`, 'music'],
    ['音源健康', `${ready}/${total}`, errors || unstable ? `${errors} 个未就绪 · ${unstable} 个调用波动或异常` : '近期未发现调用异常', errors || unstable ? 'warn' : 'ok'],
    ['内存', `${Number(st.memMB || 0).toFixed(1)} MB`, `系统 ${Number(st.sysMB || 0).toFixed(1)} MB · ${st.goroutines || 0} 协程`, 'memory']
  ]
  $('#statusCards').innerHTML = cards.map(([k, v, note, tone]) => `<div class="card overview-stat tone-${tone}"><div class="k">${k}</div><div class="v">${esc(v)}</div><div class="note">${esc(note)}</div></div>`).join('')
  const backup = st.backup || {}, last = backup.last || {}
  $('#backupSummary').innerHTML = `<div class="section-title"><h4 class="card-title"><svg class="icon"><use href="#i-backup"/></svg>数据与备份</h4><a href="#backups">管理 →</a></div><div class="data-lines"><div><span>数据库</span><b>${formatBytes(data.sizeBytes || 0)}</b></div><div><span>收藏 / 播放历史</span><b>${data.stars || 0} / ${data.history || 0}</b></div><div><span>最近备份</span><b class="${last.error ? 'err' : last.warning ? 'warn' : ''}">${last.finishedAt ? `${formatTime(last.finishedAt)} · ${last.error ? '失败' : last.warning ? '成功（清理有警告）' : '成功'}` : '尚无记录'}</b></div>${backup.pending ? '<div class="pending-mini"><span>待恢复</span><b>重启后生效</b></div>' : ''}</div>`
  $('#dashSources').innerHTML = srcs.length ? srcs.map(s => {
    const state = !s.enabled ? ['off','已停用'] : s.status?.state === 'ready' ? ['ok','就绪'] : ['err','异常']
    const platforms = Object.keys(s.status?.platforms || {}).map(p => platName[p] || p).join('、') || '无平台'
    const callSource = callSources.get(s.id), summary = callSource?.statistics.summary
    return `<div class="card dash-source"><div><span class="badge ${state[0]}">${state[1]}</span><b>${esc(s.name)}</b><span class="muted">v${esc(s.version || s.status?.version || '-')}</span></div><div class="muted">${esc(platforms)}</div><div class="source-call-mini">${healthBadge(summary, callSource?.state || 'unloaded')}<span>近 1 小时 ${countText(summary?.calls)} 次 · 成功率 ${rateText(summary)}</span></div>${s.status?.error ? `<div class="err source-error">${esc(s.status.error)}</div>` : ''}</div>`
  }).join('') : '<div class="card empty">还没有音源脚本，请到 <a href="#sources">音源</a> 页面上传或导入。</div>'
}
// ---- 音源 ----
async function loadSources() {
  await Promise.all([loadSourceList(), loadSourceStatistics()])
}
async function loadSourceList() {
  sourceListController?.abort()
  const controller = new AbortController(), version = ++sourceListVersion
  sourceListController = controller
  try {
    const srcs = await adminAPI('/sources', { signal: controller.signal })
    if (version !== sourceListVersion) return
    $('#sourceList').innerHTML = srcs.length ? srcs.map(s => sourceCard(s, false)).join('') : '<div class="card empty">暂无音源脚本，请在上方上传或从 URL 导入</div>'
  } finally { if (version === sourceListVersion) sourceListController = null }
}
async function uploadSource(e) {
  e.preventDefault()
  const fd = new FormData(e.target)
  try { const r = await adminAPI('/sources', { method: 'POST', body: fd }); e.target.reset(); await loadSources(); if (r.status?.error) toast('已保存，但加载失败：' + r.status.error, true); else toast('音源已添加') } catch (err) { toast(err.message, true) }
  return false
}
async function importSource(e) {
  e.preventDefault()
  const fd = new FormData(e.target)
  try { const r = await adminAPI('/sources/import', { method: 'POST', body: { url: fd.get('url') } }); e.target.reset(); await loadSources(); if (r.status?.error) toast('已保存，但加载失败：' + r.status.error, true); else toast('音源已添加') } catch (err) { toast(err.message, true) }
  return false
}
async function toggleSource(id, enabled) { await adminAPI('/sources/' + id, { method: 'PUT', body: { enabled } }); await loadSources() }
async function setPriority(id, priority) { await adminAPI('/sources/' + id, { method: 'PUT', body: { priority: Number(priority) } }); await loadSources() }
async function reloadSource(id) { const r = await adminAPI('/sources/' + id + '/reload', { method: 'POST' }); if (r.error) toast(r.error, true); else toast('已重新加载'); await loadSources() }
async function deleteSource(id) { if (!confirm('确定删除该音源？')) return; await adminAPI('/sources/' + id, { method: 'DELETE' }); await loadSources() }
async function testSource(id) {
  const box = $('#test-' + id)
  box.innerHTML = '<div class="muted hint" style="margin-top:10px">测试中（搜索“晴天 周杰伦”并取 128k 直链）…</div>'
  try {
    const r = await adminAPI('/sources/' + id + '/test', { method: 'POST', body: {} })
    box.innerHTML = '<div class="table-wrap sm"><table><thead><tr><th>平台</th><th>歌曲</th><th>结果</th><th>耗时</th></tr></thead><tbody>' + Object.entries(r).map(([p, v]) => `<tr><td>${platName[p] || p}</td><td>${esc(v.song || '')}</td><td>${v.ok ? `<span class="badge ok">成功 ${esc(v.quality)}</span> <a href="${esc(v.url)}" target="_blank" rel="noopener">打开链接</a>` : `<span class="badge err">失败</span> <span class="err">${esc(v.error)}</span>`}</td><td class="muted">${v.ms ?? ''} ms</td></tr>`).join('') + '</tbody></table></div>'
  } catch (err) { if (!isAbort(err)) box.innerHTML = `<div class="note err">${esc(err.message)}</div>` }
  if (sourceStatsVisible()) await loadSourceStatistics()
}

// ---- 日志 ----
async function loadLogs() {
  const logs = await adminAPI('/logs?n=300')
  const box = $('#logBox')
  box.innerHTML = logs.length ? logs.map(l => `<div class="log-line lv-${esc(String(l.level).toLowerCase())}"><span class="t">${esc(l.time)}</span><span class="lv">${esc(l.level)}</span><span class="m">${esc(l.message)}</span></div>`).join('') : '<div class="empty">暂无日志</div>'
  box.scrollTop = box.scrollHeight
}


$('#sourceList').addEventListener('click', event => {
  const button = event.target.closest('[data-source-action]')
  if (!button) return
  const id = Number(button.dataset.sourceId)
  const actions = { toggle: () => toggleSource(id, button.dataset.enabled === 'true'), reload: () => reloadSource(id), test: () => testSource(id), statistics: () => selectSourceStatistics(id), delete: () => deleteSource(id) }
  Promise.resolve(actions[button.dataset.sourceAction]?.()).catch(error => { if (!isAbort(error)) toast(error.message, true) })
})
$('#sourceList').addEventListener('change', event => {
  if (event.target.matches('[data-source-priority]')) setPriority(Number(event.target.dataset.sourcePriority), event.target.value).catch(error => toast(error.message, true))
})
$('#uploadSourceForm').addEventListener('submit', uploadSource)
$('#importSourceForm').addEventListener('submit', importSource)
$('#refreshDashboard').addEventListener('click', () => loadDashboard().catch(error => toast(error.message, true)))
$('#refreshLogs').addEventListener('click', () => loadLogs().catch(error => toast(error.message, true)))
$('#refreshSourceStats').addEventListener('click', () => loadSourceStatistics())
$('#sourceStatsWindow').addEventListener('change', () => loadSourceStatistics())
$('#sourceStatsSource').addEventListener('change', () => renderSourceStatistics())
$('#sourceStatsBody').addEventListener('click', event => {
  const source = event.target.closest('[data-source-stat]')
  if (source) selectSourceStatistics(source.dataset.sourceStat)
})
for (const type of ['mouseover', 'focusin', 'click']) $('#sourceStatsBody').addEventListener(type, event => {
  const period = event.target.closest('[data-source-period]')
  if (period) $('#sourceStatsPeriod').textContent = periodLabels[Number(period.dataset.sourcePeriod)] || ''
})
window.addEventListener('hashchange', () => { if (!sourceStatsVisible()) cancelSourceStats() })
document.addEventListener('visibilitychange', () => {
  if (!sourceStatsVisible()) cancelSourceStats()
  else void loadSourceStatistics()
})

function reset() {
  cancelSourceStats()
  sourceListVersion++
  sourceListController?.abort()
  sourceListController = null
  sourceStatsData = null
  periodLabels = []
  $('#sourceStatsSource').innerHTML = '<option value="">全部音源</option>'
  $('#sourceStatsWindow').value = '1h'
  $('#sourceStatsBody').innerHTML = ''
  $('#sourceStatsStatus').textContent = ''
  $('#sourceList').innerHTML = ''
}

return { loadDashboard, loadSources, loadLogs, reset }
}

LXSCAdminModules.register('admin', createAdmin)
