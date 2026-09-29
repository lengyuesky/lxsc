const test = require('node:test')
const assert = require('node:assert/strict')
const { readEvents, Results } = require('../../internal/assets/web/search.js')

test('繁忙平台作为明确失败显示，不把它当成成功空结果', () => {
  const result = new Results()
  result.accept({ type: 'start', sources: ['wy'] })
  result.accept({ type: 'platform', source: 'wy', status: 'busy', tracks: [] })
  assert.equal(result.failed, true)
  assert.equal(result.pending, false)
  assert.equal(result.tracks.length, 0)
})

test('流式解析支持跨字节中文，并在流结束前交付各平台结果', async () => {
  let controller
  const stream = new ReadableStream({ start(value) { controller = value } })
  const result = new Results(), events = []
  const reading = readEvents(stream, event => { events.push(event); result.accept(event) })
  const first = new TextEncoder().encode(JSON.stringify({ type: 'start', sources: ['wy', 'tx'] }) + '\n' + JSON.stringify({ type: 'platform', source: 'wy', status: 'ok', tracks: [{ id: '1', name: '中文歌曲' }] }) + '\n')
  for (const value of first) controller.enqueue(Uint8Array.of(value))
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(result.tracks[0].name, '中文歌曲')
  assert.equal(result.pending, true)
  controller.enqueue(new TextEncoder().encode(JSON.stringify({ type: 'platform', source: 'tx', status: 'timeout', tracks: [] }) + '\n{"type":"done"}\n'))
  controller.close()
  await reading
  assert.equal(events.length, 4)
  assert.equal(result.failed, true)
  assert.equal(result.pending, false)
})

test('慢平台追加结果不改变已有歌曲位置，重复平台事件不会重复添加', () => {
  const result = new Results()
  result.accept({ type: 'start', sources: ['wy', 'tx'] })
  result.accept({ type: 'platform', source: 'tx', status: 'ok', tracks: [{ id: 'tx-1' }] })
  const event = { type: 'platform', source: 'wy', status: 'ok', tracks: [{ id: 'wy-1' }] }
  result.accept(event); result.accept(event)
  assert.deepEqual(result.tracks.map(track => track.id), ['tx-1', 'wy-1'])
})

test('流中断保留已收到结果并明确报错', async () => {
  const result = new Results()
  const stream = new ReadableStream({ start(controller) {
    controller.enqueue(new TextEncoder().encode('{"type":"start","sources":["wy"]}\n{"type":"platform","source":"wy","status":"ok","tracks":[{"id":"1"}]}\n'))
    controller.close()
  } })
  await assert.rejects(readEvents(stream, event => result.accept(event)), /提前结束/)
  assert.equal(result.tracks.length, 1)
})
