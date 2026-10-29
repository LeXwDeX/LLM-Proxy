package nosql

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// quotaV2MigratedMetaKey marks completion of the one-shot quota v2 migration
// (个人折算额度/池分层限额/plan 限额覆盖 一次性作废 + 快照留存 + 职级结构化)。
const quotaV2MigratedMetaKey = "quota_v2_migrated"

// legacyQuotaGroup mirrors the pre-v2 per-client plan entry (limit overrides).
// config.QuotaGroup no longer carries these fields; the snapshot must preserve
// them, so the migration decodes raw bytes with this shape.
type legacyQuotaGroup struct {
	Name       string  `json:"name"`
	DailyUSD   float64 `json:"daily_usd,omitempty"`
	WeeklyUSD  float64 `json:"weekly_usd,omitempty"`
	MonthlyUSD float64 `json:"monthly_usd,omitempty"`
}

type legacyQuotaClientEntry struct {
	Name         string             `json:"name"`
	Groups       []legacyQuotaGroup `json:"groups"`
	DeletedAt    string             `json:"deleted_at,omitempty"`
	IPAddress    string             `json:"ip_address,omitempty"`
	JobLevel     string             `json:"job_level,omitempty"`
	ProjectGroup string             `json:"project_group,omitempty"`
}

// QuotaV2Snapshot is the JSON document written to the data directory before
// any v1 quota configuration is rewritten (一次性作废前的对照快照).
type QuotaV2Snapshot struct {
	MigratedAt        string                   `json:"migrated_at"`
	Clients           []config.Client          `json:"clients"`
	ClientQuotaGroups []legacyQuotaClientEntry `json:"client_quota_groups"`
	JobLevelsDict     []string                 `json:"job_levels_dict"`
	ModelGroups       []config.ModelGroup      `json:"model_groups"`
}

// MigrateQuotaV2 performs the one-shot quota v2 migration (idempotent via the
// meta marker):
//
//  1. Snapshot: dump every v1 quota configuration surface (client personal
//     limits + pool quotas, per-client plan limit overrides, job-level string
//     dictionary, model-group catalog) to <snapshotDir>/quota-v1-snapshot-<UTC>.json.
//     The file is written BEFORE any rewrite; a failed snapshot aborts the
//     migration (fail-fast, nothing is modified).
//  2. Job levels: upgrade dictionary strings into structured job_levels
//     records (neutral coefficients → 1).
//  3. Plans: rewrite ACTIVE client_quota_groups entries to {name} only —
//     limit overrides are voided (personal coefficient defaults to 1).
//     Archived entries (deleted clients) keep their historical bytes untouched.
//  4. Meta marker commits with the rewrites in one transaction.
//
// Client records themselves are NOT rewritten: deprecated personal-quota and
// pool-quota fields stay dormant for rollback reference; evaluation ignores them.
func MigrateQuotaV2(db *bolt.DB, snapshotDir string, logger *slog.Logger) error {
	// Issue #125: a maintenance write; the integrity classification is stale
	// from here until a rebuild that starts after this write ended.
	defer integrityMutationBegin()()
	var done bool
	if err := db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(BucketMeta)); b != nil {
			done = b.Get([]byte(quotaV2MigratedMetaKey)) != nil
		}
		return nil
	}); err != nil {
		return fmt.Errorf("check quota v2 migration meta: %w", err)
	}
	if done {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	// ---- Read phase (view tx) ----
	snapshot := QuotaV2Snapshot{MigratedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(BucketClients)); b != nil {
			if err := b.ForEach(func(_, value []byte) error {
				var client config.Client
				if err := json.Unmarshal(value, &client); err != nil {
					return fmt.Errorf("decode client: %w", err)
				}
				snapshot.Clients = append(snapshot.Clients, client)
				return nil
			}); err != nil {
				return err
			}
		}
		if b := tx.Bucket([]byte(BucketClientQuotaGroups)); b != nil {
			if err := b.ForEach(func(_, value []byte) error {
				var entry legacyQuotaClientEntry
				if err := json.Unmarshal(value, &entry); err != nil {
					return fmt.Errorf("decode client quota groups: %w", err)
				}
				snapshot.ClientQuotaGroups = append(snapshot.ClientQuotaGroups, entry)
				return nil
			}); err != nil {
				return err
			}
		}
		dict, err := readDictEntries(tx, MetaKeyJobLevels)
		if err != nil {
			return err
		}
		snapshot.JobLevelsDict = dict
		if b := tx.Bucket([]byte(BucketModelGroups)); b != nil {
			if err := b.ForEach(func(_, value []byte) error {
				var group config.ModelGroup
				if err := json.Unmarshal(value, &group); err != nil {
					return fmt.Errorf("decode model group: %w", err)
				}
				snapshot.ModelGroups = append(snapshot.ModelGroups, group)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("quota v2 snapshot read: %w", err)
	}

	// ---- Snapshot file (before any rewrite) ----
	if snapshotDir == "" {
		snapshotDir = "."
	}
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		return fmt.Errorf("quota v2 snapshot dir: %w", err)
	}
	snapshotPath := filepath.Join(snapshotDir,
		"quota-v1-snapshot-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".json")
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode quota v2 snapshot: %w", err)
	}
	// A failed DB transaction can retry in the same second. Never truncate a
	// previous pre-migration snapshot; this file is the rollback evidence.
	snapshotFile, err := os.OpenFile(snapshotPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create quota v2 snapshot: %w", err)
	}
	if _, err := snapshotFile.Write(data); err != nil {
		_ = snapshotFile.Close()
		_ = os.Remove(snapshotPath)
		return fmt.Errorf("write quota v2 snapshot: %w", err)
	}
	if err := snapshotFile.Sync(); err != nil {
		_ = snapshotFile.Close()
		_ = os.Remove(snapshotPath)
		return fmt.Errorf("sync quota v2 snapshot: %w", err)
	}
	if err := snapshotFile.Close(); err != nil {
		_ = os.Remove(snapshotPath)
		return fmt.Errorf("write quota v2 snapshot: %w", err)
	}
	// The directory entry must also be durable before committing destructive
	// rewrites; syncing only the file does not guarantee its name survives a crash.
	snapshotDirectory, err := os.Open(snapshotDir)
	if err != nil {
		return fmt.Errorf("open quota v2 snapshot dir for sync: %w", err)
	}
	if err := snapshotDirectory.Sync(); err != nil {
		_ = snapshotDirectory.Close()
		return fmt.Errorf("sync quota v2 snapshot dir: %w", err)
	}
	if err := snapshotDirectory.Close(); err != nil {
		return fmt.Errorf("close quota v2 snapshot dir: %w", err)
	}
	logger.Info("quota v2 migration: v1 snapshot written", "path", snapshotPath,
		"clients", len(snapshot.Clients), "plan_entries", len(snapshot.ClientQuotaGroups))

	// ---- Write phase: single transaction with the meta marker ----
	return db.Update(func(tx *bolt.Tx) error {
		// 2. Job levels: dict strings → structured records (neutral coefficients).
		jb := tx.Bucket([]byte(BucketJobLevels))
		if jb == nil {
			return fmt.Errorf("bucket %q not found", BucketJobLevels)
		}
		for _, name := range snapshot.JobLevelsDict {
			key := jobLevelKey(name)
			if len(key) == 0 || jb.Get(key) != nil {
				continue
			}
			record := config.JobLevel{Name: name}
			raw, err := json.Marshal(record)
			if err != nil {
				return fmt.Errorf("encode job level %q: %w", name, err)
			}
			if err := jb.Put(key, raw); err != nil {
				return err
			}
		}

		// 3. Plans: void limit overrides on ACTIVE entries (archived untouched).
		if qb := tx.Bucket([]byte(BucketClientQuotaGroups)); qb != nil {
			type rewrite struct {
				key  []byte
				data []byte
			}
			var rewrites []rewrite
			if err := qb.ForEach(func(k, value []byte) error {
				var entry QuotaClientEntry
				if err := json.Unmarshal(value, &entry); err != nil {
					return fmt.Errorf("decode client quota groups: %w", err)
				}
				if entry.DeletedAt != "" {
					return nil // archived snapshot: keep historical bytes
				}
				plan := make([]config.QuotaGroup, 0, len(entry.Groups))
				for _, group := range entry.Groups {
					plan = append(plan, config.QuotaGroup{Name: group.Name}) // Coefficient nil → 1
				}
				entry.Groups = plan
				raw, err := json.Marshal(entry)
				if err != nil {
					return fmt.Errorf("encode client quota groups for %q: %w", entry.Name, err)
				}
				rewrites = append(rewrites, rewrite{key: append([]byte(nil), k...), data: raw})
				return nil
			}); err != nil {
				return err
			}
			for _, r := range rewrites {
				if err := qb.Put(r.key, r.data); err != nil {
					return err
				}
			}
		}

		// 4. Meta marker.
		meta := tx.Bucket([]byte(BucketMeta))
		if meta == nil {
			return fmt.Errorf("bucket %q not found", BucketMeta)
		}
		marker, err := json.Marshal(map[string]string{
			"migrated_at":  snapshot.MigratedAt,
			"snapshot":     snapshotPath,
			"specgit_note": "issue #120 额度体系 v2：职级系数兜底 + plan 个人系数 + 策略唯一源头",
		})
		if err != nil {
			return err
		}
		return meta.Put([]byte(quotaV2MigratedMetaKey), marker)
	})
}
