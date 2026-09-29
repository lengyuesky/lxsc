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
    <td class="track-actions"><button type="button" title="播放" aria-label="播放 ${esc(track.name)}" data-track-action="play" ${track.unavailable ? 'disabled' : ''}><svg class="icon"><use href="#i-play"/></svg></button>${canEdit ? `<button type="button" title="上移" data-track-action="up" ${index === 0 ? 'disabled' : ''}><svg class="icon"><use href="#i-up"/></svg></button><button type="button" title="下移" data-track-action="down" ${index === playlistState.draftTracks.length - 1 ? 'disabled' : ''}><svg class="icon"><use href="#i-down"/></svg></button><button type="button" title="移除" class="remove" data-track-action="remove"><svg class="icon"><use href="#i-x"/></svg></button>` : ''}</td>
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
    if (row.dataset.trackIndex !== String(index)) row.dataset.trackIndex = String(index)
    if (row.firstElementChild.textContent !== String(index + 1)) row.firstElementChild.textContent = String(index + 1)
    const up = row.querySelector('[data-track-action=up]'), down = row.querySelector('[data-track-action=down]')
    if (up && up.disabled !== (index === 0)) up.disabled = index === 0
    if (down && down.disabled !== (index === playlistState.draftTracks.length - 1)) down.disabled = index === playlistState.draftTracks.length - 1
  })
  for (const row of Array.from(body.children)) { if (!keep.has(row)) row.remove() }
  if (focused?.isConnected && document.activeElement !== focused) focused.focus({ preventScroll: true })
  renderPlaybackMarkers()
}

function markTracksDirty() {
  playlistState.editVersion++
  playlistState.tracksDirty = true
  renderTracks()
  renderSearchResults()
}

function moveTrack(from, to) {
  if (to < 0 || to >= playlistState.draftTracks.length || from === to) return
  const [track] = playlistState.draftTracks.splice(from, 1)
  playlistState.draftTracks.splice(to, 0, track)
  markTracksDirty()
}
