package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync/atomic"
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
	// rawUnindexed is set when a completed client index has a window-exempt
	// raw row that its own client entry does not point back at (Issue #125
	// P2). A client-scoped walk cannot enumerate such a row, so the scope
	// declines while the flag is set. It is not part of A: the exhaustive
	// windowed walk may still skip these rows (their own hour is outside every
	// window without it), and only the scoped enumeration needs them.
	rawUnindexed bool
	// foreignProofs holds the readable index proofs on window-exempt objects
	// whose object the exhaustive walks may judge relevant to a client filter
	// although a client-scoped walk skips it (Issue #125 P2). Index-entry
	// proofs are kept as their decoded original scope, which the probe
	// evaluates with unresolvedScopeInRange, the predicate checkOrphans uses;
	// raw-row proofs keep the client-prefix predicate of the cost evidence
	// scan. Both sets deduplicate and are bounded; once closed is set the
	// probe declines for every client, because the exhaustive read is always
	// the correct fallback. Normal writers derive each proof from its own
	// event, so the sets stay empty unless a proof was rewritten.
	foreignProofs foreignProofSet
	anomalous     int
	duration      time.Duration
}

// integrityForeignProofLimit bounds the distinct readable proofs a scoped
// probe remembers. The sets are only populated by proofs that disagree with
// their object, so healthy snapshots keep them empty.
const integrityForeignProofLimit = 64

// foreignProofSet is the bounded, deduplicated collection described on
// integrityClassification.foreignProofs. close drops the contents so a
// damaged snapshot cannot grow the classification linearly with its proof
// damage.
type foreignProofSet struct {
	scopes   map[unresolvedScope]struct{}
	prefixes map[string]struct{}
	closed   bool
}

// addScope records one index-entry proof scope. Duplicates are free; a new
// distinct entry beyond the limit turns the whole probe into a decline.
func (f *foreignProofSet) addScope(scope unresolvedScope) {
	if f.closed {
		return
	}
	if _, ok := f.scopes[scope]; ok {
		return
	}
	if len(f.scopes)+len(f.prefixes) >= integrityForeignProofLimit {
		f.close()
		return
	}
	if f.scopes == nil {
		f.scopes = make(map[unresolvedScope]struct{})
	}
	f.scopes[scope] = struct{}{}
}

// addPrefix records one raw-row proof prefix (client field and delimiter).
func (f *foreignProofSet) addPrefix(prefix []byte) {
	if f.closed {
		return
	}
	if _, ok := f.prefixes[string(prefix)]; ok {
		return
	}
	if len(f.scopes)+len(f.prefixes) >= integrityForeignProofLimit {
		f.close()
		return
	}
	if f.prefixes == nil {
		f.prefixes = make(map[string]struct{})
	}
	f.prefixes[string(prefix)] = struct{}{}
}

func (f *foreignProofSet) close() {
	f.scopes, f.prefixes = nil, nil
	f.closed = true
}

// relevant reports whether the exhaustive walks would fail a client-filtered
// read closed because of one of the remembered proofs. The scope predicate is
// the exact call checkOrphans makes for an in-shape scoped read: the client
// name is trimmed, model is empty and allow is nil. The prefix predicate is
// the byte comparison the cost evidence scan makes against the filter
// client's index prefix.
func (f *foreignProofSet) relevant(client string, from, to time.Time) bool {
	if f.closed {
		return true
	}
	client = strings.TrimSpace(client)
	for scope := range f.scopes {
		if inside, _ := unresolvedScopeInRange(scope, client, from, to, "", nil); inside {
			return true
		}
	}
	prefix := clientIndexPrefix(client)
	for candidate := range f.prefixes {
		if candidate == prefix {
			return true
		}
	}
	return false
}

// canonicalEventKeyStamp returns the instant encoded by a raw-identity key
// whose stamp round-trips through RFC3339Nano in UTC. The id suffix is not
// inspected (legacy rows have numeric or missing record ids).
func canonicalEventKeyStamp(key []byte) (time.Time, bool) {
	stamp, _, found := strings.Cut(string(key), "_")
	if !found {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || at.UTC().Format(time.RFC3339Nano) != stamp {
		return time.Time{}, false
	}
	return at.UTC(), true
}

// canonicalEventKeyHour returns the UTC hour of a canonical raw-identity key.
// Only such keys start with their own hour, so only they are reachable by an
// hour-prefix seek.
func canonicalEventKeyHour(key []byte) (time.Time, bool) {
	at, ok := canonicalEventKeyStamp(key)
	if !ok {
		return time.Time{}, false
	}
	return at.Truncate(time.Hour), true
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

// quotaKeyOwnerWindowNeutral reports whether a quota aggregation key's owner
// field lets every client-filtered read answer (false, nil) for hours outside
// the window. quotaKeyInScope returns false for a known owner different from
// the filter client before it looks at the hour, but an empty or
// non-round-tripping owner falls through to the owner-unknown error inside the
// window. A key with fewer than three fields has no owner field and behaves
// like an unknown owner. An empty owner is neutral outside the window and is
// reached inside the window by the client-scoped walk's per-hour empty-owner
// range. Model and target soundness is not required here: a malformed model or
// target only errors for the read of the key's own (known) client, and that
// read reaches the key through its client quota range.
func quotaKeyOwnerWindowNeutral(key []byte) bool {
	parts := strings.Split(string(key), "|")
	if len(parts) < 3 {
		return false
	}
	if parts[1] == "" {
		return true
	}
	owner := quotaKeyDecode(parts[1])
	return owner != "" && utf8.ValidString(owner) && owner == strings.TrimSpace(owner) && quotaKeyEncode(owner) == parts[1]
}

// rawRowWindowExempt reports whether a raw row with a canonical key is inert
// for every window that excludes its key hour. Proof per branch (key hour H):
//
//   - checkDirectQuotaEvidenceContext raw loop: readRawOwner and the encoding
//     checks error at any hour, so the owner must be valid and the bytes valid
//     UTF-8 JSON with a non-zero timestamp. An owned row must satisfy
//     usageKey(event) == key, because the exhaustive loop compares the two
//     exactly when owned; its event hour is then H, so every quota/usage cell
//     it accumulates or checks is outside W and quotaKeyInScope answers
//     (false, nil) for them. An unowned legacy row written before record ids
//     were stored has an empty record_id and a key stamp exactly equal to its
//     event time; the exhaustive loop does not compare key and event for
//     unowned rows, so the row only accumulates cells of its own hour H.
//   - quotaProofBatchInScopeContext raw loop: a canonical proof event lands in
//     cells[quotaAggKey(event)], an hour-H key that is never a cell of W. A row
//     that is not a canonical proof event (a legacy row without record_id, for
//     example) loops over the in-window cells with proofRawMayOverlap, which is
//     false for all of them: the owner read succeeded, the JSON is not an
//     ambiguous valid document, and the key stamp parses to H outside their
//     hours, so the hour test rejects each cell before its client or event
//     scope matters.
//   - malformed rows (checkMalformedIndexScope) never reach this predicate.
//   - checkOrphans and the client index are keyed by event client and time plus
//     the raw key, not by record_id; classifyIndexEntry classifies each
//     entry from its own proof, so this relaxation cannot skip an entry whose
//     branch depends on the raw body.
func rawRowWindowExempt(tx *bolt.Tx, key, value []byte) bool {
	owner, owned, err := readRawOwner(tx, key)
	if err != nil || value == nil || !utf8.Valid(value) {
		return false
	}
	var event usage.Event
	if json.Unmarshal(value, &event) != nil || event.Timestamp.IsZero() {
		return false
	}
	if usageKey(event.Timestamp, event.RecordID) != string(key) {
		// Issue #125 P1.1: legacy rows have no record_id in the body, so the
		// key cannot be rebuilt from the event. Accept only the shape observed
		// in pre-record-id history: unowned, empty record_id, and a key stamp
		// exactly equal to the event time. The exhaustive direct check treats
		// unowned rows by their event hour alone, so this row is inert outside
		// its key hour; every other mismatch stays anomalous.
		keyAt, ok := canonicalEventKeyStamp(key)
		if owned || !ok || event.RecordID != "" || !keyAt.Equal(event.Timestamp) {
			return false
		}
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

// classifiedIndexEntry is one client index entry's window-exemption result.
// The readable proof travels with the verdict, so the caller can judge its
// original scope without decoding the same object a second time (Issue #125
// P2).
type classifiedIndexEntry struct {
	exempt bool
	// matchedRaw reports that the entry is exactly the client index key of its
	// own raw row. checkOrphans then returns after indexProofForRaw unless that
	// proof is invalid.
	matchedRaw bool
	// invalid reports a readable proof that does not match the raw row (wrong
	// digest or IndexKey). It is only meaningful with matchedRaw.
	invalid bool
	// proof is the decoded proof; proofOK reports that decoding succeeded.
	proof   indexProof
	proofOK bool
}

// classifyIndexEntry reports whether a client index entry is inert in
// checkOrphans for every window excluding its hour H, and whether a windowed
// walk may skip it without changing the reference map (see contains). It also
// returns the decoded proof so a caller can classify the proof's scope without
// reading it again.
//
//   - identity parts and pointer: a malformed identity errors at any hour, and a
//     pointer other than the third part adds a reference for another key.
//   - the entry's own time must be valid and of hour H, the referenced key
//     non-anomalous: the "current" scope then excludes every window without H.
//   - an entry matching its raw row: indexProofForRaw must not consult the
//     reference map (the exhaustive map could fail on an unrelated entry), and
//     an invalid proof must be attributable to a valid original of hour H, or
//     be unattributable for an owned row; otherwise it errors regardless of W.
//     A legacy row without record_id still matches clientEventIndexKey (it is
//     built from client, event time and the raw key), and a scan or atomic
//     proof identifies the entry's own hour, so it remains inert outside H.
//   - an entry without a matching raw row is only inert with a trusted proof
//     whose original scope has hour H; the owner shortcuts depend on the
//     client filter and the fallback errors whenever no client is filtered.
func classifyIndexEntry(tx *bolt.Tx, p *indexProgress, indexKey, pointer []byte, anomalous func([]byte) bool) classifiedIndexEntry {
	if pointer == nil || p == nil {
		return classifiedIndexEntry{}
	}
	parts := strings.SplitN(string(indexKey), "|", 3)
	if len(parts) != 3 || parts[2] == "" {
		return classifiedIndexEntry{}
	}
	key := []byte(parts[2])
	if !bytes.Equal(pointer, key) {
		return classifiedIndexEntry{}
	}
	hour, ok := canonicalEventKeyHour(key)
	if !ok || anomalous(key) {
		return classifiedIndexEntry{}
	}
	current, valid := indexProofScope(indexKey, key)
	if !valid || !current.Timestamp.UTC().Truncate(time.Hour).Equal(hour) {
		return classifiedIndexEntry{}
	}
	proofs := tx.Bucket([]byte(indexProofsV2))
	encoded := proofs != nil && proofs.Get(key) != nil
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
			return classifiedIndexEntry{}
		}
		if encoded {
			integrityIndexProofDecodes.Add(1)
		}
		if !invalid {
			return classifiedIndexEntry{exempt: true, matchedRaw: true, proof: proof, proofOK: attributable}
		}
		if attributable {
			original, valid := indexProofScope(proof.IndexKey, key)
			return classifiedIndexEntry{exempt: valid && original.Timestamp.UTC().Truncate(time.Hour).Equal(hour), matchedRaw: true, invalid: true, proof: proof, proofOK: true}
		}
		return classifiedIndexEntry{exempt: owned, matchedRaw: true, invalid: true}
	}
	if proofs == nil {
		return classifiedIndexEntry{}
	}
	proof, err := readIndexProof(proofs, p, key)
	integrityIndexProofDecodes.Add(1)
	if err != nil || !(proof.Origin == "atomic" || (len(p.Through) != 0 && bytes.Compare(key, p.Through) <= 0)) {
		return classifiedIndexEntry{}
	}
	original, valid := indexProofScope(proof.IndexKey, key)
	return classifiedIndexEntry{exempt: valid && original.Timestamp.UTC().Truncate(time.Hour).Equal(hour), proof: proof, proofOK: err == nil}
}

// integrityIndexProofDecodes counts the index proofs the classifier decodes
// (directly, or through indexProofForRaw when a proof blob exists). Tests
// assert it stays within one decode per window-exempt entry: the foreign-scope
// check reuses the classifier's proof instead of reading the object twice.
var integrityIndexProofDecodes atomic.Int64

// noteForeignScopes records the proof scopes the exhaustive checkOrphans walk
// would evaluate for a window-exempt index entry although a client-scoped walk
// skips it (Issue #125 P2). checkOrphans evaluates up to two scopes:
//
//   - the current scope from the entry's own index key, unless the entry
//     matches its raw row with a trusted proof (checkOrphans returns first);
//   - the original scope from the readable proof, when the entry has no
//     matching raw row, or when its matching proof is invalid but readable.
//
// unresolvedScopeInRange is the authoritative predicate for both, including
// the empty and "unknown" owners it treats as relevant to every client. The
// probe reuses exactly these scopes; a matching entry with a trusted proof
// names its own client and needs no decline.
func (class *integrityClassification) noteForeignScopes(entry classifiedIndexEntry, indexKey []byte) {
	parts := strings.SplitN(string(indexKey), "|", 3)
	if len(parts) != 3 || parts[2] == "" {
		return
	}
	key := []byte(parts[2])
	invalid := entry.matchedRaw && entry.invalid
	if current, ok := indexProofScope(indexKey, key); ok && strings.EqualFold(current.ClientName, "unknown") && (!entry.matchedRaw || invalid) {
		// A non-empty unknown owner never equals a known filter client, so a
		// skipped entry with this current scope fails checkOrphans closed for
		// every client. The empty owner is not recorded: the empty prefix is
		// enumerated by the scoped index walk, which reaches the same error.
		class.foreignProofs.addScope(current)
	}
	if !entry.matchedRaw || invalid {
		if entry.proofOK {
			if scope, ok := indexProofScope(entry.proof.IndexKey, key); ok {
				class.foreignProofs.addScope(scope)
			}
		}
	}
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

// integrityClassifyCheckEvery is how many objects a classification visits
// between two cancellation checks.
const integrityClassifyCheckEvery = 1024

// classifyUsageIntegrity builds A from one read transaction. It does not call
// read hooks and does not change the read context or verdict, so tests can
// build it in front of every read (with context.Background(), never the
// reader's context). It does increment the test-only proofRawParses counter
// through backfillRawScope; classification already did so for non-proof rows
// before P1.1.
// It checks ctx periodically; a canceled build returns nil. It returns nil and
// a fallback reason when the fast path cannot be used for this snapshot.
func classifyUsageIntegrity(ctx context.Context, tx *bolt.Tx) (*integrityClassification, string) {
	started := time.Now()
	visited := 0
	canceled := false
	step := func() bool {
		if visited++; visited%integrityClassifyCheckEvery != 1 {
			return !canceled
		}
		if integrityClassifyStep != nil {
			integrityClassifyStep()
		}
		canceled = canceled || ctx.Err() != nil
		return !canceled
	}
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
		for k, v := c.First(); k != nil && !overflow && step(); k, v = c.Next() {
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
	// Issue #125 P2: an owner row is only neutral outside its hour when the
	// owner walk's existence shortcut ends it. An orphan owner (no raw row,
	// receipt or quarantined row) is checked against the client filter inside
	// its hour and errors for its own client, so it must stay anomalous even
	// for client-scoped reads that cannot reach it through the client index.
	retained := func(k []byte) bool {
		raw, receipts, quarantine := tx.Bucket([]byte(BucketUsageEvents)), tx.Bucket([]byte(BucketUsageReceipts)), tx.Bucket([]byte(BucketUsageEventsQuarantine))
		return (raw != nil && raw.Get(k) != nil) || (receipts != nil && receipts.Get(k) != nil) || (quarantine != nil && quarantine.Get(k) != nil)
	}
	visitEvents(rawOwnerBucket, func(k, _ []byte) bool {
		_, _, err := readRawOwner(tx, k)
		return canonical(k, nil) && err == nil && retained(k)
	})
	// Issue #125 P2: the client-scoped enumeration reaches a raw row only
	// through its client index entry. When the index claims completion, record
	// every window-exempt row whose entry is missing or points elsewhere, and
	// every stale proof that names another client's index space; the scoped
	// probe then declines instead of returning a verdict that ignored the row
	// or the client it implicates. Other anomalous rows are covered by A (they
	// are always visited).
	index := tx.Bucket([]byte(BucketUsageEventsByClient))
	indexComplete := false
	if meta := tx.Bucket([]byte(BucketMeta)); meta != nil &&
		meta.Get([]byte(usageClientIndexDone)) != nil &&
		meta.Get([]byte(usageClientIndexSourceMissing)) == nil && meta.Bucket([]byte(usageClientIndexSourceMissing)) == nil &&
		!hasUsageQuarantine(tx) {
		indexComplete = index != nil
	}
	progress, indexClassified := classificationIndexProgress(tx)
	if indexClassified {
		class.indexProgress, class.indexClassified = progress, true
	}
	visitEvents(BucketUsageEvents, func(k, v []byte) bool {
		if !canonical(k, v) || !rawRowWindowExempt(tx, k, v) {
			return false
		}
		var event usage.Event
		if json.Unmarshal(v, &event) != nil {
			return true
		}
		if indexComplete {
			if !bytes.Equal(index.Get(clientEventIndexKey(event, k)), k) {
				class.rawUnindexed = true
			}
			if prefix, ok := staleForeignProof(tx, progress, k, event); ok {
				class.foreignProofs.addPrefix(prefix)
			}
		}
		return true
	})
	visitEvents(BucketUsageReceipts, canonical)
	// A fresh attestation without its raw row or receipt errors for its client
	// inside its hour; a client-scoped read cannot reach it through the index,
	// so it must be visited by every read.
	visitEvents(freshAttestationBucket, func(k, _ []byte) bool {
		return canonical(k, nil) && retained(k)
	})
	if overflow {
		return nil, integrityFallbackOverflow
	}
	if quota := tx.Bucket([]byte(BucketQuotaUsageAggHourly)); quota != nil {
		c := quota.Cursor()
		for k, v := c.First(); k != nil && !overflow && step(); k, v = c.Next() {
			if _, ok := quotaKeyHour(k); v == nil || !ok || !quotaKeyOwnerWindowNeutral(k) {
				class.quota = append(class.quota, bytes.Clone(k))
				class.anomalous++
				overflow = class.anomalous > integrityAnomalyLimit
			}
		}
	}
	if proofs, err := rollupProofSource(tx); err == nil && proofs != nil {
		prefix := append([]byte(BucketQuotaUsageAggHourly), 0)
		c := proofs.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix) && !overflow && step(); k, v = c.Next() {
			if _, ok := quotaKeyHour(k[len(prefix):]); v == nil || !ok {
				class.proofs = append(class.proofs, bytes.Clone(k))
				class.anomalous++
				overflow = class.anomalous > integrityAnomalyLimit
			}
		}
	}
	if indexClassified {
		if index := tx.Bucket([]byte(BucketUsageEventsByClient)); index != nil && progress != nil {
			anomalous := func(key []byte) bool { return class.eventSet[string(key)] }
			c := index.Cursor()
			for k, v := c.First(); k != nil && !overflow && step(); k, v = c.Next() {
				entry := classifyIndexEntry(tx, progress, k, v, anomalous)
				if !entry.exempt {
					class.index = append(class.index, bytes.Clone(k))
					class.anomalous++
					overflow = class.anomalous > integrityAnomalyLimit
					continue
				}
				// Issue #125 P2: a window-exempt entry is skipped by a
				// client-scoped walk. If its readable proof names a client that
				// checkOrphans fails closed for, the scoped probe must decline
				// for that client instead. Scopes are judged with
				// unresolvedScopeInRange, so empty and "unknown" owners, which
				// are relevant to every client, decline too.
				class.noteForeignScopes(entry, k)
			}
		}
	}
	if canceled {
		return nil, integrityFallbackIncomplete
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
	return !classifyIndexEntry(tx, p, indexKey, pointer, func(k []byte) bool { return !exempt(k) }).exempt
}

// integrityNoteRealtimeRow marks tx when a realtime row is anomalous.
func integrityNoteRealtimeRow(tx *bolt.Tx, event usage.Event, key []byte) {
	if integrityRealtimeRowAnomalous(tx, event, key) {
		integrityMarkTx(tx)
	}
}
