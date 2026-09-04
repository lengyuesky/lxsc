// 运行在 goja 中的前置脚本：补齐常用全局对象，并构造洛雪音源脚本所需的 lx 对象。
// 语义对齐 lx-music-desktop/src/main/modules/userApi/renderer/preload.js
import needle from '../shims/needle.js'
import crypto from '../shims/crypto.js'
import zlib from '../shims/zlib.js'
import { Buffer as NodeBuffer } from 'buffer'

// 使用完整的 Node Buffer 实现（feross/buffer），覆盖 goja_nodejs 的精简版
globalThis.Buffer = NodeBuffer

const g = globalThis
g.global = g
g.window = g
g.self = g

// 浏览器全局最小存根（部分 SDK/脚本会探测）
if (typeof g.navigator !== 'object') g.navigator = { userAgent: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36', language: 'zh-CN', platform: 'Win32' }
if (typeof g.location !== 'object') g.location = { href: 'https://localhost/', protocol: 'https:', host: 'localhost', hostname: 'localhost', origin: 'https://localhost', pathname: '/', search: '', hash: '' }
if (typeof g.document !== 'object') g.document = { cookie: '', createElement() { return {} }, getElementsByTagName() { return [] }, addEventListener() {}, removeEventListener() {} }
if (typeof g.addEventListener !== 'function') { g.addEventListener = () => {}; g.removeEventListener = () => {} }


if (typeof g.atob !== 'function') g.atob = s => Buffer.from(String(s), 'base64').toString('binary')
if (typeof g.btoa !== 'function') g.btoa = s => Buffer.from(String(s), 'binary').toString('base64')

if (typeof g.TextEncoder !== 'function') {
  g.TextEncoder = class TextEncoder {
    get encoding() { return 'utf-8' }
    encode(str = '') { return new Uint8Array(Buffer.from(String(str), 'utf8')) }
  }
}
if (typeof g.TextDecoder !== 'function') {
  g.TextDecoder = class TextDecoder {
    constructor(label = 'utf-8') { this.encoding = String(label).toLowerCase() }
    decode(input) {
      if (input == null) return ''
      const buf = Buffer.isBuffer(input) ? input : Buffer.from(input.buffer ? new Uint8Array(input.buffer, input.byteOffset || 0, input.byteLength) : input)
      return this.encoding === 'utf-8' || this.encoding === 'utf8' ? buf.toString('utf8') : __host.iconv('decode', this.encoding, buf)
    }
  }
}

// 浏览器风格 crypto 与 node 风格 crypto 合并暴露
g.crypto = Object.assign({}, crypto, { subtle: undefined, webcrypto: undefined })

if (typeof g.process !== 'object' || g.process === null) g.process = {}
if (!g.process.env) g.process.env = { NODE_ENV: 'production' }
if (!g.process.versions) g.process.versions = { node: '20.0.0' }
if (!g.process.platform) g.process.platform = 'linux'
if (typeof g.process.nextTick !== 'function') g.process.nextTick = (fn, ...args) => Promise.resolve().then(() => fn(...args))
if (typeof g.queueMicrotask !== 'function') g.queueMicrotask = fn => Promise.resolve().then(fn)

// 简单的 require：只提供 crypto/zlib/buffer/needle 等内置模块
const nativeRequire = typeof g.require === 'function' ? g.require : null
const modules = { crypto, zlib, needle, buffer: { Buffer: NodeBuffer } }
g.require = name => {
  const key = String(name).replace(/^node:/, '')
  if (modules[key]) return modules[key]
  if (nativeRequire) return nativeRequire(name)
  throw new Error(`Cannot find module '${name}'`)
}

// ----- lx 对象 -----
const EVENT_NAMES = { request: 'request', inited: 'inited', updateAlert: 'updateAlert' }
const eventNames = Object.values(EVENT_NAMES)
const events = { request: null }
let isInitedApi = false
let isShowedUpdateAlert = false

const verifyLyricInfo = info => {
  if (typeof info !== 'object' || info === null || typeof info.lyric !== 'string' || info.lyric.length > 51200) throw new Error('failed')
  return {
    lyric: info.lyric,
    tlyric: (typeof info.tlyric === 'string' && info.tlyric.length < 5120) ? info.tlyric : null,
    rlyric: (typeof info.rlyric === 'string' && info.rlyric.length < 5120) ? info.rlyric : null,
    lxlyric: (typeof info.lxlyric === 'string' && info.lxlyric.length < 8192) ? info.lxlyric : null,
  }
}

const lx = {
  EVENT_NAMES,
  request(url, options, callback) {
    if (typeof options === 'function') {
      callback = options
      options = {}
    }
    const { method = 'get', timeout, headers, body, form, formData } = options || {}
    const opts = {
      headers,
      response_timeout: typeof timeout === 'number' && timeout > 0 ? Math.min(timeout, 60000) : 60000,
    }
    let data
    if (body) {
      data = body
    } else if (form) {
      data = form
      opts.json = false
    } else if (formData) {
      data = formData
      opts.json = false
    }
    let req = needle.request(method, url, data, opts, (err, resp, body) => {
      try {
        if (err) {
          callback.call(this, err, null, null)
        } else {
          body = resp.body = resp.raw.toString()
          try {
            resp.body = JSON.parse(resp.body)
          } catch (_) { }
          body = resp.body
          callback.call(this, null, {
            statusCode: resp.statusCode,
            statusMessage: resp.statusMessage,
            headers: resp.headers,
            bytes: resp.bytes,
            raw: resp.raw,
            body,
          }, body)
        }
      } catch (e) {
        __host.log('error', `[lx.request] callback error: ${e && e.message}`)
      }
    }).request
    return () => {
      if (req && !req.aborted) req.abort()
      req = null
    }
  },
  send(eventName, data) {
    return new Promise((resolve, reject) => {
      if (!eventNames.includes(eventName)) return reject(new Error('The event is not supported: ' + eventName))
      switch (eventName) {
        case EVENT_NAMES.inited:
          if (isInitedApi) return reject(new Error('Script is inited'))
          isInitedApi = true
          __host.inited(JSON.stringify(data == null ? null : { sources: data.sources || null, openDevTools: !!data.openDevTools, message: data.message }))
          resolve()
          break
        case EVENT_NAMES.updateAlert:
          if (isShowedUpdateAlert) return reject(new Error('The update alert can only be called once.'))
          isShowedUpdateAlert = true
          __host.updateAlert(JSON.stringify(data || null))
          resolve()
          break
        default:
          reject(new Error('Unknown event name: ' + eventName))
      }
    })
  },
  on(eventName, handler) {
    if (!eventNames.includes(eventName)) return Promise.reject(new Error('The event is not supported: ' + eventName))
    switch (eventName) {
      case EVENT_NAMES.request:
        events.request = handler
        break
      default:
        return Promise.reject(new Error('The event is not supported: ' + eventName))
    }
    return Promise.resolve()
  },
  utils: {
    crypto: {
      aesEncrypt(buffer, mode, key, iv) {
        const cipher = crypto.createCipheriv(mode, key, iv)
        return Buffer.concat([cipher.update(buffer), cipher.final()])
      },
      rsaEncrypt(buffer, key) {
        buffer = Buffer.concat([Buffer.alloc(128 - buffer.length), buffer])
        return crypto.publicEncrypt({ key, padding: crypto.constants.RSA_NO_PADDING }, buffer)
      },
      randomBytes(size) {
        return crypto.randomBytes(size)
      },
      md5(str) {
        return crypto.createHash('md5').update(str).digest('hex')
      },
    },
    buffer: {
      from(...args) {
        return Buffer.from(...args)
      },
      bufToString(buf, format) {
        return Buffer.from(buf, 'binary').toString(format)
      },
    },
    zlib: {
      inflate(buf) {
        return new Promise((resolve, reject) => {
          zlib.inflate(buf, (err, data) => {
            if (err) reject(new Error(err.message))
            else resolve(data)
          })
        })
      },
      deflate(data) {
        return new Promise((resolve, reject) => {
          zlib.deflate(data, (err, buf) => {
            if (err) reject(new Error(err.message))
            else resolve(buf)
          })
        })
      },
    },
  },
  currentScriptInfo: {
    name: '',
    description: '',
    version: '',
    author: '',
    homepage: '',
    rawScript: '',
  },
  version: '2.0.0',
  env: 'desktop',
}
g.lx = lx

// 宿主在加载脚本前调用，写入脚本元信息
g.__lx_setScriptInfo = info => {
  Object.assign(lx.currentScriptInfo, info || {})
}

// 宿主调用：向脚本发起 request 事件，返回 Promise<结果>
g.__lx_request = data => {
  if (!events.request) return Promise.reject(new Error('Request event is not defined'))
  return Promise.resolve().then(() => events.request.call(lx, { source: data.source, action: data.action, info: data.info })).then(response => {
    switch (data.action) {
      case 'musicUrl':
        if (typeof response !== 'string' || response.length > 2048 || !/^https?:/.test(response)) throw new Error('failed')
        return { type: data.info.type, url: response }
      case 'lyric':
        return verifyLyricInfo(response)
      case 'pic':
        if (typeof response !== 'string' || response.length > 2048 || !/^https?:/.test(response)) throw new Error('failed')
        return response
      default:
        return response
    }
  })
}

g.__lx_isInited = () => isInitedApi
