// 临时凭据仅保留在当前页面 DOM/内存，不写入浏览器存储或 URL。
const debugState = { epoch: 0, busy: false, id: '' }
const debugPresets = {
  full: ['read', 'probe', 'inspect', 'maintain'],
  diagnose: ['read', 'probe', 'inspect'],
  read: ['read'],
}
function clearDebugSecret() {
  debugState.epoch++
  debugState.id = ''
  $('#debugTokenValue').value = ''
  $('#debugShareValue').value = ''
  $('#debugSecret').classList.add('hidden')
}
function resetDebugSession() {
  clearDebugSecret()
  debugState.busy = false
  $('#debugCreateButton').disabled = false
  $('#debugTokenList').innerHTML = ''
}
const debugManagement = (path, opts = {}) => adminAPI('/debug-tokens' + path, { ...opts, headers: { 'X-LXSC-Debug-Management': '1' } })
async function loadDebug() {
  if (!sessionState.me?.isAdmin) { resetDebugSession(); return }
  const epoch = debugState.epoch
  $('#debugHTTPSWarning').classList.toggle('hidden', location.protocol === 'https:')
  const data = await debugManagement('')
  if (epoch !== debugState.epoch || !sessionState.me?.isAdmin || location.hash !== '#debug') return
  $('#debugTokenList').innerHTML = data.tokens.length ? data.tokens.map(item => {
    const expired = Date.parse(item.expiresAt) <= Date.now()
    const status = item.revoked ? '已撤销' : expired ? '已过期' : '有效'
    return `<div class="card"><b>${esc(item.id)}</b> · ${esc(status)}<p>权限：${esc(item.scopes.join(', '))} · 调用：${esc(item.calls)}</p><p>创建：${esc(new Date(item.createdAt).toLocaleString())}<br>绝对到期：${esc(new Date(item.expiresAt).toLocaleString())}<br>最近使用：${item.lastUsedAt ? esc(new Date(item.lastUsedAt).toLocaleString()) : '尚未使用'}</p><button class="danger sm" data-debug-revoke="${esc(item.id)}" ${item.revoked || expired ? 'disabled' : ''}>撤销</button></div>`
  }).join('') : '<p class="muted">暂无临时凭据</p>'
  document.querySelectorAll('[data-debug-revoke]').forEach(button => { button.onclick = () => revokeDebugToken(button.dataset.debugRevoke) })
}
async function createDebugToken(event) {
  event.preventDefault()
  if (!sessionState.me?.isAdmin || debugState.busy) return
  clearDebugSecret()
  const epoch = debugState.epoch
  debugState.busy = true
  $('#debugCreateButton').disabled = true
  const scopes = [...(debugPresets[$('#debugPreset').value] || debugPresets.read)]
  try {
    const data = await debugManagement('', { method: 'POST', body: { ttlSeconds: Number($('#debugTTL').value), scopes } })
    if (epoch !== debugState.epoch || !sessionState.me?.isAdmin || location.hash !== '#debug') return
    debugState.id = data.credential.id
    $('#debugTokenValue').value = data.token
    const granted = data.credential.scopes
    const details = [
      '仅供管理员授权的受信任排查者，不要公开。',
      `部署地址：${location.origin}`, `临时凭据：Bearer ${data.token}`,
      `绝对到期：${data.credential.expiresAt}`, `权限：${granted.join(', ')}`,
      `API：${location.origin}/api/debug/status`, `AI 排查指南：${location.origin}/api/debug/capabilities`,
      `事件：${location.origin}/api/debug/events`,
    ]
    if (granted.includes('probe')) details.push(`主动探测：POST ${location.origin}/api/debug/probe（trackId + quality）`, `榜单探测：POST ${location.origin}/api/debug/probe/board（boardId）`, `歌词探测：POST ${location.origin}/api/debug/probe/lyrics（trackId）`, '主动探测会产生少量流量及缓存副作用。')
    if (granted.includes('inspect')) details.push(`完整诊断：GET ${location.origin}/api/debug/inspect/runtime、performance、settings、sources、logs`, `协议探测：POST ${location.origin}/api/debug/probe/protocol，例如 {"endpoint":"getPlaylists","method":"GET","format":"json","params":{}}`, `播放预检：POST ${location.origin}/api/debug/probe/playback，例如 {"trackId":"从事件获取的 tr-平台-ID","method":"HEAD","quality":"320k"}`, '可用 userId 模拟现有用户，不传播放器密码。GET/HEAD 播放探测只取响应头，不转发音频或记为播放。完整诊断可能包含用户/歌曲信息与处理过的自由文本日志，仅私下使用。')
    if (granted.includes('maintain')) details.push(`维护：POST ${location.origin}/api/debug/maintenance，支持 cache_clear（target=urls/metadata/all）、source_reload（sourceId）、settings_update（settings 对象）。`, '维护会修改共享服务状态并记录审计；撤销凭据不会回滚已经完成的操作。')
    details.push('先读取 AI 排查指南，按 grantedScopes 选择接口；对比 endpoint、method、client、format、result、count、lines、protocolCode，并结合 requestId、bytesWritten、responseBytes 核对响应写出情况。protocol_probe/playback_probe 是主动探测，不是客户端请求。', '仅接受 Authorization 请求头；只能调用 /api/debug，不能用于普通管理接口、播放器登录或实际播放。管理员降权、删除或改密会立即失效。仅服务端视角，不证明客户端能播。到期不可续期，需重建；重启全部失效，用完请撤销。公网务必 HTTPS。')
    $('#debugShareValue').value = details.join('\n')
    $('#debugSecret').classList.remove('hidden')
    await loadDebug()
  } catch (error) { if (!isAbort(error)) toast(error.message, true) }
  finally { debugState.busy = false; $('#debugCreateButton').disabled = false }
}
async function revokeDebugToken(id) {
  if (!sessionState.me?.isAdmin) return
  try {
    await debugManagement('/' + encodeURIComponent(id), { method: 'DELETE' })
    if (debugState.id === id) clearDebugSecret()
    await loadDebug()
    toast('已撤销，相关在途探测已取消')
  } catch (error) { if (!isAbort(error)) toast(error.message, true) }
}
async function copyDebugValue(selector) {
  const field = $(selector)
  if (!sessionState.me?.isAdmin || !field.value) return
  // 无剪贴板权限时原生只读文本框仍可手工选择复制。
  try { await navigator.clipboard.writeText(field.value); toast('已复制，请仅私下分享') }
  catch { field.focus(); field.select(); toast('复制失败，请在文本框中手动复制', true) }
}
$('#debugCreateForm').addEventListener('submit', createDebugToken)
$('#debugCopyToken').addEventListener('click', () => copyDebugValue('#debugTokenValue'))
$('#debugCopyShare').addEventListener('click', () => copyDebugValue('#debugShareValue'))
$('#debugClear').addEventListener('click', clearDebugSecret)
$('#debugRefresh').addEventListener('click', () => loadDebug().catch(error => { if (!isAbort(error)) toast(error.message, true) }))
window.addEventListener('pagehide', resetDebugSession)
