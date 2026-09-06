// lxsc 修改标记（2026-09-06 补记）：请求与代理改由 Go 宿主处理，保留 needle 语义封装；详见 js-bridge/vendor/PATCHES.md。
// 服务端版本 request.js：代理由 Go 宿主处理，这里只保留 needle 语义封装
import needle from 'needle'
import { debugRequest } from './env'
import { requestMsg } from './message'
import { bHh } from './musicSdk/options'
import { deflateRaw } from 'zlib'

const request = (url, options, callback) => {
  let data
  if (options.body) {
    data = options.body
  } else if (options.form) {
    data = options.form
    options.json = false
  } else if (options.formData) {
    data = options.formData
    options.json = false
  }
  options.response_timeout = options.timeout

  return needle.request(options.method || 'get', url, data, options, (err, resp, body) => {
    if (!err) {
      body = resp.body = resp.raw.toString()
      try {
        resp.body = JSON.parse(resp.body)
      } catch (_) { }
      body = resp.body
    }
    callback(err, resp, body)
  }).request
}

const defaultHeaders = {
  'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36',
}

const buildHttpPromose = (url, options) => {
  let obj = {
    isCancelled: false,
    cancelHttp: () => {
      if (!obj.requestObj) return obj.isCancelled = true
      cancelHttp(obj.requestObj)
      obj.requestObj = null
      obj.promise = obj.cancelHttp = null
      if (obj.cancelFn) obj.cancelFn(new Error(requestMsg.cancelRequest))
      obj.cancelFn = null
    },
  }
  obj.promise = new Promise((resolve, reject) => {
    obj.cancelFn = reject
    debugRequest && console.log(`\n---send request------${url}------------`)
    fetchData(url, options.method, options, (err, resp, body) => {
      debugRequest && console.log(`\n---response------${url}------------`)
      debugRequest && console.log(body)
      obj.requestObj = null
      obj.cancelFn = null
      if (err) return reject(err)
      resolve(resp)
    }).then(ro => {
      obj.requestObj = ro
      if (obj.isCancelled) obj.cancelHttp()
    })
  })
  return obj
}

export const httpFetch = (url, options = { method: 'get' }) => {
  const requestObj = buildHttpPromose(url, options)
  requestObj.promise = requestObj.promise.catch(err => {
    if (err.message === 'socket hang up') {
      return Promise.reject(new Error(requestMsg.unachievable))
    }
    switch (err.code) {
      case 'ETIMEDOUT':
      case 'ESOCKETTIMEDOUT':
        return Promise.reject(new Error(requestMsg.timeout))
      case 'ENOTFOUND':
        return Promise.reject(new Error(requestMsg.notConnectNetwork))
      default:
        return Promise.reject(err)
    }
  })
  return requestObj
}

export const cancelHttp = requestObj => {
  if (!requestObj) return
  if (!requestObj.aborted) requestObj.abort()
  requestObj = null
}

export const http = (url, options, cb) => {
  if (typeof options === 'function') {
    cb = options
    options = {}
  }
  return fetchData(url, options.method || 'get', options, cb)
}

export const httpGet = (url, options, callback) => {
  if (typeof options === 'function') {
    callback = options
    options = {}
  }
  return fetchData(url, 'get', options, callback)
}

export const httpPost = (url, data, options, callback) => {
  if (typeof options === 'function') {
    callback = options
    options = {}
  }
  options.body = data
  return fetchData(url, 'post', options, callback)
}

const handleDeflateRaw = data => new Promise((resolve, reject) => {
  deflateRaw(data, (err, buf) => {
    if (err) return reject(err)
    resolve(buf)
  })
})

const regx = /(?:\d\w)+/g

const fetchData = async (url, method, {
  headers = {},
  format = 'json',
  timeout = 15000,
  ...options
}, callback) => {
  headers = Object.assign({}, headers)
  if (headers[bHh]) {
    const path = url.replace(/^https?:\/\/[\w.:]+\//, '/')
    let s = Buffer.from(bHh, 'hex').toString()
    s = s.replace(s.substr(-1), '')
    s = Buffer.from(s, 'base64').toString()
    const v1 = '2050201'
    const v2 = '10'
    let v = v1.split('-')[0].split('.').map(n => n.length < 3 ? n.padStart(3, '0') : n).join('')
    headers[s] = !s || `${(await handleDeflateRaw(Buffer.from(JSON.stringify(`${path}${v}`.match(regx), null, 1).concat(v)).toString('base64'))).toString('hex')}&${parseInt(v)}${v2}`
    delete headers[bHh]
  }
  return request(url, {
    ...options,
    method,
    headers: Object.assign({}, defaultHeaders, headers),
    timeout,
    json: format === 'json',
    rejectUnauthorized: false,
  }, (err, resp, body) => {
    if (err) return callback(err, null)
    callback(null, resp, body)
  })
}

export const checkUrl = (url, options = {}) => {
  return new Promise((resolve, reject) => {
    fetchData(url, 'head', options, (err, resp) => {
      if (err) return reject(err)
      if (resp.statusCode === 200) return resolve()
      reject(new Error(resp.statusCode))
    })
  })
}
