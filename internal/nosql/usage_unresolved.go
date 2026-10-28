package nosql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

const unresolvedBucket = "usage_unresolved_events_v1"
const unresolvedInitialized = "usage_unresolved_events_initialized_v1"
const unresolvedCanonicalKeys = "usage_unresolved_events_canonical_v10"

type unresolvedScope struct {
	ClientName   string    `json:"client_name,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
	RecordID     string    `json:"record_id,omitempty"`
	RootRecordID string    `json:"root_record_id,omitempty"`
	Model        string    `json:"model,omitempty"`
	Target       string    `json:"target,omitempty"`
}

type unresolvedUsage struct {
	Event           usage.Event       `json:"event"`
	Original        []byte            `json:"original"`
	OriginalKey     []byte            `json:"original_key,omitempty"`
	Resolved        bool              `json:"resolved,omitempty"`
	Kind            string            `json:"kind,omitempty"`
	CommittedScopes []unresolvedScope `json:"committed_scopes,omitempty"`
	OriginalScopes  []unresolvedScope `json:"original_scopes,omitempty"`
}

func unresolvedState(tx *bolt.Tx) (*bolt.Bucket, error) {
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil {
		return nil, errors.New("usage unresolved: meta bucket missing")
	}
	marker := meta.Get([]byte(unresolvedInitialized))
	b := tx.Bucket([]byte(unresolvedBucket))
	if marker != nil && (string(marker) != "1" || b == nil) {
		return nil, fmt.Errorf("%w: unresolved evidence source missing or invalid", ErrUsageAggregateSourceMissing)
	}
	if marker == nil && b != nil {
		return nil, fmt.Errorf("%w: unresolved evidence marker missing", ErrUsageAggregateSourceMissing)
	}
	return b, nil
}

func putUnresolvedUsage(tx *bolt.Tx, event usage.Event, original []byte) error {
	return putUnresolvedEvidence(tx, unresolvedUsage{Event: event, Original: original})
}

func putUnresolvedEvidence(tx *bolt.Tx, item unresolvedUsage) error {
	if item.Kind == "" {
		if strings.TrimSpace(item.Event.RecordID) != "" && !item.Event.Timestamp.IsZero() {
			if rows, _, err := normalizeUsageBundle(item.Event); err == nil && len(rows) > 0 {
				item.Event = rows[0]
			}
		}
		item.CommittedScopes = mergeUnresolvedScopes(item.CommittedScopes, committedConflictScopes(tx, item.Event))
	}
	b, err := unresolvedState(tx)
	if err != nil {
		return err
	}
	if b == nil {
		b, err = tx.CreateBucket([]byte(unresolvedBucket))
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketMeta)).Put([]byte(unresolvedInitialized), []byte("1")); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketMeta)).Put([]byte(unresolvedCanonicalKeys), []byte("1")); err != nil {
			return err
		}
	}
	if item.Original == nil {
		item.Original, err = json.Marshal(item.Event)
		if err != nil {
			return err
		}
	}
	if item.Kind == "" {
		item.OriginalScopes = mergeUnresolvedScopes(item.OriginalScopes, ambiguousOriginalScopes(item))
	}
	if item.Resolved && !unresolvedItemProven(tx, item) {
		item.Resolved = false
	}
	key, data, err := unresolvedEvidenceKey(item)
	if err != nil {
		return err
	}
	if previous := b.Get(key); previous != nil {
		var stored unresolvedUsage
		if err := json.Unmarshal(previous, &stored); err != nil {
			return fmt.Errorf("%w: invalid unresolved evidence", ErrUsageHistoryIncomplete)
		}
		resolved := stored.Resolved
		if item.Resolved {
			resolved = true
		} else if stored.Resolved && !unresolvedItemProven(tx, stored) {
			resolved = false
		}
		merged := mergeUnresolvedScopes(stored.CommittedScopes, item.CommittedScopes)
		originalScopes := mergeUnresolvedScopes(stored.OriginalScopes, item.OriginalScopes)
		if resolved == stored.Resolved && equalUnresolvedScopes(stored.CommittedScopes, merged) && equalUnresolvedScopes(stored.OriginalScopes, originalScopes) {
			return nil
		}
		stored.Resolved = resolved
		stored.CommittedScopes = merged
		stored.OriginalScopes = originalScopes
		updated, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		return b.Put(key, updated)
	}
	return b.Put(key, data)
}

func unresolvedItemProven(tx *bolt.Tx, item unresolvedUsage) bool {
	proven, _ := unresolvedItemProvenContext(context.Background(), tx, item)
	return proven
}

func unresolvedItemProvenContext(ctx context.Context, tx *bolt.Tx, item unresolvedUsage) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	rows, keys, valid := recheckEvidenceBundle(tx, item)
	if !valid {
		return false, ctx.Err()
	}
	if !proofIdentitiesPresent(ctx, tx, keys) {
		return false, ctx.Err()
	}
	committed, inspectErr := inspectUsageBundleTx(tx, rows, keys, nil)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !committed && !errors.Is(inspectErr, ErrUsageRecordConflict) {
		return false, nil
	}
	return usageAccountingProvenContext(ctx, tx, rows, keys)
}

func unresolvedEvidenceKey(item unresolvedUsage) ([]byte, []byte, error) {
	data, err := json.Marshal(item)
	if err != nil {
		return nil, nil, err
	}
	digest, err := usage.RetryEquivalentDigest(item.Event)
	if err != nil {
		return nil, nil, err
	}
	identity := item
	identity.Resolved = false
	identity.CommittedScopes = nil
	identity.OriginalScopes = nil
	identityData, err := json.Marshal(identity)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(identityData)
	prefix := "u|"
	if item.Kind == "" && strings.TrimSpace(item.Event.RecordID) != "" && !item.Event.Timestamp.IsZero() {
		prefix = "r|" + usageKey(item.Event.Timestamp, item.Event.RecordID) + "|"
	}
	key := append(append([]byte(prefix+digest), '|'), sum[:]...)
	return key, data, nil
}

func canonicalizeUnresolvedKeys(tx *bolt.Tx, b *bolt.Bucket) error {
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil {
		return errors.New("usage unresolved: meta bucket missing")
	}
	if marker := meta.Get([]byte(unresolvedCanonicalKeys)); marker != nil {
		if string(marker) != "1" {
			return fmt.Errorf("%w: invalid unresolved key marker", ErrUsageHistoryIncomplete)
		}
		return nil
	}
	var oldKeys [][]byte
	items := make(map[string]unresolvedUsage)
	if err := b.ForEach(func(k, raw []byte) error {
		var item unresolvedUsage
		if json.Unmarshal(raw, &item) != nil {
			return fmt.Errorf("%w: invalid unresolved evidence", ErrUsageHistoryIncomplete)
		}
		if item.Kind == "" {
			item.OriginalScopes = mergeUnresolvedScopes(item.OriginalScopes, ambiguousOriginalScopes(item))
		}
		if item.Kind == "" && strings.TrimSpace(item.Event.RecordID) != "" && !item.Event.Timestamp.IsZero() {
			rows, _, err := normalizeUsageBundle(item.Event)
			if err == nil {
				item.Event = rows[0]
				item.CommittedScopes = mergeUnresolvedScopes(item.CommittedScopes, committedConflictScopes(tx, item.Event))
				if item.Resolved {
					item.Resolved = unresolvedItemProven(tx, item)
				}
			} else if item.Resolved {
				item.Resolved = false
			}
		} else if item.Kind == "" && item.Resolved {
			item.Resolved = false
		} else if item.Kind != "" && item.Resolved {
			item.Resolved = false
		}
		key, _, err := unresolvedEvidenceKey(item)
		if err != nil {
			return err
		}
		oldKeys = append(oldKeys, append([]byte(nil), k...))
		if prior, ok := items[string(key)]; ok {
			item.Resolved = item.Resolved || prior.Resolved
			item.CommittedScopes = mergeUnresolvedScopes(item.CommittedScopes, prior.CommittedScopes)
			item.OriginalScopes = mergeUnresolvedScopes(item.OriginalScopes, prior.OriginalScopes)
		}
		items[string(key)] = item
		return nil
	}); err != nil {
		return err
	}
	for _, key := range oldKeys {
		if err := b.Delete(key); err != nil {
			return err
		}
	}
	for key, item := range items {
		value, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(key), value); err != nil {
			return err
		}
	}
	return meta.Put([]byte(unresolvedCanonicalKeys), []byte("1"))
}

func resolveUnresolvedUsage(tx *bolt.Tx, event usage.Event) error {
	b, err := unresolvedState(tx)
	if err != nil || b == nil {
		return err
	}
	if err := canonicalizeUnresolvedKeys(tx, b); err != nil {
		return err
	}
	cursor := b.Cursor()
	prefix := []byte("r|" + usageKey(event.Timestamp, event.RecordID) + "|")
	for k, raw := cursor.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, raw = cursor.Next() {
		var item unresolvedUsage
		if json.Unmarshal(raw, &item) != nil {
			return fmt.Errorf("%w: invalid unresolved evidence", ErrUsageHistoryIncomplete)
		}
		proven := unresolvedItemProven(tx, item)
		if item.Resolved == proven {
			continue
		}
		item.Resolved = proven
		data, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if err := b.Put(k, data); err != nil {
			return err
		}
	}
	return nil
}

// RegisterUnresolvedUsage preserves the original journal line before it can be removed.
func (s *UsageStore) RegisterUnresolvedUsage(event usage.Event, original []byte) error {
	if s == nil || s.db == nil {
		return errors.New("usage unresolved: store not initialized")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		item := unresolvedUsage{Event: event, Original: original}
		if strings.TrimSpace(event.RecordID) != "" && !event.Timestamp.IsZero() {
			rows, _, err := normalizeUsageBundle(event)
			if err == nil {
				item.Event = rows[0]
			}
		}
		if item.Original == nil {
			data, err := json.Marshal(item.Event)
			if err != nil {
				return err
			}
			item.Original = data
		}
		if unresolvedItemProven(tx, item) {
			rows, keys, _ := recheckEvidenceBundle(tx, item)
			if recheckCommittedBundle(tx, rows, keys) {
				if err := resolveUnresolvedUsage(tx, rows[0]); err != nil {
					return err
				}
			}
			item.Resolved = true
		}
		return putUnresolvedEvidence(tx, item)
	})
}

func (s *UsageStore) preserveInvalidUsage(event usage.Event, rejection error) error {
	if s == nil || s.db == nil {
		return rejection
	}
	original, err := json.Marshal(event)
	if err == nil {
		err = s.RegisterUnresolvedUsage(event, original)
	}
	return errors.Join(rejection, err)
}

func (j *UsageJournal) preserveInvalidUsage(event usage.Event, rejection error) error {
	registrar, ok := j.primary.(interface {
		RegisterUnresolvedUsage(usage.Event, []byte) error
	})
	original, err := json.Marshal(event)
	if err == nil && ok {
		err = registrar.RegisterUnresolvedUsage(event, original)
	} else if err == nil {
		err = errors.New("usage journal: primary cannot register unresolved usage")
	}
	if err != nil {
		j.mu.Lock()
		appendErr := j.appendLocked(event)
		j.mu.Unlock()
		return errors.Join(rejection, err, appendErr)
	}
	return rejection
}

// Register sidecars produced by older versions before accepting traffic.
func (j *UsageJournal) registerExistingSidecars() error {
	for _, suffix := range []string{usageJournalRejectedSuffix, ".corrupt"} {
		data, err := os.ReadFile(j.path + suffix)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: read usage sidecar: %v", ErrUsageAggregateSourceMissing, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			raw := []byte(line)
			if suffix == usageJournalRejectedSuffix {
				var entry rejectedUsageLine
				if json.Unmarshal(raw, &entry) != nil || len(entry.Line) == 0 {
					return fmt.Errorf("%w: invalid rejected usage sidecar", ErrUsageAggregateSourceMissing)
				}
				raw = entry.Line
			}
			var event usage.Event
			scope, ambiguous := backfillRawScope(raw)
			if ambiguous || json.Unmarshal(raw, &event) != nil {
				event = scope
			}
			registrar, ok := j.primary.(interface {
				RegisterUnresolvedUsage(usage.Event, []byte) error
			})
			if !ok {
				return fmt.Errorf("%w: primary cannot register usage sidecar", ErrUsageAggregateSourceMissing)
			}
			if err := registrar.RegisterUnresolvedUsage(event, raw); err != nil {
				return fmt.Errorf("%w: register usage sidecar: %v", ErrUsageAggregateSourceMissing, err)
			}
		}
	}
	return nil
}

func checkUnresolvedUsage(tx *bolt.Tx, client string, from, to time.Time) error {
	return checkUnresolvedUsageInScope(tx, client, from, to, "", nil)
}

func checkUnresolvedUsageInScope(tx *bolt.Tx, client string, from, to time.Time, model string, allow costPairFilter) error {
	return checkUnresolvedUsageInScopeContext(context.Background(), tx, client, from, to, model, allow)
}

func checkUnresolvedUsageInScopeContext(ctx context.Context, tx *bolt.Tx, client string, from, to time.Time, model string, allow costPairFilter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := unresolvedState(tx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUsageHistoryIncomplete, err)
	}
	if b == nil {
		return nil
	}
	client = strings.TrimSpace(client)
	model = strings.ToLower(strings.TrimSpace(model))
	refs := &indexRawReferences{tx: tx}
	return b.ForEach(func(_, raw []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var item unresolvedUsage
		if json.Unmarshal(raw, &item) != nil {
			return fmt.Errorf("%w: %w: invalid unresolved evidence", ErrUsageHistoryIncomplete, ErrUsageAggregateSourceMissing)
		}
		if item.Kind == "prune_unverified" && len(item.OriginalKey) != 0 {
			var event usage.Event
			if json.Unmarshal(item.Original, &event) != nil {
				if err := checkMalformedIndexScope(ctx, tx, refs, item.OriginalKey, item.Original, client, from, to, model, allow); err != nil {
					return err
				}
			}
			owner, known, err := readRawOwner(tx, item.OriginalKey)
			if err != nil {
				return err
			}
			scope, ambiguous := backfillRawScope(item.Original)
			if known && ambiguous && json.Valid(item.Original) {
				return fmt.Errorf("%w: ambiguous raw owner evidence", ErrUsageHistoryIncomplete)
			}
			if known && !ambiguous && scope.ClientName != "" && receiptClientHash(scope.ClientName) != owner.Client {
				return fmt.Errorf("%w: raw owner and evidence disagree", ErrUsageHistoryIncomplete)
			}
			if known && client != "" && owner.Client != receiptClientHash(client) {
				return nil
			}
			var original usage.Event
			digest, digestErr := "", error(nil)
			if known && json.Unmarshal(item.Original, &original) == nil && usageKey(original.Timestamp, original.RecordID) == string(item.OriginalKey) {
				digest, digestErr = usage.RetryEquivalentDigest(original)
			}
			if known && (digestErr != nil || digest != owner.Digest) {
				stamp, _, found := strings.Cut(string(item.OriginalKey), "_")
				at, err := time.Parse(time.RFC3339Nano, stamp)
				if found && err == nil && (at.UTC().Truncate(time.Hour).Before(from.UTC().Truncate(time.Hour)) || at.UTC().Truncate(time.Hour).After(to.UTC().Truncate(time.Hour))) {
					return nil
				}
				return fmt.Errorf("%w: unverified retained raw usage", ErrUsageHistoryIncomplete)
			}
		}
		scopes := []unresolvedScope{normalizedUnresolvedScope(item.Event)}
		for _, component := range item.Event.Components {
			if err := ctx.Err(); err != nil {
				return err
			}
			scope := normalizedUnresolvedScope(item.Event)
			scope.Model = strings.ToLower(strings.TrimSpace(component.Model))
			scopes = append(scopes, scope)
		}
		scopes = append(scopes, item.OriginalScopes...)
		candidateCount := 1 + len(item.Event.Components) + len(item.OriginalScopes)
		scopes = append(scopes, item.CommittedScopes...)
		allKnown := true
		for _, scope := range scopes {
			if err := ctx.Err(); err != nil {
				return err
			}
			owner := strings.TrimSpace(scope.ClientName)
			if owner == "" || strings.EqualFold(owner, "unknown") || scope.Timestamp.IsZero() {
				allKnown = false
				break
			}
		}
		relevant, unknown := false, false
		for i, scope := range scopes {
			if err := ctx.Err(); err != nil {
				return err
			}
			intersects, missing := unresolvedScopeInRange(scope, client, from, to, model, allow)
			if err := ctx.Err(); err != nil {
				return err
			}
			if intersects && i >= candidateCount {
				proven, err := committedScopeProvenContext(ctx, tx, scope)
				if err != nil {
					return err
				}
				if proven {
					continue
				}
			}
			if intersects {
				relevant = true
				unknown = unknown || missing
			}
		}
		if !relevant {
			return nil
		}
		if item.Resolved && allKnown {
			proven, err := unresolvedItemProvenContext(ctx, tx, item)
			if err != nil {
				return err
			}
			if proven {
				return nil
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if unknown {
			return fmt.Errorf("%w: %w: unresolved evidence lacks client or time", ErrUsageHistoryIncomplete, ErrUsageAggregateSourceMissing)
		}
		if relevant {
			return fmt.Errorf("%w: unresolved usage event", ErrUsageHistoryIncomplete)
		}
		return nil
	})
}

type proofScanHooksKey struct{}

func proofScanStep(ctx context.Context, receipt bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if hooks, _ := ctx.Value(proofScanHooksKey{}).(*directIntegrityHooks); hooks != nil {
		if receipt && hooks.receipt != nil {
			hooks.receipt()
		}
		if !receipt && hooks.raw != nil {
			hooks.raw()
		}
	}
	return ctx.Err()
}

func (s *UsageStore) checkUnresolvedClient(client string) error {
	return s.CheckUnresolvedClientRange(client, time.Time{}, time.Date(9999, 12, 31, 23, 0, 0, 0, time.UTC))
}

// CheckUnresolvedClientRange is used by journal decorators that support scoped calibration.
func (s *UsageStore) CheckUnresolvedClientRange(client string, from, to time.Time) error {
	return s.db.View(func(tx *bolt.Tx) error { return checkUnresolvedUsage(tx, client, from, to) })
}
