// lxsc 统一控制台脚本（无框架）
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

const fmtDur = s => s < 60 ? Math.floor(s) + ' 秒' : s < 3600 ? Math.floor(s / 60) + ' 分钟' : s < 86400 ? (s / 3600).toFixed(1) + ' 小时' : (s / 86400).toFixed(1) + ' 天'
const formatBytes = n => !n ? '0 B' : n < 1024 ? n + ' B' : n < 1048576 ? (n / 1024).toFixed(1) + ' KB' : n < 1073741824 ? (n / 1048576).toFixed(1) + ' MB' : (n / 1073741824).toFixed(2) + ' GB'

function formatTime(value) {
  if (!value) return ''
  return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'short', timeStyle: 'short' }).format(new Date(value * 1000))
}


$('#copyServerURL').addEventListener('click', () => copyText($('#serverUrl').textContent))
$('#cancelEditUser').addEventListener('click', () => $('#userDialog').close())
$('#copyAPIKey').addEventListener('click', () => copyText($('#keyValue').value))
$('#closeAPIKey').addEventListener('click', () => { clearAPIKeyDialog(); $('#keyDialog').close() })
