// 播放队列、歌词与系统媒体控制；本地只保存歌曲信息，不保存凭据或音频地址。
let experienceKey = '', experienceSavedAt = 0, queueMarker = ''
let mediaTrack = ''
let lyricTrack = '', lyricData = null, lyricLine = -1
const lyricGate = new LXSCMusic.RequestGate()
function savePlaybackExperience(force = false) {
  if (!experienceKey || !sessionState.me || (!force && Date.now() - experienceSavedAt < 5000)) return
  experienceSavedAt = Date.now()
  try {
    if (webPlayer.track) localStorage.setItem(experienceKey, JSON.stringify({ ...webPlayer.snapshot(), savedAt: Date.now() }))
    else localStorage.removeItem(experienceKey)
  } catch { /* 存储被禁用或配额不足时仍可播放。 */ }
}
function initializePlaybackExperience() {
  experienceKey = 'lxsc.player.' + sessionState.me.id + '.' + encodeURIComponent(sessionState.me.name)
  try {
    const saved = JSON.parse(localStorage.getItem(experienceKey) || 'null')
    if (saved && Date.now() - saved.savedAt < 30 * 86400000) webPlayer.restore(saved)
  } catch { /* 损坏的快照不阻止登录。 */ }
}
function resetPlaybackExperience(erase = false) {
  if (erase && experienceKey) { try { localStorage.removeItem(experienceKey) } catch {} }
  experienceKey = ''; mediaTrack = ''; queueMarker = ''; lyricTrack = ''; lyricData = null; lyricLine = -1
  lyricGate.cancel()
  for (const id of ['queueDialog', 'lyricsDialog', 'queueAddDialog']) if ($('#' + id).open) $('#' + id).close()
  $('#queueList').replaceChildren(); $('#lyricsLines').replaceChildren()
  if ('mediaSession' in navigator) { navigator.mediaSession.metadata = null; navigator.mediaSession.playbackState = 'none' }
}
function renderPlaybackExperience(state) {
  $('#playerRepeat').value = state.repeat
  $('#playerShuffle').setAttribute('aria-pressed', String(state.shuffle))
  const marker = `${state.queueVersion}:${state.index}`
  if ($('#queueDialog').open && marker !== queueMarker) { queueMarker = marker; renderQueue() }
  if ($('#lyricsDialog').open) {
    if (state.track?.id !== lyricTrack) loadPlayerLyrics()
    else highlightLyrics(state.time)
  }
  if ('mediaSession' in navigator) {
    const media = navigator.mediaSession
    if (!state.track) { media.metadata = null; mediaTrack = '' }
    if (state.track && mediaTrack !== state.track.id) {
      mediaTrack = state.track.id
      if ('MediaMetadata' in window) media.metadata = new MediaMetadata({ title: state.track.name || '', artist: state.track.singer || '', album: state.track.album || '' })
    }
    media.playbackState = state.status === 'playing' ? 'playing' : state.track ? 'paused' : 'none'
    if (media.setPositionState) {
      try { if (state.duration > 0) media.setPositionState({ duration: state.duration, playbackRate: 1, position: Math.min(state.duration, Math.max(0, state.time)) }); else media.setPositionState() } catch {}
    }
  }
  savePlaybackExperience()
}
function renderQueue() {
  $('#queueList').innerHTML = webPlayer.queue.map((track, index) => `<div class="queue-entry ${index === webPlayer.index ? 'now-playing' : ''}"><button type="button" class="ghost queue-song" data-queue-action="play" data-index="${index}">${index + 1}. ${esc(track.name)}<small>${esc(track.singer)}</small></button><div class="actions"><button class="sec sm" data-queue-action="up" data-index="${index}" ${index === 0 ? 'disabled' : ''} aria-label="上移 ${esc(track.name)}">↑</button><button class="sec sm" data-queue-action="down" data-index="${index}" ${index === webPlayer.queue.length - 1 ? 'disabled' : ''} aria-label="下移 ${esc(track.name)}">↓</button><button class="danger sm" data-queue-action="remove" data-index="${index}" aria-label="移除 ${esc(track.name)}">移除</button></div></div>`).join('') || '<p class="muted">队列为空</p>'
}
$('#playerQueue').addEventListener('click', () => { renderQueue(); $('#queueDialog').showModal() })
$('#closeQueue').addEventListener('click', () => $('#queueDialog').close())
$('#queueList').addEventListener('click', event => {
  const button = event.target.closest('[data-queue-action]'); if (!button || button.disabled) return
  const index = Number(button.dataset.index)
  const actions = { play: () => webPlayer.playAt(index), up: () => webPlayer.move(index, index - 1), down: () => webPlayer.move(index, index + 1), remove: () => webPlayer.remove(index) }
  actions[button.dataset.queueAction]?.(); savePlaybackExperience(true)
})
$('#playerRepeat').addEventListener('change', event => { webPlayer.setRepeat(event.target.value); savePlaybackExperience(true) })
$('#playerShuffle').addEventListener('click', () => { webPlayer.setShuffle(!webPlayer.shuffle); savePlaybackExperience(true) })
window.addEventListener('pagehide', () => savePlaybackExperience(true))
async function loadPlayerLyrics() {
  const track = webPlayer.track
  lyricGate.cancel(); lyricData = null; lyricLine = -1; lyricTrack = track?.id || ''
  $('#lyricsTitle').textContent = track ? track.name + ' · ' + track.singer : '歌词'
  $('#lyricsLines').textContent = track ? '正在读取歌词…' : '请先选择歌曲'
  if (!track) return
  const request = lyricGate.begin()
  try {
    const data = await playlistAPI('/lyrics?id=' + encodeURIComponent(track.id), { signal: request.signal })
    if (!request.current() || webPlayer.track?.id !== track.id) return
    lyricData = data
    const translated = new Map((data.trans || []).map(line => [line.start, line.value]))
    $('#lyricsLines').innerHTML = (data.lines || []).map((line, index) => `<button type="button" class="lyric-line ghost" data-line="${index}" ${data.synced ? '' : 'disabled'}>${esc(line.value)}${translated.get(line.start) ? `<small>${esc(translated.get(line.start))}</small>` : ''}</button>`).join('') || '<p class="muted">暂无歌词</p>'
    highlightLyrics(webPlayer.state.time)
  } catch (error) { if (request.current() && !isAbort(error)) $('#lyricsLines').textContent = error.message }
}
function highlightLyrics(time) {
  if (!lyricData?.synced) return
  const lines = lyricData.lines || [], milliseconds = time * 1000 + (lyricData.offset || 0)
  let active = -1
  for (let index = 0; index < lines.length && lines[index].start <= milliseconds; index++) active = index
  if (active === lyricLine) return
  $('#lyricsLines [aria-current]')?.removeAttribute('aria-current'); lyricLine = active
  const node = $(`#lyricsLines [data-line="${active}"]`)
  if (node) { node.setAttribute('aria-current', 'true'); node.scrollIntoView({ block: 'nearest', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'instant' : 'smooth' }) }
}
$('#playerLyrics').addEventListener('click', () => { $('#lyricsDialog').showModal(); loadPlayerLyrics() })
$('#closeLyrics').addEventListener('click', () => $('#lyricsDialog').close())
$('#retryLyrics').addEventListener('click', loadPlayerLyrics)
$('#lyricsDialog').addEventListener('close', () => lyricGate.cancel())
$('#lyricsLines').addEventListener('click', event => {
  const button = event.target.closest('[data-line]'), line = lyricData?.lines?.[Number(button?.dataset.line)]
  if (button && !button.disabled && line) webPlayer.seek(Math.max(0, (line.start - (lyricData.offset || 0)) / 1000))
})
if ('mediaSession' in navigator) {
  const handlers = { play: () => webPlayer.track && webPlayer.resume(), pause: () => { if (webPlayer.wantsPlay) webPlayer.toggle() }, previoustrack: () => webPlayer.previous(), nexttrack: () => webPlayer.next(), seekto: event => webPlayer.seek(event.seekTime), seekbackward: event => webPlayer.seek(webPlayer.state.time - (event.seekOffset || 10)), seekforward: event => webPlayer.seek(webPlayer.state.time + (event.seekOffset || 10)) }
  for (const [action, handler] of Object.entries(handlers)) { try { navigator.mediaSession.setActionHandler(action, handler) } catch {} }
}

let queueAddTrack = null
function openQueueAdd(track) {
  if (!track || track.unavailable || !sessionState.me) return
  queueAddTrack = { ...track }
  $('#queueAddTitle').textContent = track.name + ' · ' + (track.singer || '')
  $('#queueAddDialog').showModal()
}
function addSelectedToQueue(next) {
  if (!queueAddTrack || !sessionState.me) return
  if (webPlayer.queue.length >= 2000) return toast('播放队列最多2000首，请先移除部分歌曲', true)
  webPlayer.enqueue(queueAddTrack, next)
  savePlaybackExperience(true)
  $('#queueAddDialog').close()
  toast(next ? '已加入下一首' : '已加入队列末尾')
}
$('#queueAddNext').addEventListener('click', () => addSelectedToQueue(true))
$('#queueAddLast').addEventListener('click', () => addSelectedToQueue(false))
$('#closeQueueAdd').addEventListener('click', () => $('#queueAddDialog').close())
$('#queueAddDialog').addEventListener('close', () => { queueAddTrack = null })
