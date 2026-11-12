const test = require('node:test')
const assert = require('node:assert/strict')
const { RecentLogState, outcome } = require('../../internal/assets/web/recent-playback.js')
const { RequestGate } = require('../../internal/assets/web/player.js')

const at = Date.parse('2026-11-12T12:00:00+08:00')
const item = id => ({ id, startedAt: at - id * 1000, updatedAt: at, name: '歌曲 ' + id })
const page = (ids, nextCursor = '') => ({ items: ids.map(item), nextCursor, generatedAt: at, sources: [{ id: 1, name: '测试音源' }], clients: ['网页播放器'] })
function deferred() { let resolve; const promise = new Promise(yes => { resolve = yes }); return { promise, resolve } }

test('换用户后迟到的列表与明细不会显示原用户数据', async () => {
  const pending = []
  const state = new RecentLogState((url, options) => { const held = deferred(); pending.push({ url, options, ...held }); return held.promise }, RequestGate)
  state.select({ scope: 'user', userId: '1', days: '30' })
  const first = state.load()
  const detail = state.detail(12)
  state.select({ scope: 'user', userId: '2', days: '30' })
  assert.equal(pending[0].options.signal.aborted, true)
  assert.equal(pending[1].options.signal.aborted, true)
  const second = state.load()
  pending[2].resolve({ ...page([2]), clients: ['第二个用户客户端'] })
  await second
  pending[0].resolve({ ...page([1]), clients: ['第一个用户客户端'] })
  pending[1].resolve({ id: 12, name: '第一个用户的歌曲' })
  await first
  assert.equal(await detail, null)
  assert.deepEqual(state.items.map(item => item.id), [2])
  assert.deepEqual(state.clients, ['第二个用户客户端'])
  assert.equal(state.loading, false)
})

test('加载更多去重、失败保留游标与记录，手动刷新保留已加载旧页', async () => {
  const responses = [page([3, 4], 'older-1'), page([4, 5], 'older-2'), new Error('暂时不可用'), page([6], 'older-3'), page([1, 3], 'new-head')]
  const paths = []
  const state = new RecentLogState(async path => { paths.push(path); const result = responses.shift(); if (result instanceof Error) throw result; return result }, RequestGate)
  state.select({ scope: 'me', days: '30', delivery: 'proxy' })
  await state.load(); await state.load({ more: true })
  assert.deepEqual(state.items.map(item => item.id), [3, 4, 5])
  assert.match(paths[1], /cursor=older-1/)
  await state.load({ more: true })
  assert.equal(state.cursor, 'older-2')
  assert.deepEqual(state.items.map(item => item.id), [3, 4, 5])
  assert.equal(state.error, '暂时不可用')
  await state.load({ more: true })
  await state.load()
  assert.deepEqual(state.items.map(item => item.id), [1, 3, 4, 5, 6])
  assert.equal(state.cursor, 'older-3')
  assert.equal(state.error, '')
})

test('切换筛选清空旧页，详情继承用户范围而不携带列表游标', async () => {
  const paths = []
  const state = new RecentLogState(async path => { paths.push(path); return path.includes('/recent/') ? { id: 1 } : page([1], 'cursor') }, RequestGate)
  state.select({ scope: 'user', userId: '7', days: '7', sourceId: '9', query: '周杰伦' })
  await state.load(); await state.detail(1)
  const params = new URLSearchParams(paths[1].split('?')[1])
  assert.equal(params.get('scope'), 'user')
  assert.equal(params.get('userId'), '7')
  assert.equal(params.has('cursor'), false)
  assert.equal(state.select({ scope: 'me', days: '30', client: '飞牛音乐' }), true)
  assert.deepEqual(state.items, [])
  assert.equal(state.cursor, '')
  assert.equal(state.paged, false)
})

test('302和代理结果明确描述服务端交付，开始上报没有虚构交付成功', () => {
  assert.equal(outcome({ outcome: 'success', delivery: '302' }).label, '直链已交付')
  assert.equal(outcome({ outcome: 'success', delivery: 'proxy' }).label, '传输完成')
  assert.equal(outcome({ outcome: 'reported' }).label, '开始播放上报')
  assert.equal(outcome({ outcome: 'interrupted' }).label, '传输中断')
})
