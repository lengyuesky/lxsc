const assert = require('node:assert/strict')
const path = require('node:path')
const { createHash } = require('node:crypto')

module.exports = async function ({ check, url, artifacts, login, search, playSearch, holdResponse, admin }) {
  async function mode(value) { assert.equal((await admin.put(url + '/api/admin/settings', { data: { streamMode: value } })).status(), 200) }
  async function show(page) {
    await page.locator('nav [data-tab=listening]').click()
    const panel = page.locator('#recentPlaybackPanel')
    await panel.waitFor({ state: 'attached' })
    if (!await panel.evaluate(element => element.open)) await panel.locator(':scope > summary').click()
    await page.waitForFunction(() => globalThis.LXSCRecent?.state.loaded && !LXSCRecent.state.loading)
  }
  await check('最近播放默认收起在页面底部，展开才读取并记住状态', async page => {
    let requests = 0
    page.on('request', request => { if (request.url().includes('/api/app/listening/recent')) requests++ })
    await page.locator('nav [data-tab=listening]').click()
    await page.locator('.listening-hero').waitFor()
    const panel = page.locator('#recentPlaybackPanel')
    assert.equal(await panel.evaluate(element => element.open), false, '最近播放应默认收起')
    assert.equal(await page.locator('#recentPlaybackRows').isVisible(), false)
    assert.equal(requests, 0, '收起时不应读取最近播放')
    const [box, grid, footer] = await Promise.all([panel.boundingBox(), page.locator('.listening-main-grid').boundingBox(), page.locator('.listening-footnote').boundingBox()])
    assert.ok(box.y >= grid.y + grid.height && box.y + box.height <= footer.y, '最近播放应位于统计内容之后、说明之前')
    assert.ok(box.height < 120, '收起时只占一行摘要')
    await panel.locator(':scope > summary').click()
    await page.waitForFunction(() => LXSCRecent.state.loaded && !LXSCRecent.state.loading)
    assert.ok(requests >= 1)
    assert.match(await page.locator('#recentPlaybackCount').innerText(), /条/)
    await page.reload(); await page.locator('#app:not(.hidden)').waitFor()
    await page.locator('nav [data-tab=listening]').click()
    await page.waitForFunction(() => LXSCRecent.panel?.open && LXSCRecent.state.loaded)
    await page.locator('#recentPlaybackPanel > summary').click()
    await page.waitForFunction(() => localStorage.getItem('lxsc.recentPlayback.open') === null)
  })
  await check('最近播放真实302交付、歌曲用户音源和展开详情', async page => {
    await mode('redirect'); await search(page, '最近播放真实测试'); await playSearch(page)
    await page.locator('#playerToggle').click(); await show(page)
    const data = await (await page.request.get(url + '/api/app/listening/recent')).json(), latest = data.items[0]
    for (const [field, expected] of Object.entries({ userName: 'alice', protocol: 'web', client: '网页播放器', sourceName: '浏览器测试音源', platform: 'wy', delivery: '302', outcome: 'success', kind: 'playback' })) assert.equal(latest[field], expected)
    const row = page.locator(`[data-playback-row="${latest.id}"]`)
    await row.locator('summary').click()
    await page.locator(`[data-playback-detail-row="${latest.id}"] .recent-playback-events`).waitFor()
    assert.match(await row.innerText(), /网页播放器/)
    assert.match(await page.locator(`[data-playback-detail-row="${latest.id}"]`).innerText(), /浏览器测试音源/)
    await page.locator('#recentPlaybackPanel').screenshot({ path: path.join(artifacts, 'recent-playback-real-302.png') })
  })
  await check('最近播放自动降质共用日志、代理与失败结果准确', async page => {
    await mode('proxy'); await search(page, '最近代理播放'); await playSearch(page)
    await page.locator('#playerToggle').click(); await show(page)
    await page.locator('#recentPlaybackDelivery').selectOption('proxy')
    await page.waitForFunction(() => !LXSCRecent.state.loading && LXSCRecent.state.items.length > 0)
    assert.ok((await page.evaluate(() => LXSCRecent.state.items)).every(item => item.delivery === 'proxy'))
    await search(page, '播放失败'); await page.locator('#searchTable [data-song-action=play]').first().click()
    await page.waitForFunction(() => webPlayer.status === 'error'); await show(page)
    await page.locator('#recentPlaybackDelivery').selectOption(''); await page.locator('#recentPlaybackOutcome').selectOption('failed')
    await page.waitForFunction(() => !LXSCRecent.state.loading && LXSCRecent.state.items.length > 0)
    const latest = await page.evaluate(() => LXSCRecent.state.items[0])
    assert.equal(latest.outcome, 'failed'); assert.equal(latest.requests, 2, '自动降质的两次请求应共用同一条日志')
    assert.equal(latest.delivery, '', '取链失败不能伪造已发生的代理或302交付')
    await mode('redirect')
  })
  await check('最近播放飞牛开始事件加入估算、媒体关联与客户端筛选', async page => {
    await mode('redirect')
    const before = await (await page.request.get(url + '/api/app/listening/stats')).json()
    assert.equal((await page.request.post(url + '/music/api/v1/user/password-login', { data: { username: 'alice', password: createHash('sha256').update('test-password').digest('hex'), deviceId: '1'.repeat(32) } })).status(), 200)
    const list = await (await page.request.get(url + '/music/api/v1/track/list')).json(), guid = list.data.list[0].guid
    assert.equal((await page.request.get(url + '/music/api/v1/track/stream?guid=' + guid, { maxRedirects: 0 })).status(), 302)
    for (let i = 0; i < 2; i++) assert.equal((await page.request.post(url + '/music/api/v1/event/report', { data: { events: [{ eventType: 'track_play', payload: { trackGUID: guid } }] } })).status(), 200)
    const after = await (await page.request.get(url + '/api/app/listening/stats')).json()
    assert.equal(after.clientMs - before.clientMs, 45000); assert.equal(after.plays - before.plays, 1)
    await show(page); await page.locator('#recentPlaybackClient').selectOption('飞牛音乐')
    await page.waitForFunction(() => !LXSCRecent.state.loading && LXSCRecent.state.items.length > 0)
    const items = await page.evaluate(() => LXSCRecent.state.items)
    assert.ok(items.every(item => item.protocol === 'fnmusic' && item.userName === 'alice'))
    assert.equal(items[0].kind, 'playback'); assert.equal(items[0].delivery, '302')
    assert.match(await page.locator('.listening-footnote').innerText(), /飞牛.*可能高估/)
  })
  await check('最近播放换号清理迟到请求、管理员用户筛选', async page => {
    await show(page)
    const held = await holdResponse(page, '**/api/app/listening/recent?*')
    await page.locator('#recentPlaybackRefresh').click(); await held.ready
    await page.locator('#logoutButton').click(); await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, 'bob'); await held.finish(); await show(page)
    assert.ok((await page.evaluate(() => LXSCRecent.state.items)).every(item => item.userName === 'bob'))
    assert.equal(await page.locator('#recentPlaybackClient').inputValue(), '')
    await page.locator('#logoutButton').click(); await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, 'admin'); await show(page); await page.waitForFunction(() => listeningState.usersLoaded)
    await page.locator('#listeningScope').selectOption('all')
    await page.waitForFunction(() => !LXSCRecent.state.loading && LXSCRecent.state.params.scope === 'all')
    const user = await page.locator('#listeningScope option').evaluateAll(options => options.find(option => option.textContent === 'alice').value)
    await page.locator('#listeningScope').selectOption(user)
    await page.waitForFunction(() => !LXSCRecent.state.loading && LXSCRecent.state.params.scope === 'user')
    assert.ok((await page.evaluate(() => LXSCRecent.state.items)).every(item => item.userName === 'alice'))
  })
  const now = Date.now()
  function demo(id) {
    const delivery = id % 2 ? '302' : 'proxy', failed = id % 5 === 0
    const sourceId = id === 1 ? 2 : id % 2 ? 1 : 2
    return { id, userId: 1, userName: id % 3 ? '测试用户甲' : '测试用户乙', protocol: id % 3 ? 'web' : 'fnmusic', client: id % 3 ? '网页播放器' : '飞牛音乐', kind: id % 3 ? 'playback' : 'request', trackId: 'tr-wy-' + id, name: id === 2 ? '<img src=x onerror=alert(1)>' : ['测试歌曲 · 晴天', '夜曲', '起风了'][id % 3], singer: '合成测试歌手', requestedPlatform: 'wy', platform: id === 1 || id % 3 === 0 ? 'kg' : 'wy', resolvedTrackId: 'tr-kg-' + id, sourceId, sourceName: sourceId === 1 ? '测试音源甲' : '测试音源乙', requestedQuality: 'flac', quality: '320k', mode: 'redirect', delivery: failed ? '' : delivery, outcome: failed ? 'failed' : 'success', status: failed ? 502 : delivery === '302' ? 302 : 206, cached: id % 2 === 0, startedAt: now - id * 60000, updatedAt: now, requests: 2, detailCount: 2 }
  }
  async function mock(page, count = 6) {
    await page.route('**/api/app/listening/recent?*', route => {
      const params = new URL(route.request().url()).searchParams
      const all = Array.from({ length: count }, (_, index) => demo(index + 1)).filter(item => (!params.get('sourceId') || String(item.sourceId) === params.get('sourceId')) && (!params.get('client') || item.client === params.get('client')) && (!params.get('query') || item.name.includes(params.get('query'))))
      const offset = params.get('cursor') ? 50 : 0
      return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ items: all.slice(offset, offset + 50), nextCursor: all.length > offset + 50 ? 'older-page' : '', sources: [{ id: 1, name: '测试音源甲' }, { id: 2, name: '测试音源乙' }], clients: ['网页播放器', '飞牛音乐'], generatedAt: now, retentionDays: 30 }) })
    })
    await page.route('**/api/app/listening/recent/*?*', route => {
      const id = Number(new URL(route.request().url()).pathname.split('/').at(-1)), item = demo(id)
      return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ ...item, events: [{ request: 1, at: now, stage: 'resolve', sourceId: item.sourceId === 1 ? 2 : 1, sourceName: item.sourceId === 1 ? '测试音源乙' : '测试音源甲', platform: 'wy', quality: 'flac', error: 'timeout', elapsedMs: 1500 }, { request: 2, at: now, stage: item.platform === 'kg' ? 'cross_source' : 'source_fallback', sourceId: item.sourceId, sourceName: item.sourceName, platform: item.platform, quality: '320k', status: 206, cached: item.cached, error: 'none', elapsedMs: 130 }] }) })
    })
  }
  await check('最近播放加载更多、筛选与汇总刷新保留输入和旧记录', async page => {
    await mock(page, 55); await show(page)
    assert.equal(await page.locator('[data-playback-row]').count(), 50)
    await page.locator('#recentPlaybackMore').click()
    await page.waitForFunction(() => !LXSCRecent.state.loading && LXSCRecent.state.items.length === 55)
    await page.locator('[data-playback-row="52"] summary').click()
    await page.locator('[data-playback-detail-row="52"] .recent-playback-events').waitFor()
    await page.evaluate(() => { window.oldRecentPanel = LXSCRecent.panel; window.oldRecentDetails = document.querySelector('[data-playback-row="52"] details') })
    await page.evaluate(() => loadListening(true))
    assert.equal(await page.evaluate(() => oldRecentPanel === LXSCRecent.panel && oldRecentDetails.open), true)
    await page.locator('#recentPlaybackQuery').fill('尚未提交的搜索')
    await page.evaluate(() => { window.editingRecentQuery = document.querySelector('#recentPlaybackQuery') })
    await page.evaluate(() => loadListening(true))
    assert.equal(await page.locator('#recentPlaybackQuery').inputValue(), '尚未提交的搜索')
    assert.equal(await page.evaluate(() => editingRecentQuery === document.querySelector('#recentPlaybackQuery')), true)
    assert.equal(await page.evaluate(() => LXSCRecent.state.params.query || ''), '')
    await page.locator('#recentPlaybackQuery').fill(''); await page.locator('#recentPlaybackSource').selectOption('2')
    await page.waitForFunction(() => !LXSCRecent.state.loading && LXSCRecent.state.params.sourceId === '2')
    assert.ok((await page.evaluate(() => LXSCRecent.state.items)).every(item => item.sourceId === 2))
  })
  for (const [label, width, height] of [['desktop', 1440, 1080], ['tablet', 820, 1180], ['mobile', 390, 844], ['compact', 320, 740]]) for (const theme of ['light', 'dark']) {
    await check(`最近播放 ${label} ${theme} 布局、详情与转义`, async page => {
      await mock(page); await show(page)
      assert.equal(await page.locator('#recentPlaybackPanel img').count(), 0)
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
      const panel = page.locator('#recentPlaybackPanel'), boxes = await Promise.all([panel.boundingBox(), page.locator('.listening-metrics').boundingBox()])
      assert.ok(boxes[0].y >= boxes[1].y + boxes[1].height, '日志应位于汇总卡片下方')
      await page.locator('[data-playback-row="1"] summary').click(); await page.locator('[data-playback-detail-row="1"] .recent-playback-events').waitFor()
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
      await panel.screenshot({ path: path.join(artifacts, `recent-playback-${label}-${theme}.png`), animations: 'disabled' })
    }, { viewport: { width, height }, colorScheme: theme, isMobile: width <= 390, hasTouch: width <= 390 })
  }
}
