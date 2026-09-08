// 真实浏览器验收；仅启动隔离测试服务，不使用生产账号、数据库或外部音源。
const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const { spawn, execFileSync } = require('node:child_process')
const { createInterface } = require('node:readline')
const { once } = require('node:events')
const { chromium, request: browserRequest } = require(process.env.LXSC_PLAYWRIGHT_MODULE || 'playwright')

const root = path.resolve(__dirname, '../..')
const artifacts = process.env.LXSC_BROWSER_ARTIFACT_DIR || fs.mkdtempSync(path.join(os.tmpdir(), 'lxsc-browser-results-'))
fs.mkdirSync(artifacts, { recursive: true })
function deferred() {
  let resolve
  const promise = new Promise(yes => { resolve = yes })
  return { promise, resolve }
}
async function settle(page) { await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))) }
async function login(page, url, user = 'alice') {
  await page.goto(url + '/#search')
  await page.locator('#login:not(.hidden)').waitFor()
  await page.locator('#loginForm [name=username]').fill(user)
  await page.locator('#loginForm [name=password]').fill('test-password')
  await page.locator('#loginForm button').click()
  await page.locator('#app:not(.hidden)').waitFor()
  await page.locator('#tab-search.active').waitFor()
}
async function search(page, query = '听歌') {
  await page.locator('nav [data-tab=search]').click()
  await page.locator('#songSearchForm [name=query]').fill(query)
  await page.locator('#songSearchForm [name=source]').selectOption('wy')
  await page.locator('#songSearchForm button').click()
  await page.waitForFunction(() => document.querySelectorAll('#searchTable [data-song-action=play]').length === 3)
}
async function playSearch(page, index = 0) {
  await page.locator(`#searchTable [data-song-action=play][data-song-index="${index}"]`).click()
  await page.waitForFunction(() => { const audio = document.querySelector('#webAudio'); return !audio.paused && audio.currentTime > 0 && audio.duration > 40 })
}
async function createPlaylist(page, url, name, ids = ['tr-wy-1']) {
  const response = await page.request.post(url + '/api/app/playlists', { data: { name, trackIds: ids, public: false } })
  assert.equal(response.status(), 200)
  return response.json()
}
async function selectPlaylistUI(page, id) {
  await page.locator('nav [data-tab=playlists]').click()
  await page.locator(`[data-playlist-id="${id}"]`).waitFor()
  await page.locator(`[data-playlist-id="${id}"]`).click()
  await page.waitForFunction(id => playlistState.current?.id === id, id)
}
async function adminSettings(page, url) {
  await page.locator('#logoutButton').click()
  await page.locator('#login:not(.hidden)').waitFor()
  await login(page, url, 'admin')
  await page.locator('nav [data-tab=settings]').click()
  await page.locator('#settingsForm:not([inert])').waitFor()
}
async function showBoardSettings(page) {
  await page.locator('nav [data-tab=playlists]').click()
  const panel = page.locator('#boardSettingsPanel')
  await panel.waitFor()
  if (!await panel.evaluate(element => element.open)) await panel.locator(':scope > summary').click()
  await page.locator('#boardSettingsForm:not([inert])').waitFor()
}
async function adminBoardSettings(page, url) {
  await page.locator('#logoutButton').click()
  await page.locator('#login:not(.hidden)').waitFor()
  await login(page, url, 'admin')
  await showBoardSettings(page)
}
async function boardGroup(page, source) {
  const group = page.locator(`[data-board-source="${source}"]`)
  await group.waitFor()
  if (!await group.evaluate(element => element.open)) await group.locator('summary').click()
  return group
}
async function saveSettingsUI(page, status = 200) {
  const saved = page.waitForResponse(response => response.url().endsWith('/api/admin/settings') && response.request().method() === 'PUT')
  await page.locator('#settingsForm button[type=submit]').click()
  const response = await saved
  assert.equal(response.status(), status)
  for (const field of ['showBoards', 'boardSources', 'boardSelections']) assert.equal(Object.hasOwn(response.request().postDataJSON(), field), false, '系统设置不能回写榜单字段')
  await page.waitForFunction(() => !settingsState.saving)
  return response.json()
}
async function saveBoardSettingsUI(page, status = 200) {
  const saved = page.waitForResponse(response => response.url().endsWith('/api/admin/settings') && response.request().method() === 'PUT' && Object.hasOwn(response.request().postDataJSON(), 'boardSelections'))
  await page.locator('#boardSettingsForm button[type=submit]').click()
  const response = await saved
  assert.equal(response.status(), status)
  assert.deepEqual(Object.keys(response.request().postDataJSON()).sort(), ['boardSelections', 'boardSources', 'showBoards'], '榜单保存只能更新自己的字段')
  await page.waitForFunction(() => !boardSettingsState.saving)
  return response.json()
}
async function draftSong(page) {
  await page.locator('#searchForm [name=query]').fill('草稿')
  await page.locator('#searchForm [name=source]').selectOption('wy')
  await page.locator('#searchForm button').click()
  await page.locator('#searchResults [data-add-track="tr-wy-2"]').click()
  await page.locator('#searchResults [data-play-playlist-search="2"]').click()
}
async function holdResponse(page, pattern, method = 'GET', match = () => true) {
  const ready = deferred(), release = deferred(), done = deferred()
  let held = false
  await page.route(pattern, async route => {
    if (held || route.request().method() !== method || !match(route.request())) return route.continue()
    held = true
    try {
      const response = await route.fetch()
      ready.resolve(response)
      await release.promise
      await route.fulfill({ response })
    } catch { /* 正确的取消行为可能使等待中的响应无法再发送。 */ }
    finally { done.resolve() }
  })
  return { ready: ready.promise, finish: async () => { release.resolve(); await done.promise; await settle(page) } }
}

async function main() {
  const buildDir = fs.mkdtempSync(path.join(os.tmpdir(), 'lxsc-browser-build-'))
  const binary = path.join(buildDir, 'fixture')
  execFileSync('go', ['build', '-o', binary, './tests/web/fixture'], { cwd: root, stdio: 'inherit' })
  const fixture = spawn(binary, [], { cwd: root, stdio: ['ignore', 'pipe', 'pipe'] })
  let stderr = ''
  fixture.stderr.on('data', data => { stderr = (stderr + data).slice(-16000) })
  let browser, admin
  const results = []
  try {
    const info = await new Promise((resolve, reject) => {
      const lines = createInterface({ input: fixture.stdout })
      const timeout = setTimeout(() => reject(new Error('测试服务启动超时：' + stderr)), 30000)
      fixture.once('error', reject)
      fixture.once('exit', code => { clearTimeout(timeout); reject(new Error('测试服务提前退出：' + code + ' ' + stderr)) })
      lines.on('line', line => {
        try { const info = JSON.parse(line); if (info.url) { clearTimeout(timeout); lines.close(); resolve(info) } } catch { /* 忽略非地址行。 */ }
      })
    })
    const url = info.url
    browser = await chromium.launch({ headless: true, executablePath: process.env.LXSC_CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
    admin = await browserRequest.newContext()
    assert.equal((await admin.post(url + '/api/auth/login', { data: { username: 'admin', password: 'test-password' } })).status(), 200)
    async function check(name, action, options = {}) {
      const context = await browser.newContext({ viewport: { width: 1360, height: 900 }, ...options })
      const page = await context.newPage(), errors = []
      page.setDefaultTimeout(8000)
      page.on('pageerror', error => errors.push(error.message))
      try {
        await login(page, url)
        await action(page)
        await settle(page)
        assert.deepEqual(errors, [], '页面不应出现未处理异常')
        results.push({ name, ok: true })
        console.log('通过：' + name)
      } catch (error) {
        results.push({ name, ok: false, error: error.message, pageErrors: errors })
        console.error('失败：' + name + '\n' + error.stack)
        await page.screenshot({ path: path.join(artifacts, 'failure-' + results.length + '.png'), fullPage: true }).catch(() => {})
      } finally {
        await context.close()
      }
    }

    await require('./listening-browser.cjs')({ check, url, artifacts, login, search, playSearch, holdResponse })

    await check('普通用户搜歌、真实播放、队列和导航', async page => {
      const boardRequests = []
      page.on('request', request => { if (request.url().includes('/api/admin/boards')) boardRequests.push(request.url()) })
      assert.equal(await page.locator('nav .admin-only:visible').count(), 0)
      assert.equal((await page.request.get(url + '/api/admin/settings')).status(), 403)
      await search(page)
      assert.equal(await page.locator('#searchTable img').count(), 0)
      assert.match(await page.locator('#searchTable').innerText(), /<img src=x onerror=alert\(1\)>/)
      await playSearch(page)
      assert.equal(await page.locator('audio').count(), 1)
      await page.evaluate(() => { window.originalAudio = document.querySelector('#webAudio') })
      await page.locator('#playerNext').click()
      await page.waitForFunction(() => webPlayer.index === 1 && webPlayer.status === 'playing')
      await page.locator('#playerPrevious').click()
      await page.waitForFunction(() => webPlayer.index === 0 && webPlayer.status === 'playing')
      await page.locator('nav [data-tab=playlists]').click()
      assert.equal(await page.locator('#boardSettingsPanel').isVisible(), false)
      assert.deepEqual(boardRequests, [], '普通用户进入歌单页不能加载管理员榜单目录')
      await search(page, '新搜索')
      assert.equal(await page.evaluate(() => document.querySelector('#webAudio') === window.originalAudio), true)
      assert.match(await page.locator('#playerTitle').innerText(), /^听歌/)
      await page.locator('#playerToggle').click()
      assert.equal(await page.evaluate(() => document.querySelector('#webAudio').paused), true)
      await page.locator('#playerToggle').click()
      await page.waitForFunction(() => !document.querySelector('#webAudio').paused)
      await page.locator('#playerVolume').evaluate(input => { input.value = '0.25'; input.dispatchEvent(new Event('input', { bubbles: true })) })
      assert.equal(await page.evaluate(() => document.querySelector('#webAudio').volume), 0.25)
      await page.locator('#playerMute').click()
      assert.equal(await page.evaluate(() => document.querySelector('#webAudio').muted), true)
      await page.screenshot({ path: path.join(artifacts, 'desktop.png'), fullPage: true })
      await page.evaluate(() => webPlayer.seek(webPlayer.duration - 0.15))
      await page.waitForFunction(() => webPlayer.index === 1 && webPlayer.status === 'playing')
      await page.locator('#playerNext').click()
      await page.waitForFunction(() => webPlayer.duration > 40)
      await page.evaluate(() => webPlayer.seek(webPlayer.duration - 0.15))
      await page.waitForFunction(() => webPlayer.status === 'ended')
      assert.equal(await page.locator('#playerNext').isDisabled(), true)
      await page.locator('#logoutButton').click()
      await page.locator('#login:not(.hidden)').waitFor()
      assert.deepEqual(await page.evaluate(() => [webPlayer.queue.length, document.querySelector('#webAudio').getAttribute('src'), songSearchState.tracks.length]), [0, null, 0])
    })

    await check('拖动进度条后即使保留焦点仍跟随真实播放', async page => {
      await search(page)
      await playSearch(page)
      const seek = page.locator('#playerSeek'), box = await seek.boundingBox()
      await seek.click({ position: { x: box.width / 4, y: box.height / 2 } })
      assert.equal(await seek.evaluate(input => document.activeElement === input), true)
      const before = Number(await seek.inputValue())
      await page.waitForFunction(before => Number(document.querySelector('#playerSeek').value) > before + 0.5, before, { timeout: 2500 })
    })

    await check('立即收藏、去重、新建和公开歌单只读播放', async page => {
      const target = await createPlaylist(page, url, '收藏目标')
      await search(page, '收藏')
      const collect = page.locator('#searchTable [data-song-action=collect][data-song-index="1"]')
      await collect.click()
      await page.locator('#collectForm [name=playlistId] option[value="' + target.id + '"]').waitFor({ state: 'attached' })
      assert.equal(await page.locator('#collectForm option[value="pl-bob-public"]').count(), 0)
      await page.locator('#collectForm [name=playlistId]').selectOption(target.id)
      await page.locator('#collectSubmit').click()
      await page.locator('#collectDialog').waitFor({ state: 'hidden' })
      let detail = await (await page.request.get(url + '/api/app/playlists/' + target.id)).json()
      assert.deepEqual(detail.tracks.map(track => track.id), ['tr-wy-2', 'tr-wy-1'])
      await collect.click()
      await page.locator('#collectForm [name=playlistId]').selectOption(target.id)
      await page.locator('#collectSubmit').click()
      await page.locator('#collectDialog').waitFor({ state: 'hidden' })
      assert.match(await page.locator('#toast').innerText(), /已在/)
      detail = await (await page.request.get(url + '/api/app/playlists/' + target.id)).json()
      assert.equal(detail.songCount, 2)
      await collect.click()
      await page.locator('#collectForm [name=mode]').selectOption('create')
      await page.locator('#collectForm [name=name]').fill('弹窗新建')
      await page.locator('#collectSubmit').click()
      await page.locator('#collectDialog').waitFor({ state: 'hidden' })
      const lists = await (await page.request.get(url + '/api/app/playlists')).json()
      assert.equal(lists.find(item => item.name === '弹窗新建').songCount, 1)
      await page.locator('#searchQualitySelect').selectOption('128k')
      await page.waitForFunction(() => sessionState.me.quality === '128k')
      await selectPlaylistUI(page, 'pl-bob-public')
      assert.equal(await page.locator('#qualitySelect').inputValue(), '128k')
      assert.equal(await page.locator('#deleteButton').isVisible(), false)
      assert.equal(await page.locator('#searchSection').isVisible(), false)
      assert.equal(await page.locator('#trackTable [data-track-action=play]').nth(1).isDisabled(), true)
      await page.locator('#trackTable [data-track-action=play]').first().click()
      await page.waitForFunction(() => webPlayer.status === 'playing')
    })

    await check('草稿保护和其他页面收藏引起的409不丢数据', async page => {
      const target = await createPlaylist(page, url, '草稿保护')
      await selectPlaylistUI(page, target.id)
      await draftSong(page)
      await page.locator('#playerCollect').click()
      await page.locator('#collectForm [name=playlistId]').selectOption(target.id)
      assert.equal(await page.locator('#collectSubmit').isDisabled(), true)
      await page.locator('#collectForm [name=playlistId]').selectOption('pl-alice')
      assert.equal(await page.locator('#collectSubmit').isEnabled(), true)
      await page.locator('#collectCancel').click()
      assert.equal((await page.request.post(url + '/api/app/playlists/' + target.id + '/tracks', { data: { trackId: 'tr-wy-3' } })).status(), 200)
      await page.locator('#saveTracksButton').click()
      await page.waitForFunction(() => document.querySelector('#toast').textContent.includes('其他页面'))
      assert.equal(await page.evaluate(() => playlistState.tracksDirty), true)
      const stored = await (await page.request.get(url + '/api/app/playlists/' + target.id)).json()
      assert.equal(stored.tracks[0].id, 'tr-wy-3')
      assert.deepEqual(await page.evaluate(() => playlistState.draftTracks.map(track => track.id)), ['tr-wy-2', 'tr-wy-1'])
    })

    await check('收藏完成后旧详情不能覆盖新状态和新草稿', async page => {
      const target = await createPlaylist(page, url, '旧详情竞态')
      await search(page)
      await playSearch(page, 1)
      await selectPlaylistUI(page, target.id)
      const held = await holdResponse(page, '**/api/app/playlists/' + target.id)
      await page.locator('#refreshButton').click()
      await held.ready
      await page.locator('#playerCollect').click()
      await page.locator('#collectForm [name=playlistId]').selectOption(target.id)
      await page.locator('#collectSubmit').click()
      await page.locator('#collectDialog').waitFor({ state: 'hidden' })
      await page.locator('#metaForm [name=name]').fill('仍未保存的新名称')
      await held.finish()
      const state = await page.evaluate(() => ({ ids: playlistState.draftTracks.map(track => track.id), dirty: playlistState.metaDirty, name: document.querySelector('#metaForm [name=name]').value }))
      assert.deepEqual(state, { ids: ['tr-wy-2', 'tr-wy-1'], dirty: true, name: '仍未保存的新名称' })
    })

    await check('保存完成时同步解除已打开收藏弹窗的草稿阻止', async page => {
      const target = await createPlaylist(page, url, '弹窗同步')
      await selectPlaylistUI(page, target.id)
      await draftSong(page)
      const held = await holdResponse(page, '**/api/app/playlists/' + target.id + '/tracks', 'PUT')
      await page.locator('#saveTracksButton').click()
      await held.ready
      await page.locator('#playerCollect').click()
      await page.locator('#collectForm [name=playlistId]').selectOption(target.id)
      assert.equal(await page.locator('#collectSubmit').isDisabled(), true)
      await held.finish()
      await page.waitForFunction(() => !playlistState.tracksDirty)
      await page.waitForFunction(() => !document.querySelector('#collectSubmit').disabled, null, { timeout: 2000 })
    })

    await check('保存途中续写草稿不采纳其他收藏版本，信息保存也不改变基线', async page => {
      const target = await createPlaylist(page, url, '续写版本')
      await selectPlaylistUI(page, target.id)
      await draftSong(page)
      const held = await holdResponse(page, '**/api/app/playlists/' + target.id + '/tracks', 'PUT')
      await page.locator('#saveTracksButton').click()
      const receipt = await (await held.ready).json()
      assert.deepEqual(receipt.tracks.map(track => track.id), ['tr-wy-2', 'tr-wy-1'])
      await page.locator('#searchResults [data-add-track="tr-wy-3"]').click()
      assert.equal((await page.request.post(url + '/api/app/playlists/' + target.id + '/tracks', { data: { trackId: 'tr-tx-2' } })).status(), 200)
      await held.finish()
      assert.equal(await page.evaluate(() => playlistState.tracksDirty), true)
      assert.equal(await page.evaluate(() => playlistState.draftRevision), receipt.tracksRevision)
      await page.locator('#metaForm [name=name]').fill('续写版本的新名称')
      await page.locator('#metaForm button[type=submit]').click()
      await page.waitForFunction(() => !playlistState.metaDirty)
      assert.equal(await page.evaluate(() => playlistState.draftRevision), receipt.tracksRevision)
      await page.locator('#saveTracksButton').click()
      await page.waitForFunction(() => document.querySelector('#toast').textContent.includes('其他页面'))
      const stored = await (await page.request.get(url + '/api/app/playlists/' + target.id)).json()
      assert.deepEqual(stored.tracks.map(track => track.id), ['tr-tx-2', 'tr-wy-2', 'tr-wy-1'])
      assert.deepEqual(await page.evaluate(() => playlistState.draftTracks.map(track => track.id)), ['tr-wy-3', 'tr-wy-2', 'tr-wy-1'])
    })

    await check('详情等待期间的新编辑不会被旧读取清除', async page => {
      const target = await createPlaylist(page, url, '等待读取')
      await selectPlaylistUI(page, target.id)
      const held = await holdResponse(page, '**/api/app/playlists/' + target.id)
      await page.locator('#refreshButton').click()
      await held.ready
      await page.locator('#metaForm [name=name]').fill('等待期间修改')
      await held.finish()
      assert.equal(await page.evaluate(() => playlistState.metaDirty), true)
      assert.equal(await page.locator('#metaForm [name=name]').inputValue(), '等待期间修改')
      assert.notEqual(await page.locator('#toast').innerText(), '已刷新')
    })

    await check('直连失败停下且不自动代理，失效会话释放播放器', async page => {
      const streams = []
      page.on('request', request => { if (request.url().includes('/api/app/stream?')) streams.push(request.url()) })
      // 由真实测试上游返回 403；不依赖浏览器对重定向后请求的路由拦截。
      await search(page, '播放失败')
      assert.equal(await page.evaluate(() => songSearchState.tracks[0].id), 'tr-wy-fail-1')
      await page.locator('#searchTable [data-song-action=play]').first().click()
      await page.waitForFunction(() => webPlayer.status === 'error')
      await settle(page)
      assert.equal(streams.length, 1)
      assert.equal(streams[0].includes('proxy='), false)
      assert.equal(await page.evaluate(() => webPlayer.index), 0)
      assert.equal((await page.request.post(url + '/api/auth/logout')).status(), 200)
      await page.locator('#playerToggle').click()
      await page.locator('#login:not(.hidden)').waitFor()
      assert.equal(await page.evaluate(() => webPlayer.queue.length), 0)
      assert.equal(await page.locator('#playerBar').isVisible(), false)
    })

    await check('空白新搜索取消旧结果', async page => {
      const held = await holdResponse(page, '**/api/app/search', 'POST', request => request.postDataJSON().query === '慢')
      await page.locator('#songSearchForm [name=query]').fill('慢')
      await page.locator('#songSearchForm [name=source]').selectOption('wy')
      await page.locator('#songSearchForm button').click()
      await held.ready
      await page.locator('#songSearchForm [name=query]').fill('   ')
      await page.locator('#songSearchForm button').click()
      await held.finish()
      assert.match(await page.locator('#searchTable tbody').innerText(), /请输入/)
      assert.equal(await page.locator('#searchTable [data-song-action]').count(), 0)
    })

    await check('退出取消旧详情且不在登录页弹出取消错误', async page => {
      await page.locator('nav [data-tab=playlists]').click()
      const held = await holdResponse(page, '**/api/app/playlists/pl-alice')
      await page.locator('[data-playlist-id="pl-alice"]').click()
      await held.ready
      await page.locator('#logoutButton').click()
      await page.locator('#login:not(.hidden)').waitFor()
      await held.finish()
      assert.equal(await page.locator('#toast').isVisible(), false)
      assert.equal(await page.locator('#playerBar').isVisible(), false)
    })

    await check('榜单勾选保存、刷新及两处展示一致，关闭或移除平台保留选择', async page => {
      assert.equal((await admin.put(url + '/api/admin/settings', { data: { showBoards: true, boardSources: ['tx', 'wy', 'kg'], boardSelections: {} } })).status(), 200)
      await adminBoardSettings(page, url)
      assert.equal(await page.locator('#tab-playlists #boardSettingsForm').count(), 1)
      assert.equal(await page.locator('#tab-settings [name=showBoards], #tab-settings #boardSelections').count(), 0)
      for (const [source, selected] of [['tx', 'hot'], ['wy', 'new'], ['kg', null]]) {
        const group = await boardGroup(page, source)
        await group.locator('[data-board-mode] option[value=custom]:not([disabled])').waitFor({ state: 'attached' })
        await group.locator('[data-board-mode]').selectOption('custom')
        await group.locator('[data-board-action=clear]').click()
        if (selected) await group.locator(`[data-board-id="${selected}"]`).check()
      }
      assert.equal(await page.locator('#boardSelections img').count(), 0, '榜单名称必须按文本转义')
      const wanted = { tx: ['hot'], wy: ['new'], kg: [] }
      assert.deepEqual((await saveBoardSettingsUI(page)).boardSelections, wanted)
      async function client(path) {
        const response = await page.request.get(url + '/rest/' + path + '&f=json&u=alice&p=test-password')
        const result = (await response.json())['subsonic-response']
        assert.equal(result.status, 'ok')
        return result
      }
      let root = (await client('getMusicDirectory.view?id=1')).directory
      assert.deepEqual(root.child.filter(item => item.id.startsWith('dir-')).map(item => item.id), ['dir-tx', 'dir-wy'])
      const playlistIDs = (await client('getPlaylists.view?')).playlists.playlist.filter(item => item.id.startsWith('lb-')).map(item => item.id)
      assert.deepEqual(playlistIDs, ['lb-tx-hot', 'lb-wy-new'])
      assert.deepEqual((await client('getMusicDirectory.view?id=dir-wy')).directory.child.map(item => item.id), ['lb-wy-new'])
      await page.reload()
      await showBoardSettings(page)
      assert.equal(await page.locator('[data-board-source=wy] [data-board-id=new]').isChecked(), true)
      assert.deepEqual(await page.evaluate(() => boardChoices.snapshot()), wanted)
      await page.locator('#boardSettingsPanel').screenshot({ path: path.join(artifacts, 'board-settings-desktop.png') })
      await page.locator('#boardSettingsForm [name=showBoards]').uncheck()
      assert.deepEqual((await saveBoardSettingsUI(page)).boardSelections, wanted)
      root = (await client('getMusicDirectory.view?id=1')).directory
      assert.equal(root.child.filter(item => item.id.startsWith('dir-')).length, 0)
      await page.locator('#boardSettingsForm [name=boardSources]').fill('wy,kg')
      assert.deepEqual((await saveBoardSettingsUI(page)).boardSelections, wanted)
      assert.equal(await page.locator('[data-board-source=tx]').count(), 0)
      await page.locator('#boardSettingsForm [name=boardSources]').fill('tx,wy,kg')
      await page.locator('#boardSettingsForm [name=showBoards]').check()
      assert.deepEqual((await saveBoardSettingsUI(page)).boardSelections, wanted)
      assert.equal(await page.locator('[data-board-source=tx] [data-board-id=hot]').isChecked(), true)
      assert.deepEqual((await client('getPlaylists.view?')).playlists.playlist.filter(item => item.id.startsWith('lb-')).map(item => item.id), playlistIDs)
    })

    await check('榜单加载失败可保存其他设置，重试与迟到目录不覆盖勾选', async page => {
      const savedChoices = { wy: ['hot', 'missing'], tx: ['new'] }
      assert.equal((await admin.put(url + '/api/admin/settings', { data: { showBoards: true, boardSources: ['wy', 'tx'], boardSelections: savedChoices } })).status(), 200)
      let failures = 0
      await page.route('**/api/admin/boards?source=wy', route => failures++ === 0
        ? route.fulfill({ status: 502, contentType: 'application/json', body: JSON.stringify({ error: '模拟平台不可用，请重试' }) })
        : route.continue())
      const txHeld = await holdResponse(page, '**/api/admin/boards?source=tx')
      await adminBoardSettings(page, url)
      await txHeld.ready
      const wy = await boardGroup(page, 'wy'), tx = await boardGroup(page, 'tx')
      await wy.locator('[role=alert]').waitFor()
      assert.match(await wy.innerText(), /已有选择已保留/)
      await page.locator('#boardSettingsForm [name=showBoards]').uncheck()
      await page.locator('nav [data-tab=settings]').click()
      await page.locator('#settingsForm:not([inert])').waitFor()
      await page.locator('#settingsForm [name=serverName]').fill('平台失败仍可保存')
      assert.deepEqual((await saveSettingsUI(page)).boardSelections, savedChoices)
      await showBoardSettings(page)
      assert.equal(await page.locator('#boardSettingsForm [name=showBoards]').isChecked(), false, '系统设置读取或保存不能覆盖榜单草稿')
      await page.locator('#boardSettingsForm [name=showBoards]').check()
      assert.deepEqual((await saveBoardSettingsUI(page)).boardSelections, savedChoices)
      await tx.locator('[data-board-action=clear]').click()
      const wyHeld = await holdResponse(page, '**/api/admin/boards?source=wy')
      await wy.locator('[data-board-action=reload]').click()
      await wyHeld.ready
      // 取消一个尚未返回的已选项后，该占位行会立即移除。
      await wy.locator('[data-board-id=hot]').click()
      assert.deepEqual(await page.evaluate(() => boardChoices.snapshot()), { wy: ['missing'], tx: [] })
      await wyHeld.finish()
      await txHeld.finish()
      assert.equal(await wy.locator('[data-board-id=hot]').isChecked(), false)
      assert.equal(await wy.locator('[data-board-id=missing]').isChecked(), true)
      assert.equal(await tx.locator('input:checked').count(), 0)
      assert.match(await wy.innerText(), /当前目录未返回，选择仍保留/)
      assert.deepEqual((await saveBoardSettingsUI(page)).boardSelections, { wy: ['missing'], tx: [] })
      await page.locator('#boardSettingsPanel').screenshot({ path: path.join(artifacts, 'board-settings-retry.png') })
    })

    await check('旧设置响应和保存失败均保留最新榜单草稿', async page => {
      assert.equal((await admin.put(url + '/api/admin/settings', { data: { boardSources: ['wy'], boardSelections: { wy: ['hot'] } } })).status(), 200)
      await page.locator('#logoutButton').click()
      await page.locator('#login:not(.hidden)').waitFor()
      await login(page, url, 'admin')
      const held = await holdResponse(page, '**/api/admin/settings')
      await page.locator('nav [data-tab=playlists]').click()
      await page.locator('#boardSettingsPanel > summary').click()
      await held.ready
      assert.equal((await admin.put(url + '/api/admin/settings', { data: { boardSelections: { wy: ['new'] } } })).status(), 200)
      await page.locator('nav [data-tab=search]').click()
      await page.locator('nav [data-tab=playlists]').click()
      await page.locator('#boardSettingsForm:not([inert])').waitFor()
      const wy = await boardGroup(page, 'wy')
      await wy.locator('[data-board-id=new]').uncheck()
      await held.finish()
      assert.deepEqual(await page.evaluate(() => boardChoices.snapshot()), { wy: [] })
      await wy.locator('[data-board-id=hot]').check()
      let saves = 0
      await page.route('**/api/admin/settings', route => route.request().method() === 'PUT' && saves++ === 0
        ? route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ error: '模拟保存失败' }) })
        : route.continue())
      await saveBoardSettingsUI(page, 500)
      assert.equal(await wy.locator('[data-board-id=hot]').isChecked(), true)
      assert.equal(await page.evaluate(() => boardSettingsState.dirty), true)
      assert.deepEqual((await (await admin.get(url + '/api/admin/settings')).json()).boardSelections, { wy: ['new'] })
      assert.deepEqual((await saveBoardSettingsUI(page)).boardSelections, { wy: ['hot'] })
    })

    await check('榜单设置在移动端深色界面可用且不横向溢出', async page => {
      assert.equal((await admin.put(url + '/api/admin/settings', { data: { boardSources: ['wy', 'tx', 'kw', 'kg', 'mg'], boardSelections: { wy: ['hot', 'temporarily-unavailable-board-with-a-long-identifier'], tx: [] } } })).status(), 200)
      await adminBoardSettings(page, url)
      const wy = await boardGroup(page, 'wy')
      await wy.locator('[data-board-id=new]').waitFor()
      await wy.locator('[data-board-id=new]').check()
      const saved = await saveBoardSettingsUI(page)
      assert.equal(saved.boardSelections.wy.includes('new'), true)
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
      await page.locator('#boardSettingsPanel').screenshot({ path: path.join(artifacts, 'board-settings-mobile-dark.png') })
    }, { viewport: { width: 390, height: 844 }, colorScheme: 'dark', isMobile: true, hasTouch: true })

    await check('榜单设置与系统设置独立保存，保留当前歌单草稿和播放', async page => {
      assert.equal((await admin.put(url + '/api/admin/settings', { data: { boardSources: ['wy'], boardSelections: {}, serverName: '系统原名称' } })).status(), 200)
      await adminSettings(page, url)
      await page.locator('#settingsForm [name=serverName]').fill('未保存的系统名称')
      const target = await createPlaylist(page, url, '榜单保存时的歌单草稿')
      await selectPlaylistUI(page, target.id)
      await page.locator('#metaForm [name=name]').fill('未保存的歌单名称')
      await draftSong(page)
      await page.waitForFunction(() => webPlayer.status === 'playing')
      const currentTrack = await page.evaluate(() => webPlayer.track.id)
      await showBoardSettings(page)
      const wy = await boardGroup(page, 'wy')
      await wy.locator('[data-board-mode] option[value=custom]:not([disabled])').waitFor({ state: 'attached' })
      await wy.locator('[data-board-mode]').selectOption('custom')
      await wy.locator('[data-board-action=clear]').click()
      await wy.locator('[data-board-id=hot]').check()
      const boardSaved = await saveBoardSettingsUI(page)
      assert.equal(boardSaved.serverName, '系统原名称', '保存榜单不得提交另一个表单的系统草稿')
      assert.deepEqual(await page.evaluate(() => ({ systemDirty: settingsState.dirty, metaDirty: playlistState.metaDirty, tracksDirty: playlistState.tracksDirty, ids: playlistState.draftTracks.map(track => track.id) })), { systemDirty: true, metaDirty: true, tracksDirty: true, ids: ['tr-wy-2', 'tr-wy-1'] })
      assert.equal(await page.locator('#metaForm [name=name]').inputValue(), '未保存的歌单名称')
      assert.deepEqual(await page.evaluate(() => [webPlayer.track.id, webPlayer.status]), [currentTrack, 'playing'])
      await page.locator('nav [data-tab=settings]').click()
      assert.equal(await page.locator('#settingsForm [name=serverName]').inputValue(), '未保存的系统名称')
      const systemSaved = await saveSettingsUI(page)
      assert.equal(systemSaved.serverName, '未保存的系统名称')
      assert.deepEqual(systemSaved.boardSelections, { wy: ['hot'] }, '较早打开的系统设置不能覆盖刚保存的榜单')
      const stored = await (await page.request.get(url + '/api/app/playlists/' + target.id)).json()
      assert.equal(stored.name, '榜单保存时的歌单草稿')
      assert.deepEqual(stored.tracks.map(track => track.id), ['tr-wy-1'])
    })

    await check('歌单页榜单设置读取失败后可以在面板内重试', async page => {
      await page.locator('#logoutButton').click()
      await page.locator('#login:not(.hidden)').waitFor()
      await login(page, url, 'admin')
      let reads = 0
      await page.route('**/api/admin/settings', route => route.request().method() === 'GET' && reads++ === 0
        ? route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: '模拟暂时无法读取' }) })
        : route.continue())
      await page.locator('nav [data-tab=playlists]').click()
      await page.locator('#boardSettingsPanel > summary').click()
      await page.locator('#retryBoardSettings').waitFor()
      assert.match(await page.locator('#boardSettingsLoadMsg').innerText(), /读取榜单设置失败/)
      assert.equal(await page.locator('#boardSettingsForm').isVisible(), false)
      await page.locator('#retryBoardSettings').click()
      await page.locator('#boardSettingsForm:not([inert])').waitFor()
      assert.equal(await page.locator('#boardSettingsLoadNotice').isVisible(), false)
      assert.deepEqual(await page.evaluate(() => boardChoices.snapshot()), { wy: ['hot'] })
    })

    await check('强制302设置保存、直连播放与禁止代理覆盖', async page => {
      await page.locator('#logoutButton').click()
      await page.locator('#login:not(.hidden)').waitFor()
      await login(page, url, 'admin')
      const loaded = page.waitForResponse(response => response.url().endsWith('/api/admin/settings') && response.request().method() === 'GET')
      await page.locator('nav [data-tab=settings]').click()
      assert.equal((await loaded).status(), 200)
      await settle(page)
      await page.locator('#settingsForm [name=streamMode]').selectOption('force_redirect')
      const saved = page.waitForResponse(response => response.url().endsWith('/api/admin/settings') && response.request().method() === 'PUT')
      await page.locator('#settingsForm button[type=submit]').click()
      const savedResponse = await saved
      assert.equal(savedResponse.status(), 200)
      assert.equal((await savedResponse.json()).streamMode, 'force_redirect')
      await page.reload()
      await page.waitForFunction(() => document.querySelector('#settingsForm [name=streamMode]').value === 'force_redirect')
      await page.screenshot({ path: path.join(artifacts, 'force-302-settings.png'), fullPage: true, animations: 'disabled' })
      for (const path of ['/api/app/stream?id=tr-wy-1', '/rest/stream.view?id=tr-wy-1&u=alice&p=test-password', '/rest/download.view?id=tr-wy-1&u=alice&p=test-password']) {
        const response = await page.request.get(url + path + '&proxy=1', { maxRedirects: 0, headers: { Range: 'bytes=100-199' } })
        assert.equal(response.status(), 302)
        assert.ok(response.headers().location.startsWith(info.upstreamURL + '/'))
        assert.equal(response.headers()['cache-control'], 'no-store')
      }
      const upstreamRequests = []
      page.on('request', request => { if (request.url().startsWith(info.upstreamURL)) upstreamRequests.push(request.url()) })
      await search(page, '强制直连')
      await playSearch(page)
      assert.ok(upstreamRequests.length > 0, '强制302必须让浏览器直连音源')
      const streams = []
      page.on('request', request => { if (request.url().includes('/api/app/stream?')) streams.push(request.url()) })
      await search(page, '播放失败')
      await page.locator('#searchTable [data-song-action=play]').first().click()
      await page.waitForFunction(() => webPlayer.status === 'error')
      await settle(page)
      assert.equal(streams.length, 1, '强制302直连失败不得自动重试或切换代理')
      assert.equal(streams[0].includes('proxy='), false)
    })

    assert.equal((await admin.put(url + '/api/admin/settings', { data: { streamMode: 'proxy' } })).status(), 200)
    await check('代理真实播放、Range与移动端深色布局', async page => {
      const upstreamRequests = []
      page.on('request', request => { if (request.url().startsWith(info.upstreamURL)) upstreamRequests.push(request.url()) })
      await search(page, '移动端')
      await playSearch(page)
      assert.deepEqual(upstreamRequests, [], '代理模式不得让浏览器直连上游')
      const range = await page.request.get(url + '/api/app/stream?id=tr-wy-1', { headers: { Range: 'bytes=100-199' } })
      assert.equal(range.status(), 206)
      assert.match(range.headers()['content-range'], /^bytes 100-199\//)
      assert.equal((await range.body()).length, 100)
      await page.screenshot({ path: path.join(artifacts, 'mobile-dark.png'), fullPage: true })
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
      const bar = await page.locator('#playerBar').boundingBox()
      assert.ok(bar.y >= 0 && bar.y + bar.height <= 845)
      await page.locator('#searchTable tbody tr').last().scrollIntoViewIfNeeded()
      const last = await page.locator('#searchTable tbody tr').last().boundingBox()
      assert.ok(last.y + last.height <= bar.y + 1, '播放器不应遮挡最后一行')
    }, { viewport: { width: 390, height: 844 }, colorScheme: 'dark', isMobile: true, hasTouch: true })
  } finally {
    if (admin) await admin.dispose()
    if (browser) await browser.close()
    if (fixture.exitCode === null && fixture.signalCode === null) {
      const exited = once(fixture, 'exit')
      fixture.kill('SIGTERM')
      await exited
    }
    fs.rmSync(buildDir, { recursive: true, force: true })
    fs.writeFileSync(path.join(artifacts, 'results.json'), JSON.stringify(results, null, 2))
    console.log('验收记录：' + artifacts)
  }
  if (results.some(result => !result.ok)) process.exitCode = 1
}
main().catch(error => { console.error(error); process.exitCode = 1 })
