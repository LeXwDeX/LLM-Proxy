package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

func boundRawOutsideHour(key []byte, from, to time.Time) bool {
	stamp, id, found := strings.Cut(string(key), "_")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	parsed, idErr := uuid.Parse(id)
	if !found || err != nil || idErr != nil || parsed.String() != id || usageKey(at, id) != string(key) {
		return false
	}
	hour := at.UTC().Truncate(time.Hour)
	return hour.Before(from.UTC().Truncate(time.Hour)) || hour.After(to.UTC().Truncate(time.Hour))
}

// Counters count visited scan rows, including nested rollup proof scans.
type directIntegrityHooks struct {
	call                     func(time.Time, time.Time)
	raw, receipt, quota      func()
	proofCell, proofIdentity func()
	// windowKeys：末尾的额度聚合键检查只遍历 [from, to] 小时范围（额度校准路径）。
	windowKeys bool
}

// Check only evidence whose quota dimensions are available in this snapshot.
func checkDirectQuotaEvidence(tx *bolt.Tx, filter usage.Filter, from, to time.Time, allow costPairFilter, hooks *directIntegrityHooks) error {
	return checkDirectQuotaEvidenceContext(context.Background(), tx, filter, from, to, allow, hooks)
}

func checkDirectQuotaEvidenceContext(ctx context.Context, tx *bolt.Tx, filter usage.Filter, from, to time.Time, allow costPairFilter, hooks *directIntegrityHooks) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if hooks != nil && hooks.call != nil {
		hooks.call(from, to)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	quota := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
	if quota == nil {
		return nil
	}
	// Issue #125: with a valid classification the walks below visit only the
	// keys of [from, to] plus the anomalous objects; each skipped object is one
	// whose branch below returns nil without observable side effects (see the
	// predicates in usage_integrity_class.go). The verdict is unchanged.
	ctx = integrityReadContext(ctx, tx, from, to)
	fast := integrityFastFrom(ctx, tx)
	rawCounts := make(map[string]int64)
	rawCells := make(map[string]AggCell)
	rawPartitions := make(map[string]costPartitionCounts)
	usageCells := make(map[string]AggCell)
	usageQuotaKeys := make(map[string]map[string]bool)
	var proofBatch map[string]*quotaProofBatchCell
	proofsLoaded := false
	proofValid := func(key []byte) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if _, ok := readRollupProofValue(tx, BucketQuotaUsageAggHourly, key); !ok {
			return false, ctx.Err()
		}
		if !proofsLoaded {
			var err error
			proofBatch, err = quotaProofBatchInScopeContext(ctx, tx, filter, from, to, allow, hooks)
			if err != nil {
				return false, err
			}
			proofsLoaded = true
		}
		return proofBatch[string(key)].valid(), ctx.Err()
	}
	owners, err := rawOwnerSource(tx)
	if err != nil {
		return err
	}
	if owners != nil {
		// Skipped owners: valid (else in A) with a canonical key whose hour is
		// outside the window, so the hour test below returns nil.
		if err := integrityEachEvent(fast, owners, func(key, _ []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			owner, known, err := readRawOwner(tx, key)
			if err != nil || !known {
				return fmt.Errorf("%w: invalid raw owner", ErrUsageHistoryIncomplete)
			}
			raw, receipts, quarantine := tx.Bucket([]byte(BucketUsageEvents)), tx.Bucket([]byte(BucketUsageReceipts)), tx.Bucket([]byte(BucketUsageEventsQuarantine))
			if (raw != nil && raw.Get(key) != nil) || (receipts != nil && receipts.Get(key) != nil) || (quarantine != nil && quarantine.Get(key) != nil) {
				return nil
			}
			stamp, _, ok := strings.Cut(string(key), "_")
			at, err := time.Parse(time.RFC3339Nano, stamp)
			if ok && err == nil && (at.UTC().Truncate(time.Hour).Before(from.UTC().Truncate(time.Hour)) || at.UTC().Truncate(time.Hour).After(to.UTC().Truncate(time.Hour))) {
				return nil
			}
			if client := strings.TrimSpace(filter.ClientName); client != "" && owner.Client != receiptClientHash(client) {
				return nil
			}
			return fmt.Errorf("%w: raw owner without retained identity", ErrUsageHistoryIncomplete)
		}); err != nil {
			return err
		}
	}
	check := func(key []byte) error {
		inScope, err := quotaKeyInScope(key, filter, from, to, allow)
		if err != nil || !inScope {
			return err
		}
		value := quota.Get(key)
		if value == nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("%w: quota aggregation missing", ErrUsageHistoryIncomplete)
		}
		return nil
	}
	if raw := tx.Bucket([]byte(BucketUsageEvents)); raw != nil {
		refs := &indexRawReferences{tx: tx}
		usageAgg := tx.Bucket([]byte(BucketUsageAggHourly))
		metaBucket := tx.Bucket([]byte(BucketMeta))
		built := metaBucket != nil && metaBucket.Get([]byte(quotaUsageAggBuiltMetaKey)) != nil
		// Skipped rows satisfy rawRowWindowExempt: they decode, their event hour
		// is their key hour (outside the window), so they only accumulate cells
		// outside the window, which no in-window check reads.
		if err := integrityEachEvent(fast, raw, func(key, value []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if hooks != nil && hooks.raw != nil {
				hooks.raw()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			var event usage.Event
			owner, owned, err := readRawOwner(tx, key)
			if err != nil {
				return err
			}
			if !utf8.Valid(value) {
				return fmt.Errorf("%w: invalid raw encoding", ErrUsageHistoryIncomplete)
			}
			decodeErr := json.Unmarshal(value, &event)
			if decodeErr != nil {
				if err := checkMalformedIndexScope(ctx, tx, refs, key, value, filter.ClientName, from, to, filter.Model, allow); err != nil {
					return err
				}
			}
			if decodeErr != nil || event.Timestamp.IsZero() || (owned && usageKey(event.Timestamp, event.RecordID) != string(key)) {
				scope, ambiguous := backfillRawScope(value)
				if json.Valid(value) && ambiguous {
					return fmt.Errorf("%w: ambiguous raw scope", ErrUsageHistoryIncomplete)
				}
				if owned && !ambiguous && scope.ClientName != "" && receiptClientHash(scope.ClientName) != owner.Client {
					return fmt.Errorf("%w: raw owner and scope disagree", ErrUsageHistoryIncomplete)
				}
				if owned && boundRawOutsideHour(key, from, to) {
					return nil
				}
				if owned && strings.TrimSpace(filter.ClientName) != "" && owner.Client != receiptClientHash(filter.ClientName) {
					return nil
				}
				if !ambiguous && !owned {
					if client := strings.TrimSpace(filter.ClientName); !owned && client != "" && scope.ClientName != "" && client != scope.ClientName {
						return nil
					}
					if model := strings.ToLower(strings.TrimSpace(filter.Model)); model != "" && scope.Model != "" && model != scope.Model {
						return nil
					}
					if allow != nil && scope.Model != "" && scope.Target != "" && !allow.includes(scope.Target, scope.Model) {
						return nil
					}
				}
				return fmt.Errorf("%w: corrupt raw usage", ErrUsageHistoryIncomplete)
			}
			if owned {
				digest, err := usage.RetryEquivalentDigest(event)
				if err != nil || digest != owner.Digest || receiptClientHash(event.ClientName) != owner.Client {
					if receiptClientHash(event.ClientName) == owner.Client && boundRawOutsideHour(key, from, to) {
						return nil
					}
					if client := strings.TrimSpace(filter.ClientName); client != "" && owner.Client != receiptClientHash(client) && receiptClientHash(event.ClientName) == owner.Client {
						return nil
					}
					return fmt.Errorf("%w: raw owner and event disagree", ErrUsageHistoryIncomplete)
				}
			}
			quotaKeyBytes := quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
			quotaKey := string(quotaKeyBytes)
			// 仅额度路径（无 allow、无模型过滤）：后续读取只消费 quotaKeyInScope 为真或
			// 报错的键，范围外且无错的事件跳过累积。带 allow/模型过滤的报表路径中，空
			// target 与 "unknown" 共享 usage 聚合键，范围外事件仍需参与对照，保持原行为。
			if hooks != nil && hooks.windowKeys && allow == nil && filter.Model == "" {
				if inScope, scopeErr := quotaKeyInScope(quotaKeyBytes, filter, from, to, allow); scopeErr == nil && !inScope {
					return nil
				}
			}
			rawCounts[quotaKey]++
			cell := rawCells[quotaKey]
			accumulateAggCell(&cell, event)
			rawCells[quotaKey] = cell
			one := AggCell{}
			accumulateAggCell(&one, event)
			parts := rawPartitions[quotaKey]
			parts.add(costPartitions(one))
			rawPartitions[quotaKey] = parts
			usageKey := string(aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target))
			cell = usageCells[usageKey]
			accumulateAggCell(&cell, event)
			usageCells[usageKey] = cell
			if usageQuotaKeys[usageKey] == nil {
				usageQuotaKeys[usageKey] = make(map[string]bool)
			}
			usageQuotaKeys[usageKey][quotaKey] = true
			// Legacy-only aggregates remain readable before target-aware backfill.
			if !built && (usageAgg == nil || usageAgg.Get(aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)) == nil) {
				return nil
			}
			return check(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))
		}); err != nil {
			return err
		}
	}
	if agg := tx.Bucket([]byte(BucketUsageAggHourly)); agg != nil {
		for key, raw := range usageCells {
			if err := ctx.Err(); err != nil {
				return err
			}
			inScope := false
			for quotaKey := range usageQuotaKeys[key] {
				if err := ctx.Err(); err != nil {
					return err
				}
				matches, err := quotaKeyInScope([]byte(quotaKey), filter, from, to, allow)
				if err != nil {
					return err
				}
				inScope = inScope || matches
			}
			if !inScope {
				continue
			}
			entry, err := decodeAggKey([]byte(key))
			if err != nil || !entry.dimsOK {
				return fmt.Errorf("%w: invalid usage aggregation dimensions", ErrUsageHistoryIncomplete)
			}
			legacyKey := legacyAggKey(entry.hour, entry.endpointType, entry.client, entry.model, entry.target)
			legacyEntry, legacyErr := decodeAggKey(legacyKey)
			if (legacyErr != nil || !legacyEntry.dimsOK) && (agg.Get(legacyKey) != nil || agg.Bucket(legacyKey) != nil) {
				return fmt.Errorf("%w: ambiguous legacy usage aggregation", ErrUsageHistoryIncomplete)
			}
			var combined AggCell
			found := false
			for _, physical := range [][]byte{[]byte(key), legacyKey} {
				value := agg.Get(physical)
				if value == nil {
					if agg.Bucket(physical) != nil {
						return fmt.Errorf("%w: invalid usage aggregation", ErrUsageHistoryIncomplete)
					}
					continue
				}
				decoded, err := decodeAggKey(physical)
				var cell AggCell
				var fields map[string]json.RawMessage
				if err != nil || !decoded.dimsOK || json.Unmarshal(value, &cell) != nil || json.Unmarshal(value, &fields) != nil || fields == nil || quotaCellBelowRaw(value, cell, AggCell{}) {
					return fmt.Errorf("%w: invalid usage aggregation", ErrUsageHistoryIncomplete)
				}
				found = true
				addUsageIntegrityCell(&combined, cell)
			}
			if !found {
				continue
			}
			if aggCellBelowRaw(combined, raw) {
				return fmt.Errorf("%w: usage aggregation smaller than raw usage", ErrUsageHistoryIncomplete)
			}
		}
	}
	if receipts := tx.Bucket([]byte(BucketUsageReceipts)); receipts != nil {
		verified := make(map[string]bool)
		// Skipped receipts have a canonical stamp outside the window; readable
		// cells then carry a quota key of that hour, which is out of scope.
		if err := integrityEachEvent(fast, receipts, func(key, _ []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if hooks != nil && hooks.receipt != nil {
				hooks.receipt()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			_, quotaKey, ok := readReceiptCells(tx, key)
			if !ok {
				stamp, _, found := strings.Cut(string(key), "_")
				at, err := time.Parse(time.RFC3339Nano, stamp)
				if found && err == nil && (at.UTC().Truncate(time.Hour).Before(from.UTC().Truncate(time.Hour)) || at.UTC().Truncate(time.Hour).After(to.UTC().Truncate(time.Hour))) {
					return nil
				}
				if owner, known := readReceiptOwner(tx, key); known && strings.TrimSpace(filter.ClientName) != "" && owner != receiptClientHash(filter.ClientName) {
					return nil
				}
				if client := strings.TrimSpace(filter.ClientName); client != "" {
					outside, err := unresolvedReceiptOutsideClientContext(ctx, tx, key, client)
					if err != nil {
						return err
					}
					if outside {
						return nil
					}
				}
				return fmt.Errorf("%w: receipt scope unavailable", ErrUsageHistoryIncomplete)
			}
			if err := check(quotaKey); err != nil {
				return err
			}
			inScope, err := quotaKeyInScope(quotaKey, filter, from, to, allow)
			if err != nil || !inScope {
				return err
			}
			owner, hasOwner, err := readRawOwner(tx, key)
			if err != nil {
				return err
			}
			if hasOwner {
				receipt, exists, err := readUsageReceipt(tx, key)
				receiptOwner, known := readReceiptOwner(tx, key)
				if err != nil || !exists || !known || owner.Digest != receipt.Digest || owner.Client != receiptOwner {
					return fmt.Errorf("%w: raw owner and receipt disagree", ErrUsageHistoryIncomplete)
				}
			}
			proofs := tx.Bucket([]byte(rollupProofBucket))
			if !verified[string(quotaKey)] {
				if proofs == nil || proofs.Get(rollupProofKey(BucketQuotaUsageAggHourly, quotaKey)) == nil {
					return fmt.Errorf("%w: quota aggregation proof missing", ErrUsageHistoryIncomplete)
				}
				valid, err := proofValid(quotaKey)
				if err != nil {
					return err
				}
				if !valid {
					return fmt.Errorf("%w: quota aggregation proof invalid", ErrUsageHistoryIncomplete)
				}
				verified[string(quotaKey)] = true
			}
			return nil
		}); err != nil {
			return err
		}
	}
	meta := tx.Bucket([]byte(BucketMeta))
	tracked := meta != nil && string(meta.Get([]byte(usageAccountingProofTrackedKey))) == "1" && meta.Get([]byte(quotaUsageAggBuiltMetaKey)) != nil
	// A null value is never a usable cell, even without raw or receipt evidence.
	visit := func(key, value []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if hooks != nil && hooks.quota != nil {
			hooks.quota()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return check(key)
		}
		inScope, err := quotaKeyInScope(key, filter, from, to, allow)
		if err != nil || !inScope {
			return err
		}
		var cell AggCell
		if json.Unmarshal(value, &cell) != nil {
			return fmt.Errorf("%w: invalid quota aggregation", ErrUsageHistoryIncomplete)
		}
		raw := rawCells[string(key)]
		if quotaCellBelowRaw(value, cell, raw) {
			return fmt.Errorf("%w: quota aggregation smaller than raw usage", ErrUsageHistoryIncomplete)
		}
		if costPartitions(cell).below(rawPartitions[string(key)]) {
			return fmt.Errorf("%w: quota aggregation repartitions retained usage", ErrUsageHistoryIncomplete)
		}
		if tracked && cell.recordCount() > rawCounts[string(key)] {
			valid, err := proofValid(key)
			if err != nil {
				return err
			}
			if !valid {
				return fmt.Errorf("%w: quota aggregation has unverified records", ErrUsageHistoryIncomplete)
			}
		}
		return nil
	}
	if hooks != nil && hooks.windowKeys {
		end := to.Truncate(time.Hour).Format(hourLayout) + "}"
		c := quota.Cursor()
		for k, v := c.Seek(hourPrefix(from)); k != nil && string(k) <= end; k, v = c.Next() {
			if err := visit(k, v); err != nil {
				return err
			}
		}
		return nil
	}
	if fast != nil {
		// Skipped quota keys have a parseable hour outside the window, for
		// which quotaKeyInScope answers (false, nil) and visit returns nil.
		return fast.eachQuota(quota, visit)
	}
	return quota.ForEach(visit)
}

// integrityEachEvent is b.ForEach, or the windowed walk of a raw-identity
// keyed bucket when a classification applies to this read.
func integrityEachEvent(fast *integrityFast, b *bolt.Bucket, fn func(k, v []byte) error) error {
	if fast == nil {
		return b.ForEach(fn)
	}
	return fast.eachEvent(b, fn)
}

type costPartitionCounts [16]int64

func (counts *costPartitionCounts) add(other costPartitionCounts) {
	for i, n := range other {
		counts[i] += n
	}
}

func (counts costPartitionCounts) below(other costPartitionCounts) bool {
	for i, n := range other {
		if counts[i] < n {
			return true
		}
	}
	return false
}

// Match CostFor's subset precedence, for observed and billable tokens.
func costPartitions(cell AggCell) costPartitionCounts {
	var result costPartitionCounts
	for group, current := range []AggCell{cell, cell.billable()} {
		image := max(0, min(current.ImageInputTokens, current.InputTokens))
		imageOut := max(0, min(current.ImageOutputTokens, current.OutputTokens))
		cachedImage := max(0, min(current.CachedImageInputTokens, current.CachedTokens))
		explicit := max(0, min(current.ExplicitCacheReadTokens, current.CachedTokens-cachedImage))
		five := max(0, min(current.CacheCreation5mTokens, current.CacheCreationTokens))
		hour := max(0, min(current.CacheCreation1hTokens, current.CacheCreationTokens-five))
		copy(result[group*8:], []int64{current.InputTokens - image, current.OutputTokens - imageOut, current.CachedTokens - cachedImage - explicit, explicit, cachedImage, current.CacheCreationTokens - five - hour, five, hour})
	}
	return result
}

func addUsageIntegrityCell(dst *AggCell, cell AggCell) {
	dst.RecordCount = dst.recordCount() + cell.recordCount()
	dst.PriceCoverageRecordCount += cell.PriceCoverageRecordCount
	dst.IterationPriceCompleteCount += cell.IterationPriceCompleteCount
	dst.IncompleteUsageCount += cell.IncompleteUsageCount
	dst.IncompleteCostCount += cell.IncompleteCostCount
	dst.Requests += cell.Requests
	dst.Success += cell.Success
	dst.InputTokens += cell.InputTokens
	dst.OutputTokens += cell.OutputTokens
	dst.CachedTokens += cell.CachedTokens
	dst.CacheCreationTokens += cell.CacheCreationTokens
	dst.ExplicitCacheReadTokens += cell.ExplicitCacheReadTokens
	dst.ReasoningTokens += cell.ReasoningTokens
	dst.CacheCreation5mTokens += cell.CacheCreation5mTokens
	dst.CacheCreation1hTokens += cell.CacheCreation1hTokens
	dst.ToolUsePromptTokens += cell.ToolUsePromptTokens
	dst.ImageInputTokens += cell.ImageInputTokens
	dst.CachedImageInputTokens += cell.CachedImageInputTokens
	dst.ImageOutputTokens += cell.ImageOutputTokens
	if cell.NonBillable != nil {
		if dst.NonBillable == nil {
			dst.NonBillable = &NonBillableTokens{}
		}
		dst.NonBillable.InputTokens += cell.NonBillable.InputTokens
		dst.NonBillable.OutputTokens += cell.NonBillable.OutputTokens
		dst.NonBillable.CachedTokens += cell.NonBillable.CachedTokens
		dst.NonBillable.CacheCreationTokens += cell.NonBillable.CacheCreationTokens
		dst.NonBillable.ExplicitCacheReadTokens += cell.NonBillable.ExplicitCacheReadTokens
		dst.NonBillable.CacheCreation5mTokens += cell.NonBillable.CacheCreation5mTokens
		dst.NonBillable.CacheCreation1hTokens += cell.NonBillable.CacheCreation1hTokens
		dst.NonBillable.ImageInputTokens += cell.NonBillable.ImageInputTokens
		dst.NonBillable.CachedImageInputTokens += cell.NonBillable.CachedImageInputTokens
		dst.NonBillable.ImageOutputTokens += cell.NonBillable.ImageOutputTokens
	}
}

// Optional partitions absent from legacy cells are not evidence of zero.
func quotaCellBelowRaw(value []byte, cell, raw AggCell) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(value, &fields) != nil || fields == nil {
		return true
	}
	return aggCellBelowRaw(cell, raw)
}

func aggCellBelowRaw(cell, raw AggCell) bool {
	var rawFree, cellFree NonBillableTokens
	if raw.NonBillable != nil {
		rawFree = *raw.NonBillable
	}
	if cell.NonBillable != nil {
		cellFree = *cell.NonBillable
	}
	for _, p := range []struct {
		name                     string
		actual, expected         int64
		actualFree, expectedFree int64
	}{
		{"input_tokens", cell.InputTokens, raw.InputTokens, cellFree.InputTokens, rawFree.InputTokens},
		{"output_tokens", cell.OutputTokens, raw.OutputTokens, cellFree.OutputTokens, rawFree.OutputTokens},
		{"cached_tokens", cell.CachedTokens, raw.CachedTokens, cellFree.CachedTokens, rawFree.CachedTokens},
		{"cache_creation_tokens", cell.CacheCreationTokens, raw.CacheCreationTokens, cellFree.CacheCreationTokens, rawFree.CacheCreationTokens},
		{"explicit_cache_read_tokens", cell.ExplicitCacheReadTokens, raw.ExplicitCacheReadTokens, cellFree.ExplicitCacheReadTokens, rawFree.ExplicitCacheReadTokens},
		{"cache_creation_5m_tokens", cell.CacheCreation5mTokens, raw.CacheCreation5mTokens, cellFree.CacheCreation5mTokens, rawFree.CacheCreation5mTokens},
		{"cache_creation_1h_tokens", cell.CacheCreation1hTokens, raw.CacheCreation1hTokens, cellFree.CacheCreation1hTokens, rawFree.CacheCreation1hTokens},
		{"image_input_tokens", cell.ImageInputTokens, raw.ImageInputTokens, cellFree.ImageInputTokens, rawFree.ImageInputTokens},
		{"cached_image_input_tokens", cell.CachedImageInputTokens, raw.CachedImageInputTokens, cellFree.CachedImageInputTokens, rawFree.CachedImageInputTokens},
		{"image_output_tokens", cell.ImageOutputTokens, raw.ImageOutputTokens, cellFree.ImageOutputTokens, rawFree.ImageOutputTokens},
	} {
		if p.actual < 0 || p.actualFree < 0 || p.actualFree > p.actual || p.expectedFree < 0 || p.expectedFree > p.expected || p.actual < p.expected || p.actualFree < p.expectedFree || p.actual-p.actualFree < p.expected-p.expectedFree {
			return true
		}
	}
	return false
}

// A preserved original can identify a receipt only if its normalized digest
// still matches the committed identity.
func unresolvedReceiptOutsideClient(tx *bolt.Tx, key []byte, client string) bool {
	result, _ := unresolvedReceiptOutsideClientContext(context.Background(), tx, key, client)
	return result
}

func unresolvedReceiptOutsideClientContext(ctx context.Context, tx *bolt.Tx, key []byte, client string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	b, err := unresolvedState(tx)
	if err != nil || b == nil {
		return false, nil
	}
	receipt, ok, err := readUsageReceipt(tx, key)
	if err != nil || !ok {
		return false, nil
	}
	outside := false
	err = b.ForEach(func(_, value []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var item unresolvedUsage
		if json.Unmarshal(value, &item) != nil || !bytes.Equal(item.OriginalKey, key) {
			return nil
		}
		scope, ambiguous := backfillRawScope(item.Original)
		var event usage.Event
		if ambiguous || scope.ClientName == "" || json.Unmarshal(item.Original, &event) != nil || usageKey(event.Timestamp, event.RecordID) != string(key) {
			return nil
		}
		digest, err := usage.RetryEquivalentDigest(event)
		if err == nil && digest == receipt.Digest && scope.ClientName != client {
			outside = true
		}
		return nil
	})
	return outside, err
}

// Retained fresh attestations identify a missing root even when its index was lost.
func checkMissingFreshRoots(tx *bolt.Tx, filter usage.Filter, from, to time.Time) error {
	return checkMissingFreshRootsContext(context.Background(), tx, filter, from, to)
}

func checkMissingFreshRootsContext(ctx context.Context, tx *bolt.Tx, filter usage.Filter, from, to time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	proofs := tx.Bucket([]byte(freshAttestationBucket))
	raw := tx.Bucket([]byte(BucketUsageEvents))
	receipts := tx.Bucket([]byte(BucketUsageReceipts))
	if proofs == nil || raw == nil || receipts == nil {
		return nil
	}
	// Issue #125 (v3 item 5): skipped attestations have a canonical stamp
	// outside the window and return nil below; unparseable stamps already do.
	ctx = integrityReadContext(ctx, tx, from, to)
	return integrityEachEvent(integrityFastFrom(ctx, tx), proofs, func(key, value []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if raw.Get(key) != nil || receipts.Get(key) != nil {
			return nil
		}
		var proof freshAttestation
		if json.Unmarshal(value, &proof) != nil || proof.Binding != receiptOwnerBinding(key, proof.Digest, proof.ClientHash) {
			return nil
		}
		if client := strings.TrimSpace(filter.ClientName); client != "" && receiptClientHash(client) != proof.ClientHash {
			return nil
		}
		stamp, _, ok := strings.Cut(string(key), "_")
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if !ok || err != nil || at.UTC().Truncate(time.Hour).Before(from.UTC().Truncate(time.Hour)) || at.UTC().Truncate(time.Hour).After(to.UTC().Truncate(time.Hour)) {
			return nil
		}
		return fmt.Errorf("%w: attested usage root missing", ErrUsageHistoryIncomplete)
	})
}
