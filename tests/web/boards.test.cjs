const { test } = require('node:test')
const assert = require('node:assert/strict')
const { BoardSelectionState } = require('../../internal/assets/web/board-settings.js')

function deferred() {
  let resolve, reject
  const promise = new Promise((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
const boards = () => [{ bangid: 'one', name: '热歌榜' }, { bangid: 'two', name: '新歌榜' }]

test('全部模式包含新增榜单，自定义保留显式空选择并隔离同名平台', async () => {
  const model = new BoardSelectionState(async () => boards())
  await model.load('wy')
  assert.deepEqual(model.snapshot(), {})
  model.setMode('wy', 'custom')
  model.toggle('wy', 'one', false)
  await model.load('tx')
  model.setMode('tx', 'custom')
  model.toggle('tx', 'two', false)
  assert.deepEqual(model.snapshot(), { wy: ['two'], tx: ['one'] })
  model.fetchBoards = async () => [...boards(), { bangid: 'new', name: '新榜单' }]
  await model.load('wy', true)
  assert.deepEqual(model.snapshot(), { wy: ['two'], tx: ['one'] })
  model.clear('wy')
  assert.deepEqual(model.snapshot(), { wy: [], tx: ['one'] })
  model.setMode('wy', 'all')
  assert.deepEqual(model.snapshot(), { tx: ['one'] })
})

test('失败平台和未加载平台的选择参与保存，重试成功后也不自动丢失缺失项', async () => {
  const model = new BoardSelectionState(async () => { throw new Error('平台暂不可用') })
  model.reset({ wy: ['one', 'missing'], tx: [] })
  await model.load('wy')
  assert.equal(model.catalog('wy').error, '平台暂不可用')
  assert.deepEqual(model.snapshot(), { wy: ['one', 'missing'], tx: [] })
  model.fetchBoards = async () => boards()
  await model.load('wy', true)
  assert.equal(model.catalog('wy').error, '')
  assert.equal(model.options('wy').find(board => board.bangid === 'missing').missing, true)
  model.selectAll('wy')
  assert.deepEqual(model.snapshot(), { wy: ['one', 'missing', 'two'], tx: [] })
  model.toggle('wy', 'missing', false)
  assert.deepEqual(model.snapshot(), { wy: ['one', 'two'], tx: [] })
})

test('等候榜单时的取消勾选和清空不会被迟到目录覆盖', async () => {
  const pending = deferred()
  const model = new BoardSelectionState(() => pending.promise)
  model.reset({ wy: ['one'], tx: ['two'] })
  const loading = model.load('wy')
  model.clear('wy')
  pending.resolve(boards())
  await loading
  assert.equal(model.catalog('wy').loaded, true)
  assert.deepEqual(model.snapshot(), { wy: [], tx: ['two'] })
  assert.equal(model.isCustom('wy'), true)
})

test('被替代的目录请求不能覆盖较新目录或选择', async () => {
  const first = deferred(), second = deferred(), signals = []
  const model = new BoardSelectionState((source, signal) => { signals.push(signal); return signals.length === 1 ? first.promise : second.promise })
  model.reset({ wy: ['two'] })
  const oldLoad = model.load('wy'), newLoad = model.load('wy', true)
  assert.equal(signals[0].aborted, true)
  second.resolve([{ bangid: 'two', name: '更新后的名称' }])
  await newLoad
  model.clear('wy')
  first.resolve([{ bangid: 'one', name: '旧名称' }])
  await oldLoad
  assert.equal(model.catalog('wy').boards[0].name, '更新后的名称')
  assert.deepEqual(model.snapshot(), { wy: [] })
})

test('退出或重新读取设置会取消旧加载，设置快照和输入不共享数组', async () => {
  const pending = deferred()
  let signal
  const model = new BoardSelectionState((source, current) => { signal = current; return pending.promise })
  const original = { wy: ['one'], tx: [] }
  model.reset(original)
  original.wy[0] = 'changed'
  const snapshot = model.snapshot()
  snapshot.wy[0] = 'changed'
  delete snapshot.tx
  assert.deepEqual(model.snapshot(), { wy: ['one'], tx: [] })
  const loading = model.load('wy')
  model.reset({ tx: ['two'] })
  assert.equal(signal.aborted, true)
  pending.resolve(boards())
  await loading
  assert.deepEqual(model.snapshot(), { tx: ['two'] })
  assert.equal(model.catalogs.has('wy'), false)
})
