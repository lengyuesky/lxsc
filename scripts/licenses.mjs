// 校验随二进制分发的许可副本；更新依赖版本前仍须人工审核适用条款。
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath, pathToFileURL } from 'node:url'

const root = fileURLToPath(new URL('../', import.meta.url))
const manifestPath = path.join(root, 'licenses/manifest.json')
const legalName = /^(licen[sc]e(?:[._-].*)?|notice(?:[._-].*)?|copying(?:[._-].*)?|copyright|authors|patents)$/i

export const sha256 = bytes => createHash('sha256').update(bytes).digest('hex')

export function inside(base, relative) {
  assert.equal(typeof relative, 'string', '路径必须是字符串')
  assert(relative && !path.isAbsolute(relative) && !relative.split(/[\\/]/).includes('..'), `不安全的相对路径：${relative}`)
  return path.join(base, relative)
}

export function parseGoList(text) {
  // go list -json 的顶层对象独占行，嵌套对象有缩进，字符串内换行会转义。
  return text.trim() ? text.trim().split(/\n(?=\{)/).map(value => JSON.parse(value)) : []
}

export function assertSameSet(actual, expected, label) {
  assert.deepEqual([...new Set(actual)].sort(), [...new Set(expected)].sort(), `${label} 已变化，请先审核并更新许可清单`)
}

export function legalComments(source) {
  // 先跳过 Go 字符串、字符和原始字符串，避免把其中的示例当成源文件注释。
  const tokens = source.match(/"(?:\\[\s\S]|[^"\\])*"|'(?:\\[\s\S]|[^'\\])*'|`[^`]*`|\/\/[^\n]*(?:\n[ \t]*\/\/[^\n]*)*|\/\*[\s\S]*?\*\//g) || []
  return tokens.filter(token => token.startsWith('//') || token.startsWith('/*')).filter(token =>
    /copyright|permission is hereby granted|redistribution and use|SPDX-License-Identifier:/i.test(token),
  ).filter(token => !/^\/\/ Copyright \d{4} The Go Authors\.\n\/\/ Use of this source code is governed by a BSD-style\n\/\/ license that can be found in the LICENSE file\.$/.test(token))
}

function runGo(args, env = {}) {
  const result = spawnSync('go', args, {
    cwd: root, encoding: 'utf8', maxBuffer: 64 * 1024 * 1024,
    env: { ...process.env, ...env },
  })
  assert.equal(result.status, 0, `go ${args.join(' ')} 失败：${result.stderr || result.error || ''}`)
  return result.stdout.trim()
}

function walkLegal(dir, prefix = '') {
  const result = []
  for (const entry of fs.readdirSync(inside(dir, prefix || '.'), { withFileTypes: true })) {
    if (['.git', 'testdata', 'node_modules'].includes(entry.name)) continue
    const relative = prefix ? `${prefix}/${entry.name}` : entry.name
    if (entry.isDirectory()) result.push(...walkLegal(dir, relative))
    else if (entry.isFile() && legalName.test(entry.name)) result.push(relative)
  }
  return result.sort()
}

function installedSource(component, file, directories, goroot) {
  const dir = directories.get(component.id)
  let source = inside(dir, file.source)
  // 部分 Linux 发行版将 Go 根 LICENSE 拆分安装；仍与入库原文逐字节比较。
  if (component.kind === 'go-stdlib' && file.source === 'LICENSE' && !fs.existsSync(source)) {
    source = '/usr/share/licenses/go/LICENSE'
  }
  assert(fs.existsSync(source), `找不到原文：${component.id}/${file.source}（Go 根目录：${goroot}）`)
  return fs.readFileSync(source)
}

function sourceNotices(packages, components, goroot) {
  const moduleById = new Map(components.filter(c => c.kind === 'go-module').map(c => [c.id, c]))
  const sources = new Map()
  for (const pkg of packages) {
    if (pkg.Module?.Main) continue
    const component = pkg.Standard ? 'go-stdlib' : pkg.Module?.Path
    if (!component) continue
    const base = pkg.Standard ? goroot : pkg.Module.Dir
    for (const filename of [...(pkg.GoFiles || []), ...(pkg.SFiles || [])]) {
      const absolute = path.join(pkg.Dir, filename)
      const relative = path.relative(base, absolute).split(path.sep).join('/')
      // 这四个翻译单元完整随附，避免再次摘录其中的头文件条款。
      if (moduleById.get(component)?.files.some(f => f.source === relative && f.path.endsWith('.go.txt'))) continue
      sources.set(`${component}/${relative}`, absolute)
    }
  }
  const comments = new Map()
  for (const [name, absolute] of [...sources].sort(([a], [b]) => a.localeCompare(b, 'en'))) {
    for (const comment of legalComments(fs.readFileSync(absolute, 'utf8'))) {
      if (!comments.has(comment)) comments.set(comment, new Set())
      comments.get(comment).add(name)
    }
  }
  return '# Go 依赖源码中的版权与许可注释\n\n' +
    '按清单中的 Linux 平台、CGO_ENABLED=0 和 ./cmd/lxsc 的包依赖汇总；相同原文只列一次。\n' +
    '这不是链接器逐函数保留清单；包内未被链接的声明也可能随附。注释原文未改写。\n\n' +
    [...comments].map(([comment, names]) => `来源：\n${[...names].map(name => `- ${name}`).join('\n')}\n\n${comment}\n`).join('\n----\n\n')
}

function checkVendor() {
  const provenance = JSON.parse(fs.readFileSync(path.join(root, 'licenses/vendor-provenance.json'), 'utf8'))
  for (const file of provenance.files) {
    let bytes = fs.readFileSync(inside(root, file.path))
    if (file.change) {
      const marker = `// lxsc 修改标记（2026-09-06 补记）：${file.change}；详见 js-bridge/vendor/PATCHES.md。\n`
      assert(bytes.toString('utf8').startsWith(marker), `缺少修改标记：${file.path}`)
      bytes = bytes.subarray(Buffer.byteLength(marker))
    }
    assert.equal(sha256(bytes), file.localSha256, `vendor 内容变更后须更新来源/修改记录：${file.path}`)
  }
  const actual = []
  function walk(dir) {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const next = path.join(dir, entry.name)
      if (entry.isDirectory()) walk(next)
      else if (/\.(js|ts)$/.test(entry.name)) actual.push(path.relative(root, next).split(path.sep).join('/'))
    }
  }
  walk(path.join(root, 'js-bridge/vendor'))
  assertSameSet(actual, provenance.files.map(file => file.path), 'vendor 源文件')
}

export function checkLicenses({ update = false } = {}) {
  const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'))
  assert.equal(manifest.schemaVersion, 1, '不支持的许可清单版本')
  const ids = manifest.components.map(component => component.id)
  assert.equal(new Set(ids).size, ids.length, '许可组件标识重复')
  const version = runGo(['env', 'GOVERSION']).match(/^go(\d+\.\d+\.\d+)(?:$|-)/)?.[1]
  assert.equal(version, manifest.goVersion, 'Go 版本变化后须重新收集标准库声明')
  const goroot = runGo(['env', 'GOROOT'])
  const packages = manifest.platforms.flatMap(platform => {
    const [GOOS, GOARCH] = platform.split('/')
    return parseGoList(runGo(['list', '-deps', '-json', './cmd/lxsc'], { GOOS, GOARCH, CGO_ENABLED: '0' }))
  })
  const modules = new Map(packages.filter(p => p.Module && !p.Module.Main).map(p => [p.Module.Path, p.Module]))
  const goComponents = manifest.components.filter(c => c.kind === 'go-module')
  assertSameSet(modules.keys(), goComponents.map(c => c.id), 'Go 二进制依赖')
  const directories = new Map([['go-stdlib', goroot]])
  for (const component of goComponents) {
    const mod = modules.get(component.id)
    assert(!mod.Replace, `替换依赖须单独核对：${component.id}`)
    assert.equal(mod.Version, component.version, `Go 模块版本变化：${component.id}`)
    directories.set(component.id, mod.Dir)
    const recorded = component.files.map(f => f.source).filter(f => legalName.test(path.basename(f)))
    assertSameSet(walkLegal(mod.Dir), recorded, `${component.id} 的许可文件`)
  }
  const bundle = JSON.parse(fs.readFileSync(path.join(root, 'js-bridge/bundle-inputs.json'), 'utf8'))
  const npm = manifest.components.filter(c => c.kind === 'npm')
  assertSameSet(bundle.dependencies.map(p => `${p.name}@${p.version}`), npm.filter(c => c.role === 'bundle').map(c => `${c.id}@${c.version}`), 'JS bundle 依赖')
  for (const component of npm) {
    const dir = inside(path.join(root, 'js-bridge/node_modules'), component.id)
    const pkg = JSON.parse(fs.readFileSync(path.join(dir, 'package.json'), 'utf8'))
    assert.equal(pkg.version, component.version, `npm 包版本变化：${component.id}`)
    if (component.id === 'esbuild') assert.equal(bundle.esbuildVersion, component.version, 'JS 生成器版本变化')
    directories.set(component.id, dir)
    assertSameSet(walkLegal(dir), component.files.map(f => f.source), `${component.id} 的许可文件`)
  }
  let count = 0
  const paths = new Set()
  for (const component of manifest.components) {
    assert(component.files.length, `缺少许可原文：${component.id}`)
    for (const file of component.files) {
      assert(!paths.has(file.path), `许可副本路径重复：${file.path}`)
      paths.add(file.path)
      const destination = inside(root, file.path)
      if (component.kind !== 'archive') {
        const original = installedSource(component, file, directories, goroot)
        if (update) {
          fs.mkdirSync(path.dirname(destination), { recursive: true })
          fs.writeFileSync(destination, original)
          file.sha256 = sha256(original)
        } else {
          assert.equal(sha256(original), file.sha256, `安装的原文已变化：${component.id}/${file.source}`)
        }
      }
      assert.equal(sha256(fs.readFileSync(destination)), file.sha256, `许可副本缺失或变化：${file.path}`)
      count++
    }
  }
  const notices = sourceNotices(packages, manifest.components, goroot)
  const noticePath = inside(root, manifest.sourceNotices.path)
  if (update) {
    fs.writeFileSync(noticePath, notices)
    manifest.sourceNotices.sha256 = sha256(notices)
    fs.writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`)
  }
  assert.equal(sha256(notices), manifest.sourceNotices.sha256, 'Go 源码声明清单变化，须审核后更新')
  assert.equal(sha256(fs.readFileSync(noticePath)), manifest.sourceNotices.sha256, 'Go 源码声明副本已变化')
  for (const file of ['LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES.md']) assert(fs.statSync(path.join(root, file)).size > 0, `缺少 ${file}`)
  checkVendor()
  console.log(`许可校验通过：${goComponents.length} 个 Go 模块、Go ${version} 标准库、${npm.filter(c => c.role === 'bundle').length} 个 bundle 包及 esbuild；${count} 份原文/源码副本。`)
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  const args = process.argv.slice(2)
  assert(args.length <= 1 && (!args.length || ['--check', '--update'].includes(args[0])), '用法：node scripts/licenses.mjs [--check|--update]')
  checkLicenses({ update: args[0] === '--update' })
}
