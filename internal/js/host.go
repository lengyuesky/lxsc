package js

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/buffer"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
)

// installHost 向运行时注入 __host 对象
func installHost(w *Worker, vm *goja.Runtime) {
	host := vm.NewObject()
	_ = host.Set("fetch", func(call goja.FunctionCall) goja.Value { return hostFetch(w, vm, call) })
	_ = host.Set("hash", func(call goja.FunctionCall) goja.Value {
		alg := call.Argument(0).String()
		data := buffer.Bytes(vm, call.Argument(1))
		h := newHash(alg)
		if h == nil {
			panic(vm.NewTypeError("unsupported hash: " + alg))
		}
		h.Write(data)
		return buffer.WrapBytes(vm, h.Sum(nil))
	})
	_ = host.Set("hmac", func(call goja.FunctionCall) goja.Value {
		alg := call.Argument(0).String()
		key := buffer.Bytes(vm, call.Argument(1))
		data := buffer.Bytes(vm, call.Argument(2))
		mk := func() hash.Hash { return newHash(alg) }
		if mk() == nil {
			panic(vm.NewTypeError("unsupported hmac: " + alg))
		}
		m := hmac.New(mk, key)
		m.Write(data)
		return buffer.WrapBytes(vm, m.Sum(nil))
	})
	_ = host.Set("aes", func(call goja.FunctionCall) goja.Value {
		op := call.Argument(0).String()
		mode := call.Argument(1).String()
		key := buffer.Bytes(vm, call.Argument(2))
		iv := []byte{}
		if a := call.Argument(3); !goja.IsUndefined(a) && !goja.IsNull(a) {
			iv = buffer.Bytes(vm, a)
		}
		data := buffer.Bytes(vm, call.Argument(4))
		autoPad := true
		if a := call.Argument(5); !goja.IsUndefined(a) {
			autoPad = a.ToBoolean()
		}
		out, err := aesCrypt(op == "decrypt", mode, key, iv, data, autoPad)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return buffer.WrapBytes(vm, out)
	})
	_ = host.Set("rsaPublicEncrypt", func(call goja.FunctionCall) goja.Value {
		keyPEM := call.Argument(0).String()
		data := buffer.Bytes(vm, call.Argument(1))
		padding := int(call.Argument(2).ToInteger())
		out, err := rsaPublicEncrypt(keyPEM, data, padding)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return buffer.WrapBytes(vm, out)
	})
	_ = host.Set("randomBytes", func(call goja.FunctionCall) goja.Value {
		n := int(call.Argument(0).ToInteger())
		if n < 0 || n > 1<<20 {
			panic(vm.NewTypeError("invalid size"))
		}
		b := make([]byte, n)
		_, _ = rand.Read(b)
		return buffer.WrapBytes(vm, b)
	})
	_ = host.Set("zlib", func(call goja.FunctionCall) goja.Value {
		op := call.Argument(0).String()
		data := buffer.Bytes(vm, call.Argument(1))
		out, err := zlibOp(op, data)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return buffer.WrapBytes(vm, out)
	})
	_ = host.Set("iconv", func(call goja.FunctionCall) goja.Value {
		op := call.Argument(0).String()
		enc := call.Argument(1).String()
		e := lookupEncoding(enc)
		if op == "decode" {
			data := buffer.Bytes(vm, call.Argument(2))
			if e == nil {
				return vm.ToValue(string(data))
			}
			out, err := e.NewDecoder().Bytes(data)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			return vm.ToValue(string(out))
		}
		str := call.Argument(2).String()
		if e == nil {
			return buffer.WrapBytes(vm, []byte(str))
		}
		out, err := e.NewEncoder().Bytes([]byte(str))
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return buffer.WrapBytes(vm, out)
	})
	_ = host.Set("log", func(call goja.FunctionCall) goja.Value {
		w.consoleOut(call.Argument(0).String(), call.Argument(1).String())
		return goja.Undefined()
	})
	_ = host.Set("inited", func(call goja.FunctionCall) goja.Value {
		if w.opts.OnInited != nil {
			w.opts.OnInited(call.Argument(0).String())
		}
		return goja.Undefined()
	})
	_ = host.Set("updateAlert", func(call goja.FunctionCall) goja.Value {
		if w.opts.OnUpdateAlert != nil {
			w.opts.OnUpdateAlert(call.Argument(0).String())
		}
		return goja.Undefined()
	})
	vm.Set("__host", host)
}

func newHash(alg string) hash.Hash {
	switch strings.ToLower(strings.ReplaceAll(alg, "-", "")) {
	case "md5":
		return md5.New()
	case "sha1":
		return sha1.New()
	case "sha256":
		return sha256.New()
	case "sha512":
		return sha512.New()
	}
	return nil
}

func pkcs7Pad(b []byte, size int) []byte {
	n := size - len(b)%size
	return append(b, bytes.Repeat([]byte{byte(n)}, n)...)
}

func pkcs7Unpad(b []byte, size int) ([]byte, error) {
	if len(b) == 0 || len(b)%size != 0 {
		return nil, errors.New("bad padded data length")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > size || n > len(b) {
		return nil, errors.New("bad padding")
	}
	return b[:len(b)-n], nil
}

// aesCrypt 支持 aes-{128,192,256}-{cbc,ecb,ctr,cfb,ofb}
func aesCrypt(decrypt bool, mode string, key, iv, data []byte, autoPad bool) ([]byte, error) {
	parts := strings.Split(strings.ToLower(mode), "-")
	if len(parts) != 3 || parts[0] != "aes" {
		return nil, fmt.Errorf("unsupported cipher: %s", mode)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	switch parts[2] {
	case "ecb":
		if decrypt {
			if len(data)%bs != 0 {
				return nil, errors.New("input not full blocks")
			}
			out := make([]byte, len(data))
			for i := 0; i < len(data); i += bs {
				block.Decrypt(out[i:i+bs], data[i:i+bs])
			}
			if autoPad {
				return pkcs7Unpad(out, bs)
			}
			return out, nil
		}
		if autoPad {
			data = pkcs7Pad(append([]byte(nil), data...), bs)
		}
		out := make([]byte, len(data))
		for i := 0; i < len(data); i += bs {
			block.Encrypt(out[i:i+bs], data[i:i+bs])
		}
		return out, nil
	case "cbc":
		if len(iv) != bs {
			return nil, errors.New("invalid iv length")
		}
		if decrypt {
			if len(data)%bs != 0 {
				return nil, errors.New("input not full blocks")
			}
			out := make([]byte, len(data))
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
			if autoPad {
				return pkcs7Unpad(out, bs)
			}
			return out, nil
		}
		if autoPad {
			data = pkcs7Pad(append([]byte(nil), data...), bs)
		}
		out := make([]byte, len(data))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, data)
		return out, nil
	case "ctr", "cfb", "ofb":
		if len(iv) != bs {
			return nil, errors.New("invalid iv length")
		}
		var s cipher.Stream
		switch parts[2] {
		case "ctr":
			s = cipher.NewCTR(block, iv)
		case "cfb":
			if decrypt {
				s = cipher.NewCFBDecrypter(block, iv)
			} else {
				s = cipher.NewCFBEncrypter(block, iv)
			}
		default:
			s = cipher.NewOFB(block, iv)
		}
		out := make([]byte, len(data))
		s.XORKeyStream(out, data)
		return out, nil
	}
	return nil, fmt.Errorf("unsupported cipher mode: %s", mode)
}

func parseRSAPublicKey(keyPEM string) (*rsa.PublicKey, error) {
	blk, _ := pem.Decode([]byte(keyPEM))
	if blk == nil {
		return nil, errors.New("invalid PEM public key")
	}
	if k, err := x509.ParsePKIXPublicKey(blk.Bytes); err == nil {
		if r, ok := k.(*rsa.PublicKey); ok {
			return r, nil
		}
		return nil, errors.New("not an RSA public key")
	}
	if k, err := x509.ParsePKCS1PublicKey(blk.Bytes); err == nil {
		return k, nil
	}
	return nil, errors.New("cannot parse RSA public key")
}

func rsaPublicEncrypt(keyPEM string, data []byte, padding int) ([]byte, error) {
	pub, err := parseRSAPublicKey(keyPEM)
	if err != nil {
		return nil, err
	}
	switch padding {
	case 3: // RSA_NO_PADDING：裸模幂
		k := (pub.N.BitLen() + 7) / 8
		if len(data) > k {
			return nil, errors.New("data too large for key")
		}
		m := new(big.Int).SetBytes(data)
		c := new(big.Int).Exp(m, big.NewInt(int64(pub.E)), pub.N)
		out := make([]byte, k)
		c.FillBytes(out)
		return out, nil
	case 4:
		return rsa.EncryptOAEP(sha1.New(), rand.Reader, pub, data, nil)
	default:
		return rsa.EncryptPKCS1v15(rand.Reader, pub, data)
	}
}

func zlibOp(op string, data []byte) ([]byte, error) {
	var buf bytes.Buffer
	switch op {
	case "inflate", "unzip":
		r, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			if op == "unzip" {
				return zlibOp("gunzip", data)
			}
			return nil, err
		}
		defer r.Close()
		_, err = io.Copy(&buf, r)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, err
		}
		return buf.Bytes(), nil
	case "deflate":
		wr := zlib.NewWriter(&buf)
		wr.Write(data)
		wr.Close()
		return buf.Bytes(), nil
	case "inflateRaw":
		r := flate.NewReader(bytes.NewReader(data))
		defer r.Close()
		if _, err := io.Copy(&buf, r); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, err
		}
		return buf.Bytes(), nil
	case "deflateRaw":
		wr, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		wr.Write(data)
		wr.Close()
		return buf.Bytes(), nil
	case "gunzip":
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		if _, err := io.Copy(&buf, r); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, err
		}
		return buf.Bytes(), nil
	case "gzip":
		wr := gzip.NewWriter(&buf)
		wr.Write(data)
		wr.Close()
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("unsupported zlib op: %s", op)
}

func lookupEncoding(name string) encoding.Encoding {
	switch strings.ToLower(strings.ReplaceAll(name, "_", "-")) {
	case "gb18030":
		return simplifiedchinese.GB18030
	case "gbk", "cp936", "gb2312":
		return simplifiedchinese.GBK
	case "big5":
		return traditionalchinese.Big5
	case "latin1", "iso-8859-1", "binary":
		return charmap.ISO8859_1
	case "utf16le", "utf-16le", "ucs2", "ucs-2":
		return unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)
	case "utf16be", "utf-16be":
		return unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM)
	}
	return nil
}

// hostFetch 实现 __host.fetch(opts, cb)：在 goroutine 中发起请求，完成后回到事件循环调用 cb
func hostFetch(w *Worker, vm *goja.Runtime, call goja.FunctionCall) goja.Value {
	optsObj := call.Argument(0).ToObject(vm)
	cb, ok := goja.AssertFunction(call.Argument(1))
	if !ok {
		panic(vm.NewTypeError("fetch callback required"))
	}
	getStr := func(k string) string {
		v := optsObj.Get(k)
		if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
			return ""
		}
		return v.String()
	}
	rawURL := getStr("url")
	method := getStr("method")
	if method == "" {
		method = "GET"
	}
	headers := map[string]string{}
	if hv := optsObj.Get("headers"); hv != nil && !goja.IsUndefined(hv) && !goja.IsNull(hv) {
		ho := hv.ToObject(vm)
		for _, k := range ho.Keys() {
			v := ho.Get(k)
			if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
				continue
			}
			headers[k] = v.String()
		}
	}
	var body []byte
	if bv := optsObj.Get("body"); bv != nil && !goja.IsUndefined(bv) && !goja.IsNull(bv) {
		if bv.ExportType() != nil && bv.ExportType().Kind().String() == "string" {
			body = []byte(bv.String())
		} else {
			body = buffer.Bytes(vm, bv)
		}
	}
	getInt := func(k string) int64 {
		v := optsObj.Get(k)
		if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
			return 0
		}
		return v.ToInteger()
	}
	getBool := func(k string) bool {
		v := optsObj.Get(k)
		if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
			return false
		}
		return v.ToBoolean()
	}
	timeout := time.Duration(getInt("timeout")) * time.Millisecond
	if timeout <= 0 || timeout > 60*time.Second {
		timeout = 60 * time.Second
	}
	follow := int(getInt("follow"))
	insecure := getBool("insecure")

	scope := w.calls.current
	parent := context.Background()
	if scope != nil {
		parent = scope.ctx
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	handle := vm.NewObject()
	_ = handle.Set("abort", func(goja.FunctionCall) goja.Value { cancel(); return goja.Undefined() })

	go func() {
		defer cancel()
		resp, data, err := w.doFetch(ctx, method, rawURL, headers, body, follow, insecure)
		w.run(func(vm *goja.Runtime) {
			if parent.Err() != nil {
				return
			}
			defer w.enterCall(scope)()
			if err != nil {
				eo := vm.NewObject()
				_ = eo.Set("message", err.Error())
				_ = eo.Set("code", errCode(err))
				_, _ = cb(goja.Undefined(), eo, goja.Null())
				return
			}
			ro := vm.NewObject()
			_ = ro.Set("statusCode", resp.StatusCode)
			_ = ro.Set("statusMessage", strings.TrimSpace(strings.TrimPrefix(resp.Status, fmt.Sprint(resp.StatusCode))))
			ho := vm.NewObject()
			for k, vals := range resp.Header {
				lk := strings.ToLower(k)
				if lk == "set-cookie" {
					_ = ho.Set(lk, vals)
				} else {
					_ = ho.Set(lk, strings.Join(vals, ", "))
				}
			}
			_ = ro.Set("headers", ho)
			_ = ro.Set("raw", buffer.WrapBytes(vm, data))
			_, _ = cb(goja.Undefined(), goja.Null(), ro)
		})
	}()
	return handle
}

func errCode(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "ETIMEDOUT"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "ETIMEDOUT"
	}
	if errors.Is(err, context.Canceled) {
		return "ECANCELED"
	}
	var de *net.DNSError
	if errors.As(err, &de) {
		return "ENOTFOUND"
	}
	return "ECONNRESET"
}

func (w *Worker) doFetch(ctx context.Context, method, rawURL string, headers map[string]string, body []byte, follow int, insecure bool) (*http.Response, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		if strings.EqualFold(k, "host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	}
	base := w.opts.HTTP
	if insecure && w.opts.HTTPInsecure != nil {
		base = w.opts.HTTPInsecure
	}
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > follow {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, w.opts.MaxBodyBytes))
	if err != nil {
		return nil, nil, err
	}
	return resp, data, nil
}
