const assert = require('node:assert/strict')
const http = require('node:http')
const path = require('node:path')

const script = version => `// @name ${version === 1 ? '自动更新验收' : '源端修改的名称'}
// @version ${version}
// @description <img src=x onerror=alert(1)>
lx.on(lx.EVENT_NAMES.request,()=>Promise.resolve('https://media.invalid/version-${version}'));
lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['320k']}}});`

module.exports = async ({ check, url, login, artifacts }) => {
  await check('音源 URL 导入、手动更新、失败保留和更新开关', async page => {
    let content = script(1)
    const upstream = http.createServer((request, response) => { response.writeHead(200, { 'Content-Type': 'text/javascript' }); response.end(content) })
    await new Promise(resolve => upstream.listen(0, '127.0.0.1', resolve))
    const address = `http://127.0.0.1:${upstream.address().port}/source.js`
    let id
    try {
      await page.locator('#logoutButton').click()
      await page.locator('#login:not(.hidden)').waitFor()
      await login(page, url, 'admin')
      await page.locator('nav [data-tab=sources]').click()
      assert.equal(await page.locator('#importSourceForm [name=autoUpdate]').isChecked(), true)
      await page.locator('#importSourceForm [name=url]').fill(address)
      const imported = page.waitForResponse(response => new URL(response.url()).pathname === '/api/admin/sources/import')
      await page.locator('#importSourceForm button').click()
      const source = await (await imported).json()
      id = source.id
      assert.equal(source.url, address)
      assert.equal(source.autoUpdate, true)
      const state = page.locator(`[data-source-update="${id}"]`)
      await state.waitFor()
      assert.match(await state.textContent(), /每日自动更新已开启/)
      assert.match(await state.textContent(), /已导入/)
      assert.equal(await page.locator('#sourceList img').count(), 0)
      const checkButton = page.locator(`[data-source-action=check][data-source-id="${id}"]`)
      content = script(2)
      let checked = page.waitForResponse(response => new URL(response.url()).pathname === `/api/admin/sources/${id}/check`)
      await checkButton.click()
      const updated = await (await checked).json()
      assert.equal(updated.updateResult, 'updated')
      assert.equal(updated.name, source.name)
      await page.waitForFunction(id => document.querySelector(`[data-source-version="${id}"]`)?.textContent === 'v2', id)
      assert.match(await state.textContent(), /已更新/)
      content = `throw new Error('https://private.invalid/?token=secret');`
      checked = page.waitForResponse(response => new URL(response.url()).pathname === `/api/admin/sources/${id}/check`)
      await checkButton.click()
      assert.equal((await checked).status(), 502)
      await page.waitForFunction(id => document.querySelector(`[data-source-update="${id}"]`)?.textContent.includes('更新失败'), id)
      assert.equal(await page.locator(`[data-source-version="${id}"]`).textContent(), 'v2')
      assert.doesNotMatch(await state.textContent(), /private\.invalid|token=secret/)
      await page.locator(`[data-source-action=update-settings][data-source-id="${id}"]`).click()
      assert.equal(await page.locator('#sourceUpdateForm [name=url]').inputValue(), address)
      await page.locator('#sourceUpdateForm [name=autoUpdate]').uncheck()
      const saved = page.waitForResponse(response => new URL(response.url()).pathname === `/api/admin/sources/${id}` && response.request().method() === 'PUT')
      await page.locator('#saveSourceUpdate').click()
      assert.equal((await saved).status(), 200)
      await page.locator('#sourceUpdateDialog').waitFor({ state: 'hidden' })
      await page.waitForFunction(id => document.querySelector(`[data-source-update="${id}"]`)?.textContent.includes('已关闭'), id)
      assert.equal(await page.locator('#sourceUpdateForm [name=url]').inputValue(), '')
      await state.screenshot({ path: path.join(artifacts, 'source-update-desktop.png'), animations: 'disabled' })
      await page.locator(`[data-source-action=toggle][data-source-id="${id}"]`).click()
      await page.waitForFunction(id => document.querySelector(`[data-source-badge="${id}"]`)?.textContent === '已停用', id)
      content = script(3)
      checked = page.waitForResponse(response => new URL(response.url()).pathname === `/api/admin/sources/${id}/check`)
      await checkButton.click()
      const inactive = await (await checked).json()
      assert.equal(inactive.enabled, false)
      assert.equal(inactive.status, null)
      assert.equal(inactive.version, '3')
    } finally {
      if (id) await page.request.delete(url + '/api/admin/sources/' + id)
      await new Promise(resolve => upstream.close(resolve))
    }
  })

  await check('音源更新：后台刷新保留编辑与日志，退出清空来源', async page => {
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, 'admin')
    let version = '1'
    await page.route('**/api/admin/sources', route => route.fulfill({ json: [{
      id: 700, name: '合成更新音源', version, enabled: true, url: 'https://source.invalid/original.js?key=synthetic', autoUpdate: true,
      lastCheckedAt: 1794358800, scriptUpdatedAt: 1794358800, updateResult: 'unchanged',
      status: { state: 'ready', version, platforms: { wy: { qualitys: ['320k'] } }, logs: ['合成日志'] },
    }] }))
    await page.clock.install()
    await page.locator('nav [data-tab=sources]').click()
    await page.locator('[data-source-update="700"]').waitFor()
    const details = page.locator('#sourceList details')
    await details.locator('summary').click()
    await page.locator('[data-source-action=update-settings][data-source-id="700"]').click()
    await page.locator('#sourceUpdateForm [name=url]').fill('https://source.invalid/edited.js')
    await page.locator('#sourceUpdateForm [name=autoUpdate]').uncheck()
    version = '2'
    const refreshed = page.waitForResponse(response => new URL(response.url()).pathname === '/api/admin/sources')
    await page.clock.fastForward(31000)
    await refreshed
    await page.waitForFunction(() => document.querySelector('[data-source-version="700"]')?.textContent === 'v2')
    assert.equal(await page.locator('#sourceUpdateForm [name=url]').inputValue(), 'https://source.invalid/edited.js')
    assert.equal(await page.locator('#sourceUpdateForm [name=autoUpdate]').isChecked(), false)
    assert.equal(await details.getAttribute('open'), '')
    await page.locator('#closeSourceUpdate').click()
    await page.locator('[data-source-action=update-settings][data-source-id="700"]').click()
    await page.locator('#sourceUpdateForm [name=url]').fill('https://source.invalid/unsaved.js')
    await page.keyboard.press('Escape')
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    assert.equal(await page.locator('#sourceUpdateForm [name=url]').inputValue(), '')
    assert.equal(await page.locator('#sourceUpdateName').textContent(), '')
    assert.equal(await page.locator('#sourceUpdateDialog').isVisible(), false)
  })

  await check('音源更新：手机深色布局与来源必填提示', async page => {
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, 'admin')
    await page.locator('nav [data-tab=sources]').click()
    await page.locator('[data-source-action=update-settings]').first().click()
    await page.locator('#sourceUpdateForm [name=url]').fill('')
    await page.locator('#sourceUpdateForm [name=autoUpdate]').check()
    await page.locator('#saveSourceUpdate').click()
    assert.match(await page.locator('#sourceUpdateError').textContent(), /先填写来源/)
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    await page.locator('#sourceUpdateDialog').screenshot({ path: path.join(artifacts, 'source-update-mobile-dark.png'), animations: 'disabled' })
  }, { viewport: { width: 390, height: 844 }, colorScheme: 'dark', isMobile: true, hasTouch: true })
}
