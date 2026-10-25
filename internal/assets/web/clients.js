// 说明页只展示当前地址与本人用户名，不读取密码、密钥，也不发起额外 API 请求。
const LXSCClients = (() => {
  const fields = ['#clientServerURL', '#clientUsername']
  function reset() {
    for (const selector of fields) $(selector).value = ''
    $('#clientNetworkNotice').textContent = ''
    $('#clientNetworkNotice').classList.add('hidden')
  }
  function load() {
    if (!sessionState.me) return reset()
    $('#clientServerURL').value = location.origin
    $('#clientUsername').value = sessionState.me.name
    const notices = []
    if (location.protocol !== 'https:') notices.push('当前使用 HTTP，公网连接请优先配置 HTTPS，避免账号密码在传输中泄露。')
    if (/^(?:localhost|127(?:\.\d{1,3}){3}|0\.0\.0\.0|\[::1\]|\[::\])$/i.test(location.hostname)) notices.push('当前地址仅供本机访问，不能直接用于另一台手机或电脑；请在客户端填写服务器的局域网 IP、公网域名或 VPN 地址。')
    $('#clientNetworkNotice').textContent = notices.join(' ')
    $('#clientNetworkNotice').classList.toggle('hidden', !notices.length)
  }
  async function copy(selector) {
    if (!sessionState.me || !fields.includes(selector)) return
    const field = $(selector), value = field.value, epoch = sessionState.epoch
    if (!value) return
    const current = () => !!sessionState.me && epoch === sessionState.epoch
    try {
      await navigator.clipboard.writeText(value)
      if (current()) toast('已复制')
    } catch {
      if (!current()) return
      field.focus()
      field.select()
      toast('复制失败，已选中内容，请手动复制', true)
    }
  }
  $('#copyClientServerURL').addEventListener('click', () => { void copy('#clientServerURL') })
  $('#copyClientUsername').addEventListener('click', () => { void copy('#clientUsername') })
  return { load, reset, copy }
})()
