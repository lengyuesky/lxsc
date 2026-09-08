// 听歌看板复用统一会话、路由与主题，图表使用原生 SVG。
const listeningGate = new LXSCMusic.RequestGate()
const listeningState = { data: null, usersLoaded: false, selected: '', loading: false }
const listeningTime = ms => {
  const seconds = Math.floor(Math.max(0, ms) / 1000)
  if (seconds < 60) return `${seconds} 秒`
  const minutes = Math.floor(seconds / 60)
  return minutes < 60 ? `${minutes} 分钟` : `${Math.floor(minutes / 60)} 小时 ${minutes % 60} 分钟`
}
function resetListeningSession() {
  listeningGate.cancel()
  Object.assign(listeningState, { data: null, usersLoaded: false, selected: '', loading: false })
  $('#listeningContent').innerHTML = ''
  $('#listeningContent').setAttribute('aria-busy', 'false')
  $('#listeningNotice').textContent = ''
  $('#listeningScope').innerHTML = '<option value="me">我的统计</option><option value="all">全站统计</option>'
  $('#listeningRange').value = '30'
  $('#listeningScopeWrap').classList.add('hidden')
}
async function loadListening(silent = false) {
  if (!sessionState.me) return
  const request = listeningGate.begin()
  listeningState.loading = true
  const content = $('#listeningContent'), scope = $('#listeningScope').value
  const params = new URLSearchParams({ days: $('#listeningRange').value, scope: scope.startsWith('user:') ? 'user' : scope })
  if (scope.startsWith('user:')) params.set('userId', scope.slice(5))
  $('#listeningScopeWrap').classList.toggle('hidden', !sessionState.me.isAdmin)
  content.setAttribute('aria-busy', 'true')
  $('#listeningNotice').textContent = ''
  if (!silent || !listeningState.data) {
    content.innerHTML = '<div class="listening-skeleton" role="status" aria-label="正在加载听歌统计"><div></div><div></div><div></div><div></div></div>'
  }
  try {
    const stats = await playlistAPI('/listening/stats?' + params, { signal: request.signal })
    if (!request.current()) return
    listeningState.data = stats
    renderListening(stats)
    if (sessionState.me.isAdmin && !listeningState.usersLoaded) {
      try {
        const users = await playlistAPI('/users', { signal: request.signal })
        if (!request.current()) return
        $('#listeningScope').innerHTML = '<option value="me">我的统计</option><option value="all">全站统计</option>' + users.map(u => `<option value="user:${u.id}">${esc(u.name)}</option>`).join('')
        $('#listeningScope').value = scope
        listeningState.usersLoaded = true
      } catch (error) { if (!isAbort(error)) $('#listeningNotice').textContent = '用户列表暂时不可用，刷新后重试。' }
    }
  } catch (error) {
    if (!request.current() || isAbort(error)) return
    if (!silent) content.innerHTML = ''
    $('#listeningNotice').innerHTML = `<div class="listening-error" role="alert"><span>${silent ? '刷新失败，保留上次结果。' : ''}${esc(error.message)}</span><button type="button" class="sec sm" id="listeningRetry">重试</button></div>`
    $('#listeningRetry').onclick = () => loadListening(silent)
  } finally {
    if (request.current()) { listeningState.loading = false; content.setAttribute('aria-busy', 'false') }
  }
}
function listeningRank(items, users = false) {
  if (!items.length) return '<div class="listening-rank-empty"><svg class="icon"><use href="#i-music"/></svg><p>下一首喜欢的歌，值得多听一会儿</p></div>'
  const maxMS = Math.max(1, items[0].totalMs)
  return `<ol class="listening-rank">${items.map((item, i) => `<li><span class="listening-position ${i < 3 ? 'top' : ''}">${String(i + 1).padStart(2, '0')}</span><div class="listening-rank-main">${users ? `<button type="button" class="listening-user" data-listening-user="${esc(item.id)}">${esc(item.name)}</button>` : `<strong title="${esc(item.name)}">${esc(item.name || '未知歌曲')}</strong>`}<small>${users ? `${item.plays} 次收听` : esc(item.singer || '未知歌手')}</small><div class="listening-rank-meter"><i style="width:${item.totalMs / maxMS * 100}%"></i></div></div><span class="listening-rank-time">${listeningTime(item.totalMs)}</span></li>`).join('')}</ol>`
}
function listeningChart(stats) {
  const daily = stats.daily, width = 700, height = 200, left = 48, right = 16, top = 18, bottom = 28
  const maximum = Math.max(60000, ...daily.map(day => day.totalMs))
  const x = i => left + i / Math.max(1, daily.length - 1) * (width - left - right)
  const y = ms => height - bottom - ms / maximum * (height - top - bottom)
  const line = key => daily.map((day, i) => `${i ? 'L' : 'M'}${x(i).toFixed(2)},${y(day[key]).toFixed(2)}`).join(' ')
  const ticks = [0, .5, 1].map(r => `<line x1="${left}" y1="${y(maximum * r)}" x2="${width - right}" y2="${y(maximum * r)}" class="listening-gridline"/><text x="${left - 10}" y="${y(maximum * r) + 4}" text-anchor="end">${Math.round(maximum * r / 60000)}</text>`).join('')
  return `<div class="listening-chart"><span class="muted hint">分钟</span><svg viewBox="0 0 ${width} ${height}" role="img" aria-label="每日听歌时长趋势，可使用下方日期滑块查看准确数值"><defs><linearGradient id="listeningArea" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="var(--pri)" stop-opacity=".18"/><stop offset="100%" stop-color="var(--pri)" stop-opacity="0"/></linearGradient></defs>${ticks}<path d="${line('totalMs')} L${x(daily.length - 1)},${y(0)} L${x(0)},${y(0)} Z" fill="url(#listeningArea)"/><path d="${line('clientMs')}" class="listening-client-line"/><path d="${line('webMs')}" class="listening-web-line"/>${[0, Math.floor((daily.length - 1) / 2), daily.length - 1].map(i => `<text x="${x(i)}" y="${height - 4}" text-anchor="${i === 0 ? 'start' : i === daily.length - 1 ? 'end' : 'middle'}">${daily[i].day.slice(5).replace('-', '/')}</text>`).join('')}<circle id="listeningChartPoint" r="4" class="listening-chart-point"/>${daily.map((day, i) => `<rect x="${x(i) - (width - left - right) / daily.length / 2}" y="${top}" width="${Math.max(2, (width - left - right) / daily.length)}" height="${height - top - bottom}" fill="transparent" data-listening-day="${day.day}"/>`).join('')}</svg><label class="listening-date-control"><span>查看日期</span><input type="range" id="listeningDaySlider" aria-label="查看每日听歌时长" min="0" max="${daily.length - 1}" value="${daily.length - 1}"></label></div>`
}
function listeningCalendar(stats) {
  const maxMS = Math.max(1, ...stats.daily.map(day => day.totalMs))
  const offset = new Date(stats.from + 'T00:00:00Z').getUTCDay()
  const detailed = stats.daily.length <= 30
  return `<div class="listening-calendar-scroll${detailed ? ' detailed' : ''}">${detailed ? '<div class="listening-weekdays">' + ['日', '一', '二', '三', '四', '五', '六'].map(day => `<span>${day}</span>`).join('') + '</div>' : ''}<div class="listening-calendar" role="group" aria-label="每日听歌日历，方向键移动日期">${'<span class="listening-day-spacer"></span>'.repeat(offset)}${stats.daily.map((day, i) => {
    const level = day.totalMs ? Math.max(1, Math.ceil(day.totalMs / maxMS * 4)) : 0
    const label = `${day.day}：${listeningTime(day.totalMs)}，${day.plays} 次收听`
    return `<button type="button" class="listening-day level-${level}" data-listening-day="${day.day}" tabindex="${i === stats.daily.length - 1 ? 0 : -1}" aria-label="${label}" title="${label}">${detailed ? (day.day.endsWith('-01') || i === 0 ? Number(day.day.slice(5, 7)) + '/' : '') + Number(day.day.slice(8)) : ''}</button>`
  }).join('')}</div></div>`
}
function renderListening(stats) {
  const focused = document.activeElement?.dataset.listeningDay
  const sliderFocused = document.activeElement?.id === 'listeningDaySlider'
  const scope = $('#listeningScope').value
  const hours = Math.floor(stats.totalMs / 3600000), minutes = Math.floor(stats.totalMs / 60000) % 60
  const share = stats.totalMs ? stats.webMs / stats.totalMs * 100 : 0
  const enabled = new Date(stats.enabledAt).toLocaleDateString('zh-CN', { timeZone: 'Asia/Shanghai' })
  const name = scope === 'all' ? '全站的音乐时光' : scope.startsWith('user:') ? `${$('#listeningScope').selectedOptions[0]?.textContent || '用户'}的音乐时光` : '你的音乐时光'
  $('#listeningContent').innerHTML = `
    <div class="listening-hero card"><div class="listening-hero-copy"><span class="listening-eyebrow"><i></i>${esc(name)}</span><h3>把时间，交给喜欢的旋律。</h3><div class="listening-total" aria-label="总收听时长 ${listeningTime(stats.totalMs)}"><b>${hours.toLocaleString('zh-CN')}</b><span>小时</span><b>${minutes}</b><span>分钟</span></div><p>${stats.plays ? `${stats.from.replaceAll('-', '/')} — ${stats.to.replaceAll('-', '/')} · ${stats.plays.toLocaleString('zh-CN')} 次收听` : '统计已就绪，从今天的第一首歌开始。'}</p><div class="listening-source"><span><i class="web"></i>网页实听 <b>${listeningTime(stats.webMs)}</b></span><span><i class="client"></i>客户端估算 <b>${listeningTime(stats.clientMs)}</b></span></div><div class="listening-source-meter"><i style="width:${share}%"></i></div></div><div class="listening-record" aria-hidden="true"><div class="listening-record-label"><svg><use href="#i-music"/></svg><span>此刻 · 有声</span></div><i></i></div></div>
    <div class="listening-metrics"><div class="card"><span class="listening-metric-icon"><svg class="icon"><use href="#i-dash"/></svg></span><div><span>日均听歌</span><strong>${listeningTime(stats.averageMs)}</strong><small>按启用后的 ${stats.countedDays} 个自然日计算</small></div></div><div class="card"><span class="listening-metric-icon mint"><svg class="icon"><use href="#i-music"/></svg></span><div><span>听过的歌曲</span><strong>${stats.tracks.toLocaleString('zh-CN')} <em>首</em></strong><small>每首歌，都是一段心情</small></div></div><div class="card"><span class="listening-metric-icon amber"><svg class="icon"><use href="#i-zap"/></svg></span><div><span>活跃天数</span><strong>${stats.activeDays} <em>天</em></strong><small>有音乐陪伴的日子</small></div></div></div>
    ${!stats.plays ? '<div class="listening-empty card"><div><strong>音乐时光，等你点亮</strong><p>在网页播放一首歌，或使用支持播放上报的客户端开始收听。</p></div><a class="btn" href="#search">去听一首歌 →</a></div>' : ''}
    <div class="listening-main-grid"><section class="card listening-trend"><div class="section-title"><div><h3>收听趋势</h3><p>每一天，都有自己的节奏</p></div><div class="listening-legend"><span><i class="web"></i>实听</span><span><i class="client"></i>估算</span></div></div>${listeningChart(stats)}<div id="listeningDayDetail" class="listening-day-detail" role="status"></div></section><section class="card listening-top"><div class="section-title"><div><h3>最常听的旋律</h3><p>按收听时长排序</p></div><span class="listening-top-label">TOP 10</span></div>${listeningRank(stats.topTracks)}</section>
    <section class="card listening-calendar-card"><div class="section-title"><div><h3>听歌日历</h3><p>点亮的日子，是音乐留下的足迹</p></div><span class="muted hint">${stats.activeDays} 天有音乐相伴</span></div>${listeningCalendar(stats)}<div class="listening-calendar-footer"><span>${stats.from} — ${stats.to}</span><span class="listening-intensity">少 ${[0, 1, 2, 3, 4].map(i => `<i class="level-${i}"></i>`).join('')} 多</span></div></section></div>
    ${scope === 'all' ? `<section class="card listening-users"><div class="section-title"><div><h3>一起听歌的人</h3><p>点击用户，查看他的音乐时光</p></div><span class="listening-top-label">用户 TOP 10</span></div>${listeningRank(stats.topUsers, true)}</section>` : ''}
    <footer class="listening-footnote"><span>统计始于 ${enabled} · 北京时间 · 每 30 秒自动更新</span><p>网页按实际播放计时；客户端根据正式播放上报及歌曲时长估算。总时长包含两者，多设备分别累计，旧播放历史不回填。${stats.unknownDuration ? `当前有 ${stats.unknownDuration} 条客户端记录缺少歌曲时长，仅计收听次数。` : ''}</p></footer>`
  const daily = stats.daily
  if (!daily.some(day => day.day === listeningState.selected)) listeningState.selected = stats.to
  selectListeningDay(listeningState.selected)
  $('#listeningDaySlider').oninput = event => selectListeningDay(daily[Number(event.target.value)].day)
  document.querySelectorAll('[data-listening-user]').forEach(button => button.onclick = () => { $('#listeningScope').value = 'user:' + button.dataset.listeningUser; loadListening() })
  document.querySelectorAll('[data-listening-day]').forEach(element => {
    const select = () => selectListeningDay(element.dataset.listeningDay)
    element.onclick = select
    element.onpointerenter = event => { if (event.pointerType === 'mouse') select() }
    element.onfocus = select
    if (element.tagName === 'BUTTON') element.onkeydown = event => {
      const detailed = daily.length <= 30
      const delta = { ArrowLeft: detailed ? -1 : -7, ArrowRight: detailed ? 1 : 7, ArrowUp: detailed ? -7 : -1, ArrowDown: detailed ? 7 : 1, Home: -365, End: 365 }[event.key]
      if (delta === undefined) return
      event.preventDefault()
      const i = daily.findIndex(day => day.day === element.dataset.listeningDay)
      document.querySelector(`button[data-listening-day="${daily[Math.max(0, Math.min(daily.length - 1, i + delta))].day}"]`).focus()
    }
  })
  if (sliderFocused) $('#listeningDaySlider').focus()
  else if (focused) document.querySelector(`button[data-listening-day="${focused}"]`)?.focus()
}
function selectListeningDay(day) {
  const stats = listeningState.data
  if (!stats) return
  const index = stats.daily.findIndex(item => item.day === day), item = stats.daily[index]
  if (!item) return
  listeningState.selected = day
  $('#listeningDaySlider').value = String(index)
  $('#listeningDaySlider').setAttribute('aria-valuetext', `${day}，${listeningTime(item.totalMs)}`)
  $('#listeningDayDetail').innerHTML = `<strong>${day.slice(5).replace('-', ' 月 ')} 日</strong><span>实听 <b>${listeningTime(item.webMs)}</b></span><span>估算 <b>${listeningTime(item.clientMs)}</b></span><span>${item.plays} 次收听</span>`
  const max = Math.max(60000, ...stats.daily.map(d => d.totalMs))
  $('#listeningChartPoint').setAttribute('cx', 48 + index / Math.max(1, stats.daily.length - 1) * 636)
  $('#listeningChartPoint').setAttribute('cy', 172 - item.webMs / max * 154)
  document.querySelectorAll('button[data-listening-day]').forEach(button => {
    const selected = button.dataset.listeningDay === day
    button.classList.toggle('selected', selected); button.tabIndex = selected ? 0 : -1
    button.setAttribute('aria-pressed', String(selected))
  })
}
$('#listeningScope').onchange = () => { listeningState.selected = ''; loadListening() }
$('#listeningRange').onchange = () => { listeningState.selected = ''; loadListening() }
$('#listeningRefresh').onclick = () => loadListening(true)
setInterval(() => { if (!document.hidden && sessionState.me && location.hash === '#listening' && !listeningState.loading) loadListening(true) }, 30000)
document.addEventListener('visibilitychange', () => { if (!document.hidden && sessionState.me && location.hash === '#listening') loadListening(true) })
