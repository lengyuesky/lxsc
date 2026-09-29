// 导入预览、取消与提交状态由导入模块持有。
const LXSCPlaylistImport = (() => {
// ---- 歌单导入：洛雪文件 / 网易云 / QQ 音乐 ----
const importState = { version: 0, controller: null, ready: false, busy: false }

function invalidateImportPreview() {
  importState.version++
  importState.controller?.abort()
  importState.controller = null
  importState.ready = false
  $('#importLists').classList.add('hidden')
  $('#importLists').innerHTML = ''
  $('#importStatus').textContent = ''
  $('#previewOnlineImport').disabled = false
  $('#importForm').querySelector('button[type=submit]').disabled = true
}

function updateImportSource() {
  invalidateImportPreview()
  const form = $('#importForm'), online = form.elements.source.value !== 'lx'
  $('#importFileWrap').classList.toggle('hidden', online)
  $('#importOnlineWrap').classList.toggle('hidden', !online)
  form.elements.file.required = !online
  form.elements.file.disabled = online
  form.elements.input.required = online
  form.elements.input.disabled = !online
  form.elements.input.placeholder = form.elements.source.value === 'wy'
    ? 'https://music.163.com/#/playlist?id=… 或 ID'
    : 'https://y.qq.com/n/ryqq/playlist/… 或 ID'
}

function resetImportForm(keepBusy = false) {
  const form = $('#importForm')
  form.reset()
  form.inert = keepBusy
  importState.busy = keepBusy
  form.elements.public.checked = playlistState.defaultPublic
  if (playlistState.me?.isAdmin) form.elements.ownerId.value = String(playlistState.me.id)
  updateImportSource()
  updateImportMode()
}

function updateImportMode() {
  const form = $('#importForm')
  const append = form.elements.mode.value === 'append'
  const canAppend = !!playlistState.current?.canEdit
  form.elements.mode.options[1].disabled = !canAppend
  form.elements.mode.options[1].textContent = canAppend ? `追加到「${playlistState.current.name}」` : '追加到当前选中的歌单'
  if (append && !canAppend) form.elements.mode.value = 'create'
  const creating = form.elements.mode.value === 'create'
  $('#importOwnerWrap').classList.toggle('hidden', !creating || !playlistState.me?.isAdmin)
  $('#importPublicWrap').classList.toggle('hidden', !creating)
}

function importRequest(preview, selected = []) {
  const form = $('#importForm'), source = form.elements.source.value
  if (source !== 'lx') {
    return { path: '/playlists/import/online', body: { source, input: form.elements.input.value.trim(), preview } }
  }
  const body = new FormData()
  body.append('file', form.elements.file.files[0])
  if (preview) body.append('preview', '1')
  else body.append('lists', selected.join(','))
  return { path: '/playlists/import', body }
}

async function previewImport() {
  if (importState.busy) return
  invalidateImportPreview()
  const form = $('#importForm'), online = form.elements.source.value !== 'lx'
  if (online ? !form.elements.input.value.trim() : !form.elements.file.files[0]) return
  const version = importState.version, epoch = sessionState.epoch
  const controller = new AbortController()
  importState.controller = controller
  const current = () => version === importState.version && epoch === sessionState.epoch
  $('#importStatus').textContent = online ? '正在读取歌单，请稍候…' : '正在解析文件…'
  $('#previewOnlineImport').disabled = true
  const { path, body } = importRequest(true)
  try {
    const result = await playlistAPI(path, { method: 'POST', body, signal: controller.signal })
    if (!current()) return
    const box = $('#importLists')
    box.innerHTML = result.lists.map(item => `<label><input type="checkbox" name="lists" value="${item.index}" checked><span class="import-name">${esc(item.name)}</span><span class="import-meta">${item.source ? esc(platName[item.source] || item.source) + ' · ' : ''}${item.count} 首${item.skipped ? ` · 跳过 ${item.skipped}` : ''}</span></label>`).join('')
    box.classList.remove('hidden')
    $('#importStatus').textContent = result.truncated ? `歌单共 ${result.total} 首，最多导入前 2000 首有效歌曲；超出上限 ${result.truncated} 首。` : '读取完成，请确认后导入。'
    importState.ready = result.lists.length > 0
    form.querySelector('button[type=submit]').disabled = !importState.ready
  } catch (error) {
    if (current() && !isAbort(error)) $('#importStatus').textContent = error.message
  } finally {
    if (current()) { $('#previewOnlineImport').disabled = false; importState.controller = null }
  }
}

$('#importForm').elements.source.addEventListener('change', () => {
  updateImportSource()
  if ($('#importForm').elements.source.value === 'lx') void previewImport()
})
$('#importForm').elements.input.addEventListener('input', invalidateImportPreview)
$('#importForm').elements.file.addEventListener('change', previewImport)
$('#previewOnlineImport').addEventListener('click', previewImport)
$('#importForm').elements.mode.addEventListener('change', updateImportMode)

$('#importForm').addEventListener('submit', async event => {
  event.preventDefault()
  if (importState.busy || !importState.ready) return
  const form = event.currentTarget
  const selected = [...form.querySelectorAll('input[name=lists]:checked')].map(input => input.value)
  if (!selected.length) return toast('请至少勾选一个列表')
  const append = form.elements.mode.value === 'append'
  if (append && !playlistState.current?.canEdit) return
  if (!confirmDiscard()) return
  const { path, body } = importRequest(false, selected)
  const set = (key, value) => body instanceof FormData ? body.append(key, String(value)) : body[key] = value
  if (append) {
    set('target', playlistState.current.id)
    if (!(body instanceof FormData)) set('expectedRevision', playlistState.draftRevision)
  } else {
    set('public', form.elements.public.checked)
    if (playlistState.me.isAdmin) set('ownerId', Number(form.elements.ownerId.value))
  }
  const epoch = sessionState.epoch, editVersion = playlistState.editVersion, currentID = playlistState.current?.id
  const unchanged = () => editVersion === playlistState.editVersion && currentID === playlistState.current?.id
  const submit = form.querySelector('button[type=submit]')
  importState.busy = true
  form.inert = true
  submit.disabled = true
  submit.textContent = '导入中…'
  let imported = false
  try {
    const result = await playlistAPI(path, { method: 'POST', body })
    if (epoch !== sessionState.epoch) return
    imported = true
    let message = append ? `已追加 ${result.added} 首歌曲` : `已导入 ${result.playlists.length} 个歌单，共 ${result.added} 首歌曲`
    if (result.skipped) message += `，跳过 ${result.skipped} 首本地或无效歌曲`
    if (result.truncated) message += `，超出上限截断 ${result.truncated} 首`
    for (const item of result.playlists) invalidatePlaylistReads(item.id)
    // 刷新完成前继续锁定表单，避免上一请求的收尾影响下一次导入。
    resetImportForm(true)
    form.closest('details').open = false
    if (unchanged()) { playlistState.tracksDirty = false; playlistState.metaDirty = false }
    if (playlistState.me.isAdmin) $('#ownerFilter').value = ''
    toast(message)
    await loadPlaylists()
    if (unchanged() && result.playlists.length) await selectPlaylist(result.playlists[0].id, true)
  } catch (error) {
    if (epoch === sessionState.epoch && !isAbort(error)) toast((imported ? '导入已完成，但刷新列表失败：' : '') + error.message, true)
  } finally {
    if (epoch === sessionState.epoch) {
      importState.busy = false
      form.inert = false
      submit.textContent = '导入'
      submit.disabled = !importState.ready
    }
  }
})


return { reset: resetImportForm, updateMode: updateImportMode }
})()
