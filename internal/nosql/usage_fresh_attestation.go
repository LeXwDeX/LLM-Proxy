package nosql

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

const freshAttestationBucket = "usage_fresh_commit_attestations_v1"
const freshAttestationMarker = "usage_fresh_commit_attestations_initialized_v1"

type freshAttestation struct {
	Digest     string `json:"digest"`
	ClientHash string `json:"client_hash"`
	Binding    string `json:"binding"`
}

// A memo is valid only in the pre-write phase of one unchanged transaction.
type freshScopeMemo map[string]bool

func (memo freshScopeMemo) proven(ctx context.Context, tx *bolt.Tx, rows []usage.Event) bool {
	// Admission includes the exact timestamp, not just the hour in the key.
	if checkUsageIdentityAdmission(tx, rows) != nil {
		return false
	}
	cells := make([]string, 0, len(rows))
	for _, row := range rows {
		client := strings.TrimSpace(row.ClientName)
		if client == "" || strings.EqualFold(client, "unknown") || row.Timestamp.IsZero() {
			return false
		}
		parts := [5]string{
			client, row.Timestamp.UTC().Truncate(time.Hour).Format(time.RFC3339Nano),
			string(aggKey(row.Timestamp, row.EndpointType, row.ClientName, row.Model, row.Target)),
			string(legacyAggKey(row.Timestamp, row.EndpointType, row.ClientName, row.Model, row.Target)),
			string(quotaAggKey(row.Timestamp, row.ClientName, row.Target, row.Model)),
		}
		var cell []byte
		for _, part := range parts {
			cell = binary.AppendUvarint(cell, uint64(len(part)))
			cell = append(cell, part...)
		}
		cells = append(cells, string(cell))
	}
	sort.Strings(cells)
	unique := cells[:0]
	for _, cell := range cells {
		if len(unique) == 0 || unique[len(unique)-1] != cell {
			unique = append(unique, cell)
		}
	}
	var encoded []byte
	for _, cell := range unique {
		encoded = binary.AppendUvarint(encoded, uint64(len(cell)))
		encoded = append(encoded, cell...)
	}
	key := string(encoded)
	if proven, ok := memo[key]; ok {
		return proven
	}
	proven := freshScopeProvenContext(ctx, tx, rows) || continuationScopeProvenContext(ctx, tx, rows)
	memo[key] = proven
	return proven
}

// The pre-write check never infers that retained old history is complete.
// It only permits a new attestation for an empty client/hour and empty cells.
func freshScopeProven(tx *bolt.Tx, rows []usage.Event) bool {
	return freshScopeProvenContext(context.Background(), tx, rows)
}

func freshScopeProvenContext(ctx context.Context, tx *bolt.Tx, rows []usage.Event) bool {
	meta := tx.Bucket([]byte(BucketMeta))
	raw := tx.Bucket([]byte(BucketUsageEvents))
	usageAgg := tx.Bucket([]byte(BucketUsageAggHourly))
	quota := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
	receipts := tx.Bucket([]byte(BucketUsageReceipts))
	index := tx.Bucket([]byte(BucketUsageEventsByClient))
	quarantine := tx.Bucket([]byte(BucketUsageEventsQuarantine))
	if meta == nil || raw == nil || usageAgg == nil || quota == nil || receipts == nil || index == nil || quarantine == nil || tx.Bucket([]byte(BucketUsageClientHours)) == nil || len(rows) == 0 {
		return false
	}
	if meta.Get([]byte(usageAccountingProofTrackedKey)) != nil {
		return false
	}
	if checkUsageIdentityAdmission(tx, rows) != nil {
		return false
	}
	for _, source := range []string{BucketUsageAggHourly, BucketQuotaUsageAggHourly} {
		if meta.Get([]byte(usageAggregateSourceMissingPrefix+source)) != nil {
			return false
		}
	}
	owners := make(map[string]bool)
	hours := make(map[time.Time]bool)
	scopes := make(map[string]bool)
	for _, row := range rows {
		client := strings.TrimSpace(row.ClientName)
		if client == "" || strings.EqualFold(client, "unknown") || row.Timestamp.IsZero() {
			return false
		}
		hour := row.Timestamp.UTC().Truncate(time.Hour)
		owners[receiptClientHash(client)] = true
		hours[hour] = true
		scopes[client+"\x00"+hour.Format(hourLayout)] = true
		if usageAgg.Get(aggKey(row.Timestamp, row.EndpointType, client, row.Model, row.Target)) != nil || usageAgg.Get(legacyAggKey(row.Timestamp, row.EndpointType, client, row.Model, row.Target)) != nil || quota.Get(quotaAggKey(row.Timestamp, client, row.Target, row.Model)) != nil {
			return false
		}
		state, err := loadClientHour(tx, clientHourKey(client, hour))
		if err != nil || state.Count != 0 || state.Digest != ([32]byte{}) {
			return false
		}
		prefix := []byte(clientIndexPrefix(client) + hour.Format(hourLayout))
		k, _ := index.Cursor().Seek(prefix)
		if strings.HasPrefix(string(k), string(prefix)) {
			return false
		}
	}
	if !quarantineOutsideProof(tx, owners, hours) {
		return false
	}
	if err := raw.ForEach(func(k, v []byte) error {
		if err := proofScanStep(ctx, false); err != nil {
			return err
		}
		var e usage.Event
		if !decodeProofEvent(v, &e) || usageKey(e.Timestamp, e.RecordID) != string(k) {
			if !proofRawMayOverlap(tx, k, v, owners, hours) {
				return nil
			}
			return ErrUsageHistoryIncomplete
		}
		if scopes[strings.TrimSpace(e.ClientName)+"\x00"+e.Timestamp.UTC().Truncate(time.Hour).Format(hourLayout)] {
			return ErrUsageHistoryIncomplete
		}
		return nil
	}); err != nil {
		return false
	}
	if err := receipts.ForEach(func(k, _ []byte) error {
		if err := proofScanStep(ctx, true); err != nil {
			return err
		}
		if receiptMayOverlap(tx, k, owners, hours) {
			return ErrUsageHistoryIncomplete
		}
		return nil
	}); err != nil {
		return false
	}
	if !freshSnapshotsDisjoint(tx, owners, hours) {
		return false
	}
	return true
}

// Continue only a client/hour whose retained rows all belong to complete,
// already-attested bundles. A different cell must have no conflicting source.
func continuationScopeProven(tx *bolt.Tx, rows []usage.Event) bool {
	return continuationScopeProvenContext(context.Background(), tx, rows)
}

func continuationScopeProvenContext(ctx context.Context, tx *bolt.Tx, rows []usage.Event) bool {
	meta := tx.Bucket([]byte(BucketMeta))
	raw := tx.Bucket([]byte(BucketUsageEvents))
	if meta == nil || raw == nil || len(rows) == 0 || meta.Get([]byte(usageAccountingProofTrackedKey)) != nil || checkUsageIdentityAdmission(tx, rows) != nil {
		return false
	}
	type cellPair struct{ usage, quota string }
	scopes := make(map[string]bool)
	owners := make(map[string]bool)
	hours := make(map[time.Time]bool)
	for _, row := range rows {
		client := strings.TrimSpace(row.ClientName)
		if client == "" || strings.EqualFold(client, "unknown") || row.Timestamp.IsZero() {
			return false
		}
		hour := row.Timestamp.UTC().Truncate(time.Hour)
		scopes[client+"\x00"+hour.Format(hourLayout)] = true
		owners[receiptClientHash(client)] = true
		hours[hour] = true
	}
	if !quarantineOutsideProof(tx, owners, hours) {
		return false
	}
	if !freshSnapshotsDisjoint(tx, owners, hours) {
		return false
	}
	covered := make(map[cellPair]bool)
	seenRoots := make(map[string]bool)
	if err := raw.ForEach(func(k, value []byte) error {
		if err := proofScanStep(ctx, false); err != nil {
			return err
		}
		var event usage.Event
		if !decodeProofEvent(value, &event) || usageKey(event.Timestamp, event.RecordID) != string(k) {
			if !proofRawMayOverlap(tx, k, value, owners, hours) {
				return nil
			}
			return ErrUsageHistoryIncomplete
		}
		hour := event.Timestamp.UTC().Truncate(time.Hour)
		if !scopes[strings.TrimSpace(event.ClientName)+"\x00"+hour.Format(hourLayout)] {
			return nil
		}
		rootKey := string(k)
		if event.Supplementary {
			if strings.TrimSpace(event.ParentRecordID) == "" {
				return ErrUsageHistoryIncomplete
			}
			rootKey = usageKey(event.Timestamp, event.ParentRecordID)
		} else if event.ParentRecordID != "" {
			return ErrUsageHistoryIncomplete
		}
		var root usage.Event
		if !decodeProofEvent(raw.Get([]byte(rootKey)), &root) || root.Supplementary || root.ParentRecordID != "" {
			return ErrUsageHistoryIncomplete
		}
		anchorRows, anchorKeys, err := normalizeUsageBundle(root)
		if err != nil || len(anchorKeys) == 0 || anchorKeys[0] != rootKey || !validFreshAttestation(tx, anchorRows, anchorKeys) {
			return ErrUsageHistoryIncomplete
		}
		found := false
		for i, anchorKey := range anchorKeys {
			if anchorKey != string(k) {
				continue
			}
			want, err := usage.RetryEquivalentDigest(anchorRows[i])
			if err != nil {
				return err
			}
			got, err := usage.RetryEquivalentDigest(event)
			if err != nil || got != want {
				return ErrUsageHistoryIncomplete
			}
			found = true
			break
		}
		if !found {
			return ErrUsageHistoryIncomplete
		}
		if seenRoots[rootKey] {
			return nil
		}
		committed, err := inspectUsageBundleTx(tx, anchorRows, anchorKeys, nil)
		if err != nil || !committed {
			return ErrUsageHistoryIncomplete
		}
		needsProof := false
		for _, row := range anchorRows {
			pair := cellPair{string(aggKey(row.Timestamp, row.EndpointType, row.ClientName, row.Model, row.Target)), string(quotaAggKey(row.Timestamp, row.ClientName, row.Target, row.Model))}
			if !covered[pair] {
				needsProof = true
				break
			}
		}
		if needsProof {
			proven, err := usageAccountingProvenContext(ctx, tx, anchorRows, anchorKeys)
			if err != nil || !proven {
				return ErrUsageHistoryIncomplete
			}
		}
		seenRoots[rootKey] = true
		for _, row := range anchorRows {
			covered[cellPair{string(aggKey(row.Timestamp, row.EndpointType, row.ClientName, row.Model, row.Target)), string(quotaAggKey(row.Timestamp, row.ClientName, row.Target, row.Model))}] = true
		}
		return nil
	}); err != nil {
		return false
	}
	if len(seenRoots) == 0 {
		return false
	}
	for _, row := range rows {
		if !covered[cellPair{string(aggKey(row.Timestamp, row.EndpointType, row.ClientName, row.Model, row.Target)), string(quotaAggKey(row.Timestamp, row.ClientName, row.Target, row.Model))}] && !unoccupiedContinuationCellContext(ctx, tx, row) {
			return false
		}
	}
	return true
}

func unoccupiedContinuationCell(tx *bolt.Tx, row usage.Event) bool {
	return unoccupiedContinuationCellContext(context.Background(), tx, row)
}

func unoccupiedContinuationCellContext(ctx context.Context, tx *bolt.Tx, row usage.Event) bool {
	usageAgg := tx.Bucket([]byte(BucketUsageAggHourly))
	quota := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
	receipts := tx.Bucket([]byte(BucketUsageReceipts))
	if usageAgg == nil || quota == nil || receipts == nil {
		return false
	}
	usageKey := aggKey(row.Timestamp, row.EndpointType, row.ClientName, row.Model, row.Target)
	quotaKey := quotaAggKey(row.Timestamp, row.ClientName, row.Target, row.Model)
	if usageAgg.Get(usageKey) != nil || usageAgg.Get(legacyAggKey(row.Timestamp, row.EndpointType, row.ClientName, row.Model, row.Target)) != nil || quota.Get(quotaKey) != nil {
		return false
	}
	owners := map[string]bool{receiptClientHash(row.ClientName): true}
	hours := map[time.Time]bool{row.Timestamp.UTC().Truncate(time.Hour): true}
	return receipts.ForEach(func(key, _ []byte) error {
		if err := proofScanStep(ctx, true); err != nil {
			return err
		}
		if !receiptMayOverlap(tx, key, owners, hours) {
			return nil
		}
		u, q, ok := readReceiptCells(tx, key)
		if !ok || string(u) == string(usageKey) || string(q) == string(quotaKey) {
			return ErrUsageHistoryIncomplete
		}
		return nil
	}) == nil
}

func freshSnapshotsDisjoint(tx *bolt.Tx, owners map[string]bool, hours map[time.Time]bool) bool {
	root := tx.Bucket([]byte(usageBackfillSnapshotBucket))
	if root == nil {
		return true
	}
	for _, source := range []string{BucketUsageAggHourly, BucketQuotaUsageAggHourly} {
		generations := root.Bucket([]byte(source))
		if generations == nil {
			continue
		}
		if err := generations.ForEach(func(key, value []byte) error {
			if value != nil {
				return ErrUsageHistoryIncomplete
			}
			snapshot := generations.Bucket(key)
			if snapshot == nil {
				return ErrUsageHistoryIncomplete
			}
			return snapshot.ForEach(func(k, v []byte) error {
				if v == nil && snapshot.Bucket(k) != nil {
					return ErrUsageHistoryIncomplete
				}
				scope := backfillEvidenceScope(source, k)
				if scope.ClientName != "" && !owners[receiptClientHash(scope.ClientName)] {
					return nil
				}
				if !scope.Timestamp.IsZero() && !hours[scope.Timestamp.UTC().Truncate(time.Hour)] {
					return nil
				}
				return ErrUsageHistoryIncomplete
			})
		}); err != nil {
			return false
		}
	}
	return true
}

func writeFreshAttestation(tx *bolt.Tx, root usage.Event, key string) error {
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil {
		return errors.New("fresh attestation: meta missing")
	}
	b := tx.Bucket([]byte(freshAttestationBucket))
	marker := meta.Get([]byte(freshAttestationMarker))
	if (marker != nil && (string(marker) != "1" || b == nil)) || (marker == nil && b != nil) {
		return errors.New("fresh attestation: source unavailable")
	}
	if b == nil {
		var err error
		b, err = tx.CreateBucket([]byte(freshAttestationBucket))
		if err != nil {
			return err
		}
		if err := meta.Put([]byte(freshAttestationMarker), []byte("1")); err != nil {
			return err
		}
	}
	digest, err := usage.RetryEquivalentDigest(root)
	if err != nil {
		return err
	}
	owner := receiptClientHash(root.ClientName)
	attestation := freshAttestation{Digest: digest, ClientHash: owner, Binding: receiptOwnerBinding([]byte(key), digest, owner)}
	value, err := json.Marshal(attestation)
	if err != nil {
		return err
	}
	if old := b.Get([]byte(key)); old != nil && string(old) != string(value) {
		return errors.New("fresh attestation: identity conflict")
	}
	return b.Put([]byte(key), value)
}

func validFreshAttestation(tx *bolt.Tx, rows []usage.Event, keys []string) bool {
	if len(rows) == 0 || len(keys) == 0 {
		return false
	}
	meta := tx.Bucket([]byte(BucketMeta))
	b := tx.Bucket([]byte(freshAttestationBucket))
	raw := tx.Bucket([]byte(BucketUsageEvents))
	if meta == nil || b == nil || raw == nil || string(meta.Get([]byte(freshAttestationMarker))) != "1" {
		return false
	}
	var proof freshAttestation
	var stored usage.Event
	if json.Unmarshal(b.Get([]byte(keys[0])), &proof) != nil || !decodeProofEvent(raw.Get([]byte(keys[0])), &stored) || strings.TrimSpace(stored.ClientName) != strings.TrimSpace(rows[0].ClientName) {
		return false
	}
	digest, err := usage.RetryEquivalentDigest(stored)
	if err != nil {
		return false
	}
	owner := receiptClientHash(stored.ClientName)
	return proof.Digest == digest && proof.ClientHash == owner && proof.Binding == receiptOwnerBinding([]byte(keys[0]), digest, owner)
}
