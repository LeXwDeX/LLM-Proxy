package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// JobLevelStore persists structured job levels (职级) in a dedicated bucket.
// 额度体系 v2：职级从字符串字典升级为「日/周/月三系数」记录，是个人维度
// 额度的兜底系数来源（有效额度 = 组基数 × 职级系数 × 个人系数）。
// 客户端通过 Client.JobLevel（职级名）绑定；未绑定或绑定值不存在 = 未配置
// 额度 = 请求被拒绝（internal/quota 评估）。
type JobLevelStore struct{ db *bolt.DB }

func NewJobLevelStore(db *bolt.DB) *JobLevelStore { return &JobLevelStore{db: db} }

func jobLevelKey(name string) []byte {
	return []byte(strings.ToLower(strings.TrimSpace(name)))
}

// List returns all job levels sorted by name.
func (s *JobLevelStore) List() ([]config.JobLevel, error) {
	var levels []config.JobLevel
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketJobLevels))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, value []byte) error {
			var level config.JobLevel
			if err := json.Unmarshal(value, &level); err != nil {
				return fmt.Errorf("decode job level: %w", err)
			}
			levels = append(levels, level)
			return nil
		})
	})
	return levels, err
}

// Get returns one job level by name (case-insensitive). Returns (nil, nil)
// when absent.
func (s *JobLevelStore) Get(name string) (*config.JobLevel, error) {
	key := jobLevelKey(name)
	if len(key) == 0 {
		return nil, nil
	}
	var level config.JobLevel
	err := s.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket([]byte(BucketJobLevels)).Get(key)
		if value == nil {
			return nil
		}
		return json.Unmarshal(value, &level)
	})
	if err != nil {
		return nil, err
	}
	if level.Name == "" {
		return nil, nil
	}
	return &level, nil
}

func validateJobLevel(level config.JobLevel) error {
	if strings.TrimSpace(level.Name) == "" {
		return errors.New("name must not be empty")
	}
	for _, c := range []*float64{level.CoefficientDaily, level.CoefficientWeekly, level.CoefficientMonthly} {
		if c != nil && *c < 0 {
			return errors.New("coefficients must not be negative")
		}
	}
	return nil
}

// Create adds a new job level.
func (s *JobLevelStore) Create(level config.JobLevel) error {
	if err := validateJobLevel(level); err != nil {
		return err
	}
	level.Name = strings.TrimSpace(level.Name)
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketJobLevels))
		key := jobLevelKey(level.Name)
		if b.Get(key) != nil {
			return fmt.Errorf("job level %q already exists", level.Name)
		}
		data, err := json.Marshal(level)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	})
}

// Update replaces a job level. Renaming cascades to every client record whose
// JobLevel references the old name (same transaction), so a rename never
// orphans a client into the "no quota → reject" state. Returns the names of
// clients whose JobLevel was rewritten.
func (s *JobLevelStore) Update(name string, level config.JobLevel) ([]string, error) {
	if err := validateJobLevel(level); err != nil {
		return nil, err
	}
	level.Name = strings.TrimSpace(level.Name)
	oldKey := jobLevelKey(name)
	newKey := jobLevelKey(level.Name)
	var renamedClients []string
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketJobLevels))
		if b.Get(oldKey) == nil {
			return fmt.Errorf("job level %q not found", name)
		}
		if string(oldKey) != string(newKey) {
			if b.Get(newKey) != nil {
				return fmt.Errorf("job level %q already exists", level.Name)
			}
		}
		data, err := json.Marshal(level)
		if err != nil {
			return err
		}
		if string(oldKey) != string(newKey) {
			clients, err := cascadeClientJobLevel(tx, strings.TrimSpace(name), level.Name)
			if err != nil {
				return err
			}
			renamedClients = clients
			if err := b.Delete(oldKey); err != nil {
				return err
			}
		}
		return b.Put(newKey, data)
	})
	if err != nil {
		return nil, err
	}
	return renamedClients, nil
}

// cascadeClientJobLevel rewrites client records whose JobLevel matches oldName
// (case-insensitive) to newName. Caller must hold a write tx.
func cascadeClientJobLevel(tx *bolt.Tx, oldName, newName string) ([]string, error) {
	b := tx.Bucket([]byte(BucketClients))
	if b == nil {
		return nil, nil
	}
	var affected []string
	type pending struct {
		key  []byte
		data []byte
		name string
	}
	var updates []pending
	err := b.ForEach(func(k, value []byte) error {
		var client config.Client
		if err := json.Unmarshal(value, &client); err != nil {
			return err
		}
		if !strings.EqualFold(strings.TrimSpace(client.JobLevel), oldName) {
			return nil
		}
		client.JobLevel = newName
		data, err := json.Marshal(client)
		if err != nil {
			return fmt.Errorf("encode client %q: %w", client.Name, err)
		}
		affected = append(affected, client.Name)
		updates = append(updates, pending{key: append([]byte(nil), k...), data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, u := range updates {
		if err := b.Put(u.key, u.data); err != nil {
			return nil, err
		}
	}
	return affected, nil
}

// Delete removes a job level. Deletion is REJECTED while active clients still
// reference the level: unbinding would silently drop those clients into the
// "no quota → reject all requests" state, so the error names every referencing
// client for the admin to rebind first. Returns the referencing client names
// in the error.
func (s *JobLevelStore) Delete(name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketJobLevels))
		key := jobLevelKey(name)
		if b.Get(key) == nil {
			return fmt.Errorf("job level %q not found", name)
		}
		refs, err := clientsReferencingJobLevel(tx, name)
		if err != nil {
			return err
		}
		if len(refs) > 0 {
			return fmt.Errorf("job level %q is referenced by clients: %s（请先为这些客户端改绑职级）", name, strings.Join(refs, ", "))
		}
		return b.Delete(key)
	})
}

// clientsReferencingJobLevel lists active client names bound to the level
// (case-insensitive). Caller must hold a tx.
func clientsReferencingJobLevel(tx *bolt.Tx, name string) ([]string, error) {
	b := tx.Bucket([]byte(BucketClients))
	if b == nil {
		return nil, nil
	}
	var refs []string
	err := b.ForEach(func(_, value []byte) error {
		var client config.Client
		if err := json.Unmarshal(value, &client); err != nil {
			return err
		}
		if strings.EqualFold(strings.TrimSpace(client.JobLevel), strings.TrimSpace(name)) {
			refs = append(refs, client.Name)
		}
		return nil
	})
	return refs, err
}
