// 歌单编辑复用已有行，只更新顺序、序号和按钮状态，保留焦点与重复歌曲。
function renderTracks() {
  const canEdit = !!playlistState.current?.canEdit
  $('#trackCount').textContent = `(${playlistState.draftTracks.length} 首)`
  $('#saveTracksButton').disabled = !canEdit || !playlistState.tracksDirty
  renderCollectForm()
  document.querySelector('.tracks-card .track-tip').classList.toggle('hidden', !canEdit)
  const body = $('#trackTable')
  if (!playlistState.draftTracks.length) {
    body.innerHTML = `<tr><td colspan="6" class="muted">${canEdit ? '歌单还是空的，可从上方搜索添加歌曲。' : '该歌单暂无歌曲。'}</td></tr>`
    return
  }
  const focused = document.activeElement
  const previous = new Map()
  for (const row of body.children) {
    if (!row.dataset.playingId) { row.remove(); continue }
    const rows = previous.get(row.dataset.playingId) || []
    rows.push(row)
    previous.set(row.dataset.playingId, rows)
  }
  const keep = new Set()
  let cursor = body.firstElementChild
  playlistState.draftTracks.forEach((track, index) => {
    let row = previous.get(track.id)?.shift()
    const signature = JSON.stringify([track, canEdit])
    if (!row || row.trackSignature !== signature) {
      const holder = document.createElement('template')
      holder.innerHTML = `<tr class="track-row" data-track-index="${index}" data-playing-id="${esc(track.id)}" draggable="${canEdit}">
    <td>${index + 1}</td>
    <td class="${track.unavailable ? 'unavailable' : ''}"><div class="song-title">${esc(track.name)}</div>${track.unavailable ? `<div class="song-sub">${esc(track.id)}</div>` : ''}</td>
    <td>${esc(track.singer || '')}</td><td class="muted">${esc(track.album || '')}</td><td><span class="badge">${esc(platName[track.source] || track.source || '')}</span></td>
    <td class="track-actions"><button type="button" title="播放" aria-label="播放 ${esc(track.name)}" data-track-action="play" ${track.unavailable ? 'disabled' : ''}><svg class="icon"><use href="#i-play"/></svg></button><button type="button" title="加入播放队列" aria-label="加入播放队列 ${esc(track.name)}" data-track-action="queue" ${track.unavailable ? 'disabled' : ''}>＋</button>${canEdit ? `<button type="button" title="查找其他版本" data-track-action="repair">换版本</button><button type="button" title="上移" data-track-action="up" ${index === 0 ? 'disabled' : ''}><svg class="icon"><use href="#i-up"/></svg></button><button type="button" title="下移" data-track-action="down" ${index === playlistState.draftTracks.length - 1 ? 'disabled' : ''}><svg class="icon"><use href="#i-down"/></svg></button><button type="button" title="移除" class="remove" data-track-action="remove"><svg class="icon"><use href="#i-x"/></svg></button>` : ''}</td>
  </tr>`
      const fresh = holder.content.firstElementChild
      if (row) row.replaceWith(fresh)
      if (cursor === row) cursor = fresh
      row = fresh
      row.trackSignature = signature
    }
    keep.add(row)
    if (row !== cursor) body.insertBefore(row, cursor)
    cursor = row.nextElementSibling
    updateTrackPosition(row, index, playlistState.draftTracks.length)
  })
  for (const row of Array.from(body.children)) { if (!keep.has(row)) row.remove() }
  if (focused?.isConnected && document.activeElement !== focused) focused.focus({ preventScroll: true })
  renderPlaybackMarkers()
}

function updateTrackPosition(row, index, count) {
  if (row.dataset.trackIndex !== String(index)) row.dataset.trackIndex = String(index)
  if (row.firstElementChild.textContent !== String(index + 1)) row.firstElementChild.textContent = String(index + 1)
  const buttons = row.trackPositionButtons ||= [row.querySelector('[data-track-action=up]'), row.querySelector('[data-track-action=down]')]
  if (buttons[0] && buttons[0].disabled !== (index === 0)) buttons[0].disabled = index === 0
  if (buttons[1] && buttons[1].disabled !== (index === count - 1)) buttons[1].disabled = index === count - 1
}

function markTracksDirty() {
  playlistState.editVersion++
  playlistState.tracksDirty = true
  renderTracks()
  renderSearchResults()
}

function moveTrack(from, to) {
  const count = playlistState.draftTracks.length
  if (!playlistState.current?.canEdit || !Number.isInteger(from) || !Number.isInteger(to) || from < 0 || from >= count || to < 0 || to >= count || from === to) return
  const body = $('#trackTable'), row = body.children[from], target = body.children[to]
  const focused = document.activeElement
  const [track] = playlistState.draftTracks.splice(from, 1)
  playlistState.draftTracks.splice(to, 0, track)
  playlistState.editVersion++
  playlistState.tracksDirty = true
  if (!row || !target || body.children.length !== count) { renderTracks(); return }
  // 只移动现有行，更新受影响区间；搜索集合与播放标记均未改变。
  body.insertBefore(row, from < to ? target.nextElementSibling : target)
  for (let index = Math.min(from, to); index <= Math.max(from, to); index++) updateTrackPosition(body.children[index], index, count)
  $('#saveTracksButton').disabled = false
  renderCollectForm()
  if (focused?.isConnected && document.activeElement !== focused) focused.focus({ preventScroll: true })
}
