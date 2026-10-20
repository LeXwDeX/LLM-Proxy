package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// ModelGroupStore persists the global model-group catalog.
type ModelGroupStore struct{ db *bolt.DB }

func NewModelGroupStore(db *bolt.DB) *ModelGroupStore { return &ModelGroupStore{db: db} }

// GroupOwnershipIndex resolves (target, model) traffic to the catalog model
// group that owns it for consumption-statistics reporting. The resolution
// order mirrors quota matching (quotaGroupForCounters) and the coefficient
// lookup: an exact (target, model) binding wins over a whole-target binding.
// Ownership is unambiguous because catalog validation (ensureBindingsUnique)
// keeps exact bindings unique across groups and whole-target bindings unique
// across groups, so a pair resolves to at most one group while one target may
// legitimately split across groups by model.
type GroupOwnershipIndex struct {
	whole map[string]string
	exact map[string]string
}

// Group returns the owning model-group name for (target, model). ok=false
// means no catalog binding owns the pair (callers fall back to the built-in
// company_general / unknown_target buckets). A nil index owns nothing.
func (idx *GroupOwnershipIndex) Group(target, model string) (string, bool) {
	if idx == nil {
		return "", false
	}
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return "", false
	}
	if m := strings.ToLower(strings.TrimSpace(model)); m != "" {
		if group, ok := idx.exact[t+"\x00"+m]; ok {
			return group, true
		}
	}
	group, ok := idx.whole[t]
	return group, ok
}

// OwnershipIndex reads the catalog once so callers can resolve many
// (target, model) pairs without rescanning the bucket per pair.
func (s *ModelGroupStore) OwnershipIndex() (*GroupOwnershipIndex, error) {
	idx := &GroupOwnershipIndex{whole: make(map[string]string), exact: make(map[string]string)}
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelGroups))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, value []byte) error {
			var group config.ModelGroup
			if err := json.Unmarshal(value, &group); err != nil {
				return err
			}
			name := strings.TrimSpace(group.Name)
			for _, binding := range group.Bindings {
				target := strings.ToLower(strings.TrimSpace(binding.Target))
				if target == "" {
					continue
				}
				if model := strings.ToLower(strings.TrimSpace(binding.Model)); model != "" {
					idx.exact[target+"\x00"+model] = name
					continue
				}
				idx.whole[target] = name
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return idx, nil
}

// CoefficientLookup returns a function resolving (target, model) to the owning
// global model group's cost coefficient: an exact (target, model) binding wins
// over a whole-target binding, and unmatched pairs resolve to 1 (no discount).
// The resolution order mirrors quota.Manager's quotaGroupForCounters so
// statistics and quota discount the same traffic identically. The index is
// snapshotted once per call.
func (s *ModelGroupStore) CoefficientLookup() (func(target, model string) float64, error) {
	exact := make(map[string]float64)
	targetOnly := make(map[string]float64)
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelGroups))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, value []byte) error {
			var group config.ModelGroup
			if err := json.Unmarshal(value, &group); err != nil {
				return err
			}
			coef := group.CostCoefficient
			for _, binding := range group.Bindings {
				target := strings.ToLower(strings.TrimSpace(binding.Target))
				if target == "" {
					continue
				}
				model := strings.ToLower(strings.TrimSpace(binding.Model))
				if model != "" {
					exact[target+"\x00"+model] = coef
				} else {
					targetOnly[target] = coef
				}
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return func(target, model string) float64 {
		t := strings.ToLower(strings.TrimSpace(target))
		m := strings.ToLower(strings.TrimSpace(model))
		if t != "" && m != "" {
			if coef, ok := exact[t+"\x00"+m]; ok {
				return coef
			}
		}
		if coef, ok := targetOnly[t]; ok {
			return coef
		}
		return 1
	}, nil
}

func (s *ModelGroupStore) List() ([]config.ModelGroup, error) {
	var groups []config.ModelGroup
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelGroups))
		return b.ForEach(func(_, value []byte) error {
			var group config.ModelGroup
			if err := json.Unmarshal(value, &group); err != nil {
				return err
			}
			groups = append(groups, group)
			return nil
		})
	})
	return groups, err
}

func (s *ModelGroupStore) Create(group config.ModelGroup) error {
	if err := validateModelGroup(group); err != nil {
		return err
	}
	group.Name = strings.TrimSpace(group.Name)
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelGroups))
		key := []byte(strings.ToLower(group.Name))
		if b.Get(key) != nil {
			return fmt.Errorf("model group %q already exists", group.Name)
		}
		if err := validateQuotaGroupBindings(tx, group.Bindings, nil); err != nil {
			return err
		}
		if err := ensureBindingsUnique(tx, group.Name, group); err != nil {
			return err
		}
		data, err := json.Marshal(group)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	})
}

func (s *ModelGroupStore) Update(name string, group config.ModelGroup) ([]string, error) {
	if err := validateModelGroup(group); err != nil {
		return nil, err
	}
	group.Name = strings.TrimSpace(group.Name)
	oldKey := []byte(strings.ToLower(strings.TrimSpace(name)))
	key := []byte(strings.ToLower(group.Name))
	var renamedClients []string
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelGroups))
		oldValue := b.Get(oldKey)
		if oldValue == nil {
			return fmt.Errorf("model group %q not found", name)
		}
		var old config.ModelGroup
		if err := json.Unmarshal(oldValue, &old); err != nil {
			return fmt.Errorf("decode model group %q: %w", name, err)
		}
		if string(oldKey) != string(key) {
			if b.Get(key) != nil {
				return fmt.Errorf("model group %q already exists", group.Name)
			}
			// Rename cascades to active client plans instead of being blocked
			// by references: the catalog is the source, clients only apply it.
			clients, err := cascadeClientGroupRefs(tx, strings.TrimSpace(name), group.Name)
			if err != nil {
				return err
			}
			renamedClients = clients
		}
		staleAllowed := make(map[string]bool, len(old.Bindings))
		for _, ob := range old.Bindings {
			staleAllowed[bindingKey(ob)] = true
		}
		if err := validateQuotaGroupBindings(tx, group.Bindings, staleAllowed); err != nil {
			return err
		}
		if err := ensureBindingsUnique(tx, name, group); err != nil {
			return err
		}
		data, err := json.Marshal(group)
		if err != nil {
			return err
		}
		if string(oldKey) != string(key) {
			if err := b.Delete(oldKey); err != nil {
				return err
			}
		}
		return b.Put(key, data)
	})
	if err != nil {
		return nil, err
	}
	return renamedClients, nil
}

// Delete removes a model group and cascades the removal to every active
// client's plan (client_quota_groups) in the same transaction. Archived
// entries (deleted clients) keep their historical snapshot untouched. Returns
// the stored names of clients whose plan was modified.
func (s *ModelGroupStore) Delete(name string) ([]string, error) {
	var affected []string
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelGroups))
		key := []byte(strings.ToLower(strings.TrimSpace(name)))
		if b.Get(key) == nil {
			return fmt.Errorf("model group %q not found", name)
		}
		clients, err := cascadeClientGroupRefs(tx, name, "")
		if err != nil {
			return err
		}
		affected = clients
		return b.Delete(key)
	})
	if err != nil {
		return nil, err
	}
	return affected, nil
}

// cascadeClientGroupRefs rewrites active clients' plan entries when a global
// model group is deleted (newName empty) or renamed (newName set). Archived
// entries (DeletedAt set — deleted clients' historical snapshots) are never
// rewritten. Plan entries that become empty are removed entirely. Mutations
// are collected during the scan and applied after it (bbolt forbids mutating
// a bucket while iterating it). Returns the stored names of affected clients.
// Caller must hold a write tx.
func cascadeClientGroupRefs(tx *bolt.Tx, oldName, newName string) ([]string, error) {
	b := tx.Bucket([]byte(BucketClientQuotaGroups))
	if b == nil {
		return nil, nil
	}
	oldKey := strings.ToLower(strings.TrimSpace(oldName))
	newName = strings.TrimSpace(newName)
	var affected []string
	type pending struct {
		key  []byte
		data []byte // nil = delete the entry
	}
	var updates []pending
	err := b.ForEach(func(k, value []byte) error {
		var entry QuotaClientEntry
		if err := json.Unmarshal(value, &entry); err != nil {
			return err
		}
		if entry.DeletedAt != "" {
			return nil // archived snapshot: history is never rewritten
		}
		changed := false
		kept := make([]config.QuotaGroup, 0, len(entry.Groups))
		for _, group := range entry.Groups {
			if strings.ToLower(strings.TrimSpace(group.Name)) != oldKey {
				kept = append(kept, group)
				continue
			}
			changed = true
			if newName != "" {
				group.Name = newName
				kept = append(kept, group)
			}
		}
		if !changed {
			return nil
		}
		affected = append(affected, entry.Name)
		if len(kept) == 0 {
			updates = append(updates, pending{key: append([]byte(nil), k...)})
			return nil
		}
		entry.Groups = kept
		data, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("encode client quota groups for %q: %w", entry.Name, err)
		}
		updates = append(updates, pending{key: append([]byte(nil), k...), data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, u := range updates {
		if u.data == nil {
			if err := b.Delete(u.key); err != nil {
				return nil, err
			}
			continue
		}
		if err := b.Put(u.key, u.data); err != nil {
			return nil, err
		}
	}
	return affected, nil
}

// ensureBindingsUnique enforces catalog-level binding uniqueness across all
// model groups (the group named selfName is skipped). An exact target+model
// binding may belong to only one group; a whole-target binding may belong to
// only one group; an exact binding may coexist with a whole-target binding in
// another group.
func ensureBindingsUnique(tx *bolt.Tx, selfName string, group config.ModelGroup) error {
	b := tx.Bucket([]byte(BucketModelGroups))
	if b == nil {
		return nil
	}
	selfKey := strings.ToLower(strings.TrimSpace(selfName))
	return b.ForEach(func(k, value []byte) error {
		if strings.ToLower(string(k)) == selfKey {
			return nil
		}
		var other config.ModelGroup
		if err := json.Unmarshal(value, &other); err != nil {
			return err
		}
		for _, mine := range group.Bindings {
			for _, theirs := range other.Bindings {
				if bindingKey(mine) != bindingKey(theirs) {
					continue
				}
				return fmt.Errorf("binding %q/%q already used by model group %q", mine.Target, mine.Model, other.Name)
			}
		}
		return nil
	})
}

func validateModelGroup(group config.ModelGroup) error {
	if strings.TrimSpace(group.Name) == "" {
		return errors.New("name must not be empty")
	}
	if group.CostCoefficient < 0 {
		return errors.New("cost_coefficient must not be negative")
	}
	if group.DailyUSD < 0 || group.WeeklyUSD < 0 || group.MonthlyUSD < 0 {
		return errors.New("limits must not be negative")
	}
	return nil
}
