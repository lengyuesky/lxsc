// 搜歌、播放器与收藏共用现有登录和歌单状态；此脚本在 app.js 之后加载。
const songSearchGate = new LXSCMusic.RequestGate()
const playlistSearchGate = new LXSCMusic.RequestGate()
const playlistDetailGate = new LXSCMusic.RequestGate()
const playlistListGate = new LXSCMusic.RequestGate()
let playlistDetailTarget = ''
const collectGate = new LXSCMusic.RequestGate()
const songSearchState = { tracks: [] }
const collectState = { track: null, playlists: [], loading: false, busy: false, error: '' }
let playbackMarker = ''
let seekDragging = false

// 已确认的写入会使先前读取失效，但不取消对另一个歌单的详情请求。
function invalidatePlaylistReads(id) {
  playlistListGate.cancel()
  if (playlistDetailTarget === id) playlistDetailGate.cancel()
}

function formatSongTime(seconds) {
  if (!Number.isFinite(seconds) || seconds < 0) return '0:00'
  const value = Math.floor(seconds)
  return `${Math.floor(value / 60)}:${String(value % 60).padStart(2, '0')}`
}

const webPlayer = new LXSCMusic.PlayerController({
  audio: $('#webAudio'),
  streamURL: track => '/api/app/stream?' + new URLSearchParams({ id: track.id, quality: sessionState.me?.quality || '320k' }),
  onChange: renderPlayer,
  onFailure: () => {
    // 原生音频事件拿不到 HTTP 状态，通过普通会话接口确认是否需要退出。
    authAPI('/me').catch(() => {})
  },
})

function renderPlaybackMarkers() {
  const track = webPlayer.track
  document.querySelectorAll('[data-playing-id]').forEach(row => row.classList.toggle('now-playing', !!track && row.dataset.playingId === track.id))
}

function renderPlayer(state) {
  const visible = !!state.track && !!sessionState.me
  $('#playerBar').classList.toggle('hidden', !visible)
  document.body.classList.toggle('has-player', visible)
  if (!visible) {
    document.documentElement.style.removeProperty('--player-height')
    playbackMarker = ''
    renderPlaybackMarkers()
    return
  }
  $('#playerTitle').textContent = state.track.name
  $('#playerTitle').title = state.track.name
  $('#playerMeta').textContent = [state.track.singer, state.track.album, platName[state.track.source] || state.track.source].filter(Boolean).join(' · ')
  $('#playerMeta').title = $('#playerMeta').textContent
  const playing = state.status === 'playing' || state.status === 'loading'
  const toggleText = state.status === 'error' ? '重试播放' : state.status === 'ended' ? '重新播放' : playing ? '暂停' : '播放'
  $('#playerToggle').setAttribute('aria-label', toggleText)
  $('#playerToggle').title = toggleText
  $('#playerToggle use').setAttribute('href', playing ? '#i-pause' : '#i-play')
  $('#playerPrevious').disabled = !state.canPrevious
  $('#playerNext').disabled = !state.canNext
  $('#playerPosition').textContent = `${state.index + 1} / ${state.count}`
  const statusText = state.message || ({ loading: '正在加载…', playing: '正在播放', paused: '已暂停', ended: '列表播放完毕', error: '播放失败' }[state.status] || '')
  if ($('#playerStatus').textContent !== statusText) $('#playerStatus').textContent = statusText
  $('#playerStatus').classList.toggle('err', state.status === 'error')
  const seek = $('#playerSeek')
  seek.disabled = !state.duration
  seek.max = String(state.duration)
  if (!seekDragging) seek.value = String(state.time)
  seek.setAttribute('aria-valuetext', `${formatSongTime(state.time)}，共 ${formatSongTime(state.duration || state.track.duration)} `)
  $('#playerTime').textContent = `${formatSongTime(state.time)} / ${formatSongTime(state.duration || state.track.duration)}`
  $('#playerVolume').value = String(state.volume)
  $('#playerMute').setAttribute('aria-pressed', String(state.muted))
  $('#playerMute').setAttribute('aria-label', state.muted ? '取消静音' : '静音')
  $('#playerMute').title = state.muted ? '取消静音' : '静音'
  if (playbackMarker !== state.track.id) { playbackMarker = state.track.id; renderPlaybackMarkers() }
}

// 控制条高度随错误文案、窄屏与安全区变化，内容区和提示始终为它留出空间。
const playerResizeObserver = new ResizeObserver(entries => {
  if (!$('#playerBar').classList.contains('hidden')) document.documentElement.style.setProperty('--player-height', `${Math.ceil(entries[0].target.getBoundingClientRect().height)}px`)
})
playerResizeObserver.observe($('#playerBar'))
$('#playerToggle').addEventListener('click', () => webPlayer.toggle())
$('#playerPrevious').addEventListener('click', () => webPlayer.previous())
$('#playerNext').addEventListener('click', () => webPlayer.next())
$('#playerSeek').addEventListener('pointerdown', () => { seekDragging = true })
function finishSeeking() { if (seekDragging) { seekDragging = false; webPlayer.notify() } }
document.addEventListener('pointerup', finishSeeking)
document.addEventListener('pointercancel', finishSeeking)
window.addEventListener('blur', finishSeeking)
$('#playerSeek').addEventListener('input', event => webPlayer.seek(Number(event.target.value)))
$('#playerVolume').addEventListener('input', event => webPlayer.setVolume(Number(event.target.value)))
$('#playerMute').addEventListener('click', () => webPlayer.toggleMuted())
$('#playerCollect').addEventListener('click', () => { if (webPlayer.track) openCollect(webPlayer.track) })

function syncQualityControls() {
  const quality = sessionState.me?.quality || '320k'
  document.querySelectorAll('[data-quality-select]').forEach(select => { select.value = quality; select.disabled = false })
}
document.querySelectorAll('[data-quality-select]').forEach(select => select.addEventListener('change', async event => {
  const epoch = sessionState.epoch, quality = event.target.value
  document.querySelectorAll('[data-quality-select]').forEach(control => { control.disabled = true })
  try {
    const user = await playlistAPI('/profile', { method: 'PUT', body: { quality } })
    sessionState.me = { ...sessionState.me, quality: user.quality }
    playlistState.me = { ...playlistState.me, quality: user.quality }
    toast('默认音质已保存，下次播放或重试时生效；不可用时由音源向下降级')
  } catch (error) { if (!isAbort(error) && sessionState.me) toast(error.message, true) }
  finally { if (epoch === sessionState.epoch) syncQualityControls() }
}))

function searchMessage(message, error = false) {
  $('#searchTable tbody').innerHTML = `<tr><td colspan="7" class="${error ? 'err' : 'muted'} center">${esc(message)}</td></tr>`
}
function renderSongResults() {
  if (!songSearchState.tracks.length) return searchMessage('没有找到可用结果，请更换关键词或平台再试')
  $('#searchTable tbody').innerHTML = songSearchState.tracks.map((track, index) => `<tr data-playing-id="${esc(track.id)}"><td><div class="song-title" title="${esc(track.name)}">${esc(track.name)}</div></td><td>${esc(track.singer)}</td><td class="muted song-album">${esc(track.album)}</td><td><span class="badge">${esc(platName[track.source] || track.source)}</span></td><td class="muted song-duration">${track.duration ? formatSongTime(track.duration) : '—'}</td><td class="muted song-quality">${esc((track.qualities || []).join('/'))}</td><td class="song-actions"><button type="button" class="sec sm" data-song-action="play" data-song-index="${index}" ${track.unavailable ? 'disabled' : ''}><svg class="icon"><use href="#i-play"/></svg>播放</button><button type="button" class="sm" data-song-action="collect" data-song-index="${index}" ${track.unavailable ? 'disabled' : ''}>收藏到歌单</button></td></tr>`).join('')
  renderPlaybackMarkers()
}
$('#songSearchForm').addEventListener('submit', async event => {
  event.preventDefault()
  const form = new FormData(event.currentTarget)
  const query = String(form.get('query') || '').trim()
  const request = songSearchGate.begin()
  songSearchState.tracks = []
  if (!query) return searchMessage('请输入歌名或歌手')
  searchMessage('搜索中…')
  try {
    const tracks = await playlistAPI('/search', { method: 'POST', signal: request.signal, body: { query, sources: form.get('source') ? [form.get('source')] : [] } })
    if (!request.current()) return
    songSearchState.tracks = tracks
    renderSongResults()
  } catch (error) { if (request.current() && !isAbort(error)) searchMessage(error.message, true) }
})
$('#searchTable tbody').addEventListener('click', event => {
  const button = event.target.closest('[data-song-action]')
  if (!button || button.disabled) return
  const index = Number(button.dataset.songIndex)
  const track = songSearchState.tracks[index]
  if (!track) return
  if (button.dataset.songAction === 'play') webPlayer.playList(songSearchState.tracks, index)
  else openCollect(track)
})

$('#searchForm').addEventListener('submit', async event => {
  event.preventDefault()
  if (!playlistState.current?.canEdit) return
  const id = playlistState.current.id, form = new FormData(event.currentTarget)
  const request = playlistSearchGate.begin(), box = $('#searchResults')
  playlistState.searchResults = []
  box.innerHTML = '<p class="muted">搜索中…</p>'
  try {
    const tracks = await playlistAPI('/search', { method: 'POST', signal: request.signal, body: { query: form.get('query'), sources: form.get('source') ? [form.get('source')] : [] } })
    if (!request.current() || playlistState.current?.id !== id) return
    playlistState.searchResults = tracks
    if (!tracks.length) box.innerHTML = '<p class="muted">没有找到可用结果，请更换关键词或平台再试</p>'
    else renderSearchResults()
  } catch (error) { if (request.current() && !isAbort(error)) box.innerHTML = `<p class="err">${esc(error.message)}</p>` }
})

async function openCollect(track) {
  if (!sessionState.me || !track || track.unavailable || collectState.busy) return
  const request = collectGate.begin()
  const form = $('#collectForm')
  form.reset()
  form.elements.public.checked = sessionState.defaultPublic
  collectState.track = { ...track }
  collectState.playlists = []
  collectState.loading = true
  collectState.error = ''
  $('#collectSong').textContent = [track.name, track.singer].filter(Boolean).join(' · ')
  form.elements.playlistId.innerHTML = '<option value="">正在读取歌单…</option>'
  renderCollectForm()
  if (!$('#collectDialog').open) $('#collectDialog').showModal()
  try {
    const playlists = await playlistAPI('/playlists', { signal: request.signal })
    if (!request.current()) return
    collectState.playlists = playlists.filter(item => item.canEdit).sort((a, b) => Number(b.userId === sessionState.me.id) - Number(a.userId === sessionState.me.id) || b.updatedAt - a.updatedAt)
    form.elements.playlistId.innerHTML = collectState.playlists.length ? collectState.playlists.map(item => `<option value="${esc(item.id)}">${esc(item.name)} · ${esc(item.owner)} · ${item.songCount} 首</option>`).join('') : '<option value="">暂无可编辑歌单</option>'
    if (collectState.playlists.some(item => item.id === playlistState.current?.id)) form.elements.playlistId.value = playlistState.current.id
    if (!collectState.playlists.length) form.elements.mode.value = 'create'
  } catch (error) { if (request.current() && !isAbort(error)) collectState.error = error.message }
  finally { if (request.current()) { collectState.loading = false; renderCollectForm() } }
}

function renderCollectForm() {
  if (!collectState.track) return
  const form = $('#collectForm'), creating = form.elements.mode.value === 'create'
  const targetID = form.elements.playlistId.value
  const blocked = !creating && LXSCMusic.collectionBlocked(targetID, playlistState.current?.id, playlistState.tracksDirty, playlistState.metaDirty)
  $('#collectTargetWrap').classList.toggle('hidden', creating)
  $('#collectCreateFields').classList.toggle('hidden', !creating)
  form.elements.name.required = creating
  form.elements.playlistId.required = !creating
  for (const control of form.elements) control.disabled = collectState.busy
  form.elements.name.disabled = collectState.busy || !creating
  form.elements.public.disabled = collectState.busy || !creating
  form.elements.playlistId.disabled = collectState.busy || creating || collectState.loading
  $('#collectSubmit').disabled = collectState.busy || collectState.loading || blocked || (creating ? !form.elements.name.value.trim() : !targetID)
  $('#collectSubmit').textContent = collectState.busy ? '正在保存…' : creating ? '新建并收藏' : '收藏'
  $('#collectHint').textContent = blocked ? '该歌单有未保存修改，请先保存或放弃草稿，再收藏到这个歌单；也可以选择其他歌单。' : creating ? '一次完成新建与收藏，歌曲会立即保存。' : '歌曲立即保存到歌单最前方；重复歌曲不会再次加入。'
  $('#collectHint').classList.toggle('warn', blocked)
  $('#collectError').textContent = collectState.error
}
$('#collectForm').addEventListener('input', renderCollectForm)
$('#collectForm').addEventListener('change', () => { collectState.error = ''; renderCollectForm() })
$('#collectCancel').addEventListener('click', () => { if (!collectState.busy) $('#collectDialog').close() })
$('#collectDialog').addEventListener('cancel', event => { if (collectState.busy) event.preventDefault() })
$('#collectDialog').addEventListener('close', () => {
  collectGate.cancel()
  collectState.track = null
  collectState.loading = false
  collectState.busy = false
  collectState.error = ''
})

function applyCollectedPlaylist(detail) {
  invalidatePlaylistReads(detail.id)
  const { tracks, ...summary } = detail
  const index = playlistState.playlists.findIndex(item => item.id === detail.id)
  const ownerFilter = sessionState.me?.isAdmin ? $('#ownerFilter').value : ''
  if (index >= 0) playlistState.playlists[index] = summary
  else if (!ownerFilter || String(detail.userId) === ownerFilter) playlistState.playlists.unshift(summary)
  playlistState.playlists.sort((a, b) => b.updatedAt - a.updatedAt)
  if (playlistState.current?.id === detail.id && !playlistState.tracksDirty && !playlistState.metaDirty) {
    playlistState.current = detail
    playlistState.draftTracks = tracks.map(track => ({ ...track }))
    playlistState.draftRevision = detail.tracksRevision
    renderDetail()
  }
  renderPlaylistList()
}

$('#collectForm').addEventListener('submit', async event => {
  event.preventDefault()
  if (collectState.busy || collectState.loading || !collectState.track || !sessionState.me) return
  const form = event.currentTarget, creating = form.elements.mode.value === 'create'
  const targetID = form.elements.playlistId.value
  if (!creating && LXSCMusic.collectionBlocked(targetID, playlistState.current?.id, playlistState.tracksDirty, playlistState.metaDirty)) return renderCollectForm()
  if (creating ? !form.elements.name.value.trim() : !targetID) return
  const path = creating ? '/playlists' : '/playlists/' + encodeURIComponent(targetID) + '/tracks'
  const body = creating ? { name: form.elements.name.value.trim(), public: form.elements.public.checked, trackIds: [collectState.track.id] } : { trackId: collectState.track.id }
  const request = collectGate.begin()
  collectState.busy = true
  collectState.error = ''
  renderCollectForm()
  try {
    const result = await playlistAPI(path, { method: 'POST', body, signal: request.signal })
    if (!request.current()) return
    const detail = creating ? result : result.playlist
    applyCollectedPlaylist(detail)
    $('#collectDialog').close()
    toast(!creating && !result.added ? `歌曲已在「${detail.name}」中` : `已收藏到「${detail.name}」`)
  } catch (error) { if (request.current() && !isAbort(error)) collectState.error = error.message }
  finally { if (request.current()) { collectState.busy = false; renderCollectForm() } }
})

function resetMusicSession() {
  sessionState.epoch++
  for (const controller of sessionState.requests) controller.abort()
  sessionState.requests.clear()
  songSearchGate.cancel()
  playlistSearchGate.cancel()
  playlistDetailGate.cancel()
  playlistListGate.cancel()
  playlistDetailTarget = ''
  seekDragging = false
  collectGate.cancel()
  collectState.track = null
  collectState.playlists = []
  collectState.loading = false
  collectState.busy = false
  collectState.error = ''
  if ($('#collectDialog').open) $('#collectDialog').close()
  songSearchState.tracks = []
  playlistState.searchResults = []
  $('#songSearchForm').reset()
  $('#searchForm').reset()
  searchMessage('输入关键词开始搜索')
  $('#searchResults').innerHTML = ''
  clearTimeout(toastTimer)
  $('#toast').classList.add('hidden')
  webPlayer.clear()
}

// 所有控件与清理逻辑就绪后再恢复登录，避免脚本加载顺序导致旧会话残留。
authAPI('/me').then(initializeSession).catch(error => { if (!isAbort(error)) showLogin() })
