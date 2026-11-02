package nosql

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// legacyTargetJSON is the on-disk bbolt format used to detect and migrate
// old targets that stored allowed_models (now replaced by model_mappings).
// It embeds the current config.Target plus the legacy AllowedModels slice.
// Because Go's standard json.Unmarshal allows the same JSON key to populate
// both an embedded field and a shadowed sibling, AllowedModels is populated
// from the pre-migration "allowed_models" JSON array while ModelMappings is
// populated from the new "model_mappings" array. When AllowedModels is non-empty
// and ModelMappings is empty, the List() loop migrates the record.
type legacyTargetJSON struct {
	config.Target
	AllowedModels []string `json:"allowed_models"`
}

var targetOrderKey = []byte("target_order")

// TargetStore manages upstream targets backed by bbolt.
type TargetStore struct {
	db     *bolt.DB
	logger *slog.Logger
}

// NewTargetStore creates a new bbolt-backed target store.
func NewTargetStore(db *bolt.DB, loggers ...*slog.Logger) *TargetStore {
	logger := slog.Default()
	if len(loggers) > 0 && loggers[0] != nil {
		logger = loggers[0]
	}
	return &TargetStore{db: db, logger: logger}
}

// MigrateFromConfig copies legacy config targets into this store when empty.
func (s *TargetStore) MigrateFromConfig(targets []config.Target, loggers ...*slog.Logger) (bool, error) {
	logger := s.logger
	if len(loggers) > 0 && loggers[0] != nil {
		logger = loggers[0]
	}
	return MigrateTargetsFromConfig(s.db, targets, logger)
}

// MigrateUsageAdapterExplicit converts persisted legacy selections before load.
func (s *TargetStore) MigrateUsageAdapterExplicit(logger *slog.Logger) error {
	return MigrateUsageAdapterExplicit(s.db, logger)
}

// List returns all targets in configured order.
func (s *TargetStore) List() ([]config.Target, error) {
	targetsByKey := make(map[string]config.Target)
	var keys []string
	var order []string
	var changed []targetWriteback

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		if b == nil {
			return nil
		}
		if meta := tx.Bucket([]byte(BucketMeta)); meta != nil {
			_ = json.Unmarshal(meta.Get(targetOrderKey), &order)
		}
		return b.ForEach(func(k, v []byte) error {
			key := string(k)
			// The explicit field shadows Target.CustomBody for this decode, so the
			// custom body arrives as raw JSON text instead of float64 values.
			var record struct {
				config.Target
				RawCustomBody map[string]json.RawMessage `json:"custom_body"`
			}
			if err := json.Unmarshal(v, &record); err != nil {
				return fmt.Errorf("decode target %q: %w", key, err)
			}
			target := record.Target
			if !config.IsValidUsageAdapter(target.UsageAdapter) {
				// Do not infer by host or fall back to standard on residual data.
				s.logger.Error("skipping target with invalid usage_adapter", "target", target.Name, "target_key", key)
				return nil
			}
			// Migrate legacy allowed_models → model_mappings when the old field
			// is observed in raw bytes but ModelMappings is empty. The legacy
			// struct is embedded so json.Unmarshal reads AllowedModels alongside
			// the new ModelMappings field; we only act when ModelMappings is empty.
			var legacy legacyTargetJSON
			migrated := false
			if err := json.Unmarshal(v, &legacy); err == nil && len(legacy.AllowedModels) > 0 && len(legacy.Target.ModelMappings) == 0 {
				target = legacy.Target
				target.ModelMappings = make([]config.ModelMapping, 0, len(legacy.AllowedModels))
				for _, m := range legacy.AllowedModels {
					m = strings.TrimSpace(m)
					if m == "" {
						continue
					}
					target.ModelMappings = append(target.ModelMappings, config.ModelMapping{Upstream: m})
				}
				migrated = true
			}
			if record.RawCustomBody != nil {
				target.CustomBody = config.CustomBodyFromRaw(record.RawCustomBody)
			}
			normalized := normalizeTarget(target)
			if migrated || targetJSONChanged(target, normalized) {
				changed = append(changed, targetWriteback{
					key:        key,
					observed:   append([]byte(nil), v...),
					normalized: normalized,
				})
			}
			targetsByKey[key] = normalized
			keys = append(keys, key)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if len(changed) > 0 {
		if err := s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(BucketTargets))
			for _, write := range changed {
				if err := putTargetIfUnchanged(b, write); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	seen := make(map[string]struct{}, len(targetsByKey))
	targets := make([]config.Target, 0, len(targetsByKey))
	for _, key := range order {
		key = strings.ToLower(strings.TrimSpace(key))
		target, ok := targetsByKey[key]
		if !ok {
			continue
		}
		targets = append(targets, target)
		seen[key] = struct{}{}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		targets = append(targets, targetsByKey[key])
	}
	return cloneTargets(targets), nil
}

// Create appends a new target.
func (s *TargetStore) Create(target config.Target) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	target = normalizeTarget(target)
	key := strings.ToLower(target.Name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		if b.Get([]byte(key)) != nil {
			return fmt.Errorf("target %q already exists", target.Name)
		}
		if err := putTarget(b, key, target); err != nil {
			return err
		}
		return updateTargetOrder(tx, func(order []string) []string {
			return appendMissingTargetOrder(order, key)
		})
	})
}

// Update updates one target by name.
func (s *TargetStore) Update(name string, target config.Target) error {
	_, err := s.update(name, target, nil)
	return err
}

// UpdateIfMatch 仅当目标当前存储字节的 ETag 等于 etag 时更新（同一写事务内重新计算并比较），
// 返回新 ETag；不匹配或目标不存在返回 ErrTargetETagConflict 且库内容不变。
// 有意冗余：admin handler 的预检（resolveIfMatch）只用于快速失败，最终裁决在此写事务内完成。
func (s *TargetStore) UpdateIfMatch(name string, target config.Target, etag string) (string, error) {
	return s.update(name, target, &etag)
}

func (s *TargetStore) update(name string, target config.Target, expected *string) (string, error) {
	if err := validateTarget(target); err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name must not be empty")
	}
	oldKey := strings.ToLower(name)
	next := normalizeTarget(target)
	newKey := strings.ToLower(next.Name)

	var newETag string
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		if b.Get([]byte(oldKey)) == nil {
			if expected != nil {
				return ErrTargetETagConflict
			}
			return fmt.Errorf("target %q not found", name)
		}
		if expected != nil && targetETagTx(tx, []byte(oldKey)) != *expected {
			return ErrTargetETagConflict
		}
		if newKey != oldKey && b.Get([]byte(newKey)) != nil {
			return fmt.Errorf("target %q already exists", next.Name)
		}
		if newKey != oldKey {
			if err := b.Delete([]byte(oldKey)); err != nil {
				return err
			}
		}
		if err := putTarget(b, newKey, next); err != nil {
			return err
		}
		newETag = targetETagOf(b.Get([]byte(newKey)))
		return updateTargetOrder(tx, func(order []string) []string {
			replaced := false
			for i, key := range order {
				if key == oldKey {
					order[i] = newKey
					replaced = true
				}
			}
			if !replaced {
				order = append(order, newKey)
			}
			return dedupeTargetOrder(order)
		})
	})
	if err != nil {
		return "", err
	}
	return newETag, nil
}

// Delete removes one target by name.
func (s *TargetStore) Delete(name string) error {
	return s.deleteTarget(name, nil)
}

// DeleteIfMatch 仅当目标当前存储字节的 ETag 等于 etag 时删除；不匹配或不存在返回 ErrTargetETagConflict。
func (s *TargetStore) DeleteIfMatch(name string, etag string) error {
	return s.deleteTarget(name, &etag)
}

func (s *TargetStore) deleteTarget(name string, expected *string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name must not be empty")
	}
	key := strings.ToLower(name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		if b.Get([]byte(key)) == nil {
			if expected != nil {
				return ErrTargetETagConflict
			}
			return fmt.Errorf("target %q not found", name)
		}
		if expected != nil && targetETagTx(tx, []byte(key)) != *expected {
			return ErrTargetETagConflict
		}
		if err := b.Delete([]byte(key)); err != nil {
			return err
		}
		return updateTargetOrder(tx, func(order []string) []string {
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

// MigrateTargetsFromConfig copies legacy config.json targets into bbolt when
// the target bucket is empty. It returns true when targets were copied.
func MigrateTargetsFromConfig(db *bolt.DB, targets []config.Target, loggers ...*slog.Logger) (bool, error) {
	if len(targets) == 0 {
		return false, nil
	}
	// Admin reads call this on every request while config.json still lists
	// targets. Skip the write transaction once bbolt owns the targets.
	var populated bool
	if err := db.View(func(tx *bolt.Tx) error {
		k, _ := tx.Bucket([]byte(BucketTargets)).Cursor().First()
		populated = k != nil
		return nil
	}); err != nil || populated {
		return false, err
	}

	migrated := false
	var writes []usageAdapterMigrationWrite
	err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		k, _ := b.Cursor().First()
		if k != nil {
			return nil
		}
		// Validate only when importing: stale file targets must not prevent
		// startup when bbolt already owns the target configuration.
		seen := make(map[string]struct{}, len(targets))
		order := make([]string, 0, len(targets))
		for _, target := range targets {
			originalValue, present := target.UsageAdapterImportInput()
			adapter := config.NormalizeUsageAdapter(target.UsageAdapter)
			if adapter == "" || adapter == config.UsageAdapterAuto {
				explicit, err := config.LegacyUsageAdapterForEndpoint(target.Endpoint)
				if err != nil {
					return fmt.Errorf("target %q: %w", target.Name, err)
				}
				target.UsageAdapter = explicit
			}
			if err := validateTarget(target); err != nil {
				return err
			}
			target = normalizeTarget(target)
			key := strings.ToLower(target.Name)
			if _, ok := seen[key]; ok {
				return fmt.Errorf("target %q already exists", target.Name)
			}
			seen[key] = struct{}{}
			// Stage the original legacy field inside this same transaction so
			// the shared migration records missing/empty/auto accurately. The
			// staged legacy targets can never be observed outside the transaction.
			if adapter == "" || adapter == config.UsageAdapterAuto {
				target.UsageAdapter = originalValue
			}
			if err := putImportedTarget(b, key, target, present); err != nil {
				return err
			}
			order = append(order, key)
		}
		if err := writeTargetOrder(tx, order); err != nil {
			return err
		}
		var err error
		writes, err = migrateUsageAdapterTx(tx, time.Now().UTC())
		if err != nil {
			return err
		}
		migrated = true
		return nil
	})
	if err != nil {
		return false, err
	}
	logger := slog.Default()
	if len(loggers) > 0 && loggers[0] != nil {
		logger = loggers[0]
	}
	logUsageAdapterMigrations(logger, writes)
	return migrated, nil
}

func putImportedTarget(b *bolt.Bucket, key string, target config.Target, adapterPresent bool) error {
	data, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("encode target: %w", err)
	}
	if adapterPresent && target.UsageAdapter == "" {
		// The public Target JSON tag omits empty strings. Retain an explicitly
		// empty input field until migration has recorded its original state.
		data = append(data[:len(data)-1], []byte(`,"usage_adapter":""}`)...)
	}
	return b.Put([]byte(key), data)
}

func putTarget(b *bolt.Bucket, key string, target config.Target) error {
	data, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("encode target: %w", err)
	}
	return b.Put([]byte(key), data)
}

func validateTarget(target config.Target) error {
	if err := config.ValidateModelPrefix(target.ModelPrefix); err != nil {
		return err
	}
	if !config.IsValidUsageAdapter(target.UsageAdapter) {
		return errors.New("invalid usage_adapter")
	}
	name := strings.TrimSpace(target.Name)
	if name == "" {
		return errors.New("name must not be empty")
	}
	epType := config.NormalizeEndpointType(target.EndpointType)
	if !config.IsValidEndpointType(epType) {
		return errors.New("invalid endpoint_type")
	}
	if strings.TrimSpace(target.Endpoint) == "" {
		return errors.New("endpoint must not be empty")
	}
	hasAnyKey := strings.TrimSpace(target.APIKey) != ""
	for _, key := range target.APIKeys {
		if strings.TrimSpace(key) != "" {
			hasAnyKey = true
			break
		}
	}
	if !hasAnyKey && !target.AllowBearer {
		return errors.New("api_key must not be empty when allow_bearer_passthrough is false")
	}
	return nil
}

func normalizeTarget(target config.Target) config.Target {
	target = config.NormalizeTargetForCompatibility(target)
	target.Name = strings.TrimSpace(target.Name)
	target.EndpointType = config.NormalizeEndpointType(target.EndpointType)
	target.UsageAdapter = config.NormalizeUsageAdapter(target.UsageAdapter)
	target.Endpoint = strings.TrimSpace(target.Endpoint)
	target.APIKey = strings.TrimSpace(target.APIKey)
	target.KeyResetTime = strings.TrimSpace(target.KeyResetTime)
	target.ProviderClass = strings.TrimSpace(target.ProviderClass)
	target.AuthMode = strings.TrimSpace(target.AuthMode)
	target.ImageOperation = config.NormalizeImageOperation(target.ImageOperation)
	// 部署池成员归属：与池名同一规范化口径（小写 + 去空白）。
	target.Pool = strings.ToLower(strings.TrimSpace(target.Pool))
	// 消费分组成员归属：与组名同一规范化口径（小写 + 去空白）。
	if target.MaxConcurrency < 0 {
		target.MaxConcurrency = 0
	}
	for i := range target.APIKeys {
		target.APIKeys[i] = strings.TrimSpace(target.APIKeys[i])
	}
	for i := range target.ModelMappings {
		target.ModelMappings[i].Upstream = strings.TrimSpace(target.ModelMappings[i].Upstream)
		target.ModelMappings[i].Fallback = strings.TrimSpace(target.ModelMappings[i].Fallback)
	}
	rpp := strings.TrimSpace(target.ResourcePathPrefix)
	if rpp != "" && rpp != "/" {
		endpoint := strings.TrimRight(target.Endpoint, "/")
		if !strings.HasSuffix(endpoint, rpp) {
			target.Endpoint = endpoint + rpp
		}
	}
	target.ResourcePathPrefix = ""
	return target
}

func targetJSONChanged(a, b config.Target) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return true
	}
	return string(ab) != string(bb)
}

type targetWriteback struct {
	key        string
	observed   []byte
	normalized config.Target
}

func putTargetIfUnchanged(b *bolt.Bucket, write targetWriteback) error {
	if b == nil {
		return nil
	}
	if !bytes.Equal(b.Get([]byte(write.key)), write.observed) {
		return nil
	}
	return putTarget(b, write.key, write.normalized)
}

func updateTargetOrder(tx *bolt.Tx, update func([]string) []string) error {
	var order []string
	if meta := tx.Bucket([]byte(BucketMeta)); meta != nil {
		_ = json.Unmarshal(meta.Get(targetOrderKey), &order)
	}
	return writeTargetOrder(tx, update(order))
}

func writeTargetOrder(tx *bolt.Tx, order []string) error {
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil {
		return nil
	}
	data, err := json.Marshal(dedupeTargetOrder(order))
	if err != nil {
		return err
	}
	return meta.Put(targetOrderKey, data)
}

func appendMissingTargetOrder(order []string, key string) []string {
	for _, existing := range order {
		if existing == key {
			return order
		}
	}
	return append(order, key)
}

func dedupeTargetOrder(order []string) []string {
	seen := make(map[string]struct{}, len(order))
	next := make([]string, 0, len(order))
	for _, key := range order {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		next = append(next, key)
	}
	return next
}

func cloneTargets(targets []config.Target) []config.Target {
	if targets == nil {
		return nil
	}
	cloned := make([]config.Target, len(targets))
	for i := range targets {
		cloned[i] = targets[i]
		if len(targets[i].APIKeys) > 0 {
			cloned[i].APIKeys = append([]string(nil), targets[i].APIKeys...)
		}
		if len(targets[i].ModelMappings) > 0 {
			cloned[i].ModelMappings = append([]config.ModelMapping(nil), targets[i].ModelMappings...)
		}
	}
	return cloned
}
