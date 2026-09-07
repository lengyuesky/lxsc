// lxsc 统一控制台脚本（无框架）
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
    if (o.body && !(o.body instanceof FormData)) { o.headers['Content-Type'] = 'application/json'; o.body = JSON.stringify(o.body) }
    const r = await fetch(base + path, o)
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
const esc = value => String(value ?? '').replace(/[&<>"']/g, ch => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]))
const platName = { wy: '网易云', tx: 'QQ音乐', kw: '酷我', kg: '酷狗', mg: '咪咕', local: '本地' }

let toastTimer
function toast(message, isError = false) {
  if (!sessionState.me) return
  const box = $('#toast')
  box.textContent = message
  box.classList.toggle('err', isError)
  box.classList.remove('hidden')
  clearTimeout(toastTimer)
  toastTimer = setTimeout(() => box.classList.add('hidden'), 2600)
}
async function copyText(text) {
  try { await navigator.clipboard.writeText(text); toast('已复制') } catch { toast('复制失败，请手动选择复制', true) }
}

function showLogin() {
  $('#login').classList.remove('hidden')
  $('#app').classList.add('hidden')
  sessionState.me = null
  playlistState.me = null
  resetMusicSession()
  resetSettingsSession()
  clearDetail()
}
function showApp() {
  $('#login').classList.add('hidden')
  $('#app').classList.remove('hidden')
}
async function initializeSession(data) {
  resetMusicSession()
  resetSettingsSession()
  sessionState.me = data.user
  sessionState.defaultPublic = !!data.defaultPublic
  document.querySelectorAll('.admin-only').forEach(el => el.classList.toggle('hidden', !data.user.isAdmin))
  $('#identity').textContent = data.user.name
  $('#roleTag').textContent = data.user.isAdmin ? 'admin' : 'user'
  showApp()
  await initializePlaylists(data)
  route()
}

$('#loginForm').addEventListener('submit', async event => {
  event.preventDefault()
  const loginForm = event.currentTarget
  const form = new FormData(loginForm)
  $('#loginError').textContent = ''
  try {
    const data = await authAPI('/login', { method: 'POST', body: { username: form.get('username'), password: form.get('password') } })
    loginForm.reset()
    await initializeSession(data)
  } catch (error) { $('#loginError').textContent = error.message }
})

$('#logoutButton').addEventListener('click', async event => {
  event.preventDefault()
  if (!confirmDiscard()) return
  await authAPI('/logout', { method: 'POST' })
  clearDetail()
  location.hash = ''
  showLogin()
})

// ---- 路由 ----
const loaders = { playlists: loadPlaylistTab, dashboard: loadDashboard, sources: loadSources, users: loadUsers, backups: loadBackups, settings: loadSettings, search: () => {}, logs: loadLogs }
function route() {
  if (!sessionState.me) return
  const fallback = sessionState.me.isAdmin ? 'dashboard' : 'playlists'
  let tab = (location.hash || '#' + fallback).slice(1)
  if (!loaders[tab] || (!sessionState.me.isAdmin && !['playlists', 'search'].includes(tab))) tab = fallback
  if (location.hash !== '#' + tab) history.replaceState(null, '', '#' + tab)
  document.querySelectorAll('nav a[data-tab]').forEach(a => a.classList.toggle('active', a.dataset.tab === tab))
  document.querySelectorAll('.tab').forEach(s => s.classList.toggle('active', s.id === 'tab-' + tab))
  Promise.resolve((loaders[tab] || loaders[fallback])()).catch(error => { if (!isAbort(error)) toast(error.message, true) })
}
window.addEventListener('hashchange', route)

// ---- 概览 ----
function sourceCard(s, brief) {
  const st = s.status
  const badge = !s.enabled ? '<span class="badge off">已停用</span>' : !st ? '<span class="badge off">未加载</span>' : st.state === 'ready' ? '<span class="badge ok">就绪</span>' : st.state === 'loading' ? '<span class="badge pri">加载中</span>' : '<span class="badge err">错误</span>'
  const chips = st && st.platforms ? Object.entries(st.platforms).map(([p, c]) => `<span class="chip"><b>${esc(platName[p] || p)}</b><span>${esc((c.qualitys || []).join('/'))}</span></span>`).join('') : ''
  const err = st && st.error ? `<div class="note err">${esc(st.error)}</div>` : ''
  const alert = st && st.alert ? `<div class="note warn">脚本提示更新：${esc(st.alert)}</div>` : ''
  const meta = [s.description, s.author, s.homepage ? `<a href="${esc(s.homepage)}" target="_blank" rel="noopener">主页</a>` : ''].map((x, i) => i === 2 ? x : esc(x || '')).filter(Boolean).join(' · ')
  const actions = brief ? '' : `<div class="actions">
      <button class="sec sm" onclick="toggleSource(${s.id},${!s.enabled})">${s.enabled ? '停用' : '启用'}</button>
      <button class="sec sm" onclick="reloadSource(${s.id})"><svg class="icon"><use href="#i-refresh"/></svg>重新加载</button>
      <button class="sec sm" onclick="testSource(${s.id})"><svg class="icon"><use href="#i-zap"/></svg>测试取链</button>
      <a class="btn sec sm" href="/api/admin/sources/${s.id}/script" download>下载脚本</a>
      <label class="prio">优先级<input type="number" value="${s.priority}" onchange="setPriority(${s.id},this.value)"></label>
      <button class="danger sm" onclick="deleteSource(${s.id})">删除</button>
    </div><div id="test-${s.id}"></div>
    ${st && st.logs && st.logs.length ? `<details><summary>脚本日志（${st.logs.length}）</summary><pre>${esc(st.logs.slice(-50).join('\n'))}</pre></details>` : ''}`
  return `<div class="card src">
    <div class="src-head"><div>
      <div class="src-title">${esc(s.name)} ${badge} <span class="ver">v${esc(s.version || st?.version || '')}</span></div>
      ${meta ? `<div class="meta">${meta}</div>` : ''}
    </div></div>
    <div class="chips">${chips || '<span class="muted hint">无平台信息</span>'}</div>${err}${alert}${actions}</div>`
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
const fmtDur = s => s < 60 ? Math.floor(s) + ' 秒' : s < 3600 ? Math.floor(s / 60) + ' 分钟' : s < 86400 ? (s / 3600).toFixed(1) + ' 小时' : (s / 86400).toFixed(1) + ' 天'
const formatBytes = n => !n ? '0 B' : n < 1024 ? n + ' B' : n < 1048576 ? (n / 1024).toFixed(1) + ' KB' : n < 1073741824 ? (n / 1048576).toFixed(1) + ' MB' : (n / 1073741824).toFixed(2) + ' GB'

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

// ---- 用户 ----
let usersCache = []
async function loadUsers() {
  usersCache = await adminAPI('/users')
  $('#userTable tbody').innerHTML = usersCache.length ? usersCache.map(u => `<tr><td class="muted">${u.id}</td><td><b>${esc(u.name)}</b></td><td>${u.isAdmin ? '<span class="badge pri">管理员</span>' : '<span class="badge off">普通用户</span>'}</td><td><code>${esc(u.quality)}</code></td>
    <td class="actions"><button class="sec sm" onclick="editUser(${u.id})">编辑</button><button class="sec sm" onclick="apiKey(${u.id})">生成 API Key</button><button class="danger sm" onclick="deleteUser(${u.id})">删除</button></td></tr>`).join('') : '<tr><td colspan="5" class="muted center">暂无用户</td></tr>'
}
async function createUser(e) {
  e.preventDefault()
  const f = new FormData(e.target)
  try { await adminAPI('/users', { method: 'POST', body: { name: f.get('name'), password: f.get('password'), quality: f.get('quality'), isAdmin: f.get('isAdmin') === 'on' } }); e.target.reset(); toast('用户已创建'); loadUsers() } catch (err) { toast(err.message, true) }
  return false
}
function editUser(id) {
  const u = usersCache.find(x => x.id === id); if (!u) return
  const d = $('#userDialog'), f = d.querySelector('form')
  f.reset()
  f.elements.id.value = u.id
  f.elements.name.value = u.name
  f.elements.quality.value = u.quality
  f.elements.isAdmin.checked = !!u.isAdmin
  d.showModal()
}
async function submitUser(e) {
  e.preventDefault()
  const f = e.target
  try {
    await adminAPI('/users/' + f.elements.id.value, { method: 'PUT', body: { name: f.elements.name.value, password: f.elements.password.value, quality: f.elements.quality.value, isAdmin: f.elements.isAdmin.checked } })
    $('#userDialog').close(); toast('已保存'); loadUsers()
  } catch (err) { toast(err.message, true) }
  return false
}
async function deleteUser(id) { if (!confirm('确定删除该用户及其歌单/收藏？')) return; try { await adminAPI('/users/' + id, { method: 'DELETE' }); toast('用户已删除'); loadUsers() } catch (err) { toast(err.message, true) } }
async function apiKey(id) {
  try { const r = await adminAPI('/users/' + id + '/apikey', { method: 'POST' }); $('#keyValue').value = r.apiKey; $('#keyDialog').showModal(); $('#keyValue').select() } catch (err) { toast(err.message, true) }
}

// ---- 备份 ----
let webdavConfig = null
async function loadBackups() {
  const [status, config] = await Promise.all([adminAPI('/backups/status'), adminAPI('/backups/webdav/config')])
  webdavConfig = config
  renderPendingRestore(status)
  fillWebDAVConfig(config)
  renderBackupLast(status.last || {})
  if (config.url) await loadRemoteBackups()
  else $('#remoteBackupTable tbody').innerHTML = '<tr><td colspan="4">请先配置 WebDAV</td></tr>'
}
function renderPendingRestore(status) {
  const box = $('#pendingRestore')
  if (!status.pending) {
    box.innerHTML = status.lastRestore ? `<div class="card notice backup-result"><svg class="icon"><use href="#i-backup"/></svg><span>${esc(status.lastRestore)}</span></div>` : ''
    return
  }
  const p = status.pending
  box.innerHTML = `<div class="card pending-restore"><div><span class="badge warn">等待重启</span><h4>完整备份已经校验并暂存</h4><p>来源：${esc(p.source)} · 备份时间：${esc(formatTime(p.backupAt))} · 版本：${esc(p.sourceVersion)}</p><p class="muted hint">请执行 <code>docker compose restart</code>，重启后将替换当前数据。当前运行数据尚未改变。</p></div><button class="danger sm" onclick="cancelPendingRestore()">取消恢复</button></div>`
}
function fillWebDAVConfig(config) {
  const f = $('#webdavForm')
  for (const name of ['url','username','path','frequency','time','weekday','retention']) if (f.elements[name]) f.elements[name].value = config[name] ?? ''
  f.elements.auto.checked = !!config.auto
  f.elements.password.value = ''
  f.elements.backupPassword.value = ''
  f.elements.clearPassword.checked = false
  f.elements.clearBackupPassword.checked = false
  $('#webdavPasswordState').textContent = config.passwordConfigured ? '已保存密码；留空不会修改' : '尚未配置密码'
  $('#backupPasswordState').textContent = config.backupPasswordConfigured ? '远程备份将使用已保存密码加密' : '留空则远程备份不额外加密'
  $('#backupTimezone').textContent = `按服务器时区 ${config.timezone || 'Local'} 执行`
  $('#webdavSecurity').className = `badge ${config.url?.startsWith('https://') ? 'ok' : config.url ? 'warn' : 'off'}`
  $('#webdavSecurity').textContent = config.url?.startsWith('https://') ? 'HTTPS' : config.url ? 'HTTP 不安全' : '未配置'
  $('#weekdayWrap').classList.toggle('hidden', config.frequency !== 'weekly')
}
$('#webdavForm').elements.frequency.addEventListener('change', event => $('#weekdayWrap').classList.toggle('hidden', event.target.value !== 'weekly'))
function exportBackup(event) {
  event.preventDefault()
  const source = event.currentTarget, password = source.elements.password.value
  if (password !== source.elements.confirmPassword.value) { toast('两次备份密码不一致', true); return false }
  // 使用原生表单下载，让浏览器直接流式保存，不把大型备份完整载入页面内存。
  const frameName = 'backup-download-' + Date.now()
  const frame = document.createElement('iframe'); frame.name = frameName; frame.className = 'hidden'; document.body.appendChild(frame)
  const form = document.createElement('form'); form.method = 'POST'; form.action = '/api/admin/backups/export'; form.target = frameName; form.className = 'hidden'
  const input = document.createElement('input'); input.name = 'password'; input.value = password; form.appendChild(input); document.body.appendChild(form)
  form.submit(); source.reset(); toast('正在生成完整备份，浏览器稍后开始下载')
  setTimeout(() => { form.remove(); frame.remove() }, 10 * 60 * 1000)
  return false
}
let backupPreviewSignature = ''
function currentBackupSignature() {
  const form = $('#importBackupForm'), file = form.elements.file.files[0]
  return file ? `${file.name}|${file.size}|${file.lastModified}|${form.elements.password.value}` : ''
}
function resetBackupPreview() {
  backupPreviewSignature = ''
  const form = $('#importBackupForm')
  form.elements.confirm.checked = false
  form.elements.confirm.disabled = true
  form.querySelector('button[type="submit"]').disabled = true
  $('#backupPreview').className = 'backup-preview muted'
  $('#backupPreview').textContent = '尚未检查备份'
}
$('#importBackupForm').elements.file.addEventListener('change', resetBackupPreview)
$('#importBackupForm').elements.password.addEventListener('input', resetBackupPreview)
async function previewBackup() {
  const form = $('#importBackupForm'), file = form.elements.file.files[0]
  if (!file) { toast('请先选择备份文件', true); return }
  const button = form.querySelector('button[type="button"]'); button.disabled = true; button.textContent = '正在检查…'
  try {
    const data = new FormData(); data.append('file', file); data.append('password', form.elements.password.value)
    const result = await adminAPI('/backups/inspect', { method: 'POST', body: data })
    const m = result.manifest || {}, stats = m.stats || {}
    $('#backupPreview').className = 'backup-preview ok'
    $('#backupPreview').innerHTML = `<b>备份有效</b><span>创建于 ${esc(formatTime(m.createdAt))} · lxsc ${esc(m.appVersion || '-')} · ${m.encrypted ? '已加密' : '未加密'}</span><span>${stats.users || 0} 个用户 · ${stats.playlists || 0} 个歌单 · ${stats.tracks || 0} 首歌曲元数据</span>`
    backupPreviewSignature = currentBackupSignature()
    form.elements.confirm.disabled = false
    form.querySelector('button[type="submit"]').disabled = false
  } catch (error) { resetBackupPreview(); toast(error.message, true) }
  finally { button.disabled = false; button.textContent = '检查备份' }
}
async function importBackup(event) {
  event.preventDefault()
  const form = event.currentTarget
  if (!backupPreviewSignature || backupPreviewSignature !== currentBackupSignature()) { resetBackupPreview(); toast('文件或密码已变化，请重新检查备份', true); return false }
  if (!form.elements.confirm.checked || !confirm('确定暂存这个完整备份吗？重启服务后当前用户、歌单、设置和音源将被替换。')) return false
  const button = form.querySelector('button[type="submit"]'); button.disabled = true; button.textContent = '正在暂存…'
  try {
    const data = new FormData(form)
    await adminAPI('/backups/import', { method: 'POST', body: data })
    form.reset(); resetBackupPreview(); toast('备份已校验并暂存，请重启服务'); await loadBackups()
  } catch (error) { toast(error.message, true) }
  finally { button.disabled = !backupPreviewSignature; button.textContent = '暂存恢复' }
  return false
}
async function cancelPendingRestore() {
  if (!confirm('取消尚未生效的恢复任务？')) return
  try { await adminAPI('/backups/pending', { method: 'DELETE' }); toast('已取消待恢复任务'); loadBackups() } catch (error) { toast(error.message, true) }
}
async function saveWebDAV(event) {
  event.preventDefault()
  const f = event.currentTarget
  const body = { url: f.elements.url.value, username: f.elements.username.value, password: f.elements.password.value, path: f.elements.path.value, backupPassword: f.elements.backupPassword.value, frequency: f.elements.frequency.value, time: f.elements.time.value, weekday: Number(f.elements.weekday.value), retention: Number(f.elements.retention.value), auto: f.elements.auto.checked, clearPassword: f.elements.clearPassword.checked, clearBackupPassword: f.elements.clearBackupPassword.checked }
  try { webdavConfig = await adminAPI('/backups/webdav/config', { method: 'PUT', body }); fillWebDAVConfig(webdavConfig); toast('WebDAV 配置已保存'); if (webdavConfig.url) await loadRemoteBackups(); else $('#remoteBackupTable tbody').innerHTML = '<tr><td colspan="4">请先配置 WebDAV</td></tr>' } catch (error) { toast(error.message, true) }
  return false
}
async function testWebDAV() {
  try { await adminAPI('/backups/webdav/test', { method: 'POST', body: {} }); toast('WebDAV 连接成功') } catch (error) { toast(error.message, true) }
}
async function runWebDAVBackup() {
  if (!confirm('现在生成完整备份并上传到 WebDAV？')) return
  try { toast('正在生成并上传备份…'); const status = await adminAPI('/backups/webdav/run', { method: 'POST', body: {} }); renderBackupLast(status.last || {}); toast('WebDAV 备份完成'); await loadRemoteBackups() } catch (error) { toast(error.message, true) }
}
async function loadRemoteBackups() {
  const tbody = $('#remoteBackupTable tbody'); tbody.innerHTML = '<tr><td colspan="4">正在读取远程目录…</td></tr>'
  try {
    const files = await adminAPI('/backups/webdav/files')
    tbody.innerHTML = files.length ? files.map(file => `<tr><td><b>${esc(file.name)}</b></td><td>${esc(formatTime(file.lastModified))}</td><td>${formatBytes(file.size)}</td><td class="actions"><a class="btn sec sm" href="/api/admin/backups/webdav/files/${encodeURIComponent(file.name)}">下载</a><button class="sec sm" onclick="restoreRemoteBackup(decodeURIComponent('${encodeURIComponent(file.name)}'))">恢复</button><button class="danger sm" onclick="deleteRemoteBackup(decodeURIComponent('${encodeURIComponent(file.name)}'))">删除</button></td></tr>`).join('') : '<tr><td colspan="4">远程目录暂无备份</td></tr>'
  } catch (error) { tbody.innerHTML = `<tr><td colspan="4" class="err">${esc(error.message)}</td></tr>` }
}
async function restoreRemoteBackup(name) {
  const password = prompt('如该备份已加密，请输入备份密码；使用已保存的远程备份密码可留空：', '')
  if (password === null || !confirm(`确定暂存远程备份 ${name}？重启后将替换当前数据。`)) return
  try { await adminAPI('/backups/webdav/files/' + encodeURIComponent(name) + '/restore', { method: 'POST', body: { password } }); toast('远程备份已暂存，请重启服务'); await loadBackups() } catch (error) { toast(error.message, true) }
}
async function deleteRemoteBackup(name) {
  if (!confirm(`永久删除远程备份 ${name}？`)) return
  try { await adminAPI('/backups/webdav/files/' + encodeURIComponent(name), { method: 'DELETE' }); toast('远程备份已删除'); await loadRemoteBackups() } catch (error) { toast(error.message, true) }
}
function renderBackupLast(last) {
  $('#backupLastStatus').textContent = !last.finishedAt ? '尚无备份记录' : `${formatTime(last.finishedAt)} · ${last.target || ''} · ${last.error ? '失败：' + last.error : last.warning ? '成功，但有警告：' + last.warning : '成功 ' + formatBytes(last.size || 0)}`
}

// ---- 设置 ----
const settingsState = { request: 0, ready: false, saving: false, dirty: false }
const boardSettingsState = { request: 0, ready: false, saving: false, dirty: false }
const boardChoices = new LXSCBoardSettings.BoardSelectionState(
  (source, signal) => adminAPI('/boards?source=' + encodeURIComponent(source), { signal }),
  source => renderBoardSource(source),
)

function resetSettingsSession() {
  for (const [state, form, message] of [[settingsState, '#settingsForm', '#settingsMsg'], [boardSettingsState, '#boardSettingsForm', '#boardSettingsMsg']]) {
    state.request++
    state.ready = false
    state.saving = false
    state.dirty = false
    $(form).inert = true
    $(message).textContent = ''
  }
  boardChoices.reset()
  $('#boardSelections').replaceChildren()
  $('#boardSettingsPanel').open = false
  $('#boardSettingsForm').classList.add('hidden')
  $('#boardSettingsLoadNotice').classList.add('hidden')
}

function markSettingsDirty() {
  if (!settingsState.ready || settingsState.saving) return
  settingsState.dirty = true
  $('#settingsMsg').textContent = '有未保存的修改'
}

function markBoardSettingsDirty() {
  if (!boardSettingsState.ready || boardSettingsState.saving) return
  boardSettingsState.dirty = true
  $('#boardSettingsMsg').className = 'muted'
  $('#boardSettingsMsg').textContent = '有未保存的修改'
}

function syncBoardSettings() {
  const form = $('#boardSettingsForm'), root = $('#boardSelections')
  const sources = [...new Set(form.elements.boardSources.value.split(/[,，\s]+/).filter(source => ['wy', 'tx', 'kw', 'kg', 'mg'].includes(source)))]
  $('#boardDisplayHint').textContent = form.elements.showBoards.checked
    ? '在线音乐目录和客户端歌单列表共用以下选择，保存后刷新客户端列表。'
    : '榜单展示已关闭；下方选择仍会保存，重新开启后恢复展示。'
  root.querySelectorAll('[data-board-source]').forEach(group => { if (!sources.includes(group.dataset.boardSource)) group.remove() })
  root.querySelector('[data-board-empty]')?.remove()
  if (!sources.length) root.innerHTML = '<p class="muted hint" data-board-empty>尚未选择有效平台，请在上方填写平台代码。</p>'
  for (const source of sources) {
    let group = root.querySelector(`[data-board-source="${source}"]`)
    if (!group) {
      group = document.createElement('details')
      group.className = 'board-platform'
      group.dataset.boardSource = source
      group.open = boardChoices.isCustom(source)
      group.innerHTML = `<summary><strong>${esc(platName[source])}</strong><span data-board-summary></span></summary><div class="board-content" data-board-content></div>`
    }
    root.append(group)
    renderBoardSource(source)
    void boardChoices.load(source)
  }
}

function renderBoardSource(source) {
  const group = $('#boardSelections').querySelector(`[data-board-source="${source}"]`)
  if (!group) return
  const catalog = boardChoices.catalog(source), custom = boardChoices.isCustom(source)
  const selected = new Set(boardChoices.selections[source] || []), options = boardChoices.options(source)
  const summary = group.querySelector('[data-board-summary]')
  summary.textContent = (custom ? `已选 ${selected.size} 个` : '全部榜单') + (catalog.loading ? ' · 加载中…' : catalog.error ? ' · 加载失败' : catalog.loaded ? ` · 目录 ${catalog.boards.length} 个` : '')
  const content = group.querySelector('[data-board-content]'), scrollTop = content.querySelector('.board-options')?.scrollTop || 0
  const focused = content.contains(document.activeElement) ? document.activeElement : null
  const focusAttr = focused && ['data-board-id', 'data-board-mode', 'data-board-action'].find(attr => focused.hasAttribute(attr))
  const focusSelector = focusAttr ? `[${focusAttr}="${CSS.escape(focused.getAttribute(focusAttr))}"]` : null
  const count = custom ? `已选 ${selected.size} 个榜单` : `展示全部 ${catalog.boards.length} 个榜单`
  content.innerHTML = `<label>展示方式<select data-board-mode aria-label="${esc(platName[source])}榜单展示方式"><option value="all" ${custom ? '' : 'selected'}>全部榜单</option><option value="custom" ${custom ? 'selected' : ''} ${!catalog.loaded && !custom ? 'disabled' : ''}>自定义</option></select></label>
    <p class="muted hint">${custom ? '只展示勾选的榜单；新榜单不会自动加入。清空后隐藏该平台入口。' : '包含以后新增的榜单；切换到自定义可逐项选择。'}</p>
    ${catalog.error ? `<p class="err hint" role="alert">${esc(catalog.error)}，已有选择已保留。</p>` : ''}
    <div class="board-actions"><span class="muted hint" role="status">${catalog.loading ? '正在加载榜单名称…' : catalog.loaded || custom ? count : '榜单名称尚未加载'}</span>
      ${custom ? `<button type="button" class="sec sm" data-board-action="all" ${catalog.loaded ? '' : 'disabled'}>全选</button><button type="button" class="sec sm" data-board-action="clear">清空</button>` : ''}
      <button type="button" class="sec sm" data-board-action="reload" ${catalog.loading ? 'disabled' : ''}>${catalog.error ? '重试' : '刷新榜单'}</button></div>
    ${options.length ? `<div class="board-options">${options.map(board => `<label><input type="checkbox" data-board-id="${esc(board.bangid)}" ${!custom || selected.has(board.bangid) ? 'checked' : ''} ${custom ? '' : 'disabled'}><span>${esc(board.name)}${board.missing ? `<small>${catalog.loaded && !catalog.error ? '当前目录未返回，选择仍保留' : '已保存的选择，名称暂不可用'}</small>` : ''}</span></label>`).join('')}</div>` : catalog.loaded ? '<p class="muted hint">该平台暂未返回可选榜单。</p>' : ''}`
  if (focusSelector) content.querySelector(focusSelector)?.focus({ preventScroll: true })
  const list = content.querySelector('.board-options')
  if (list) list.scrollTop = scrollTop
}

$('#boardSelections').addEventListener('change', event => {
  const input = event.target, source = input.closest('[data-board-source]')?.dataset.boardSource
  if (!source) return
  if (input.hasAttribute('data-board-mode')) boardChoices.setMode(source, input.value)
  else if (input.hasAttribute('data-board-id')) boardChoices.toggle(source, input.dataset.boardId, input.checked)
})
$('#boardSelections').addEventListener('click', event => {
  const button = event.target.closest('[data-board-action]')
  if (!button) return
  const source = button.closest('[data-board-source]').dataset.boardSource
  if (button.dataset.boardAction === 'reload') { void boardChoices.load(source, true); return }
  if (button.dataset.boardAction === 'all') boardChoices.selectAll(source)
  else if (button.dataset.boardAction === 'clear') boardChoices.clear(source)
  markBoardSettingsDirty()
})
$('#boardSettingsForm').addEventListener('input', event => {
  markBoardSettingsDirty()
  // 输入时调整平台分组，避免失焦后才改变布局而使保存按钮在点击途中移动。
  if (event.target.name === 'boardSources') syncBoardSettings()
})
$('#boardSettingsForm').addEventListener('change', event => {
  markBoardSettingsDirty()
  if (event.target.name === 'showBoards') syncBoardSettings()
})
$('#settingsForm').addEventListener('input', markSettingsDirty)
$('#settingsForm').addEventListener('change', markSettingsDirty)
$('#boardSettingsPanel').addEventListener('toggle', event => {
  if (event.currentTarget.open) void loadBoardSettings()
})

async function loadBoardSettings() {
  if (!sessionState.me?.isAdmin || boardSettingsState.saving || (boardSettingsState.ready && boardSettingsState.dirty)) return
  const request = ++boardSettingsState.request, form = $('#boardSettingsForm')
  form.inert = true
  form.setAttribute('aria-busy', 'true')
  $('#boardSettingsLoadNotice').classList.remove('hidden')
  $('#boardSettingsLoadMsg').className = ''
  $('#boardSettingsLoadMsg').textContent = '正在读取榜单设置…'
  $('#retryBoardSettings').classList.add('hidden')
  try {
    const values = await adminAPI('/settings')
    if (request !== boardSettingsState.request) return
    form.elements.showBoards.checked = !!values.showBoards
    form.elements.boardSources.value = (values.boardSources || []).join(',')
    boardSettingsState.ready = true
    boardSettingsState.dirty = false
    $('#boardSettingsMsg').textContent = ''
    boardChoices.reset(values.boardSelections || {})
    $('#boardSelections').replaceChildren()
    syncBoardSettings()
    form.classList.remove('hidden')
    $('#boardSettingsLoadNotice').classList.add('hidden')
  } catch (error) {
    if (request === boardSettingsState.request && !isAbort(error)) {
      $('#boardSettingsLoadMsg').className = 'err'
      $('#boardSettingsLoadMsg').textContent = '读取榜单设置失败：' + error.message
      $('#retryBoardSettings').classList.remove('hidden')
    }
  } finally {
    if (request === boardSettingsState.request) {
      form.inert = !boardSettingsState.ready
      form.setAttribute('aria-busy', 'false')
    }
  }
}

async function saveBoardSettings(event) {
  event.preventDefault()
  if (!sessionState.me?.isAdmin || !boardSettingsState.ready || boardSettingsState.saving) return false
  const form = event.target, request = ++boardSettingsState.request
  // 只提交榜单字段，避免覆盖系统设置页或当前歌单的未保存修改。
  const body = {
    showBoards: form.elements.showBoards.checked,
    boardSources: form.elements.boardSources.value.split(/[,，\s]+/).filter(Boolean),
    boardSelections: boardChoices.snapshot(),
  }
  boardSettingsState.saving = true
  form.inert = true
  form.setAttribute('aria-busy', 'true')
  try {
    await adminAPI('/settings', { method: 'PUT', body })
    if (request !== boardSettingsState.request) return false
    boardSettingsState.dirty = false
    $('#boardSettingsMsg').className = 'ok'
    $('#boardSettingsMsg').textContent = '已保存'
    toast('榜单设置已保存，客户端刷新列表后生效')
  } catch (error) {
    if (request === boardSettingsState.request && !isAbort(error)) {
      $('#boardSettingsMsg').className = 'err'
      $('#boardSettingsMsg').textContent = '保存失败，修改已保留'
      toast(error.message, true)
    }
  } finally {
    if (request === boardSettingsState.request) {
      boardSettingsState.saving = false
      form.inert = false
      form.setAttribute('aria-busy', 'false')
    }
  }
  return false
}

async function loadSettings() {
  // 导航回来时保留本页草稿；较早的设置响应不得覆盖较新的读取或保存。
  if (settingsState.saving || (settingsState.ready && settingsState.dirty)) return
  const request = ++settingsState.request
  const f = $('#settingsForm')
  f.inert = true
  f.setAttribute('aria-busy', 'true')
  try {
    const v = await adminAPI('/settings')
    if (request !== settingsState.request) return
    for (const [k, val] of Object.entries(v)) {
      const el = f.elements[k]; if (!el) continue
      if (el.type === 'checkbox') el.checked = !!val
      else el.value = Array.isArray(val) ? val.join(',') : val
    }
    settingsState.ready = true
    settingsState.dirty = false
    $('#settingsMsg').textContent = ''
  } finally {
    if (request === settingsState.request) {
      f.inert = !settingsState.ready
      f.setAttribute('aria-busy', 'false')
    }
  }
}
async function saveSettings(e) {
  e.preventDefault()
  if (!settingsState.ready || settingsState.saving) return false
  const f = e.target
  const body = {}
  for (const el of f.elements) {
    if (!el.name) continue
    if (el.type === 'checkbox') body[el.name] = el.checked
    else if (el.type === 'number') body[el.name] = Number(el.value)
    else if (el.name.endsWith('Sources')) body[el.name] = el.value.split(/[,，\s]+/).filter(Boolean)
    else body[el.name] = el.value
  }
  const request = ++settingsState.request
  settingsState.saving = true
  f.inert = true
  f.setAttribute('aria-busy', 'true')
  try {
    await adminAPI('/settings', { method: 'PUT', body })
    if (request !== settingsState.request) return false
    settingsState.dirty = false
    $('#settingsMsg').textContent = '已保存'
    toast('设置已保存')
  } catch (err) {
    if (request === settingsState.request && !isAbort(err)) toast(err.message, true)
  } finally {
    if (request === settingsState.request) {
      settingsState.saving = false
      f.inert = false
      f.setAttribute('aria-busy', 'false')
    }
  }
  return false
}
async function cleanupMetadata() {
  if (!confirm('确定清理无引用元数据并压缩数据库？歌单、收藏和播放历史不会被删除。')) return
  try {
    const r = await adminAPI('/metadata/cleanup', { method: 'POST', body: {} })
    const c = r.cleanup || {}
    toast(`已清理 ${c.tracks || 0} 首歌曲、${c.albums || 0} 个专辑、${c.artists || 0} 个歌手缓存`)
    if (r.warning) toast(r.warning, true)
    await loadDashboard()
  } catch (err) { toast(err.message, true) }
}

// ---- 日志 ----
async function loadLogs() {
  const logs = await adminAPI('/logs?n=300')
  const box = $('#logBox')
  box.innerHTML = logs.length ? logs.map(l => `<div class="log-line lv-${esc(String(l.level).toLowerCase())}"><span class="t">${esc(l.time)}</span><span class="lv">${esc(l.level)}</span><span class="m">${esc(l.message)}</span></div>`).join('') : '<div class="empty">暂无日志</div>'
  box.scrollTop = box.scrollHeight
}

// ---- 歌单 ----
const playlistState = {
  me: null,
  defaultPublic: false,
  users: [],
  playlists: [],
  current: null,
  draftTracks: [],
  draftRevision: '',
  editVersion: 0,
  tracksDirty: false,
  metaDirty: false,
  searchResults: [],
  dragIndex: -1,
}

function formatTime(value) {
  if (!value) return ''
  return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'short', timeStyle: 'short' }).format(new Date(value * 1000))
}

async function initializePlaylists(data) {
  playlistState.me = data.user
  playlistState.defaultPublic = !!data.defaultPublic
  playlistState.users = []
  playlistState.playlists = []
  playlistState.current = null
  playlistState.draftTracks = []
  playlistState.tracksDirty = false
  playlistState.metaDirty = false
  playlistState.searchResults = []
  $('#ownerFilterWrap').classList.toggle('hidden', !playlistState.me.isAdmin)
  $('#ownerCreateWrap').classList.toggle('hidden', !playlistState.me.isAdmin)
  $('#importOwnerWrap').classList.toggle('hidden', !playlistState.me.isAdmin)
  syncQualityControls()
  $('#importForm').elements.public.checked = playlistState.defaultPublic
  resetImportForm()
  $('#createForm').elements.public.checked = playlistState.defaultPublic
  clearDetail()
}

async function loadPlaylistTab() {
  if (playlistState.me?.isAdmin) {
    if ($('#boardSettingsPanel').open) void loadBoardSettings()
    const filterValue = $('#ownerFilter').value
    const createValue = $('#createForm').elements.ownerId.value
    const importValue = $('#importForm').elements.ownerId.value
    playlistState.users = await playlistAPI('/users')
    renderUserOptions()
    if ([...$('#ownerFilter').options].some(option => option.value === filterValue)) $('#ownerFilter').value = filterValue
    if ([...$('#createForm').elements.ownerId.options].some(option => option.value === createValue)) $('#createForm').elements.ownerId.value = createValue
    if ([...$('#importForm').elements.ownerId.options].some(option => option.value === importValue)) $('#importForm').elements.ownerId.value = importValue
  }
  await loadPlaylists()
}

function renderUserOptions() {
  const filter = $('#ownerFilter')
  const create = $('#createForm').elements.ownerId
  filter.innerHTML = '<option value="">全部用户</option>' + playlistState.users.map(u => `<option value="${u.id}">${esc(u.name)}</option>`).join('')
  create.innerHTML = playlistState.users.map(u => `<option value="${u.id}" ${u.id === playlistState.me.id ? 'selected' : ''}>${esc(u.name)}</option>`).join('')
  $('#importForm').elements.ownerId.innerHTML = create.innerHTML
}

async function loadPlaylists() {
  const selectedID = playlistState.current?.id
  const ownerID = playlistState.me?.isAdmin ? $('#ownerFilter').value : ''
  const request = playlistListGate.begin()
  let playlists
  try { playlists = await playlistAPI('/playlists' + (ownerID ? '?ownerId=' + encodeURIComponent(ownerID) : ''), { signal: request.signal }) }
  catch (error) { if (isAbort(error)) return false; throw error }
  if (!request.current()) return false
  playlistState.playlists = playlists
  renderPlaylistList()
  if (selectedID && playlistState.current?.id === selectedID && !playlists.some(item => item.id === selectedID) && !playlistState.tracksDirty && !playlistState.metaDirty) clearDetail()
  return true
}

function renderPlaylistList() {
  const box = $('#playlistList')
  if (!playlistState.playlists.length) {
    box.innerHTML = '<div class="card muted">暂无歌单，可在下方新建</div>'
    return
  }
  box.innerHTML = playlistState.playlists.map(item => `<button class="playlist-item ${playlistState.current?.id === item.id ? 'active' : ''}" data-playlist-id="${esc(item.id)}">
    <div class="playlist-name"><span>${esc(item.name)}</span>${item.public ? '<span class="badge public">公开</span>' : '<span class="badge">私有</span>'}</div>
    <div class="playlist-meta"><span>${esc(item.owner)} · ${item.songCount} 首</span><span>${formatTime(item.updatedAt)}</span></div>
  </button>`).join('')
}

function confirmDiscard() {
  return (!playlistState.tracksDirty && !playlistState.metaDirty) || confirm('当前歌单有尚未保存的变更，确定放弃吗？')
}

async function selectPlaylist(id, force = false) {
  if (!force && playlistState.current?.id === id) return
  if (!force && !confirmDiscard()) return
  const request = playlistDetailGate.begin()
  playlistDetailTarget = id
  const editVersion = playlistState.editVersion
  const detail = await playlistAPI('/playlists/' + encodeURIComponent(id), { signal: request.signal })
  if (!request.current() || playlistState.editVersion !== editVersion) return false
  playlistSearchGate.cancel()
  playlistState.current = detail
  playlistState.draftTracks = detail.tracks.map(track => ({ ...track }))
  playlistState.draftRevision = detail.tracksRevision
  playlistState.tracksDirty = false
  playlistState.metaDirty = false
  playlistState.searchResults = []
  renderDetail()
  renderPlaylistList()
  updateImportMode()
  return true
}

function clearDetail() {
  playlistDetailGate.cancel()
  playlistDetailTarget = ''
  playlistState.editVersion++
  playlistSearchGate.cancel()
  playlistState.current = null
  playlistState.draftTracks = []
  playlistState.draftRevision = ''
  playlistState.tracksDirty = false
  playlistState.metaDirty = false
  playlistState.searchResults = []
  $('#detail').classList.add('hidden')
  $('#emptyDetail').classList.remove('hidden')
  renderPlaylistList()
  updateImportMode()
}

function renderDetail() {
  const item = playlistState.current
  if (!item) return clearDetail()
  $('#emptyDetail').classList.add('hidden')
  $('#detail').classList.remove('hidden')
  $('#detailTitle').textContent = item.name
  $('#detailOwner').textContent = `${item.owner} · 创建于 ${formatTime(item.createdAt)} · 更新于 ${formatTime(item.updatedAt)}`
  const form = $('#metaForm')
  form.elements.name.value = item.name
  form.elements.comment.value = item.comment || ''
  form.elements.public.checked = !!item.public
  for (const control of form.elements) control.disabled = !item.canEdit
  $('#deleteButton').classList.toggle('hidden', !item.canEdit)
  $('#readonlyNotice').classList.toggle('hidden', item.canEdit)
  $('#searchSection').classList.toggle('hidden', !item.canEdit)
  $('#searchResults').innerHTML = ''
  playlistState.metaDirty = false
  renderTracks()
}

function renderTracks() {
  const canEdit = !!playlistState.current?.canEdit
  $('#trackCount').textContent = `(${playlistState.draftTracks.length} 首)`
  $('#saveTracksButton').disabled = !canEdit || !playlistState.tracksDirty
  renderCollectForm()
  document.querySelector('.tracks-card .track-tip').classList.toggle('hidden', !canEdit)
  const body = $('#trackTable')
  if (!playlistState.draftTracks.length) {
    body.innerHTML = `<tr><td colspan="6" class="muted">${canEdit ? '歌单还是空的，可从上方搜索添加歌曲。' : '该歌单暂无歌曲。'}</td></tr>`
    return
  }
  body.innerHTML = playlistState.draftTracks.map((track, index) => `<tr class="track-row" data-track-index="${index}" data-playing-id="${esc(track.id)}" draggable="${canEdit}">
    <td>${index + 1}</td>
    <td class="${track.unavailable ? 'unavailable' : ''}"><div class="song-title">${esc(track.name)}</div>${track.unavailable ? `<div class="song-sub">${esc(track.id)}</div>` : ''}</td>
    <td>${esc(track.singer || '')}</td><td class="muted">${esc(track.album || '')}</td><td><span class="badge">${esc(platName[track.source] || track.source || '')}</span></td>
    <td class="track-actions"><button type="button" title="播放" aria-label="播放 ${esc(track.name)}" data-track-action="play" ${track.unavailable ? 'disabled' : ''}><svg class="icon"><use href="#i-play"/></svg></button>${canEdit ? `<button type="button" title="上移" data-track-action="up" ${index === 0 ? 'disabled' : ''}><svg class="icon"><use href="#i-up"/></svg></button><button type="button" title="下移" data-track-action="down" ${index === playlistState.draftTracks.length - 1 ? 'disabled' : ''}><svg class="icon"><use href="#i-down"/></svg></button><button type="button" title="移除" class="remove" data-track-action="remove"><svg class="icon"><use href="#i-x"/></svg></button>` : ''}</td>
  </tr>`).join('')
  renderPlaybackMarkers()
}

function markTracksDirty() {
  playlistState.editVersion++
  playlistState.tracksDirty = true
  renderTracks()
  renderSearchResults()
}

function moveTrack(from, to) {
  if (to < 0 || to >= playlistState.draftTracks.length || from === to) return
  const [track] = playlistState.draftTracks.splice(from, 1)
  playlistState.draftTracks.splice(to, 0, track)
  markTracksDirty()
}

function renderSearchResults() {
  const box = $('#searchResults')
  if (!playlistState.searchResults.length) return
  const existing = new Set(playlistState.draftTracks.map(track => track.id))
  box.innerHTML = '<div class="search-results">' + playlistState.searchResults.map((track, index) => {
    const added = existing.has(track.id)
    return `<div class="search-item" data-playing-id="${esc(track.id)}"><div><div class="song-title">${esc(track.name)}</div><div class="song-sub">${esc(track.singer)} · ${esc(track.album)} · ${esc(platName[track.source] || track.source)}</div></div><div class="search-item-actions"><button type="button" class="sec" data-play-playlist-search="${index}" ${track.unavailable ? 'disabled' : ''}>播放</button><button type="button" data-add-track="${esc(track.id)}" ${added ? 'disabled' : ''}>${added ? '已加入' : '加入草稿'}</button></div></div>`
  }).join('') + '</div>'
  renderPlaybackMarkers()
}

$('#refreshButton').addEventListener('click', async () => {
  if (!confirmDiscard()) return
  try {
    playlistState.tracksDirty = false
    playlistState.metaDirty = false
    if (!await loadPlaylists()) return
    if (playlistState.current && !await selectPlaylist(playlistState.current.id, true)) return
    toast('已刷新')
  } catch (error) { if (!isAbort(error)) toast(error.message, true) }
})

$('#ownerFilter').addEventListener('change', async () => {
  if (!confirmDiscard()) {
    $('#ownerFilter').value = ''
    return
  }
  playlistState.tracksDirty = false
  clearDetail()
  await loadPlaylists()
})

$('#playlistList').addEventListener('click', event => {
  const button = event.target.closest('[data-playlist-id]')
  if (button) selectPlaylist(button.dataset.playlistId).catch(error => { if (!isAbort(error)) toast(error.message, true) })
})

$('#createForm').addEventListener('submit', async event => {
  event.preventDefault()
  const createForm = event.currentTarget
  const form = new FormData(createForm)
  const body = { name: form.get('name'), comment: form.get('comment'), public: form.get('public') === 'on' }
  if (playlistState.me.isAdmin) body.ownerId = Number(form.get('ownerId'))
  try {
    const created = await playlistAPI('/playlists', { method: 'POST', body })
    createForm.reset()
    createForm.elements.public.checked = playlistState.defaultPublic
    if (playlistState.me.isAdmin) {
      createForm.elements.ownerId.value = String(playlistState.me.id)
      $('#ownerFilter').value = ''
    }
    createForm.closest('details').open = false
    await loadPlaylists()
    await selectPlaylist(created.id)
    toast('歌单已创建')
  } catch (error) { toast(error.message) }
})

$('#metaForm').addEventListener('input', () => {
  if (playlistState.current?.canEdit) { playlistState.metaDirty = true; playlistState.editVersion++ }
  renderCollectForm()
})

$('#metaForm').addEventListener('submit', async event => {
  event.preventDefault()
  if (!playlistState.current?.canEdit) return
  const form = new FormData(event.currentTarget), id = playlistState.current.id
  const body = { name: form.get('name'), comment: form.get('comment'), public: form.get('public') === 'on' }
  try {
    const updated = await playlistAPI('/playlists/' + encodeURIComponent(id), { method: 'PUT', body })
    invalidatePlaylistReads(id)
    if (playlistState.current?.id === id) {
      playlistState.current = { ...updated, tracks: playlistState.current.tracks, tracksRevision: playlistState.current.tracksRevision }
      const fields = $('#metaForm').elements
      playlistState.metaDirty = fields.name.value !== body.name || fields.comment.value !== body.comment || fields.public.checked !== body.public
      if (!playlistState.metaDirty) renderDetail()
      else renderCollectForm()
    }
    await loadPlaylists()
    toast('歌单信息已保存')
  } catch (error) { if (!isAbort(error)) toast(error.message, true) }
})

$('#deleteButton').addEventListener('click', async () => {
  if (!playlistState.current?.canEdit || !confirm(`确定删除歌单“${playlistState.current.name}”吗？此操作无法撤销。`)) return
  try {
    await playlistAPI('/playlists/' + encodeURIComponent(playlistState.current.id), { method: 'DELETE' })
    clearDetail()
    await loadPlaylists()
    toast('歌单已删除')
  } catch (error) { toast(error.message) }
})

$('#searchResults').addEventListener('click', event => {
  const play = event.target.closest('[data-play-playlist-search]')
  if (play && !play.disabled) { webPlayer.playList(playlistState.searchResults, Number(play.dataset.playPlaylistSearch)); return }
  const button = event.target.closest('[data-add-track]')
  if (!button || button.disabled) return
  const track = playlistState.searchResults.find(item => item.id === button.dataset.addTrack)
  if (!playlistState.current?.canEdit || !track || playlistState.draftTracks.some(item => item.id === track.id)) return
  if (playlistState.draftTracks.length >= 2000) return toast('单个歌单最多包含 2000 首歌曲', true)
  playlistState.draftTracks.unshift({ ...track })
  markTracksDirty()
  toast('已加入草稿，请保存歌曲变更')
})

$('#trackTable').addEventListener('click', event => {
  const button = event.target.closest('[data-track-action]')
  if (!button || button.disabled) return
  const row = button.closest('[data-track-index]')
  const index = Number(row.dataset.trackIndex)
  if (button.dataset.trackAction === 'play') { webPlayer.playList(playlistState.draftTracks, index); return }
  if (!playlistState.current?.canEdit) return
  switch (button.dataset.trackAction) {
    case 'up': moveTrack(index, index - 1); break
    case 'down': moveTrack(index, index + 1); break
    case 'remove': playlistState.draftTracks.splice(index, 1); markTracksDirty(); break
  }
})

$('#trackTable').addEventListener('dragstart', event => {
  if (event.target.closest('button')) { event.preventDefault(); return }
  const row = event.target.closest('[data-track-index]')
  if (!row || !playlistState.current?.canEdit) return
  playlistState.dragIndex = Number(row.dataset.trackIndex)
  row.classList.add('dragging')
  event.dataTransfer.effectAllowed = 'move'
})
$('#trackTable').addEventListener('dragover', event => {
  const row = event.target.closest('[data-track-index]')
  if (!row || playlistState.dragIndex < 0) return
  event.preventDefault()
  document.querySelectorAll('.drag-over').forEach(item => item.classList.remove('drag-over'))
  row.classList.add('drag-over')
})
$('#trackTable').addEventListener('drop', event => {
  const row = event.target.closest('[data-track-index]')
  if (!row || playlistState.dragIndex < 0) return
  event.preventDefault()
  const target = Number(row.dataset.trackIndex)
  moveTrack(playlistState.dragIndex, target)
  playlistState.dragIndex = -1
})
$('#trackTable').addEventListener('dragend', () => {
  playlistState.dragIndex = -1
  document.querySelectorAll('.dragging,.drag-over').forEach(item => item.classList.remove('dragging', 'drag-over'))
})

$('#saveTracksButton').addEventListener('click', async () => {
  if (!playlistState.current?.canEdit || !playlistState.tracksDirty) return
  try {
    const id = playlistState.current.id
    const draft = playlistState.draftTracks.map(track => track.id)
    const updated = await playlistAPI('/playlists/' + encodeURIComponent(id) + '/tracks', { method: 'PUT', body: { trackIds: draft, expectedRevision: playlistState.draftRevision } })
    invalidatePlaylistReads(id)
    if (playlistState.current?.id !== id) { await loadPlaylists(); return }
    playlistState.current = updated
    playlistState.draftRevision = updated.tracksRevision
    // 请求发出后继续编辑的草稿不能被旧响应覆盖。
    if (JSON.stringify(draft) === JSON.stringify(playlistState.draftTracks.map(track => track.id))) {
      playlistState.draftTracks = updated.tracks.map(track => ({ ...track }))
      playlistState.tracksDirty = false
    }
    $('#detailOwner').textContent = `${updated.owner} · 创建于 ${formatTime(updated.createdAt)} · 更新于 ${formatTime(updated.updatedAt)}`
    renderTracks()
    renderSearchResults()
    await loadPlaylists()
    toast('歌曲变更已保存')
  } catch (error) { if (!isAbort(error)) toast(error.message, true) }
})

// ---- 导入洛雪歌单 ----
function resetImportForm() {
  const form = $('#importForm')
  form.reset()
  form.elements.public.checked = playlistState.defaultPublic
  if (playlistState.me?.isAdmin) form.elements.ownerId.value = String(playlistState.me.id)
  $('#importLists').classList.add('hidden')
  $('#importLists').innerHTML = ''
  form.querySelector('button[type=submit]').disabled = true
  updateImportMode()
}

function updateImportMode() {
  const form = $('#importForm')
  const append = form.elements.mode.value === 'append'
  const canAppend = !!playlistState.current?.canEdit
  form.elements.mode.options[1].disabled = !canAppend
  form.elements.mode.options[1].textContent = canAppend ? `追加到「${playlistState.current.name}」` : '追加到当前选中的歌单'
  if (append && !canAppend) form.elements.mode.value = 'create'
  const creating = form.elements.mode.value === 'create'
  $('#importOwnerWrap').classList.toggle('hidden', !creating || !playlistState.me?.isAdmin)
  $('#importPublicWrap').classList.toggle('hidden', !creating)
}

$('#importForm').elements.file.addEventListener('change', async event => {
  const file = event.target.files[0]
  const box = $('#importLists')
  const submit = $('#importForm').querySelector('button[type=submit]')
  submit.disabled = true
  if (!file) return box.classList.add('hidden')
  box.classList.remove('hidden')
  box.innerHTML = '<p class="muted" style="padding:10px;margin:0">解析中…</p>'
  const body = new FormData()
  body.append('file', file)
  body.append('preview', '1')
  try {
    const result = await playlistAPI('/playlists/import', { method: 'POST', body })
    box.innerHTML = result.lists.map(item => `<label><input type="checkbox" name="lists" value="${item.index}" checked><span class="import-name">${esc(item.name)}</span><span class="import-meta">${item.source ? esc(platName[item.source] || item.source) + ' · ' : ''}${item.count} 首${item.skipped ? ` · 跳过 ${item.skipped}` : ''}</span></label>`).join('')
    submit.disabled = false
  } catch (error) {
    box.innerHTML = `<p class="err">${esc(error.message)}</p>`
  }
})

$('#importForm').elements.mode.addEventListener('change', updateImportMode)

$('#importForm').addEventListener('submit', async event => {
  event.preventDefault()
  const form = event.currentTarget
  const file = form.elements.file.files[0]
  if (!file) return toast('请先选择歌单文件')
  const selected = [...form.querySelectorAll('input[name=lists]:checked')].map(input => input.value)
  if (!selected.length) return toast('请至少勾选一个列表')
  const append = form.elements.mode.value === 'append'
  if (append && (!playlistState.current?.canEdit || !confirmDiscard())) return
  const body = new FormData()
  body.append('file', file)
  body.append('lists', selected.join(','))
  if (append) body.append('target', playlistState.current.id)
  else {
    body.append('public', form.elements.public.checked ? '1' : '0')
    if (playlistState.me.isAdmin) body.append('ownerId', form.elements.ownerId.value)
  }
  const submit = form.querySelector('button[type=submit]')
  submit.disabled = true
  submit.textContent = '导入中…'
  try {
    const result = await playlistAPI('/playlists/import', { method: 'POST', body })
    let message = append ? `已追加 ${result.added} 首歌曲` : `已导入 ${result.playlists.length} 个歌单，共 ${result.added} 首歌曲`
    if (result.skipped) message += `，跳过 ${result.skipped} 首本地或无效歌曲`
    if (result.truncated) message += `，超出上限截断 ${result.truncated} 首`
    resetImportForm()
    form.closest('details').open = false
    playlistState.tracksDirty = false
    playlistState.metaDirty = false
    if (playlistState.me.isAdmin) $('#ownerFilter').value = ''
    await loadPlaylists()
    if (result.playlists.length) await selectPlaylist(result.playlists[0].id, true)
    toast(message)
  } catch (error) { toast(error.message) } finally {
    submit.textContent = '导入'
    submit.disabled = !form.elements.file.files[0]
  }
})

window.addEventListener('beforeunload', event => {
  if (!playlistState.tracksDirty && !playlistState.metaDirty) return
  event.preventDefault()
  event.returnValue = ''
})
// 登录初始化由最后加载的 music-ui.js 发起，确保音乐控件与会话清理已就绪。
