const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')
const apiSource = fs.readFileSync(require.resolve('../../internal/assets/web/api.js'), 'utf8')
function fixture(fetch) {
  const scope = { document: {}, AbortController, DOMException, FormData, fetch, showLogin() { scope.loggedOut = true } }
  vm.runInNewContext(apiSource + '\nglobalThis.api = { requestAPI, sessionState }', scope)
  return scope
}
test('统一请求只发送同源会话，并在结束后释放取消控制器', async () => {
  const f = fixture(async (url, options) => {
    assert.equal(url, '/api/app/profile')
    assert.equal(options.credentials, 'same-origin')
    assert.equal(options.headers['Content-Type'], 'application/json')
    assert.equal(options.body, '{"quality":"320k"}')
    return { ok: true, status: 200, json: async () => ({ ok: true }) }
  })
  await f.api.requestAPI('/api/app', '/profile', { method: 'PUT', body: { quality: '320k' } })
  assert.equal(f.api.sessionState.requests.size, 0)
})
test('退出后的迟到响应不能更新页面，会话失效仍回到登录页', async () => {
  let resolve
  const f = fixture(() => new Promise(done => { resolve = done }))
  const pending = f.api.requestAPI('/api/app', '/playlists')
  f.api.sessionState.epoch++
  resolve({ ok: true, status: 200, json: async () => ({ secret: '旧账号数据' }) })
  await assert.rejects(pending, { name: 'AbortError' })
  f.fetch = async () => ({ ok: false, status: 401, json: async () => ({ error: '未登录' }) })
  await assert.rejects(f.api.requestAPI('/api/app', '/playlists'), /未登录/)
  assert.equal(f.loggedOut, true)
})
test('拆分脚本顺序满足全局依赖，维护和用户按钮不拼接内联事件', () => {
  const html = fs.readFileSync(require.resolve('../../internal/assets/web/index.html'), 'utf8')
  for (const name of ['api.js', 'users.js']) assert(html.indexOf(`src="${name}"`) < html.indexOf('src="app.js"'))
  const users = fs.readFileSync(require.resolve('../../internal/assets/web/users.js'), 'utf8')
  assert.doesNotMatch(users, /onclick=/)
  assert.match(users, /data-user-action/)
  assert.match(html, /id="compactMetadataButton"/)
  assert.doesNotMatch(html, /onclick="cleanupMetadata/)
})
