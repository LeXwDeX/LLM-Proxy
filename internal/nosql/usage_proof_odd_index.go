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
// not. Absent, incomplete, or damaged index data makes proofs fall back to the
// previous full scans; they never fail open.
//
// The completion marker records the classification scheme version, so an index
// built with different location semantics is rebuilt instead of trusted. A
// build records that scheme in the transaction that scans its first raw batch,
// so a resumable partial build always carries it and any partial state without
// the current scheme was made by older semantics and is discarded. The raw
// phase also records the raw revision it scanned; the receipt phase only
// completes when that snapshot still matches, so a writer that advances the
// data between the phases without maintaining the index forces a rebuild
// instead of a revision stamp the scan never observed.
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
// kind byte followed by the optional hour; pointers use 'r'/'c'.
const (
	proofOddRawGlobal      = 'g'
	proofOddRawHour        = 'h'
	proofOddReceiptGlobal  = 'G'
	proofOddReceiptHour    = 'H'
	proofOddRawPointer     = 'r'
	proofOddReceiptPointer = 'c'
)

var (
	// proofOddFullClassifications counts keys examined by the fallback full
	// classification scan. Tests and operators use it to prove the scoped path
	// no longer walks all history.
	proofOddFullClassifications atomic.Int64
	// proofOddIndexLoads counts entries loaded from the persisted index.
	proofOddIndexLoads atomic.Int64
)

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
	if string(meta.Get([]byte(proofOddReadyKey))) != "1" {
		return nil
	}
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], revision)
	return meta.Put([]byte(proofOddRevisionKey), data[:])
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

// proofOddNote writes the classification of one object, replacing any previous
// location through its pointer.
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
	}
	return b.Delete(pointerKey)
}

// proofOddEntryMatches checks that the indexed object still exists with the
// bytes the index recorded. A missing object makes the index untrusted, which
// falls back to the exhaustive scan.
func proofOddEntryMatches(data *bolt.Bucket, key, want []byte) bool {
	actualKey, actualValue := data.Cursor().Seek(key)
	return actualKey != nil && bytes.Equal(actualKey, key) && bytes.Equal(want, proofOddDigest(key, actualValue))
}

// proofOddLoadGlobal returns every global odd key and verifies each entry.
func proofOddLoadGlobal(tx *bolt.Tx, kind byte, data *bolt.Bucket) ([][]byte, bool) {
	index := proofOddBucket(tx)
	if index == nil || data == nil {
		return nil, false
	}
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
		proofOddIndexLoads.Add(1)
		keys = append(keys, append([]byte(nil), key...))
	}
	return keys, true
}

// proofOddLoadHour returns the odd keys indexed under one UTC hour and
// verifies each entry.
func proofOddLoadHour(tx *bolt.Tx, kind byte, hour string, data *bolt.Bucket) ([][]byte, bool) {
	index := proofOddBucket(tx)
	if index == nil || data == nil {
		return nil, false
	}
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
		proofOddIndexLoads.Add(1)
		keys = append(keys, append([]byte(nil), key...))
	}
	return keys, true
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
				done = true
				return nil
			}
			meta := proofOddMeta(tx)
			if string(meta.Get([]byte(proofOddReadyKey))) == "1" {
				// The classification was completed for an older raw revision or
				// scheme: a writer that does not maintain the index (an older
				// binary after a downgrade) advanced the data without it, or the
				// index predates the current location semantics. Discard the
				// stale classification and rebuild; proofs stay on the fallback
				// until this finishes.
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
		if err := proofOddNoteRaw(tx, key, value); err != nil {
			return err
		}
		last = append(last[:0], key...)
		processed++
	}
	if key == nil {
		if err := meta.Delete([]byte(proofOddRawCursorKey)); err != nil {
			return err
		}
		if err := meta.Put([]byte(proofOddPhaseKey), []byte("receipts")); err != nil {
			return err
		}
		// Bind the receipt phase to the raw revision this scan covered. A
		// writer that advances the revision before the receipt phase finishes
		// without maintaining the index (an older binary or an out-of-band
		// edit) leaves part of the data unobserved; the receipt phase discards
		// this build instead of stamping a revision the scan never saw.
		var revision [8]byte
		binary.BigEndian.PutUint64(revision[:], rawRevision(tx))
		return meta.Put([]byte(proofOddRawRevisionKey), revision[:])
	}
	return meta.Put([]byte(proofOddRawCursorKey), last)
}

func migrateProofOddReceipts(ctx context.Context, tx *bolt.Tx, batchSize int) error {
	meta := proofOddMeta(tx)
	if !proofOddRawRevisionBound(tx) {
		// The raw phase did not finish for the live revision: its entries no
		// longer describe the bucket, so this state cannot be resumed.
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
	// revision would report a complete index that never saw it.
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
