// 会话、路由与歌单编辑入口。
function showLogin() {
  resetDebugSession()
  resetPlaybackExperience(true)
  clearAPIKeyDialog()
  if ($('#keyDialog').open) $('#keyDialog').close()
  $('#login').classList.remove('hidden')
  $('#app').classList.add('hidden')
  sessionState.me = null
  playlistState.me = null
  resetMusicSession()
  LXSCAdminModules.reset()
  LXSCSettings.reset()
  clearDetail()
}
function showApp() {
  $('#login').classList.add('hidden')
  $('#app').classList.remove('hidden')
}
async function initializeSession(data) {
  resetDebugSession()
  resetMusicSession()
  LXSCAdminModules.reset()
  LXSCSettings.reset()
  sessionState.me = data.user
  sessionState.defaultPublic = !!data.defaultPublic
  initializePlaybackExperience()
  document.querySelectorAll('.admin-only').forEach(el => el.classList.toggle('hidden', !data.user.isAdmin))
  $('#identity').textContent = data.user.name
  $('#roleTag').textContent = data.user.isAdmin ? 'admin' : 'user'
  showApp()
  await initializePlaylists(data)
  route()
}

$('#loginForm').addEventListener('submit', async event => {
  event.preventDefault()
  const loginForm = event.currentTarget
  const form = new FormData(loginForm)
  $('#loginError').textContent = ''
  try {
    const data = await authAPI('/login', { method: 'POST', body: { username: form.get('username'), password: form.get('password') } })
    loginForm.reset()
    await initializeSession(data)
  } catch (error) { $('#loginError').textContent = error.message }
})

$('#logoutButton').addEventListener('click', async event => {
  event.preventDefault()
  if (!confirmDiscard()) return
  resetDebugSession()
  webPlayer.audio.pause()
  await listeningTracker.flush()
  await authAPI('/logout', { method: 'POST' })
  clearDetail()
  location.hash = ''
  showLogin()
})

// ---- 路由 ----
const loaders = { playlists: loadPlaylistTab, listening: () => loadListening(), dashboard: () => LXSCAdminModules.load('admin', 'loadDashboard'), sources: () => LXSCAdminModules.load('admin', 'loadSources'), users: loadUsers, backups: () => LXSCAdminModules.load('backups', 'load'), settings: LXSCSettings.load, search: () => {}, logs: () => LXSCAdminModules.load('admin', 'loadLogs'), debug: () => loadDebug() }
function route() {
  LXSCAdminModules.cancelLoad()
  if (!sessionState.me) return
  const fallback = sessionState.me.isAdmin ? 'dashboard' : 'playlists'
  let tab = (location.hash || '#' + fallback).slice(1)
  if (!loaders[tab] || (!sessionState.me.isAdmin && !['playlists', 'search', 'listening'].includes(tab))) tab = fallback
  if (tab !== 'debug') clearDebugSecret()
  if (location.hash !== '#' + tab) history.replaceState(null, '', '#' + tab)
  document.querySelectorAll('nav a[data-tab]').forEach(a => a.classList.toggle('active', a.dataset.tab === tab))
  document.querySelectorAll('.tab').forEach(s => s.classList.toggle('active', s.id === 'tab-' + tab))
  Promise.resolve((loaders[tab] || loaders[fallback])()).catch(error => { if (!isAbort(error)) toast(error.message, true) })
}
window.addEventListener('hashchange', route)

// ---- 歌单 ----
const playlistState = {
  me: null,
  defaultPublic: false,
  users: [],
  playlists: [],
  current: null,
  draftTracks: [],
  draftRevision: '',
  editVersion: 0,
  tracksDirty: false,
  metaDirty: false,
  searchResults: [],
  dragIndex: -1,
}

async function initializePlaylists(data) {
  playlistState.me = data.user
  playlistState.defaultPublic = !!data.defaultPublic
  playlistState.users = []
  playlistState.playlists = []
  playlistState.current = null
  playlistState.draftTracks = []
  playlistState.tracksDirty = false
  playlistState.metaDirty = false
  playlistState.searchResults = []
  $('#playlistSearchStatus').textContent = ''
  $('#ownerFilterWrap').classList.toggle('hidden', !playlistState.me.isAdmin)
  $('#ownerCreateWrap').classList.toggle('hidden', !playlistState.me.isAdmin)
  $('#importOwnerWrap').classList.toggle('hidden', !playlistState.me.isAdmin)
  syncQualityControls()
  $('#importForm').elements.public.checked = playlistState.defaultPublic
  LXSCPlaylistImport.reset()
  $('#createForm').elements.public.checked = playlistState.defaultPublic
  clearDetail()
}

async function loadPlaylistTab() {
  if (playlistState.me?.isAdmin) {
    if ($('#boardSettingsPanel').open) void LXSCSettings.loadBoards()
    const filterValue = $('#ownerFilter').value
    const createValue = $('#createForm').elements.ownerId.value
    const importValue = $('#importForm').elements.ownerId.value
    playlistState.users = await playlistAPI('/users')
    renderUserOptions()
    if ([...$('#ownerFilter').options].some(option => option.value === filterValue)) $('#ownerFilter').value = filterValue
    if ([...$('#createForm').elements.ownerId.options].some(option => option.value === createValue)) $('#createForm').elements.ownerId.value = createValue
    if ([...$('#importForm').elements.ownerId.options].some(option => option.value === importValue)) $('#importForm').elements.ownerId.value = importValue
  }
  await loadPlaylists()
}

function renderUserOptions() {
  const filter = $('#ownerFilter')
  const create = $('#createForm').elements.ownerId
  filter.innerHTML = '<option value="">全部用户</option>' + playlistState.users.map(u => `<option value="${u.id}">${esc(u.name)}</option>`).join('')
  create.innerHTML = playlistState.users.map(u => `<option value="${u.id}" ${u.id === playlistState.me.id ? 'selected' : ''}>${esc(u.name)}</option>`).join('')
  $('#importForm').elements.ownerId.innerHTML = create.innerHTML
}

async function loadPlaylists() {
  const selectedID = playlistState.current?.id
  const ownerID = playlistState.me?.isAdmin ? $('#ownerFilter').value : ''
  const request = playlistListGate.begin()
  let playlists
  try { playlists = await playlistAPI('/playlists' + (ownerID ? '?ownerId=' + encodeURIComponent(ownerID) : ''), { signal: request.signal }) }
  catch (error) { if (isAbort(error)) return false; throw error }
  if (!request.current()) return false
  playlistState.playlists = playlists
  renderPlaylistList()
  if (selectedID && playlistState.current?.id === selectedID && !playlists.some(item => item.id === selectedID) && !playlistState.tracksDirty && !playlistState.metaDirty) clearDetail()
  return true
}

function renderPlaylistList() {
  const box = $('#playlistList')
  if (!playlistState.playlists.length) {
    box.innerHTML = '<div class="card muted">暂无歌单，可在下方新建</div>'
    return
  }
  box.innerHTML = playlistState.playlists.map(item => `<button class="playlist-item ${playlistState.current?.id === item.id ? 'active' : ''}" data-playlist-id="${esc(item.id)}">
    <div class="playlist-name"><span>${esc(item.name)}</span>${item.public ? '<span class="badge public">公开</span>' : '<span class="badge">私有</span>'}</div>
    <div class="playlist-meta"><span>${esc(item.owner)} · ${item.songCount} 首</span><span>${formatTime(item.updatedAt)}</span></div>
  </button>`).join('')
}

function confirmDiscard() {
  return (!playlistState.tracksDirty && !playlistState.metaDirty) || confirm('当前歌单有尚未保存的变更，确定放弃吗？')
}

async function selectPlaylist(id, force = false) {
  if (!force && playlistState.current?.id === id) return
  if (!force && !confirmDiscard()) return
  const request = playlistDetailGate.begin()
  playlistDetailTarget = id
  const editVersion = playlistState.editVersion
  const detail = await playlistAPI('/playlists/' + encodeURIComponent(id), { signal: request.signal })
  if (!request.current() || playlistState.editVersion !== editVersion) return false
  playlistSearchGate.cancel()
  playlistState.current = detail
  playlistState.draftTracks = detail.tracks.map(track => ({ ...track }))
  playlistState.draftRevision = detail.tracksRevision
  playlistState.tracksDirty = false
  playlistState.metaDirty = false
  playlistState.searchResults = []
  renderDetail()
  renderPlaylistList()
  LXSCPlaylistImport.updateMode()
  return true
}

function clearDetail() {
  playlistDetailGate.cancel()
  playlistDetailTarget = ''
  playlistState.editVersion++
  playlistSearchGate.cancel()
  playlistState.current = null
  playlistState.draftTracks = []
  playlistState.draftRevision = ''
  playlistState.tracksDirty = false
  playlistState.metaDirty = false
  playlistState.searchResults = []
  $('#detail').classList.add('hidden')
  $('#emptyDetail').classList.remove('hidden')
  renderPlaylistList()
  LXSCPlaylistImport.updateMode()
}

function renderDetail() {
  const item = playlistState.current
  if (!item) return clearDetail()
  $('#emptyDetail').classList.add('hidden')
  $('#detail').classList.remove('hidden')
  $('#detailTitle').textContent = item.name
  $('#detailOwner').textContent = `${item.owner} · 创建于 ${formatTime(item.createdAt)} · 更新于 ${formatTime(item.updatedAt)}`
  const form = $('#metaForm')
  form.elements.name.value = item.name
  form.elements.comment.value = item.comment || ''
  form.elements.public.checked = !!item.public
  for (const control of form.elements) control.disabled = !item.canEdit
  $('#deleteButton').classList.toggle('hidden', !item.canEdit)
  $('#subscriptionOpen').classList.toggle('hidden', !item.canEdit)
  $('#playlistHistoryOpen').classList.toggle('hidden', !item.canEdit)
  $('#readonlyNotice').classList.toggle('hidden', item.canEdit)
  $('#searchSection').classList.toggle('hidden', !item.canEdit)
  $('#searchResults').innerHTML = ''
  playlistState.metaDirty = false
  renderTracks()
}

function renderSearchResults() {
  const box = $('#searchResults')
  if (!playlistState.searchResults.length) return
  const existing = new Set(playlistState.draftTracks.map(track => track.id))
  let list = box.querySelector(':scope > .search-results')
  if (!list) { list = document.createElement('div'); list.className = 'search-results'; box.replaceChildren(list) }
  LXSCSearch.appendRows(list, playlistState.searchResults, (track, index) => {
    const added = existing.has(track.id)
    return `<div class="search-item" data-playing-id="${esc(track.id)}"><div><div class="song-title">${esc(track.name)}</div><div class="song-sub">${esc(track.singer)} · ${esc(track.album)} · ${esc(platName[track.source] || track.source)}</div></div><div class="search-item-actions"><button type="button" class="sec" data-play-playlist-search="${index}" ${track.unavailable ? 'disabled' : ''}>播放</button><button type="button" data-add-track="${esc(track.id)}" ${added ? 'disabled' : ''}>${added ? '已加入' : '加入草稿'}</button></div></div>`
  })
  list.querySelectorAll('[data-add-track]').forEach(button => {
    const added = existing.has(button.dataset.addTrack)
    if (button.disabled !== added) button.disabled = added
    const label = added ? '已加入' : '加入草稿'
    if (button.textContent !== label) button.textContent = label
  })
  renderPlaybackMarkers()
}

$('#refreshButton').addEventListener('click', async () => {
  if (!confirmDiscard()) return
  try {
    playlistState.tracksDirty = false
    playlistState.metaDirty = false
    if (!await loadPlaylists()) return
    if (playlistState.current && !await selectPlaylist(playlistState.current.id, true)) return
    toast('已刷新')
  } catch (error) { if (!isAbort(error)) toast(error.message, true) }
})

$('#ownerFilter').addEventListener('change', async () => {
  if (!confirmDiscard()) {
    $('#ownerFilter').value = ''
    return
  }
  playlistState.tracksDirty = false
  clearDetail()
  await loadPlaylists()
})

$('#playlistList').addEventListener('click', event => {
  const button = event.target.closest('[data-playlist-id]')
  if (button) selectPlaylist(button.dataset.playlistId).catch(error => { if (!isAbort(error)) toast(error.message, true) })
})

$('#createForm').addEventListener('submit', async event => {
  event.preventDefault()
  const createForm = event.currentTarget
  const form = new FormData(createForm)
  const body = { name: form.get('name'), comment: form.get('comment'), public: form.get('public') === 'on' }
  if (playlistState.me.isAdmin) body.ownerId = Number(form.get('ownerId'))
  try {
    const created = await playlistAPI('/playlists', { method: 'POST', body })
    createForm.reset()
    createForm.elements.public.checked = playlistState.defaultPublic
    if (playlistState.me.isAdmin) {
      createForm.elements.ownerId.value = String(playlistState.me.id)
      $('#ownerFilter').value = ''
    }
    createForm.closest('details').open = false
    await loadPlaylists()
    await selectPlaylist(created.id)
    toast('歌单已创建')
  } catch (error) { toast(error.message) }
})

$('#metaForm').addEventListener('input', () => {
  if (playlistState.current?.canEdit) { playlistState.metaDirty = true; playlistState.editVersion++ }
  renderCollectForm()
})

$('#metaForm').addEventListener('submit', async event => {
  event.preventDefault()
  if (!playlistState.current?.canEdit) return
  const form = new FormData(event.currentTarget), id = playlistState.current.id
  const body = { name: form.get('name'), comment: form.get('comment'), public: form.get('public') === 'on' }
  try {
    const updated = await playlistAPI('/playlists/' + encodeURIComponent(id), { method: 'PUT', body })
    invalidatePlaylistReads(id)
    if (playlistState.current?.id === id) {
      playlistState.current = { ...updated, tracks: playlistState.current.tracks, tracksRevision: playlistState.current.tracksRevision }
      const fields = $('#metaForm').elements
      playlistState.metaDirty = fields.name.value !== body.name || fields.comment.value !== body.comment || fields.public.checked !== body.public
      if (!playlistState.metaDirty) renderDetail()
      else renderCollectForm()
    }
    await loadPlaylists()
    toast('歌单信息已保存')
  } catch (error) { if (!isAbort(error)) toast(error.message, true) }
})

$('#deleteButton').addEventListener('click', async () => {
  if (!playlistState.current?.canEdit || !confirm(`确定删除歌单“${playlistState.current.name}”吗？此操作无法撤销。`)) return
  try {
    await playlistAPI('/playlists/' + encodeURIComponent(playlistState.current.id), { method: 'DELETE' })
    clearDetail()
    await loadPlaylists()
    toast('歌单已删除')
  } catch (error) { toast(error.message) }
})

$('#searchResults').addEventListener('click', event => {
  const play = event.target.closest('[data-play-playlist-search]')
  if (play && !play.disabled) { webPlayer.playList(playlistState.searchResults, Number(play.dataset.playPlaylistSearch)); return }
  const button = event.target.closest('[data-add-track]')
  if (!button || button.disabled) return
  const track = playlistState.searchResults.find(item => item.id === button.dataset.addTrack)
  if (!playlistState.current?.canEdit || !track || playlistState.draftTracks.some(item => item.id === track.id)) return
  if (playlistState.draftTracks.length >= 2000) return toast('单个歌单最多包含 2000 首歌曲', true)
  playlistState.draftTracks.unshift({ ...track })
  markTracksDirty()
  toast('已加入草稿，请保存歌曲变更')
})

$('#trackTable').addEventListener('click', event => {
  const button = event.target.closest('[data-track-action]')
  if (!button || button.disabled) return
  const row = button.closest('[data-track-index]')
  const index = Number(row.dataset.trackIndex)
  if (button.dataset.trackAction === 'play') { webPlayer.playList(playlistState.draftTracks, index); return }
  if (button.dataset.trackAction === 'queue') { openQueueAdd(playlistState.draftTracks[index]); return }
  if (!playlistState.current?.canEdit) return
  switch (button.dataset.trackAction) {
    case 'repair': openRepair(index); break
    case 'up': moveTrack(index, index - 1); break
    case 'down': moveTrack(index, index + 1); break
    case 'remove': playlistState.draftTracks.splice(index, 1); markTracksDirty(); break
  }
})

$('#trackTable').addEventListener('dragstart', event => {
  if (event.target.closest('button')) { event.preventDefault(); return }
  const row = event.target.closest('[data-track-index]')
  if (!row || !playlistState.current?.canEdit) return
  playlistState.dragIndex = Number(row.dataset.trackIndex)
  row.classList.add('dragging')
  event.dataTransfer.effectAllowed = 'move'
})
$('#trackTable').addEventListener('dragover', event => {
  const row = event.target.closest('[data-track-index]')
  if (!row || playlistState.dragIndex < 0) return
  event.preventDefault()
  document.querySelectorAll('.drag-over').forEach(item => item.classList.remove('drag-over'))
  row.classList.add('drag-over')
})
$('#trackTable').addEventListener('drop', event => {
  const row = event.target.closest('[data-track-index]')
  if (!row || playlistState.dragIndex < 0) return
  event.preventDefault()
  const target = Number(row.dataset.trackIndex)
  moveTrack(playlistState.dragIndex, target)
  playlistState.dragIndex = -1
})
$('#trackTable').addEventListener('dragend', () => {
  playlistState.dragIndex = -1
  document.querySelectorAll('.dragging,.drag-over').forEach(item => item.classList.remove('dragging', 'drag-over'))
})

$('#saveTracksButton').addEventListener('click', async () => {
  if (!playlistState.current?.canEdit || !playlistState.tracksDirty) return
  try {
    const id = playlistState.current.id
    const draft = playlistState.draftTracks.map(track => track.id)
    const updated = await playlistAPI('/playlists/' + encodeURIComponent(id) + '/tracks', { method: 'PUT', body: { trackIds: draft, expectedRevision: playlistState.draftRevision } })
    invalidatePlaylistReads(id)
    if (playlistState.current?.id !== id) { await loadPlaylists(); return }
    playlistState.current = updated
    playlistState.draftRevision = updated.tracksRevision
    // 请求发出后继续编辑的草稿不能被旧响应覆盖。
    if (JSON.stringify(draft) === JSON.stringify(playlistState.draftTracks.map(track => track.id))) {
      playlistState.draftTracks = updated.tracks.map(track => ({ ...track }))
      playlistState.tracksDirty = false
    }
    $('#detailOwner').textContent = `${updated.owner} · 创建于 ${formatTime(updated.createdAt)} · 更新于 ${formatTime(updated.updatedAt)}`
    renderTracks()
    renderSearchResults()
    await loadPlaylists()
    toast('歌曲变更已保存')
  } catch (error) { if (!isAbort(error)) toast(error.message, true) }
})

window.addEventListener('beforeunload', event => {
  if (!playlistState.tracksDirty && !playlistState.metaDirty) return
  event.preventDefault()
  event.returnValue = ''
})
// 登录初始化由最后加载的 music-ui.js 发起，确保音乐控件与会话清理已就绪。
