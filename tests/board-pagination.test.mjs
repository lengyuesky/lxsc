import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'

function sdk(source, globals = {}) {
  const code = fs.readFileSync(new URL('../js-bridge/vendor/musicSdk/' + source + '/leaderboard.js', import.meta.url), 'utf8')
    .replace(/^import .*$/gm, '').replace('export default', 'globalThis.board =')
  const ctx = vm.createContext(globals)
  vm.runInContext(code, ctx)
  return ctx.board
}

test('酷我榜单的数字歌手 ID 不得使整页歌曲解析失败', () => {
  // 使用真实实体解码器，避免把它替换成恒等函数而漏掉数字字段崩溃。
  const helpers = vm.createContext({})
  vm.runInContext(fs.readFileSync(new URL('../js-bridge/vendor/index.js', import.meta.url), 'utf8')
    .replace(/export const /g, 'globalThis.'), helpers)
  const board = sdk('kw', {
    decodeName: helpers.decodeName, formatPlayTime: helpers.formatPlayTime,
    formatSinger: value => value.replace(/&/g, '、'), formatPic: value => value,
  })
  const list = board.filterData([
    { id: 1, artistid: 12345, artist: '甲&amp;乙', name: '歌曲&lt;一&gt;', album: '专辑', n_minfo: '', duration: 180 },
    { id: 2, artistId: '67890', artist: '丙', name: '歌曲二', album: null, n_minfo: '', duration: 120 },
    { id: 3, artist: '丁', name: '歌曲三', n_minfo: '', duration: 60 },
  ])
  assert.equal(list.length, 3)
  assert.equal(list[0].singerId, '12345')
  assert.equal(list[0].singer, '甲、乙')
  assert.equal(list[0].name, '歌曲<一>')
  assert.equal(list[1].singerId, '67890')
  assert.equal(list[1].albumName, '')
  assert.equal(list[2].singerId, '')
})

test('QQ 榜单逐页传递 offset，整榜总数不取第一页长度', async () => {
  const requests = []
  const board = sdk('tx', { httpFetch: (_url, options) => {
    requests.push(options.body.toplist.param)
    return { promise: Promise.resolve({ body: { code: 0, toplist: { data: { data: { totalNum: 601 }, songInfoList: [{ id: 601 }] } } } }) }
  } })
  board.periods = { 26: { period: '2026-10-03' } }
  board.filterData = list => list
  const result = await board.getList('26', 3)
  assert.equal(requests[0].num, 300)
  assert.equal(requests[0].offset, 600)
  assert.equal(result.total, 601)
  assert.equal(result.page, 3)
  assert.equal(result.list.length, 1)
})

test('QQ 缺少总数时保持未知，交由后续分页判断末页', async () => {
  const board = sdk('tx', { httpFetch: () => ({ promise: Promise.resolve({ body: { code: 0, toplist: { data: { songInfoList: [1, 2] } } } }) }) })
  board.periods = { 26: { period: '2026-10-03' } }
  board.filterData = list => list
  assert.equal((await board.getList('26', 1)).total, undefined)
})

test('咪咕请求完整栏目，超过200首仍保留完整结果', async () => {
  let requested
  const board = sdk('mg', { filterMusicInfoList: list => list, httpFetch: url => {
    requested = url
    return { promise: Promise.resolve({ statusCode: 200, body: { code: '000000', columnInfo: { contents: Array.from({ length: 501 }, (_, id) => ({ objectInfo: { id } })) } } }) }
  } })
  const result = await board.getList('27553319', 1)
  assert.equal(new URL(requested).searchParams.get('needAll'), '1')
  assert.equal(result.list.length, 501)
  assert.equal(result.total, 501)
})
