import { isIP } from 'node:net'
import { fileURLToPath } from 'node:url'
import path from 'node:path'

const allowedHosts = new Set(['archive.ubuntu.com', 'security.ubuntu.com', 'ports.ubuntu.com'])
const resolvers = ['https://dns.google/resolve', 'https://cloudflare-dns.com/dns-query']

// 仅为固定 Ubuntu 包域名生成 hosts 记录；TLS 主机名和 APT 签名仍照常校验。
export async function resolveUbuntuHosts(hosts, request = fetch) {
  hosts = [...new Set(hosts)]
  if (!hosts.length || hosts.some(host => !allowedHosts.has(host))) throw new Error('仅允许解析 CI 的 Ubuntu 包域名')
  const records = await Promise.all(hosts.map(async host => {
    for (const resolver of resolvers) {
      try {
        const url = new URL(resolver)
        url.searchParams.set('name', host)
        url.searchParams.set('type', 'A')
        const response = await request(url, { headers: { accept: 'application/dns-json' }, signal: AbortSignal.timeout(8000) })
        if (!response.ok) continue
        const data = await response.json()
        if (data.Status !== 0) continue
        const addresses = [...new Set((data.Answer || []).filter(answer => answer.type === 1 && isIP(answer.data) === 4).map(answer => answer.data))]
        if (addresses.length) return addresses.map(address => `${address} ${host}`).join('\n')
      } catch { /* 公共解析器暂不可用时尝试下一个，不输出自由文本响应。 */ }
    }
    throw new Error(`HTTPS DNS 无法解析 ${host}`)
  }))
  return records.join('\n') + '\n'
}

if (process.argv[1] && fileURLToPath(import.meta.url) === path.resolve(process.argv[1])) {
  try { process.stdout.write(await resolveUbuntuHosts(process.argv.slice(2))) }
  catch (error) { console.error(error.message); process.exitCode = 1 }
}
