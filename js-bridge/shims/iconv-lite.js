// iconv-lite 子集：由宿主 __host.iconv 实现（gb18030/gbk/utf8 等）
const decode = (buf, enc) => __host.iconv('decode', String(enc || 'utf8').toLowerCase(), Buffer.isBuffer(buf) ? buf : Buffer.from(buf))
const encode = (str, enc) => __host.iconv('encode', String(enc || 'utf8').toLowerCase(), String(str))
export { decode, encode }
export default { decode, encode }
