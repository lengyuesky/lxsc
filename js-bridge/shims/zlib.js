// node:zlib 子集：同步由 __host.zlib(op, buf) 实现，异步接口用微任务包装
const toBuf = d => Buffer.isBuffer(d) ? d : Buffer.from(d)
const mk = op => (data, options, cb) => {
  if (typeof options === 'function') cb = options
  let result, error = null
  try { result = __host.zlib(op, toBuf(data)) } catch (e) { error = e instanceof Error ? e : new Error(String(e)) }
  Promise.resolve().then(() => cb(error, result))
}
const mkSync = op => (data) => __host.zlib(op, toBuf(data))
export const inflate = mk('inflate')
export const deflate = mk('deflate')
export const inflateRaw = mk('inflateRaw')
export const deflateRaw = mk('deflateRaw')
export const gunzip = mk('gunzip')
export const gzip = mk('gzip')
export const unzip = mk('unzip')
export const inflateSync = mkSync('inflate')
export const deflateSync = mkSync('deflate')
export const inflateRawSync = mkSync('inflateRaw')
export const deflateRawSync = mkSync('deflateRaw')
export const gunzipSync = mkSync('gunzip')
export const gzipSync = mkSync('gzip')
export const unzipSync = mkSync('unzip')
export default { inflate, deflate, inflateRaw, deflateRaw, gunzip, gzip, unzip, inflateSync, deflateSync, inflateRawSync, deflateRawSync, gunzipSync, gzipSync, unzipSync }
