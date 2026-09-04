// 使用 esbuild 打包 prelude 与 SDK，目标 ES2017（goja 支持范围内），产物写入 ../internal/assets/js
import * as esbuild from 'esbuild'
import path from 'node:path'

const root = import.meta.dir
const out = path.resolve(root, '../internal/assets/js')

const aliasPlugin: esbuild.Plugin = {
  name: 'lxsc-alias',
  setup(build) {
    const map: Record<string, string> = {
      crypto: 'shims/crypto.js',
      'node:crypto': 'shims/crypto.js',
      zlib: 'shims/zlib.js',
      'node:zlib': 'shims/zlib.js',
      dns: 'shims/dns.js',
      'node:dns': 'shims/dns.js',
      needle: 'shims/needle.js',
      'iconv-lite': 'shims/iconv-lite.js',
      '@renderer/utils': 'shims/renderer-utils.js',
      '@renderer/utils/musicSdk/kg/vendors/infSign.min': 'vendor/musicSdk/kg/vendors/infSign.min.js',
      '@common/utils/lyricUtils/kg': 'vendor/common/lyricUtils/kg.js',
    }
    build.onResolve({ filter: /.*/ }, args => {
      const target = map[args.path]
      if (target) return { path: path.resolve(root, target) }
      if (args.path.startsWith('@renderer/') || args.path.startsWith('@common/')) {
        throw new Error(`未映射的别名: ${args.path}（来自 ${args.importer}）`)
      }
      return null
    })
  },
}

const common: esbuild.BuildOptions = {
  bundle: true,
  format: 'iife',
  target: ['es2017'],
  platform: 'neutral',
  mainFields: ['module', 'main'],
  plugins: [aliasPlugin],
  logLevel: 'info',
  legalComments: 'none',
  minify: false,
  define: { 'process.env.NODE_ENV': '"production"' },
}

await esbuild.build({ ...common, entryPoints: [path.join(root, 'prelude/lx.js')], outfile: path.join(out, 'prelude.js') })
await esbuild.build({ ...common, entryPoints: [path.join(root, 'sdk-entry.js')], outfile: path.join(out, 'sdk.bundle.js') })
console.log('构建完成 →', out)
