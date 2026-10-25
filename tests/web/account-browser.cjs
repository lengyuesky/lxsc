const assert = require('node:assert/strict')

module.exports = async ({ check, url, login, admin, holdResponse }) => {
  let sequence = 0
  async function createAccount(isAdmin = false) {
    const name = '密码功能验收-' + Date.now() + '-' + ++sequence
    const response = await admin.post(url + '/api/admin/users', { data: { name, password: 'test-password', isAdmin, quality: 'flac' } })
    assert.equal(response.status(), 200)
    return response.json()
  }
  async function switchAccount(page, user, password = 'test-password') {
    await page.locator('#logoutButton').click()
    await page.locator('#login:not(.hidden)').waitFor()
    await login(page, url, user, password)
  }
  async function fillPassword(page, current = 'test-password', next = 'changed-password') {
    await page.locator('#changePasswordForm [name=currentPassword]').fill(current)
    await page.locator('#changePasswordForm [name=newPassword]').fill(next)
    await page.locator('#changePasswordForm [name=confirmPassword]').fill(next)
  }
  async function submitPassword(page, status) {
    const changed = page.waitForResponse(response => response.url().endsWith('/api/auth/password') && response.request().method() === 'PUT')
    await page.locator('#changePasswordForm button[type=submit]').click()
    assert.equal((await changed).status(), status)
  }

  await check('本人密码：普通用户校验当前密码、确认输入并撤销全部旧会话', async page => {
    const user = await createAccount(), other = await page.context().browser().newContext()
    try {
      assert.equal((await other.request.post(url + '/api/auth/login', { data: { username: user.name, password: 'test-password' } })).status(), 200)
      await switchAccount(page, user.name)
      assert.equal(await page.locator('nav .admin-only:visible').count(), 0)
      await page.locator('#changePasswordButton').click()
      await fillPassword(page)
      await page.locator('#changePasswordForm [name=confirmPassword]').fill('does-not-match')
      let requests = 0
      page.on('request', request => { if (request.url().endsWith('/api/auth/password')) requests++ })
      await page.locator('#changePasswordForm button[type=submit]').click()
      assert.match(await page.locator('#changePasswordError').innerText(), /不一致/)
      assert.equal(requests, 0)
      await fillPassword(page, 'wrong-password')
      await submitPassword(page, 400)
      await page.waitForFunction(() => document.querySelector('#changePasswordError').textContent.includes('当前密码错误'))
      assert.equal((await page.request.get(url + '/api/auth/me')).status(), 200)
      await fillPassword(page)
      await submitPassword(page, 200)
      await page.locator('#login:not(.hidden)').waitFor()
      assert.match(await page.locator('#loginError').innerText(), /密码已修改/)
      assert.equal(await page.locator('#passwordDialog').isVisible(), false)
      assert.deepEqual(await page.locator('#changePasswordForm input').evaluateAll(inputs => inputs.map(input => input.value)), ['', '', ''])
      assert.equal((await other.request.get(url + '/api/auth/me')).status(), 401)
      await login(page, url, user.name, 'changed-password')
      assert.equal((await page.request.get(url + '/api/admin/users')).status(), 403, '本人改密不能顺带获得管理权限')
      const protocol = await (await page.request.get(url + '/rest/ping?f=json&u=' + encodeURIComponent(user.name) + '&p=changed-password')).json()
      assert.equal(protocol['subsonic-response'].status, 'ok')
      const old = await (await page.request.get(url + '/rest/ping?f=json&u=' + encodeURIComponent(user.name) + '&p=test-password')).json()
      assert.equal(old['subsonic-response'].status, 'failed')
      await page.locator('#changePasswordButton').click()
      await fillPassword(page)
      await page.locator('#cancelChangePassword').click()
      await page.waitForFunction(() => !document.querySelector('#changePasswordForm [name=newPassword]').value)
    } finally {
      await other.close()
      await admin.delete(url + '/api/admin/users/' + user.id)
    }
  })

  await check('本人密码：有歌单草稿时可取消改密且不丢失草稿', async page => {
    const user = await createAccount()
    try {
      await switchAccount(page, user.name)
      const created = await (await page.request.post(url + '/api/app/playlists', { data: { name: '改密草稿保护' } })).json()
      await page.locator('nav [data-tab=playlists]').click()
      await page.locator(`[data-playlist-id="${created.id}"]`).click()
      await page.locator('#metaForm [name=name]').fill('尚未保存的名称')
      await page.locator('#changePasswordButton').click()
      await fillPassword(page)
      page.once('dialog', dialog => dialog.dismiss())
      await page.locator('#changePasswordForm button[type=submit]').click()
      assert.equal(await page.evaluate(() => playlistState.metaDirty), true)
      assert.equal(await page.locator('#metaForm [name=name]').inputValue(), '尚未保存的名称')
      assert.equal((await page.request.get(url + '/api/auth/me')).status(), 200)
      await page.locator('#cancelChangePassword').click()
    } finally { await admin.delete(url + '/api/admin/users/' + user.id) }
  })

  await check('本人密码：管理员改密同时撤销临时调试凭据', async page => {
    const user = await createAccount(true)
    try {
      await switchAccount(page, user.name)
      await page.locator('nav [data-tab=debug]').click()
      await page.locator('#debugCreateButton').click()
      await page.waitForFunction(() => !!document.querySelector('#debugTokenValue').value)
      const token = await page.locator('#debugTokenValue').inputValue()
      await page.locator('#changePasswordButton').click()
      await fillPassword(page)
      await submitPassword(page, 200)
      await page.locator('#login:not(.hidden)').waitFor()
      assert.equal((await page.request.get(url + '/api/debug/status', { headers: { Authorization: 'Bearer ' + token } })).status(), 401)
      assert.equal(await page.locator('#debugTokenValue').inputValue(), '')
      await login(page, url, user.name, 'changed-password')
      assert.equal((await page.request.get(url + '/api/admin/users')).status(), 200)
    } finally { await admin.delete(url + '/api/admin/users/' + user.id) }
  })

  await check('随机密码：创建选项、一次性展示、复制回退和新账号登录', async page => {
    await switchAccount(page, 'admin')
    await page.locator('nav [data-tab=users]').click()
    const name = '随机密码验收-' + Date.now()
    let created
    try {
      await page.locator('#createUserForm [name=name]').fill(name)
      await page.locator('#createUserForm [name=password]').fill('must-not-submit')
      await page.locator('#createUserForm [name=randomPassword]').check()
      assert.equal(await page.locator('#createUserForm [name=password]').isDisabled(), true)
      assert.equal(await page.locator('#createUserForm [name=password]').inputValue(), '')
      assert.equal(await page.locator('#createUserForm [name=password]').getAttribute('required'), null)
      const saved = page.waitForResponse(response => response.url().endsWith('/api/admin/users') && response.request().method() === 'POST')
      await page.locator('#createUserForm button[type=submit]').click()
      const response = await saved
      assert.equal(response.status(), 200)
      assert.equal(Object.hasOwn(response.request().postDataJSON(), 'password'), false)
      created = await response.json()
      await page.locator('#createdUserDialog[open]').waitFor()
      assert.equal(await page.locator('#createdUserName').inputValue(), name)
      assert.match(created.password, /^[A-Za-z0-9_-]{24}$/)
      assert.equal(await page.locator('#createdUserPassword').inputValue(), created.password)
      await page.evaluate(() => Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { throw new Error('模拟无剪贴板权限') } } }))
      await page.locator('#copyCreatedUserPassword').click()
      await page.waitForFunction(() => document.querySelector('#toast').textContent.includes('手动选择复制'))
      assert.equal(await page.locator('#createdUserPassword').evaluate(field => field.selectionEnd - field.selectionStart), 24)
      assert.equal(await page.locator('#createUserForm [name=password]').isEnabled(), true)
      const users = await (await page.request.get(url + '/api/admin/users')).json()
      assert.equal(users.some(user => Object.hasOwn(user, 'password')), false)
      await page.locator('#closeCreatedUser').click()
      assert.equal(await page.locator('#createdUserPassword').inputValue(), '')
      assert.equal(await page.locator('#createdUserName').inputValue(), '')
      await switchAccount(page, name, created.password)
      assert.equal((await (await page.request.get(url + '/api/auth/me')).json()).user.name, name)
    } finally { if (created) await admin.delete(url + '/api/admin/users/' + created.id) }
  })

  await check('随机密码：等待创建时切换页面仍可取得密码，离开展示后清除', async page => {
    await switchAccount(page, 'admin')
    await page.locator('nav [data-tab=users]').click()
    const held = await holdResponse(page, '**/api/admin/users', 'POST')
    await page.locator('#createUserForm [name=name]').fill('切页随机密码-' + Date.now())
    await page.locator('#createUserForm [name=randomPassword]').check()
    await page.locator('#createUserForm button[type=submit]').click()
    const created = await (await held.ready).json()
    try {
      await page.locator('nav [data-tab=search]').click()
      await held.finish()
      await page.locator('#createdUserDialog[open]').waitFor()
      assert.equal(await page.locator('#createdUserPassword').inputValue(), created.password)
      await page.evaluate(() => { location.hash = '#playlists' })
      await page.locator('#createdUserDialog').waitFor({ state: 'hidden' })
      assert.equal(await page.locator('#createdUserPassword').inputValue(), '')
    } finally { await admin.delete(url + '/api/admin/users/' + created.id) }
  })

  await check('随机密码：退出后迟到的创建响应不得重新展示密码', async page => {
    await switchAccount(page, 'admin')
    await page.locator('nav [data-tab=users]').click()
    const held = await holdResponse(page, '**/api/admin/users', 'POST')
    await page.locator('#createUserForm [name=name]').fill('迟到随机密码-' + Date.now())
    await page.locator('#createUserForm [name=randomPassword]').check()
    await page.locator('#createUserForm button[type=submit]').click()
    const created = await (await held.ready).json()
    try {
      await page.request.post(url + '/api/auth/logout')
      await page.evaluate(() => showLogin())
      await held.finish()
      assert.equal(await page.locator('#createdUserDialog').isVisible(), false)
      assert.equal(await page.locator('#createdUserPassword').inputValue(), '')
      assert.equal(await page.locator('#createUserForm [name=password]').inputValue(), '')
    } finally { await admin.delete(url + '/api/admin/users/' + created.id) }
  })

  await check('本人密码：移动端深色界面入口可用且不横向溢出', async page => {
    await page.locator('#changePasswordButton').click()
    await fillPassword(page)
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    await page.locator('#cancelChangePassword').click()
    await page.waitForFunction(() => !document.querySelector('#changePasswordForm [name=currentPassword]').value)
  }, { viewport: { width: 390, height: 844 }, colorScheme: 'dark', isMobile: true, hasTouch: true })
}
