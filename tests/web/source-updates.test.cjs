const { test } = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')

const scope = { LXSCAdminModules: { register() {} } }
vm.runInNewContext(fs.readFileSync(require.resolve('../../internal/assets/web/admin-pages.js'), 'utf8').replaceAll('export function ', 'function ') + '\nglobalThis.updateState = sourceUpdateState', scope)

test('音源更新区分未配置、关闭、停用暂停和每日开启', () => {
  const state = source => scope.updateState({ enabled: true, ...source })
  assert.match(state({}).automatic, /尚未设置来源/)
  assert.match(state({ url: 'https://source.invalid', autoUpdate: false }).automatic, /已关闭/)
  assert.match(state({ url: 'https://source.invalid', autoUpdate: true, enabled: false }).automatic, /暂停/)
  assert.match(state({ url: 'https://source.invalid', autoUpdate: true }).automatic, /北京时间 3:00/)
})

test('音源检查状态以在途检查优先，未变化与已更新明确区分', () => {
  assert.equal(scope.updateState({ updateResult: 'unchanged' }).result, '内容未变化')
  assert.equal(scope.updateState({ updateResult: 'updated' }).result, '已更新')
  assert.equal(scope.updateState({ updateResult: 'failed' }).failed, true)
  const checking = scope.updateState({ updateResult: 'failed', updateRunning: true })
  assert.match(checking.result, /正在检查/)
  assert.equal(checking.failed, false)
})
