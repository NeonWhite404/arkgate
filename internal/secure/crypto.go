// Package secure 提供本地加密能力：API Key 加/解密。
//
// 采用 AES-256-GCM。主密钥持久化到 DataDir/secret.key（0600），
// 首次运行自动生成 32 字节随机密钥。也可通过 ARKGATE_SECRET 显式指定。
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrNotConfigured 在未提供密钥且无法生成时返回。
	ErrNotConfigured = errors.New("secure: no key available")
)

type Box struct {
	aead cipher.AEAD
}

// New 基于 dataDir 构造加解密器。
func New(dataDir string) (*Box, error) {
	key, err := loadOrCreateKey(dataDir)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Encrypt 加密纯文本，返回 base64 字符串。
func (b *Box) Encrypt(plain string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.RawStdEncoding.EncodeToString(ct), nil
}

// Decrypt 解密 base64 字符串，返回纯文本。
func (b *Box) Decrypt(enc string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("secure: ciphertext too short")
	}
	pt, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// Mask 生成长 Key 的展示提示（保留末 4 位）。
func Mask(key string) string {
	if len(key) <= 4 {
		return "...."
	}
	return "...." + key[len(key)-4:]
}

func loadOrCreateKey(dataDir string) ([]byte, error) {
	// 1. 环境变量显式密钥。
	if env := strings.TrimSpace(os.Getenv("ARKGATE_SECRET")); env != "" {
		return deriveKey([]byte(env)), nil
	}
	// 2. 磁盘上的 secret.key。
	keyPath := filepath.Join(dataDir, "secret.key")
	if b, err := os.ReadFile(keyPath); err == nil && len(b) >= 16 {
		// 兼容：直接存 32 字节原始密钥。
		if len(b) == 32 {
			return b, nil
		}
		return deriveKey(b), nil
	}
	// 3. 生成并落盘。
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func deriveKey(seed []byte) []byte {
	h := sha256.Sum256(seed)
	return h[:]
}
