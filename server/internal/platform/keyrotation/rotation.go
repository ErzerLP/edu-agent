// Package keyrotation 只在停机维护事务内重封装密文，不写出明文或替换密钥文件。
package keyrotation

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"

	"github.com/jackc/pgx/v5"
)

type Rewrap func(aad string, ciphertext []byte) ([]byte, error)

func New(oldKey, newKey []byte) (Rewrap, error) {
	if len(oldKey) != 32 || len(newKey) != 32 {
		return nil, errors.New("轮换需要两个独立的 32 字节密钥")
	}
	oldBlock, err := aes.NewCipher(oldKey)
	if err != nil {
		return nil, err
	}
	newBlock, err := aes.NewCipher(newKey)
	if err != nil {
		return nil, err
	}
	oldAEAD, err := cipher.NewGCM(oldBlock)
	if err != nil {
		return nil, err
	}
	newAEAD, err := cipher.NewGCM(newBlock)
	if err != nil {
		return nil, err
	}
	return func(aad string, raw []byte) ([]byte, error) {
		if len(raw) < oldAEAD.NonceSize() {
			return nil, errors.New("密文不完整，轮换已回滚")
		}
		plain, err := oldAEAD.Open(nil, raw[:oldAEAD.NonceSize()], raw[oldAEAD.NonceSize():], []byte(aad))
		if err != nil {
			return nil, errors.New("旧密钥或认证密文无效，轮换已回滚")
		}
		defer clear(plain)
		nonce := make([]byte, newAEAD.NonceSize())
		if _, err = rand.Read(nonce); err != nil {
			return nil, err
		}
		return newAEAD.Seal(nonce, nonce, plain, []byte(aad)), nil
	}, nil
}

// Rewrite 的查询由各 owner 提供，返回稳定唯一键、AAD、密文；按键分批，最多驻留 16 条。
func Rewrite(ctx context.Context, tx pgx.Tx, query, update string, rewrap Rewrap) error {
	cursor := ""
	for {
		rows, err := tx.Query(ctx, query, cursor)
		if err != nil {
			return err
		}
		type record struct {
			key, aad string
			raw      []byte
		}
		batch := []record{}
		for rows.Next() {
			var r record
			if err = rows.Scan(&r.key, &r.aad, &r.raw); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, r := range batch {
			raw, e := rewrap(r.aad, r.raw)
			if e != nil {
				return e
			}
			if _, err = tx.Exec(ctx, update, r.key, raw); err != nil {
				return err
			}
			cursor = r.key
		}
	}
}
