// 各平台 SDK 打包入口：暴露 globalThis.__sdk 与统一调用函数 __sdk_call
import kw from './vendor/musicSdk/kw/index'
import kg from './vendor/musicSdk/kg/index'
import tx from './vendor/musicSdk/tx/index'
import wy from './vendor/musicSdk/wy/index'
import mg from './vendor/musicSdk/mg/index'

const sdk = { kw, kg, tx, wy, mg }
globalThis.__sdk = sdk

// path 形如 "wy.musicSearch.search"；args 为参数数组。
// 使用 Object.create(parent) 作为 this，避免 SDK 内 this._requestObj 的"取消上一次请求"逻辑在并发调用时互相干扰。
globalThis.__sdk_call = (path, args) => {
  const parts = String(path).split('.')
  let parent = sdk
  let fn = null
  for (let i = 0; i < parts.length; i++) {
    const next = parent[parts[i]]
    if (next == null) return Promise.reject(new Error(`sdk method not found: ${path}`))
    if (i === parts.length - 1) fn = next
    else parent = next
  }
  if (typeof fn !== 'function') return Promise.reject(new Error(`sdk method not callable: ${path}`))
  try {
    const r = fn.apply(Object.create(parent), Array.isArray(args) ? args : [])
    // 部分方法返回 { requestObj, promise } 形式，取其 promise
    if (r && typeof r === 'object' && r.promise && typeof r.promise.then === 'function') return r.promise
    return Promise.resolve(r)
  } catch (e) {
    return Promise.reject(e)
  }
}

// 统一的"按平台 ID 取歌曲元数据"接口，返回与搜索结果同构的对象或 null
import wyDetail from './vendor/musicSdk/wy/musicDetail'
import txInfo from './vendor/musicSdk/tx/musicInfo'
import { getMusicInfos as kgInfos } from './vendor/musicSdk/kg/musicInfo'
import { getMusicInfos as mgInfos } from './vendor/musicSdk/mg/musicInfo'

globalThis.__sdk_info = async (source, key) => {
  switch (source) {
    case 'wy': {
      const r = await wyDetail.getList([key])
      return (r && r.list && r.list[0]) || null
    }
    case 'tx':
      return (await txInfo(key)) || null
    case 'kg': {
      const r = await kgInfos([{ hash: key }])
      return (r && r[0]) || null
    }
    case 'mg': {
      const r = await mgInfos([key])
      return (r && r[0]) || null
    }
    case 'kw': {
      const info = await kw.getMusicInfo({ songmid: key })
      if (!info) return null
      return {
        source: 'kw', songmid: String(key), name: info.name, singer: info.artist, albumName: info.album,
        albumId: info.albumid != null ? String(info.albumid) : '', img: info.pic, interval: info.songTimeMinutes,
        types: [{ type: '128k' }, { type: '320k' }, ...(info.hasLossless ? [{ type: 'flac' }] : [])],
        _types: { '128k': {}, '320k': {}, ...(info.hasLossless ? { flac: {} } : {}) },
      }
    }
  }
  return null
}
