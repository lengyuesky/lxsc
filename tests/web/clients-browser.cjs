// 使用隔离 fixture 验收说明页，不访问客户端官网或第三方音乐资源。
const assert = require('node:assert/strict')
const path = require('node:path')

module.exports = async ({ check, url, login, artifacts, search, playSearch }) => {
  const openGuide = async page => {
    await page.locator('nav [data-tab=clients]').click()
    await page.locator('#tab-clients.active').waitFor()
  }

  await check('客户端说明：普通用户入口、连接信息与复制，不读取管理接口', async page => {
    const adminCalls = []
    page.on('request', request => { if (new URL(request.url()).pathname.startsWith('/api/admin/')) adminCalls.push(request.url()) })
    await openGuide(page)
    assert.equal(await page.locator('#clientServerURL').inputValue(), url)
    assert.equal(await page.locator('#clientUsername').inputValue(), 'alice')
    assert.equal(await page.locator('#clientServerURL').evaluate(field => field.readOnly), true)
    assert.equal(await page.locator('#clientUsername').evaluate(field => field.readOnly), true)
    assert.equal(await page.locator('#tab-clients input[type=password], #tab-clients canvas, #tab-clients img').count(), 0)
    assert.equal(await page.locator('#clientAPIHelp').isVisible(), false)
    assert.equal(await page.locator('nav [data-tab=debug]').isVisible(), false)
    await page.locator('.client-faq details').last().locator('summary').click()
    assert.doesNotMatch(await page.locator('#tab-clients').innerText(), /API Key|API 调试/)
    assert.match(await page.locator('#tab-clients').innerText(), /箭头音乐.*Amcfy Music/s)
    assert.match(await page.locator('#tab-clients').innerText(), /不要附加 \/rest/)
    await page.evaluate(() => {
      globalThis.clientCopied = []
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async value => { globalThis.clientCopied.push(value) } } })
    })
    await page.locator('#copyClientServerURL').click()
    await page.locator('#copyClientUsername').click()
    assert.deepEqual(await page.evaluate(() => globalThis.clientCopied), [url, 'alice'])
    assert.deepEqual(adminCalls, [])
    await page.screenshot({ path: path.join(artifacts, 'client-guide-desktop.png'), fullPage: true })
  })

  await check('客户端说明：API 管理仅管理员可见，普通用户请求被后端拒绝', async page => {
    await openGuide(page)
    const me = (await (await page.request.get(url + '/api/auth/me')).json()).user
    for (const targetID of [me.id, 1]) {
      const base = url + '/api/admin/users/' + targetID
      assert.equal((await page.request.get(base + '/apikeys')).status(), 403)
      assert.equal((await page.request.post(base + '/apikey', { data: { label: '不应创建', days: 90 } })).status(), 403)
      assert.equal((await page.request.delete(base + '/apikeys/forbidden-key')).status(), 403)
    }
    assert.equal((await page.request.get(url + '/api/admin/debug-tokens')).status(), 403)
    assert.equal((await page.request.post(url + '/api/admin/debug-tokens', { data: {}, headers: { 'X-LXSC-Debug-Management': '1' } })).status(), 403)
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, 'admin')
    await openGuide(page)
    assert.equal(await page.locator('#clientAPIHelp').isVisible(), true)
    assert.match(await page.locator('#clientAPIHelp').innerText(), /仅管理员/)
    assert.equal(await page.locator('nav [data-tab=debug]').isVisible(), true)
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, 'alice')
    await openGuide(page)
    assert.equal(await page.locator('#clientAPIHelp').isVisible(), false)
    await page.locator('.client-faq details').last().locator('summary').click()
    assert.doesNotMatch(await page.locator('#tab-clients').innerText(), /API Key|API 调试/)
  })

  await check('客户端说明：直接访问、刷新、退出与换号不保留旧连接信息', async page => {
    await openGuide(page)
    await page.reload()
    await page.locator('#tab-clients.active').waitFor()
    assert.equal(await page.locator('#clientUsername').inputValue(), 'alice')
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    assert.equal(await page.locator('#clientServerURL').inputValue(), '')
    assert.equal(await page.locator('#clientUsername').inputValue(), '')
    await page.goto(url + '/#clients')
    await page.locator('#login:not(.hidden)').waitFor()
    await page.locator('#loginForm [name=username]').fill('bob')
    await page.locator('#loginForm [name=password]').fill('test-password')
    await page.locator('#loginForm button').click()
    await page.locator('#tab-clients.active').waitFor()
    assert.equal(await page.locator('#clientUsername').inputValue(), 'bob')
    assert.equal(await page.locator('#clientServerURL').inputValue(), url)
    assert.equal(await page.locator('nav [data-tab=clients]').evaluate(link => link.classList.contains('active')), true)
  })

  await check('客户端说明：剪贴板不可用时选中文本，常见问题可展开', async page => {
    await openGuide(page)
    await page.evaluate(() => Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined }))
    await page.locator('#copyClientServerURL').click()
    assert.match(await page.locator('#toast').innerText(), /手动复制/)
    assert.equal(await page.locator('#clientServerURL').evaluate(field => field.selectionEnd - field.selectionStart), url.length)
    await page.locator('#copyClientUsername').click()
    assert.equal(await page.locator('#clientUsername').evaluate(field => field.selectionEnd - field.selectionStart), 'alice'.length)
    assert.match(await page.locator('#clientNetworkNotice').innerText(), /HTTPS/)
    assert.match(await page.locator('#clientNetworkNotice').innerText(), /仅供本机访问/)
    const question = page.locator('.client-faq details').filter({ hasText: '连接成功，但首页或音乐库没有歌曲' })
    await question.locator('summary').click()
    assert.equal(await question.evaluate(element => element.open), true)
    assert.match(await question.locator('p').innerText(), /不是本地音乐文件扫描器/)
  })

  await check('客户端说明：管理员概览可进入，导航不打断现有播放', async page => {
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, 'admin')
    await page.locator('nav [data-tab=dashboard]').click()
    await page.locator('#tab-dashboard a[href="#clients"]').click()
    await page.locator('#tab-clients.active').waitFor()
    assert.equal(await page.locator('#clientUsername').inputValue(), 'admin')
    await search(page)
    await playSearch(page)
    const current = await page.evaluate(() => webPlayer.track.id)
    await openGuide(page)
    assert.deepEqual(await page.evaluate(() => [webPlayer.track.id, webPlayer.status]), [current, 'playing'])
    await page.locator('#tab-clients a[href="#search"]').click()
    await page.locator('#tab-search.active').waitFor()
  })

  await check('客户端说明：移动端深色可阅读和复制，无横向溢出', async page => {
    await openGuide(page)
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    await page.locator('#copyClientServerURL').click()
    await page.locator('.client-faq details summary').first().click()
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    await page.screenshot({ path: path.join(artifacts, 'client-guide-mobile-dark.png'), fullPage: true })
  }, { viewport: { width: 390, height: 844 }, colorScheme: 'dark', isMobile: true, hasTouch: true })
}
