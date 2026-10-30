package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// Issue #125 phase P2: client-filtered quota reads enumerate the client's own
// objects through the v1 client index (raw rows) and the quota bucket's
// hour-prefixed client field (cells, proofs), instead of walking every
// client's rows in the window. The scope is only attached after the probe
// proves the enumeration complete for the read window (see
// probeClientScopedRead); when it is, every walk keeps visiting the
// classification's anomalous keys A, so the verdict equals the exhaustive
// read. Every fallback and every replay is counted under
// integrityFallbackClientScope.

// clientScopedRead is a successful probe: the scope for the read context and
// the window hours the scoped index block assembles.
type clientScopedRead struct {
	scope    *integrityClientScope
	from, to time.Time // window hour bounds as the exhaustive index block sees them
}

// errClientScopedReplay asks readCostUnitsInScope to replay the whole read
// without the scope. A scoped attempt that fails proof verification or cost
// assembly can have reached a different first error than the exhaustive path,
// so it is discarded rather than reported.
var errClientScopedReplay = errors.New("client-scoped read must replay without scope")

type noClientScopeKey struct{}

func clientScopeDisabled(ctx context.Context) bool {
	disabled, _ := ctx.Value(noClientScopeKey{}).(bool)
	return disabled
}

// receiptsInHourRange reports whether any receipt key falls inside the hour
// range [start, end). Receipts are written only by pruning; a receipt in the
// window means raw rows of the window were removed and cannot be enumerated
// through the client index.
func receiptsInHourRange(tx *bolt.Tx, start, end time.Time) bool {
	b := tx.Bucket([]byte(BucketUsageReceipts))
	if b == nil {
		return false
	}
	k, _ := b.Cursor().Seek([]byte(start.UTC().Format(hourLayout)))
	return k != nil && string(k) < end.UTC().Format(hourLayout)
}

// quotaOwnerHourRange bounds the quota aggregation keys of one hour whose
// owner field is ownerEnc. The owner field is delimited by '|' and never
// contains one, so every key starting with the prefix has that owner; the
// prefix's last byte is incremented for the upper bound, which covers every
// suffix (including invalid UTF-8) because the differing byte is the
// delimiter itself.
func quotaOwnerHourRange(hour time.Time, ownerEnc string) keyRange {
	lo := []byte(hour.UTC().Format(hourLayout) + "|" + ownerEnc + "|")
	hi := bytes.Clone(lo)
	hi[len(hi)-1] = '}'
	return keyRange{lo: lo, hi: hi}
}

// staleForeignProof reports the index-key prefix (client field and delimiter)
// of a readable index proof that no longer matches its raw row and names a
// different client's index space. The exhaustive cost evidence scan rejects
// exactly that shape for the client the proof names
// (observeCostRawHour's invalid attributable branch), while a client-scoped
// read skips the row; the probe declines for the named client instead. The
// read's own progress is used, so a proof this snapshot cannot read is not
// attributable and needs no fallback: the entry-level walks already fail such
// rows closed when they are unowned.
func staleForeignProof(tx *bolt.Tx, p *indexProgress, key []byte, event usage.Event) ([]byte, bool) {
	if p == nil || tx.Bucket([]byte(indexProofsV2)) == nil {
		return nil, false
	}
	proof, err := readIndexProof(tx.Bucket([]byte(indexProofsV2)), p, key)
	if err != nil {
		return nil, false
	}
	if bytes.Equal(proof.IndexKey, clientEventIndexKey(event, key)) {
		return nil, false
	}
	if bytes.HasPrefix(proof.IndexKey, []byte(clientIndexPrefix(event.ClientName))) {
		return nil, false
	}
	i := bytes.IndexByte(proof.IndexKey, '|')
	if i < 0 {
		return nil, false
	}
	return bytes.Clone(proof.IndexKey[:i+1]), true
}

// Client-scope probe instrumentation. The probe enumerates the filter client
// and the empty client for every window hour: one hour-state read and one
// index range seek per prefix, plus one decode per empty-client row. The
// extensibility guards count these through the hooks so the
// O(clients × window hours) enumeration is visible next to the object visits;
// production leaves them nil.
var (
	clientScopeProbeHourState func()
	clientScopeProbeRangeSeek func()
	clientScopeProbeDecoded   func()
)

// probeClientScopedRead enumerates one client's window through the client
// index. Nil means the scope cannot be proven complete and the caller counts
// a fallback before replaying the exhaustive read:
//
//   - the classification must have classified the client index under the
//     read's own v2 progress; checkOrphans abandons its fast index walk
//     otherwise, so the foreign-proof fields would not describe this read;
//   - fast must carry hour-prefix bounds (a comparable window), and
//     requireClientIndex must pass (done marker, both index buckets, no
//     quarantined rows): without it the index block would fall back anyway;
//   - a v2 progress that is still scanning cannot vouch for the index;
//   - the window's hour text must be UTC text: the index and quota keys are
//     built from UTC hour text, while the exhaustive hour loop prints its own
//     bounds, so a non-UTC bound would make the enumeration disagree;
//   - a receipt inside the window may be the only evidence of pruned rows;
//   - a window-exempt raw row without its own index entry (rawUnindexed) or a
//     decodable anomalous raw row in the window's textual range cannot be
//     reproduced by the scoped path: the skipped cost evidence scan judges
//     both (direct evidence, proof identity, parity), so the scope declines;
//   - a readable proof that the exhaustive walks judge relevant to this client
//     (foreignProofs) makes the read fail closed or take a different first
//     error there; the scoped walk skips the object, so the probe declines.
//     The scopes are evaluated with unresolvedScopeInRange, the same predicate
//     checkOrphans uses, so an empty or "unknown" owner declines for every
//     client. A bounded overflow of distinct proofs declines unconditionally;
//   - an hour whose index state count or digest does not match the entries
//     prevents both enumeration and the index block;
//   - a row whose enumerated hour (its index entry's event hour) differs from
//     the hour of its raw key was moved by an offline rewrite; the skipped
//     evidence scan, which ranges over key text and compares hour states, can
//     no longer account for it, so the scope declines.
//
// The probe passes no hooks: its reads are validation, not read visits. The
// scoped index block repeats them with the read's hooks and proof
// verification, so a disagreement replays instead of reporting.
func probeClientScopedRead(ctx context.Context, tx *bolt.Tx, client string, start, end time.Time, progress *indexProgress, fast *integrityFast) *clientScopedRead {
	if fast == nil || fast.empty || fast.scope != nil {
		return nil
	}
	// checkOrphans abandons the fast index walk in these two situations, so the
	// classification's index fields were never filled or describe a different
	// v2 progress than the read's. The probe trusts rawUnindexed and
	// foreignProofs, so it must decline here and let the exhaustive read decide.
	if !fast.class.indexClassified || !sameIndexProgress(fast.class.indexProgress, progress) {
		return nil
	}
	if fast.lo == nil || fast.hi == nil {
		return nil
	}
	if start.Format(hourLayout) != start.UTC().Format(hourLayout) || end.Format(hourLayout) != end.UTC().Format(hourLayout) {
		return nil
	}
	if err := requireClientIndex(tx); err != nil {
		return nil
	}
	if progress != nil && progress.Phase != "complete" {
		return nil
	}
	if receiptsInHourRange(tx, start, end) {
		return nil
	}
	if fast.class.rawUnindexed {
		return nil
	}
	// checkOrphans compares unresolvedScopeInRange against [from, to] as the
	// cost read's hour bounds, while start/end span [from, to] plus the final
	// hour; end minus one hour restores the comparable upper bound. The shape
	// probeClientScopedRead serves fixes model to "" and allow to nil, so the
	// call is the same one checkOrphans makes for this read.
	if fast.class.foreignProofs.relevant(client, start, end.Add(-time.Hour)) {
		return nil
	}
	// The exhaustive index block precedes its evidence scan; that scan runs
	// over every decodable raw row of the window in key-text range and judges
	// anomalous rows too. A decodable anomalous row in that range can fail the
	// read closed (or force the legacy scan) in ways the scoped index block
	// does not reproduce, so the scope declines.
	raw := tx.Bucket([]byte(BucketUsageEvents))
	loText, hiText := []byte(start.Format(hourLayout)), []byte(end.Format(hourLayout))
	for _, key := range fast.class.events {
		if bytes.Compare(key, loText) < 0 || bytes.Compare(key, hiText) >= 0 {
			continue
		}
		value := raw.Get(key)
		if value == nil {
			continue
		}
		var event usage.Event
		if json.Unmarshal(value, &event) != nil {
			continue
		}
		return nil
	}
	client = strings.TrimSpace(client)
	scope := &integrityClientScope{client: client}
	seen := make(map[string]bool)
	// Empty-client rows have an unknown quota owner, which fails every
	// client-filtered read inside the window; the index enumerates them under
	// the empty client prefix, and their raw rows must take part in the walks.
	// Reading every window hour for both clients keeps the enumeration exact:
	// readClientHourRowsInTx verifies each hour's count, digest and pointer
	// identities, and an hour with no records is the zero state.
	for hour := start; hour.Before(end); hour = hour.Add(time.Hour) {
		if err := ctx.Err(); err != nil {
			return nil
		}
		for _, c := range []string{client, ""} {
			if clientScopeProbeHourState != nil {
				clientScopeProbeHourState()
			}
			if clientScopeProbeRangeSeek != nil {
				clientScopeProbeRangeSeek()
			}
			rows, err := readClientHourRowsInTx(ctx, tx, c, hour, nil, nil)
			if err != nil {
				return nil
			}
			if c == "" && clientScopeProbeDecoded != nil {
				for range rows {
					clientScopeProbeDecoded()
				}
			}
			for _, row := range rows {
				// An index entry carries the row's event hour, while the raw key
				// carries the hour of its own stamp; they agree unless an offline
				// ReindexUsageEvent-style rewrite moved the body (and the index
				// entry, hour state and rollups) to another hour. The skipped
				// evidence scan observes rows by key text and compares the hour
				// states the index builds from event hours against them, so such
				// a row is invisible to it while it still counts in the state; the
				// scoped index block would assemble it and return a snapshot the
				// exhaustive read fails closed on (or rejects in the legacy
				// parity check). A key whose hour cannot be derived is equally
				// unprovable, so both decline the scope.
				if keyHour, ok := canonicalEventKeyHour(row.key); !ok || !keyHour.Equal(hour) {
					return nil
				}
				if key := string(row.key); !seen[key] {
					seen[key] = true
					scope.rawKeys = append(scope.rawKeys, row.key)
				}
			}
		}
	}
	sort.Slice(scope.rawKeys, func(i, j int) bool { return bytes.Compare(scope.rawKeys[i], scope.rawKeys[j]) < 0 })
	for hour := start; hour.Before(end); hour = hour.Add(time.Hour) {
		scope.quotaRanges = append(scope.quotaRanges,
			quotaOwnerHourRange(hour, quotaKeyEncode(client)),
			quotaOwnerHourRange(hour, ""),
		)
	}
	scope.quotaRanges = sortKeyRanges(scope.quotaRanges)
	return &clientScopedRead{scope: scope, from: start, to: end}
}

// readCostRollupsClientHour reads the quota cells of one client for one hour.
// The client field of a quota aggregation key is delimited, so the prefix scan
// visits exactly the cells quotaKeyInScope accepts for that client (other
// clients' cells answer false before any cell is decoded). Empty-owner cells
// are not part of this client's prefix; they error in the earlier quota-key
// visit, which is why the classification keeps them out of the neutral set
// and the scope walks their range too.
func readCostRollupsClientHour(ctx context.Context, tx *bolt.Tx, filter usage.Filter, hour time.Time, allow costPairFilter) ([]TargetModelCell, error) {
	b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
	if b == nil {
		return nil, nil
	}
	r := quotaOwnerHourRange(hour, quotaKeyEncode(strings.TrimSpace(filter.ClientName)))
	var result []TargetModelCell
	c := b.Cursor()
	for k, v := c.Seek(r.lo); k != nil && bytes.Compare(k, r.hi) < 0; k, v = c.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		inScope, err := quotaKeyInScope(k, filter, hour, hour, allow)
		if err != nil {
			return nil, err
		}
		if !inScope {
			continue
		}
		parts := strings.Split(string(k), "|")
		at, _ := parseAggHour(parts[0])
		owner, currentModel := quotaKeyDecode(parts[1]), quotaKeyDecode(parts[3])
		target := quotaKeyDecode(parts[2])
		var cell AggCell
		if json.Unmarshal(v, &cell) != nil {
			return nil, fmt.Errorf("%w: invalid quota aggregation", ErrUsageHistoryIncomplete)
		}
		result = append(result, TargetModelCell{Hour: at, Client: owner, Target: target, Model: currentModel, AggCell: cell})
	}
	return result, nil
}

// runClientScopedIndex is the scoped counterpart of the exhaustive client
// index block. It keeps the block's per-hour order and error classes: a
// rollup read error is returned as-is, while any index or assembly error asks
// for a replay (the exhaustive block would fall back to the legacy scan, or
// select its own first error, in the same situations). Every row of the
// filter client is proof-verified, not only the rows the exhaustive block
// calls relevant: the exhaustive evidence scan also judges the proofs of
// stable-priced rows, and a replay settles both cases exactly.
func runClientScopedIndex(ctx context.Context, tx *bolt.Tx, filter usage.Filter, resolver usage.RateResolver, precise bool, refs *indexRawReferences, progress *indexProgress, scoped *clientScopedRead, hooks costReadHooks) ([]CostUnit, error) {
	client := strings.TrimSpace(filter.ClientName)
	verify := func(key, data []byte, event usage.Event) error {
		_, _, invalid, _, err := indexProofForRaw(ctx, tx, refs, progress, key, data, event)
		if err != nil {
			return err
		}
		if invalid {
			return indexV2Error("invalid client index proof")
		}
		return nil
	}
	var units []CostUnit
	for hour := scoped.from; hour.Before(scoped.to); hour = hour.Add(time.Hour) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := readClientHourRowsInTx(ctx, tx, client, hour, hooks.decoded, verify)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errClientScopedReplay
		}
		events := make([]usage.Event, len(rows))
		for i, row := range rows {
			events[i] = row.event
		}
		cells, err := readCostRollupsClientHour(ctx, tx, filter, hour, nil)
		if err != nil {
			return nil, err
		}
		part, err := assembleCostUnits(ctx, cells, events, filter, resolver, precise, nil)
		if err != nil {
			return nil, errClientScopedReplay
		}
		units = append(units, part...)
	}
	return units, nil
}
