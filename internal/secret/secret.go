// Package secret 提供口令的对称加密（AES-GCM），密钥持久化在数据目录
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
)

// Box 加解密器
type Box struct {
	aead cipher.AEAD
	key  string
}

// Load 使用给定密钥；为空时读取/生成 dataDir/secret.key
func Load(dataDir, key string) (*Box, error) {
	if key == "" {
		if dataDir == "" {
			return nil, errors.New("未提供密钥或密钥目录")
		}
		p := filepath.Join(dataDir, "secret.key")
		b, err := os.ReadFile(p)
		if err != nil {
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return nil, err
			}
			key = base64.StdEncoding.EncodeToString(raw)
			if err := os.WriteFile(p, []byte(key), 0o600); err != nil {
				return nil, err
			}
		} else {
			key = string(b)
		}
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, key: key}, nil
}

// ExportKey 返回迁移备份使用的有效密钥。调用方不得记录或直接暴露该值。
func (b *Box) ExportKey() string { return b.key }

// Encrypt 加密为 base64
func (b *Box) Encrypt(plain string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt 解密
func (b *Box) Decrypt(enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	plain, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// RandomToken 随机 token（hex 风格 base64url）
func RandomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
