package nosql

import (
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Bucket name constants.
const (
	BucketClients           = "clients"
	BucketModelCosts        = "model_costs"
	BucketPricingTemplates  = "pricing_templates"
	BucketUsageEvents       = "usage_events"
	BucketAdminUsers        = "admin_users"
	BucketAdminAudit        = "admin_audit"
	BucketMeta              = "meta"
	BucketTargets           = "targets"
	BucketPools             = "pools"
	BucketModelGroups       = "model_groups"
	BucketClientQuotaGroups = "client_quota_groups"
	BucketJobLevels         = "job_levels"
)

var allBuckets = []string{
	BucketClients, BucketModelCosts, BucketPricingTemplates, BucketUsageEvents,
	BucketAdminUsers, BucketAdminAudit, BucketMeta,
	BucketUsageAggHourly, BucketTargets, BucketPools,
	BucketModelGroups, BucketClientQuotaGroups, BucketQuotaUsageAggHourly,
	BucketJobLevels,
	BucketUsageEventsByClient, BucketUsageClientHours,
	BucketUsageEventsQuarantine, BucketUsageReceipts,
}

// dbMaxBatchDelay bounds how long a lone db.Batch caller waits for company
// before committing. bbolt's 10ms default sat directly on synchronous write
// paths; 2ms still coalesces concurrent writers (the commit itself holds the
// write lock and fsyncs, so later callers keep piling into the next batch).
const dbMaxBatchDelay = 2 * time.Millisecond

// OpenDB opens a bbolt database and creates all required buckets.
func OpenDB(path string) (*bolt.DB, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bolt db: %w", err)
	}
	db.MaxBatchDelay = dbMaxBatchDelay
	err = db.Update(func(tx *bolt.Tx) error {
		indexSchemaMissing := tx.Bucket([]byte(BucketUsageEventsByClient)) == nil || tx.Bucket([]byte(BucketUsageClientHours)) == nil
		receiptsMissing := tx.Bucket([]byte(BucketUsageReceipts)) == nil
		for _, name := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("create bucket %q: %w", name, err)
			}
		}
		if indexSchemaMissing {
			// Recreating a missing derived bucket must not retain a completion
			// marker that could turn absent history into a successful empty read.
			meta := tx.Bucket([]byte(BucketMeta))
			if err := meta.Delete([]byte(usageClientIndexDone)); err != nil {
				return err
			}
			if err := meta.Delete([]byte(usageClientIndexCursor)); err != nil {
				return err
			}
		}
		if receiptsMissing {
			// A recreated, empty receipt bucket cannot prove that old identities
			// were never committed. Recompute the conservative legacy watermark.
			meta := tx.Bucket([]byte(BucketMeta))
			if err := meta.Delete([]byte(usageReceiptsWatermarkDoneKey)); err != nil {
				return err
			}
		}
		return initRequestStats(tx, time.Now())
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
