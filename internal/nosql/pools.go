package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

var poolOrderKey = []byte("pool_order")

// PoolStore manages deployment pools backed by bbolt（镜像 targets.go 模式）。
type PoolStore struct {
	db *bolt.DB
}

// NewPoolStore creates a new bbolt-backed pool store.
func NewPoolStore(db *bolt.DB) *PoolStore {
	return &PoolStore{db: db}
}

// Pools returns a PoolStore view sharing the same underlying db as the
// TargetStore. admin 注入点：NewHandler 无需签名变更即可获得池存储。
func (s *TargetStore) Pools() *PoolStore {
	return &PoolStore{db: s.db}
}

// MigrateFromConfig copies legacy config pools into this store when empty.
func (s *PoolStore) MigrateFromConfig(pools []config.DeploymentPool) (bool, error) {
	return MigratePoolsFromConfig(s.db, pools)
}

// List returns all pools in configured order.
func (s *PoolStore) List() ([]config.DeploymentPool, error) {
	poolsByKey := make(map[string]config.DeploymentPool)
	var keys []string
	var order []string

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPools))
		if b == nil {
			return nil
		}
		if meta := tx.Bucket([]byte(BucketMeta)); meta != nil {
			_ = json.Unmarshal(meta.Get(poolOrderKey), &order)
		}
		return b.ForEach(func(k, v []byte) error {
			key := string(k)
			var pool config.DeploymentPool
			if err := json.Unmarshal(v, &pool); err != nil {
				return fmt.Errorf("decode pool %q: %w", key, err)
			}
			poolsByKey[key] = normalizePool(pool)
			keys = append(keys, key)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(poolsByKey))
	pools := make([]config.DeploymentPool, 0, len(poolsByKey))
	for _, key := range order {
		key = strings.ToLower(strings.TrimSpace(key))
		pool, ok := poolsByKey[key]
		if !ok {
			continue
		}
		pools = append(pools, pool)
		seen[key] = struct{}{}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		pools = append(pools, poolsByKey[key])
	}
	return clonePools(pools), nil
}

// Create appends a new pool.
func (s *PoolStore) Create(pool config.DeploymentPool) error {
	if err := validatePool(pool); err != nil {
		return err
	}
	pool = normalizePool(pool)
	key := pool.Name

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPools))
		if b.Get([]byte(key)) != nil {
			return fmt.Errorf("pool %q already exists", pool.Name)
		}
		if err := putPool(b, key, pool); err != nil {
			return err
		}
		return updatePoolOrder(tx, func(order []string) []string {
			return appendMissingPoolOrder(order, key)
		})
	})
}

// Update updates one pool by name. 池名不可变（防止成员悬空）。
func (s *PoolStore) Update(name string, pool config.DeploymentPool) error {
	if err := validatePool(pool); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("pool name must not be empty")
	}
	pool = normalizePool(pool)
	if pool.Name != strings.ToLower(name) {
		return fmt.Errorf("pool name cannot be changed (path %q, body %q)", name, pool.Name)
	}
	key := pool.Name

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPools))
		if b.Get([]byte(key)) == nil {
			return fmt.Errorf("pool %q not found", name)
		}
		if err := putPool(b, key, pool); err != nil {
			return err
		}
		return updatePoolOrder(tx, func(order []string) []string {
			return appendMissingPoolOrder(order, key)
		})
	})
}

// Delete removes one pool by name.
func (s *PoolStore) Delete(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("pool name must not be empty")
	}
	key := strings.ToLower(name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPools))
		if b.Get([]byte(key)) == nil {
			return fmt.Errorf("pool %q not found", name)
		}
		if err := b.Delete([]byte(key)); err != nil {
			return err
		}
		return updatePoolOrder(tx, func(order []string) []string {
			next := order[:0]
			for _, existing := range order {
				if existing != key {
					next = append(next, existing)
				}
			}
			return next
		})
	})
}

// MigratePoolsFromConfig copies legacy config.json pools into bbolt when
// the pool bucket is empty. It returns true when pools were copied.
func MigratePoolsFromConfig(db *bolt.DB, pools []config.DeploymentPool) (bool, error) {
	if len(pools) == 0 {
		return false, nil
	}

	normalized := make([]config.DeploymentPool, 0, len(pools))
	seen := make(map[string]struct{}, len(pools))
	for _, pool := range pools {
		if err := validatePool(pool); err != nil {
			return false, err
		}
		pool = normalizePool(pool)
		if _, ok := seen[pool.Name]; ok {
			return false, fmt.Errorf("pool %q already exists", pool.Name)
		}
		seen[pool.Name] = struct{}{}
		normalized = append(normalized, pool)
	}

	migrated := false
	err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPools))
		k, _ := b.Cursor().First()
		if k != nil {
			return nil
		}
		order := make([]string, 0, len(normalized))
		for _, pool := range normalized {
			if err := putPool(b, pool.Name, pool); err != nil {
				return err
			}
			order = append(order, pool.Name)
		}
		if err := writePoolOrder(tx, order); err != nil {
			return err
		}
		migrated = true
		return nil
	})
	return migrated, err
}

func putPool(b *bolt.Bucket, key string, pool config.DeploymentPool) error {
	data, err := json.Marshal(pool)
	if err != nil {
		return fmt.Errorf("encode pool: %w", err)
	}
	return b.Put([]byte(key), data)
}

func validatePool(pool config.DeploymentPool) error {
	if strings.TrimSpace(pool.Name) == "" {
		return errors.New("pool name must not be empty")
	}
	if display := strings.TrimSpace(pool.DisplayName); display != "" && strings.ToLower(display) != strings.ToLower(strings.TrimSpace(pool.Name)) {
		return errors.New("pool display name may only change letter casing")
	}
	return nil
}

func normalizePool(pool config.DeploymentPool) config.DeploymentPool {
	return config.NormalizeDeploymentPool(pool)
}

func updatePoolOrder(tx *bolt.Tx, update func([]string) []string) error {
	var order []string
	if meta := tx.Bucket([]byte(BucketMeta)); meta != nil {
		_ = json.Unmarshal(meta.Get(poolOrderKey), &order)
	}
	return writePoolOrder(tx, update(order))
}

func writePoolOrder(tx *bolt.Tx, order []string) error {
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil {
		return nil
	}
	data, err := json.Marshal(dedupeTargetOrder(order))
	if err != nil {
		return err
	}
	return meta.Put(poolOrderKey, data)
}

func appendMissingPoolOrder(order []string, key string) []string {
	for _, existing := range order {
		if existing == key {
			return order
		}
	}
	return append(order, key)
}

func clonePools(pools []config.DeploymentPool) []config.DeploymentPool {
	if pools == nil {
		return nil
	}
	return append([]config.DeploymentPool(nil), pools...)
}
