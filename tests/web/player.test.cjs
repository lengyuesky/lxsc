const { test } = require('node:test')
const assert = require('node:assert/strict')
const { PlayerController, RequestGate, collectionBlocked } = require('../../internal/assets/web/player.js')

class FakeAudio extends EventTarget {
  constructor() {
    super()
    Object.assign(this, { src: '', currentSrc: '', paused: true, ended: false, duration: NaN, currentTime: 0, volume: 1, muted: false, error: null, loads: 0, plays: 0 })
  }
  load() { this.loads++; this.currentSrc = this.src; this.currentTime = 0; this.duration = NaN; this.error = null; this.ended = false; this.paused = true }
  play() {
    this.plays++
    if (this.playResult) return this.playResult()
    this.paused = false
    this.dispatchEvent(new Event('playing'))
    return Promise.resolve()
  }
  pause() { const changed = !this.paused; this.paused = true; if (changed) this.dispatchEvent(new Event('pause')) }
  removeAttribute(name) { if (name === 'src') this.src = '' }
  ready(duration) { this.duration = duration; this.dispatchEvent(new Event('loadedmetadata')) }
  end() { this.ended = true; this.paused = true; this.dispatchEvent(new Event('ended')) }
  fail() { this.error = { code: 4 }; this.dispatchEvent(new Event('error')) }
}
const songs = () => [{ id: 'tr-wy-1', name: '一' }, { id: 'tr-tx-2', name: '二' }, { id: 'tr-kg-3', name: '三' }]
function fixture(options = {}) {
  const audio = new FakeAudio(), changes = []
  const player = new PlayerController({ audio, streamURL: track => '/stream?id=' + track.id, onChange: state => changes.push(state), ...options })
  return { audio, player, changes }
}
function deferred() {
  let resolve, reject
  const promise = new Promise((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

test('列表快照保留重复项并跳过不可用条目，不受后续搜索或编辑影响', () => {
  const { player, audio } = fixture()
  const list = songs()
  list.splice(1, 0, { id: 'bad', unavailable: true })
  list.push({ ...list[0] })
  player.playList(list, 2)
  assert.equal(player.index, 1)
  assert.equal(player.track.id, 'tr-tx-2')
  assert.equal(player.queue.length, 4)
  list[2].name = '修改'
  list.reverse()
  assert.equal(player.track.name, '二')
  assert.equal(player.queue[0].id, player.queue[3].id)
  const loads = audio.loads
  player.playList([{ unavailable: true }], 0)
  assert.equal(audio.loads, loads)
})

test('正常结束顺序播放，队尾停止，前后边界不循环', async () => {
  const { player, audio } = fixture()
  player.playList(songs(), 0)
  player.previous()
  assert.equal(player.index, 0)
  assert.equal(audio.plays, 1)
  audio.end()
  assert.equal(player.index, 1)
  audio.end()
  assert.equal(player.index, 2)
  await Promise.resolve()
  audio.end()
  assert.equal(player.status, 'ended')
  assert.equal(player.wantsPlay, false)
  player.next()
  assert.equal(audio.plays, 3)
  player.toggle()
  assert.equal(player.index, 2)
  assert.equal(audio.plays, 4)
})

test('暂停恢复不重新取链，进度、音量和静音可控制', async () => {
  const { player, audio } = fixture()
  player.playList(songs(), 0)
  await Promise.resolve()
  const loads = audio.loads
  player.seek(20)
  assert.equal(audio.currentTime, 0)
  audio.ready(60)
  player.seek(20)
  assert.equal(audio.currentTime, 20)
  player.toggle()
  assert.equal(player.status, 'paused')
  player.toggle()
  await Promise.resolve()
  assert.equal(player.status, 'playing')
  assert.equal(audio.loads, loads)
  player.seek(100)
  assert.equal(audio.currentTime, 60)
  player.setVolume(0.25)
  assert.equal(player.state.volume, 0.25)
  player.setVolume(2)
  assert.equal(player.state.volume, 1)
  player.toggleMuted()
  assert.equal(player.state.muted, true)
})

test('加载期间暂停后，迟到的 play 结果不能恢复播放', async () => {
  const { player, audio } = fixture(), pending = deferred()
  audio.playResult = () => pending.promise
  player.playList(songs(), 0)
  player.toggle()
  pending.resolve()
  await Promise.resolve()
  assert.equal(player.status, 'paused')
  assert.equal(player.wantsPlay, false)
})

test('同一首快速暂停再恢复时，旧 play 拒绝不能覆盖新的播放', async () => {
  const { player, audio } = fixture(), first = deferred(), second = deferred()
  audio.playResult = () => audio.plays === 1 ? first.promise : second.promise
  player.playList(songs(), 0)
  player.toggle()
  player.toggle()
  first.reject(new DOMException('旧播放已被暂停', 'AbortError'))
  await Promise.resolve()
  assert.notEqual(player.status, 'error')
  audio.paused = false
  second.resolve()
  await Promise.resolve()
  assert.equal(player.status, 'playing')
})

test('快速切歌忽略旧 promise 和旧事件，不覆盖新歌曲', async () => {
  const { player, audio } = fixture(), pending = deferred()
  audio.playResult = () => pending.promise
  player.playList(songs(), 0)
  const oldError = player.listeners.find(([name]) => name === 'error')[1]
  audio.playResult = null
  player.next()
  pending.reject(new Error('旧播放失败'))
  audio.error = { code: 4 }
  oldError()
  audio.error = null
  await Promise.resolve()
  assert.equal(player.track.id, 'tr-tx-2')
  assert.equal(player.status, 'playing')
})

test('媒体错误停止，不自动跳过列表；手动重试和下一首可用', () => {
  let failures = 0
  const { player, audio } = fixture({ onFailure: () => failures++ })
  player.playList(songs(), 0)
  audio.fail()
  audio.end()
  assert.equal(player.index, 0)
  assert.equal(player.status, 'error')
  assert.equal(failures, 1)
  player.toggle()
  assert.equal(audio.plays, 2)
  audio.fail()
  player.next()
  assert.equal(player.index, 1)
  assert.equal(player.status, 'playing')
})

test('浏览器拒绝自动播放时等待手动点击，不跳歌或重试', async () => {
  const { player, audio } = fixture()
  audio.playResult = () => Promise.reject(new DOMException('需要用户手势', 'NotAllowedError'))
  player.playList(songs(), 0)
  await Promise.resolve()
  assert.equal(player.status, 'paused')
  assert.match(player.message, /点击/)
  assert.equal(audio.plays, 1)
  audio.playResult = null
  player.toggle()
  await Promise.resolve()
  assert.equal(player.status, 'playing')
})

test('音质变化只在新播放或重试时使用，不中断当前歌曲', () => {
  let quality = '320k'
  const { player, audio } = fixture({ streamURL: track => '/stream?id=' + track.id + '&quality=' + quality })
  player.playList(songs(), 0)
  quality = '128k'
  player.toggle()
  player.toggle()
  assert.match(audio.src, /quality=320k/)
  player.next()
  assert.match(audio.src, /quality=128k/)
})

test('退出清理释放音源和队列，忽略所有迟到回调', async () => {
  const { player, audio } = fixture(), pending = deferred()
  audio.playResult = () => pending.promise
  player.playList(songs(), 0)
  player.clear()
  pending.reject(new Error('迟到错误'))
  await Promise.resolve()
  audio.end()
  assert.equal(player.status, 'idle')
  assert.equal(player.track, null)
  assert.equal(audio.src, '')
  assert.equal(audio.currentSrc, '')
  assert.equal(player.queue.length, 0)
})

test('请求代际取消旧请求，只有最新搜索可以提交结果', async () => {
  const gate = new RequestGate(), first = gate.begin(), second = gate.begin()
  assert.equal(first.signal.aborted, true)
  const results = []
  if (second.current()) results.push('新搜索')
  await Promise.resolve()
  if (first.current()) results.push('旧搜索')
  assert.deepEqual(results, ['新搜索'])
  gate.cancel()
  assert.equal(second.current(), false)
  assert.equal(second.signal.aborted, true)
})

test('即时收藏保护当前歌曲和信息草稿，不阻止收藏到其他歌单', () => {
  assert.equal(collectionBlocked('a', 'a', true, false), true)
  assert.equal(collectionBlocked('a', 'a', false, true), true)
  assert.equal(collectionBlocked('b', 'a', true, true), false)
  assert.equal(collectionBlocked('a', 'a', false, false), false)
  assert.equal(collectionBlocked('a', undefined, false, false), false)
})
