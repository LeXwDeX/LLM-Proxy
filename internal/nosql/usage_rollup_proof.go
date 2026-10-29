package nosql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

const rollupProofBucket = "usage_rollup_proofs_v2"
const rollupProofMarker = "usage_rollup_proofs_initialized_v2"

type rollupProofState struct {
	Version       int               `json:"version"`
	CellDigest    []byte            `json:"cell_digest"`
	IdentityCount uint64            `json:"identity_count"`
	IdentityXor   [sha256.Size]byte `json:"identity_xor"`
}

func rollupProofKey(source string, key []byte) []byte {
	return append(append([]byte(source), 0), key...)
}

func rollupProofDigest(source string, key, value []byte) []byte {
	h := sha256.New()
	h.Write([]byte("usage-rollup-proof-v1\x00"))
	h.Write([]byte(source))
	h.Write([]byte{0})
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(key)))
	h.Write(size[:])
	h.Write(key)
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	h.Write(size[:])
	h.Write(value)
	return h.Sum(nil)
}

func rollupIdentityDigest(key []byte, digest string) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte("usage-rollup-identity-v2\x00"))
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(key)))
	h.Write(size[:])
	h.Write(key)
	binary.BigEndian.PutUint64(size[:], uint64(len(digest)))
	h.Write(size[:])
	h.Write([]byte(digest))
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func currentRollupIdentities(tx *bolt.Tx, source string, key []byte, hooks *directIntegrityHooks) (uint64, [sha256.Size]byte, bool) {
	count, digest, ok, _ := currentRollupIdentitiesContext(context.Background(), tx, source, key, hooks)
	return count, digest, ok
}

func currentRollupIdentitiesContext(ctx context.Context, tx *bolt.Tx, source string, key []byte, hooks *directIntegrityHooks) (uint64, [sha256.Size]byte, bool, error) {
	var xor [sha256.Size]byte
	if err := ctx.Err(); err != nil {
		return 0, xor, false, err
	}
	raw := tx.Bucket([]byte(BucketUsageEvents))
	receipts := tx.Bucket([]byte(BucketUsageReceipts))
	index := tx.Bucket([]byte(BucketUsageEventsByClient))
	if raw == nil || receipts == nil || index == nil {
		return 0, xor, false, nil
	}
	var scope usage.Event
	if source == BucketUsageAggHourly {
		entry, err := decodeAggKey(key)
		if err != nil || !strings.HasPrefix(string(key), "v2|") || !entry.dimsOK {
			return 0, xor, false, nil
		}
		scope = usage.Event{ClientName: entry.client, Timestamp: entry.hour, Model: entry.model}
	} else if source == BucketQuotaUsageAggHourly {
		scope = backfillEvidenceScope(source, key)
	} else {
		return 0, xor, false, nil
	}
	if scope.Timestamp.IsZero() || strings.TrimSpace(scope.ClientName) == "" || strings.EqualFold(strings.TrimSpace(scope.ClientName), "unknown") {
		return 0, xor, false, nil
	}
	owners := map[string]bool{receiptClientHash(scope.ClientName): true}
	hours := map[time.Time]bool{scope.Timestamp.UTC().Truncate(time.Hour): true}
	seen := make(map[string]string)
	var count uint64
	add := func(k []byte, digest string) error {
		if old, ok := seen[string(k)]; ok {
			if old != digest {
				return ErrUsageHistoryIncomplete
			}
			return nil
		}
		if count == ^uint64(0) {
			return ErrUsageHistoryIncomplete
		}
		count++
		seen[string(k)] = digest
		xorDigest(&xor, rollupIdentityDigest(k, digest))
		return nil
	}
	if err := raw.ForEach(func(k, data []byte) error {
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
		if !decodeProofEventCached(data, &event) || usageKey(event.Timestamp, event.RecordID) != string(k) {
			if !proofRawMayOverlap(tx, k, data, owners, hours) {
				return nil
			}
			corrupt := backfillRawScopeCached(data)
			ambiguous := corrupt.ambiguous
			owner, known, ownerErr := readRawOwner(tx, k)
			if ownerErr != nil || (known && !ambiguous && corrupt.clientName != "" && corrupt.clientHash != owner.Client) {
				return ErrUsageHistoryIncomplete
			}
			if !known && !ambiguous && corrupt.model != "" && scope.Model != "" && corrupt.model != scope.Model {
				return nil
			}
			return ErrUsageHistoryIncomplete
		}
		var eventKey []byte
		if source == BucketUsageAggHourly {
			eventKey = aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)
		} else {
			eventKey = quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
		}
		if !bytes.Equal(eventKey, key) {
			return nil
		}
		if !bytes.Equal(index.Get(clientEventIndexKey(event, k)), k) {
			return ErrUsageHistoryIncomplete
		}
		digest, err := usage.RetryEquivalentDigest(event)
		if err != nil {
			return err
		}
		return add(k, digest)
	}); err != nil {
		return 0, xor, false, ctx.Err()
	}
	if err := receipts.ForEach(func(k, _ []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if hooks != nil && hooks.receipt != nil {
			hooks.receipt()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !receiptMayOverlap(tx, k, owners, hours) {
			return nil
		}
		u, q, ok := readReceiptCells(tx, k)
		if !ok {
			return ErrUsageHistoryIncomplete
		}
		actual := q
		if source == BucketUsageAggHourly {
			actual = u
		}
		if !bytes.Equal(actual, key) {
			return nil
		}
		receipt, ok, err := readUsageReceipt(tx, k)
		if err != nil || !ok {
			return ErrUsageHistoryIncomplete
		}
		return add(k, receipt.Digest)
	}); err != nil {
		return 0, xor, false, ctx.Err()
	}
	return count, xor, true, ctx.Err()
}

func readRollupProofValue(tx *bolt.Tx, source string, key []byte) (rollupProofState, bool) {
	var state rollupProofState
	proofs, err := rollupProofSource(tx)
	bucket := tx.Bucket([]byte(source))
	if err != nil || proofs == nil || bucket == nil {
		return state, false
	}
	value := bucket.Get(key)
	if value == nil {
		return state, false
	}
	raw := proofs.Get(rollupProofKey(source, key))
	if json.Unmarshal(raw, &state) != nil || state.Version != 2 || len(state.CellDigest) != sha256.Size || state.IdentityCount == 0 {
		return state, false
	}
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(canonical, raw) || !bytes.Equal(state.CellDigest, rollupProofDigest(source, key, value)) {
		return state, false
	}
	return state, true
}

func validRollupProof(tx *bolt.Tx, source string, key []byte, hooks *directIntegrityHooks) bool {
	ok, _ := validRollupProofContext(context.Background(), tx, source, key, hooks)
	return ok
}

func validRollupProofContext(ctx context.Context, tx *bolt.Tx, source string, key []byte, hooks *directIntegrityHooks) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	state, ok := readRollupProofValue(tx, source, key)
	if !ok {
		return false, ctx.Err()
	}
	count, digest, ok, err := currentRollupIdentitiesContext(ctx, tx, source, key, hooks)
	return ok && count == state.IdentityCount && digest == state.IdentityXor, err
}

// quotaProofBatch is confined to one read transaction; invalid cells are kept
// separate so damaged evidence cannot invalidate a provably unrelated cell.
type quotaProofBatchCell struct {
	state   rollupProofState
	scope   usage.Event
	seen    map[string]string
	count   uint64
	xor     [sha256.Size]byte
	invalid bool
}

func quotaProofBatchContext(ctx context.Context, tx *bolt.Tx, hooks *directIntegrityHooks) (map[string]*quotaProofBatchCell, error) {
	return quotaProofBatchInScopeContext(ctx, tx, usage.Filter{}, time.Time{}, time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), nil, hooks)
}

func quotaProofBatchInScopeContext(ctx context.Context, tx *bolt.Tx, filter usage.Filter, from, to time.Time, allow costPairFilter, hooks *directIntegrityHooks) (map[string]*quotaProofBatchCell, error) {
	cells := make(map[string]*quotaProofBatchCell)
	proofs, err := rollupProofSource(tx)
	if err != nil || proofs == nil {
		return cells, ctx.Err()
	}
	prefix := append([]byte(BucketQuotaUsageAggHourly), 0)
	cursor := proofs.Cursor()
	for k, _ := cursor.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = cursor.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := string(k[len(prefix):])
		inScope, scopeErr := quotaKeyInScope([]byte(key), filter, from, to, allow)
		if scopeErr == nil && !inScope {
			continue
		}
		state, ok := readRollupProofValue(tx, BucketQuotaUsageAggHourly, []byte(key))
		if !ok {
			continue
		}
		scope := backfillEvidenceScope(BucketQuotaUsageAggHourly, []byte(key))
		cell := &quotaProofBatchCell{state: state, scope: scope, seen: make(map[string]string)}
		if hooks != nil && hooks.proofCell != nil {
			hooks.proofCell()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cell.invalid = scope.Timestamp.IsZero() || strings.TrimSpace(scope.ClientName) == "" || strings.EqualFold(strings.TrimSpace(scope.ClientName), "unknown")
		cells[key] = cell
	}
	if len(cells) == 0 {
		return cells, ctx.Err()
	}
	raw, receipts, index := tx.Bucket([]byte(BucketUsageEvents)), tx.Bucket([]byte(BucketUsageReceipts)), tx.Bucket([]byte(BucketUsageEventsByClient))
	if raw == nil || receipts == nil || index == nil {
		for _, cell := range cells {
			cell.invalid = true
		}
		return cells, ctx.Err()
	}
	add := func(cell *quotaProofBatchCell, k []byte, digest string) {
		if cell.invalid {
			return
		}
		if old, ok := cell.seen[string(k)]; ok {
			if old != digest {
				cell.invalid = true
			}
			return
		}
		if cell.count == ^uint64(0) {
			cell.invalid = true
			return
		}
		cell.seen[string(k)] = digest
		if hooks != nil && hooks.proofIdentity != nil {
			hooks.proofIdentity()
		}
		cell.count++
		xorDigest(&cell.xor, rollupIdentityDigest(k, digest))
	}
	if err := raw.ForEach(func(k, data []byte) error {
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
		if !decodeProofEventCached(data, &event) || usageKey(event.Timestamp, event.RecordID) != string(k) {
			corrupt := backfillRawScopeCached(data)
			ambiguous := corrupt.ambiguous
			owner, known, ownerErr := readRawOwner(tx, k)
			for _, cell := range cells {
				if err := ctx.Err(); err != nil {
					return err
				}
				if cell.invalid {
					continue
				}
				owners := map[string]bool{receiptClientHash(cell.scope.ClientName): true}
				hours := map[time.Time]bool{cell.scope.Timestamp.UTC().Truncate(time.Hour): true}
				if !proofRawMayOverlap(tx, k, data, owners, hours) {
					continue
				}
				if ownerErr == nil && !(known && !ambiguous && corrupt.clientName != "" && corrupt.clientHash != owner.Client) && !known && !ambiguous && corrupt.model != "" && cell.scope.Model != "" && corrupt.model != cell.scope.Model {
					continue
				}
				cell.invalid = true
			}
			return nil
		}
		cell := cells[string(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model))]
		if cell == nil || cell.invalid {
			return nil
		}
		if !bytes.Equal(index.Get(clientEventIndexKey(event, k)), k) {
			cell.invalid = true
			return nil
		}
		digest, err := usage.RetryEquivalentDigest(event)
		if err != nil {
			cell.invalid = true
			return nil
		}
		add(cell, k, digest)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := receipts.ForEach(func(k, _ []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if hooks != nil && hooks.receipt != nil {
			hooks.receipt()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		_, quotaKey, ok := readReceiptCells(tx, k)
		if !ok {
			for _, cell := range cells {
				if err := ctx.Err(); err != nil {
					return err
				}
				if !cell.invalid && receiptMayOverlap(tx, k, map[string]bool{receiptClientHash(cell.scope.ClientName): true}, map[time.Time]bool{cell.scope.Timestamp.UTC().Truncate(time.Hour): true}) {
					cell.invalid = true
				}
			}
			return nil
		}
		cell := cells[string(quotaKey)]
		if cell == nil || cell.invalid || !receiptMayOverlap(tx, k, map[string]bool{receiptClientHash(cell.scope.ClientName): true}, map[time.Time]bool{cell.scope.Timestamp.UTC().Truncate(time.Hour): true}) {
			return nil
		}
		receipt, exists, err := readUsageReceipt(tx, k)
		if err != nil || !exists {
			cell.invalid = true
			return nil
		}
		add(cell, k, receipt.Digest)
		return nil
	}); err != nil {
		return nil, err
	}
	return cells, ctx.Err()
}

func (cell *quotaProofBatchCell) valid() bool {
	return cell != nil && !cell.invalid && cell.count == cell.state.IdentityCount && cell.xor == cell.state.IdentityXor
}

func rollupProofSource(tx *bolt.Tx) (*bolt.Bucket, error) {
	meta := tx.Bucket([]byte(BucketMeta))
	proofs := tx.Bucket([]byte(rollupProofBucket))
	if meta == nil {
		return nil, errors.New("rollup proof: meta unavailable")
	}
	marker := meta.Get([]byte(rollupProofMarker))
	if (marker != nil && (string(marker) != "1" || proofs == nil)) || (marker == nil && proofs != nil) {
		return nil, errors.New("rollup proof: proof source unavailable")
	}
	return proofs, nil
}

func putRollupProof(tx *bolt.Tx, source string, key []byte, state rollupProofState) error {
	meta := tx.Bucket([]byte(BucketMeta))
	sourceBucket := tx.Bucket([]byte(source))
	if meta == nil || sourceBucket == nil {
		return errors.New("rollup proof: source unavailable")
	}
	value := sourceBucket.Get(key)
	if value == nil {
		return errors.New("rollup proof: cell missing")
	}
	proofs, err := rollupProofSource(tx)
	if err != nil {
		return err
	}
	if proofs == nil {
		proofs, err = tx.CreateBucket([]byte(rollupProofBucket))
		if err != nil {
			return err
		}
		if err := meta.Put([]byte(rollupProofMarker), []byte("1")); err != nil {
			return err
		}
	}
	state.Version = 2
	state.CellDigest = rollupProofDigest(source, key, value)
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return proofs.Put(rollupProofKey(source, key), data)
}

// Preserve the old digest as an invalid tombstone so a recreated cell with
// identical totals cannot accidentally revive a proof for deleted history.
func invalidateRollupProof(tx *bolt.Tx, source string, key []byte) error {
	b, err := rollupProofSource(tx)
	if err != nil {
		return err
	}
	if b == nil {
		return nil
	}
	proofKey := rollupProofKey(source, key)
	old := b.Get(proofKey)
	if len(old) == 0 || old[0] == 0 {
		return nil
	}
	tombstone := append([]byte{0}, old...)
	return b.Put(proofKey, tombstone)
}

func invalidateSourceRollupProofs(tx *bolt.Tx, source string) error {
	proofs, err := rollupProofSource(tx)
	if err != nil {
		return err
	}
	if proofs == nil {
		return nil
	}
	prefix := append([]byte(source), 0)
	cursor := proofs.Cursor()
	var keys [][]byte
	for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
		keys = append(keys, append([]byte(nil), key[len(prefix):]...))
	}
	for _, key := range keys {
		if err := invalidateRollupProof(tx, source, key); err != nil {
			return err
		}
	}
	return nil
}

// Capture the complete bundle before retention removes its raw identities.
func capturePruneRollupProof(tx *bolt.Tx, event usage.Event, key []byte, caches ...map[string]bool) error {
	proofEvent, proofKey := event, key
	if event.Supplementary && strings.TrimSpace(event.ParentRecordID) != "" {
		rootKey := []byte(usageKey(event.Timestamp, event.ParentRecordID))
		raw := tx.Bucket([]byte(BucketUsageEvents))
		if raw != nil && raw.Get(rootKey) != nil {
			var root usage.Event
			if !decodeProofEvent(raw.Get(rootKey), &root) || root.Supplementary || root.ParentRecordID != "" {
				return preserveUnprovenPrune(tx, event, key)
			}
			rows, keys, err := normalizeUsageBundle(root)
			found := false
			if err == nil {
				for i, candidate := range keys {
					if candidate == string(key) {
						want, wantErr := usage.RetryEquivalentDigest(rows[i])
						got, gotErr := usage.RetryEquivalentDigest(event)
						found = wantErr == nil && gotErr == nil && want == got
						break
					}
				}
			}
			if !found {
				return preserveUnprovenPrune(tx, event, key)
			}
			proofEvent, proofKey = root, rootKey
		}
	}
	rows, keys, err := normalizeUsageBundle(proofEvent)
	if err != nil || len(rows) == 0 || keys[0] != string(proofKey) {
		return preserveUnprovenPrune(tx, event, key)
	}
	if !proofEvent.Supplementary {
		committed, err := inspectUsageBundleTx(tx, rows, keys, nil)
		if err != nil || !committed {
			return preserveUnprovenPrune(tx, event, key)
		}
	}
	type cell struct {
		source string
		key    []byte
		state  rollupProofState
	}
	var cells []cell
	var cache map[string]bool
	if len(caches) > 0 {
		cache = caches[0]
	}
	allValid := true
	for _, row := range rows {
		for _, entry := range []cell{{source: BucketUsageAggHourly, key: aggKey(row.Timestamp, row.EndpointType, row.ClientName, row.Model, row.Target)}, {source: BucketQuotaUsageAggHourly, key: quotaAggKey(row.Timestamp, row.ClientName, row.Target, row.Model)}} {
			duplicate := false
			for _, existing := range cells {
				if existing.source == entry.source && bytes.Equal(existing.key, entry.key) {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			known, seen := cache[string(rollupProofKey(entry.source, entry.key))]
			if !seen {
				known = validRollupProof(tx, entry.source, entry.key, nil)
			}
			if seen && !known {
				return preserveUnprovenPrune(tx, event, key)
			}
			if !known {
				allValid = false
			}
			cells = append(cells, entry)
		}
	}
	if allValid {
		for _, cell := range cells {
			if cache != nil {
				cache[string(rollupProofKey(cell.source, cell.key))] = true
			}
		}
		return nil
	}
	if !usageAccountingRawProven(tx, rows, keys) {
		for _, cell := range cells {
			if cache != nil {
				cache[string(rollupProofKey(cell.source, cell.key))] = false
			}
		}
		return preserveUnprovenPrune(tx, event, key)
	}
	for i := range cells {
		count, xor, ok := currentRollupIdentities(tx, cells[i].source, cells[i].key, nil)
		if !ok || count == 0 {
			return preserveUnprovenPrune(tx, event, key)
		}
		cells[i].state = rollupProofState{IdentityCount: count, IdentityXor: xor}
	}
	for _, cell := range cells {
		if err := putRollupProof(tx, cell.source, cell.key, cell.state); err != nil {
			return err
		}
		if cache != nil {
			cache[string(rollupProofKey(cell.source, cell.key))] = true
		}
	}
	return nil
}

func preserveUnprovenPrune(tx *bolt.Tx, event usage.Event, key []byte) error {
	raw := tx.Bucket([]byte(BucketUsageEvents))
	if raw == nil {
		return errors.New("prune evidence: raw source missing")
	}
	data := raw.Get(key)
	if data == nil {
		return errors.New("prune evidence: raw record missing")
	}
	var canonical usage.Event
	if !decodeProofEvent(data, &canonical) || usageKey(canonical.Timestamp, canonical.RecordID) != string(key) {
		event, _ = backfillRawScope(data)
	} else {
		event = canonical
	}
	return putUnresolvedEvidence(tx, unresolvedUsage{Event: event, Original: append([]byte{}, data...), OriginalKey: append([]byte(nil), key...), Kind: "prune_unverified"})
}

func advanceRollupProof(tx *bolt.Tx, source string, key []byte, state rollupProofState, event usage.Event, rawKey []byte) error {
	if state.IdentityCount == ^uint64(0) {
		return errors.New("rollup proof: identity count overflow")
	}
	digest, err := usage.RetryEquivalentDigest(event)
	if err != nil {
		return err
	}
	state.IdentityCount++
	xorDigest(&state.IdentityXor, rollupIdentityDigest(rawKey, digest))
	return putRollupProof(tx, source, key, state)
}

// A retained proof is usable only with exact committed identity and both
// unmodified permanent cells; old receipts without such a proof stay unknown.
func usageRetainedRollupsProven(tx *bolt.Tx, rows []usage.Event, keys []string) bool {
	proven, _ := usageRetainedRollupsProvenContext(context.Background(), tx, rows, keys)
	return proven
}

func usageRetainedRollupsProvenContext(ctx context.Context, tx *bolt.Tx, rows []usage.Event, keys []string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	proven := usageRetainedRollupsProvenImpl(ctx, tx, rows, keys)
	return proven, ctx.Err()
}

func usageRetainedRollupsProvenImpl(ctx context.Context, tx *bolt.Tx, rows []usage.Event, keys []string) bool {
	if ctx.Err() != nil {
		return false
	}
	meta := tx.Bucket([]byte(BucketMeta))
	raw := tx.Bucket([]byte(BucketUsageEvents))
	quarantine := tx.Bucket([]byte(BucketUsageEventsQuarantine))
	receipts := tx.Bucket([]byte(BucketUsageReceipts))
	if meta == nil || raw == nil || quarantine == nil || receipts == nil || len(rows) == 0 || len(rows) != len(keys) {
		return false
	}
	for _, source := range []string{BucketUsageAggHourly, BucketQuotaUsageAggHourly} {
		if meta.Get([]byte(usageAggregateSourceMissingPrefix+source)) != nil {
			return false
		}
	}
	actual := make([]usage.Event, 0, len(rows))
	usageKeys, quotaKeys := make(map[string]bool), make(map[string]bool)
	owners, hours := make(map[string]bool), make(map[time.Time]bool)
	proofCache := make(map[string]bool)
	proofValid := func(source string, key []byte) bool {
		cacheKey := string(rollupProofKey(source, key))
		if value, ok := proofCache[cacheKey]; ok {
			return value
		}
		hooks, _ := ctx.Value(proofScanHooksKey{}).(*directIntegrityHooks)
		value, _ := validRollupProofContext(ctx, tx, source, key, hooks)
		proofCache[cacheKey] = value
		return value
	}
	for i, key := range keys {
		if ctx.Err() != nil {
			return false
		}
		row := rows[i]
		if row.Timestamp.IsZero() || strings.TrimSpace(row.ClientName) == "" || strings.EqualFold(strings.TrimSpace(row.ClientName), "unknown") || usageKey(row.Timestamp, row.RecordID) != key {
			return false
		}
		data := raw.Get([]byte(key))
		if data != nil {
			var stored usage.Event
			if !decodeProofEvent(data, &stored) || usageKey(stored.Timestamp, stored.RecordID) != key || strings.TrimSpace(stored.ClientName) != strings.TrimSpace(row.ClientName) {
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
			row = stored
		} else {
			if receipts.Get([]byte(key)) == nil {
				return false
			}
			digest, err := usage.RetryEquivalentDigest(row)
			if err != nil {
				return false
			}
			state, _, _, err := lookupUsageIdentity(tx, []byte(key), []byte(digest))
			if err != nil || state != identityReceipt {
				return false
			}
		}
		usageKey := aggKey(row.Timestamp, row.EndpointType, row.ClientName, row.Model, row.Target)
		quotaKey := quotaAggKey(row.Timestamp, row.ClientName, row.Target, row.Model)
		if !proofValid(BucketUsageAggHourly, usageKey) || !proofValid(BucketQuotaUsageAggHourly, quotaKey) {
			return false
		}
		usageKeys[string(usageKey)] = true
		quotaKeys[string(quotaKey)] = true
		owners[receiptClientHash(row.ClientName)] = true
		hours[row.Timestamp.UTC().Truncate(time.Hour)] = true
		actual = append(actual, row)
	}
	if !quarantineOutsideProofContext(ctx, tx, owners, hours) {
		return false
	}
	if checkUsageIdentityAdmission(tx, actual) != nil {
		return false
	}
	if err := receipts.ForEach(func(key, _ []byte) error {
		if err := proofScanStep(ctx, true); err != nil {
			return err
		}
		if !receiptMayOverlap(tx, key, owners, hours) {
			return nil
		}
		usageKey, quotaKey, ok := readReceiptCells(tx, key)
		if !ok {
			return ErrUsageHistoryIncomplete
		}
		if !usageKeys[string(usageKey)] && !quotaKeys[string(quotaKey)] {
			return nil
		}
		if !proofValid(BucketUsageAggHourly, usageKey) || !proofValid(BucketQuotaUsageAggHourly, quotaKey) {
			return ErrUsageHistoryIncomplete
		}
		return nil
	}); err != nil {
		return false
	}
	return true
}
