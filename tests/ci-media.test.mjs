import test from 'node:test'
import assert from 'node:assert/strict'
import { resolveUbuntuHosts } from '../scripts/ci-ubuntu-hosts.mjs'

test('CI 的 HTTPS DNS 只接受 IPv4 地址并按固定 Ubuntu 域名输出', async () => {
  const result = await resolveUbuntuHosts(['archive.ubuntu.com', 'archive.ubuntu.com'], async () => ({ ok: true, json: async () => ({ Status: 0, Answer: [
    { type: 5, data: 'mirror.example' }, { type: 1, data: '192.0.2.10' },
    { type: 1, data: '192.0.2.10' }, { type: 1, data: '192.0.2.11\nmalicious.example' }, { type: 28, data: '2001:db8::1' },
  ] }) }))
  assert.equal(result, '192.0.2.10 archive.ubuntu.com\n')
})

test('DNS 故障尝试另一 HTTPS 解析器，全部失败时不给出部分 hosts 记录', async () => {
  const calls = []
  const result = await resolveUbuntuHosts(['security.ubuntu.com'], async url => {
    calls.push(url.hostname)
    if (calls.length === 1) throw new Error('故障')
    return { ok: true, json: async () => ({ Status: 0, Answer: [{ type: 1, data: '192.0.2.20' }] }) }
  })
  assert.deepEqual(calls, ['dns.google', 'cloudflare-dns.com'])
  assert.equal(result, '192.0.2.20 security.ubuntu.com\n')
  await assert.rejects(resolveUbuntuHosts(['archive.ubuntu.com', 'security.ubuntu.com'], async () => ({ ok: true, json: async () => ({ Status: 2 }) })), /HTTPS DNS 无法解析/)
})

test('CI 不允许通过 HTTPS DNS 接口修改其他系统域名', async () => {
  let called = false
  await assert.rejects(resolveUbuntuHosts(['github.com'], async () => { called = true }), /仅允许解析/)
  assert.equal(called, false)
})
