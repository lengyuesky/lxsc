// 概览、音源与日志的事件和实现由本模块管理。
const LXSCAdmin = (() => {
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
  const [st, srcs] = await Promise.all([adminAPI('/status'), adminAPI('/sources')])
  const health = st.sourceHealth || {}, ready = health.ready || 0, total = health.total || 0, errors = health.error || 0
  const healthType = ready > 0 && errors === 0 ? 'ok' : ready > 0 ? 'warn' : 'err'
  const healthText = healthType === 'ok' ? '服务运行正常' : healthType === 'warn' ? '部分音源异常' : '暂无可用音源'
  $('#healthHero').innerHTML = `<div><span class="health-dot ${healthType}"></span><div><div class="health-title">${healthText}</div><div class="muted">lxsc ${esc(st.version)} · 已运行 ${esc(fmtDur(st.uptime))}</div></div></div><div class="health-platforms">${(st.platforms || []).map(p => `<span class="chip"><b>${esc(platName[p] || p)}</b></span>`).join('') || '<span class="muted">没有可用平台</span>'}<small>更新于 ${new Date().toLocaleTimeString('zh-CN')}</small></div>`
  const data = st.data || {}
  const cards = [
    ['用户', data.users ?? st.users ?? 0, '已创建的登录账号', 'users'],
    ['歌单', data.playlists ?? 0, `${data.tracks ?? 0} 首歌曲元数据`, 'music'],
    ['音源健康', `${ready}/${total}`, errors ? `${errors} 个异常` : '全部运行正常', errors ? 'warn' : 'ok'],
    ['内存', `${Number(st.memMB || 0).toFixed(1)} MB`, `系统 ${Number(st.sysMB || 0).toFixed(1)} MB · ${st.goroutines || 0} 协程`, 'memory']
  ]
  $('#statusCards').innerHTML = cards.map(([k, v, note, tone]) => `<div class="card overview-stat tone-${tone}"><div class="k">${k}</div><div class="v">${esc(v)}</div><div class="note">${esc(note)}</div></div>`).join('')
  const backup = st.backup || {}, last = backup.last || {}
  $('#backupSummary').innerHTML = `<div class="section-title"><h4 class="card-title"><svg class="icon"><use href="#i-backup"/></svg>数据与备份</h4><a href="#backups">管理 →</a></div><div class="data-lines"><div><span>数据库</span><b>${formatBytes(data.sizeBytes || 0)}</b></div><div><span>收藏 / 播放历史</span><b>${data.stars || 0} / ${data.history || 0}</b></div><div><span>最近备份</span><b class="${last.error ? 'err' : last.warning ? 'warn' : ''}">${last.finishedAt ? `${formatTime(last.finishedAt)} · ${last.error ? '失败' : last.warning ? '成功（清理有警告）' : '成功'}` : '尚无记录'}</b></div>${backup.pending ? '<div class="pending-mini"><span>待恢复</span><b>重启后生效</b></div>' : ''}</div>`
  $('#dashSources').innerHTML = srcs.length ? srcs.map(s => {
    const state = !s.enabled ? ['off','已停用'] : s.status?.state === 'ready' ? ['ok','就绪'] : ['err','异常']
    const platforms = Object.keys(s.status?.platforms || {}).map(p => platName[p] || p).join('、') || '无平台'
    return `<div class="card dash-source"><div><span class="badge ${state[0]}">${state[1]}</span><b>${esc(s.name)}</b><span class="muted">v${esc(s.version || s.status?.version || '-')}</span></div><div class="muted">${esc(platforms)}</div>${s.status?.error ? `<div class="err source-error">${esc(s.status.error)}</div>` : ''}</div>`
  }).join('') : '<div class="card empty">还没有音源脚本，请到 <a href="#sources">音源</a> 页面上传或导入。</div>'
}
// ---- 音源 ----
async function loadSources() {
  const srcs = await adminAPI('/sources')
  $('#sourceList').innerHTML = srcs.length ? srcs.map(s => sourceCard(s, false)).join('') : '<div class="card empty">暂无音源脚本，请在上方上传或从 URL 导入</div>'
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
async function toggleSource(id, enabled) { await adminAPI('/sources/' + id, { method: 'PUT', body: { enabled } }); loadSources() }
async function setPriority(id, priority) { await adminAPI('/sources/' + id, { method: 'PUT', body: { priority: Number(priority) } }); loadSources() }
async function reloadSource(id) { const r = await adminAPI('/sources/' + id + '/reload', { method: 'POST' }); if (r.error) toast(r.error, true); else toast('已重新加载'); loadSources() }
async function deleteSource(id) { if (!confirm('确定删除该音源？')) return; await adminAPI('/sources/' + id, { method: 'DELETE' }); loadSources() }
async function testSource(id) {
  const box = $('#test-' + id)
  box.innerHTML = '<div class="muted hint" style="margin-top:10px">测试中（搜索“晴天 周杰伦”并取 128k 直链）…</div>'
  try {
    const r = await adminAPI('/sources/' + id + '/test', { method: 'POST', body: {} })
    box.innerHTML = '<div class="table-wrap sm"><table><thead><tr><th>平台</th><th>歌曲</th><th>结果</th><th>耗时</th></tr></thead><tbody>' + Object.entries(r).map(([p, v]) => `<tr><td>${platName[p] || p}</td><td>${esc(v.song || '')}</td><td>${v.ok ? `<span class="badge ok">成功 ${esc(v.quality)}</span> <a href="${esc(v.url)}" target="_blank" rel="noopener">打开链接</a>` : `<span class="badge err">失败</span> <span class="err">${esc(v.error)}</span>`}</td><td class="muted">${v.ms ?? ''} ms</td></tr>`).join('') + '</tbody></table></div>'
  } catch (err) { box.innerHTML = `<div class="note err">${esc(err.message)}</div>` }
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
  const actions = { toggle: () => toggleSource(id, button.dataset.enabled === 'true'), reload: () => reloadSource(id), test: () => testSource(id), delete: () => deleteSource(id) }
  Promise.resolve(actions[button.dataset.sourceAction]?.()).catch(error => { if (!isAbort(error)) toast(error.message, true) })
})
$('#sourceList').addEventListener('change', event => {
  if (event.target.matches('[data-source-priority]')) setPriority(Number(event.target.dataset.sourcePriority), event.target.value).catch(error => toast(error.message, true))
})
$('#uploadSourceForm').addEventListener('submit', uploadSource)
$('#importSourceForm').addEventListener('submit', importSource)
$('#refreshDashboard').addEventListener('click', () => loadDashboard().catch(error => toast(error.message, true)))
$('#refreshLogs').addEventListener('click', () => loadLogs().catch(error => toast(error.message, true)))

return { loadDashboard, loadSources, loadLogs }
})()
