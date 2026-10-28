package nosql

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// BucketUsageReceipts keeps one digest per pruned event identity. It is
// append-only: no garbage collection is defined, so disk use grows with the
// number of retained event identities (O(N)). A receipt stores only the
// version and the normalized digest, never the event payload.
const (
	BucketUsageReceipts              = "usage_record_receipts_v1"
	usageReceiptsWatermarkDoneKey    = "usage_receipts_watermark_done_v1"
	usageReceiptsWatermarkDoneV2Key  = "usage_receipts_watermark_done_v2"
	usageReceiptsWatermarkKey        = "usage_receipts_legacy_unknown_before_v1"
	usageReceiptsUnknownHourKey      = "usage_receipts_unknown_aggregate_hour_v2"
	usageReceiptsUntrustedThroughKey = "usage_receipts_untrusted_through_v2"
	usageReceiptVersion              = 1
)

// ErrUsageIdentityUnverified reports an identity whose prior commitment cannot
// be ruled out from retained raw records, receipts and aggregate evidence. It
// is never charged automatically; journal recovery moves it to the rejected
// sidecar for manual review.
var ErrUsageIdentityUnverified = errors.New("usage identity predates receipt tracking and cannot be verified")

var ErrUsageBundlePartial = errors.New("usage bundle is partially committed")

// ErrUsageRecordConflict reports an explicit identity whose committed raw row
// or receipt holds a different event digest. Retrying cannot change the
// committed data, so the rejection is permanent.
var ErrUsageRecordConflict = errors.New("usage record conflicts with committed event data")

// ErrUsageBundleInvalid reports a bundle the ledger can never accept: its
// payload fails validation, or only part of it is already committed.
var ErrUsageBundleInvalid = errors.New("usage bundle cannot be committed")

// usageKindError tags an error with a permanent rejection kind while keeping
// the original message unchanged for callers and logs.
type usageKindError struct {
	kind error
	err  error
}

func (e *usageKindError) Error() string   { return e.err.Error() }
func (e *usageKindError) Unwrap() []error { return []error{e.err, e.kind} }

func withUsageKind(kind, err error) error {
	if err == nil {
		return nil
	}
	return &usageKindError{kind: kind, err: err}
}

// Receipt migration retry backoff after a failed attempt.
const (
	usageReceiptRetryMin = time.Second
	usageReceiptRetryMax = time.Minute
)

// receiptState is the result of looking up one bundle row.
type receiptState int

const (
	identityNew receiptState = iota
	identityRaw
	identityReceipt
	identityRawAndReceipt
)

type usageReceipt struct {
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

// EnsureUsageReceipts performs the one-time legacy coverage check before any
// receipt can be written. The check is repeatable: a failed scan writes no
// completion marker, and a successful scan is skipped by later calls.
//
// The watermark is the end of the newest hour where retained raw records and
// permanent hourly record counts disagree, or whose aggregate evidence cannot
// be decoded. An identity in an untrusted range with neither raw data nor a
// receipt cannot be proven committed or new, so it is rejected. The result
// is cached per store; a failed attempt is cached too and retried only after
// an exponential backoff, so writes do not rescan the database each time.
func (s *UsageStore) EnsureUsageReceipts() error {
	if s == nil || s.db == nil {
		return errors.New("usage receipts: store is not initialized")
	}
	s.receiptMu.Lock()
	defer s.receiptMu.Unlock()
	if s.receiptsReady {
		return nil
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	if s.receiptErr != nil && now().Before(s.receiptRetryAt) {
		return s.receiptErr
	}
	err := s.ensureUsageReceiptsLocked(now())
	if err == nil {
		s.receiptsReady = true
		s.receiptErr, s.receiptBackoff = nil, 0
		return nil
	}
	s.receiptBackoff *= 2
	if s.receiptBackoff < usageReceiptRetryMin {
		s.receiptBackoff = usageReceiptRetryMin
	}
	if s.receiptBackoff > usageReceiptRetryMax {
		s.receiptBackoff = usageReceiptRetryMax
	}
	s.receiptErr = err
	s.receiptRetryAt = now().Add(s.receiptBackoff)
	slog.Error("usage receipts: watermark migration failed; unverified writes stay blocked until a retry succeeds",
		"retry_in", s.receiptBackoff.String(), "error", err)
	return err
}

func (s *UsageStore) ensureUsageReceiptsLocked(scanTime time.Time) error {
	s.receiptScans.Add(1)
	return s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketUsageReceipts)) == nil {
			return fmt.Errorf("usage receipts: bucket %q missing", BucketUsageReceipts)
		}
		meta := tx.Bucket([]byte(BucketMeta))
		if meta == nil {
			return errors.New("usage receipts: meta bucket missing")
		}
		old, err := decodeUsageReceiptWatermark(meta.Get([]byte(usageReceiptsWatermarkKey)))
		if err != nil {
			return err
		}
		if meta.Get([]byte(usageReceiptsWatermarkDoneKey)) != nil && meta.Get([]byte(usageReceiptsWatermarkDoneV2Key)) != nil {
			return nil
		}
		watermark, untrustedThrough, unknownHour, err := computeUsageReceiptWatermark(tx, scanTime)
		if err != nil {
			return err
		}
		if old.After(watermark) {
			watermark = old
		}
		if err := putUsageReceiptWatermark(meta, watermark); err != nil {
			return err
		}
		oldUntrusted, err := decodeUsageReceiptWatermark(meta.Get([]byte(usageReceiptsUntrustedThroughKey)))
		if err != nil {
			return err
		}
		if oldUntrusted.After(untrustedThrough) {
			untrustedThrough = oldUntrusted
		}
		if !untrustedThrough.IsZero() {
			if err := meta.Put([]byte(usageReceiptsUntrustedThroughKey), []byte(untrustedThrough.UTC().Format(time.RFC3339Nano))); err != nil {
				return err
			}
		}
		if unknownHour {
			if err := meta.Put([]byte(usageReceiptsUnknownHourKey), []byte("1")); err != nil {
				return err
			}
		}
		if err := meta.Put([]byte(usageReceiptsWatermarkDoneKey), []byte("1")); err != nil {
			return err
		}
		return meta.Put([]byte(usageReceiptsWatermarkDoneV2Key), []byte("1"))
	})
}

func putUsageReceiptWatermark(meta *bolt.Bucket, watermark time.Time) error {
	if watermark.IsZero() {
		return meta.Put([]byte(usageReceiptsWatermarkKey), nil)
	}
	return meta.Put([]byte(usageReceiptsWatermarkKey), []byte(watermark.UTC().Format(time.RFC3339Nano)))
}

func decodeUsageReceiptWatermark(raw []byte) (time.Time, error) {
	if len(raw) == 0 {
		return time.Time{}, nil
	}
	watermark, err := time.Parse(time.RFC3339Nano, string(raw))
	if err != nil {
		return time.Time{}, fmt.Errorf("usage receipts: invalid legacy watermark: %w", err)
	}
	return watermark.UTC(), nil
}

// computeUsageReceiptWatermark compares retained raw row counts with permanent
// hourly record counts. Any mismatch makes that hour untrustworthy. Aggregate
// dimensions are joined with '|' without escaping, so a client name, model or
// target may itself contain that separator and a valid historical key can have
// more than five segments. Historical cells are matched by their original
// bytes against the legacy projection of each raw row. Versioned cells are
// matched per exact v2 key as a lower bound and also projected into the same
// historical space. Legacy four-segment keys never match a projection and stay
// conservatively untrusted.
//
// v1.0.1 readers skipped undecodable cells and malformed keys, so an upgraded
// ledger may still hold them. A later insertion of an older raw row means the
// earliest surviving row cannot prove that a corrupt hour was never pruned.
// Every recognizable corrupt hour is untrusted. Unattributed keys retain a
// separate unknown-hour diagnostic. Every decision is logged at ERROR with the
// bucket and key, never the value.
//
// The legacy watermark remains capped at the scan hour. Admission also uses
// the separately persisted uncapped boundary; neither proves completeness.
func computeUsageReceiptWatermark(tx *bolt.Tx, scanTime time.Time) (time.Time, time.Time, bool, error) {
	rawLegacy := make(map[string]int64)
	rawV2 := make(map[string]int64)
	rawQuota := make(map[string]int64)
	if raw := tx.Bucket([]byte(BucketUsageEvents)); raw != nil {
		cursor := raw.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			var event usage.Event
			if json.Unmarshal(v, &event) != nil {
				continue
			}
			rawLegacy[string(legacyAggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target))]++
			rawV2[string(aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target))]++
			rawQuota[string(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))]++
		}
	}

	var untrusted time.Time
	mark := func(hour time.Time) {
		if hour.After(untrusted) {
			untrusted = hour
		}
	}

	type corruptEvidence struct {
		bucket, key, reason string
		hour                time.Time
		hourKnown           bool
	}
	var corrupt []corruptEvidence
	corruptHours := make(map[time.Time]bool)
	addCorrupt := func(bucket, key, reason string) {
		hour, ok := untrustedAggHour(key)
		corrupt = append(corrupt, corruptEvidence{bucket: bucket, key: key, reason: reason, hour: hour, hourKnown: ok})
		if ok {
			corruptHours[hour] = true
		}
	}

	// Historical cells are compared by their original bytes; current cells are
	// projected into historical key space. Counts accumulate per key so two
	// different v2 dimensions that share one legacy projection cannot cancel a
	// missing row. The per-v2 lower bound adds the dimension-level check.
	expectedLegacy := make(map[string]int64)
	if bucket := tx.Bucket([]byte(BucketUsageAggHourly)); bucket != nil {
		cursor := bucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			key := string(k)
			entry, err := decodeAggKey(k)
			if err != nil {
				addCorrupt(BucketUsageAggHourly, key, err.Error())
				continue
			}
			var cell AggCell
			if err := json.Unmarshal(v, &cell); err != nil {
				addCorrupt(BucketUsageAggHourly, key, "decode hourly aggregate cell")
				continue
			}
			if strings.HasPrefix(key, "v2|") {
				projected := string(legacyAggKey(entry.hour, entry.endpointType, entry.client, entry.model, entry.target))
				expectedLegacy[projected] += cell.recordCount()
				if rawV2[key] < cell.recordCount() {
					mark(entry.hour)
				}
				continue
			}
			expectedLegacy[key] += cell.recordCount()
			if len(strings.Split(key, "|")) == 4 {
				mark(entry.hour)
			}
		}
	}
	expectedQuota := make(map[string]int64)
	if bucket := tx.Bucket([]byte(BucketQuotaUsageAggHourly)); bucket != nil {
		cursor := bucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			key := string(k)
			if _, err := hourFromAggKey(key); err != nil {
				addCorrupt(BucketQuotaUsageAggHourly, key, "invalid quota aggregate key")
				continue
			}
			var cell AggCell
			if err := json.Unmarshal(v, &cell); err != nil {
				addCorrupt(BucketQuotaUsageAggHourly, key, "decode quota aggregate cell")
				continue
			}
			expectedQuota[key] += cell.recordCount()
		}
	}

	// compare marks a count mismatch. Surplus raw rows in an hour with corrupt
	// evidence are expected (the corrupt cell counts nothing) and say nothing
	// about cleanup, so only the corrupt-evidence rule below decides that hour.
	compare := func(bucket string, raw, expected map[string]int64) {
		check := func(key string) {
			rawCount, want := raw[key], expected[key]
			if rawCount == want {
				return
			}
			hour, err := hourFromAggKey(key)
			if err != nil {
				addCorrupt(bucket, key, "invalid aggregate hour")
				return
			}
			if rawCount > want && corruptHours[hour] {
				return
			}
			mark(hour)
		}
		for key := range raw {
			check(key)
		}
		for key := range expected {
			if _, seen := raw[key]; !seen {
				check(key)
			}
		}
	}
	compare(BucketUsageAggHourly, rawLegacy, expectedLegacy)
	compare(BucketQuotaUsageAggHourly, rawQuota, expectedQuota)

	unknownHour := false
	for _, evidence := range corrupt {
		switch {
		case !evidence.hourKnown:
			unknownHour = true
			slog.Error("usage receipts: aggregate key has no recognizable hour; it cannot be counted or attributed",
				"bucket", evidence.bucket, "key", evidence.key, "reason", evidence.reason)
		default:
			mark(evidence.hour)
			slog.Error("usage receipts: undecodable aggregate evidence; hour treated as untrusted",
				"bucket", evidence.bucket, "key", evidence.key, "hour", evidence.hour.Format(hourLayout), "reason", evidence.reason)
		}
	}

	if untrusted.IsZero() {
		return time.Time{}, time.Time{}, unknownHour, nil
	}
	next := untrusted.Add(time.Hour)
	if !next.After(untrusted) {
		return time.Time{}, time.Time{}, false, fmt.Errorf("usage receipts: legacy watermark overflow")
	}
	untrustedThrough := next
	if limit := scanTime.UTC().Truncate(time.Hour); next.After(limit) {
		slog.Error("usage receipts: legacy watermark capped at the scan hour; untrusted evidence lies in the current or a future hour",
			"untrusted_hour", untrusted.Format(hourLayout), "watermark", limit.Format(time.RFC3339))
		next = limit
	}
	return next.UTC(), untrustedThrough.UTC(), unknownHour, nil
}

// untrustedAggHour recovers the hour a malformed aggregate key claims, so the
// evidence still bounds the watermark. It accepts the hour segment of either
// key format, then any parseable hour prefix of the key.
func untrustedAggHour(key string) (time.Time, bool) {
	text := key
	if rest, ok := strings.CutPrefix(text, "v2|"); ok {
		text = rest
	}
	candidates := []string{text}
	if before, _, found := strings.Cut(text, "|"); found {
		candidates = append(candidates, before)
	}
	if len(text) >= len(hourLayout) {
		candidates = append(candidates, text[:len(hourLayout)])
	}
	for _, candidate := range candidates {
		if hour, err := time.ParseInLocation(hourLayout, candidate, time.UTC); err == nil {
			return hour.UTC(), true
		}
	}
	return time.Time{}, false
}

func hourFromAggKey(key string) (time.Time, error) {
	if len(key) <= len(hourLayout) || key[len(hourLayout)] != '|' {
		return time.Time{}, fmt.Errorf("usage receipts: invalid aggregate key")
	}
	hour, err := time.ParseInLocation(hourLayout, key[:len(hourLayout)], time.UTC)
	if err != nil || hour.Format(hourLayout) != key[:len(hourLayout)] {
		return time.Time{}, fmt.Errorf("usage receipts: invalid aggregate hour")
	}
	return hour, nil
}

func readUsageReceipt(tx *bolt.Tx, key []byte) (usageReceipt, bool, error) {
	bucket := tx.Bucket([]byte(BucketUsageReceipts))
	if bucket == nil {
		return usageReceipt{}, false, fmt.Errorf("usage receipts: bucket %q missing", BucketUsageReceipts)
	}
	data := bucket.Get(key)
	if data == nil {
		return usageReceipt{}, false, nil
	}
	var receipt usageReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return usageReceipt{}, false, fmt.Errorf("usage receipts: decode receipt: %w", err)
	}
	if receipt.Version != usageReceiptVersion {
		return usageReceipt{}, false, fmt.Errorf("usage receipts: unknown receipt version %d", receipt.Version)
	}
	if receipt.Digest == "" {
		return usageReceipt{}, false, fmt.Errorf("usage receipts: empty receipt digest")
	}
	return receipt, true, nil
}

func writeUsageReceipt(tx *bolt.Tx, key []byte, digest string) error {
	bucket := tx.Bucket([]byte(BucketUsageReceipts))
	if bucket == nil {
		return fmt.Errorf("usage receipts: bucket %q missing", BucketUsageReceipts)
	}
	if existing, ok, err := readUsageReceipt(tx, key); err != nil {
		return err
	} else if ok {
		if existing.Digest != digest {
			return fmt.Errorf("usage receipts: receipt already exists with different payload digest")
		}
		return nil
	}
	data, err := json.Marshal(usageReceipt{Version: usageReceiptVersion, Digest: digest})
	if err != nil {
		return err
	}
	return bucket.Put(key, data)
}

// lookupUsageIdentity verifies one row against raw detail, a receipt, or both.
// Raw row bytes are returned so a hit can repair the client index with the
// originally stored bytes instead of a freshly repriced event.
func lookupUsageIdentity(tx *bolt.Tx, key, digest []byte) (receiptState, usage.Event, []byte, error) {
	rawBucket := tx.Bucket([]byte(BucketUsageEvents))
	if rawBucket == nil {
		return identityNew, usage.Event{}, nil, errors.New("usage events bucket missing")
	}
	raw := rawBucket.Get(key)
	owner, hasOwner, err := readRawOwner(tx, key)
	if err != nil {
		return identityNew, usage.Event{}, nil, err
	}
	receipt, hasReceipt, err := readUsageReceipt(tx, key)
	if err != nil {
		return identityNew, usage.Event{}, nil, err
	}
	if raw == nil {
		if hasReceipt {
			if hasOwner && owner.Digest != receipt.Digest {
				return identityNew, usage.Event{}, nil, fmt.Errorf("%w: raw owner and receipt disagree", ErrUsageIdentityUnverified)
			}
			if receiptOwner, known := readReceiptOwner(tx, key); hasOwner && known && receiptOwner != owner.Client {
				return identityNew, usage.Event{}, nil, fmt.Errorf("%w: raw and receipt owners disagree", ErrUsageIdentityUnverified)
			}
			if receipt.Digest != string(digest) {
				return identityNew, usage.Event{}, nil, withUsageKind(ErrUsageRecordConflict, fmt.Errorf("usage record already exists with different event data"))
			}
			return identityReceipt, usage.Event{}, nil, nil
		}
		if hasOwner {
			return identityNew, usage.Event{}, nil, fmt.Errorf("%w: %w", ErrUsageIdentityUnverified, errRawOwnerOrphan)
		}
		if index := tx.Bucket([]byte(BucketUsageEventsByClient)); index != nil {
			if err := index.ForEach(func(_, value []byte) error {
				if bytes.Equal(value, key) {
					return fmt.Errorf("%w: retained index refers to absent identity", ErrUsageIdentityUnverified)
				}
				return nil
			}); err != nil {
				return identityNew, usage.Event{}, nil, err
			}
		}
		return identityNew, usage.Event{}, nil, nil
	}

	var stored usage.Event
	if err := json.Unmarshal(raw, &stored); err != nil {
		return identityNew, usage.Event{}, nil, fmt.Errorf("usage record is unreadable: %w", err)
	}
	storedDigest, err := usage.RetryEquivalentDigest(stored)
	if err != nil {
		return identityNew, usage.Event{}, nil, err
	}
	if hasOwner && (owner.Digest != storedDigest || owner.Client != receiptClientHash(stored.ClientName)) {
		return identityNew, usage.Event{}, nil, fmt.Errorf("%w: raw owner and event disagree", ErrUsageIdentityUnverified)
	}
	if storedDigest != string(digest) {
		return identityNew, usage.Event{}, nil, withUsageKind(ErrUsageRecordConflict, fmt.Errorf("usage record ID %q already exists with different event data", stored.RecordID))
	}
	if hasReceipt {
		if receipt.Digest != storedDigest {
			return identityNew, usage.Event{}, nil, fmt.Errorf("usage receipt and raw record disagree")
		}
		return identityRawAndReceipt, stored, append([]byte(nil), raw...), nil
	}
	return identityRaw, stored, append([]byte(nil), raw...), nil
}

// checkUsageIdentityAdmission applies the same conservative bounds to lookups
// and writes, after the whole bundle has been checked for committed identities.
func checkUsageIdentityAdmission(tx *bolt.Tx, rows []usage.Event) error {
	return checkUsageIdentityAdmissionAt(tx, rows, nil)
}

// A lookup can evaluate pending migration in its read transaction without
// publishing a marker. The write path still requires a completed migration.
func checkUsageIdentityAdmissionAt(tx *bolt.Tx, rows []usage.Event, scanTime *time.Time) error {
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil {
		return errors.New("usage receipts: meta bucket missing")
	}
	done := meta.Get([]byte(usageReceiptsWatermarkDoneKey)) != nil && meta.Get([]byte(usageReceiptsWatermarkDoneV2Key)) != nil
	if !done && scanTime == nil {
		return errors.New("usage receipts: legacy watermark was not established")
	}
	if meta.Get([]byte(usageReceiptsUnknownHourKey)) != nil {
		return fmt.Errorf("%w: aggregate or receipt evidence has an unknown time range", ErrUsageIdentityUnverified)
	}
	watermark, err := decodeUsageReceiptWatermark(meta.Get([]byte(usageReceiptsWatermarkKey)))
	if err != nil {
		return err
	}
	untrusted, err := decodeUsageReceiptWatermark(meta.Get([]byte(usageReceiptsUntrustedThroughKey)))
	if err != nil {
		return err
	}
	if !done {
		newWatermark, newUntrusted, unknownHour, err := computeUsageReceiptWatermark(tx, *scanTime)
		if err != nil {
			return err
		}
		if unknownHour {
			return fmt.Errorf("%w: aggregate or receipt evidence has an unknown time range", ErrUsageIdentityUnverified)
		}
		if newWatermark.After(watermark) {
			watermark = newWatermark
		}
		if newUntrusted.After(untrusted) {
			untrusted = newUntrusted
		}
	}
	if untrusted.After(watermark) {
		watermark = untrusted
	}
	for _, row := range rows {
		if !watermark.IsZero() && row.Timestamp.Before(watermark) {
			return fmt.Errorf("%w: record %q before %s", ErrUsageIdentityUnverified, row.RecordID, watermark.Format(time.RFC3339Nano))
		}
	}
	return nil
}

// A digest conflict is safe to ignore only when every bundle identity exists.
func classifyUsageConflict(tx *bolt.Tx, rows []usage.Event, keys []string, conflict error) error {
	partialOwner := func(err error) bool {
		if len(rows) <= 1 || !errors.Is(err, errRawOwnerOrphan) {
			return false
		}
		raw := tx.Bucket([]byte(BucketUsageEvents))
		receipts := tx.Bucket([]byte(BucketUsageReceipts))
		for _, key := range keys {
			if (raw != nil && raw.Get([]byte(key)) != nil) || (receipts != nil && receipts.Get([]byte(key)) != nil) {
				return true
			}
		}
		return false
	}
	if partialOwner(conflict) {
		return withUsageKind(ErrUsageBundleInvalid, ErrUsageBundlePartial)
	}
	if !errors.Is(conflict, ErrUsageRecordConflict) {
		return conflict
	}
	for i, row := range rows {
		digest, err := usage.RetryEquivalentDigest(row)
		if err != nil {
			return err
		}
		state, _, _, err := lookupUsageIdentity(tx, []byte(keys[i]), []byte(digest))
		if partialOwner(err) {
			return withUsageKind(ErrUsageBundleInvalid, ErrUsageBundlePartial)
		}
		if err != nil && !errors.Is(err, ErrUsageRecordConflict) {
			return err
		}
		if errors.Is(err, ErrUsageRecordConflict) {
			raw := tx.Bucket([]byte(BucketUsageEvents)).Get([]byte(keys[i]))
			var stored usage.Event
			if raw == nil || json.Unmarshal(raw, &stored) != nil || strings.TrimSpace(row.ClientName) == "" || strings.TrimSpace(stored.ClientName) != strings.TrimSpace(row.ClientName) {
				return fmt.Errorf("%w: conflicting identity has unverified client ownership", ErrUsageIdentityUnverified)
			}
			storedDigest, digestErr := usage.RetryEquivalentDigest(stored)
			if digestErr != nil {
				return digestErr
			}
			if _, _, _, verifyErr := lookupUsageIdentity(tx, []byte(keys[i]), []byte(storedDigest)); verifyErr != nil {
				return verifyErr
			}
		}
		if err == nil && state == identityNew {
			return withUsageKind(ErrUsageBundleInvalid, ErrUsageBundlePartial)
		}
	}
	return conflict
}

// inspectUsageBundleTx makes the batch decision before any ledger row is written.
func inspectUsageBundleTx(tx *bolt.Tx, rows []usage.Event, keys []string, migrationErr error) (bool, error) {
	quarantine := tx.Bucket([]byte(BucketUsageEventsQuarantine))
	committed := 0
	for i, row := range rows {
		if quarantine != nil && quarantine.Get([]byte(keys[i])) != nil {
			return false, ErrUsageClientIndexIncomplete
		}
		digest, err := usage.RetryEquivalentDigest(row)
		if err != nil {
			return false, err
		}
		state, _, _, err := lookupUsageIdentity(tx, []byte(keys[i]), []byte(digest))
		if err != nil {
			return false, classifyUsageConflict(tx, rows, keys, err)
		}
		if state != identityNew {
			committed++
		}
	}
	if committed == len(rows) {
		return true, nil
	}
	if committed != 0 {
		return false, withUsageKind(ErrUsageBundleInvalid, ErrUsageBundlePartial)
	}
	if migrationErr != nil {
		return false, migrationErr
	}
	return false, checkUsageIdentityAdmission(tx, rows)
}

// recordUsageBundleTx commits every expanded row of one request bundle in the
// caller's write transaction. A complete retry is suppressed; a partial bundle
// is rejected so it cannot be resurrected or charged twice.
func recordUsageBundleTx(tx *bolt.Tx, rows []usage.Event, keys []string, migrationErr error) (bool, error) {
	if len(rows) == 0 || len(rows) != len(keys) {
		return false, errors.New("usage: invalid bundle")
	}
	quarantine := tx.Bucket([]byte(BucketUsageEventsQuarantine))
	for _, key := range keys {
		if quarantine != nil && quarantine.Get([]byte(key)) != nil {
			return false, fmt.Errorf("%w: usage record is quarantined pending reconciliation", ErrUsageClientIndexIncomplete)
		}
	}

	committed := 0
	states := make([]receiptState, len(rows))
	storedEvents := make([]usage.Event, len(rows))
	storedRaw := make([][]byte, len(rows))
	for i := range rows {
		digest, err := usage.RetryEquivalentDigest(rows[i])
		if err != nil {
			return false, err
		}
		state, stored, raw, err := lookupUsageIdentity(tx, []byte(keys[i]), []byte(digest))
		if err != nil {
			return false, classifyUsageConflict(tx, rows, keys, err)
		}
		states[i] = state
		storedEvents[i] = stored
		storedRaw[i] = raw
		if state != identityNew {
			committed++
		}
	}
	if committed == len(rows) {
		for i, state := range states {
			if state == identityRaw || state == identityRawAndReceipt {
				if err := addClientEventIndex(tx, storedEvents[i], []byte(keys[i]), storedRaw[i]); err != nil {
					return false, err
				}
			}
		}
		return false, nil
	}
	if committed > 0 {
		return false, withUsageKind(ErrUsageBundleInvalid, ErrUsageBundlePartial)
	}

	if migrationErr != nil {
		return false, migrationErr
	}
	if err := checkUsageIdentityAdmission(tx, rows); err != nil {
		return false, err
	}

	for i := range rows {
		if err := writeUsageRowTx(tx, rows[i], keys[i]); err != nil {
			return false, err
		}
	}
	return true, nil
}

// writeUsageRowTx appends one new row and its derived data. Callers have
// already checked that the identity has neither raw data nor a receipt.
func writeUsageRowTx(tx *bolt.Tx, event usage.Event, key string) error {
	b := tx.Bucket([]byte(BucketUsageEvents))
	usageKey := aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
	quotaKey := quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
	usageProof, usageProven := readRollupProofValue(tx, BucketUsageAggHourly, usageKey)
	quotaProof, quotaProven := readRollupProofValue(tx, BucketQuotaUsageAggHourly, quotaKey)
	if !usageProven {
		if err := invalidateRollupProof(tx, BucketUsageAggHourly, usageKey); err != nil {
			return err
		}
	}
	if !quotaProven {
		if err := invalidateRollupProof(tx, BucketQuotaUsageAggHourly, quotaKey); err != nil {
			return err
		}
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal usage event: %w", err)
	}
	if err := b.Put([]byte(key), data); err != nil {
		return err
	}
	if err := writeRawOwner(tx, []byte(key), event); err != nil {
		return err
	}
	if err := bumpAgg(tx, event); err != nil {
		return err
	}
	if err := bumpQuotaAgg(tx, event); err != nil {
		return err
	}
	if usageProven {
		if err := advanceRollupProof(tx, BucketUsageAggHourly, usageKey, usageProof, event, []byte(key)); err != nil {
			return err
		}
	}
	if quotaProven {
		if err := advanceRollupProof(tx, BucketQuotaUsageAggHourly, quotaKey, quotaProof, event, []byte(key)); err != nil {
			return err
		}
	}
	if err := addClientEventIndex(tx, event, []byte(key), data); err != nil {
		return err
	}
	return bumpRawRevision(tx)
}
