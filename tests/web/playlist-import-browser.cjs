// 隔离浏览器验收：通过合成 SDK 走真实导入接口，不访问外部平台。
const assert = require('node:assert/strict')

module.exports = async ({ check, url, holdResponse, createPlaylist, selectPlaylistUI }) => {
  async function openImport(page, source, input) {
    await page.locator('nav [data-tab=playlists]').click()
    await page.locator('#importForm').evaluate(form => { form.closest('details').open = true })
    await page.locator('#importForm [name=source]').selectOption(source)
    if (source !== 'lx') await page.locator('#importForm [name=input]').fill(input)
  }
  async function preview(page) {
    await page.locator('#previewOnlineImport').click()
    await page.waitForFunction(() => !document.querySelector('#importForm button[type=submit]').disabled)
  }
  async function submit(page) {
    const response = page.waitForResponse(response => response.url().includes('/api/app/playlists/import') && response.request().method() === 'POST')
    await page.locator('#importForm [type=submit]').click()
    const result = await response
    assert.equal(result.status(), 200)
    await page.waitForFunction(() => !document.querySelector('#importForm').inert)
    return result.json()
  }

  for (const source of ['wy', 'tx']) {
    await check(`${source} 在线歌单预览、新建与播放元数据`, async page => {
      const input = source === 'wy' ? '分享歌单 https://music.163.com/#/playlist?id=123' : 'https://i.y.qq.com/n2/m/share/details/taoge.html?id=123'
      await openImport(page, source, input)
      assert.equal(await page.locator('#importForm [name=file]').isDisabled(), true)
      assert.equal(await page.locator('#importForm [type=submit]').isDisabled(), true)
      await preview(page)
      assert.match(await page.locator('#importLists').innerText(), new RegExp(source + ' 在线歌单 123'))
      const result = await submit(page)
      const id = result.playlists[0].id
      assert.equal(result.added, 3)
      await page.waitForFunction(id => playlistState.current?.id === id, id)
      assert.equal(await page.locator('#trackTable img').count(), 0)
      assert.deepEqual(await page.evaluate(() => playlistState.draftTracks.map(track => track.source)), [source, source, source])
      assert.equal(await page.locator('#importForm [name=source]').inputValue(), 'lx')
      assert.equal(await page.locator('#importForm [type=submit]').isDisabled(), true)
      const detail = await (await page.request.get(url + '/api/app/playlists/' + id)).json()
      assert.equal(detail.songCount, 3)
      assert.match(detail.comment, source === 'wy' ? /music\.163\.com/ : /y\.qq\.com/)
    })
  }

  await check('在线歌单追加保留顺序并去重，错误链接可重试', async page => {
    const target = await createPlaylist(page, url, '在线导入追加测试')
    await selectPlaylistUI(page, target.id)
    await openImport(page, 'wy', 'http://127.0.0.1/playlist?id=123')
    await page.locator('#previewOnlineImport').click()
    await page.waitForFunction(() => document.querySelector('#importStatus').textContent.includes('完整歌单链接'))
    assert.equal(await page.locator('#importForm [type=submit]').isDisabled(), true)
    await page.locator('#importForm [name=input]').fill('456')
    await preview(page)
    await page.locator('#importForm [name=mode]').selectOption('append')
    const result = await submit(page)
    assert.equal(result.added, 2)
    assert.equal(result.playlists[0].id, target.id)
    await page.waitForFunction(() => playlistState.draftTracks.length === 3)
    assert.deepEqual(await page.evaluate(() => playlistState.draftTracks.map(track => track.id)), ['tr-wy-1', 'tr-wy-2', 'tr-wy-3'])
  })

  await check('切换来源或修改链接后，迟到预览不能恢复导入按钮', async page => {
    await openImport(page, 'wy', '888')
    const held = await holdResponse(page, '**/api/app/playlists/import/online', 'POST', request => request.postDataJSON().preview)
    await page.locator('#previewOnlineImport').click()
    await held.ready
    await page.locator('#importForm [name=source]').selectOption('tx')
    await page.locator('#importForm [name=input]').fill('999')
    await held.finish()
    assert.equal(await page.locator('#importForm [type=submit]').isDisabled(), true)
    assert.equal(await page.locator('#importLists').isVisible(), false)
    await preview(page)
    assert.match(await page.locator('#importLists').innerText(), /tx 在线歌单 999/)
    await page.locator('#importForm [name=input]').fill('1000')
    assert.equal(await page.locator('#importForm [type=submit]').isDisabled(), true)
  })

  await check('导入期间新产生的歌单草稿不能被刷新覆盖', async page => {
    const target = await createPlaylist(page, url, '导入草稿保护')
    await selectPlaylistUI(page, target.id)
    await openImport(page, 'tx', '555')
    await preview(page)
    const held = await holdResponse(page, '**/api/app/playlists/import/online', 'POST', request => !request.postDataJSON().preview)
    await page.locator('#importForm [type=submit]').click()
    await held.ready
    await page.locator('#metaForm [name=name]').fill('导入期间的新草稿')
    await held.finish()
    await page.waitForFunction(() => !document.querySelector('#importForm').inert)
    assert.equal(await page.evaluate(() => playlistState.current.id), target.id)
    assert.equal(await page.evaluate(() => playlistState.metaDirty), true)
    assert.equal(await page.locator('#metaForm [name=name]').inputValue(), '导入期间的新草稿')
  })

  await check('保留洛雪文件预览与导入', async page => {
    await openImport(page, 'lx')
    await page.locator('#importForm [name=file]').setInputFiles({
      name: '歌单.json', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify({ type: 'playList_v2', data: [{ id: 'default', name: '文件导入回归', list: [{ source: 'wy', songmid: '1', name: '测试歌曲一' }] }] })),
    })
    await page.waitForFunction(() => !document.querySelector('#importForm button[type=submit]').disabled)
    assert.equal(await page.locator('#importForm [name=input]').isDisabled(), true)
    const result = await submit(page)
    assert.equal(result.added, 1)
    assert.equal(result.playlists[0].name, '文件导入回归')
  })
}
