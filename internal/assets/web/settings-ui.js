// 系统与榜单设置共享加载状态，向页面暴露明确入口。
const LXSCSettings = (() => {
// ---- 设置 ----
const settingsState = { request: 0, ready: false, saving: false, dirty: false }
const boardSettingsState = { request: 0, ready: false, saving: false, dirty: false }
const boardChoices = new LXSCBoardSettings.BoardSelectionState(
  (source, signal) => adminAPI('/boards?source=' + encodeURIComponent(source), { signal }),
  source => renderBoardSource(source),
)

function resetSettingsSession() {
  for (const [state, form, message] of [[settingsState, '#settingsForm', '#settingsMsg'], [boardSettingsState, '#boardSettingsForm', '#boardSettingsMsg']]) {
    state.request++
    state.ready = false
    state.saving = false
    state.dirty = false
    $(form).inert = true
    $(message).textContent = ''
  }
  boardChoices.reset()
  $('#boardSelections').replaceChildren()
  $('#boardSettingsPanel').open = false
  $('#boardSettingsForm').classList.add('hidden')
  $('#boardSettingsLoadNotice').classList.add('hidden')
}

function markSettingsDirty() {
  if (!settingsState.ready || settingsState.saving) return
  settingsState.dirty = true
  $('#settingsMsg').textContent = '有未保存的修改'
}

function markBoardSettingsDirty() {
  if (!boardSettingsState.ready || boardSettingsState.saving) return
  boardSettingsState.dirty = true
  $('#boardSettingsMsg').className = 'muted'
  $('#boardSettingsMsg').textContent = '有未保存的修改'
}

function syncBoardSettings() {
  const form = $('#boardSettingsForm'), root = $('#boardSelections')
  const sources = [...new Set(form.elements.boardSources.value.split(/[,，\s]+/).filter(source => ['wy', 'tx', 'kw', 'kg', 'mg'].includes(source)))]
  $('#boardDisplayHint').textContent = form.elements.showBoards.checked
    ? '在线音乐目录和客户端歌单列表共用以下选择，保存后刷新客户端列表。'
    : '榜单展示已关闭；下方选择仍会保存，重新开启后恢复展示。'
  root.querySelectorAll('[data-board-source]').forEach(group => { if (!sources.includes(group.dataset.boardSource)) group.remove() })
  root.querySelector('[data-board-empty]')?.remove()
  if (!sources.length) root.innerHTML = '<p class="muted hint" data-board-empty>尚未选择有效平台，请在上方填写平台代码。</p>'
  for (const source of sources) {
    let group = root.querySelector(`[data-board-source="${source}"]`)
    if (!group) {
      group = document.createElement('details')
      group.className = 'board-platform'
      group.dataset.boardSource = source
      group.open = boardChoices.isCustom(source)
      group.innerHTML = `<summary><strong>${esc(platName[source])}</strong><span data-board-summary></span></summary><div class="board-content" data-board-content></div>`
    }
    root.append(group)
    renderBoardSource(source)
    void boardChoices.load(source)
  }
}

function renderBoardSource(source) {
  const group = $('#boardSelections').querySelector(`[data-board-source="${source}"]`)
  if (!group) return
  const catalog = boardChoices.catalog(source), custom = boardChoices.isCustom(source)
  const selected = new Set(boardChoices.selections[source] || []), options = boardChoices.options(source)
  const summary = group.querySelector('[data-board-summary]')
  summary.textContent = (custom ? `已选 ${selected.size} 个` : '全部榜单') + (catalog.loading ? ' · 加载中…' : catalog.error ? ' · 加载失败' : catalog.loaded ? ` · 目录 ${catalog.boards.length} 个` : '')
  const content = group.querySelector('[data-board-content]'), scrollTop = content.querySelector('.board-options')?.scrollTop || 0
  const focused = content.contains(document.activeElement) ? document.activeElement : null
  const focusAttr = focused && ['data-board-id', 'data-board-mode', 'data-board-action'].find(attr => focused.hasAttribute(attr))
  const focusSelector = focusAttr ? `[${focusAttr}="${CSS.escape(focused.getAttribute(focusAttr))}"]` : null
  const count = custom ? `已选 ${selected.size} 个榜单` : `展示全部 ${catalog.boards.length} 个榜单`
  content.innerHTML = `<label>展示方式<select data-board-mode aria-label="${esc(platName[source])}榜单展示方式"><option value="all" ${custom ? '' : 'selected'}>全部榜单</option><option value="custom" ${custom ? 'selected' : ''} ${!catalog.loaded && !custom ? 'disabled' : ''}>自定义</option></select></label>
    <p class="muted hint">${custom ? '只展示勾选的榜单；新榜单不会自动加入。清空后隐藏该平台入口。' : '包含以后新增的榜单；切换到自定义可逐项选择。'}</p>
    ${catalog.error ? `<p class="err hint" role="alert">${esc(catalog.error)}，已有选择已保留。</p>` : ''}
    <div class="board-actions"><span class="muted hint" role="status">${catalog.loading ? '正在加载榜单名称…' : catalog.loaded || custom ? count : '榜单名称尚未加载'}</span>
      ${custom ? `<button type="button" class="sec sm" data-board-action="all" ${catalog.loaded ? '' : 'disabled'}>全选</button><button type="button" class="sec sm" data-board-action="clear">清空</button>` : ''}
      <button type="button" class="sec sm" data-board-action="reload" ${catalog.loading ? 'disabled' : ''}>${catalog.error ? '重试' : '刷新榜单'}</button></div>
    ${options.length ? `<div class="board-options">${options.map(board => `<label><input type="checkbox" data-board-id="${esc(board.bangid)}" ${!custom || selected.has(board.bangid) ? 'checked' : ''} ${custom ? '' : 'disabled'}><span>${esc(board.name)}${board.missing ? `<small>${catalog.loaded && !catalog.error ? '当前目录未返回，选择仍保留' : '已保存的选择，名称暂不可用'}</small>` : ''}</span></label>`).join('')}</div>` : catalog.loaded ? '<p class="muted hint">该平台暂未返回可选榜单。</p>' : ''}`
  if (focusSelector) content.querySelector(focusSelector)?.focus({ preventScroll: true })
  const list = content.querySelector('.board-options')
  if (list) list.scrollTop = scrollTop
}

$('#boardSelections').addEventListener('change', event => {
  const input = event.target, source = input.closest('[data-board-source]')?.dataset.boardSource
  if (!source) return
  if (input.hasAttribute('data-board-mode')) boardChoices.setMode(source, input.value)
  else if (input.hasAttribute('data-board-id')) boardChoices.toggle(source, input.dataset.boardId, input.checked)
})
$('#boardSelections').addEventListener('click', event => {
  const button = event.target.closest('[data-board-action]')
  if (!button) return
  const source = button.closest('[data-board-source]').dataset.boardSource
  if (button.dataset.boardAction === 'reload') { void boardChoices.load(source, true); return }
  if (button.dataset.boardAction === 'all') boardChoices.selectAll(source)
  else if (button.dataset.boardAction === 'clear') boardChoices.clear(source)
  markBoardSettingsDirty()
})
$('#boardSettingsForm').addEventListener('input', event => {
  markBoardSettingsDirty()
  // 输入时调整平台分组，避免失焦后才改变布局而使保存按钮在点击途中移动。
  if (event.target.name === 'boardSources') syncBoardSettings()
})
$('#boardSettingsForm').addEventListener('change', event => {
  markBoardSettingsDirty()
  if (event.target.name === 'showBoards') syncBoardSettings()
})
$('#settingsForm').addEventListener('input', markSettingsDirty)
$('#settingsForm').addEventListener('change', markSettingsDirty)
$('#boardSettingsPanel').addEventListener('toggle', event => {
  if (event.currentTarget.open) void loadBoardSettings()
})

async function loadBoardSettings() {
  if (!sessionState.me?.isAdmin || boardSettingsState.saving || (boardSettingsState.ready && boardSettingsState.dirty)) return
  const request = ++boardSettingsState.request, form = $('#boardSettingsForm')
  form.inert = true
  form.setAttribute('aria-busy', 'true')
  $('#boardSettingsLoadNotice').classList.remove('hidden')
  $('#boardSettingsLoadMsg').className = ''
  $('#boardSettingsLoadMsg').textContent = '正在读取榜单设置…'
  $('#retryBoardSettings').classList.add('hidden')
  try {
    const values = await adminAPI('/settings')
    if (request !== boardSettingsState.request) return
    form.elements.showBoards.checked = !!values.showBoards
    form.elements.boardSources.value = (values.boardSources || []).join(',')
    boardSettingsState.ready = true
    boardSettingsState.dirty = false
    $('#boardSettingsMsg').textContent = ''
    boardChoices.reset(values.boardSelections || {})
    $('#boardSelections').replaceChildren()
    syncBoardSettings()
    form.classList.remove('hidden')
    $('#boardSettingsLoadNotice').classList.add('hidden')
  } catch (error) {
    if (request === boardSettingsState.request && !isAbort(error)) {
      $('#boardSettingsLoadMsg').className = 'err'
      $('#boardSettingsLoadMsg').textContent = '读取榜单设置失败：' + error.message
      $('#retryBoardSettings').classList.remove('hidden')
    }
  } finally {
    if (request === boardSettingsState.request) {
      form.inert = !boardSettingsState.ready
      form.setAttribute('aria-busy', 'false')
    }
  }
}

async function saveBoardSettings(event) {
  event.preventDefault()
  if (!sessionState.me?.isAdmin || !boardSettingsState.ready || boardSettingsState.saving) return false
  const form = event.target, request = ++boardSettingsState.request
  // 只提交榜单字段，避免覆盖系统设置页或当前歌单的未保存修改。
  const body = {
    showBoards: form.elements.showBoards.checked,
    boardSources: form.elements.boardSources.value.split(/[,，\s]+/).filter(Boolean),
    boardSelections: boardChoices.snapshot(),
  }
  boardSettingsState.saving = true
  form.inert = true
  form.setAttribute('aria-busy', 'true')
  try {
    await adminAPI('/settings', { method: 'PUT', body })
    if (request !== boardSettingsState.request) return false
    boardSettingsState.dirty = false
    $('#boardSettingsMsg').className = 'ok'
    $('#boardSettingsMsg').textContent = '已保存'
    toast('榜单设置已保存，客户端刷新列表后生效')
  } catch (error) {
    if (request === boardSettingsState.request && !isAbort(error)) {
      $('#boardSettingsMsg').className = 'err'
      $('#boardSettingsMsg').textContent = '保存失败，修改已保留'
      toast(error.message, true)
    }
  } finally {
    if (request === boardSettingsState.request) {
      boardSettingsState.saving = false
      form.inert = false
      form.setAttribute('aria-busy', 'false')
    }
  }
  return false
}

async function loadSettings() {
  // 导航回来时保留本页草稿；较早的设置响应不得覆盖较新的读取或保存。
  if (settingsState.saving || (settingsState.ready && settingsState.dirty)) return
  const request = ++settingsState.request
  const f = $('#settingsForm')
  f.inert = true
  f.setAttribute('aria-busy', 'true')
  try {
    const v = await adminAPI('/settings')
    if (request !== settingsState.request) return
    for (const [k, val] of Object.entries(v)) {
      const el = f.elements[k]; if (!el) continue
      if (el.type === 'checkbox') el.checked = !!val
      else el.value = Array.isArray(val) ? val.join(',') : val
    }
    settingsState.ready = true
    settingsState.dirty = false
    $('#settingsMsg').textContent = ''
  } finally {
    if (request === settingsState.request) {
      f.inert = !settingsState.ready
      f.setAttribute('aria-busy', 'false')
    }
  }
}
async function saveSettings(e) {
  e.preventDefault()
  if (!settingsState.ready || settingsState.saving) return false
  const f = e.target
  const body = {}
  for (const el of f.elements) {
    if (!el.name) continue
    if (el.type === 'checkbox') body[el.name] = el.checked
    else if (el.type === 'number' || el.name === 'urlCacheTTL') body[el.name] = Number(el.value)
    else if (el.name.endsWith('Sources')) body[el.name] = el.value.split(/[,，\s]+/).filter(Boolean)
    else body[el.name] = el.value
  }
  const request = ++settingsState.request
  settingsState.saving = true
  f.inert = true
  f.setAttribute('aria-busy', 'true')
  try {
    await adminAPI('/settings', { method: 'PUT', body })
    if (request !== settingsState.request) return false
    settingsState.dirty = false
    $('#settingsMsg').textContent = '已保存'
    toast('设置已保存')
  } catch (err) {
    if (request === settingsState.request && !isAbort(err)) toast(err.message, true)
  } finally {
    if (request === settingsState.request) {
      settingsState.saving = false
      f.inert = false
      f.setAttribute('aria-busy', 'false')
    }
  }
  return false
}


$('#retryBoardSettings').addEventListener('click', loadBoardSettings)
$('#boardSettingsForm').addEventListener('submit', saveBoardSettings)
$('#settingsForm').addEventListener('submit', saveSettings)

return { reset: resetSettingsSession, load: loadSettings, loadBoards: loadBoardSettings, system: settingsState, boards: boardSettingsState, selection: () => boardChoices.snapshot() }
})()
