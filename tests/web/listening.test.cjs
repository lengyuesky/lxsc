const test = require('node:test')
const assert = require('node:assert/strict')
const { ListeningTracker, dayOf } = require('../../internal/assets/web/listening-tracker.js')

class Audio extends EventTarget {
  constructor() { super(); this.currentTime = 0; this.paused = true; this.seeking = false; this.readyState = 4; this.playbackRate = 1 }
  emit(name) { this.dispatchEvent(new Event(name)) }
}
function fixture(send = async () => {}) {
  let now = Date.parse('2026-09-07T23:59:58+08:00'), mono = 0, id = 0, user = { id: 1 }
  const audio = new Audio(), sent = []
  const tracker = new ListeningTracker({ audio, user: () => user, now: () => now, monotonic: () => mono, uuid: () => 'session-' + ++id, send: async (body, opts) => { sent.push(structuredClone(body)); return send(body, opts) } })
  function advance(ms, playback = true) { now += ms; mono += ms; if (playback) audio.currentTime += ms / 1000 * audio.playbackRate; tracker.sample() }
  function play() { audio.paused = false; audio.emit('playing') }
  tracker.start({ id: 'tr-wy-1' })
  return { audio, tracker, sent, advance, play, setUser: value => { user = value } }
}
const total = tracker => Object.values(tracker.active.days).reduce((a, b) => a + b, 0)

test('网页实际经过时间按北京时间跨日拆分，后台长间隔继续累计', async () => {
  const f = fixture(); f.play(); f.advance(5000); f.advance(60000)
  await f.tracker.flush()
  assert.deepEqual(f.sent.at(-1).days, { '2026-09-07': 2000, '2026-09-08': 63000 })
  assert.equal(dayOf(Date.parse('2026-09-07T16:00:00Z')), '2026-09-08')
})
test('暂停、缓冲、拖动和失败不增加时长，恢复后正常计时', async () => {
  const f = fixture(); f.play(); f.advance(1000)
  f.audio.paused = true; f.audio.emit('pause'); f.advance(10000, false)
  assert.equal(total(f.tracker), 1000)
  f.play(); f.advance(1000); f.audio.readyState = 2; f.audio.emit('waiting'); f.advance(10000, false)
  assert.equal(total(f.tracker), 2000)
  f.audio.readyState = 4; f.play(); f.audio.seeking = true; f.audio.currentTime = 100; f.audio.emit('seeking'); f.advance(1000, false)
  f.audio.seeking = false; f.audio.emit('seeked'); f.advance(1000)
  assert.equal(total(f.tracker), 3000)
  f.audio.error = { code: 3 }; f.audio.emit('error'); f.advance(10000, false)
  assert.equal(total(f.tracker), 3000)
  await f.tracker.flush()
})
test('倍速按实际经过时间计时，进度跳转不能伪造时长', () => {
  const f = fixture(); f.audio.playbackRate = 2; f.play(); f.advance(5000)
  assert.equal(total(f.tracker), 5000)
  f.audio.currentTime += 200; f.tracker.sample(); assert.equal(total(f.tracker), 5000)
  f.audio.playbackRate = 0.5; f.audio.emit('ratechange'); f.advance(3000)
  assert.equal(total(f.tracker), 8000)
})
test('断网保留累计快照重试，暂停后已确认的进度不重复发送', async () => {
  let offline = true
  const f = fixture(async () => { if (offline) throw new Error('网络中断') })
  f.play(); f.advance(15000); await f.tracker.flush()
  assert.equal(f.tracker.pending.size, 1)
  f.advance(10000); offline = false; await f.tracker.flush()
  assert.equal(Object.values(f.sent.at(-1).days).reduce((a, b) => a + b, 0), 25000)
  assert.equal(f.tracker.pending.size, 0)
  const count = f.sent.length; await f.tracker.flush(); assert.equal(f.sent.length, count)
})
test('切歌保留上一首快照，重新播放同一首使用新的会话', async () => {
  const f = fixture(); f.play(); f.advance(1000); f.tracker.start({ id: 'tr-wy-1' }); f.play(); f.advance(2000)
  await f.tracker.flush()
  assert.ok(f.sent.some(body => body.sessionId === 'session-1'))
  assert.equal(f.tracker.active.sessionId, 'session-2')
  assert.equal(total(f.tracker), 2000)
})
test('最终补报等待旧请求后发送最新累计量，迟到响应不能清除新数据', async () => {
  let release
  const held = new Promise(resolve => { release = resolve })
  let calls = 0
  const f = fixture(async () => { if (++calls === 1) await held })
  f.play(); f.advance(1000); const first = f.tracker.flush()
  f.advance(2000); const final = f.tracker.flush()
  release(); await Promise.all([first, final])
  assert.equal(f.tracker.pending.size, 0)
  assert.equal(Object.values(f.sent.at(-1).days).reduce((a, b) => a + b, 0), 3000)
})
test('换号清理请求和数据，旧上报不会使用新账号重放', async () => {
  let release
  const f = fixture(() => new Promise(resolve => { release = resolve }))
  f.play(); f.advance(1000); const old = f.tracker.flush()
  f.tracker.reset(); f.setUser({ id: 2 }); release(); await old
  assert.equal(f.tracker.pending.size, 0); assert.equal(f.tracker.active, null)
  const before = f.sent.length; await f.tracker.flush(); assert.equal(f.sent.length, before)
})

test('快速切歌后的旧媒体事件不会中断新会话计时', () => {
  const f = fixture(); f.play(); f.advance(1000)
  f.tracker.start({ id: 'tr-wy-2' }); f.play()
  for (const event of ['pause', 'waiting', 'ended', 'error', 'seeking']) f.audio.emit(event)
  f.advance(1000)
  assert.equal(total(f.tracker), 1000)
  assert.equal(f.tracker.active.trackId, 'tr-wy-2')
})

test('局域网 HTTP 缺少 randomUUID 时仍能创建播放会话', () => {
  const vm = require('node:vm'), fs = require('node:fs')
  const scope = { module: { exports: {} }, crypto: { getRandomValues: data => data.fill(42) }, setTimeout, clearTimeout, AbortController }
  vm.runInNewContext(fs.readFileSync(require.resolve('../../internal/assets/web/listening-tracker.js'), 'utf8'), scope)
  const tracker = new scope.module.exports.ListeningTracker({ audio: new Audio(), user: () => ({ id: 1 }), send: async () => {}, now: () => 1000, monotonic: () => 0 })
  tracker.start({ id: 'tr-wy-1' })
  assert.match(tracker.active.sessionId, /^[a-f0-9]{32}$/)
})
