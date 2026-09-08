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
        canPrevious: this.index > 0, canNext: this.index >= 0 && this.index < this.queue.length - 1,
      }
    }
    notify() { this.onChange(this.state) }

    playList(tracks, index) {
      if (!tracks[index] || tracks[index].unavailable) return
      this.index = tracks.slice(0, index).filter(track => !track.unavailable).length
      this.queue = tracks.filter(track => !track.unavailable).map(track => ({ ...track, qualities: [...(track.qualities || [])] }))
      this.load()
    }

    unbind() {
      for (const [event, handler] of this.listeners) this.audio.removeEventListener(event, handler)
      this.listeners = []
    }

    load() {
      if (!this.track) return
      this.onLoad(this.track)
      const version = ++this.version
      this.unbind()
      this.audio.pause()
      this.status = 'loading'
      this.message = ''
      this.wantsPlay = true
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
      bind('loadedmetadata', () => this.notify())
      bind('durationchange', () => this.notify())
      bind('ended', () => {
        if (!this.audio.ended || !this.wantsPlay || this.status === 'error') return
        if (this.index < this.queue.length - 1) this.next()
        else { this.wantsPlay = false; this.status = 'ended'; this.notify() }
      })
      bind('error', () => {
        if (this.audio.error) this.failed(version, '播放失败，请重试、更换平台或降低音质；直连受限时请检查服务器播放模式。')
      })
      this.audio.load()
      this.notify()
      // 在点击处理链中立即调用 play，避免等待异步接口丢失浏览器用户手势。
      this.resume(version)
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
    previous() { if (this.index > 0) { this.index--; this.load() } }
    next() { if (this.index >= 0 && this.index < this.queue.length - 1) { this.index++; this.load() } }
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
