// 本人密码修改独立于管理员页面，不把密码写入存储、URL 或日志。
let passwordSaving = false, passwordGeneration = 0
function clearPasswordForm() {
  passwordGeneration++
  passwordSaving = false
  $('#changePasswordForm').reset()
  $('#changePasswordError').textContent = ''
  $('#changePasswordForm button[type=submit]').disabled = false
  $('#cancelChangePassword').disabled = false
}
function resetPasswordSession() {
  clearPasswordForm()
  if ($('#passwordDialog').open) $('#passwordDialog').close()
}
$('#changePasswordButton').addEventListener('click', event => {
  event.preventDefault()
  if (!sessionState.me) return
  clearPasswordForm()
  $('#passwordDialog').showModal()
})
$('#cancelChangePassword').addEventListener('click', () => {
  if (!passwordSaving) $('#passwordDialog').close()
})
$('#passwordDialog').addEventListener('close', clearPasswordForm)
$('#passwordDialog').addEventListener('cancel', event => { if (passwordSaving) event.preventDefault() })
$('#changePasswordForm').addEventListener('input', () => { $('#changePasswordError').textContent = '' })
$('#changePasswordForm').addEventListener('submit', async event => {
  event.preventDefault()
  if (passwordSaving || !sessionState.me) return
  const form = event.currentTarget, errorBox = $('#changePasswordError')
  const body = { currentPassword: form.elements.currentPassword.value, newPassword: form.elements.newPassword.value }
  if (body.newPassword !== form.elements.confirmPassword.value) { errorBox.textContent = '两次输入的新密码不一致'; return }
  const length = [...body.newPassword].length
  if (!body.newPassword.trim() || length < 8 || length > 128) { errorBox.textContent = '新密码需为 8–128 个字符且不能全为空白'; return }
  if (!confirmDiscard()) return
  const generation = passwordGeneration
  passwordSaving = true
  errorBox.textContent = ''
  form.querySelector('button[type=submit]').disabled = true
  $('#cancelChangePassword').disabled = true
  try {
    await listeningTracker.flush()
    if (generation !== passwordGeneration) return
    await authAPI('/password', { method: 'PUT', body })
    if (generation !== passwordGeneration) return
    showLogin()
    $('#loginError').textContent = '密码已修改，请使用新密码重新登录'
  } catch (error) {
    if (!isAbort(error) && generation === passwordGeneration) errorBox.textContent = error.message
  } finally {
    if (generation === passwordGeneration) {
      passwordSaving = false
      form.querySelector('button[type=submit]').disabled = false
      $('#cancelChangePassword').disabled = false
    }
  }
})
