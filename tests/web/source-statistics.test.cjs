const { test } = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')

const scope = { LXSCAdminModules: { register() {} } }
vm.runInNewContext(fs.readFileSync(require.resolve('../../internal/assets/web/admin-pages.js'), 'utf8').replaceAll('export function ', 'function ') + '\nglobalThis.stats = { summarizeSourceCalls, sourceCallHealth }', scope)
const { summarizeSourceCalls, sourceCallHealth } = scope.stats

test('音源统计按调用数量加权耗时，取消独立保留，不平均各源平均值', () => {
  const summary = summarizeSourceCalls([
    { calls: 100, success: 98, failed: 1, cancelled: 1, durationMs: 10000, averageMs: 100, maxMs: 300, attempts: 102, errors: { timeout: 1, cancelled: 1 } },
    { calls: 1, success: 0, failed: 1, durationMs: 1000, averageMs: 1000, maxMs: 1000, attempts: 2, errors: { timeout: 1 } },
  ])
  assert.equal(summary.calls, 101)
  assert.equal(summary.success, 98)
  assert.equal(summary.failed, 2)
  assert.equal(summary.cancelled, 1)
  assert.equal(summary.averageMs, 108)
  assert.equal(summary.maxMs, 1000)
  assert.equal(summary.attempts, 104)
  assert.equal(summary.errors.timeout, 2)
})

test('无调用和只有取消时显示暂无样本，避免把零调用显示为健康或故障', () => {
  for (const summary of [{}, { calls: 4, success: 0, failed: 0, cancelled: 4 }]) assert.equal(sourceCallHealth(summary).label, '暂无有效样本')
  assert.equal(sourceCallHealth({ success: 95, failed: 5, cancelled: 100 }).tone, 'ok')
  assert.equal(sourceCallHealth({ success: 94, failed: 6 }).tone, 'warn')
  assert.equal(sourceCallHealth({ success: 80, failed: 20 }).tone, 'warn')
  assert.equal(sourceCallHealth({ success: 79, failed: 21 }).tone, 'err')
  assert.equal(sourceCallHealth({ success: 0, failed: 1 }).tone, 'err')
})

test('停用、加载和未就绪状态优先于历史成功率', () => {
  const healthy = { success: 10, failed: 0 }
  assert.equal(sourceCallHealth(healthy, 'disabled').label, '已停用')
  assert.equal(sourceCallHealth(healthy, 'loading').label, '加载中')
  assert.equal(sourceCallHealth(healthy, 'unloaded').label, '未就绪')
  assert.equal(sourceCallHealth(healthy, 'error').label, '未就绪')
})
