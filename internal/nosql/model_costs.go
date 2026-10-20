package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"

	bolt "go.etcd.io/bbolt"
)

// ModelCost defines token pricing (per 1M tokens).
//
// Target 为空 = 全局默认层（阶梯最底层，兼容存量裸 model 键数据）；
// 非空 = per-target 精确价（阶梯中优先于全局层）。支持 "*" 通配的是
// 计费模板规则，本表拒绝 target="*"（validateCost）。
type ModelCost struct {
	Target                      string   `json:"target,omitempty"`        // 空 = 全局默认层；非空 = 目标名（小写）
	EndpointType                string   `json:"endpoint_type,omitempty"` // deprecated, not used as key
	Model                       string   `json:"model"`
	InputPer1MTokens            float64  `json:"input_per_1m_tokens"`
	OutputPer1MTokens           float64  `json:"output_per_1m_tokens"`
	CacheWritePer1MToken        float64  `json:"cache_write_per_1m_tokens"`
	CacheReadPer1MToken         float64  `json:"cache_read_per_1m_tokens"`
	ExplicitCacheReadPer1MToken *float64 `json:"explicit_cache_read_per_1m_tokens,omitempty"`
	CacheWrite5mPer1MToken      *float64 `json:"cache_write_5m_per_1m_tokens,omitempty"`
	CacheWrite1hPer1MToken      *float64 `json:"cache_write_1h_per_1m_tokens,omitempty"`
	ImageInputPer1MTokens       *float64 `json:"image_input_per_1m_tokens,omitempty"`
	CachedImageInputPer1MTokens *float64 `json:"cached_image_input_per_1m_tokens,omitempty"`
	ImageOutputPer1MTokens      *float64 `json:"image_output_per_1m_tokens,omitempty"`
}

// ModelCostStore manages model costs backed by bbolt.
type ModelCostStore struct {
	db *bolt.DB
}

// NewModelCostStore creates a new bbolt-backed model cost store.
func NewModelCostStore(db *bolt.DB) *ModelCostStore {
	return &ModelCostStore{db: db}
}

// targetKeySep 分隔 per-target 复合键。裸 model 键（全局层）与
// "target\x00model" 键共存于同一桶：含 \x00 即 per-target 记录，
// 不含即全局层——无歧义且对存量数据零迁移。
const targetKeySep = "\x00"

// costKey builds the bucket key: bare model for the global layer,
// "target\x00model" for per-target entries.
func costKey(cost ModelCost) string {
	model := strings.ToLower(strings.TrimSpace(cost.Model))
	target := strings.ToLower(strings.TrimSpace(cost.Target))
	if target == "" {
		return model
	}
	return target + targetKeySep + model
}

// List returns all model costs.
func (s *ModelCostStore) List() ([]ModelCost, error) {
	var costs []ModelCost
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelCosts))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var c ModelCost
			if err := json.Unmarshal(v, &c); err != nil {
				return fmt.Errorf("decode model cost %q: %w", string(k), err)
			}
			costs = append(costs, c)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return cloneCosts(costs), nil
}

// Upsert inserts or updates one model cost.
func (s *ModelCostStore) Upsert(cost ModelCost) error {
	if err := validateCost(cost); err != nil {
		return err
	}
	cost = normalizeCost(cost)
	key := costKey(cost)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelCosts))
		data, err := json.Marshal(cost)
		if err != nil {
			return fmt.Errorf("encode model cost: %w", err)
		}
		return b.Put([]byte(key), data)
	})
}

// Delete removes one global-layer model cost by model name.
// per-target 记录不受影响（键不同）。
func (s *ModelCostStore) Delete(model string) error {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return errors.New("model must not be empty")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelCosts))
		if b.Get([]byte(model)) == nil {
			return fmt.Errorf("model %q not found", model)
		}
		return b.Delete([]byte(model))
	})
}

// DeleteForTarget removes one per-target model cost.
func (s *ModelCostStore) DeleteForTarget(target, model string) error {
	target = strings.ToLower(strings.TrimSpace(target))
	model = strings.ToLower(strings.TrimSpace(model))
	if target == "" {
		return errors.New("target must not be empty")
	}
	if model == "" {
		return errors.New("model must not be empty")
	}
	key := costKey(ModelCost{Target: target, Model: model})
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelCosts))
		if b.Get([]byte(key)) == nil {
			return fmt.Errorf("model %q not found for target %q", model, target)
		}
		return b.Delete([]byte(key))
	})
}

// DeleteByKey is deprecated; kept for backward compatibility. Delegates to Delete.
func (s *ModelCostStore) DeleteByKey(endpointType, model string) error {
	return s.Delete(model)
}

// MigrateKeys migrates old "endpoint_type:model" keys to model-only keys.
// Safe to call multiple times; idempotent.
func (s *ModelCostStore) MigrateKeys() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelCosts))
		if b == nil {
			return nil
		}
		var toMigrate []struct {
			oldKey []byte
			cost   ModelCost
		}
		err := b.ForEach(func(k, v []byte) error {
			key := string(k)
			// per-target 复合键（含 \x00）是新形态，跳过 legacy 迁移判定——
			// 否则 "ws\x00qwen2.5:7b" 这类 target+Ollama 风格 model 键会被
			// “含冒号即 legacy”误迁移，导致 per-target 定价丢失。
			if strings.Contains(key, targetKeySep) {
				return nil
			}
			if !strings.Contains(key, ":") {
				return nil // already model-only key
			}
			var c ModelCost
			if err := json.Unmarshal(v, &c); err != nil {
				return nil // skip corrupt entries
			}
			toMigrate = append(toMigrate, struct {
				oldKey []byte
				cost   ModelCost
			}{append([]byte(nil), k...), c})
			return nil
		})
		if err != nil {
			return err
		}
		for _, m := range toMigrate {
			newKey := strings.ToLower(strings.TrimSpace(m.cost.Model))
			if newKey == "" {
				continue
			}
			// P10：model 名本身含 ':'（如 Ollama 风格 "qwen2.5:7b"）时，其
			// model-only key 也会被“含冒号即 legacy”判定命中，newKey == oldKey，
			// 先 Put 再 Delete 会把刚写入的记录删掉 → 定价丢失、配额漏计
			//（每次启动都执行）。key 已是目标形态，直接跳过。
			if newKey == string(m.oldKey) {
				continue
			}
			// Write under new key (last-writer-wins if conflict)
			data, err := json.Marshal(m.cost)
			if err != nil {
				continue
			}
			_ = b.Put([]byte(newKey), data)
			_ = b.Delete(m.oldKey)
		}
		return nil
	})
}

func validateCost(cost ModelCost) error {
	for _, value := range []*float64{cost.ExplicitCacheReadPer1MToken, cost.CacheWrite5mPer1MToken, cost.CacheWrite1hPer1MToken, cost.ImageInputPer1MTokens, cost.CachedImageInputPer1MTokens, cost.ImageOutputPer1MTokens} {
		if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return errors.New("optional token costs must be finite and non-negative")
		}
	}
	if strings.TrimSpace(cost.Model) == "" {
		return errors.New("model must not be empty")
	}
	if strings.TrimSpace(cost.Target) == "*" {
		return errors.New("target \"*\" is not supported in model costs; use billing template rules for wildcards")
	}
	if cost.InputPer1MTokens < 0 || cost.OutputPer1MTokens < 0 || cost.CacheWritePer1MToken < 0 || cost.CacheReadPer1MToken < 0 {
		return errors.New("token costs must be non-negative")
	}
	return nil
}

func normalizeCost(cost ModelCost) ModelCost {
	cost.Target = strings.ToLower(strings.TrimSpace(cost.Target))
	cost.Model = strings.ToLower(strings.TrimSpace(cost.Model))
	cost.EndpointType = strings.ToLower(strings.TrimSpace(cost.EndpointType))
	return cost
}

func cloneCosts(costs []ModelCost) []ModelCost {
	if costs == nil {
		return nil
	}
	cloned := make([]ModelCost, len(costs))
	copy(cloned, costs)
	for i := range cloned {
		cloned[i].ExplicitCacheReadPer1MToken = usage.CloneRate(costs[i].ExplicitCacheReadPer1MToken)
		cloned[i].CacheWrite5mPer1MToken = usage.CloneRate(costs[i].CacheWrite5mPer1MToken)
		cloned[i].CacheWrite1hPer1MToken = usage.CloneRate(costs[i].CacheWrite1hPer1MToken)
		cloned[i].ImageInputPer1MTokens = usage.CloneRate(costs[i].ImageInputPer1MTokens)
		cloned[i].CachedImageInputPer1MTokens = usage.CloneRate(costs[i].CachedImageInputPer1MTokens)
		cloned[i].ImageOutputPer1MTokens = usage.CloneRate(costs[i].ImageOutputPer1MTokens)
	}
	return cloned
}

// UnmarshalJSON supports both the new cache_write_per_1m_tokens key and the
// legacy cached_input_per_1m_tokens key for backward compatibility with
// existing bbolt data. The new key takes priority; the legacy key is only
// consulted when the new key is absent or zero.
func (m *ModelCost) UnmarshalJSON(data []byte) error {
	type alias ModelCost
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*m = ModelCost(a)
	if m.CacheWritePer1MToken == 0 {
		raw := map[string]json.RawMessage{}
		if json.Unmarshal(data, &raw) == nil {
			if v, ok := raw["cached_input_per_1m_tokens"]; ok {
				var f float64
				if json.Unmarshal(v, &f) == nil {
					m.CacheWritePer1MToken = f
				}
			}
		}
	}
	return nil
}
