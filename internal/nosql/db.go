package nosql

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
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

const (
	// mmapGranularity rounds InitialMmapSize up so the value is stable across restarts.
	mmapGranularity = 64 << 20
	// mmapFloor64 is the minimum headroom reserved beyond the current file size on
	// 64-bit platforms. A bbolt remap waits for every in-flight read transaction,
	// and while it waits all new read and write transactions block; reports hold
	// read transactions for seconds, so growth beyond the mapping stalled writers
	// for that long. 32-bit processes have scarce address space and use a smaller
	// floor.
	mmapFloor64 = 1 << 30
	mmapFloor32 = 128 << 20
	// mmapHeadroomWarnPercent is the database-size share of the reservation above
	// which CheckMmapHeadroom warns that growth will soon remap again.
	mmapHeadroomWarnPercent = 80
)

// initialMmapSize returns bbolt's InitialMmapSize for a database file:
// size + max(floor, size), rounded up to mmapGranularity. The headroom is at
// least as large as the file, so the file can double before the next remap, and
// at least the floor for small files. It only reserves virtual address space: no
// physical memory is used and the file is not grown (it still extends on
// demand). Caveat: the reservation counts against RLIMIT_AS (ulimit -v) and
// container address-space limits and so can squeeze the Go heap; a refused
// reservation falls back to no reservation (see openBolt).
func initialMmapSize(fileSize int64) int {
	floor := int64(mmapFloor64)
	if strconv.IntSize == 32 {
		floor = mmapFloor32
	}
	if fileSize < 0 {
		fileSize = 0
	}
	headroom := fileSize
	if headroom < floor {
		headroom = floor
	}
	want := fileSize + headroom
	if maxInt := int64(^uint(0) >> 1); want > maxInt/2 || want < 0 {
		// Leave room for bbolt's own rounding.
		want = maxInt / 2
	}
	want = (want + mmapGranularity - 1) / mmapGranularity * mmapGranularity
	return int(want)
}

// reservedMmapSize is the InitialMmapSize to request on this platform; zero
// where reserving is harmful (see mmapReservationSupported).
func reservedMmapSize(fileSize int64) int {
	if !mmapReservationSupported {
		return 0
	}
	return initialMmapSize(fileSize)
}

// boltOpen is replaceable in tests to exercise the mmap fallback.
var boltOpen = bolt.Open

// mmapReservations records, per database path, the reservation made at open so
// CheckMmapHeadroom can compare it with the file size later.
var mmapReservations sync.Map // path -> *mmapReservationState

type mmapReservationState struct {
	reserved int64
	warned   atomic.Bool
}

// openBolt opens the file with a reserved mapping. When the OS refuses the
// reservation (restricted address space, ulimit -v, containers) it logs a
// warning and retries without InitialMmapSize, restoring the previous behavior.
func openBolt(path string) (*bolt.DB, error) {
	var size int64
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
	}
	opts := &bolt.Options{Timeout: 1 * time.Second, InitialMmapSize: reservedMmapSize(size)}
	db, err := boltOpen(path, 0o600, opts)
	if err != nil && opts.InitialMmapSize > 0 && errors.Is(err, syscall.ENOMEM) {
		slog.Warn("bbolt: reserving initial mmap failed; opening without it (database growth may briefly block while reports run)",
			"initial_mmap_bytes", opts.InitialMmapSize, "error", err)
		opts = &bolt.Options{Timeout: 1 * time.Second}
		db, err = boltOpen(path, 0o600, opts)
	}
	if err == nil {
		mmapReservations.Store(path, &mmapReservationState{reserved: int64(opts.InitialMmapSize)})
	}
	return db, err
}

// CheckMmapHeadroom warns once per database when the file has grown past
// mmapHeadroomWarnPercent of the mapping reserved at open: beyond it bbolt
// remaps, which waits for in-flight reports and blocks all transactions
// meanwhile. A restart re-reserves with fresh headroom. Cheap (one stat), meant
// for a low-frequency periodic call; it reports whether it warned.
// It takes the path, not the handle, so a monitor goroutine never touches a
// closed *bolt.DB.
func CheckMmapHeadroom(path string, logger *slog.Logger) bool {
	v, ok := mmapReservations.Load(path)
	if !ok {
		return false
	}
	st := v.(*mmapReservationState)
	if st.reserved <= 0 {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size()*100 <= st.reserved*mmapHeadroomWarnPercent {
		return false
	}
	if !st.warned.CompareAndSwap(false, true) {
		return false
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("bbolt: database is close to its reserved mmap size; further growth will remap and may briefly block while reports run (restart to re-reserve)",
		"db_bytes", fi.Size(), "reserved_mmap_bytes", st.reserved)
	return true
}

// OpenDB opens a bbolt database and creates all required buckets.
func OpenDB(path string) (*bolt.DB, error) {
	db, err := openBolt(path)
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
		// 稳定 key ID 的派生密钥：只生成一次，重复打开不变。
		if _, err := ensureKeyIDSecret(tx); err != nil {
			return err
		}
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
