// 临时凭据仅保留在当前页面 DOM/内存，不写入浏览器存储或 URL。
const debugState = { epoch: 0, busy: false, id: '' }
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
  const scopes = $('#debugProbeScope').checked ? ['read', 'probe'] : ['read']
  try {
    const data = await debugManagement('', { method: 'POST', body: { ttlSeconds: Number($('#debugTTL').value), scopes } })
    if (epoch !== debugState.epoch || !sessionState.me?.isAdmin || location.hash !== '#debug') return
    debugState.id = data.credential.id
    $('#debugTokenValue').value = data.token
    $('#debugShareValue').value = `仅分享给受信任的排查者，不要公开。\n部署地址：${location.origin}\n临时凭据：Bearer ${data.token}\n绝对到期：${data.credential.expiresAt}\n权限：${data.credential.scopes.join(', ')}\nAPI：${location.origin}/api/debug/status\n事件：${location.origin}/api/debug/events\n${scopes.includes('probe') ? `主动探测：POST ${location.origin}/api/debug/probe（会向音源发送请求，产生少量流量及链接缓存副作用）\n` : ''}只允许 Authorization 请求头，不能用于管理或播放。仅服务端视角，不证明客户端能播。到期不可续期，需重建；用完请撤销。公网务必 HTTPS。`
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
