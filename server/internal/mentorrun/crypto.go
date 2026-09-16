package mentorrun

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// 密钥由操作者独立挂载；不自动生成丢失密钥，不将密钥或正文写入错误。
func LoadKey(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrStorage
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != 32 {
		return nil, ErrStorage
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrStorage
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrStorage
	}
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(key) != 32 {
		return nil, ErrStorage
	}
	return key, nil
}

func newCipher(key []byte) (cipher.AEAD, error) {
	if len(key) == 0 {
		return nil, nil
	}
	if len(key) != 32 {
		return nil, ErrStorage
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrStorage
	}
	return cipher.NewGCM(block)
}

func seal(a cipher.AEAD, id string, body Body) ([]byte, error) {
	if a == nil {
		return nil, ErrStorage
	}
	raw, err := json.Marshal(body)
	if err != nil || len(raw) > MaxBody {
		return nil, ErrLimit
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, ErrStorage
	}
	return a.Seal(nonce, nonce, raw, []byte(id)), nil
}

func unseal(a cipher.AEAD, id string, raw []byte) (Body, error) {
	var body Body
	if a == nil || len(raw) < a.NonceSize() || len(raw) > MaxBody+64 {
		return body, ErrStorage
	}
	plain, err := a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], []byte(id))
	if err != nil || json.Unmarshal(plain, &body) != nil {
		return body, ErrStorage
	}
	return body, nil
}
