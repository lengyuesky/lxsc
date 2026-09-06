// lxsc 修改标记（2026-09-06 补记）：音频 URL 改由宿主的自定义音源提供；详见 js-bridge/vendor/PATCHES.md。
// 服务端版本：musicUrl 由 Go 宿主通过音源脚本获取，SDK 内不再处理
const apis = source => ({
  getMusicUrl() {
    return Promise.reject(new Error(`musicUrl for ${source} is handled by host`))
  },
})
const supportQuality = {}
export { apis, supportQuality }
