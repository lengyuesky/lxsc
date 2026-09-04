// dns 存根：SDK 仅用于缓存主机 IP，这里直接回传主机名
export const lookup = (hostname, options, callback) => {
  if (typeof options === 'function') callback = options
  Promise.resolve().then(() => callback(null, hostname, 4))
}
export default { lookup }
