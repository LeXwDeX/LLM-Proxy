package nosql

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"
)

// ErrTargetETagConflict 表示目标当前内容的 ETag 与调用方期望值不一致
// （目标已被他人修改、删除或不存在）。发生时库内容保持不变。
var ErrTargetETagConflict = errors.New("target etag conflict")

var (
	// keyIDSecretKey 是 meta 中的 32 字节随机密钥，用于派生稳定的 key ID。
	keyIDSecretKey = []byte("target_key_id_secret")
)

const keyIDSecretSize = 32

// ensureKeyIDSecret 在写事务中生成一次 key ID 密钥，已存在则不动，可重复执行。
func ensureKeyIDSecret(tx *bolt.Tx) ([]byte, error) {
	meta, err := tx.CreateBucketIfNotExists([]byte(BucketMeta))
	if err != nil {
		return nil, err
	}
	if existing := meta.Get(keyIDSecretKey); len(existing) == keyIDSecretSize {
		return append([]byte(nil), existing...), nil
	}
	secret := make([]byte, keyIDSecretSize)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate key id secret: %w", err)
	}
	if err := meta.Put(keyIDSecretKey, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// KeyIDSecret 返回稳定 key ID 的派生密钥；它随数据库持久化，重启后不变。
func (s *TargetStore) KeyIDSecret() ([]byte, error) {
	var secret []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		if meta := tx.Bucket([]byte(BucketMeta)); meta != nil {
			if v := meta.Get(keyIDSecretKey); len(v) == keyIDSecretSize {
				secret = append([]byte(nil), v...)
			}
		}
		return nil
	})
	if err != nil || secret != nil {
		return secret, err
	}
	// 不是经 OpenDB 打开的库：补建一次。
	err = s.db.Update(func(tx *bolt.Tx) error {
		var err error
		secret, err = ensureKeyIDSecret(tx)
		return err
	})
	return secret, err
}

const (
	targetETagPrefix = "t1-"
	targetETagBytes  = 16
)

func targetETagKey(name string) []byte {
	return []byte(strings.ToLower(strings.TrimSpace(name)))
}

// targetETagOf 对 targets bucket 中存储的原始 JSON 字节取 SHA-256 前 16 字节，
// base64url 无填充，带 t1- 前缀。ETag 只取决于存储内容：任何写入者（含旧二进制、
// 规范化回写、迁移）改变字节就改变 ETag，无需各写入点配合。
func targetETagOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return targetETagPrefix + base64.RawURLEncoding.EncodeToString(sum[:targetETagBytes])
}

// targetETagTx 读取目标当前 ETag；目标不存在返回空串。
func targetETagTx(tx *bolt.Tx, key []byte) string {
	b := tx.Bucket([]byte(BucketTargets))
	if b == nil {
		return ""
	}
	raw := b.Get(key)
	if raw == nil {
		return ""
	}
	return targetETagOf(raw)
}

// ETag 返回目标当前内容的 ETag；目标不存在返回空串。只读，不写库。
func (s *TargetStore) ETag(name string) (string, error) {
	var etag string
	err := s.db.View(func(tx *bolt.Tx) error {
		etag = targetETagTx(tx, targetETagKey(name))
		return nil
	})
	return etag, err
}

// ETags 在一次只读事务内返回全部目标的 ETag（键为小写名）。
func (s *TargetStore) ETags() (map[string]string, error) {
	out := make(map[string]string)
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			out[string(k)] = targetETagOf(v)
			return nil
		})
	})
	return out, err
}
