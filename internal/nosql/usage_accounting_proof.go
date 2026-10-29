package nosql

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

const usageAccountingProofTrackedKey = "usage_accounting_proof_tracked_v1"

// A missing raw row and receipt cannot contribute to a positive proof. Avoid
// a full index search in lookupUsageIdentity for that negative case.
func proofIdentitiesPresent(ctx context.Context, tx *bolt.Tx, keys []string) bool {
	raw := tx.Bucket([]byte(BucketUsageEvents))
	receipts := tx.Bucket([]byte(BucketUsageReceipts))
	if raw == nil || receipts == nil {
		return false
	}
	for _, key := range keys {
		if ctx.Err() != nil || raw.Get([]byte(key)) == nil && receipts.Get([]byte(key)) == nil {
			return false
		}
	}
	return true
}

func quarantineOutsideProofContext(ctx context.Context, tx *bolt.Tx, owners map[string]bool, hours map[time.Time]bool) bool {
	b := tx.Bucket([]byte(BucketUsageEventsQuarantine))
	meta := tx.Bucket([]byte(BucketMeta))
	if b == nil || meta == nil {
		return false
	}
	var count uint64
	err := b.ForEach(func(key, data []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		var record usageQuarantineRecord
		if json.Unmarshal(data, &record) != nil {
			return ErrUsageHistoryIncomplete
		}
		owner, known, err := readRawOwner(tx, key)
		if err != nil || !known {
			return ErrUsageHistoryIncomplete
		}
		scope, ambiguous := backfillRawScope(record.RawValue)
		if ambiguous && json.Valid(record.RawValue) || (!ambiguous && scope.ClientName != "" && receiptClientHash(scope.ClientName) != owner.Client) {
			return ErrUsageHistoryIncomplete
		}
		if owners[owner.Client] {
			stamp, _, found := strings.Cut(string(key), "_")
			at, err := time.Parse(time.RFC3339Nano, stamp)
			if !found || err != nil || hours[at.UTC().Truncate(time.Hour)] {
				return ErrUsageHistoryIncomplete
			}
		}
		return nil
	})
	if err != nil {
		return false
	}
	rawCount := meta.Get([]byte(usageQuarantineCountKey))
	return (rawCount == nil && count == 0 || len(rawCount) == 8 && binary.BigEndian.Uint64(rawCount) == count) && ctx.Err() == nil
}

// A retained live identity cannot be covered by an absent, invalid, or smaller
// quota cell. Legacy aggregate formats remain an identity-only result.
func knownUnverifiedLiveAccounting(tx *bolt.Tx, rows []usage.Event, keys []string) bool {
	meta := tx.Bucket([]byte(BucketMeta))
	raw := tx.Bucket([]byte(BucketUsageEvents))
	quota := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
	usageAgg := tx.Bucket([]byte(BucketUsageAggHourly))
	if meta == nil || raw == nil || quota == nil || usageAgg == nil || len(rows) == 0 || len(rows) != len(keys) || checkUsageIdentityAdmission(tx, rows) != nil {
		return false
	}
	for _, source := range []string{BucketUsageAggHourly, BucketQuotaUsageAggHourly} {
		if meta.Get([]byte(usageAggregateSourceMissingPrefix+source)) != nil {
			return false
		}
	}
	for _, key := range keys {
		var event usage.Event
		if !decodeProofEvent(raw.Get([]byte(key)), &event) {
			continue
		}
		if usageAgg.Get(aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)) == nil {
			continue
		}
		data := quota.Get(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
		var cell AggCell
		if data == nil || bytes.Equal(bytes.TrimSpace(data), []byte("null")) || json.Unmarshal(data, &cell) != nil {
			return true
		}
		if (event.InputTokens > 0 && cell.InputTokens < event.InputTokens) || (event.OutputTokens > 0 && cell.OutputTokens < event.OutputTokens) || (event.CachedTokens > 0 && cell.CachedTokens < event.CachedTokens) || (event.CacheCreationTokens > 0 && cell.CacheCreationTokens < event.CacheCreationTokens) || (event.ImageInputTokens > 0 && cell.ImageInputTokens < event.ImageInputTokens) || (event.ImageOutputTokens > 0 && cell.ImageOutputTokens < event.ImageOutputTokens) {
			return true
		}
	}
	return false
}

// Identity evidence alone cannot prove that either billing rollup survived.
// This check is deliberately conservative when raw history may be incomplete.
func usageAccountingProven(tx *bolt.Tx, rows []usage.Event, keys []string) bool {
	proven, _ := usageAccountingProvenContext(context.Background(), tx, rows, keys)
	return proven
}

func usageAccountingProvenContext(ctx context.Context, tx *bolt.Tx, rows []usage.Event, keys []string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	proven := usageAccountingProvenImpl(ctx, tx, rows, keys)
	return proven, ctx.Err()
}

func usageAccountingProvenImpl(ctx context.Context, tx *bolt.Tx, rows []usage.Event, keys []string) bool {
	if len(rows) == 0 || len(rows) != len(keys) {
		return false
	}
	proven, err := usageAccountingRowsProvenContext(ctx, tx, rows, keys)
	if err != nil || !proven {
		return false
	}
	if rows[0].Supplementary || rows[0].ParentRecordID != "" {
		return true
	}
	raw := tx.Bucket([]byte(BucketUsageEvents))
	if raw == nil {
		return false
	}
	stored := raw.Get([]byte(keys[0]))
	if stored == nil {
		digest, err := usage.RetryEquivalentDigest(rows[0])
		if err != nil {
			return false
		}
		state, _, _, err := lookupUsageIdentity(tx, []byte(keys[0]), []byte(digest))
		return err == nil && state == identityReceipt
	}
	var root usage.Event
	if !decodeProofEvent(stored, &root) || root.Supplementary || root.ParentRecordID != "" || usageKey(root.Timestamp, root.RecordID) != keys[0] || strings.TrimSpace(root.ClientName) != strings.TrimSpace(rows[0].ClientName) {
		return false
	}
	committedRows, committedKeys, err := normalizeUsageBundle(root)
	if err != nil || len(committedKeys) == 0 || committedKeys[0] != keys[0] {
		return false
	}
	if !proofIdentitiesPresent(ctx, tx, committedKeys) {
		return false
	}
	committed, err := inspectUsageBundleTx(tx, committedRows, committedKeys, nil)
	if err != nil || !committed {
		return false
	}
	proven, _ = usageAccountingRowsProvenContext(ctx, tx, committedRows, committedKeys)
	return proven
}

func usageAccountingRowsProven(tx *bolt.Tx, rows []usage.Event, keys []string) bool {
	proven, _ := usageAccountingRowsProvenContext(context.Background(), tx, rows, keys)
	return proven
}

func usageAccountingRowsProvenContext(ctx context.Context, tx *bolt.Tx, rows []usage.Event, keys []string) (bool, error) {
	retained, err := usageRetainedRollupsProvenContext(ctx, tx, rows, keys)
	if err != nil {
		return false, err
	}
	if retained {
		return true, nil
	}
	return usageAccountingRawProvenContext(ctx, tx, rows, keys)
}

func proofRawMayOverlap(tx *bolt.Tx, key, data []byte, owners map[string]bool, hours map[time.Time]bool) bool {
	owner, known, err := readRawOwner(tx, key)
	scope := backfillRawScopeCached(data)
	ambiguous := scope.ambiguous
	if err != nil || (ambiguous && scope.valid) || (known && !ambiguous && scope.clientName != "" && scope.clientHash != owner.Client) {
		return true
	}
	if err == nil && known && !owners[owner.Client] {
		return false
	}
	if !known && !ambiguous && scope.clientName != "" && !owners[scope.clientHash] {
		return false
	}
	stamp, _, found := strings.Cut(string(key), "_")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if !found || err != nil {
		if !ambiguous && !scope.timestamp.IsZero() {
			at, found, err = scope.timestamp, true, nil
		}
	}
	if found && err == nil && !hours[at.UTC().Truncate(time.Hour)] {
		return false
	}
	return true
}

func usageAccountingRawProven(tx *bolt.Tx, rows []usage.Event, keys []string) bool {
	proven, _ := usageAccountingRawProvenContext(context.Background(), tx, rows, keys)
	return proven
}

func usageAccountingRawProvenContext(ctx context.Context, tx *bolt.Tx, rows []usage.Event, keys []string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	proven := usageAccountingRawProvenImpl(ctx, tx, rows, keys)
	return proven, ctx.Err()
}

func usageAccountingRawProvenImpl(ctx context.Context, tx *bolt.Tx, rows []usage.Event, keys []string) bool {
	if ctx.Err() != nil {
		return false
	}
	meta := tx.Bucket([]byte(BucketMeta))
	raw := tx.Bucket([]byte(BucketUsageEvents))
	quota := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
	usageAgg := tx.Bucket([]byte(BucketUsageAggHourly))
	quarantine := tx.Bucket([]byte(BucketUsageEventsQuarantine))
	receipts := tx.Bucket([]byte(BucketUsageReceipts))
	index := tx.Bucket([]byte(BucketUsageEventsByClient))
	clientHours := tx.Bucket([]byte(BucketUsageClientHours))
	if meta == nil || raw == nil || quota == nil || usageAgg == nil || quarantine == nil || receipts == nil || index == nil || clientHours == nil || len(rows) == 0 || len(rows) != len(keys) {
		return false
	}
	tracked := meta.Get([]byte(usageAccountingProofTrackedKey))
	if tracked != nil && string(tracked) != "1" {
		return false
	}
	if tracked == nil && !validFreshAttestation(tx, rows, keys) {
		return false
	}
	for _, source := range []string{BucketUsageAggHourly, BucketQuotaUsageAggHourly} {
		if meta.Get([]byte(usageAggregateSourceMissingPrefix+source)) != nil {
			return false
		}
	}
	usageKeys, quotaKeys := make(map[string]AggCell), make(map[string]AggCell)
	hours := make(map[time.Time]bool)
	owners := make(map[string]bool)
	expectedHours := make(map[string]usageClientHour)
	indexPrefixes := make(map[string][]byte)
	storedRows := make([]usage.Event, 0, len(rows))
	for _, key := range keys {
		if ctx.Err() != nil {
			return false
		}
		data := raw.Get([]byte(key))
		var stored usage.Event
		if !decodeProofEvent(data, &stored) || strings.EqualFold(strings.TrimSpace(stored.ClientName), "unknown") || usageKey(stored.Timestamp, stored.RecordID) != key {
			return false
		}
		digest, err := usage.RetryEquivalentDigest(stored)
		if err != nil {
			return false
		}
		state, _, _, err := lookupUsageIdentity(tx, []byte(key), []byte(digest))
		if err != nil || (state != identityRaw && state != identityRawAndReceipt) {
			return false
		}
		storedRows = append(storedRows, stored)
		owners[receiptClientHash(stored.ClientName)] = true
		usageKeys[string(aggKey(stored.Timestamp, stored.EndpointType, stored.ClientName, stored.Model, stored.Target))] = AggCell{}
		quotaKeys[string(quotaAggKey(stored.Timestamp, stored.ClientName, stored.Target, stored.Model))] = AggCell{}
		if usageAgg.Get(legacyAggKey(stored.Timestamp, stored.EndpointType, stored.ClientName, stored.Model, stored.Target)) != nil {
			return false
		}
		hours[stored.Timestamp.UTC().Truncate(time.Hour)] = true
		hourKey := string(clientHourKey(stored.ClientName, stored.Timestamp))
		expectedHours[hourKey] = usageClientHour{}
		indexPrefixes[hourKey] = []byte(clientIndexPrefix(stored.ClientName) + stored.Timestamp.UTC().Truncate(time.Hour).Format(hourLayout))
	}
	if !quarantineOutsideProofContext(ctx, tx, owners, hours) {
		return false
	}
	if err := checkUsageIdentityAdmission(tx, storedRows); err != nil {
		return false
	}
	if err := forEachReceiptInHours(ctx, tx, receipts, hours, func(k, _ []byte) error {
		if err := proofScanStep(ctx, true); err != nil {
			return err
		}
		if keyHourOutside(k, hours) || !receiptMayOverlap(tx, k, owners, hours) {
			return nil
		}
		usageKey, quotaKey, ok := readReceiptCells(tx, k)
		if !ok {
			return ErrUsageHistoryIncomplete
		}
		_, usageRelevant := usageKeys[string(usageKey)]
		_, quotaRelevant := quotaKeys[string(quotaKey)]
		if usageRelevant || quotaRelevant {
			return ErrUsageHistoryIncomplete
		}
		return nil
	}); err != nil {
		return false
	}
	if err := forEachRawInHours(ctx, tx, raw, hours, func(k, data []byte) error {
		if err := proofScanStep(ctx, false); err != nil {
			return err
		}
		if canonicalRowOutsideHours(k, data, hours) {
			return nil
		}
		var event usage.Event
		if !decodeProofEventCached(data, &event) || usageKey(event.Timestamp, event.RecordID) != string(k) {
			if !proofRawMayOverlap(tx, k, data, owners, hours) {
				return nil
			}
			return ErrUsageHistoryIncomplete
		}
		hourKey := string(clientHourKey(event.ClientName, event.Timestamp))
		if state, ok := expectedHours[hourKey]; ok {
			indexKey := clientEventIndexKey(event, k)
			if !bytes.Equal(index.Get(indexKey), k) {
				return ErrUsageHistoryIncomplete
			}
			state.Count++
			xorDigest(&state.Digest, rawDigest(k, data))
			expectedHours[hourKey] = state
		}
		uk := string(aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target))
		if cell, ok := usageKeys[uk]; ok {
			accumulateAggCell(&cell, event)
			usageKeys[uk] = cell
		}
		qk := string(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
		if cell, ok := quotaKeys[qk]; ok {
			accumulateAggCell(&cell, event)
			quotaKeys[qk] = cell
		}
		return nil
	}); err != nil {
		return false
	}
	for key, expected := range expectedHours {
		if ctx.Err() != nil {
			return false
		}
		actual, err := loadClientHour(tx, []byte(key))
		if err != nil || actual.Count != expected.Count || actual.Digest != expected.Digest {
			return false
		}
		prefix := indexPrefixes[key]
		cursor := index.Cursor()
		var count uint64
		for k, rawKey := cursor.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, rawKey = cursor.Next() {
			if ctx.Err() != nil {
				return false
			}
			data := raw.Get(rawKey)
			var event usage.Event
			if !decodeProofEvent(data, &event) || !bytes.Equal(clientEventIndexKey(event, rawKey), k) {
				return false
			}
			count++
		}
		if count != expected.Count {
			return false
		}
	}
	for key, cell := range usageKeys {
		if ctx.Err() != nil {
			return false
		}
		encoded, err := json.Marshal(cell)
		if err != nil || !bytes.Equal(usageAgg.Get([]byte(key)), encoded) {
			return false
		}
	}
	for key, cell := range quotaKeys {
		if ctx.Err() != nil {
			return false
		}
		encoded, err := json.Marshal(cell)
		if err != nil || !bytes.Equal(quota.Get([]byte(key)), encoded) {
			return false
		}
	}
	return true
}

func decodeProofEvent(data []byte, event *usage.Event) bool {
	proofRawParses.Add(1)
	return decodeProofEventUncounted(data, event)
}

// decodeProofEventUncounted is decodeProofEvent without the parse counter; the
// integrity classifier uses it so it never perturbs scan-count assertions.
func decodeProofEventUncounted(data []byte, event *usage.Event) bool {
	if !json.Valid(data) || !utf8.Valid(data) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(event) != nil || event.Timestamp.IsZero() || strings.TrimSpace(event.RecordID) == "" || strings.TrimSpace(event.ClientName) == "" {
		return false
	}
	canonical, err := json.Marshal(event)
	return err == nil && bytes.Equal(data, canonical)
}
