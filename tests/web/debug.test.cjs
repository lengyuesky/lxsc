const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')
const source = fs.readFileSync(require.resolve('../../internal/assets/web/debug.js'), 'utf8')
function fixture() {
  const elements = new Map(), calls = [], copied = [], notices = [], listeners = {}
  const $ = selector => {
    if (!elements.has(selector)) {
      const classes = new Set(['hidden'])
      elements.set(selector, { value: '', innerHTML: '', checked: false, disabled: false, focused: false, selected: false,
        classList: { add: v => classes.add(v), remove: v => classes.delete(v), toggle: (v, enabled) => enabled ? classes.add(v) : classes.delete(v), contains: v => classes.has(v) },
        addEventListener(name, fn) { this[name] = fn }, focus() { this.focused = true }, select() { this.selected = true },
      })
    }
    return elements.get(selector)
  }
  $('#debugTTL').value = '900'
  const item = { id: 'safe-id', scopes: ['read'], createdAt: '2026-09-09T00:00:00Z', expiresAt: '2099-09-09T00:15:00Z', lastUsedAt: null, calls: 0, revoked: false }
  const scope = { $, document: { querySelectorAll: () => [] }, window: { addEventListener: (name, fn) => { listeners[name] = fn } },
    location: { protocol: 'https:', origin: 'https://test.example', hash: '#debug' }, sessionState: { me: { isAdmin: true } },
    navigator: { clipboard: { writeText: async value => { copied.push(value) } } }, isAbort: () => false, toast: (...args) => notices.push(args), esc: String,
    adminAPI: async (path, opts) => { calls.push({ path, opts }); if (opts.method === 'POST') return { token: 'SYNTHETIC_ONE_TIME_KEY', credential: { ...item, scopes: opts.body.scopes } }; if (opts.method === 'DELETE') item.revoked = true; return { tokens: [item] } },
  }
  vm.runInNewContext(source + '\nglobalThis.ui = { createDebugToken, loadDebug, revokeDebugToken, copyDebugValue, clearDebugSecret, resetDebugSession }', scope)
  return { $, scope, calls, copied, notices, listeners, item, ui: scope.ui }
}
const submit = { preventDefault() {} }
test('限时调试默认只读15分钟，主动探测必须勾选，变更均带非简单请求头', async () => {
  const f = fixture()
  await f.ui.createDebugToken(submit)
  assert.equal(f.calls[0].path, '/debug-tokens')
  assert.equal(f.calls[0].opts.body.ttlSeconds, 900)
  assert.deepEqual(Array.from(f.calls[0].opts.body.scopes), ['read'])
  assert.equal(f.calls[0].opts.headers['X-LXSC-Debug-Management'], '1')
  assert.equal(f.$('#debugTokenValue').value, 'SYNTHETIC_ONE_TIME_KEY')
  assert.equal(f.$('#debugSecret').classList.contains('hidden'), false)
  assert.ok(!f.$('#debugTokenList').innerHTML.includes('SYNTHETIC_ONE_TIME_KEY'))
  f.$('#debugProbeScope').checked = true
  f.$('#debugTTL').value = '86400'
  await f.ui.createDebugToken(submit)
  const create = f.calls.filter(v => v.opts.method === 'POST').at(-1)
  assert.deepEqual(Array.from(create.opts.body.scopes), ['read', 'probe'])
  assert.equal(create.opts.body.ttlSeconds, 86400)
  const share = f.$('#debugShareValue').value
  for (const part of ['https://test.example', 'Bearer SYNTHETIC_ONE_TIME_KEY', f.item.expiresAt, 'read, probe', '/api/debug/status', '/api/debug/events', '/api/debug/probe', '少量流量', '到期不可续期']) assert.ok(share.includes(part), part)
})
test('复制密钥和分享说明，剪贴板失败可手工选择，撤销清除展示', async () => {
  const f = fixture()
  await f.ui.createDebugToken(submit)
  await f.ui.copyDebugValue('#debugTokenValue')
  await f.ui.copyDebugValue('#debugShareValue')
  assert.equal(f.copied[0], 'SYNTHETIC_ONE_TIME_KEY')
  assert.match(f.copied[1], /Bearer SYNTHETIC_ONE_TIME_KEY/)
  f.scope.navigator.clipboard.writeText = async () => { throw new Error('denied') }
  await f.ui.copyDebugValue('#debugShareValue')
  assert.equal(f.$('#debugShareValue').selected, true)
  assert.equal(f.$('#debugShareValue').focused, true)
  await f.ui.revokeDebugToken('safe-id')
  const revoke = f.calls.find(v => v.opts.method === 'DELETE')
  assert.equal(revoke.opts.headers['X-LXSC-Debug-Management'], '1')
  assert.equal(f.$('#debugTokenValue').value, '')
  assert.equal(f.$('#debugShareValue').value, '')
  assert.match(f.$('#debugTokenList').innerHTML, /已撤销/)
})
test('离开/退出清除且迟到创建响应不能重新展示，普通用户不创建或列举', async () => {
  const f = fixture()
  let resolve
  f.scope.adminAPI = () => new Promise(done => { resolve = done })
  const pending = f.ui.createDebugToken(submit)
  f.ui.clearDebugSecret()
  f.scope.location.hash = '#search'
  resolve({ token: 'LATE_SECRET', credential: f.item })
  await pending
  assert.equal(f.$('#debugTokenValue').value, '')
  assert.equal(f.$('#debugSecret').classList.contains('hidden'), true)
  f.$('#debugTokenValue').value = 'secret'
  f.listeners.pagehide()
  assert.equal(f.$('#debugTokenValue').value, '')
  f.scope.sessionState.me = { isAdmin: false }
  await f.ui.createDebugToken(submit)
  await f.ui.loadDebug()
  assert.equal(f.calls.length, 0)
})
test('页面集成管理员路由/退出清理且不保存到存储或URL', () => {
  const app = fs.readFileSync(require.resolve('../../internal/assets/web/app.js'), 'utf8')
  const html = fs.readFileSync(require.resolve('../../internal/assets/web/index.html'), 'utf8')
  assert.match(app, /debug: \(\) => loadDebug\(\)/)
  assert.match(app, /if \(tab !== 'debug'\) clearDebugSecret\(\)/)
  assert.match(app, /function showLogin\(\) \{\s+resetDebugSession\(\)/)
  assert.match(html, /data-tab="debug" class="admin-only hidden"/)
  assert.match(html, /debugTokenValue" readonly autocomplete="off"/)
  assert.doesNotMatch(source, /localStorage|sessionStorage|history\.|location\.(?:hash|href)\s*=/)
})
