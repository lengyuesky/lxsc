// 备份表单状态仅在本模块内保存，退出时清空敏感输入。
const LXSCBackups = (() => {
// ---- 备份 ----
let webdavConfig = null
async function loadBackups() {
  const [status, config] = await Promise.all([adminAPI('/backups/status'), adminAPI('/backups/webdav/config')])
  webdavConfig = config
  renderPendingRestore(status)
  fillWebDAVConfig(config)
  renderBackupLast(status.last || {})
  $('#backupScheduleStatus').textContent = `最近成功上传：${status.lastSuccessAt ? formatTime(status.lastSuccessAt) : '暂无'}${status.last?.error && status.nextRetryAt ? ' · 下次重试不早于：' + formatTime(status.nextRetryAt) : ''}`
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
  box.innerHTML = `<div class="card pending-restore"><div><span class="badge warn">等待重启</span><h4>完整备份已经校验并暂存</h4><p>来源：${esc(p.source)} · 备份时间：${esc(formatTime(p.backupAt))} · 版本：${esc(p.sourceVersion)}</p><p class="muted hint">请执行 <code>docker compose restart</code>，重启后将替换当前数据。当前运行数据尚未改变。</p></div><button class="danger sm" data-backup-cancel>取消恢复</button></div>`
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
const previewGate = new LXSCMusic.RequestGate()
function currentBackupSignature() {
  const form = $('#importBackupForm'), file = form.elements.file.files[0]
  return file ? `${file.name}|${file.size}|${file.lastModified}|${form.elements.password.value}` : ''
}
function resetBackupPreview() {
	previewGate.cancel()
  backupPreviewSignature = ''
  const form = $('#importBackupForm')
  form.elements.confirm.checked = false
  form.elements.confirm.disabled = true
  form.querySelector('button[type="submit"]').disabled = true
  $('#backupPreview').className = 'backup-preview muted'
  $('#backupPreview').textContent = '尚未检查备份'
  $('#inspectBackupButton').disabled = false
  $('#inspectBackupButton').textContent = '检查备份'
}
$('#importBackupForm').elements.file.addEventListener('change', resetBackupPreview)
$('#importBackupForm').elements.password.addEventListener('input', resetBackupPreview)
async function previewBackup() {
  const form = $('#importBackupForm'), file = form.elements.file.files[0]
  if (!file) { toast('请先选择备份文件', true); return }
  resetBackupPreview()
  const task = previewGate.begin(), signature = currentBackupSignature()
  const button = form.querySelector('button[type="button"]'); button.disabled = true; button.textContent = '正在检查…'
  try {
    const data = new FormData(); data.append('file', file); data.append('password', form.elements.password.value)
    const result = await adminAPI('/backups/inspect', { method: 'POST', body: data, signal: task.signal })
    if (!task.current() || signature !== currentBackupSignature()) return
    const m = result.manifest || {}, stats = m.stats || {}
    $('#backupPreview').className = 'backup-preview ok'
    $('#backupPreview').innerHTML = `<b>备份有效</b><span>创建于 ${esc(formatTime(m.createdAt))} · lxsc ${esc(m.appVersion || '-')} · ${m.encrypted ? '已加密' : '未加密'}</span><span>${stats.users || 0} 个用户 · ${stats.playlists || 0} 个歌单 · ${stats.tracks || 0} 首歌曲元数据</span>`
    backupPreviewSignature = signature
    form.elements.confirm.disabled = false
    form.querySelector('button[type="submit"]').disabled = false
  } catch (error) { if (task.current()) { resetBackupPreview(); if (!isAbort(error)) toast(error.message, true) } }
  finally { if (task.current()) { button.disabled = false; button.textContent = '检查备份' } }
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
    tbody.innerHTML = files.length ? files.map(file => `<tr><td><b>${esc(file.name)}</b></td><td>${esc(formatTime(file.lastModified))}</td><td>${formatBytes(file.size)}</td><td class="actions"><a class="btn sec sm" href="/api/admin/backups/webdav/files/${encodeURIComponent(file.name)}">下载</a><button class="sec sm" data-backup-action="restore" data-backup-name="${esc(file.name)}">恢复</button><button class="danger sm" data-backup-action="delete" data-backup-name="${esc(file.name)}">删除</button></td></tr>`).join('') : '<tr><td colspan="4">远程目录暂无备份</td></tr>'
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


function reset() {
  webdavConfig = null
  resetBackupPreview()
  $('#exportBackupForm').reset()
  $('#importBackupForm').reset()
  $('#webdavForm').reset()
  $('#pendingRestore').replaceChildren()
  $('#remoteBackupTable tbody').replaceChildren()
}
$('#pendingRestore').addEventListener('click', event => { if (event.target.closest('[data-backup-cancel]')) void cancelPendingRestore() })
$('#remoteBackupTable').addEventListener('click', event => {
  const button = event.target.closest('[data-backup-action]')
  if (!button) return
  if (button.dataset.backupAction === 'restore') void restoreRemoteBackup(button.dataset.backupName)
  if (button.dataset.backupAction === 'delete') void deleteRemoteBackup(button.dataset.backupName)
})
$('#refreshBackups').addEventListener('click', () => loadBackups().catch(error => toast(error.message, true)))
$('#exportBackupForm').addEventListener('submit', exportBackup)
$('#importBackupForm').addEventListener('submit', importBackup)
$('#inspectBackupButton').addEventListener('click', previewBackup)
$('#webdavForm').addEventListener('submit', saveWebDAV)
$('#testWebDAVButton').addEventListener('click', testWebDAV)
$('#runWebDAVButton').addEventListener('click', runWebDAVBackup)

return { load: loadBackups, reset }
})()
