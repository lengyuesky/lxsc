// needle 兼容层：把 needle.request(method, url, data, options, cb) 映射到宿主 __host.fetch
// 语义对齐 needle：
//   - data 为 string/Buffer 时原样发送
//   - data 为对象且 options.json 为 true 时发送 JSON
//   - data 为对象且 json 非 true 时发送 x-www-form-urlencoded
//   - 默认不跟随重定向（needle follow_max 默认为 0）
const isBuffer = v => typeof Buffer !== 'undefined' && Buffer.isBuffer(v)

const toForm = obj => Object.keys(obj).map(k => {
  const v = obj[k]
  if (v === undefined) return null
  if (Array.isArray(v)) return v.map(i => `${encodeURIComponent(k)}=${encodeURIComponent(i)}`).join('&')
  return `${encodeURIComponent(k)}=${encodeURIComponent(v == null ? '' : typeof v === 'object' ? JSON.stringify(v) : v)}`
}).filter(Boolean).join('&')

const hasHeader = (headers, name) => Object.keys(headers).some(k => k.toLowerCase() === name)

function request(method, url, data, options, callback) {
  if (typeof options === 'function') {
    callback = options
    options = {}
  }
  options = options || {}
  const headers = Object.assign({}, options.headers || {})
  let body = null
  if (data != null) {
    if (typeof data === 'string' || isBuffer(data)) {
      body = data
    } else if (typeof data === 'object') {
      if (options.json) {
        body = JSON.stringify(data)
        if (!hasHeader(headers, 'content-type')) headers['Content-Type'] = 'application/json'
      } else if (options.multipart) {
        body = JSON.stringify(data) // 极少使用；多段表单暂按 JSON 处理
      } else {
        body = toForm(data)
        if (!hasHeader(headers, 'content-type')) headers['Content-Type'] = 'application/x-www-form-urlencoded'
      }
    } else {
      body = String(data)
    }
  }
  if (!hasHeader(headers, 'accept')) headers['Accept'] = '*/*'
  const timeout = options.response_timeout || options.open_timeout || options.read_timeout || options.timeout || 0
  const follow = options.follow_max != null ? options.follow_max : (options.follow === true ? 10 : (options.follow || 0))

  const req = { aborted: false, abort() { this.aborted = true; if (this._cancel) this._cancel() } }
  const handle = __host.fetch({
    url,
    method: String(method || 'get').toUpperCase(),
    headers,
    body,
    timeout,
    follow,
    insecure: options.rejectUnauthorized === false,
  }, (err, resp) => {
    if (req.aborted && !err) return
    if (err) {
      const e = new Error(err.message || String(err))
      if (err.code) e.code = err.code
      return callback(e, null, null)
    }
    const raw = resp.raw
    const r = {
      statusCode: resp.statusCode,
      statusMessage: resp.statusMessage,
      headers: resp.headers,
      raw,
      bytes: raw.length,
      body: raw,
    }
    callback(null, r, raw)
  })
  req._cancel = () => handle && handle.abort && handle.abort()
  return { request: req }
}

const shortcut = method => (url, data, options, callback) => {
  if (typeof data === 'function' || (method === 'get' || method === 'head')) {
    // get/head: (url, options, cb)
    return request(method, url, null, data, options)
  }
  return request(method, url, data, options, callback)
}

const needle = {
  request,
  get: shortcut('get'),
  head: shortcut('head'),
  post: shortcut('post'),
  put: shortcut('put'),
  patch: shortcut('patch'),
  delete: shortcut('delete'),
}
export default needle
export { request }
