// 用户管理与列表事件独立于主路由，动态按钮不再拼接内联脚本。
let usersCache = []
async function loadUsers() {
  usersCache = await adminAPI('/users')
  $('#userTable tbody').innerHTML = usersCache.length ? usersCache.map(u => `<tr><td class="muted">${u.id}</td><td><b>${esc(u.name)}</b></td><td>${u.isAdmin ? '<span class="badge pri">管理员</span>' : '<span class="badge off">普通用户</span>'}</td><td><code>${esc(u.quality)}</code></td>
    <td class="actions"><button class="sec sm" data-user-action="edit" data-user-id="${u.id}">编辑</button><button class="sec sm" data-user-action="key" data-user-id="${u.id}">管理 API Key</button><button class="danger sm" data-user-action="delete" data-user-id="${u.id}">删除</button></td></tr>`).join('') : '<tr><td colspan="5" class="muted center">暂无用户</td></tr>'
}
async function createUser(e) {
  e.preventDefault()
  const form = e.currentTarget, button = form.querySelector('button[type=submit]')
  if (button.disabled) return false
  const f = new FormData(form), epoch = sessionState.epoch
  const randomPassword = f.get('randomPassword') === 'on'
  const body = { name: f.get('name'), quality: f.get('quality'), isAdmin: f.get('isAdmin') === 'on', randomPassword }
  if (!randomPassword) body.password = f.get('password')
  button.disabled = true
  try {
    const created = await adminAPI('/users', { method: 'POST', body })
    if (epoch !== sessionState.epoch || !sessionState.me?.isAdmin) return false
    form.reset(); syncCreatePasswordMode(); toast('用户已创建')
    if (created.password) {
      $('#createdUserName').value = created.name
      $('#createdUserPassword').value = created.password
      $('#createdUserDialog').showModal()
    }
    await loadUsers()
  } catch (err) { if (!isAbort(err)) toast(err.message, true) }
  finally { if (epoch === sessionState.epoch) button.disabled = false }
  return false
}
function syncCreatePasswordMode() {
  const form = $('#createUserForm'), random = form.elements.randomPassword.checked
  form.elements.password.disabled = random
  form.elements.password.required = !random
  form.elements.password.placeholder = random ? '创建时自动生成，成功后仅显示一次' : '密码'
  if (random) form.elements.password.value = ''
}
function clearCreatedUserPassword() {
  $('#createdUserName').value = ''
  $('#createdUserPassword').value = ''
}
function closeCreatedUserPassword() {
  clearCreatedUserPassword()
  if ($('#createdUserDialog').open) $('#createdUserDialog').close()
}
function resetUserSession() {
  closeCreatedUserPassword()
  $('#createUserForm').reset()
  $('#createUserForm button[type=submit]').disabled = false
  syncCreatePasswordMode()
  $('#editUserForm').reset()
  if ($('#userDialog').open) $('#userDialog').close()
  usersCache = []
  $('#userTable tbody').replaceChildren()
}
function editUser(id) {
  const u = usersCache.find(x => x.id === id); if (!u) return
  const d = $('#userDialog'), f = d.querySelector('form')
  f.reset()
  f.elements.id.value = u.id
  f.elements.name.value = u.name
  f.elements.quality.value = u.quality
  f.elements.isAdmin.checked = !!u.isAdmin
  d.showModal()
}
async function submitUser(e) {
  e.preventDefault()
  const f = e.target
  try {
    await adminAPI('/users/' + f.elements.id.value, { method: 'PUT', body: { name: f.elements.name.value, password: f.elements.password.value, quality: f.elements.quality.value, isAdmin: f.elements.isAdmin.checked } })
    $('#userDialog').close(); toast('已保存'); await loadUsers()
  } catch (err) { if (!isAbort(err)) toast(err.message, true) }
  return false
}
async function deleteUser(id) {
  if (!confirm('确定删除该用户及其歌单/收藏？')) return
  try { await adminAPI('/users/' + id, { method: 'DELETE' }); toast('用户已删除'); await loadUsers() } catch (err) { if (!isAbort(err)) toast(err.message, true) }
}
let keyUserID = 0, keyEpoch = 0
async function apiKey(id) {
  keyUserID = id; keyEpoch++
  $('#keyValue').value = ''
  $('#keyCreateForm').reset()
  $('#keyDialog').showModal()
  await loadAPIKeys()
}
async function loadAPIKeys() {
  const userID = keyUserID, epoch = sessionState.epoch, generation = keyEpoch
  try {
    const keys = await adminAPI('/users/' + userID + '/apikeys')
    if (userID !== keyUserID || epoch !== sessionState.epoch || generation !== keyEpoch) return
    const date = value => value ? new Date(value * 1000).toLocaleString() : '—'
    $('#keyList').innerHTML = keys.length ? keys.map(key => `<div class="card"><b>${esc(key.label)}</b><p class="muted">创建：${esc(date(key.createdAt))}<br>有效期：${key.expiresAt ? esc(date(key.expiresAt)) : '长期有效'}<br>最近使用：${esc(date(key.lastUsedAt))}</p><button type="button" class="danger sm" data-revoke-key="${esc(key.id)}">撤销</button></div>`).join('') : '<p class="muted">暂无密钥</p>'
  } catch (error) { if (!isAbort(error)) toast(error.message, true) }
}
$('#keyCreateForm').addEventListener('submit', async event => {
  event.preventDefault()
  const form = event.currentTarget, button = form.querySelector('button'), userID = keyUserID, generation = keyEpoch
  button.disabled = true
  try {
    const r = await adminAPI('/users/' + userID + '/apikey', { method: 'POST', body: { label: form.elements.label.value, days: Number(form.elements.days.value) } })
    if (keyUserID !== userID || generation !== keyEpoch || !$('#keyDialog').open) return
    $('#keyValue').value = r.apiKey; $('#keyValue').select(); await loadAPIKeys()
  } catch (error) { if (!isAbort(error)) toast(error.message, true) }
  finally { button.disabled = false }
})
$('#keyList').addEventListener('click', async event => {
  const button = event.target.closest('[data-revoke-key]')
  if (!button || !confirm('撤销后，使用该密钥的客户端需重新配置。确定撤销？')) return
  button.disabled = true
  try { await adminAPI('/users/' + keyUserID + '/apikeys/' + encodeURIComponent(button.dataset.revokeKey), { method: 'DELETE' }); await loadAPIKeys() }
  catch (error) { if (!isAbort(error)) toast(error.message, true); button.disabled = false }
})
function clearAPIKeyDialog() {
  keyUserID = 0; keyEpoch++; $('#keyValue').value = ''; $('#keyList').replaceChildren()
}
$('#keyDialog').addEventListener('close', clearAPIKeyDialog)
$('#keyDialog').addEventListener('cancel', clearAPIKeyDialog)
$('#createUserForm [name=randomPassword]').addEventListener('change', syncCreatePasswordMode)
$('#createdUserDialog').addEventListener('close', clearCreatedUserPassword)
$('#createdUserDialog').addEventListener('cancel', clearCreatedUserPassword)
$('#copyCreatedUserPassword').addEventListener('click', () => {
  const field = $('#createdUserPassword')
  if (!field.value) return
  field.select()
  void copyText(field.value)
})
$('#closeCreatedUser').addEventListener('click', closeCreatedUserPassword)
$('#createUserForm').addEventListener('submit', createUser)
$('#editUserForm').addEventListener('submit', submitUser)
$('#userTable').addEventListener('click', event => {
  const button = event.target.closest('[data-user-action]')
  if (!button) return
  const action = { edit: editUser, key: apiKey, delete: deleteUser }[button.dataset.userAction]
  if (action) action(Number(button.dataset.userId))
})
