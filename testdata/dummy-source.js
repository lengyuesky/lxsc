/**
 * @name 测试音源
 * @description 仅用于测试，返回固定链接
 * @version 1.0.0
 * @author lxsc
 * @homepage https://example.com
 */
const { EVENT_NAMES, request, on, send, utils } = globalThis.lx
const qualitys = ['128k', '320k', 'flac']
on(EVENT_NAMES.request, ({ source, action, info }) => {
  if (action === 'musicUrl') {
    return new Promise((resolve, reject) => {
      // 走一次 request 验证桥接（对不存在的域名会失败则回退）
      request('https://example.com/', { method: 'get', timeout: 5000 }, (err, resp) => {
        resolve(`https://cdn.example.com/${source}/${info.musicInfo.songmid}.${info.type}.mp3?ok=${err ? 0 : resp.statusCode}`)
      })
    })
  }
  return Promise.reject(new Error('action not supported'))
})
send(EVENT_NAMES.inited, {
  status: true,
  openDevTools: false,
  sources: {
    wy: { name: '网易', type: 'music', actions: ['musicUrl'], qualitys },
    tx: { name: 'QQ', type: 'music', actions: ['musicUrl'], qualitys },
  },
})
