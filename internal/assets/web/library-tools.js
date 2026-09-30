// 歌单修复通过现有草稿保存；订阅使用版本检查，不覆盖未保存编辑。
const repairGate = new LXSCMusic.RequestGate(), subscriptionGate = new LXSCMusic.RequestGate()
let repairTarget = null, repairTracks = [], subscriptionID = '', subscriptionPreview = null
function resetLibraryTools() {
  resetPlaylistHistory(); repairGate.cancel(); subscriptionGate.cancel(); repairTarget = null; repairTracks = []; subscriptionID = ''; subscriptionPreview = null
  for (const id of ['repairDialog', 'subscriptionDialog']) if ($('#' + id).open) $('#' + id).close()
}
function openRepair(index) {
  const track = playlistState.draftTracks[index]
  if (!track || !playlistState.current?.canEdit) return
  repairTarget = { playlist: playlistState.current.id, track }
  $('#repairForm').elements.query.value = track.unavailable ? '' : [track.name, track.singer].filter(Boolean).join(' ')
  $('#repairOriginal').textContent = '待替换：' + track.name + ' · ' + track.id
  $('#repairResults').replaceChildren(); $('#repairDialog').showModal()
}
$('#repairForm').addEventListener('submit', async event => {
  event.preventDefault(); const request = repairGate.begin(), target = repairTarget
  $('#repairResults').textContent = '正在查找其他版本…'
  try {
    const tracks = await playlistAPI('/search', { method: 'POST', body: { query: event.currentTarget.elements.query.value }, signal: request.signal })
    if (!request.current() || !target) return
    const normalized = value => String(value || '').toLowerCase().replace(/[\s·•]/g, '')
    const score = track => Number(normalized(track.name) === normalized(target.track.name)) * 4 + Number(normalized(track.singer) === normalized(target.track.singer)) * 3 + Number(track.duration > 0 && target.track.duration > 0 && Math.abs(track.duration - target.track.duration) <= 5) * 2
    repairTracks = tracks.filter(track => track.id !== target.track.id).sort((a, b) => score(b) - score(a))
    $('#repairResults').innerHTML = repairTracks.map((track, index) => `<div class="queue-entry"><div>${esc(track.name)}<small class="song-sub">${esc(track.singer)} · ${esc(track.album)} · ${esc(platName[track.source])} · ${formatSongTime(track.duration)}</small></div><div class="actions"><button class="sec sm" data-repair-play="${index}">试听</button><button class="sm" data-repair-use="${index}">替换到草稿</button></div></div>`).join('') || '<p class="muted">没有其他候选，请调整关键词</p>'
  } catch (error) { if (request.current() && !isAbort(error)) $('#repairResults').textContent = error.message }
})
$('#repairResults').addEventListener('click', event => {
  const play = event.target.closest('[data-repair-play]'); if (play) return webPlayer.playList(repairTracks, Number(play.dataset.repairPlay))
  const button = event.target.closest('[data-repair-use]'); if (!button || !repairTarget) return
  const track = repairTracks[Number(button.dataset.repairUse)], index = playlistState.draftTracks.indexOf(repairTarget.track)
  if (!track || playlistState.current?.id !== repairTarget.playlist || !playlistState.current?.canEdit || index < 0) return toast('歌单已变化，请重新选择歌曲', true)
  playlistState.draftTracks[index] = { ...track }; markTracksDirty(); $('#repairDialog').close(); toast('已替换到草稿，请保存歌曲变更')
})
$('#closeRepair').addEventListener('click', () => $('#repairDialog').close())
$('#repairDialog').addEventListener('close', () => { repairGate.cancel(); repairTarget = null; repairTracks = [] })
$('#subscriptionOpen').addEventListener('click', async () => {
  if (!playlistState.current?.canEdit) return toast('请选择自己的歌单', true)
  if (playlistState.tracksDirty || playlistState.metaDirty) return toast('请先保存或放弃歌单草稿', true)
  subscriptionID = playlistState.current.id; subscriptionPreview = null
  const request = subscriptionGate.begin()
  $('#subscriptionForm').reset(); $('#subscriptionApply').disabled = true; $('#subscriptionStatus').textContent = '正在读取…'; $('#subscriptionDialog').showModal()
  try {
    const sub = await playlistAPI('/playlists/' + encodeURIComponent(subscriptionID) + '/subscription', { signal: request.signal })
    if (!request.current()) return
    if (sub) { const form = $('#subscriptionForm'); form.elements.source.value = sub.source; form.elements.input.value = sub.remoteId; form.elements.auto.checked = sub.auto }
    $('#subscriptionStatus').textContent = sub ? `${sub.lastAt ? '最近更新：' + formatTime(sub.lastAt) : '尚未更新'}${sub.error ? ' · ' + sub.error : ''}` : '保存来源后，可预览并追加新歌'
  } catch (error) { if (request.current() && !isAbort(error)) $('#subscriptionStatus').textContent = error.message }
})
function subscriptionPath() { return '/playlists/' + encodeURIComponent(subscriptionID) + '/subscription' }
function invalidateSubscriptionPreview() { subscriptionPreview = null; $('#subscriptionApply').disabled = true }
$('#subscriptionForm').addEventListener('input', invalidateSubscriptionPreview)
$('#subscriptionForm').addEventListener('submit', async event => {
  event.preventDefault(); const form = event.currentTarget, request = subscriptionGate.begin()
  invalidateSubscriptionPreview()
  try {
    await playlistAPI(subscriptionPath(), { method: 'PUT', signal: request.signal, body: { source: form.elements.source.value, input: form.elements.input.value, auto: form.elements.auto.checked } })
    if (request.current()) $('#subscriptionStatus').textContent = '来源已保存；自动更新首次将在24小时后执行'
  } catch (error) { if (request.current() && !isAbort(error)) $('#subscriptionStatus').textContent = error.message }
})
$('#subscriptionPreview').addEventListener('click', async () => {
  const request = subscriptionGate.begin(); invalidateSubscriptionPreview(); $('#subscriptionStatus').textContent = '正在读取已保存的源歌单…'
  try {
    const result = await playlistAPI(subscriptionPath() + '/sync', { method: 'POST', signal: request.signal, body: { preview: true } })
    if (!request.current()) return
    subscriptionPreview = result
    $('#subscriptionApply').disabled = !!(result.truncated || result.skipped)
    $('#subscriptionStatus').textContent = `新增 ${result.added} 首；源端移除 ${result.removed} 首（本地保留）。${result.truncated || result.skipped ? '源歌单未完整读取，暂不应用更新。' : '确认后仅追加新歌。'}`
  } catch (error) { if (request.current() && !isAbort(error)) $('#subscriptionStatus').textContent = error.message }
})
$('#subscriptionApply').addEventListener('click', async () => {
  if (!subscriptionPreview || playlistState.current?.id !== subscriptionID || playlistState.tracksDirty || playlistState.metaDirty) return toast('请重新打开并预览订阅', true)
  const preview = subscriptionPreview, request = subscriptionGate.begin(); invalidateSubscriptionPreview()
  try {
    const detail = await playlistAPI(subscriptionPath() + '/sync', { method: 'POST', signal: request.signal, body: { preview: false, expectedRevision: preview.expectedRevision, version: preview.version, remoteRevision: preview.remoteRevision } })
    if (request.current()) { applyCollectedPlaylist(detail); $('#subscriptionStatus').textContent = '更新完成，现有歌曲和顺序已保留' }
  } catch (error) { if (request.current() && !isAbort(error)) $('#subscriptionStatus').textContent = error.message }
})
$('#subscriptionRemove').addEventListener('click', async () => {
  if (!confirm('取消来源订阅？已经导入的歌曲会保留。')) return
  const request = subscriptionGate.begin(); invalidateSubscriptionPreview()
  try { await playlistAPI(subscriptionPath(), { method: 'PUT', signal: request.signal, body: { source: '' } }); if (request.current()) { $('#subscriptionForm').reset(); $('#subscriptionStatus').textContent = '已取消订阅' } }
  catch (error) { if (request.current() && !isAbort(error)) $('#subscriptionStatus').textContent = error.message }
})
$('#closeSubscription').addEventListener('click', () => $('#subscriptionDialog').close())
$('#subscriptionDialog').addEventListener('close', () => { subscriptionGate.cancel(); subscriptionID = ''; invalidateSubscriptionPreview() })
$('#smartTracksForm').addEventListener('submit', async event => {
  event.preventDefault(); const form = event.currentTarget, request = songSearchGate.begin()
  const kind = form.elements.kind.value
  songSearchState.smartKind = ''; $('#smartSave').disabled = true
  songSearchState.results = null; songSearchState.tracks = []; $('#searchPaging').replaceChildren(); searchMessage('正在生成智能歌单…')
  try {
    const tracks = await playlistAPI('/smart?' + new URLSearchParams({ kind: form.elements.kind.value, singer: form.elements.singer.value }), { signal: request.signal })
    if (!request.current()) return
    songSearchState.smartKind = kind; songSearchState.tracks = tracks; renderSongResults(); $('#songSearchStatus').textContent = `${({ frequent: '近30天常听', month: '本月最爱', rediscover: '久未听的收藏', singer: '歌手收藏' })[kind]} · ${tracks.length} 首（最多100首，可播放或另存歌单）`
    $('#smartSave').disabled = !tracks.length
  } catch (error) { if (request.current() && !isAbort(error)) searchMessage(error.message, true) }
})
$('#smartSave').addEventListener('click', async () => {
  if (!songSearchState.smartKind || !songSearchState.tracks.length || songSearchState.results) return toast('请先生成智能歌单', true)
  const name = prompt('新歌单名称', '我的智能歌单'); if (!name?.trim()) return
  try { const detail = await playlistAPI('/playlists', { method: 'POST', body: { name: name.trim(), public: false, trackIds: songSearchState.tracks.map(track => track.id) } }); applyCollectedPlaylist(detail); toast('已保存为个人歌单') }
  catch (error) { if (!isAbort(error)) toast(error.message, true) }
})

const playlistHistoryGate = new LXSCMusic.RequestGate()
let playlistHistoryState = null
function resetPlaylistHistory() {
  playlistHistoryGate.cancel(); playlistHistoryState = null
  if ($('#playlistHistoryDialog').open) $('#playlistHistoryDialog').close()
  $('#playlistHistoryList').replaceChildren(); $('#playlistHistoryTracks').replaceChildren()
  $('#restorePlaylistHistory').disabled = true
}
$('#playlistHistoryOpen').addEventListener('click', async () => {
  if (!playlistState.current?.canEdit) return
  if (playlistState.tracksDirty || playlistState.metaDirty) return toast('请先保存或放弃当前草稿', true)
  resetPlaylistHistory()
  const request = playlistHistoryGate.begin(), id = playlistState.current.id
  playlistHistoryState = { id, editVersion: playlistState.editVersion, tracks: null }
  $('#playlistHistoryStatus').textContent = '正在读取历史…'; $('#playlistHistoryDialog').showModal()
  try {
    const history = await playlistAPI('/playlists/' + encodeURIComponent(id) + '/history', { signal: request.signal })
    if (!request.current()) return
    $('#playlistHistoryList').innerHTML = history.map((h, i) => `<button class="sec sm" data-history-id="${h.id}">${i + 1}. ${esc(formatTime(h.createdAt))} · ${h.count} 首</button>`).join('')
    $('#playlistHistoryStatus').textContent = history.length ? '选择一次保存前的记录以预览歌曲' : '暂无历史，成功保存歌曲变更后会保留原列表'
  } catch (error) { if (request.current() && !isAbort(error)) $('#playlistHistoryStatus').textContent = error.message }
})
$('#playlistHistoryList').addEventListener('click', async event => {
  const button = event.target.closest('[data-history-id]'), state = playlistHistoryState
  if (!button || !state) return
  const request = playlistHistoryGate.begin()
  state.tracks = null; $('#restorePlaylistHistory').disabled = true; $('#playlistHistoryStatus').textContent = '正在读取歌曲…'
  try {
    const tracks = await playlistAPI('/playlists/' + encodeURIComponent(state.id) + '/history/' + button.dataset.historyId, { signal: request.signal })
    if (!request.current()) return
    state.tracks = tracks
    $('#playlistHistoryTracks').innerHTML = tracks.map((t, i) => `<div class="queue-entry"><div>${i + 1}. ${esc(t.name)}<small class="song-sub">${esc(t.singer)} · ${esc(platName[t.source] || '')} · ${esc(t.id)}</small></div></div>`).join('') || '<p class="muted">空歌单</p>'
    $('#playlistHistoryStatus').textContent = tracks.some(t => t.unavailable) ? '部分歌曲元数据不可用；恢复到草稿后可查找其他版本' : `共${tracks.length}首，原顺序及重复项保持不变`
    $('#restorePlaylistHistory').disabled = false
  } catch (error) { if (request.current() && !isAbort(error)) $('#playlistHistoryStatus').textContent = error.message }
})
$('#restorePlaylistHistory').addEventListener('click', () => {
  const state = playlistHistoryState
  if (!state?.tracks || !playlistState.current?.canEdit || playlistState.current.id !== state.id || playlistState.editVersion !== state.editVersion) return toast('歌单已变化，请重新读取历史', true)
  playlistState.draftTracks = state.tracks.map(t => ({ ...t }))
  markTracksDirty(); $('#playlistHistoryDialog').close(); toast('原歌曲列表已恢复到草稿，请确认后保存')
})
$('#closePlaylistHistory').addEventListener('click', () => $('#playlistHistoryDialog').close())
$('#playlistHistoryDialog').addEventListener('close', () => { playlistHistoryGate.cancel(); playlistHistoryState = null })
