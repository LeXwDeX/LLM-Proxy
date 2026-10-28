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
		oldMeta := tx.Bucket([]byte(BucketMeta))
		indexInitialized := false
		if oldMeta != nil {
			indexInitialized = oldMeta.Get([]byte(usageClientIndexDone)) != nil || oldMeta.Get([]byte(usageClientIndexCursor)) != nil || oldMeta.Get([]byte(usageRawRevisionKey)) != nil
		}
		for _, name := range []string{BucketUsageEventsByClient, BucketUsageClientHours} {
			if b := tx.Bucket([]byte(name)); b != nil {
				key, _ := b.Cursor().First()
				indexInitialized = indexInitialized || key != nil
			}
		}
		if indexSchemaMissing && indexInitialized {
			if oldMeta == nil {
				return fmt.Errorf("usage client index source missing: meta bucket missing")
			}
			if value := oldMeta.Get([]byte(usageClientIndexSourceMissing)); value != nil && string(value) != "1" {
				return fmt.Errorf("invalid usage client index source loss marker")
			}
			if err := oldMeta.Put([]byte(usageClientIndexSourceMissing), []byte("1")); err != nil {
				return fmt.Errorf("record usage client index source loss: %w", err)
			}
		}
		freshUsage := oldMeta == nil
		for _, name := range []string{BucketUsageEvents, BucketUsageAggHourly, BucketQuotaUsageAggHourly, BucketUsageReceipts, receiptOwnersBucket, rawOwnerBucket, freshAttestationBucket, "usage_rollup_proofs_v1", rollupProofBucket, BucketUsageEventsQuarantine, BucketUsageEventsByClient, BucketUsageClientHours, unresolvedBucket, usageBackfillSnapshotBucket} {
			if bucket := tx.Bucket([]byte(name)); bucket != nil {
				if key, _ := bucket.Cursor().First(); key != nil {
					freshUsage = false
				}
			}
		}
		usageAggMissing := oldMeta != nil && oldMeta.Get([]byte(usageAggBuiltMetaKey)) != nil && tx.Bucket([]byte(BucketUsageAggHourly)) == nil
		quotaAggMissing := oldMeta != nil && oldMeta.Get([]byte(quotaUsageAggBuiltMetaKey)) != nil && tx.Bucket([]byte(BucketQuotaUsageAggHourly)) == nil
		for _, name := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("create bucket %q: %w", name, err)
			}
		}
		meta := tx.Bucket([]byte(BucketMeta))
		if freshUsage {
			if err := meta.Put([]byte(usageAccountingProofTrackedKey), []byte("1")); err != nil {
				return err
			}
		}
		if usageAggMissing {
			if err := meta.Put([]byte(usageAggregateSourceMissingPrefix+BucketUsageAggHourly), []byte("1")); err != nil {
				return err
			}
		}
		if quotaAggMissing {
			if err := meta.Put([]byte(usageAggregateSourceMissingPrefix+BucketQuotaUsageAggHourly), []byte("1")); err != nil {
				return err
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
			// A lost initialized receipt bucket may have contained identities at
			// any time, even if their corresponding rollups have also been lost.
			meta := tx.Bucket([]byte(BucketMeta))
			if meta.Get([]byte(usageReceiptsWatermarkDoneKey)) != nil || meta.Get([]byte(usageReceiptsWatermarkDoneV2Key)) != nil {
				if err := meta.Put([]byte(usageReceiptsUnknownHourKey), []byte("1")); err != nil {
					return err
				}
			}
			if err := meta.Delete([]byte(usageReceiptsWatermarkDoneKey)); err != nil {
				return err
			}
			if err := meta.Delete([]byte(usageReceiptsWatermarkDoneV2Key)); err != nil {
				return err
			}
		}
		if err := restoreIdentityOnlyRechecks(tx); err != nil {
			return err
		}
		if evidence := tx.Bucket([]byte(unresolvedBucket)); evidence != nil {
			if _, err := unresolvedState(tx); err != nil {
				return err
			}
			if err := canonicalizeUnresolvedKeys(tx, evidence); err != nil {
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
