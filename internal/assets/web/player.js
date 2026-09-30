// 独立的网页音乐状态模块，无框架、无构建依赖；音频对象可注入测试。
;(function (root) {
  class RequestGate {
    constructor() { this.version = 0; this.controller = null }
    cancel() { this.version++; this.controller?.abort(); this.controller = null }
    begin() {
      this.cancel()
      const version = this.version
      const controller = this.controller = new AbortController()
      return { signal: controller.signal, current: () => version === this.version && !controller.signal.aborted }
    }
  }

  function collectionBlocked(targetID, currentID, tracksDirty, metaDirty) {
    return targetID === currentID && (tracksDirty || metaDirty)
  }

  class PlayerController {
    constructor({ audio, streamURL, onChange = () => {}, onFailure = () => {}, onLoad = () => {} }) {
      this.audio = audio
      this.streamURL = streamURL
      this.onChange = onChange
      this.onFailure = onFailure
      this.onLoad = onLoad
      this.queue = []
      this.repeat = 'off'
      this.shuffle = false
      this.queueVersion = 0
      this.index = -1
      this.version = 0
      this.playAttempt = 0
      this.listeners = []
      this.status = 'idle'
      this.message = ''
      this.wantsPlay = false
      audio.preload = 'none'
      audio.addEventListener('volumechange', () => this.notify())
    }

    get track() { return this.queue[this.index] || null }
    get duration() {
      return Number.isFinite(this.audio.duration) && this.audio.duration > 0 ? this.audio.duration : 0
    }
    get state() {
      return {
        track: this.track, index: this.index, count: this.queue.length,
        status: this.status, message: this.message,
        time: Number.isFinite(this.audio.currentTime) ? this.audio.currentTime : 0,
        duration: this.duration, volume: this.audio.volume, muted: this.audio.muted,
        repeat: this.repeat, shuffle: this.shuffle, queueVersion: this.queueVersion,
        canPrevious: this.index > 0 || (this.repeat === 'all' && this.queue.length > 1), canNext: this.index >= 0 && (this.index < this.queue.length - 1 || this.repeat === 'all' || (this.shuffle && this.queue.length > 1) || this.queue.some(track => track.nextRequested)),
      }
    }
    notify() { this.onChange(this.state) }

    playList(tracks, index) {
      if (!tracks[index] || tracks[index].unavailable) return
      this.index = tracks.slice(0, index).filter(track => !track.unavailable).length
      this.queue = tracks.filter(track => !track.unavailable).map(track => ({ ...track, qualities: [...(track.qualities || [])] }))
      this.queueVersion++
      this.load()
    }

    unbind() {
      for (const [event, handler] of this.listeners) this.audio.removeEventListener(event, handler)
      this.listeners = []
    }

    load({ autoplay = true, position = 0 } = {}) {
      if (!this.track) return
      delete this.track.nextRequested
      this.onLoad(this.track)
      const version = ++this.version
      this.unbind()
      this.audio.pause()
      this.status = autoplay ? 'loading' : 'paused'
      this.message = ''
      this.wantsPlay = autoplay
      this.audio.src = this.streamURL(this.track)
      const source = this.audio.src
      const current = () => version === this.version && this.audio.src === source && (!this.audio.currentSrc || this.audio.currentSrc === source)
      const bind = (event, action) => {
        const handler = () => { if (current()) action() }
        this.listeners.push([event, handler])
        this.audio.addEventListener(event, handler)
      }
      bind('playing', () => {
        if (!this.wantsPlay || this.audio.paused) return
        this.status = 'playing'; this.message = ''; this.notify()
      })
      bind('pause', () => {
        if (!this.audio.paused || this.audio.ended || this.status === 'error') return
        this.wantsPlay = false; this.status = 'paused'; this.notify()
      })
      bind('waiting', () => { if (this.wantsPlay) { this.status = 'loading'; this.notify() } })
      bind('timeupdate', () => this.notify())
      bind('loadedmetadata', () => { if (position > 0) { this.seek(position); position = 0 }; this.notify() })
      bind('durationchange', () => this.notify())
      bind('ended', () => {
        if (!this.audio.ended || !this.wantsPlay || this.status === 'error') return
        if (this.repeat === 'one') this.load()
        else if (this.state.canNext) this.next()
        else { this.wantsPlay = false; this.status = 'ended'; this.notify() }
      })
      bind('error', () => {
        if (this.audio.error) this.failed(version, '播放失败，请重试、更换平台或降低音质；直连受限时请检查服务器播放模式。')
      })
      this.audio.load()
      this.notify()
      // 在点击处理链中立即调用 play，避免等待异步接口丢失浏览器用户手势。
      if (autoplay) this.resume(version)
    }

    async resume(version = this.version) {
      const attempt = ++this.playAttempt
      this.wantsPlay = true
      this.status = 'loading'
      this.message = ''
      this.notify()
      try {
        await this.audio.play()
        if (version !== this.version || attempt !== this.playAttempt || !this.wantsPlay || this.status === 'error') return
        this.status = this.audio.paused ? 'paused' : 'playing'
        this.notify()
      } catch (error) {
        if (version !== this.version || attempt !== this.playAttempt || !this.wantsPlay || this.status === 'error') return
        if (error.name === 'NotAllowedError') {
          this.wantsPlay = false
          this.status = 'paused'
          this.message = '浏览器暂未允许播放，请点击播放按钮。'
          this.notify()
        } else {
          this.failed(version, '无法播放，请重试；如果格式不受支持，可将默认音质改为 128k 或 320k。')
        }
      }
    }

    failed(version, message) {
      if (version !== this.version || this.status === 'error') return
      this.wantsPlay = false
      this.status = 'error'
      this.message = message
      this.audio.pause()
      this.notify()
      this.onFailure()
    }

    toggle() {
      if (!this.track) return
      if (this.status === 'error' || this.status === 'ended') return this.load()
      if (this.wantsPlay) {
        this.wantsPlay = false
        this.audio.pause()
        this.status = 'paused'
        this.notify()
      } else this.resume()
    }
    previous() {
      if (!this.state.canPrevious) return
      this.index = this.index > 0 ? this.index - 1 : this.queue.length - 1
      this.load()
    }
    next() {
      if (!this.state.canNext) return
      const requested = this.queue.findIndex(track => track.nextRequested)
      if (requested >= 0) this.index = requested
      else if (this.shuffle && this.queue.length > 1) this.index = (this.index + 1 + Math.floor(Math.random() * (this.queue.length - 1))) % this.queue.length
      else this.index = (this.index + 1) % this.queue.length
      this.load()
    }
    setRepeat(value) { if (['off', 'one', 'all'].includes(value)) { this.repeat = value; this.notify() } }
    setShuffle(value) { this.shuffle = !!value; this.notify() }
    playAt(index) { if (Number.isInteger(index) && this.queue[index]) { this.index = index; this.load() } }
    enqueue(track, next = false) {
      if (!track || track.unavailable || this.queue.length >= 2000) return
      if (!this.track) return this.playList([track], 0)
      this.queue.splice(next ? this.index + 1 : this.queue.length, 0, { ...track, nextRequested: next, qualities: [...(track.qualities || [])] })
      this.queueVersion++; this.notify()
    }
    remove(index) {
      if (!Number.isInteger(index) || !this.queue[index]) return
      const current = index === this.index, autoplay = this.wantsPlay
      this.queue.splice(index, 1); this.queueVersion++
      if (!this.queue.length) return this.clear()
      if (index < this.index || this.index >= this.queue.length) this.index--
      if (current) this.load({ autoplay }); else this.notify()
    }
    move(from, to) {
      if (!Number.isInteger(from) || !Number.isInteger(to) || !this.queue[from] || !this.queue[to] || from === to) return
      const current = this.track
      this.queue.splice(to, 0, this.queue.splice(from, 1)[0])
      this.index = this.queue.indexOf(current); this.queueVersion++; this.notify()
    }
    snapshot() { return { queue: this.queue, index: this.index, position: this.state.time, repeat: this.repeat, shuffle: this.shuffle, volume: this.audio.volume, muted: this.audio.muted } }
    restore(saved) {
      if (!saved || !Array.isArray(saved.queue) || saved.queue.length > 2000 || !Number.isInteger(saved.index) || saved.index < 0 || saved.index >= saved.queue.length) return false
      if (saved.queue.some(track => !track || typeof track.id !== 'string' || !/^tr-(wy|tx|kw|kg|mg)-.{1,1000}$/.test(track.id))) return false
      this.queue = saved.queue.map(track => ({ id: track.id, name: String(track.name || ''), singer: String(track.singer || ''), album: String(track.album || ''), source: String(track.source || ''), nextRequested: track.nextRequested === true, duration: Number(track.duration) || 0, qualities: Array.isArray(track.qualities) ? track.qualities : [] }))
      this.index = saved.index; this.queueVersion++
      this.repeat = ['off', 'one', 'all'].includes(saved.repeat) ? saved.repeat : 'off'; this.shuffle = !!saved.shuffle
      if (Number.isFinite(saved.volume)) this.audio.volume = Math.max(0, Math.min(1, saved.volume))
      this.audio.muted = !!saved.muted
      this.load({ autoplay: false, position: Number.isFinite(saved.position) ? Math.max(0, saved.position) : 0 })
      return true
    }
    seek(time) {
      if (!this.duration || !Number.isFinite(time)) return
      try { this.audio.currentTime = Math.max(0, Math.min(time, this.duration)); this.notify() } catch { /* 媒体尚不可定位时保持现有进度。 */ }
    }
    setVolume(value) {
      if (!Number.isFinite(value)) return
      try { this.audio.volume = Math.max(0, Math.min(value, 1)) } catch { /* 部分移动浏览器只允许系统调节音量。 */ }
      this.notify()
    }
    toggleMuted() { this.audio.muted = !this.audio.muted; this.notify() }
    clear() {
      this.version++
      this.unbind()
      this.wantsPlay = false
      this.audio.pause()
      this.audio.removeAttribute('src')
      this.audio.load()
      this.queue = []
      this.queueVersion++
      this.repeat = 'off'
      this.shuffle = false
      this.index = -1
      this.status = 'idle'
      this.message = ''
      this.notify()
    }
  }

  const api = { PlayerController, RequestGate, collectionBlocked }
  if (typeof module === 'object' && module.exports) module.exports = api
  else root.LXSCMusic = api
})(globalThis)
