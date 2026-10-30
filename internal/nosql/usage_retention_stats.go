package nosql

import (
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// UsageRetentionRefreshInterval is how long a cached usage retention snapshot
// is served before a read triggers an asynchronous refresh. Production uses
// five minutes; tests may shorten it to observe the stale path.
var UsageRetentionRefreshInterval = 5 * time.Minute

// defaultUsageRetentionRefreshInterval is used when the var above is not
// positive.
const defaultUsageRetentionRefreshInterval = 5 * time.Minute

// Readiness and failure categories reported in UsageRetentionStats.Reason.
// They are stable identifiers, never free-form errors: error strings may carry
// keys or stored bytes.
const (
	UsageRetentionReasonNotReady          = "not_ready"
	UsageRetentionReasonStoreUnavailable  = "store_unavailable"
	UsageRetentionReasonTransactionFailed = "transaction_failed"
	UsageRetentionReasonInternalError     = "internal_error"
)

// UsageRetentionBucketUsage is the key-free size estimate of one retention
// ledger bucket. Entries, page and byte counts come from bbolt's bucket
// statistics; no key or value is decoded.
type UsageRetentionBucketUsage struct {
	Present    bool   `json:"present"`
	Entries    uint64 `json:"entries"`
	Pages      int    `json:"pages"`
	InUseBytes uint64 `json:"in_use_bytes"`
}

// UsageRetentionBuckets reports the four append-only ledger indexes whose
// growth Issue #125 P3 tracks.
type UsageRetentionBuckets struct {
	Receipts          UsageRetentionBucketUsage `json:"receipts"`
	ReceiptOwners     UsageRetentionBucketUsage `json:"receipt_owners"`
	RawOwners         UsageRetentionBucketUsage `json:"raw_owners"`
	FreshAttestations UsageRetentionBucketUsage `json:"fresh_attestations"`
}

// UsageRetentionEvents reports the earliest and latest parseable usage_events
// key time. InvalidKeys counts boundary keys that were inspected while
// locating those times but did not parse; a key time is never guessed.
type UsageRetentionEvents struct {
	Earliest    *time.Time `json:"earliest"`
	Latest      *time.Time `json:"latest"`
	InvalidKeys uint64     `json:"invalid_keys"`
}

// UsageRetentionMetadata reports the persisted receipt watermarks and marker
// presence. Corrupt watermark values are flagged and never echoed.
type UsageRetentionMetadata struct {
	LegacyUnknownBefore        *time.Time `json:"legacy_unknown_before"`
	LegacyUnknownBeforeInvalid bool       `json:"legacy_unknown_before_invalid"`
	UntrustedThrough           *time.Time `json:"untrusted_through"`
	UntrustedThroughInvalid    bool       `json:"untrusted_through_invalid"`
	WatermarkDoneV1            bool       `json:"watermark_done_v1"`
	WatermarkDoneV2            bool       `json:"watermark_done_v2"`
	UnknownAggregateHour       bool       `json:"unknown_aggregate_hour"`
}

// UsageRetentionStats is a read-only, key-free snapshot of usage retention
// ledger growth and watermark state. It never contains keys, client names,
// model names or stored bytes. Available is false when no snapshot could be
// produced; Reason is then one of the UsageRetentionReason* categories.
type UsageRetentionStats struct {
	Available  bool                    `json:"available"`
	Reason     string                  `json:"reason"`
	CapturedAt *time.Time              `json:"captured_at"`
	Buckets    *UsageRetentionBuckets  `json:"buckets"`
	Events     *UsageRetentionEvents   `json:"events"`
	Metadata   *UsageRetentionMetadata `json:"metadata"`
}

// usageRetentionCache holds the last snapshot and the single-flight refresh
// state. A refresh only runs in the short-lived goroutine started by a read;
// there is no resident ticker.
type usageRetentionCache struct {
	mu         sync.Mutex
	snapshot   *UsageRetentionStats
	refreshing bool
	// load overrides the database loader in tests, which can hold a refresh
	// open to observe single-flight behavior. nil means the real loader.
	load func() UsageRetentionStats
}

// UsageRetentionSnapshot returns the most recent retention snapshot without
// reading the database on the request path. The first call starts an
// asynchronous refresh and reports not_ready. A stale snapshot is returned
// immediately while at most one asynchronous refresh runs.
func (s *UsageStore) UsageRetentionSnapshot() UsageRetentionStats {
	if s == nil || s.db == nil {
		return UsageRetentionStats{Available: false, Reason: UsageRetentionReasonStoreUnavailable}
	}
	cache := &s.retention
	now := s.retentionNow()
	cache.mu.Lock()
	snap := cache.snapshot
	stale := snap == nil || snap.CapturedAt == nil || now.Sub(*snap.CapturedAt) >= usageRetentionInterval()
	startRefresh := false
	var load func() UsageRetentionStats
	if stale && !cache.refreshing {
		cache.refreshing = true
		load = cache.load
		if load == nil {
			load = s.loadUsageRetentionStats
		}
		startRefresh = true
	}
	cache.mu.Unlock()

	if startRefresh {
		go s.refreshUsageRetention(load)
	}
	if snap == nil {
		return UsageRetentionStats{Available: false, Reason: UsageRetentionReasonNotReady}
	}
	return snap.clone()
}

// refreshUsageRetention publishes one snapshot and clears the single-flight
// flag, including after a recovered panic.
func (s *UsageStore) refreshUsageRetention(load func() UsageRetentionStats) {
	stats := usageRetentionLoadSafely(s, load)
	now := s.retentionNow()
	stats.CapturedAt = &now

	cache := &s.retention
	cache.mu.Lock()
	cache.snapshot = &stats
	cache.refreshing = false
	cache.mu.Unlock()
}

func usageRetentionLoadSafely(s *UsageStore, load func() UsageRetentionStats) (stats UsageRetentionStats) {
	defer func() {
		if recover() != nil {
			stats = UsageRetentionStats{Available: false, Reason: UsageRetentionReasonInternalError}
		}
	}()
	return load()
}

// loadUsageRetentionStats reads every metric in one read transaction.
func (s *UsageStore) loadUsageRetentionStats() UsageRetentionStats {
	stats := UsageRetentionStats{Available: true}
	err := s.db.View(func(tx *bolt.Tx) error {
		stats.Buckets = &UsageRetentionBuckets{
			Receipts:          usageRetentionBucket(tx, BucketUsageReceipts),
			ReceiptOwners:     usageRetentionBucket(tx, receiptOwnersBucket),
			RawOwners:         usageRetentionBucket(tx, rawOwnerBucket),
			FreshAttestations: usageRetentionBucket(tx, freshAttestationBucket),
		}
		stats.Events = usageRetentionEventRange(tx.Bucket([]byte(BucketUsageEvents)))
		stats.Metadata = usageRetentionMetadata(tx.Bucket([]byte(BucketMeta)))
		return nil
	})
	if err != nil {
		return UsageRetentionStats{Available: false, Reason: UsageRetentionReasonTransactionFailed}
	}
	return stats
}

// usageRetentionBucket reports only counts derived from bucket statistics; it
// never walks keys or values. A small bucket is stored inline in its parent,
// where bbolt reports its bytes as InlineBucketInuse instead of LeafInuse, so
// the byte estimate must include it. Pages counts logical leaf and branch
// pages only: an inline bucket has none of its own.
func usageRetentionBucket(tx *bolt.Tx, name string) UsageRetentionBucketUsage {
	b := tx.Bucket([]byte(name))
	if b == nil {
		return UsageRetentionBucketUsage{}
	}
	st := b.Stats()
	return UsageRetentionBucketUsage{
		Present:    true,
		Entries:    uint64(st.KeyN),
		Pages:      st.LeafPageN + st.BranchPageN,
		InUseBytes: uint64(st.LeafInuse + st.BranchInuse + st.InlineBucketInuse),
	}
}

// usageRetentionEventRange locates the first and last parseable key time.
// Boundary keys that do not parse are counted; no timestamp is inferred from
// an unparseable key. If every key is invalid both times stay nil.
func usageRetentionEventRange(b *bolt.Bucket) *UsageRetentionEvents {
	events := &UsageRetentionEvents{}
	if b == nil {
		return events
	}
	cursor := b.Cursor()
	found := false
	for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
		at, ok := canonicalEventKeyStamp(k)
		if !ok {
			events.InvalidKeys++
			continue
		}
		earliest := at
		events.Earliest = &earliest
		found = true
		break
	}
	if !found {
		// The forward scan already counted every key exactly once.
		return events
	}
	for k, _ := cursor.Last(); k != nil; k, _ = cursor.Prev() {
		at, ok := canonicalEventKeyStamp(k)
		if !ok {
			events.InvalidKeys++
			continue
		}
		latest := at
		events.Latest = &latest
		break
	}
	return events
}

func usageRetentionMetadata(meta *bolt.Bucket) *UsageRetentionMetadata {
	metadata := &UsageRetentionMetadata{}
	if meta == nil {
		return metadata
	}
	metadata.LegacyUnknownBefore, metadata.LegacyUnknownBeforeInvalid = usageRetentionWatermark(meta, usageReceiptsWatermarkKey)
	metadata.UntrustedThrough, metadata.UntrustedThroughInvalid = usageRetentionWatermark(meta, usageReceiptsUntrustedThroughKey)
	metadata.WatermarkDoneV1 = meta.Get([]byte(usageReceiptsWatermarkDoneKey)) != nil
	metadata.WatermarkDoneV2 = meta.Get([]byte(usageReceiptsWatermarkDoneV2Key)) != nil
	metadata.UnknownAggregateHour = meta.Get([]byte(usageReceiptsUnknownHourKey)) != nil
	return metadata
}

// usageRetentionWatermark decodes one stored watermark. A corrupt value is
// reported only as invalid; the decode error would embed the raw bytes, so it
// is deliberately dropped.
func usageRetentionWatermark(meta *bolt.Bucket, key string) (*time.Time, bool) {
	raw := meta.Get([]byte(key))
	if len(raw) == 0 {
		return nil, false
	}
	at, err := decodeUsageReceiptWatermark(raw)
	if err != nil {
		return nil, true
	}
	at = at.UTC()
	return &at, false
}

func usageRetentionInterval() time.Duration {
	if UsageRetentionRefreshInterval > 0 {
		return UsageRetentionRefreshInterval
	}
	return defaultUsageRetentionRefreshInterval
}

func (s *UsageStore) retentionNow() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// clone returns an independent copy so concurrent readers cannot mutate the
// published snapshot through its exported pointers.
func (s UsageRetentionStats) clone() UsageRetentionStats {
	out := s
	if s.CapturedAt != nil {
		at := *s.CapturedAt
		out.CapturedAt = &at
	}
	if s.Buckets != nil {
		buckets := *s.Buckets
		out.Buckets = &buckets
	}
	if s.Events != nil {
		events := *s.Events
		if s.Events.Earliest != nil {
			at := *s.Events.Earliest
			events.Earliest = &at
		}
		if s.Events.Latest != nil {
			at := *s.Events.Latest
			events.Latest = &at
		}
		out.Events = &events
	}
	if s.Metadata != nil {
		metadata := *s.Metadata
		if s.Metadata.LegacyUnknownBefore != nil {
			at := *s.Metadata.LegacyUnknownBefore
			metadata.LegacyUnknownBefore = &at
		}
		if s.Metadata.UntrustedThrough != nil {
			at := *s.Metadata.UntrustedThrough
			metadata.UntrustedThrough = &at
		}
		out.Metadata = &metadata
	}
	return out
}
