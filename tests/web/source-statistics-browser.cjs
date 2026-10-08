const assert = require('node:assert/strict')
const path = require('node:path')

function statistics(window = '1h', calls = 10) {
  const now = Date.now(), bucketMinutes = window === '24h' ? 60 : 5, count = window === '24h' ? 24 : 12
  const from = now - count * bucketMinutes * 60000
  const summary = { calls, success: calls ? calls - 2 : 0, failed: calls ? 2 : 0, cancelled: 0, attempts: calls, downgrades: 0, durationMs: calls * 120, averageMs: calls ? 120 : 0, maxMs: calls ? 200 : 0, errors: calls ? { timeout: 1, script: 1 } : {} }
  const empty = { calls: 0, success: 0, failed: 0, cancelled: 0, attempts: 0, downgrades: 0, durationMs: 0, averageMs: 0, maxMs: 0, errors: {} }
  const source = { id: 10, name: '调用统计测试音源', enabled: true, state: 'ready', statistics: { since: new Date(from).toISOString(), from: new Date(from).toISOString(), to: new Date(now).toISOString(), bucketMinutes, inFlight: 0, total: summary, summary, groups: [{ platform: 'wy', action: 'musicUrl', ...summary }], trend: Array.from({ length: count }, (_, index) => ({ at: new Date(from + index * bucketMinutes * 60000).toISOString(), ...(index === count - 1 ? summary : empty) })), recent: calls ? [{ at: new Date(now - 1000).toISOString(), platform: 'wy', action: 'musicUrl', song: '合成测试歌曲', singer: '测试歌手', requestedQuality: '320k', quality: '', attempts: 1, durationMs: 200, outcome: 'failed', errorCategory: 'timeout', downgraded: false }] : [] } }
  return { generatedAt: new Date(now).toISOString(), window, recentLimit: 100, sources: [source] }
}

module.exports = async ({ check, url, login, artifacts }) => {
  async function asAdmin(page) {
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, 'admin')
  }

  await check('音源调用统计：真实计数、指定音源测试和自动刷新', async page => {
    assert.equal((await page.request.get(url + '/api/admin/sources/statistics')).status(), 403)
    await asAdmin(page)
    const script = `// @name 调用统计验收
lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['320k','128k']}}});
globalThis.__lx_request=({info})=>{if(info.type==='320k'||info.musicInfo.name.includes('失败'))throw new Error('https://private.invalid/?token=secret');return {url:'https://media.invalid/test?token=secret'}};`
    const created = await page.request.post(url + '/api/admin/sources', { data: { name: '测试音源 <img src=x onerror=alert(1)>', script, priority: 200 } })
    assert.equal(created.status(), 200)
    const source = await created.json()
    try {
      for (const query of ['调用成功', '调用失败']) {
        const response = await page.request.post(url + `/api/admin/sources/${source.id}/test`, { data: { platform: 'wy', query, quality: '320k' } })
        assert.equal(response.status(), 200)
        assert.equal((await response.json()).wy.ok, query === '调用成功')
      }
      await page.clock.install()
      await page.locator('nav [data-tab=sources]').click()
      await page.locator(`#sourceStatsSource option[value="${source.id}"]`).waitFor({ state: 'attached' })
      await page.locator('#sourceStatsSource').selectOption(String(source.id))
      await page.waitForFunction(() => document.querySelector('#sourceStatsTable tbody')?.textContent.includes('50%'))
      const metrics = page.locator('.source-call-metric strong')
      assert.equal(await metrics.nth(0).textContent(), '2')
      assert.equal(await metrics.nth(1).textContent(), '50%')
      assert.equal(await metrics.nth(3).textContent(), '1')
      assert.match(await page.locator('#sourceStatsBody').textContent(), /4 次尝试 · 降级 1 次/)
      assert.equal(await page.locator('#sourceStatsBody img').count(), 0)
      await page.locator('#sourceStatsRecent summary').click()
      assert.equal(await page.locator('#sourceStatsRecent tbody tr').count(), 2)
      assert.match(await page.locator('#sourceStatsRecent').textContent(), /320k → 128k/)
      assert.match(await page.locator('#sourceStatsRecent').textContent(), /脚本调用失败/)
      assert.doesNotMatch(await page.locator('#sourceStatsPanel').textContent(), /private\.invalid|media\.invalid|token=secret/)
      await page.locator('[data-source-period]').last().click()
      assert.match(await page.locator('#sourceStatsPeriod').textContent(), /成功 1，失败 1/)
      const priority = page.locator(`[data-source-priority="${source.id}"]`)
      await priority.fill('777')
      const fresh = page.waitForResponse(response => new URL(response.url()).pathname === '/api/admin/sources/statistics')
      await page.clock.fastForward(31000)
      await fresh
      await page.waitForFunction(() => !document.querySelector('#refreshSourceStats').disabled)
      assert.equal(await priority.inputValue(), '777', '统计刷新不能重新渲染正在编辑的音源卡片')
      assert.equal(await page.locator('#sourceStatsRecent').getAttribute('open'), '')
      assert.equal(await page.locator('#sourceStatsSource').inputValue(), String(source.id))
      await priority.fill('200')
      await page.locator('#sourceStatsPanel').screenshot({ path: path.join(artifacts, 'source-statistics-desktop.png'), animations: 'disabled' })
      const test = page.waitForResponse(response => response.url().endsWith(`/sources/${source.id}/test`))
      await page.locator(`[data-source-action=test][data-source-id="${source.id}"]`).click()
      assert.equal((await test).status(), 200)
      await page.waitForFunction(() => document.querySelector('.source-call-metric strong')?.textContent === '3')
      const snapshot = await (await page.request.get(url + '/api/admin/sources/statistics')).json()
      assert.equal(snapshot.sources.find(item => item.id === source.id).statistics.summary.calls, 3)
    } finally {
      await page.request.delete(url + '/api/admin/sources/' + source.id)
    }
  })

  await check('音源调用统计：切换时间范围、失败重试与退出清理', async page => {
    await asAdmin(page)
    let release, arrived, first = true, fail = false
    const held = new Promise(resolve => { release = resolve }), ready = new Promise(resolve => { arrived = resolve })
    await page.route('**/api/admin/sources/statistics?*', async route => {
      const window = new URL(route.request().url()).searchParams.get('window')
      if (first) { first = false; arrived(); await held }
      if (fail) return route.fulfill({ status: 503, json: { error: '合成统计故障' } })
      await route.fulfill({ json: statistics(window, window === '24h' ? 40 : 10) }).catch(() => {})
    })
    await page.locator('nav [data-tab=sources]').click()
    await ready
    await page.locator('#sourceStatsWindow').selectOption('24h')
    await page.waitForFunction(() => document.querySelector('.source-call-metric strong')?.textContent === '40')
    release()
    await page.waitForFunction(() => !document.querySelector('#refreshSourceStats').disabled)
    assert.equal(await page.locator('#sourceStatsWindow').inputValue(), '24h')
    assert.equal(await page.locator('[data-source-period]').count(), 24)
    assert.equal(await page.locator('.source-call-metric strong').first().textContent(), '40')
    fail = true
    await page.locator('#refreshSourceStats').click()
    await page.waitForFunction(() => document.querySelector('#sourceStatsStatus').textContent.includes('仍显示上次数据'))
    assert.equal(await page.locator('.source-call-metric strong').first().textContent(), '40')
    fail = false
    await page.locator('#refreshSourceStats').click()
    await page.waitForFunction(() => document.querySelector('#sourceStatsStatus').textContent.startsWith('更新于'))
    await page.locator('#sourceStatsRecent summary').click()
    assert.match(await page.locator('#sourceStatsRecent').textContent(), /合成测试歌曲/)
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    assert.equal(await page.locator('#sourceStatsBody').textContent(), '')
    await login(page, url, 'alice')
    assert.equal(await page.locator('nav [data-tab=sources]').isVisible(), false)
    assert.equal(await page.locator('#sourceStatsBody').textContent(), '')
  })

  await check('音源调用统计：手机深色布局与空样本', async page => {
    await asAdmin(page)
    let calls = 0
    await page.route('**/api/admin/sources/statistics?*', route => route.fulfill({ json: statistics('1h', calls) }))
    await page.locator('nav [data-tab=sources]').click()
    await page.locator('#sourceStatsTable').waitFor()
    assert.match(await page.locator('#sourceStatsTable').textContent(), /暂无有效样本/)
    assert.equal(await page.locator('.source-call-metric strong').nth(1).textContent(), '—')
    assert.match(await page.locator('#sourceStatsPeriod').textContent(), /暂无调用/)
    calls = 10
    await page.locator('#refreshSourceStats').click()
    await page.waitForFunction(() => document.querySelector('.source-call-metric strong')?.textContent === '10')
    await page.locator('#sourceStatsGroups summary').click()
    await page.locator('#sourceStatsRecent summary').click()
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    await page.locator('#sourceStatsPanel').screenshot({ path: path.join(artifacts, 'source-statistics-mobile-dark.png'), animations: 'disabled' })
  }, { viewport: { width: 390, height: 844 }, colorScheme: 'dark', isMobile: true, hasTouch: true })
}
