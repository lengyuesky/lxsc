// 接入既有隔离浏览器服务，真实音频计时和示例看板布局分别验收。
const assert = require('node:assert/strict')
const path = require('node:path')

module.exports = async function ({ check, url, artifacts, login, search, playSearch, holdResponse }) {
  const stats = async page => {
    const response = await page.request.get(url + '/api/app/listening/stats')
    assert.equal(response.status(), 200)
    return response.json()
  }
  await check('听歌统计空状态、权限和错误重试', async page => {
    await page.locator('nav [data-tab=listening]').click()
    await page.locator('.listening-empty').waitFor()
    assert.equal(await page.locator('#listeningScopeWrap').isVisible(), false)
    assert.equal((await page.request.get(url + '/api/app/listening/stats?scope=all')).status(), 403)
    let failures = 0
    await page.route('**/api/app/listening/stats?*', route => failures++ === 0
      ? route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: '暂时无法读取，请重试' }) }) : route.continue())
    await page.locator('#listeningRefresh').click()
    await page.locator('#listeningRetry').waitFor()
    assert.equal(await page.locator('.listening-hero').isVisible(), true)
    await page.locator('#listeningRetry').click()
    await page.waitForFunction(() => !listeningState.loading && !document.querySelector('#listeningRetry'))
    await page.screenshot({ path: path.join(artifacts, 'listening-empty.png'), fullPage: true })
  })
  await check('真实音频收听计时、暂停与拖动排除、客户端估算去重', async page => {
    const before = await stats(page)
    await search(page); await playSearch(page)
    await page.waitForFunction(() => document.querySelector('#webAudio').currentTime >= 2)
    await page.locator('#playerToggle').click()
    await page.evaluate(() => listeningTracker.flush())
    const paused = await stats(page)
    assert.ok(paused.webMs - before.webMs >= 1200 && paused.webMs - before.webMs < 5000, '实际播放应约为 2 秒')
    await page.evaluate(() => webPlayer.seek(25))
    await page.waitForFunction(() => !document.querySelector('#webAudio').seeking)
    await page.evaluate(() => listeningTracker.flush())
    assert.equal((await stats(page)).webMs, paused.webMs, '暂停后的拖动不应计时')
    await page.locator('#playerToggle').click()
    await page.waitForFunction(() => document.querySelector('#webAudio').currentTime >= 27)
    await page.locator('#playerToggle').click()
    await page.evaluate(() => listeningTracker.flush())
    assert.ok((await stats(page)).webMs > paused.webMs + 1200)
    const stamp = Date.now()
    const scrobble = `${url}/rest/scrobble.view?u=alice&p=test-password&f=json&c=browser-test&id=tr-wy-1&time=${stamp}`
    assert.equal((await page.request.get(scrobble + '&submission=false')).status(), 200)
    const preClient = await stats(page)
    for (let i = 0; i < 2; i++) assert.equal((await page.request.get(scrobble)).status(), 200)
    const after = await stats(page)
    assert.equal(after.clientMs - preClient.clientMs, 45000)
    assert.equal(after.plays - preClient.plays, 1)
    await page.locator('nav [data-tab=listening]').click()
    await page.locator('.listening-hero').waitFor()
    assert.ok(await page.locator('.listening-rank li').count() > 0)
  })
  await check('管理员全站排行和用户筛选、退出取消统计请求', async page => {
    await page.locator('#logoutButton').click(); await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, 'admin')
    await page.locator('nav [data-tab=listening]').click()
    await page.waitForFunction(() => listeningState.usersLoaded)
    await page.locator('#listeningScope').selectOption('all')
    await page.locator('.listening-users [data-listening-user]').first().waitFor()
    await page.locator('.listening-users [data-listening-user]').first().click()
    await page.waitForFunction(() => !listeningState.loading)
    assert.match(await page.locator('#listeningScope').inputValue(), /^user:/)
    assert.equal(await page.locator('.listening-users').count(), 0)
    const held = await holdResponse(page, '**/api/app/listening/stats?*')
    await page.locator('#listeningRefresh').click(); await held.ready
    await page.locator('#logoutButton').click(); await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, 'bob'); await held.finish()
    await page.locator('nav [data-tab=listening]').click(); await page.locator('.listening-empty').waitFor()
    assert.equal(await page.locator('#listeningScopeWrap').isVisible(), false)
    assert.equal((await stats(page)).totalMs, 0)
  })
  await check('统计范围切换、全年日历与移动播放器避让', async page => {
    await search(page); await playSearch(page)
    await page.locator('nav [data-tab=listening]').click()
    await page.locator('.listening-hero').waitFor()
    for (const days of [7, 90, 365, 30]) {
      await page.locator('#listeningRange').selectOption(String(days))
      await page.waitForFunction(days => !listeningState.loading && listeningState.data.daily.length === days, days)
      assert.equal(await page.locator('button.listening-day').count(), days)
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    }
    await page.locator('.listening-footnote').scrollIntoViewIfNeeded()
    const footer = await page.locator('.listening-footnote').boundingBox(), player = await page.locator('#playerBar').boundingBox()
    assert.ok(footer.y + footer.height <= player.y + 1, '底部播放器不能遮挡统计说明')
  }, { viewport: { width: 390, height: 844 }, colorScheme: 'dark', isMobile: true, hasTouch: true })
  const demo = () => {
    const today = new Date(Date.now() + 8 * 3600000).toISOString().slice(0, 10)
    const end = Date.parse(today + 'T00:00:00+08:00')
    const daily = Array.from({ length: 30 }, (_, i) => {
      const webMs = i % 8 === 0 ? 0 : Math.round((Math.sin(i * 1.7) + 1.3) * 1300000)
      const clientMs = i % 6 === 0 ? 0 : Math.round((Math.cos(i * .9) + 1.2) * 900000)
      return { day: new Date(end - (29 - i) * 86400000 + 8 * 3600000).toISOString().slice(0, 10), webMs, clientMs, totalMs: webMs + clientMs, plays: i % 8 === 0 ? 0 : 12, unknownDuration: 0 }
    })
    const names = ['晴天', '夜曲', '这世界那么多人', '起风了', '如愿', '七里香', '慢冷', '平凡之路', '日落大道', '<img src=x onerror=alert(1)>']
    const topTracks = names.map((name, i) => ({ id: 'tr-wy-' + i, name, singer: ['周杰伦', '周杰伦', '莫文蔚', '买辣椒也用券', '王菲', '周杰伦', '梁静茹', '朴树', '梁博', '测试歌手'][i], totalMs: 4000000 - i * 320000, plays: 22 - i }))
    const webMs = daily.reduce((sum, d) => sum + d.webMs, 0), clientMs = daily.reduce((sum, d) => sum + d.clientMs, 0)
    return { enabledAt: end - 60 * 86400000, from: daily[0].day, to: today, timezone: 'Asia/Shanghai', daily, webMs, clientMs, totalMs: webMs + clientMs, plays: 312, tracks: 87, activeDays: 29, countedDays: 30, averageMs: Math.round((webMs + clientMs) / 30), unknownDuration: 2, topTracks, topUsers: [] }
  }
  for (const [label, width, height] of [['desktop', 1440, 1080], ['tablet', 820, 1180], ['mobile', 390, 844]]) {
    for (const theme of ['light', 'dark']) {
      await check(`听歌看板 ${label} ${theme} 布局与图表交互`, async page => {
        await page.route('**/api/app/listening/stats?*', route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(demo()) }))
        await page.locator('nav [data-tab=listening]').click(); await page.locator('.listening-hero').waitFor()
        assert.equal(await page.locator('#listeningContent img').count(), 0)
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
        await page.locator('#listeningDaySlider').focus(); await page.keyboard.press('ArrowLeft')
        assert.match(await page.locator('#listeningDayDetail').innerText(), /实听/)
        await page.locator('button.listening-day').last().click(); await page.keyboard.press('ArrowLeft')
        assert.equal(await page.locator('.listening-day.selected').count(), 1)
        await page.evaluate(() => { document.activeElement?.blur(); window.scrollTo(0, 0) })
        await page.screenshot({ path: path.join(artifacts, `listening-${label}-${theme}.png`), fullPage: true, animations: 'disabled' })
      }, { viewport: { width, height }, colorScheme: theme, isMobile: label === 'mobile', hasTouch: label !== 'desktop' })
    }
  }
}
