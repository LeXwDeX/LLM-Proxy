package nosql

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// MigrateFromJSON migrates data from old JSON/JSONL files into the bbolt database.
// It is idempotent: if the meta bucket contains a "migrated" key, migration is skipped.
// Individual file failures are logged as warnings and do not abort the overall migration.
func MigrateFromJSON(db *bolt.DB, dataFiles config.DataFiles) error {
	// Issue #125: a maintenance write; the integrity classification is stale
	// from here until a rebuild that starts after this write ended.
	defer integrityMutationBegin()()
	// Check if already migrated.
	var alreadyMigrated bool
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketMeta))
		if b == nil {
			return nil
		}
		if b.Get([]byte("migrated")) != nil {
			alreadyMigrated = true
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("check migration status: %w", err)
	}
	if alreadyMigrated {
		slog.Info("bbolt migration: already migrated, skipping")
		return nil
	}

	// Migrate each data source.
	migrateClients(db, dataFiles.ClientsFile)
	migrateModelCosts(db, dataFiles.ModelCostsFile)
	migrateUsageEvents(db, dataFiles.UsageEventsFile)
	migrateAdminUsers(db, dataFiles.AdminUsersFile)
	migrateAdminAudit(db, dataFiles.AdminAuditFile)

	// Mark migration complete.
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketMeta))
		meta := map[string]string{
			"migrated_at": time.Now().UTC().Format(time.RFC3339),
			"source":      "json_files",
		}
		data, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		return b.Put([]byte("migrated"), data)
	})
	if err != nil {
		return fmt.Errorf("write migration marker: %w", err)
	}

	slog.Info("bbolt migration: completed successfully")
	return nil
}

func migrateClients(db *bolt.DB, path string) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		slog.Warn("bbolt migration: clients file not found", "path", path, "err", err)
		return
	}

	// Check if bucket already has data.
	var hasData bool
	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		if b == nil {
			return nil
		}
		k, _ := b.Cursor().First()
		hasData = k != nil
		return nil
	})
	if hasData {
		slog.Info("bbolt migration: clients bucket already has data, skipping")
		return
	}

	content, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("bbolt migration: failed to read clients file", "path", path, "err", err)
		return
	}
	if len(strings.TrimSpace(string(content))) == 0 {
		return
	}

	var clients []config.Client
	if err := json.Unmarshal(content, &clients); err != nil {
		slog.Warn("bbolt migration: failed to decode clients", "err", err)
		return
	}

	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		for _, c := range clients {
			c = normalizeClient(c)
			key := strings.ToLower(c.Name)
			data, err := json.Marshal(c)
			if err != nil {
				slog.Warn("bbolt migration: failed to encode client", "name", c.Name, "err", err)
				continue
			}
			if err := b.Put([]byte(key), data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.Warn("bbolt migration: failed to write clients", "err", err)
		return
	}
	slog.Info("bbolt migration: migrated clients", "count", len(clients))
}

func migrateModelCosts(db *bolt.DB, path string) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		slog.Warn("bbolt migration: model costs file not found", "path", path, "err", err)
		return
	}

	var hasData bool
	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelCosts))
		if b == nil {
			return nil
		}
		k, _ := b.Cursor().First()
		hasData = k != nil
		return nil
	})
	if hasData {
		slog.Info("bbolt migration: model_costs bucket already has data, skipping")
		return
	}

	content, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("bbolt migration: failed to read model costs file", "path", path, "err", err)
		return
	}
	if len(strings.TrimSpace(string(content))) == 0 {
		return
	}

	var costs []ModelCost
	if err := json.Unmarshal(content, &costs); err != nil {
		slog.Warn("bbolt migration: failed to decode model costs", "err", err)
		return
	}

	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketModelCosts))
		for _, c := range costs {
			c = normalizeCost(c)
			key := costKey(c)
			data, err := json.Marshal(c)
			if err != nil {
				slog.Warn("bbolt migration: failed to encode model cost", "model", c.Model, "err", err)
				continue
			}
			if err := b.Put([]byte(key), data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.Warn("bbolt migration: failed to write model costs", "err", err)
		return
	}
	slog.Info("bbolt migration: migrated model costs", "count", len(costs))
}

func migrateUsageEvents(db *bolt.DB, path string) {
	// Issue #125: a maintenance write; the integrity classification is stale
	// from here until a rebuild that starts after this write ended.
	defer integrityMutationBegin()()
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		slog.Warn("bbolt migration: usage events file not found", "path", path, "err", err)
		return
	}

	var hasData bool
	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		if b == nil {
			return nil
		}
		k, _ := b.Cursor().First()
		hasData = k != nil
		return nil
	})
	if hasData {
		slog.Info("bbolt migration: usage_events bucket already has data, skipping")
		return
	}

	f, err := os.Open(path)
	if err != nil {
		slog.Warn("bbolt migration: failed to open usage events file", "path", path, "err", err)
		return
	}
	defer f.Close()

	var events []usage.Event
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var evt usage.Event
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		evt.ClientName = strings.TrimSpace(evt.ClientName)
		evt.EndpointType = strings.ToLower(strings.TrimSpace(evt.EndpointType))
		evt.Model = strings.ToLower(strings.TrimSpace(evt.Model))
		events = append(events, evt)
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("bbolt migration: scan error in usage events", "err", err)
		return
	}

	// Write in batches of 1000 to avoid oversized transactions.
	batchSize := 1000
	count := 0
	for i := 0; i < len(events); i += batchSize {
		end := i + batchSize
		if end > len(events) {
			end = len(events)
		}
		batch := events[i:end]
		err = db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(BucketUsageEvents))
			for _, evt := range batch {
				key := usageKey(evt.Timestamp, fmt.Sprintf("%d", count))
				count++
				data, err := json.Marshal(evt)
				if err != nil {
					continue
				}
				if err := b.Put([]byte(key), data); err != nil {
					return err
				}
				if err := addClientEventIndex(tx, evt, []byte(key), data); err != nil {
					return err
				}
				if err := bumpRawRevision(tx); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			slog.Warn("bbolt migration: failed to write usage events batch", "err", err)
			return
		}
	}
	slog.Info("bbolt migration: migrated usage events", "count", count)
}

func migrateAdminUsers(db *bolt.DB, path string) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		slog.Warn("bbolt migration: admin users file not found", "path", path, "err", err)
		return
	}

	var hasData bool
	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAdminUsers))
		if b == nil {
			return nil
		}
		k, _ := b.Cursor().First()
		hasData = k != nil
		return nil
	})
	if hasData {
		slog.Info("bbolt migration: admin_users bucket already has data, skipping")
		return
	}

	content, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("bbolt migration: failed to read admin users file", "path", path, "err", err)
		return
	}
	if len(strings.TrimSpace(string(content))) == 0 {
		return
	}

	var users []config.AdminUser
	if err := json.Unmarshal(content, &users); err != nil {
		slog.Warn("bbolt migration: failed to decode admin users", "err", err)
		return
	}

	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAdminUsers))
		for _, u := range users {
			u.Username = strings.TrimSpace(u.Username)
			if u.Role == "" {
				u.Role = "admin"
			}
			key := strings.ToLower(u.Username)
			data, err := json.Marshal(u)
			if err != nil {
				slog.Warn("bbolt migration: failed to encode admin user", "username", u.Username, "err", err)
				continue
			}
			if err := b.Put([]byte(key), data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.Warn("bbolt migration: failed to write admin users", "err", err)
		return
	}
	slog.Info("bbolt migration: migrated admin users", "count", len(users))
}

func migrateAdminAudit(db *bolt.DB, path string) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		slog.Warn("bbolt migration: admin audit file not found", "path", path, "err", err)
		return
	}

	var hasData bool
	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAdminAudit))
		if b == nil {
			return nil
		}
		k, _ := b.Cursor().First()
		hasData = k != nil
		return nil
	})
	if hasData {
		slog.Info("bbolt migration: admin_audit bucket already has data, skipping")
		return
	}

	f, err := os.Open(path)
	if err != nil {
		slog.Warn("bbolt migration: failed to open admin audit file", "path", path, "err", err)
		return
	}
	defer f.Close()

	var events []AuditEvent
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var evt AuditEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		events = append(events, evt)
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("bbolt migration: scan error in admin audit", "err", err)
		return
	}

	count := 0
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAdminAudit))
		for _, evt := range events {
			if evt.ID == "" {
				evt.ID = fmt.Sprintf("migrated-%d", count)
			}
			if evt.Timestamp.IsZero() {
				evt.Timestamp = time.Now().UTC()
			}
			key := auditKey(evt.Timestamp, evt.ID)
			data, err := json.Marshal(evt)
			if err != nil {
				continue
			}
			if err := b.Put([]byte(key), data); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		slog.Warn("bbolt migration: failed to write admin audit events", "err", err)
		return
	}
	slog.Info("bbolt migration: migrated admin audit events", "count", count)
}
