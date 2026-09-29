// 流式搜索的解析和结果状态独立于页面，方便复用与验证。
;(function (root) {
  async function readEvents(stream, onEvent) {
    if (!stream) throw new Error('浏览器无法读取搜索结果')
    const reader = stream.getReader(), decoder = new TextDecoder()
    let buffer = '', complete = false
    const emit = line => {
      if (!line.trim()) return
      const event = JSON.parse(line)
      if (event.type === 'done') complete = true
      onEvent(event)
    }
    try {
      while (true) {
        const { done, value } = await reader.read()
        buffer += decoder.decode(value, { stream: !done })
        let index
        while ((index = buffer.indexOf('\n')) >= 0) { emit(buffer.slice(0, index)); buffer = buffer.slice(index + 1) }
        if (buffer.length > 1024 * 1024) throw new Error('搜索响应过大')
        if (done) { emit(buffer); break }
      }
      if (!complete) throw new Error('搜索连接提前结束，已返回的结果仍可使用')
    } finally {
      if (!complete) await reader.cancel().catch(() => {})
      reader.releaseLock()
    }
  }

  class Results {
    constructor() { this.sources = []; this.platforms = new Map(); this.tracks = []; this.complete = false }
    accept(event) {
      if (event.type === 'start') {
        this.sources = event.sources
        this.platforms = new Map(event.sources.map(source => [source, { status: 'pending', tracks: [] }]))
        this.tracks = []
        this.complete = false
      } else if (event.type === 'platform' && this.platforms.get(event.source)?.status === 'pending') {
        this.platforms.set(event.source, event)
        // 后到的平台追加在末尾，避免用户点击时已有歌曲改变位置。
        if (event.status === 'ok') this.tracks.push(...event.tracks)
      } else if (event.type === 'done') this.complete = true
    }
    get pending() { return [...this.platforms.values()].some(item => item.status === 'pending') }
    get failed() { return [...this.platforms.values()].some(item => ['error', 'timeout', 'busy'].includes(item.status)) }
  }
  const rendered = new WeakMap()
  // 每次搜索的数组保持同一引用，仅追加新结果；旧按钮与焦点不被重建。
  function appendRows(container, tracks, render) {
    let state = rendered.get(container)
    if (!state || state.tracks !== tracks) {
      container.replaceChildren()
      state = { tracks, count: 0 }
      rendered.set(container, state)
    }
    if (state.count < tracks.length) {
      container.insertAdjacentHTML('beforeend', tracks.slice(state.count).map((track, offset) => render(track, state.count + offset)).join(''))
      state.count = tracks.length
    }
  }
  const api = { readEvents, Results, appendRows }
  if (typeof module === 'object' && module.exports) module.exports = api
  else root.LXSCSearch = api
})(globalThis)
