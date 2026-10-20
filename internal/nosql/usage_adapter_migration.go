package nosql

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	bolt "go.etcd.io/bbolt"
)

const BucketUsageAdapterMigration = "target_usage_adapter_migrations"
const usageAdapterMigrationVersion = "explicit-v1"

type usageAdapterMigrationRecord struct {
	Target     string    `json:"target"`
	OldState   string    `json:"old_state"` // missing, empty, or auto
	OldValue   string    `json:"old_value"`
	NewValue   string    `json:"new_value"`
	MigratedAt time.Time `json:"migrated_at"`
	Version    string    `json:"version"`
}

type usageAdapterMigrationWrite struct {
	key      []byte
	data     []byte
	record   usageAdapterMigrationRecord
	hostname string
}

// MigrateUsageAdapterExplicit converts legacy automatic selections atomically.
// It edits only the raw usage_adapter field, preserving all other bytes and
// unknown/legacy fields. Records and targets commit together. No done flag is
// used, so a later legacy import can be converted without losing old records.
func MigrateUsageAdapterExplicit(db *bolt.DB, logger *slog.Logger) error {
	var writes []usageAdapterMigrationWrite
	err := db.Update(func(tx *bolt.Tx) error {
		var err error
		writes, err = migrateUsageAdapterTx(tx, time.Now().UTC())
		return err
	})
	if err != nil {
		return err
	}
	logUsageAdapterMigrations(logger, writes)
	return nil
}

// migrateUsageAdapterTx is shared by existing-database migration and legacy
// config import. The caller owns the transaction and logs only after commit.
func migrateUsageAdapterTx(tx *bolt.Tx, now time.Time) ([]usageAdapterMigrationWrite, error) {
	var writes []usageAdapterMigrationWrite
	b := tx.Bucket([]byte(BucketTargets))
	if b == nil {
		return nil, nil
	}
	if err := b.ForEach(func(k, raw []byte) error {
		fields, closeOffset, err := rawObjectFields(raw)
		if err != nil {
			return fmt.Errorf("decode target %q for usage_adapter migration: %w", k, err)
		}
		field, present := fields["usage_adapter"]
		oldValue := ""
		oldState := "missing"
		if present {
			if bytes.Equal(bytes.TrimSpace(field.value), []byte("null")) || json.Unmarshal(field.value, &oldValue) != nil {
				return fmt.Errorf("target %q usage_adapter must be a string", k)
			}
			normalized := config.NormalizeUsageAdapter(oldValue)
			switch normalized {
			case "":
				oldState = "empty"
			case config.UsageAdapterAuto:
				oldState = "auto"
			default:
				return nil // Explicit values are never rewritten, even unknown ones.
			}
		}
		var endpoint, name string
		if json.Unmarshal(fields["endpoint"].value, &endpoint) != nil {
			return fmt.Errorf("target %q endpoint must be a string", k)
		}
		if f, ok := fields["name"]; ok {
			if json.Unmarshal(f.value, &name) != nil {
				return fmt.Errorf("target %q name must be a string", k)
			}
		}
		if name == "" {
			name = string(k)
		}
		adapter, err := config.LegacyUsageAdapterForEndpoint(endpoint)
		if err != nil {
			return fmt.Errorf("target %q: %w", k, err)
		}
		u, _ := url.Parse(strings.TrimSpace(endpoint)) // Validated by the shared mapping.
		value, err := json.Marshal(adapter)
		if err != nil {
			return fmt.Errorf("encode target %q usage_adapter: %w", k, err)
		}
		var data []byte
		if present {
			data = append(data, raw[:field.start]...)
			data = append(data, value...)
			data = append(data, raw[field.end:]...)
		} else {
			data = append(data, raw[:closeOffset]...)
			if len(fields) > 0 {
				data = append(data, ',')
			}
			data = append(data, `"usage_adapter":`...)
			data = append(data, value...)
			data = append(data, raw[closeOffset:]...)
		}
		writes = append(writes, usageAdapterMigrationWrite{
			key: append([]byte(nil), k...), data: data, hostname: strings.ToLower(u.Hostname()),
			record: usageAdapterMigrationRecord{Target: name, OldState: oldState, OldValue: oldValue, NewValue: adapter, MigratedAt: now, Version: usageAdapterMigrationVersion},
		})
		return nil
	}); err != nil {
		return nil, err
	}
	if len(writes) == 0 {
		return nil, nil
	}
	records, err := tx.CreateBucketIfNotExists([]byte(BucketUsageAdapterMigration))
	if err != nil {
		return nil, fmt.Errorf("create usage_adapter migration records: %w", err)
	}
	for _, write := range writes {
		if err := b.Put(write.key, write.data); err != nil {
			return nil, fmt.Errorf("write target %q usage_adapter: %w", write.key, err)
		}
		if records.Get(write.key) != nil {
			continue // Preserve the first field-level migration record.
		}
		data, err := json.Marshal(write.record)
		if err != nil {
			return nil, fmt.Errorf("encode target %q migration record: %w", write.key, err)
		}
		if err := records.Put(write.key, data); err != nil {
			return nil, fmt.Errorf("write target %q migration record: %w", write.key, err)
		}
	}
	return writes, nil
}

func logUsageAdapterMigrations(logger *slog.Logger, writes []usageAdapterMigrationWrite) {
	if logger != nil {
		for _, write := range writes {
			logger.Info("migrated target usage_adapter", "target", write.record.Target,
				"old_state", write.record.OldState, "old", write.record.OldValue,
				"new", write.record.NewValue, "hostname", write.hostname)
		}
	}
}

type rawJSONField struct {
	value      json.RawMessage
	start, end int
}

// rawObjectFields uses decoder offsets instead of re-encoding the object. This
// preserves spacing, key order, unknown fields, and exact number spellings.
func rawObjectFields(raw []byte) (map[string]rawJSONField, int, error) {
	if !json.Valid(raw) {
		return nil, 0, errors.New("invalid target JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, 0, errors.New("target JSON must be an object")
	}
	fields := make(map[string]rawJSONField)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, 0, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, 0, errors.New("invalid target JSON field")
		}
		if _, exists := fields[name]; exists {
			return nil, 0, errors.New("duplicate target JSON field")
		}
		start := int(decoder.InputOffset())
		// The next value starts after the colon and JSON whitespace. Valid JSON
		// ensures both the colon and value exist within raw.
		for raw[start] != ':' {
			start++
		}
		start++
		for raw[start] == ' ' || raw[start] == '\n' || raw[start] == '\r' || raw[start] == '\t' {
			start++
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, 0, err
		}
		fields[name] = rawJSONField{value: value, start: start, end: int(decoder.InputOffset())}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, 0, err
	}
	return fields, int(decoder.InputOffset()) - 1, nil
}
