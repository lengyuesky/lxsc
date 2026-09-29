// 优化回归只使用隔离服务与拦截响应，不访问远程备份或真实音源。
const assert = require('node:assert/strict')
module.exports = async ({ check, url, login }) => {
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
