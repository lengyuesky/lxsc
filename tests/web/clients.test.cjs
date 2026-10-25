const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')
const source = fs.readFileSync(require.resolve('../../internal/assets/web/clients.js'), 'utf8')

function fixture(address = 'https://music.example:27880') {
  const elements = new Map(), copied = [], notices = []
  const $ = selector => {
    if (!elements.has(selector)) {
      const classes = new Set()
      elements.set(selector, { value: '', textContent: '', focused: false, selected: false,
        classList: { add: name => classes.add(name), toggle: (name, enabled) => enabled ? classes.add(name) : classes.delete(name), contains: name => classes.has(name) },
        focus() { this.focused = true }, select() { this.selected = true },
        addEventListener(name, listener) { this['on' + name] = listener },
      })
    }
    return elements.get(selector)
  }
  const scope = { $, location: new URL(address), sessionState: { me: { id: 7, name: '普通用户 <测试>', isAdmin: false }, epoch: 1 },
    navigator: { clipboard: { writeText: async value => { copied.push(value) } } }, toast: (...args) => notices.push(args),
  }
  vm.runInNewContext(source + '\nglobalThis.ui = LXSCClients', scope)
  return { $, scope, ui: scope.ui, copied, notices }
}

test('客户端说明使用浏览器实际地址及本人账号，保留协议、非默认端口与 IPv6', () => {
  for (const address of ['https://music.example:27880', 'http://[2001:db8::1]:8080', 'https://xn--fsqu00a.example']) {
    const f = fixture(address + '/?private=should-not-copy#clients')
    f.ui.load()
    assert.equal(f.$('#clientServerURL').value, new URL(address).origin)
    assert.equal(f.$('#clientUsername').value, '普通用户 <测试>')
    assert.doesNotMatch(f.$('#clientServerURL').value, /private|clients|rest|api/)
  }
})

test('连接提示区分 HTTPS、HTTP 和本机地址', () => {
  const safe = fixture()
  safe.ui.load()
  assert.equal(safe.$('#clientNetworkNotice').classList.contains('hidden'), true)
  assert.equal(safe.$('#clientNetworkNotice').textContent, '')
  for (const host of ['localhost', '127.0.0.1', '127.1.2.3', '[::1]', '0.0.0.0', '[::]']) {
    const f = fixture('http://' + host + ':27880')
    f.ui.load()
    assert.equal(f.$('#clientNetworkNotice').classList.contains('hidden'), false)
    assert.match(f.$('#clientNetworkNotice').textContent, /HTTP.*HTTPS/)
    assert.match(f.$('#clientNetworkNotice').textContent, /仅供本机访问/)
  }
  const lan = fixture('http://192.168.1.2:27880')
  lan.ui.load()
  assert.match(lan.$('#clientNetworkNotice').textContent, /HTTPS/)
  assert.doesNotMatch(lan.$('#clientNetworkNotice').textContent, /仅供本机访问/)
})

test('复制只允许服务器地址和本人用户名，失败或无剪贴板 API 时选中文本', async () => {
  const f = fixture()
  f.ui.load()
  await f.ui.copy('#clientServerURL')
  await f.ui.copy('#clientUsername')
  await f.ui.copy('#keyValue')
  assert.deepEqual(f.copied, ['https://music.example:27880', '普通用户 <测试>'])
  assert.deepEqual(f.notices, [['已复制'], ['已复制']])
  f.scope.navigator.clipboard.writeText = async () => { throw new Error('denied') }
  await f.ui.copy('#clientServerURL')
  assert.equal(f.$('#clientServerURL').focused, true)
  assert.equal(f.$('#clientServerURL').selected, true)
  delete f.scope.navigator.clipboard
  await f.ui.copy('#clientUsername')
  assert.equal(f.$('#clientUsername').selected, true)
  assert.match(f.notices.at(-1)[0], /手动复制/)
  assert.equal(f.notices.at(-1)[1], true)
})

test('退出及重新登录清空旧账号，迟到剪贴板响应不改变新会话', async () => {
  for (const fail of [false, true]) {
    const f = fixture()
    f.ui.load()
    let finish
    f.scope.navigator.clipboard.writeText = () => new Promise((resolve, reject) => { finish = () => fail ? reject(new Error('late')) : resolve() })
    const pending = f.ui.copy('#clientUsername')
    f.ui.reset()
    f.scope.sessionState.me = null
    f.scope.sessionState.epoch++
    assert.equal(f.$('#clientServerURL').value, '')
    assert.equal(f.$('#clientUsername').value, '')
    assert.equal(f.$('#clientNetworkNotice').textContent, '')
    f.ui.load()
    await f.ui.copy('#clientUsername')
    f.scope.sessionState.me = { id: 8, name: '新用户', isAdmin: true }
    f.ui.load()
    finish()
    await pending
    assert.equal(f.$('#clientUsername').value, '新用户')
    assert.equal(f.$('#clientUsername').focused, false)
    assert.deepEqual(f.notices, [])
  }
})

test('客户端说明集成公共路由、版本化资源和会话清理，不读取凭据或存储', () => {
  const app = fs.readFileSync(require.resolve('../../internal/assets/web/app.js'), 'utf8')
  const html = fs.readFileSync(require.resolve('../../internal/assets/web/index.html'), 'utf8')
  assert.match(app, /clients: LXSCClients\.load/)
  assert.match(app, /\['playlists', 'search', 'listening', 'clients'\]\.includes\(tab\)/)
  assert.equal((app.match(/LXSCClients\.reset\(\)/g) || []).length, 2)
  assert.match(html, /href="#clients" data-tab="clients">/)
  assert.match(html, /id="tab-clients" class="tab client-guide"/)
  assert.match(html, /href="clients\.css"/)
  assert(html.indexOf('src="ui-common.js"') < html.indexOf('src="clients.js"'))
  assert(html.indexOf('src="clients.js"') < html.indexOf('src="app.js"'))
  assert.doesNotMatch(source, /localStorage|sessionStorage|fetch\(|(?:auth|admin|playlist)API\(|innerHTML|password|apiKey/)
  const guide = html.split('id="tab-clients"')[1].split('<section id="tab-dashboard"')[0]
  assert.doesNotMatch(guide, /type="password"|二维码|<canvas|<img/)
  assert.match(guide, /id="clientAPIHelp" class="card client-key-help admin-only hidden"/)
  assert.match(guide, /<p class="admin-only hidden">设备 API Key/)
  for (const client of ['箭头音乐', '音流', 'Feishin', 'Symfonium']) assert(guide.includes(client))
  for (const link of guide.matchAll(/<a[^>]+target="_blank"[^>]*>/g)) assert.match(link[0], /rel="noopener noreferrer"/)
})
