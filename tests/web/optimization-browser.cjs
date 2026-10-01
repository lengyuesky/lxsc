// 优化回归只使用隔离服务与拦截响应，不访问远程备份或真实音源。
const assert = require('node:assert/strict')
module.exports = async ({ check, url, login }) => {
  await check('管理模块按需加载，退出后的迟到模块不请求旧账号数据', async page => {
    assert.equal(await page.evaluate(() => performance.getEntriesByType('resource').filter(r => /\/(admin-pages|backups)\.js/.test(r.name)).length), 0)
    let release, started, scriptRequests = 0, sourceRequests = 0
    const ready = new Promise(resolve => { started = resolve }), pending = new Promise(resolve => { release = resolve })
    await page.route('**/admin-pages.js?*', async route => {
      scriptRequests++; started(); await pending; await route.continue()
    })
    page.on('request', request => { if (new URL(request.url()).pathname === '/api/admin/sources') sourceRequests++ })
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, 'admin')
    await page.locator('nav [data-tab=sources]').click()
    await ready
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    release()
    await page.waitForFunction(() => !document.querySelector('script[src*="admin-pages.js"]'))
    assert.equal(sourceRequests, 0)
    await login(page, url, 'admin')
    const response = page.waitForResponse(r => new URL(r.url()).pathname === '/api/admin/sources')
    await page.locator('nav [data-tab=sources]').click()
    await response
    assert.equal(scriptRequests, 1, '重新登录复用模块，事件只绑定一次')
  })
  await check('管理模块网络失败后可重试', async page => {
    let attempts = 0
    await page.route('**/admin-pages.js?*', async route => {
      if (++attempts === 1) await route.abort('failed')
      else await route.continue()
    })
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, 'admin')
    await page.locator('nav [data-tab=sources]').click()
    await page.locator('#toast').filter({ hasText: '管理页面加载失败' }).waitFor()
    await page.locator('nav [data-tab=search]').click()
    const response = page.waitForResponse(r => new URL(r.url()).pathname === '/api/admin/sources')
    await page.locator('nav [data-tab=sources]').click()
    await response
    assert.equal(attempts, 2)
  })
  await check('2000首歌单排序只更新受影响行并保留重复项和焦点', async page => {
    await page.locator('nav [data-tab=playlists]').click()
    await page.locator('#tab-playlists.active').waitFor()
    const result = await page.evaluate(() => {
      playlistState.current = { id: 'local-render-test', canEdit: true }
      playlistState.draftTracks = Array.from({ length: 2000 }, (_, i) => ({ id: 'tr-wy-' + (i % 317), name: '测试' + i, source: 'wy' }))
      renderTracks()
      document.querySelector('#detail').classList.remove('hidden')
      const body = document.querySelector('#trackTable'), original = Array.from(body.children)
      const focused = original[999].querySelector('[data-track-action=up]')
      focused.focus({ preventScroll: true })
      const observer = new MutationObserver(() => {})
      observer.observe(body, { subtree: true, childList: true, attributes: true, characterData: true })
      moveTrack(999, 1000)
      const focusRetained = document.activeElement === focused
      const mutations = observer.takeRecords().length
      observer.disconnect()
      const local = body.children[1000] === original[999] && body.children[999] === original[1000] && body.children[998] === original[998]
      moveTrack(0, 1999)
      const endpoints = body.children[1999] === original[0] && body.children[0].querySelector('[data-track-action=up]').disabled && body.children[1999].querySelector('[data-track-action=down]').disabled
      const order = Array.from(body.children).every((row, i) => row.dataset.trackIndex === String(i) && row.dataset.playingId === playlistState.draftTracks[i].id)
      playlistState.tracksDirty = false
      return { local, endpoints, order, focusRetained, mutations, count: body.children.length }
    })
    assert.equal(result.local, true); assert.equal(result.endpoints, true); assert.equal(result.order, true)
    assert.equal(result.count, 2000)
    assert.equal(result.focusRetained, true)
    assert(result.mutations < 30, '相邻移动不应刷新整张表')
  })

  await check('CSP 授权静态脚本并阻止内联脚本和事件', async page => {
    const response = await page.request.get(url)
    const policy = response.headers()['content-security-policy']
    assert.match(policy, /script-src [^;]*'strict-dynamic'/)
    assert.doesNotMatch(policy.split('script-src ')[1].split(';')[0], /unsafe-inline|unsafe-eval/)
    const html = await response.text()
    assert.match(html, /src="app.js\?v=[a-f0-9]+" integrity="sha256-/)
    await page.route(url + '/', async route => {
      const original = await route.fetch()
      const body = (await original.text()).replace('</body>', '<script>globalThis.inlineRan = true</script><button id="policyTestButton" onclick="globalThis.inlineEventRan = true">策略测试</button></body>')
      await route.fulfill({ response: original, body })
    })
    await page.reload()
    await page.locator('#policyTestButton').click()
    await page.waitForFunction(() => globalThis.cspViolations.length >= 2)
    assert.deepEqual(await page.evaluate(() => [!!globalThis.inlineRan, !!globalThis.inlineEventRan]), [false, false])
    await page.evaluate(() => { globalThis.cspViolations = [] })
  })
  await check('备份模块事件绑定、预览取消和退出清理', async page => {
    let config = { url: 'https://backup.invalid', username: '测试', path: 'backups', frequency: 'daily', time: '03:00', weekday: 1, retention: 3, auto: false }
    const calls = []
    await page.route('**/api/admin/backups/**', async route => {
      const request = route.request(), path = new URL(request.url()).pathname
      if (request.method() !== 'GET') calls.push(path)
      let result = {}
      if (path.endsWith('/webdav/config')) {
        if (request.method() === 'PUT') config = request.postDataJSON()
        result = config
      } else if (path.endsWith('/webdav/files')) result = [{ name: 'lxsc-test.lxsc-backup', size: 1024, lastModified: 1 }]
      else if (path.endsWith('/inspect')) result = { manifest: { createdAt: 1, appVersion: '测试', stats: { users: 1 } } }
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(result) })
    })
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, 'admin')
    await page.locator('nav [data-tab=backups]').click()
    await page.locator('[data-backup-action=restore]').waitFor()
    await page.locator('#webdavForm [name=password]').fill('仅供测试的密码')
    const saved = page.waitForResponse(r => r.url().endsWith('/webdav/config') && r.request().method() === 'PUT')
    await page.locator('#webdavForm [type=submit]').click(); await saved
    const tested = page.waitForResponse(r => r.url().endsWith('/webdav/test'))
    await page.locator('#testWebDAVButton').click(); await tested
    const form = page.locator('#importBackupForm')
    await form.locator('[name=file]').setInputFiles({ name: 'test.lxsc-backup', mimeType: 'application/octet-stream', buffer: Buffer.from('隔离备份') })
    let started, release
    const ready = new Promise(resolve => { started = resolve }), pending = new Promise(resolve => { release = resolve })
    await page.route('**/api/admin/backups/inspect', async route => {
      started(); await pending
      try { await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ manifest: { stats: { users: 1 } } }) }) } catch { /* 预览取消后无需发送旧响应。 */ }
    })
    await page.locator('#inspectBackupButton').click(); await ready
    await form.locator('[name=password]').fill('新的密码')
    release()
    await page.waitForTimeout(100)
    assert.equal(await form.locator('[name=confirm]').isDisabled(), true, '旧预览不能授权新密码或文件')
    await page.unroute('**/api/admin/backups/inspect')
    await page.locator('#inspectBackupButton').click()
    await form.locator('[name=confirm]').waitFor({ state: 'visible' })
    await page.waitForFunction(() => !document.querySelector('#importBackupForm [name=confirm]').disabled)
    await form.locator('[name=confirm]').check()
    page.once('dialog', dialog => dialog.accept())
    const imported = page.waitForResponse(r => r.url().endsWith('/backups/import'))
    await form.locator('[type=submit]').click(); await imported
    assert(calls.includes('/api/admin/backups/webdav/config'))
    assert(calls.includes('/api/admin/backups/webdav/test'))
    assert(calls.includes('/api/admin/backups/import'))
    await page.locator('#webdavForm [name=password]').fill('退出应清空')
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    assert.equal(await page.locator('#webdavForm [name=password]').inputValue(), '')
  })
  await check('音源模块通过事件委托切换状态与优先级', async page => {
    const source = { id: 99, name: '测试音源', enabled: true, priority: 5, status: { state: 'ready', platforms: {} } }
    await page.route('**/api/admin/sources**', async route => {
      if (route.request().method() === 'PUT') Object.assign(source, route.request().postDataJSON())
      const list = new URL(route.request().url()).pathname.endsWith('/sources')
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(list ? [source] : source) })
    })
    await page.locator('#logoutButton').click(); await page.locator('#login:not(.hidden)').waitFor(); await login(page, url, 'admin')
    await page.locator('nav [data-tab=sources]').click()
    await page.locator('[data-source-action=toggle]').click()
    await page.waitForFunction(() => document.querySelector('[data-source-action=toggle]')?.textContent === '启用')
    assert.equal(source.enabled, false)
    await page.locator('[data-source-priority]').fill('9')
    await page.locator('[data-source-priority]').press('Tab')
    await page.waitForFunction(() => document.querySelector('[data-source-priority]')?.value === '9')
    await page.waitForTimeout(100)
    assert.equal(source.priority, 9)
  })
}
