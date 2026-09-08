// 实听计时独立于播放器界面，可注入时钟、音频和网络进行行为验证。
;(function (root) {
  const dayOf = ms => new Date(ms + 8 * 3600000).toISOString().slice(0, 10)
  // 局域网 HTTP 页面可能没有 randomUUID，getRandomValues 仍可生成会话标识。
  const sessionID = () => crypto.randomUUID ? crypto.randomUUID() : Array.from(crypto.getRandomValues(new Uint8Array(16)), n => n.toString(16).padStart(2, '0')).join('')
  class ListeningTracker {
    constructor({ audio, user, send, now = () => Date.now(), monotonic = () => performance.now(), uuid = sessionID }) {
      Object.assign(this, { audio, user, send, now, monotonic, uuid })
      this.pending = new Map()
      this.requests = new Set()
      this.epoch = 0
      this.active = null
      this.running = false
      this.acknowledged = ''
      this.anchor()
      for (const name of ['playing', 'seeked']) audio.addEventListener(name, () => {
        this.anchor(); this.running = !audio.paused && !audio.seeking && audio.readyState >= 3
      })
      for (const name of ['pause', 'waiting', 'ended', 'error', 'seeking', 'ratechange']) audio.addEventListener(name, () => {
        // 快速切歌后，旧音源排队中的媒体事件不能停止新会话的计时。
        if ((name === 'pause' && !audio.paused) || (name === 'ended' && !audio.ended) || (name === 'error' && !audio.error) || (name === 'seeking' && !audio.seeking) || (name === 'waiting' && audio.readyState >= 3)) return
        this.sample(name !== 'seeking')
        this.running = name === 'ratechange' && !audio.paused && !audio.seeking && audio.readyState >= 3
        this.anchor()
        this.flush()
      })
      audio.addEventListener('timeupdate', () => this.sample())
    }
    anchor() {
      this.wall = this.now(); this.mono = this.monotonic(); this.media = this.audio.currentTime || 0
      this.rate = this.audio.playbackRate || 1
    }
    sample(allow = true) {
      const wall = this.now(), mono = this.monotonic(), media = this.audio.currentTime || 0
      const elapsed = mono - this.mono, advanced = (media - this.media) * 1000 / this.rate
      // 大幅跳转或系统时钟跳变不当作播放；实际累计不超过墙钟和音频推进量。
      if (allow && this.active && this.running && elapsed > 0 && advanced > 0 && advanced <= elapsed + 1000 && Math.abs(wall - this.wall - elapsed) < 2000) {
        let start = Math.max(this.active.startedAt, wall - Math.min(elapsed, advanced))
        while (start < wall) {
          const day = dayOf(start), end = Math.min(wall, Date.parse(day + 'T00:00:00+08:00') + 86400000)
          this.active.days[day] = (this.active.days[day] || 0) + end - start
          start = end
        }
      }
      this.anchor()
    }
    start(track) {
      this.sample(); this.snapshot(); this.flush()
      this.active = this.user() && track ? { userId: this.user().id, sessionId: this.uuid(), trackId: track.id, startedAt: this.now(), days: {} } : null
      this.acknowledged = ''
      this.running = false
      this.anchor()
    }
    snapshot() {
      if (!this.active) return
      const days = Object.fromEntries(Object.entries(this.active.days).map(([day, ms]) => [day, Math.floor(ms)]).filter(([, ms]) => ms > 0))
      const body = { ...this.active, days }
      if (Object.keys(days).length && JSON.stringify(body) !== this.acknowledged) this.pending.set(this.active.sessionId, body)
    }
    async flush(keepalive = false) {
      this.sample(); this.snapshot()
      const epoch = this.epoch
      await Promise.all([...this.pending.keys()].map(async key => {
        const existing = [...this.requests].find(r => r.key === key)
        if (existing && !keepalive) await existing.promise
        if (epoch !== this.epoch) return
        const body = this.pending.get(key)
        if (!body) return
        if (body.userId !== this.user()?.id) { this.pending.delete(key); return }
        const signature = JSON.stringify(body)
        const controller = new AbortController()
        const entry = { key, controller }
        const timeout = setTimeout(() => controller.abort(), 8000)
        this.requests.add(entry)
        entry.promise = (async () => {
          try {
            await this.send(body, { signal: controller.signal, keepalive })
            if (epoch === this.epoch && this.active?.sessionId === key) this.acknowledged = signature
            if (epoch === this.epoch && JSON.stringify(this.pending.get(key)) === signature) this.pending.delete(key)
          } catch (error) {
            // 网络失败保留累计快照重试；已失效或无效的请求不跨账号重放。
            if (epoch === this.epoch && [400, 401, 403].includes(error.status) && JSON.stringify(this.pending.get(key)) === signature) this.pending.delete(key)
          } finally { clearTimeout(timeout); this.requests.delete(entry) }
        })()
        await entry.promise
      }))
    }
    reset() {
      this.epoch++
      for (const request of this.requests) request.controller.abort()
      this.requests.clear(); this.pending.clear(); this.active = null; this.running = false; this.acknowledged = ''; this.anchor()
    }
  }
  if (typeof module === 'object' && module.exports) module.exports = { ListeningTracker, dayOf }
  else root.LXSCListening = { ListeningTracker, dayOf }
})(globalThis)
