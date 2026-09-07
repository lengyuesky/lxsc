// 榜单选择与候选目录分开保存，异步加载只能更新目录，不能改写管理员的选择。
;(function (root) {
  'use strict'

  class BoardSelectionState {
    constructor(fetchBoards, onChange = () => {}) {
      this.fetchBoards = fetchBoards
      this.onChange = onChange
      this.requests = new Map()
      this.reset()
    }

    reset(selections = {}) {
      for (const request of this.requests.values()) request.abort()
      this.requests.clear()
      this.selections = Object.fromEntries(Object.entries(selections).map(([source, ids]) => [source, [...ids]]))
      this.catalogs = new Map()
    }

    snapshot() {
      return Object.fromEntries(Object.entries(this.selections).map(([source, ids]) => [source, [...ids]]))
    }

    isCustom(source) { return Object.hasOwn(this.selections, source) }

    catalog(source) {
      if (!this.catalogs.has(source)) this.catalogs.set(source, { boards: [], loaded: false, loading: false, error: '' })
      return this.catalogs.get(source)
    }

    async load(source, refresh = false) {
      const catalog = this.catalog(source)
      if (!refresh && (catalog.loaded || catalog.loading || catalog.error)) return
      this.requests.get(source)?.abort()
      const request = new AbortController()
      this.requests.set(source, request)
      catalog.loading = true
      catalog.error = ''
      this.onChange(source)
      try {
        const boards = await this.fetchBoards(source, request.signal)
        if (this.requests.get(source) !== request) return
        if (!Array.isArray(boards)) throw new Error('榜单目录格式错误，请重试')
        catalog.boards = boards
        catalog.loaded = true
      } catch (error) {
        if (this.requests.get(source) !== request) return
        if (error.name !== 'AbortError') catalog.error = error.message || '榜单加载失败，请重试'
      } finally {
        if (this.requests.get(source) === request) {
          this.requests.delete(source)
          catalog.loading = false
          this.onChange(source)
        }
      }
    }

    setMode(source, mode) {
      if (mode === 'all') delete this.selections[source]
      else if (!this.isCustom(source)) {
        if (!this.catalog(source).loaded) return false
        this.selections[source] = this.catalog(source).boards.map(board => board.bangid)
      }
      this.onChange(source)
      return true
    }

    toggle(source, id, checked) {
      if (!this.isCustom(source)) return
      const selected = new Set(this.selections[source])
      if (checked) selected.add(id)
      else selected.delete(id)
      this.selections[source] = [...selected]
      this.onChange(source)
    }

    selectAll(source) {
      if (!this.catalog(source).loaded) return
      // 全选当前目录时仍保留暂时缺失的已选 ID，只有明确取消或清空才移除。
      this.selections[source] = [...new Set([...(this.selections[source] || []), ...this.catalog(source).boards.map(board => board.bangid)])]
      this.onChange(source)
    }

    clear(source) {
      this.selections[source] = []
      this.onChange(source)
    }

    options(source) {
      const catalog = this.catalog(source)
      const known = new Set(catalog.boards.map(board => board.bangid))
      const missing = (this.selections[source] || []).filter(id => !known.has(id)).map(id => ({ bangid: id, name: '榜单 ' + id, missing: true }))
      return [...catalog.boards, ...missing]
    }
  }

  const api = { BoardSelectionState }
  if (typeof module === 'object' && module.exports) module.exports = api
  else root.LXSCBoardSettings = api
})(globalThis)
