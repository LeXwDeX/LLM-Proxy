package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func ambiguousOriginalScopes(item unresolvedUsage) []unresolvedScope {
	if len(item.Original) == 0 || !utf8.Valid(item.Original) || !json.Valid(item.Original) {
		return nil
	}
	scope, ambiguous := backfillRawScope(item.Original)
	if !ambiguous {
		var original usage.Event
		if json.Unmarshal(item.Original, &original) == nil && strings.TrimSpace(original.RecordID) != "" && !original.Timestamp.IsZero() && strings.TrimSpace(item.Event.RecordID) != "" && !item.Event.Timestamp.IsZero() {
			left, _, leftErr := normalizeUsageBundle(item.Event)
			right, _, rightErr := normalizeUsageBundle(original)
			if leftErr == nil && rightErr == nil && len(left) > 0 && len(right) > 0 {
				leftDigest, leftErr := usage.RetryEquivalentDigest(left[0])
				rightDigest, rightErr := usage.RetryEquivalentDigest(right[0])
				if leftErr == nil && rightErr == nil && leftDigest == rightDigest {
					return nil
				}
			}
		}
	}
	return []unresolvedScope{normalizedUnresolvedScope(scope)}
}

func normalizedUnresolvedScope(event usage.Event) unresolvedScope {
	root := strings.TrimSpace(event.RecordID)
	if event.Supplementary && strings.TrimSpace(event.ParentRecordID) != "" {
		root = strings.TrimSpace(event.ParentRecordID)
	}
	return unresolvedScope{ClientName: strings.TrimSpace(event.ClientName), Timestamp: event.Timestamp.UTC(), RecordID: strings.TrimSpace(event.RecordID), RootRecordID: root, Model: strings.ToLower(strings.TrimSpace(event.Model)), Target: strings.ToLower(strings.TrimSpace(event.Target))}
}

func committedScopeProven(tx *bolt.Tx, scope unresolvedScope) bool {
	proven, _ := committedScopeProvenContext(context.Background(), tx, scope)
	return proven
}

func committedScopeProvenContext(ctx context.Context, tx *bolt.Tx, scope unresolvedScope) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	proven := committedScopeProvenImpl(ctx, tx, scope)
	return proven, ctx.Err()
}

func committedScopeProvenImpl(ctx context.Context, tx *bolt.Tx, scope unresolvedScope) bool {
	if scope.ClientName == "" || strings.EqualFold(scope.ClientName, "unknown") || scope.Timestamp.IsZero() || scope.RecordID == "" || scope.RootRecordID == "" || scope.Model == "" || scope.Target == "" {
		return false
	}
	raw := tx.Bucket([]byte(BucketUsageEvents))
	if raw == nil {
		return false
	}
	rootKey := usageKey(scope.Timestamp, scope.RootRecordID)
	var root usage.Event
	if !decodeProofEvent(raw.Get([]byte(rootKey)), &root) || root.Supplementary || root.ParentRecordID != "" || usageKey(root.Timestamp, root.RecordID) != rootKey {
		return false
	}
	rows, keys, err := normalizeUsageBundle(root)
	if err != nil || len(keys) == 0 || keys[0] != rootKey {
		return false
	}
	found := false
	for _, row := range rows {
		if ctx.Err() != nil {
			return false
		}
		if normalizedUnresolvedScope(row) == scope {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	if !proofIdentitiesPresent(ctx, tx, keys) {
		return false
	}
	committed, err := inspectUsageBundleTx(tx, rows, keys, nil)
	if err != nil || !committed {
		return false
	}
	proven, _ := usageAccountingProvenContext(ctx, tx, rows, keys)
	return proven
}

func unresolvedScopeInRange(scope unresolvedScope, client string, from, to time.Time, model string, allow costPairFilter) (bool, bool) {
	owner := strings.TrimSpace(scope.ClientName)
	known := owner != "" && !strings.EqualFold(owner, "unknown")
	if client != "" && known && owner != client {
		return false, false
	}
	if !scope.Timestamp.IsZero() {
		hour := scope.Timestamp.UTC().Truncate(time.Hour)
		if hour.Before(from.UTC().Truncate(time.Hour)) || hour.After(to.UTC().Truncate(time.Hour)) {
			return false, false
		}
	}
	candidate := strings.ToLower(strings.TrimSpace(scope.Model))
	target := strings.ToLower(strings.TrimSpace(scope.Target))
	if model != "" && candidate != "" && model != candidate {
		return false, false
	}
	if allow != nil && candidate != "" && target != "" && !allow.includes(target, candidate) {
		return false, false
	}
	return true, !known || scope.Timestamp.IsZero()
}

func mergeUnresolvedScopes(a, b []unresolvedScope) []unresolvedScope {
	for i, old := range a {
		if old.RootRecordID != "" {
			continue
		}
		matches := 0
		var replacement unresolvedScope
		for _, candidate := range b {
			if old.ClientName == candidate.ClientName && old.Timestamp.Equal(candidate.Timestamp) && old.Model == candidate.Model && old.Target == candidate.Target && (old.RecordID == "" || old.RecordID == candidate.RecordID) {
				matches++
				replacement = candidate
			}
		}
		if matches == 1 {
			a = append([]unresolvedScope(nil), a...)
			a[i] = replacement
		}
	}
	seen := make(map[unresolvedScope]bool, len(a)+len(b))
	for _, scope := range append(append([]unresolvedScope(nil), a...), b...) {
		scope.Timestamp = scope.Timestamp.UTC()
		scope.ClientName = strings.TrimSpace(scope.ClientName)
		scope.Model = strings.ToLower(strings.TrimSpace(scope.Model))
		scope.Target = strings.ToLower(strings.TrimSpace(scope.Target))
		seen[scope] = true
	}
	out := make([]unresolvedScope, 0, len(seen))
	for scope := range seen {
		out = append(out, scope)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.Timestamp.Equal(b.Timestamp) {
			return a.Timestamp.Before(b.Timestamp)
		}
		if a.ClientName != b.ClientName {
			return a.ClientName < b.ClientName
		}
		if a.RecordID != b.RecordID {
			return a.RecordID < b.RecordID
		}
		if a.RootRecordID != b.RootRecordID {
			return a.RootRecordID < b.RootRecordID
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		return a.Model < b.Model
	})
	return out
}

func equalUnresolvedScopes(a, b []unresolvedScope) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The candidate and a conflicting committed bundle can cover different
// clients or models. Never infer dimensions that are absent from retained data.
func committedConflictScopes(tx *bolt.Tx, event usage.Event) []unresolvedScope {
	if strings.TrimSpace(event.RecordID) == "" || event.Timestamp.IsZero() {
		return nil
	}
	rows, keys, err := normalizeUsageBundle(event)
	if err != nil {
		return nil
	}
	raw := tx.Bucket([]byte(BucketUsageEvents))
	if raw == nil {
		return nil
	}
	var scopes []unresolvedScope
	for i, row := range rows {
		digest, err := usage.RetryEquivalentDigest(row)
		if err != nil {
			continue
		}
		_, _, _, lookupErr := lookupUsageIdentity(tx, []byte(keys[i]), []byte(digest))
		if lookupErr == nil {
			continue
		}
		data := raw.Get([]byte(keys[i]))
		if data != nil {
			var stored usage.Event
			if decodeProofEvent(data, &stored) && usageKey(stored.Timestamp, stored.RecordID) == keys[i] {
				if i == 0 && !stored.Supplementary && stored.ParentRecordID == "" {
					storedRows, _, err := normalizeUsageBundle(stored)
					if err == nil {
						for _, storedRow := range storedRows {
							scopes = append(scopes, normalizedUnresolvedScope(storedRow))
						}
						continue
					}
				}
				scopes = append(scopes, normalizedUnresolvedScope(stored))
				continue
			}
			scope, _ := backfillRawScope(data)
			scopes = append(scopes, normalizedUnresolvedScope(scope))
			continue
		}
		if errors.Is(lookupErr, ErrUsageRecordConflict) || errors.Is(lookupErr, ErrUsageIdentityUnverified) {
			scope := unresolvedScope{Timestamp: row.Timestamp.UTC(), RecordID: row.RecordID, RootRecordID: rows[0].RecordID}
			if usageKey, quotaKey, ok := readReceiptCells(tx, []byte(keys[i])); ok {
				if actual, err := decodeAggKey(usageKey); err == nil && actual.dimsOK {
					parts := strings.Split(string(quotaKey), "|")
					scope = unresolvedScope{ClientName: actual.client, Timestamp: row.Timestamp.UTC(), RecordID: row.RecordID, RootRecordID: rows[0].RecordID, Model: quotaKeyDecode(parts[3]), Target: quotaKeyDecode(parts[2])}
				}
			} else if owner, ok := readReceiptOwner(tx, []byte(keys[i])); ok && owner == receiptClientHash(row.ClientName) {
				scope.ClientName = strings.TrimSpace(row.ClientName)
			}
			scopes = append(scopes, scope)
		} else {
			scopes = append(scopes, unresolvedScope{Timestamp: row.Timestamp.UTC(), RecordID: row.RecordID, RootRecordID: rows[0].RecordID})
		}
	}
	return mergeUnresolvedScopes(nil, scopes)
}
