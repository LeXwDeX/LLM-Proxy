package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
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
	BucketUsageReceipts           = "usage_record_receipts_v1"
	usageReceiptsWatermarkDoneKey = "usage_receipts_watermark_done_v1"
	usageReceiptsWatermarkKey     = "usage_receipts_legacy_unknown_before_v1"
	usageReceiptVersion           = 1
)

// ErrUsageIdentityUnverified reports an explicit identity that predates
// receipt tracking and has no retained raw record. It cannot be proven either
// way, so recovery must leave it queued instead of creating a new charge.
var ErrUsageIdentityUnverified = errors.New("usage identity predates receipt tracking and cannot be verified")

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
// permanent hourly record counts disagree. An explicit identity in an older
// hour with neither raw data nor a receipt cannot be proven committed or new,
// so it is rejected. This scan runs once, not per request.
func (s *UsageStore) EnsureUsageReceipts() error {
	if s == nil || s.db == nil {
		return errors.New("usage receipts: store is not initialized")
	}
	s.receiptMu.Lock()
	defer s.receiptMu.Unlock()
	if s.receiptsReady {
		return nil
	}

	var watermark time.Time
	done := false
	if err := s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketUsageReceipts)) == nil {
			return fmt.Errorf("usage receipts: bucket %q missing", BucketUsageReceipts)
		}
		meta := tx.Bucket([]byte(BucketMeta))
		if meta == nil {
			return errors.New("usage receipts: meta bucket missing")
		}
		if meta.Get([]byte(usageReceiptsWatermarkDoneKey)) != nil {
			done = true
			var err error
			watermark, err = decodeUsageReceiptWatermark(meta.Get([]byte(usageReceiptsWatermarkKey)))
			return err
		}
		var err error
		watermark, err = computeUsageReceiptWatermark(tx)
		return err
	}); err != nil {
		return err
	}

	if !done {
		if err := s.db.Update(func(tx *bolt.Tx) error {
			meta := tx.Bucket([]byte(BucketMeta))
			if meta == nil {
				return errors.New("usage receipts: meta bucket missing")
			}
			if meta.Get([]byte(usageReceiptsWatermarkDoneKey)) != nil {
				return nil
			}
			if err := putUsageReceiptWatermark(meta, watermark); err != nil {
				return err
			}
			return meta.Put([]byte(usageReceiptsWatermarkDoneKey), []byte("1"))
		}); err != nil {
			return err
		}
	}
	s.receiptsReady = true
	return nil
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
// hourly record counts. Any mismatch makes that hour untrustworthy. Legacy
// four-segment aggregate keys carry no target and cannot be matched row by
// row, so their hours are conservatively untrusted.
func computeUsageReceiptWatermark(tx *bolt.Tx) (time.Time, error) {
	rawAgg := make(map[string]int64)
	rawQuota := make(map[string]int64)
	if raw := tx.Bucket([]byte(BucketUsageEvents)); raw != nil {
		cursor := raw.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			var event usage.Event
			if json.Unmarshal(v, &event) != nil {
				continue
			}
			rawAgg[string(aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target))]++
			rawQuota[string(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))]++
		}
	}

	var untrusted time.Time
	mark := func(hour time.Time) {
		if hour.After(untrusted) {
			untrusted = hour
		}
	}

	if bucket := tx.Bucket([]byte(BucketUsageAggHourly)); bucket != nil {
		cursor := bucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			hour, endpointType, client, model, target, ok := parseAggKey(k)
			if !ok {
				return time.Time{}, fmt.Errorf("usage receipts: invalid hourly aggregate key")
			}
			var cell AggCell
			if err := json.Unmarshal(v, &cell); err != nil {
				return time.Time{}, fmt.Errorf("usage receipts: decode hourly aggregate cell: %w", err)
			}
			key := string(aggKey(hour, endpointType, client, model, target))
			legacy := len(strings.Split(string(k), "|")) == 4 || target == ""
			if legacy || rawAgg[key] != cell.recordCount() {
				mark(hour)
			}
			delete(rawAgg, key)
		}
	}
	for key := range rawAgg {
		hour, err := hourFromAggKey(key)
		if err != nil {
			return time.Time{}, err
		}
		mark(hour)
	}

	if bucket := tx.Bucket([]byte(BucketQuotaUsageAggHourly)); bucket != nil {
		cursor := bucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			parts := strings.SplitN(string(k), "|", 4)
			if len(parts) != 4 {
				return time.Time{}, fmt.Errorf("usage receipts: invalid quota aggregate key")
			}
			hour, err := time.ParseInLocation(hourLayout, parts[0], time.UTC)
			if err != nil {
				return time.Time{}, fmt.Errorf("usage receipts: parse quota aggregate hour: %w", err)
			}
			var cell AggCell
			if err := json.Unmarshal(v, &cell); err != nil {
				return time.Time{}, fmt.Errorf("usage receipts: decode quota aggregate cell: %w", err)
			}
			if rawQuota[string(k)] != cell.recordCount() {
				mark(hour)
			}
			delete(rawQuota, string(k))
		}
	}
	for key := range rawQuota {
		parts := strings.SplitN(key, "|", 4)
		if len(parts) != 4 {
			return time.Time{}, fmt.Errorf("usage receipts: invalid raw quota aggregate key")
		}
		hour, err := time.ParseInLocation(hourLayout, parts[0], time.UTC)
		if err != nil {
			return time.Time{}, fmt.Errorf("usage receipts: parse raw quota aggregate hour: %w", err)
		}
		mark(hour)
	}

	if untrusted.IsZero() {
		return time.Time{}, nil
	}
	return untrusted.Add(time.Hour).UTC(), nil
}

func hourFromAggKey(key string) (time.Time, error) {
	if len(key) < len(hourLayout) {
		return time.Time{}, fmt.Errorf("usage receipts: invalid aggregate key")
	}
	hour, err := time.ParseInLocation(hourLayout, key[:len(hourLayout)], time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("usage receipts: parse aggregate hour: %w", err)
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
	receipt, hasReceipt, err := readUsageReceipt(tx, key)
	if err != nil {
		return identityNew, usage.Event{}, nil, err
	}
	if raw == nil {
		if hasReceipt {
			if receipt.Digest != string(digest) {
				return identityNew, usage.Event{}, nil, fmt.Errorf("usage record already exists with different event data")
			}
			return identityReceipt, usage.Event{}, nil, nil
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
	if storedDigest != string(digest) {
		return identityNew, usage.Event{}, nil, fmt.Errorf("usage record ID %q already exists with different event data", stored.RecordID)
	}
	if hasReceipt {
		if receipt.Digest != storedDigest {
			return identityNew, usage.Event{}, nil, fmt.Errorf("usage receipt and raw record disagree")
		}
		return identityRawAndReceipt, stored, append([]byte(nil), raw...), nil
	}
	return identityRaw, stored, append([]byte(nil), raw...), nil
}

// recordUsageBundleTx commits every expanded row of one request bundle in the
// caller's write transaction. Any already-committed row suppresses the whole
// bundle so a pruned record is never resurrected or charged twice.
func recordUsageBundleTx(tx *bolt.Tx, rows []usage.Event, keys []string, explicit bool) (bool, error) {
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
			return false, err
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
		return false, fmt.Errorf("usage bundle is partially committed and cannot be proven complete")
	}

	if explicit {
		meta := tx.Bucket([]byte(BucketMeta))
		if meta == nil {
			return false, errors.New("usage receipts: meta bucket missing")
		}
		if meta.Get([]byte(usageReceiptsWatermarkDoneKey)) == nil {
			return false, errors.New("usage receipts: legacy watermark was not established")
		}
		watermark, err := decodeUsageReceiptWatermark(meta.Get([]byte(usageReceiptsWatermarkKey)))
		if err != nil {
			return false, err
		}
		if !watermark.IsZero() {
			for _, row := range rows {
				if row.Timestamp.Before(watermark) {
					return false, fmt.Errorf("%w: record %q before %s", ErrUsageIdentityUnverified, row.RecordID, watermark.Format(time.RFC3339Nano))
				}
			}
		}
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
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal usage event: %w", err)
	}
	if err := b.Put([]byte(key), data); err != nil {
		return err
	}
	if err := bumpAgg(tx, event); err != nil {
		return err
	}
	if err := bumpQuotaAgg(tx, event); err != nil {
		return err
	}
	if err := addClientEventIndex(tx, event, []byte(key), data); err != nil {
		return err
	}
	return bumpRawRevision(tx)
}
