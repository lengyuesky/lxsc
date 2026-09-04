// node:crypto 子集，底层由 Go 宿主 __host 实现
const toBuf = (d, enc) => Buffer.isBuffer(d) ? d : Buffer.from(String(d), enc || 'utf8')

class Hash {
  constructor(alg, key) { this.alg = alg; this.key = key; this.chunks = [] }
  update(data, enc) { this.chunks.push(toBuf(data, enc)); return this }
  digest(enc) {
    const out = this.key == null ? __host.hash(this.alg, Buffer.concat(this.chunks)) : __host.hmac(this.alg, this.key, Buffer.concat(this.chunks))
    return enc ? out.toString(enc) : out
  }
}
export const createHash = alg => new Hash(String(alg).toLowerCase())
export const createHmac = (alg, key) => new Hash(String(alg).toLowerCase(), toBuf(key))

class Cipher {
  constructor(decrypt, mode, key, iv) {
    this.decrypt = decrypt
    this.mode = String(mode).toLowerCase()
    this.key = toBuf(key)
    this.iv = iv == null || iv === '' ? Buffer.alloc(0) : toBuf(iv)
    this.chunks = []
    this.autoPadding = true
  }
  setAutoPadding(v = true) { this.autoPadding = !!v; return this }
  update(data, inEnc, outEnc) {
    this.chunks.push(toBuf(data, inEnc))
    const empty = Buffer.alloc(0)
    return outEnc ? empty.toString(outEnc) : empty
  }
  final(outEnc) {
    const out = __host.aes(this.decrypt ? 'decrypt' : 'encrypt', this.mode, this.key, this.iv, Buffer.concat(this.chunks), this.autoPadding)
    return outEnc ? out.toString(outEnc) : out
  }
}
export const createCipheriv = (mode, key, iv) => new Cipher(false, mode, key, iv)
export const createDecipheriv = (mode, key, iv) => new Cipher(true, mode, key, iv)

export const constants = { RSA_PKCS1_PADDING: 1, RSA_NO_PADDING: 3, RSA_PKCS1_OAEP_PADDING: 4 }

export const publicEncrypt = (keyOpts, buffer) => {
  let key = keyOpts, padding = constants.RSA_PKCS1_PADDING
  if (keyOpts && typeof keyOpts === 'object' && !Buffer.isBuffer(keyOpts)) {
    key = keyOpts.key
    if (keyOpts.padding != null) padding = keyOpts.padding
  }
  return __host.rsaPublicEncrypt(String(Buffer.isBuffer(key) ? key.toString() : key), toBuf(buffer), padding)
}

export const randomBytes = n => __host.randomBytes(n)
export const randomUUID = () => {
  const b = __host.randomBytes(16)
  b[6] = (b[6] & 0x0f) | 0x40
  b[8] = (b[8] & 0x3f) | 0x80
  const h = b.toString('hex')
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`
}
export const getRandomValues = arr => {
  const b = __host.randomBytes(arr.length)
  for (let i = 0; i < arr.length; i++) arr[i] = b[i]
  return arr
}

export default { createHash, createHmac, createCipheriv, createDecipheriv, publicEncrypt, randomBytes, randomUUID, getRandomValues, constants }
