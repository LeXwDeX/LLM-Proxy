package nosql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// Issue #114: proofs used to classify every raw row and receipt with a full
// bucket scan inside each write transaction, and startup warm-up decoded every
// raw row. This file keeps a persistent classification of the only objects a
// scoped proof cannot reach by key prefix:
//
//   - a raw row that is not canonical (its key is not usageKey(event));
//   - a receipt whose key stamp is not canonical UTC.
//
// Objects are located by key-stamp hour, so a proof only loads the odd rows of
// the hours it proves plus the set that cannot be located by hour (unparseable
// stamps) and the rows bound to a readable raw owner. The classification
// mapping is exact:
//
//   - canonical raw rows and canonical receipts are never indexed: their key
//     starts with their own UTC hour, so the proof's prefix walk reaches them;
//   - odd raw rows with a parseable key stamp and no readable raw owner entry
//     are indexed under that UTC hour; the proof walk reaches them by prefix
//     when the literal key matches and through the index otherwise
//     (offset-zone keys);
//   - odd raw rows whose key stamp is unparseable, whose bytes make
//     proofRawMayOverlap return true regardless of the hour, or that have a
//     readable raw owner entry are global and every proof visits them. The
//     owner is re-read by proofRawMayOverlap, so an entry that later turns
//     unreadable would widen the row's overlap to every hour; owner-bound rows
//     must therefore never be hidden under a single hour;
//   - receipts with a parseable key stamp are indexed under that UTC hour;
//     unparseable stamps are global. receiptMayOverlap is false for every
//     parseable stamp outside the proved hours.
//
// The owner source is part of the classification input: while rawOwnerSource
// is inconsistent (a missing bucket with its markers intact, for example),
// readRawOwner fails for every odd row, so the scoped walk must fall back to
// the exhaustive scan instead of trusting hour-located entries.
//
// Owner-bound rows are global, so every proof loads all of them. Proof cost
// therefore grows with the owner-bound odd history instead of staying
// proportional to the proved hours: a database whose odd rows all have readable
// owners degrades to the previous full scan. That is the deliberate price of
// keeping owner damage fail-closed; an hour-located row would be unreachable
// once an owner read that decides overlap changes after classification.
//
// Every supported writer updates the index in the same transaction:
// writeUsageRowTx and writeUsageReceipt, ReindexUsageEvent, the JSON import,
// and retention's raw delete. Any other path (including out-of-band edits and
// data restored from a backup) must rebuild it with ResetUsageProofOddIndex
// followed by MigrateUsageProofOddIndex; the rebuild entry point is a Go
// function only, there is no admin or CLI command for it. Out-of-band owner
// writes are a residual risk: adding a readable owner after the build without
// maintaining the index turns an hour-located row into one proofRawMayOverlap
// can report outside its hour, and nothing in the index detects the change. A
// resumed raw phase trusts its cursor the same way: an out-of-band rewrite is
// caught by the entry digest, but an out-of-band insertion behind the cursor is
// not. Damage the index can determine from its own checkable state — a missing
// or unreadable bucket or marker, a foreign scheme or revision, a leftover
// phase marker, a per-kind count or pointer that disagrees with the entries, an
// entry digest that no longer matches its object, or an unreadable raw-owner
// source — makes proofs fall back to the previous full scans; the index never
// fails open on that account. That guarantee is bounded by the index's own
// checkable state. Two shapes are genuinely undetectable: an odd object added
// out of band that the index does not record, including an insertion behind a
// resumed raw cursor, and a location entry whose pointer and per-kind count are
// removed together in a self-consistent way. A restore of only the raw or
// receipt buckets, a restore into a different database, and any other partial
// restore may delete or rewrite objects the index still references, which the
// entry, pointer, and count checks can detect, but the restore as a whole is
// outside this guarantee because it can also add odd objects the index does not
// record. After restoring data or editing out of band, run
// ResetUsageProofOddIndex followed by MigrateUsageProofOddIndex before serving.
// Adding or rewriting a readable owner outside the supported writers is the
// same kind of residual risk: the index cannot detect it, and the same reset
// and migration restore the guarantee.
//
// The completion marker records the classification scheme version, so an index
// built with different location semantics is rebuilt instead of trusted. A
// build records that scheme in the transaction that scans its first raw batch,
// so a resumable partial build always carries it and any partial state without
// the current scheme was made by older semantics and is discarded. The raw
// phase also records the raw revision it scanned; the receipt phase only
// completes when that snapshot still matches, so a writer that advances the
// revision between the phases without maintaining the index forces a rebuild
// instead of a revision stamp the scan never observed. A supported writer
// advances the snapshot in the same transaction that maintains the index, so
// an interrupted receipt phase resumes across starts; a writer that advances
// only the revision (an older binary or an out-of-band edit) leaves the
// snapshot behind and the next migration rebuilds.
const (
	bucketUsageProofOdd      = "usage_proof_odd_v1"
	proofOddReadyKey         = "usage_proof_odd_ready_v1"
	proofOddPhaseKey         = "usage_proof_odd_phase_v1"
	proofOddRawCursorKey     = "usage_proof_odd_raw_cursor_v1"
	proofOddReceiptCursorKey = "usage_proof_odd_receipt_cursor_v1"
	proofOddRevisionKey      = "usage_proof_odd_revision_v1"
	proofOddRawRevisionKey   = "usage_proof_odd_raw_revision_v1"
	proofOddSchemeKey        = "usage_proof_odd_scheme_v1"
	proofOddMigrateBatch     = 512
)

// proofOddScheme versions the location semantics stored in the index. Bump it
// whenever a row's location decision changes, so an index built by the older
// semantics is rebuilt rather than trusted. Version 2 made owner-bound odd rows
// global; version 1 located them by key hour.
const proofOddScheme = "2"

// Single-byte domains inside bucketUsageProofOdd. Locations use the lower/upper
// kind byte followed by the optional hour; pointers use 'r'/'c'; per-kind
// counts use 'n', and the marker that says the counts were established is
// proofOddCountsMarkerKey. The marker and the counts live in the index bucket
// itself so a reset (which deletes and recreates the bucket) clears them with
// the entries they describe.
const (
	proofOddRawGlobal      = 'g'
	proofOddRawHour        = 'h'
	proofOddReceiptGlobal  = 'G'
	proofOddReceiptHour    = 'H'
	proofOddRawPointer     = 'r'
	proofOddReceiptPointer = 'c'
	proofOddCountPrefix    = 'n'
)

// proofOddCountsMarkerKey marks that every location entry has a matching
// per-kind count. A completed index without it was built before the counts
// existed; its entries are still readable but its completeness cannot be
// checked, so it is rebuilt instead of trusted.
var proofOddCountsMarkerKey = []byte("m1")

var (
	// proofOddFullClassifications counts keys examined by the fallback full
	// classification scan. The exported metrics below expose it so operators
	// can see a proof index that is not being trusted, and tests use it to
	// prove the scoped path no longer walks all history.
	proofOddFullClassifications atomic.Int64
	// proofOddIndexLoads counts entries loaded from the persisted index. The
	// exported metrics expose it next to the fallback count.
	proofOddIndexLoads atomic.Int64
)

// UsageProofIndexMetrics is the exported, key-free state of the persistent
// odd-row proof index (Issue #114). Ready reports whether proofs currently
// trust the index's own state; false at runtime means every proof scans the
// whole history through the fallback. Ready is not a data-consistency proof:
// it only covers the top-level checks that the bucket exists, the ready and
// counts markers are present, the scheme and raw revision match, no phase
// marker is left over, and the raw-owner source is readable. Entries are not
// checked here: per-kind counts, pointers, and entry digests are checked when a
// proof loads a scope, or by the migration's count verification. Damage that
// leaves those top-level checks self-consistent — a location entry, its
// pointer, and its count removed together, or data restored without the index —
// is outside what Ready can observe. The counters cover the lifetime of this
// process and never include keys or stored bytes.
type UsageProofIndexMetrics struct {
	Ready               bool   `json:"ready"`
	FullClassifications uint64 `json:"full_classifications"`
	IndexLoads          uint64 `json:"index_loads"`
}

// UsageProofIndexStats reports the proof-index state for db. A failed read
// transaction reports ready=false rather than guessing. Ready carries the same
// meaning as UsageProofIndexMetrics.Ready: it covers only those top-level
// checks and is not a data-consistency proof.
func UsageProofIndexStats(db *bolt.DB) UsageProofIndexMetrics {
	m := UsageProofIndexMetrics{
		FullClassifications: uint64(proofOddFullClassifications.Load()),
		IndexLoads:          uint64(proofOddIndexLoads.Load()),
	}
	if db == nil {
		return m
	}
	_ = db.View(func(tx *bolt.Tx) error {
		m.Ready = proofOddIndexReady(tx)
		return nil
	})
	return m
}

// UsageProofIndexStats reports the proof-index state of the store's database.
func (s *UsageStore) UsageProofIndexStats() UsageProofIndexMetrics {
	if s == nil || s.db == nil {
		return UsageProofIndexMetrics{}
	}
	return UsageProofIndexStats(s.db)
}

func proofOddBucket(tx *bolt.Tx) *bolt.Bucket {
	return tx.Bucket([]byte(bucketUsageProofOdd))
}

func proofOddMeta(tx *bolt.Tx) *bolt.Bucket {
	return tx.Bucket([]byte(BucketMeta))
}

// proofOddIndexReady reports whether a proof may trust the persisted
// classification. The index must be complete and current, and the raw owner
// source it was classified from must be readable: when rawOwnerSource is
// inconsistent, readRawOwner fails for every odd row and proofRawMayOverlap
// treats each of them as relevant at any hour, so hour-located entries must not
// be used. Proofs then fall back to the exhaustive scan.
func proofOddIndexReady(tx *bolt.Tx) bool {
	return proofOddIndexCurrent(tx) && proofOddOwnerSourceReadable(tx)
}

// proofOddIndexCurrent reports whether the persisted classification is
// complete for the current scheme and raw revision. The stored raw revision
// must equal the live one: supported writers advance both in the same
// transaction, while a writer that does not maintain the index (an older
// binary or an out-of-band edit that bumps the revision) makes the check fail.
// The revision is a writer sequence number, not a digest of the data, so an
// out-of-band rewrite that leaves it unchanged is outside this check.
// Migration uses this check without the owner-source condition so it can finish
// while the owner source is unreadable instead of rebuilding on every attempt;
// proofs use proofOddIndexReady and keep the full-scan fallback meanwhile.
func proofOddIndexCurrent(tx *bolt.Tx) bool {
	b := proofOddBucket(tx)
	meta := proofOddMeta(tx)
	if b == nil || meta == nil {
		return false
	}
	if string(meta.Get([]byte(proofOddReadyKey))) != "1" {
		return false
	}
	if string(meta.Get([]byte(proofOddSchemeKey))) != proofOddScheme {
		return false
	}
	if meta.Get([]byte(proofOddPhaseKey)) != nil {
		return false
	}
	if b.Get(proofOddCountsMarkerKey) == nil {
		// No counts marker: either a partial build or an index from before the
		// counts existed. Without per-kind counts a load cannot reconcile the
		// entries it visited with the entries the bucket should contain, so such
		// an index is not current, proofs fall back, and the next migration
		// rebuilds it. The marker only says the counts existed at completion: a
		// location entry, its pointer, and its count removed together afterwards
		// are self-consistent and stay outside what the index can detect.
		return false
	}
	stored := meta.Get([]byte(proofOddRevisionKey))
	return len(stored) == 8 && binary.BigEndian.Uint64(stored) == rawRevision(tx)
}

// proofOddOwnerSourceReadable reports whether the raw owner source every odd
// row's classification depends on is consistent.
func proofOddOwnerSourceReadable(tx *bolt.Tx) bool {
	_, err := rawOwnerSource(tx)
	return err == nil
}

// proofOddFollowRawRevision advances the revision recorded alongside the
// classification. Every supported raw writer bumps rawRevision in the same
// transaction after updating the index.
func proofOddFollowRawRevision(tx *bolt.Tx, revision uint64) error {
	meta := proofOddMeta(tx)
	if meta == nil || proofOddBucket(tx) == nil {
		return nil
	}
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], revision)
	if string(meta.Get([]byte(proofOddReadyKey))) == "1" {
		// A completed classification may only adopt a contiguous step from the
		// revision it stored. A stored revision that already lags belongs to a
		// writer that advanced the revision without maintaining the index (an older
		// binary or an out-of-band edit); adopting the live value here would
		// hide that change permanently, because every later start would see a
		// current index. Leaving it behind keeps proofs on the full-scan
		// fallback and makes the next migration rebuild.
		stored := meta.Get([]byte(proofOddRevisionKey))
		if len(stored) != 8 || binary.BigEndian.Uint64(stored)+1 != revision {
			return nil
		}
		return meta.Put([]byte(proofOddRevisionKey), data[:])
	}
	// A staged build is bound to the raw revision its scanner started from
	// (proofOddRawRevisionKey). This writer updated the index in the same
	// transaction, so advancing the snapshot lets a paused raw or receipt phase
	// resume across starts instead of forcing a reset and a full rescan. Only a
	// contiguous step is safe: a bump that did not maintain the index leaves the
	// snapshot behind, and a later supported write must not launder that
	// inconsistency into a resumable state. Consecutive bumps in one
	// transaction advance step by step, because each sees the snapshot written
	// by the previous bump. Before the first raw batch records the snapshot
	// there is nothing to advance: that batch takes the live revision itself,
	// after this write committed.
	phase := string(meta.Get([]byte(proofOddPhaseKey)))
	if phase != "raw" && phase != "receipts" {
		return nil
	}
	stored := meta.Get([]byte(proofOddRawRevisionKey))
	if len(stored) != 8 || binary.BigEndian.Uint64(stored)+1 != revision {
		return nil
	}
	return meta.Put([]byte(proofOddRawRevisionKey), data[:])
}

// proofOddInvalidate drops the completion marker so proofs fall back to full
// scans until a migration rebuilds the classification.
func proofOddInvalidate(tx *bolt.Tx) {
	meta := proofOddMeta(tx)
	if meta == nil {
		return
	}
	_ = meta.Delete([]byte(proofOddReadyKey))
	_ = meta.Delete([]byte(proofOddSchemeKey))
	_ = meta.Delete([]byte(proofOddPhaseKey))
	_ = meta.Delete([]byte(proofOddRawCursorKey))
	_ = meta.Delete([]byte(proofOddReceiptCursorKey))
	_ = meta.Delete([]byte(proofOddRevisionKey))
	_ = meta.Delete([]byte(proofOddRawRevisionKey))
}

// proofOddResetBucket discards every entry and progress marker but keeps the
// bucket, so the next migration transaction starts a fresh build. Proofs fall
// back to the exhaustive scan until that build records the ready marker.
func proofOddResetBucket(tx *bolt.Tx) error {
	if tx.Bucket([]byte(bucketUsageProofOdd)) != nil {
		if err := tx.DeleteBucket([]byte(bucketUsageProofOdd)); err != nil {
			return err
		}
	}
	if _, err := tx.CreateBucketIfNotExists([]byte(bucketUsageProofOdd)); err != nil {
		return err
	}
	proofOddInvalidate(tx)
	return nil
}

// proofOddHasProgress reports whether the persisted classification carries
// staged state: a phase, a cursor, the raw-phase revision snapshot, or any
// entry. Partial progress without the current scheme cannot be resumed.
func proofOddHasProgress(tx *bolt.Tx) bool {
	meta := proofOddMeta(tx)
	if meta.Get([]byte(proofOddPhaseKey)) != nil ||
		meta.Get([]byte(proofOddRawCursorKey)) != nil ||
		meta.Get([]byte(proofOddReceiptCursorKey)) != nil ||
		meta.Get([]byte(proofOddRawRevisionKey)) != nil {
		return true
	}
	b := proofOddBucket(tx)
	if b == nil {
		return false
	}
	k, _ := b.Cursor().First()
	return k != nil
}

// proofOddRawRevisionBound reports whether the raw-phase revision snapshot
// still matches the live raw revision.
func proofOddRawRevisionBound(tx *bolt.Tx) bool {
	meta := proofOddMeta(tx)
	if meta == nil {
		return false
	}
	stored := meta.Get([]byte(proofOddRawRevisionKey))
	return len(stored) == 8 && binary.BigEndian.Uint64(stored) == rawRevision(tx)
}

// proofOddDigest binds key and bytes with a process-independent digest, so a
// rewritten row is detected after a restart. It is not a security primitive.
func proofOddDigest(key, data []byte) []byte {
	h := sha256.New()
	h.Write([]byte("usage-proof-odd-v1\x00"))
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(key)))
	h.Write(size[:])
	h.Write(key)
	h.Write(data)
	return h.Sum(nil)
}

func proofOddPointerKey(pointer byte, key []byte) []byte {
	out := make([]byte, 1+len(key))
	out[0] = pointer
	copy(out[1:], key)
	return out
}

func proofOddLocationKey(kind byte, hour string, key []byte) []byte {
	out := make([]byte, 1+len(hour)+len(key))
	out[0] = kind
	copy(out[1:], hour)
	copy(out[1+len(hour):], key)
	return out
}

func proofOddPointerValue(kind byte, hour string) []byte {
	out := make([]byte, 1+len(hour))
	out[0] = kind
	copy(out[1:], hour)
	return out
}

func proofOddParsePointer(value []byte) (byte, string, bool) {
	if len(value) == 0 {
		return 0, "", false
	}
	switch kind := value[0]; kind {
	case proofOddRawGlobal, proofOddReceiptGlobal:
		return kind, "", len(value) == 1
	case proofOddRawHour, proofOddReceiptHour:
		return kind, string(value[1:]), len(value) == 1+len(hourLayout)
	default:
		return 0, "", false
	}
}

// proofOddRawIsCanonical is canonicalRawRowCached without the parse counters:
// the writer path must not disturb scan-count assertions. The predicate is
// identical.
func proofOddRawIsCanonical(key, data []byte) bool {
	var event usage.Event
	return decodeProofEventUncounted(data, &event) && usageKey(event.Timestamp, event.RecordID) == string(key)
}

// proofOddClassifyRaw maps an odd raw row to its index location. A row that
// proofRawMayOverlap can report relevant outside its own key hour must be
// global; everything else is locatable by the key stamp's UTC hour.
//
// A row with a readable owner entry is always global. proofRawMayOverlap reads
// that entry live, and an entry that later turns unreadable makes the row
// relevant at every hour while an hour-located entry would never be visited.
// Keeping owner-bound rows global means every proof re-judges them with the
// current owner bytes, so owner damage can only fall through to the reference
// verdict, never skip a row the exhaustive walk would act on.
func proofOddClassifyRaw(tx *bolt.Tx, key, data []byte) (byte, string) {
	stamp, _, found := strings.Cut(string(key), "_")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if !found || err != nil {
		return proofOddRawGlobal, ""
	}
	scope := backfillRawScopeCached(data)
	if scope.ambiguous && scope.valid {
		return proofOddRawGlobal, ""
	}
	if _, known, err := readRawOwner(tx, key); err != nil || known {
		return proofOddRawGlobal, ""
	}
	return proofOddRawHour, at.UTC().Truncate(time.Hour).Format(hourLayout)
}

// proofOddClassifyReceipt maps a non-canonical receipt to its index location.
// kind 0 means the receipt is canonical and needs no entry.
func proofOddClassifyReceipt(key []byte) (byte, string) {
	if canonicalReceiptStamp(key) {
		return 0, ""
	}
	stamp, _, found := strings.Cut(string(key), "_")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if !found || err != nil {
		return proofOddReceiptGlobal, ""
	}
	return proofOddReceiptHour, at.UTC().Truncate(time.Hour).Format(hourLayout)
}

// proofOddCountKey locates the entry count for one kind and hour. The hour is
// empty for a global kind, so the key is 'n', kind, hour.
func proofOddCountKey(kind byte, hour string) []byte {
	out := make([]byte, 2+len(hour))
	out[0] = proofOddCountPrefix
	out[1] = kind
	copy(out[2:], hour)
	return out
}

// proofOddLoadCount returns the recorded entry count; a missing count reads as
// zero. The counts marker is written last at completion and implies every count
// exists, but a count deleted together with its entries and pointer leaves no
// trace, so zero here is indistinguishable from a kind that legitimately has no
// entries.
func proofOddLoadCount(b *bolt.Bucket, kind byte, hour string) (uint64, bool) {
	v := b.Get(proofOddCountKey(kind, hour))
	if v == nil {
		return 0, true
	}
	if len(v) != 8 {
		return 0, false
	}
	return binary.BigEndian.Uint64(v), true
}

// proofOddCountAdd adjusts the entry count for one kind and hour. A negative
// delta that would underflow means the recorded count no longer describes the
// bucket; the counts marker is dropped so proofs fall back to the exhaustive
// scan and the next migration rebuilds the classification. The writer itself
// must never fail because of index bookkeeping.
func proofOddCountAdd(b *bolt.Bucket, kind byte, hour string, delta int) error {
	if delta == 0 {
		return nil
	}
	count, ok := proofOddLoadCount(b, kind, hour)
	if !ok {
		return b.Delete(proofOddCountsMarkerKey)
	}
	if delta < 0 {
		if count < uint64(-delta) {
			return b.Delete(proofOddCountsMarkerKey)
		}
		count -= uint64(-delta)
	} else {
		count += uint64(delta)
	}
	if count == 0 {
		return b.Delete(proofOddCountKey(kind, hour))
	}
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], count)
	return b.Put(proofOddCountKey(kind, hour), data[:])
}

// proofOddNote writes the classification of one object, replacing any previous
// location through its pointer. The location count for the old and new kinds is
// maintained in the same transaction; a location entry the bucket lost out of
// band is still subtracted, which keeps the count equal to the bucket once the
// object is written again.
func proofOddNote(tx *bolt.Tx, pointer byte, key, data []byte, kind byte, hour string) error {
	b := proofOddBucket(tx)
	if b == nil {
		proofOddInvalidate(tx)
		return nil
	}
	pointerKey := proofOddPointerKey(pointer, key)
	if old := b.Get(pointerKey); old != nil {
		if oldKind, oldHour, ok := proofOddParsePointer(old); ok {
			if err := b.Delete(proofOddLocationKey(oldKind, oldHour, key)); err != nil {
				return err
			}
			if err := proofOddCountAdd(b, oldKind, oldHour, -1); err != nil {
				return err
			}
		} else if err := b.Delete(pointerKey); err != nil {
			return err
		}
	}
	if kind == 0 {
		return b.Delete(pointerKey)
	}
	if err := b.Put(proofOddLocationKey(kind, hour, key), proofOddDigest(key, data)); err != nil {
		return err
	}
	if err := proofOddCountAdd(b, kind, hour, +1); err != nil {
		return err
	}
	return b.Put(pointerKey, proofOddPointerValue(kind, hour))
}

// proofOddNoteRaw maintains the classification after a raw row write. Callers
// must pass the bytes that will be (or just were) stored at key.
func proofOddNoteRaw(tx *bolt.Tx, key, data []byte) error {
	kind, hour := byte(0), ""
	if !proofOddRawIsCanonical(key, data) {
		kind, hour = proofOddClassifyRaw(tx, key, data)
	}
	return proofOddNote(tx, proofOddRawPointer, key, data, kind, hour)
}

// proofOddNoteRawEvent is proofOddNoteRaw for a row the caller just produced
// with json.Marshal(event) under key == usageKey(event). It reaches the same
// decision as proofOddRawIsCanonical without decoding the bytes again on the
// hot write path, where a second JSON pass measurably slows the writer.
func proofOddNoteRawEvent(tx *bolt.Tx, key, data []byte, event usage.Event) error {
	if !event.Timestamp.IsZero() && strings.TrimSpace(event.RecordID) != "" && strings.TrimSpace(event.ClientName) != "" && usageKey(event.Timestamp, event.RecordID) == string(key) {
		return proofOddNote(tx, proofOddRawPointer, key, data, 0, "")
	}
	return proofOddNoteRaw(tx, key, data)
}

// proofOddNoteReceipt maintains the classification after a receipt write.
func proofOddNoteReceipt(tx *bolt.Tx, key, data []byte) error {
	kind, hour := proofOddClassifyReceipt(key)
	return proofOddNote(tx, proofOddReceiptPointer, key, data, kind, hour)
}

// proofOddRemoveRaw drops the classification of a raw row. Extra entries are
// harmless (walks re-derive each verdict from the stored bytes), so a damaged
// pointer is simply removed.
func proofOddRemoveRaw(tx *bolt.Tx, key []byte) error {
	b := proofOddBucket(tx)
	if b == nil {
		proofOddInvalidate(tx)
		return nil
	}
	pointerKey := proofOddPointerKey(proofOddRawPointer, key)
	old := b.Get(pointerKey)
	if old == nil {
		return nil
	}
	if kind, hour, ok := proofOddParsePointer(old); ok {
		if err := b.Delete(proofOddLocationKey(kind, hour, key)); err != nil {
			return err
		}
		if err := proofOddCountAdd(b, kind, hour, -1); err != nil {
			return err
		}
	}
	return b.Delete(pointerKey)
}

// proofOddEntryMatches checks that the indexed object still exists with the
// bytes the index recorded. A missing object makes the index untrusted, which
// falls back to the exhaustive scan.
func proofOddEntryMatches(data *bolt.Bucket, key, want []byte) bool {
	if data == nil || len(want) != sha256.Size {
		return false
	}
	actualKey, actualValue := data.Cursor().Seek(key)
	return actualKey != nil && bytes.Equal(actualKey, key) && bytes.Equal(want, proofOddDigest(key, actualValue))
}

// proofOddDataBucket returns the bucket that location kind indexes: raw kinds
// point into the usage events bucket, receipt kinds into the receipts bucket.
func proofOddDataBucket(kind byte, raw, receipts *bolt.Bucket) *bolt.Bucket {
	if kind == proofOddRawGlobal || kind == proofOddRawHour {
		return raw
	}
	return receipts
}

// proofOddPointerFor returns the pointer kind that locates an object of a
// location kind.
func proofOddPointerFor(kind byte) byte {
	switch kind {
	case proofOddRawGlobal, proofOddRawHour:
		return proofOddRawPointer
	default:
		return proofOddReceiptPointer
	}
}

// proofOddLoadGlobal returns every global odd key and verifies each entry. The
// entry count must equal the count the writers maintain: a location entry
// deleted while its count remains would otherwise read as "this kind has no odd
// rows" and let a scoped proof skip a row the exhaustive walk acts on. A
// location entry, its pointer, and its count removed together stay
// self-consistent and are outside this check. Each entry must also still own its
// pointer in the same location.
func proofOddLoadGlobal(tx *bolt.Tx, kind byte, data *bolt.Bucket) ([][]byte, bool) {
	index := proofOddBucket(tx)
	if index == nil || data == nil || index.Get(proofOddCountsMarkerKey) == nil {
		return nil, false
	}
	want, ok := proofOddLoadCount(index, kind, "")
	if !ok {
		return nil, false
	}
	pointer := proofOddPointerFor(kind)
	pointerValue := proofOddPointerValue(kind, "")
	var keys [][]byte
	cursor := index.Cursor()
	for k, v := cursor.Seek([]byte{kind}); k != nil && len(k) > 0 && k[0] == kind; k, v = cursor.Next() {
		if len(v) != sha256.Size {
			return nil, false
		}
		key := k[1:]
		if !proofOddEntryMatches(data, key, v) {
			return nil, false
		}
		if !bytes.Equal(index.Get(proofOddPointerKey(pointer, key)), pointerValue) {
			return nil, false
		}
		proofOddIndexLoads.Add(1)
		keys = append(keys, append([]byte(nil), key...))
	}
	if uint64(len(keys)) != want {
		return nil, false
	}
	return keys, true
}

// proofOddLoadHour returns the odd keys indexed under one UTC hour and
// verifies each entry against the bucket and its pointer, then reconciles the
// visited count with the count the writers maintain for that hour. The same
// self-consistent removal of an entry, its pointer, and its count is outside
// this check.
func proofOddLoadHour(tx *bolt.Tx, kind byte, hour string, data *bolt.Bucket) ([][]byte, bool) {
	index := proofOddBucket(tx)
	if index == nil || data == nil || index.Get(proofOddCountsMarkerKey) == nil {
		return nil, false
	}
	want, ok := proofOddLoadCount(index, kind, hour)
	if !ok {
		return nil, false
	}
	pointer := proofOddPointerFor(kind)
	pointerValue := proofOddPointerValue(kind, hour)
	prefix := append([]byte{kind}, hour...)
	var keys [][]byte
	cursor := index.Cursor()
	for k, v := cursor.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = cursor.Next() {
		if len(k) == len(prefix) || len(v) != sha256.Size {
			return nil, false
		}
		key := k[len(prefix):]
		if !proofOddEntryMatches(data, key, v) {
			return nil, false
		}
		if !bytes.Equal(index.Get(proofOddPointerKey(pointer, key)), pointerValue) {
			return nil, false
		}
		proofOddIndexLoads.Add(1)
		keys = append(keys, append([]byte(nil), key...))
	}
	if uint64(len(keys)) != want {
		return nil, false
	}
	return keys, true
}

// proofOddVerifyCheckEvery bounds how long the verification below can run
// without observing cancellation. The pass holds one write transaction, which
// bbolt cannot interrupt, so the startup budget's context is checked every this
// many index entries; a cancelled pass returns the context error and the
// transaction rolls back, leaving the untouched index for the next start.
const proofOddVerifyCheckEvery = 1024

// proofOddVerifyCounts re-derives every per-kind count from the bucket and
// checks that every location entry still owns its pointer. It is O(index
// entries), so it runs only when a startup migration would otherwise trust a
// completed index; a fresh proof instead reconciles the counts of the hours it
// loads. A mismatch means an entry was added or removed out of band while the
// remaining counts, pointers, or digests disagree; removing a location entry,
// its pointer, and its count together leaves no mismatch to find. A non-nil
// error means the pass was interrupted and
// proves nothing; the caller must not treat it as a mismatch.
func proofOddVerifyCounts(ctx context.Context, tx *bolt.Tx) (bool, error) {
	index := proofOddBucket(tx)
	if index == nil || index.Get(proofOddCountsMarkerKey) == nil {
		return false, nil
	}
	rawData := tx.Bucket([]byte(BucketUsageEvents))
	receiptData := tx.Bucket([]byte(BucketUsageReceipts))
	actual := make(map[string]uint64)
	declared := make(map[string]struct{})
	cursor := index.Cursor()
	checked := 0
	for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
		checked++
		if checked%proofOddVerifyCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		if len(k) == 0 {
			return false, nil
		}
		switch k[0] {
		case proofOddRawGlobal, proofOddReceiptGlobal:
			key := k[1:]
			if !proofOddEntryMatches(proofOddDataBucket(k[0], rawData, receiptData), key, v) {
				return false, nil
			}
			if !bytes.Equal(index.Get(proofOddPointerKey(proofOddPointerFor(k[0]), key)), proofOddPointerValue(k[0], "")) {
				return false, nil
			}
			actual[string(proofOddCountKey(k[0], ""))]++
		case proofOddRawHour, proofOddReceiptHour:
			if len(k) < 1+len(hourLayout) {
				return false, nil
			}
			hour := string(k[1 : 1+len(hourLayout)])
			key := k[1+len(hourLayout):]
			if !proofOddEntryMatches(proofOddDataBucket(k[0], rawData, receiptData), key, v) {
				return false, nil
			}
			if !bytes.Equal(index.Get(proofOddPointerKey(proofOddPointerFor(k[0]), key)), proofOddPointerValue(k[0], hour)) {
				return false, nil
			}
			actual[string(proofOddCountKey(k[0], hour))]++
		case proofOddCountPrefix:
			if len(v) != 8 {
				return false, nil
			}
			declared[string(k)] = struct{}{}
		case proofOddRawPointer, proofOddReceiptPointer, 'm':
			// Pointers and the marker carry no count of their own.
		default:
			// Any other key is not part of the classification. It cannot hide an
			// entry: rewriting a location entry's kind byte makes the count
			// mismatch, and rewriting a pointer breaks the pointer check above.
		}
	}
	for key, count := range actual {
		v := index.Get([]byte(key))
		if len(v) != 8 || binary.BigEndian.Uint64(v) != count {
			return false, nil
		}
	}
	return len(actual) == len(declared), nil
}

// MigrateUsageProofOddIndex builds or resumes the persisted classification in
// bounded write transactions. It is repeatable and resumable: the build records
// its scheme in the transaction that scans the first raw batch, progress is
// stored in meta after each batch, the ready marker is only written in the
// transaction that finishes the receipt phase, and a cancelled or failed run
// leaves proofs on the full-scan fallback. A partial build whose scheme is
// missing or different was made by older location semantics; it is discarded
// and rebuilt from the raw and receipt buckets instead of resumed.
func MigrateUsageProofOddIndex(ctx context.Context, db *bolt.DB, batchSize int) error {
	if db == nil {
		return fmt.Errorf("usage proof odd index: nil database")
	}
	if batchSize <= 0 {
		batchSize = proofOddMigrateBatch
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done := false
		err := db.Update(func(tx *bolt.Tx) error {
			if proofOddBucket(tx) == nil || proofOddMeta(tx) == nil {
				return fmt.Errorf("usage proof odd index: bucket or meta missing")
			}
			if proofOddIndexCurrent(tx) {
				// The markers say the classification is current. They cannot see
				// an entry deleted out of band while its count remains, so
				// re-derive the counts and the pointer set before trusting it; a
				// mismatch discards the index instead of leaving proofs on a
				// permanent full-scan fallback. A location entry, its pointer,
				// and its count deleted together leave nothing to reconcile and
				// remain outside detection, as documented at the top of this
				// file.
				verified, err := proofOddVerifyCounts(ctx, tx)
				if err != nil {
					return err
				}
				if verified {
					done = true
					return nil
				}
				return proofOddResetBucket(tx)
			}
			meta := proofOddMeta(tx)
			if string(meta.Get([]byte(proofOddReadyKey))) == "1" {
				// The classification was completed for an older raw revision or
				// scheme: a writer that does not maintain the index (an older
				// binary after a downgrade) advanced the revision without it, or the
				// index predates the current location semantics or the entry
				// counts. Discard the stale classification and rebuild; proofs
				// stay on the fallback until this finishes.
				return proofOddResetBucket(tx)
			}
			if string(meta.Get([]byte(proofOddSchemeKey))) != proofOddScheme {
				// A build records its scheme in the transaction that scans the
				// first raw batch, so resumable progress always carries it. Any
				// phase, cursor, or entry without the current scheme was made by
				// older location semantics (or predates the marker); its entries
				// cannot be trusted and are discarded rather than resumed.
				if proofOddHasProgress(tx) {
					return proofOddResetBucket(tx)
				}
				if err := meta.Put([]byte(proofOddSchemeKey), []byte(proofOddScheme)); err != nil {
					return err
				}
			}
			phase := string(meta.Get([]byte(proofOddPhaseKey)))
			switch phase {
			case "", "raw":
				return migrateProofOddRaw(ctx, tx, batchSize)
			case "receipts":
				return migrateProofOddReceipts(ctx, tx, batchSize)
			default:
				return fmt.Errorf("usage proof odd index: unknown phase %q", phase)
			}
		})
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func migrateProofOddRaw(ctx context.Context, tx *bolt.Tx, batchSize int) error {
	meta := proofOddMeta(tx)
	raw := tx.Bucket([]byte(BucketUsageEvents))
	cursor := raw.Cursor()
	var key, value []byte
	if resume := meta.Get([]byte(proofOddRawCursorKey)); resume != nil {
		// A resumed scan only describes the bucket while the raw revision it
		// started from is still current. A writer that advanced the revision
		// without maintaining the index (an older binary or an out-of-band
		// edit) left part of the bucket unobserved; resuming would never visit
		// the rows behind the cursor, so the build is discarded instead.
		if !proofOddRawRevisionBound(tx) {
			return proofOddResetBucket(tx)
		}
		key, value = cursor.Seek(resume)
		if key != nil && bytes.Equal(key, resume) {
			key, value = cursor.Next()
		}
	} else {
		// Record the revision this scan starts from before touching any row.
		// Supported writers advance the snapshot step by step in the same
		// transaction that maintains the index (proofOddFollowRawRevision), so
		// the check below accepts a build that raced only supported writes and
		// discards one that raced an unsupported writer.
		var start [8]byte
		binary.BigEndian.PutUint64(start[:], rawRevision(tx))
		if err := meta.Put([]byte(proofOddRawRevisionKey), start[:]); err != nil {
			return err
		}
		key, value = cursor.First()
	}
	var last []byte
	processed := 0
	for ; key != nil && processed < batchSize; key, value = cursor.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := proofOddNoteRaw(tx, key, value); err != nil {
			return err
		}
		last = append(last[:0], key...)
		processed++
	}
	if key == nil {
		// The scan covered the bucket only if no writer advanced the revision
		// without maintaining the index since the start-of-scan snapshot.
		// Stamping the live revision here would make a paused raw phase report
		// every change it never saw as covered.
		if !proofOddRawRevisionBound(tx) {
			return proofOddResetBucket(tx)
		}
		if err := meta.Delete([]byte(proofOddRawCursorKey)); err != nil {
			return err
		}
		return meta.Put([]byte(proofOddPhaseKey), []byte("receipts"))
	}
	return meta.Put([]byte(proofOddRawCursorKey), last)
}

func migrateProofOddReceipts(ctx context.Context, tx *bolt.Tx, batchSize int) error {
	meta := proofOddMeta(tx)
	if !proofOddRawRevisionBound(tx) {
		// The raw phase did not finish for the live revision: its entries no
		// longer describe the bucket, so this state cannot be resumed. This
		// check runs at the start of every batch transaction, including the one
		// that scans the final receipt batch; the identical check at the end of
		// this function runs later in that same transaction. rawRevision cannot
		// change while the transaction is open, and the batch only writes index
		// entries, so there the completion check is already implied by this
		// one. It is defensive redundancy, not an independently observable
		// condition: no existing test can kill one check without the other.
		// Keep both as a guard against future transaction-boundary changes.
		return proofOddResetBucket(tx)
	}
	receipts := tx.Bucket([]byte(BucketUsageReceipts))
	cursor := receipts.Cursor()
	var key, value []byte
	if resume := meta.Get([]byte(proofOddReceiptCursorKey)); resume != nil {
		key, value = cursor.Seek(resume)
		if key != nil && bytes.Equal(key, resume) {
			key, value = cursor.Next()
		}
	} else {
		key, value = cursor.First()
	}
	var last []byte
	processed := 0
	for ; key != nil && processed < batchSize; key, value = cursor.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := proofOddNoteReceipt(tx, key, value); err != nil {
			return err
		}
		last = append(last[:0], key...)
		processed++
	}
	if key != nil {
		return meta.Put([]byte(proofOddReceiptCursorKey), last)
	}
	// The receipt phase must finish against the same raw revision the raw phase
	// scanned. A writer that advanced the revision in between (an older binary
	// or an out-of-band edit) leaves that change unobserved; stamping the live
	// revision would report a complete index that never saw it. This call
	// already passed the same check at its start, in this transaction, so a
	// failure here cannot be triggered without also failing there; the check is
	// kept as defensive redundancy (see the entry check above).
	if !proofOddRawRevisionBound(tx) {
		return proofOddResetBucket(tx)
	}
	if err := meta.Delete([]byte(proofOddReceiptCursorKey)); err != nil {
		return err
	}
	if err := meta.Delete([]byte(proofOddPhaseKey)); err != nil {
		return err
	}
	if err := meta.Delete([]byte(proofOddRawRevisionKey)); err != nil {
		return err
	}
	// Bind the finished classification to the location semantics and the raw
	// revision it covered. Every later supported write advances both; a write
	// that skips the index (an older binary) advances only the revision and
	// makes the index stale.
	if err := meta.Put([]byte(proofOddSchemeKey), []byte(proofOddScheme)); err != nil {
		return err
	}
	var revision [8]byte
	binary.BigEndian.PutUint64(revision[:], rawRevision(tx))
	if err := meta.Put([]byte(proofOddRevisionKey), revision[:]); err != nil {
		return err
	}
	// The counts were maintained by the same per-entry writes that built the
	// index; this marker says a reader may reconcile them. It is written last,
	// so a completed marker always covers a full set of counts.
	if err := proofOddBucket(tx).Put(proofOddCountsMarkerKey, []byte("1")); err != nil {
		return err
	}
	return meta.Put([]byte(proofOddReadyKey), []byte("1"))
}

// ResetUsageProofOddIndex discards the persisted classification. It is the
// documented repair after out-of-band edits or detected damage, and the next
// MigrateUsageProofOddIndex rebuilds it from the raw and receipt buckets. It is
// exported so maintenance code has a supported rebuild path, and tests use it
// to start from a clean classification.
func ResetUsageProofOddIndex(db *bolt.DB) error {
	if db == nil {
		return fmt.Errorf("usage proof odd index: nil database")
	}
	return db.Update(proofOddResetBucket)
}
