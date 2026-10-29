package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// Issue #125: windowed usage integrity reads.
//
// The exhaustive integrity checks (checkDirectQuotaEvidenceContext,
// quotaProofBatchInScopeContext, validateQuotaKeys, checkOrphans and
// checkMissingFreshRootsContext) walk whole buckets. For a query window W the
// verdict only depends on objects whose key hour lies in W plus the anomalous
// objects A: every object for which some branch of those checks may report an
// error or contribute to a cell of W although its own key hour is outside W.
// A is defined by the windowExempt predicates below; an object is in A exactly
// when its predicate is false. The fast walk visits, in key order, the keys of
// W's hours plus A, so every callback the exhaustive walk runs on a skipped key
// is one that returns nil without side effects that W's verdict can observe.
// Unresolved evidence and quarantine stay exhaustive.
//
// The predicates are deliberately stricter than the branches they summarise:
// an object that is in A although it could have been skipped only costs a
// visit, never a different verdict.

// integrityAnomalyLimit bounds |A|; a larger set disables the fast path.
const integrityAnomalyLimit = 10000

type integrityClassification struct {
	db    *bolt.DB
	path  string
	epoch uint64
	txid  int
	// Sorted anomalous keys per key space.
	events   [][]byte
	eventSet map[string]bool
	quota    [][]byte
	proofs   [][]byte // full rollup proof bucket keys (source NUL quota key)
	index    [][]byte
	// indexProgress is the client index progress the index entries were
	// classified under; indexClassified is false when it could not be loaded,
	// and the index walk then stays exhaustive.
	indexProgress   *indexProgress
	indexClassified bool
	anomalous       int
	duration        time.Duration
}

// canonicalEventKeyHour returns the UTC hour of a raw-identity key whose stamp
// round-trips through RFC3339Nano in UTC. Only such keys start with their own
// hour, so only they are reachable by an hour-prefix seek. The id suffix is not
// inspected (legacy rows have numeric ids).
func canonicalEventKeyHour(key []byte) (time.Time, bool) {
	stamp, _, found := strings.Cut(string(key), "_")
	if !found {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || at.UTC().Format(time.RFC3339Nano) != stamp {
		return time.Time{}, false
	}
	return at.UTC().Truncate(time.Hour), true
}

// quotaKeyHour returns the hour of a quota aggregation key. parseAggHour only
// accepts the exact hourLayout text, so a parseable key starts with its hour.
func quotaKeyHour(key []byte) (time.Time, bool) {
	text := string(key)
	if i := strings.IndexByte(text, '|'); i >= 0 {
		text = text[:i]
	}
	hour, err := parseAggHour(text)
	return hour, err == nil
}

// rawRowWindowExempt reports whether a raw row with a canonical key is inert
// for every window that excludes its key hour. Proof per branch (key hour H):
//
//   - checkDirectQuotaEvidenceContext raw loop: readRawOwner and the encoding
//     checks error at any hour, so the owner must be valid and the bytes valid
//     UTF-8 JSON with a non-zero timestamp. With usageKey(event) == key the
//     event's own hour is H, so every quota/usage cell it accumulates or checks
//     is outside W and quotaKeyInScope answers (false, nil) for them; a bound
//     owner must agree with the event, or the "owner and event disagree"
//     branch may fire whatever the hour (numeric ids are never bound-exempt).
//   - quotaProofBatchInScopeContext raw loop: a canonical proof event lands in
//     cells[quotaAggKey(event)], an hour-H key that is never a cell of W. A row
//     that is not a canonical proof event (for example an empty client) loops
//     over the cells with proofRawMayOverlap, which is false for every hour of
//     W when the owner is valid, the scope is not an ambiguous valid JSON
//     document and does not contradict the owner, and the stamp parses to H.
//   - malformed rows (checkMalformedIndexScope) never reach this predicate.
func rawRowWindowExempt(tx *bolt.Tx, key, value []byte) bool {
	owner, owned, err := readRawOwner(tx, key)
	if err != nil || value == nil || !utf8.Valid(value) {
		return false
	}
	var event usage.Event
	if json.Unmarshal(value, &event) != nil || event.Timestamp.IsZero() || usageKey(event.Timestamp, event.RecordID) != string(key) {
		return false
	}
	if owned {
		digest, err := usage.RetryEquivalentDigest(event)
		if err != nil || digest != owner.Digest || receiptClientHash(event.ClientName) != owner.Client {
			return false
		}
	}
	var proof usage.Event
	if decodeProofEventUncounted(value, &proof) && usageKey(proof.Timestamp, proof.RecordID) == string(key) {
		return true
	}
	scope, ambiguous := backfillRawScope(value)
	if ambiguous && json.Valid(value) {
		return false
	}
	if owned && !ambiguous && scope.ClientName != "" && receiptClientHash(scope.ClientName) != owner.Client {
		return false
	}
	return true
}

// eventKeyWindowExempt is the conjunction of the per-bucket predicates for
// one raw-identity key, used where the key is checked in isolation.
//
//   - raw owner loop: an invalid owner errors at any hour; a valid one with a
//     canonical key is skipped by the hour test for windows excluding H.
//   - receipts (both walks): unreadable cells are skipped by the canonical
//     stamp's hour; readable cells carry a quota key of hour H (enforced by
//     readReceiptCells), so check/quotaKeyInScope and the proof cells ignore it.
//   - fresh attestations: skipped by the canonical stamp's hour.
func eventKeyWindowExempt(tx *bolt.Tx, key []byte) bool {
	if _, ok := canonicalEventKeyHour(key); !ok {
		return false
	}
	if _, _, err := readRawOwner(tx, key); err != nil {
		return false
	}
	if raw := tx.Bucket([]byte(BucketUsageEvents)); raw != nil {
		if value := raw.Get(key); value != nil {
			return rawRowWindowExempt(tx, key, value)
		}
		if raw.Bucket(key) != nil {
			return false
		}
	}
	return true
}

// indexEntryWindowExempt reports whether a client index entry is inert in
// checkOrphans for every window excluding its hour H, and whether a windowed
// walk may skip it without changing the reference map (see contains).
//
//   - identity parts and pointer: a malformed identity errors at any hour, and a
//     pointer other than the third part adds a reference for another key.
//   - the entry's own time must be valid and of hour H, the referenced key
//     non-anomalous: the "current" scope then excludes every window without H.
//   - an entry matching its raw row: indexProofForRaw must not consult the
//     reference map (the exhaustive map could fail on an unrelated entry), and
//     an invalid proof must be attributable to a valid original of hour H, or
//     be unattributable for an owned row; otherwise it errors regardless of W.
//   - an entry without a matching raw row is only inert with a trusted proof
//     whose original scope has hour H; the owner shortcuts depend on the
//     client filter and the fallback errors whenever no client is filtered.
func indexEntryWindowExempt(tx *bolt.Tx, p *indexProgress, indexKey, pointer []byte, anomalous func([]byte) bool) bool {
	if pointer == nil || p == nil {
		return false
	}
	parts := strings.SplitN(string(indexKey), "|", 3)
	if len(parts) != 3 || parts[2] == "" {
		return false
	}
	key := []byte(parts[2])
	if !bytes.Equal(pointer, key) {
		return false
	}
	hour, ok := canonicalEventKeyHour(key)
	if !ok || anomalous(key) {
		return false
	}
	current, valid := indexProofScope(indexKey, key)
	if !valid || !current.Timestamp.UTC().Truncate(time.Hour).Equal(hour) {
		return false
	}
	raw := tx.Bucket([]byte(BucketUsageEvents))
	var data []byte
	if raw != nil {
		data = raw.Get(key)
	}
	var event usage.Event
	if data != nil && json.Unmarshal(data, &event) == nil && bytes.Equal(indexKey, clientEventIndexKey(event, key)) {
		consulted := false
		probe := &indexRawReferences{tx: tx, probe: &consulted}
		proof, attributable, invalid, owned, err := indexProofForRaw(context.Background(), tx, probe, p, key, data, event)
		if err != nil || consulted {
			return false
		}
		if !invalid {
			return true
		}
		if attributable {
			original, valid := indexProofScope(proof.IndexKey, key)
			return valid && original.Timestamp.UTC().Truncate(time.Hour).Equal(hour)
		}
		return owned
	}
	proofs := tx.Bucket([]byte(indexProofsV2))
	if proofs == nil {
		return false
	}
	proof, err := readIndexProof(proofs, p, key)
	if err != nil || !(proof.Origin == "atomic" || (len(p.Through) != 0 && bytes.Compare(key, p.Through) <= 0)) {
		return false
	}
	original, valid := indexProofScope(proof.IndexKey, key)
	return valid && original.Timestamp.UTC().Truncate(time.Hour).Equal(hour)
}

// classificationIndexProgress mirrors how readCostUnitsInScope obtains the
// progress that checkOrphans and indexProofForRaw run under.
func classificationIndexProgress(tx *bolt.Tx) (*indexProgress, bool) {
	p, err := loadIndexProgress(tx)
	if err == nil {
		return p, true
	}
	meta := tx.Bucket([]byte(BucketMeta))
	fallback, stateErr := loadIndexProgressState(tx, false)
	if meta == nil || stateErr != nil || fallback == nil || fallback.Phase != "complete" || meta.Get([]byte(usageClientIndexDone)) != nil || meta.Get([]byte(usageClientIndexCursor)) != nil {
		return nil, false
	}
	return fallback, true
}

func sameIndexProgress(a, b *indexProgress) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Version == b.Version && a.Generation == b.Generation && a.Phase == b.Phase && bytes.Equal(a.Through, b.Through) && bytes.Equal(a.Upper, b.Upper) && a.Binding == b.Binding
}

// classifyUsageIntegrity builds A from one read transaction. It never calls
// read hooks, never consults the caller's context and never touches the scan
// counters, so tests can build it in front of every read. It returns nil and a
// fallback reason when the fast path cannot be used for this snapshot.
func classifyUsageIntegrity(tx *bolt.Tx) (*integrityClassification, string) {
	started := time.Now()
	class := &integrityClassification{db: tx.DB(), txid: tx.ID(), eventSet: make(map[string]bool)}
	if class.db != nil {
		class.path = class.db.Path()
	}
	overflow := false
	addEvent := func(key []byte) {
		if !class.eventSet[string(key)] {
			class.eventSet[string(key)] = true
			class.anomalous++
		}
	}
	if _, err := rawOwnerSource(tx); err != nil {
		return nil, integrityFallbackIncomplete
	}
	visitEvents := func(bucket string, exempt func(k, v []byte) bool) {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil && !overflow; k, v = c.Next() {
			if v == nil || !exempt(k, v) {
				addEvent(k)
				overflow = class.anomalous > integrityAnomalyLimit
			}
		}
	}
	canonical := func(k, _ []byte) bool {
		_, ok := canonicalEventKeyHour(k)
		return ok
	}
	visitEvents(rawOwnerBucket, func(k, _ []byte) bool {
		_, _, err := readRawOwner(tx, k)
		return canonical(k, nil) && err == nil
	})
	visitEvents(BucketUsageEvents, func(k, v []byte) bool {
		return canonical(k, v) && rawRowWindowExempt(tx, k, v)
	})
	visitEvents(BucketUsageReceipts, canonical)
	visitEvents(freshAttestationBucket, canonical)
	if overflow {
		return nil, integrityFallbackOverflow
	}
	if quota := tx.Bucket([]byte(BucketQuotaUsageAggHourly)); quota != nil {
		c := quota.Cursor()
		for k, v := c.First(); k != nil && !overflow; k, v = c.Next() {
			if _, ok := quotaKeyHour(k); v == nil || !ok {
				class.quota = append(class.quota, bytes.Clone(k))
				class.anomalous++
				overflow = class.anomalous > integrityAnomalyLimit
			}
		}
	}
	if proofs, err := rollupProofSource(tx); err == nil && proofs != nil {
		prefix := append([]byte(BucketQuotaUsageAggHourly), 0)
		c := proofs.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix) && !overflow; k, v = c.Next() {
			if _, ok := quotaKeyHour(k[len(prefix):]); v == nil || !ok {
				class.proofs = append(class.proofs, bytes.Clone(k))
				class.anomalous++
				overflow = class.anomalous > integrityAnomalyLimit
			}
		}
	}
	if p, ok := classificationIndexProgress(tx); ok {
		class.indexProgress, class.indexClassified = p, true
		if index := tx.Bucket([]byte(BucketUsageEventsByClient)); index != nil && p != nil {
			anomalous := func(key []byte) bool { return class.eventSet[string(key)] }
			c := index.Cursor()
			for k, v := c.First(); k != nil && !overflow; k, v = c.Next() {
				if !indexEntryWindowExempt(tx, p, k, v, anomalous) {
					class.index = append(class.index, bytes.Clone(k))
					class.anomalous++
					overflow = class.anomalous > integrityAnomalyLimit
				}
			}
		}
	}
	if overflow {
		return nil, integrityFallbackOverflow
	}
	for key := range class.eventSet {
		class.events = append(class.events, []byte(key))
	}
	sort.Slice(class.events, func(i, j int) bool { return bytes.Compare(class.events[i], class.events[j]) < 0 })
	class.duration = time.Since(started)
	return class, ""
}

// integrityRealtimeRowAnomalous reports whether a row the realtime writer
// just committed into tx is an anomalous object. Realtime rows normally have a
// canonical key equal to usageKey(event), a bound owner, a parseable quota key
// and an atomic index proof, so they are not; anything else must invalidate
// the classification (v3 item 4). The reference map is never scanned here: an
// entry whose classification would consult it counts as anomalous.
func integrityRealtimeRowAnomalous(tx *bolt.Tx, event usage.Event, key []byte) bool {
	exempt := func(k []byte) bool { return eventKeyWindowExempt(tx, k) }
	if !exempt(key) {
		return true
	}
	if _, ok := quotaKeyHour(quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)); !ok {
		return true
	}
	index := tx.Bucket([]byte(BucketUsageEventsByClient))
	if index == nil {
		return false
	}
	indexKey := clientEventIndexKey(event, key)
	pointer := index.Get(indexKey)
	if pointer == nil {
		return index.Bucket(indexKey) != nil
	}
	p, ok := classificationIndexProgress(tx)
	if !ok || p == nil {
		// Without v2 progress the index walk stays exhaustive (or the read
		// fails before it), so the entry cannot be skipped wrongly.
		return false
	}
	return !indexEntryWindowExempt(tx, p, indexKey, pointer, func(k []byte) bool { return !exempt(k) })
}

// integrityNoteRealtimeRow marks tx when a realtime row is anomalous.
func integrityNoteRealtimeRow(tx *bolt.Tx, event usage.Event, key []byte) {
	if integrityRealtimeRowAnomalous(tx, event, key) {
		integrityMarkTx(tx)
	}
}
