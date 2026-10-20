package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/clientip"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// ClientStore manages clients backed by bbolt.
type ClientStore struct {
	db *bolt.DB
}

// NewClientStore creates a new bbolt-backed client store.
func NewClientStore(db *bolt.DB) *ClientStore {
	return &ClientStore{db: db}
}

// DB exposes the shared database for companion stores created by the admin layer.
func (s *ClientStore) DB() *bolt.DB { return s.db }

// List returns all clients from the store.
func (s *ClientStore) List() ([]config.Client, error) {
	var clients []config.Client
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var c config.Client
			if err := json.Unmarshal(v, &c); err != nil {
				return fmt.Errorf("decode client %q: %w", string(k), err)
			}
			clients = append(clients, c)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return cloneClients(clients), nil
}

// Create appends a new client. It fails when name or access_key already exists.
func (s *ClientStore) Create(client config.Client) error {
	if err := validateClient(client); err != nil {
		return err
	}
	client = normalizeClient(client)
	key := strings.ToLower(client.Name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))

		// Check name uniqueness.
		if b.Get([]byte(key)) != nil {
			return fmt.Errorf("client %q already exists", client.Name)
		}

		// Check access_key uniqueness by scanning the bucket.
		if err := checkAccessKeyUnique(b, "", client.AccessKey); err != nil {
			return err
		}

		data, err := json.Marshal(client)
		if err != nil {
			return fmt.Errorf("encode client: %w", err)
		}
		return b.Put([]byte(key), data)
	})
}

// Update updates one client by name.
func (s *ClientStore) Update(name string, client config.Client) error {
	if err := validateClient(client); err != nil {
		return err
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name must not be empty")
	}
	oldKey := strings.ToLower(name)

	next := normalizeClient(client)
	newKey := strings.ToLower(next.Name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))

		// Check old record exists.
		if b.Get([]byte(oldKey)) == nil {
			return fmt.Errorf("client %q not found", name)
		}

		// If renaming, check new name doesn't conflict.
		if newKey != oldKey {
			if b.Get([]byte(newKey)) != nil {
				return fmt.Errorf("client %q already exists", next.Name)
			}
		}

		// Check access_key uniqueness (exclude the old record).
		if err := checkAccessKeyUnique(b, oldKey, next.AccessKey); err != nil {
			return err
		}

		// Delete old key if renamed, migrating the quota-group entry to the new
		// name in the same transaction so a rename does not orphan the config.
		if newKey != oldKey {
			if err := migrateQuotaEntry(tx, oldKey, newKey, next.Name); err != nil {
				return err
			}
			if err := b.Delete([]byte(oldKey)); err != nil {
				return err
			}
		}

		data, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("encode client: %w", err)
		}
		return b.Put([]byte(newKey), data)
	})
}

// migrateQuotaEntry moves a client's quota-group entry from oldKey to newKey
// and updates the stored Name. No-op when the entry does not exist or the
// quota bucket is absent. When newKey holds an ARCHIVED entry (a deleted
// client's identity snapshot), the rename is rejected so the archive row and
// its monthly attribution are not silently destroyed — clear the archived
// record on the personnel quota page first. Caller must hold a write
// transaction; returning an error aborts the whole rename atomically.
func migrateQuotaEntry(tx *bolt.Tx, oldKey, newKey, newName string) error {
	qb := tx.Bucket([]byte(BucketClientQuotaGroups))
	if qb == nil {
		return nil
	}
	if existing := qb.Get([]byte(newKey)); existing != nil {
		var occupied QuotaClientEntry
		if err := json.Unmarshal(existing, &occupied); err != nil {
			return fmt.Errorf("decode client quota groups at rename target %q: %w", newName, err)
		}
		if occupied.DeletedAt != "" {
			return fmt.Errorf("client name %q is reserved by an archived quota record (deleted %s); clear the archived record on the personnel quota page before reusing the name", newName, occupied.DeletedAt)
		}
	}
	value := qb.Get([]byte(oldKey))
	if value == nil {
		return nil
	}
	var entry QuotaClientEntry
	if err := json.Unmarshal(value, &entry); err != nil {
		return fmt.Errorf("decode client quota groups for rename: %w", err)
	}
	entry.Name = newName
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode client quota groups for rename: %w", err)
	}
	if err := qb.Put([]byte(newKey), data); err != nil {
		return err
	}
	return qb.Delete([]byte(oldKey))
}

// UpdateQuotas atomically replaces personal quota fields for all named clients.
func (s *ClientStore) UpdateQuotas(names []string, daily, weekly, monthly float64) ([]string, error) {
	if daily < 0 || weekly < 0 || monthly < 0 {
		return nil, errors.New("quota limits must not be negative")
	}

	var updatedNames []string
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		if b == nil {
			return errors.New("clients bucket not found")
		}

		type pendingUpdate struct {
			key  []byte
			data []byte
		}
		updates := make([]pendingUpdate, 0, len(names))
		updatedNames = make([]string, 0, len(names))
		seen := make(map[string]struct{}, len(names))
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" {
				return errors.New("client name must not be empty")
			}
			key := strings.ToLower(name)
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate client %q", name)
			}
			seen[key] = struct{}{}

			value := b.Get([]byte(key))
			if value == nil {
				return fmt.Errorf("client %q not found", name)
			}
			var client config.Client
			if err := json.Unmarshal(value, &client); err != nil {
				return fmt.Errorf("decode client %q: %w", name, err)
			}
			client.QuotaDailyUSD = daily
			client.QuotaWeeklyUSD = weekly
			client.QuotaMonthlyUSD = monthly
			data, err := json.Marshal(client)
			if err != nil {
				return fmt.Errorf("encode client %q: %w", name, err)
			}
			updates = append(updates, pendingUpdate{key: []byte(key), data: data})
			updatedNames = append(updatedNames, client.Name)
		}

		for _, update := range updates {
			if err := b.Put(update.key, update.data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updatedNames, nil
}

// Get returns one client by name, or nil when not found.
func (s *ClientStore) Get(name string) (*config.Client, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name must not be empty")
	}
	key := strings.ToLower(name)
	var client *config.Client
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		if b == nil {
			return nil
		}
		value := b.Get([]byte(key))
		if value == nil {
			return nil
		}
		var c config.Client
		if err := json.Unmarshal(value, &c); err != nil {
			return fmt.Errorf("decode client %q: %w", string(key), err)
		}
		client = &c
		return nil
	})
	if err != nil || client == nil {
		return nil, err
	}
	cloned := cloneClients([]config.Client{*client})
	return &cloned[0], nil
}

// Delete removes one client by name.
func (s *ClientStore) Delete(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name must not be empty")
	}
	key := strings.ToLower(name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		if b.Get([]byte(key)) == nil {
			return fmt.Errorf("client %q not found", name)
		}
		return b.Delete([]byte(key))
	})
}

// checkAccessKeyUnique scans the bucket to ensure no other client (excluding
// excludeKey) uses the given access_key.
func checkAccessKeyUnique(b *bolt.Bucket, excludeKey, accessKey string) error {
	accessKey = strings.TrimSpace(accessKey)
	return b.ForEach(func(k, v []byte) error {
		if string(k) == excludeKey {
			return nil
		}
		var c config.Client
		if err := json.Unmarshal(v, &c); err != nil {
			return nil // skip corrupt entries
		}
		if strings.TrimSpace(c.AccessKey) == accessKey {
			return errors.New("access_key already exists")
		}
		return nil
	})
}

func normalizeClient(client config.Client) config.Client {
	client.Name = strings.TrimSpace(client.Name)
	client.AccessKey = strings.TrimSpace(client.AccessKey)
	if rules, err := clientip.Parse(client.IPAddress); err == nil {
		client.IPAddress = rules.Canonical
	}
	normalizedTargets := make([]string, 0, len(client.AllowedTargets))
	seen := make(map[string]struct{}, len(client.AllowedTargets))
	for _, target := range client.AllowedTargets {
		target = strings.ToLower(strings.TrimSpace(target))
		if target == "" {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		normalizedTargets = append(normalizedTargets, target)
	}
	client.AllowedTargets = normalizedTargets
	client.JobLevel = strings.TrimSpace(client.JobLevel)
	client.ProjectGroup = strings.TrimSpace(client.ProjectGroup)
	return client
}

func validateClient(client config.Client) error {
	if strings.TrimSpace(client.Name) == "" {
		return errors.New("name must not be empty")
	}
	if strings.TrimSpace(client.AccessKey) == "" {
		return errors.New("access_key must not be empty")
	}
	if _, err := clientip.Parse(client.IPAddress); err != nil {
		return err
	}
	return nil
}

func cloneClients(clients []config.Client) []config.Client {
	if clients == nil {
		return nil
	}
	cloned := make([]config.Client, len(clients))
	for i := range clients {
		cloned[i] = clients[i]
		if len(clients[i].AllowedTargets) > 0 {
			cloned[i].AllowedTargets = append([]string(nil), clients[i].AllowedTargets...)
		}
	}
	return cloned
}

// ListWithQuota 返回所有至少一个 quota 字段 > 0 的 client。
// 用于配额检查时只需扫描有限集合（已配额的 client 通常是少量）。
func (s *ClientStore) ListWithQuota() ([]config.Client, error) {
	var result []config.Client
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var c config.Client
			if err := json.Unmarshal(v, &c); err != nil {
				return fmt.Errorf("unmarshal client %q: %w", string(k), err)
			}
			if c.QuotaDailyUSD > 0 || c.QuotaWeeklyUSD > 0 || c.QuotaMonthlyUSD > 0 {
				result = append(result, cloneClients([]config.Client{c})...)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		result = []config.Client{}
	}
	return result, nil
}
