package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// jsonUnits renders cost units in a stable order for direct comparison.
func jsonUnits(units []CostUnit) (string, error) {
	sort.Slice(units, func(i, j int) bool {
		a, _ := json.Marshal(units[i])
		b, _ := json.Marshal(units[j])
		return string(a) < string(b)
	})
	out := make([]json.RawMessage, len(units))
	for i := range units {
		data, err := json.Marshal(units[i])
		if err != nil {
			return "", err
		}
		out[i] = data
	}
	data, err := json.Marshal(out)
	return string(data), err
}

// Issue #125 phase P2: a client-filtered quota read must enumerate the
// client's own window objects through the client index instead of walking
// every client's raw rows, receipts and quota cells. These tests pin the two
// properties that make the change safe: the work must not grow with other
// clients' retained rows, and the verdict must stay identical to the
// exhaustive read on every damaged database.

// clientScopeFallbackReason is the countable fallback reason a client-filtered
// read records when the scoped enumeration declines and it replays the
// exhaustive path. The literal is deliberate: the test pins the exported
// metrics key, not an internal symbol.
const clientScopeFallbackReason = "client_scope_fallback"

type clientScopeVisits struct {
	raw, receipt, quota, evidence int
	// Probe enumeration over the window's hours: one hour-state read and one
	// index range seek per client prefix (filter client and empty client), and
	// one decode per empty-client row. These grow with the window hours rather
	// than with the visited objects, so the scaling guards watch them too.
	hourStates, seeks, emptyDecoded int
	indexScan, legacyScan           bool
}

// scopedIndexRead reports whether this run took the index path to a result
// without replaying the legacy scan: the client-scoped enumeration replaces
// the window evidence map only in that case.
func (v clientScopeVisits) scopedIndexRead() bool {
	return v.indexScan && !v.legacyScan && v.evidence == 0
}

// clientScopeReadOptions shapes one quota read: the fast path can be forced
// (tests of the scoped enumeration) and the legacy scan requested, exactly as
// the production hooks do.
type clientScopeReadOptions struct {
	forceFast   bool
	forceLegacy bool
}

func readClientScopeUnitsOpt(store *UsageStore, filter usage.Filter, resolver usage.RateResolver, precise bool, opts clientScopeReadOptions) (clientScopeVisits, []CostUnit, error) {
	saved := integrityForceFast.Load()
	if opts.forceFast {
		integrityForceFast.Store(true)
	}
	defer integrityForceFast.Store(saved)
	var visits clientScopeVisits
	savedHour, savedSeek, savedDecoded := clientScopeProbeHourState, clientScopeProbeRangeSeek, clientScopeProbeDecoded
	clientScopeProbeHourState = func() { visits.hourStates++ }
	clientScopeProbeRangeSeek = func() { visits.seeks++ }
	clientScopeProbeDecoded = func() { visits.emptyDecoded++ }
	defer func() {
		clientScopeProbeHourState, clientScopeProbeRangeSeek, clientScopeProbeDecoded = savedHour, savedSeek, savedDecoded
	}()
	result, err := store.readCostUnitsInScope(context.Background(), filter, resolver, precise, nil, costReadHooks{
		windowKeys:  true,
		forceLegacy: opts.forceLegacy,
		evidenceRaw: func() { visits.evidence++ },
		scan: func(index bool) {
			if index {
				visits.indexScan = true
			} else {
				visits.legacyScan = true
			}
		},
		direct: &directIntegrityHooks{
			windowKeys: true,
			raw:        func() { visits.raw++ },
			receipt:    func() { visits.receipt++ },
			quota:      func() { visits.quota++ },
		},
	}, nil)
	return visits, result.units, err
}

func readClientScopeUnits(store *UsageStore, client string, from, to time.Time, forceFast bool) (clientScopeVisits, []CostUnit, error) {
	return readClientScopeUnitsOpt(store, usage.Filter{ClientName: client, From: &from, To: &to}, exactCostTable{replayCosts}, true, clientScopeReadOptions{forceFast: forceFast})
}

// clientScopeFallbacks reads the countable client-scope fallback counter.
func clientScopeFallbacks(db *bolt.DB) int {
	return int(UsageIntegrityStats(db).FallbackReads[clientScopeFallbackReason])
}

// clientScopeFullReference runs today's read: the in-memory classification is
// dropped, so the costs take the exhaustive walks with no fast path and no
// scope. It is the verdict a scoped read must reproduce.
func clientScopeFullReference(t *testing.T, db *bolt.DB, filter usage.Filter, resolver usage.RateResolver, precise bool) ([]CostUnit, error) {
	t.Helper()
	ForgetUsageIntegrityClassification(db)
	store := NewUsageStore(db)
	result, err := store.readCostUnitsInScope(context.Background(), filter, resolver, precise, nil, costReadHooks{windowKeys: true}, nil)
	return result.units, err
}

// clientScopeProbeDeclines reports whether the scope probe declines for
// client over [from, to] under the current classification. It must run before
// any ForgetUsageIntegrityClassification call, which drops the classification
// the probe needs.
//
// The helper only applies to a scopeEligible shape: a non-empty client, an
// empty model, hooks.windowKeys and a nil allow, exactly the read shape
// readClientScopeUnits builds. It also needs a live classification on a
// store without the index damage that makes classification unavailable;
// integrityFastFrom then returns nil, probeClientScopedRead returns nil at
// once, and the decline assertion would pass vacuously. The nil check after
// the transaction fails loudly instead of reporting that empty pass.
//
// A declined read normally counts exactly one client_scope fallback, but so
// does a scoped attempt that fails during its walks: the caller discards it,
// counts a fallback and replays the exhaustive read. The counter alone
// therefore cannot prove that the probe itself declined; a shape whose only
// damage is visible to the probe asserts the probe directly.
func clientScopeProbeDeclines(t *testing.T, db *bolt.DB, client string, from, to time.Time) bool {
	t.Helper()
	start, end := from.Truncate(time.Hour), to.Truncate(time.Hour).Add(time.Hour)
	declined := false
	var fast *integrityFast
	if err := db.View(func(tx *bolt.Tx) error {
		ctx := integrityReadContext(context.Background(), tx, from, to)
		progress, err := loadIndexProgress(tx)
		if err != nil {
			return err
		}
		// Report a missing classification after the transaction closes: calling
		// t.Fatalf inside the View callback would unwind through bbolt's
		// transaction handling instead of releasing it normally.
		fast = integrityFastFrom(ctx, tx)
		if fast == nil {
			return nil
		}
		declined = probeClientScopedRead(ctx, tx, client, start, end, progress, fast) == nil
		return nil
	}); err != nil {
		t.Fatalf("probe %s: %v", client, err)
	}
	if fast == nil {
		t.Fatalf("probe %s: no usable classification (integrityFastFrom returned nil), so the probe returns nil immediately and this shape assertion would pass vacuously: the shape assertion needs a usable classification, and this helper only applies to a scopeEligible shape (client set, empty model, windowKeys, nil allow) on a classified store", client)
	}
	return declined
}

// clientScopeAssertSameVerdict compares the scoped read with the exhaustive
// reference: same failure category, and on success the same cost units.
// Exhaustive failure evidence is chosen by sorted key, so a category mismatch
// is a real divergence and is reported on the first comparison.
func clientScopeAssertSameVerdict(t *testing.T, label string, gotUnits []CostUnit, gotErr error, reference func() ([]CostUnit, error)) {
	t.Helper()
	wantUnits, wantErr := reference()
	gotClass := integrityErrClass(gotErr)
	if gotClass != integrityErrClass(wantErr) {
		t.Fatalf("%s: scoped verdict %q, exhaustive %q", label, gotClass, integrityErrClass(wantErr))
	}
	if gotErr != nil {
		return
	}
	gotJSON, err := jsonUnits(gotUnits)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	wantJSON, err := jsonUnits(wantUnits)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if gotJSON != wantJSON {
		t.Fatalf("%s: scoped units differ from the exhaustive read:\n got %s\nwant %s", label, gotJSON, wantJSON)
	}
}

// clientScopeRawKeys lists the raw keys whose body names client, oldest
// first. Damage helpers use it to pick their victim before opening a write
// transaction.
func clientScopeRawKeys(t *testing.T, db *bolt.DB, client string) [][]byte {
	t.Helper()
	var keys [][]byte
	if err := db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		if raw == nil {
			return nil
		}
		return raw.ForEach(func(k, v []byte) error {
			if v == nil {
				return nil
			}
			var e usage.Event
			if json.Unmarshal(v, &e) != nil {
				return nil
			}
			if e.ClientName == client {
				keys = append(keys, bytes.Clone(k))
			}
			return nil
		})
	}); err != nil {
		t.Fatalf("list raw keys of %s: %v", client, err)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
	return keys
}

func clientScopePickKey(t *testing.T, db *bolt.DB, client string) []byte {
	t.Helper()
	keys := clientScopeRawKeys(t, db, client)
	if len(keys) == 0 {
		t.Fatalf("no raw rows for %s", client)
	}
	return keys[len(keys)-1]
}

func clientScopeEvent(t *testing.T, db *bolt.DB, key []byte) usage.Event {
	t.Helper()
	var e usage.Event
	if err := db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		if raw == nil {
			return fmt.Errorf("raw bucket missing")
		}
		value := raw.Get(key)
		if value == nil {
			return fmt.Errorf("raw row %q missing", key)
		}
		return json.Unmarshal(value, &e)
	}); err != nil {
		t.Fatalf("read raw row: %v", err)
	}
	return e
}

func clientScopeSyntheticEvent(at time.Time, client, recordID string) usage.Event {
	return usage.Event{
		Timestamp: at, ClientName: client, Target: "anthropic", Model: "executor",
		EndpointType: "claude", Path: "/v1/messages", StatusCode: 200, InputTokens: 100, OutputTokens: 10,
		RecordID: recordID,
	}
}

func clientScopePutRaw(t *testing.T, db *bolt.DB, key []byte, e usage.Event) {
	t.Helper()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put(key, data)
	}); err != nil {
		t.Fatalf("put raw row: %v", err)
	}
}

// clientScopeIndexKey builds the v2 index key a proof for rawKey would carry
// if it named client at time at.
func clientScopeIndexKey(client string, at time.Time, rawKey []byte) []byte {
	// clientIndexPrefix already ends with the field delimiter.
	return []byte(fmt.Sprintf("%s%s|%s", clientIndexPrefix(client), at.UTC().Format(clientIndexTimeLayout), rawKey))
}

// clientScopeRewriteProof replaces the v2 proof of rawKey with an atomic
// proof pointing at indexKey. Rewriting a proof is how a stale cross-client or
// cross-hour attribution is forged.
func clientScopeRewriteProof(t *testing.T, db *bolt.DB, rawKey, indexKey []byte) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		p, err := loadIndexProgressState(tx, false)
		if err != nil {
			return err
		}
		if p == nil {
			return fmt.Errorf("no v2 progress")
		}
		proofs := tx.Bucket([]byte(indexProofsV2))
		if proofs == nil {
			return fmt.Errorf("no proof bucket")
		}
		old, err := readIndexProof(proofs, p, rawKey)
		if err != nil {
			return err
		}
		proof := indexProof{Version: 2, Generation: p.Generation, Origin: "atomic", IndexKey: indexKey, RawDigest: old.RawDigest}
		proof.Binding = proof.binding(rawKey)
		data, err := json.Marshal(proof)
		if err != nil {
			return err
		}
		return proofs.Put(rawKey, data)
	}); err != nil {
		t.Fatalf("rewrite index proof: %v", err)
	}
}

// clientScopeDeleteRaw removes a raw row and its owner record, leaving its
// index entry and proof behind: an orphaned entry.
func clientScopeDeleteRaw(t *testing.T, db *bolt.DB, key []byte) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketUsageEvents)).Delete(key); err != nil {
			return err
		}
		if b := tx.Bucket([]byte(rawOwnerBucket)); b != nil {
			return b.Delete(key)
		}
		return nil
	}); err != nil {
		t.Fatalf("delete raw row: %v", err)
	}
}

// clientScopeMoveRowIntoWindow leaves a raw row's key (and its key-hour) 40
// days before now, but rewrites its body, client index entry, client/hour state
// and both rollup cells into one of alice's in-window hours, exactly as an
// offline ReindexUsageEvent-style maintenance rewrite may ("an offline rewrite
// may move the row to another hour"; the caller updates the raw value and the
// affected rollups in the same transaction). The row is unowned with an empty
// record_id, the legacy shape whose key stamp equals its event time until the
// rewrite moves the body. withProof additionally writes the v2 atomic proof a
// consistent v2 index would carry for the rewritten event.
//
// The resulting row's event hour (and index entry) lie inside the read window
// while its key text lies outside it, so the skipped evidence scan — which
// observes rows by key text and compares them against the hour states the index
// builds from event hours — cannot see it, while an in-window sibling of the
// same client and hour makes the hour state disagree with the observed count.
func clientScopeMoveRowIntoWindow(t *testing.T, db *bolt.DB, now time.Time, withProof bool) {
	t.Helper()
	sibling := clientScopeEvent(t, db, clientScopePickKey(t, db, "alice"))
	inWindow := sibling.Timestamp.UTC().Truncate(time.Hour).Add(31 * time.Minute)
	event := sibling
	event.Timestamp = inWindow
	event.RecordID = ""
	clientScopeWriteIndexedRow(t, db, []byte(usageKey(now.AddDate(0, 0, -40), "")), event, withProof)
}

// clientScopeMoveEmptyClientRowIntoWindow is the empty-client counterpart of
// clientScopeMoveRowIntoWindow: the client index enumerates empty-client rows
// under the empty prefix, and the probe's hour check must reject a moved row
// there too, not only under the filter client's prefix. The row's event hour,
// index entry, hour state and rollups lie inside the read window while its key
// hour lies 40 days before now, so the skipped evidence scan cannot account
// for it.
//
// withSibling also writes an ordinary in-window empty-client row through the
// same full path, so the in-window hour state and digest cover two rows. The
// probe then declines only because of the moved row's key hour; without the
// sibling the hour has a single, equally moved row.
func clientScopeMoveEmptyClientRowIntoWindow(t *testing.T, db *bolt.DB, now time.Time, withProof, withSibling bool) {
	t.Helper()
	at := now.Add(-12 * time.Hour).Add(30 * time.Minute)
	if withSibling {
		clientScopeWriteIndexedRow(t, db, []byte(usageKey(at, "empty-sibling")), clientScopeSyntheticEvent(at, "", "empty-sibling"), withProof)
	}
	eventAt := at.Truncate(time.Hour).Add(time.Minute)
	clientScopeWriteIndexedRow(t, db, []byte(usageKey(now.AddDate(0, 0, -40), "")), clientScopeSyntheticEvent(eventAt, "", ""), withProof)
}

// clientScopeWriteIndexedRow writes one raw row together with the derived
// state a consistent writer creates in the same transaction: client index
// entry, client/hour state, both rollup cells and, when withProof, the v2
// atomic proof.
func clientScopeWriteIndexedRow(t *testing.T, db *bolt.DB, key []byte, event usage.Event, withProof bool) {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		if raw.Get(key) != nil {
			return fmt.Errorf("raw row %q already exists", key)
		}
		if err := raw.Put(key, data); err != nil {
			return err
		}
		if err := addClientEventIndex(tx, event, key, data); err != nil {
			return err
		}
		if err := bumpAgg(tx, event); err != nil {
			return err
		}
		if err := bumpQuotaAgg(tx, event); err != nil {
			return err
		}
		if !withProof {
			return nil
		}
		p, err := loadIndexProgressState(tx, false)
		if err != nil {
			return err
		}
		if p == nil {
			return fmt.Errorf("no v2 progress")
		}
		return writeIndexProof(tx, p, key, data, event, "atomic")
	}); err != nil {
		t.Fatalf("write indexed row %q: %v", key, err)
	}
}

// clientScopeQuarantine moves a raw row into the quarantine bucket, as the
// retention path does for undecodable rows.
func clientScopeQuarantine(t *testing.T, db *bolt.DB, key []byte) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		data := raw.Get(key)
		if data == nil {
			return fmt.Errorf("raw row %q missing", key)
		}
		if err := quarantineUsageEvent(tx, key, bytes.Clone(data)); err != nil {
			return err
		}
		return raw.Delete(key)
	}); err != nil {
		t.Fatalf("quarantine raw row: %v", err)
	}
}

// buildClientScopeStore records rowsPerClient events for every client inside
// the last 20 days, so a month-long quota window holds all of them.
func buildClientScopeStore(t *testing.T, now time.Time, rowsPerClient map[string]int) (*UsageStore, *bolt.DB) {
	t.Helper()
	db := testDB(t)
	store := NewUsageStore(db)
	store.now = func() time.Time { return now }
	var batch []usage.Event
	id := 0
	for client, rows := range rowsPerClient {
		for i := 0; i < rows; i++ {
			at := now.Add(-time.Duration(1+i%(20*24))*time.Hour + time.Duration(i%3600)*time.Second)
			batch = append(batch, usage.Event{
				RecordID: fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", id), Timestamp: at,
				ClientName: client, Target: "anthropic", Model: "executor",
				EndpointType: "claude", Path: "/v1/messages", StatusCode: 200, InputTokens: 100, OutputTokens: 10,
			})
			id++
		}
	}
	if err := store.RecordBatch(batch); err != nil {
		t.Fatalf("RecordBatch: %v", err)
	}
	return store, db
}

// markClientIndexV1Done makes an index built by Record look like a completed
// v1-only migration: the done marker exists, yet no v2 anchors do.
func markClientIndexV1Done(t *testing.T, db *bolt.DB) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageClientIndexDone), []byte("1"))
	}); err != nil {
		t.Fatalf("mark v1 index: %v", err)
	}
}

// TestClientScopedQuotaReadIgnoresOtherClients is the hook-count proof: with
// two clients holding N rows each, reading one must not grow the raw, receipt
// or quota visits with the other client's rows.
func TestClientScopedQuotaReadIgnoresOtherClients(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	from := now.AddDate(0, -1, 0)
	var baseline clientScopeVisits
	for run, otherRows := range []int{40, 400} {
		store, db := buildClientScopeStore(t, now, map[string]int{"alice": 30, "bob": otherRows})
		migrateClientIndex(t, db)
		if err := RunClassificationOnce(context.Background(), db); err != nil {
			t.Fatalf("classify: %v", err)
		}
		visits, units, err := readClientScopeUnits(store, "alice", from, now, false)
		if err != nil {
			t.Fatalf("bob=%d rows: %v", otherRows, err)
		}
		if len(units) == 0 {
			t.Fatalf("bob=%d rows: no cost units", otherRows)
		}
		if visits.evidence != 0 {
			t.Fatalf("bob=%d rows: evidence scan visited %d raw rows; a client-filtered read must use the client index, not the whole window", otherRows, visits.evidence)
		}
		if run == 0 {
			baseline = visits
			continue
		}
		if visits != baseline {
			t.Fatalf("alice read with bob=%d rows visited %+v, want %+v: the work must not depend on another client's rows", otherRows, visits, baseline)
		}
	}
	if baseline.raw > 4*30 {
		t.Fatalf("alice's own read visited %d raw rows for 30 rows; expected a small constant multiple", baseline.raw)
	}
}

// TestClientScopedQuotaReadPerClientWork checks the same property from the
// scaling side: one client's visits stay flat as more clients join, and the
// per-client quota-cell work is bounded by that client's own cells.
func TestClientScopedQuotaReadPerClientWork(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	from := now.AddDate(0, -1, 0)
	var baseline clientScopeVisits
	for run, clients := range []int{2, 8} {
		rows := make(map[string]int, clients)
		for c := 0; c < clients; c++ {
			rows[fmt.Sprintf("client-%02d", c)] = 30
		}
		store, db := buildClientScopeStore(t, now, rows)
		migrateClientIndex(t, db)
		if err := RunClassificationOnce(context.Background(), db); err != nil {
			t.Fatalf("classify: %v", err)
		}
		visits, units, err := readClientScopeUnits(store, "client-00", from, now, false)
		if err != nil {
			t.Fatalf("clients=%d: %v", clients, err)
		}
		if len(units) != 30 {
			t.Fatalf("clients=%d: got %d units, want 30", clients, len(units))
		}
		if run == 0 {
			baseline = visits
			continue
		}
		if visits != baseline {
			t.Fatalf("client-00 read with %d clients visited %+v, want %+v: per-client work must not grow with the client count", clients, visits, baseline)
		}
	}
}

// buildClientScopeFuzzDB is buildIntegrityFuzzDB's database with the two
// properties a client-filtered quota read needs to ever succeed: every base
// row has an attributable owner (rows with an empty or "unknown" client fail
// every client filter by design, and writeRawOwner writes no owner for them,
// so a v2 migration cannot prove them), and the client index migration runs
// once, at the end, to completion. The event generator, prune and all 26
// damage cases (including legacy no-record-id rows, cross-hour index proofs
// and unowned rows) are the shared ones; damage may still make individual
// reads fail, and the differential comparison covers those verdicts too.
func buildClientScopeFuzzDB(t *testing.T, rng *rand.Rand, damage int) *integrityFuzzDB {
	t.Helper()
	db := testDB(t)
	store := NewUsageStore(db)
	store.now = func() time.Time { return integrityFuzzBase.Add(48 * time.Hour) }
	f := &integrityFuzzDB{db: db, store: store}
	record := func(e usage.Event) {
		if strings.TrimSpace(e.ClientName) == "" || strings.EqualFold(strings.TrimSpace(e.ClientName), "unknown") {
			e.ClientName = "dave"
		}
		if err := store.Record(e); err != nil {
			f.note("record err %v", err)
		}
	}
	events := 6 + rng.Intn(18)
	for i := 0; i < events; i++ {
		record(fuzzEvent(rng))
	}
	if rng.Intn(2) == 0 {
		cut := integrityFuzzBase.Add(time.Duration(rng.Intn(30)) * time.Hour)
		n, err := store.PruneEventsBefore(cut, 1+rng.Intn(20))
		f.note("prune before %v n=%d err=%v", cut, n, err)
	}
	for i := 0; i < rng.Intn(3); i++ {
		record(fuzzEvent(rng))
	}
	// Record leaves the v1 index populated but unproven for v2; one completed
	// migration is what requireClientIndex and the proof checks need.
	migrateClientIndex(t, db)
	// Issue #114: the persistent odd-row classification lets a proof reach odd
	// raw rows by hour instead of scanning them all. Without building it here,
	// the differential harness never exercises that proof path.
	if err := MigrateUsageProofOddIndex(context.Background(), db, 2); err != nil {
		f.note("proof odd index: %v", err)
	}
	for i := 0; i < damage; i++ {
		applyIntegrityDamage(t, rng, f)
	}
	return f
}

// TestClientScopedQuotaReadEqualsFullFuzz runs the differential harness on
// client-filtered quota queries and proves the scoped enumeration actually
// engages: for random databases with random damage, the scoped read must
// return the same verdict and units as the exhaustive read, and on the
// undamaged ones it must not scan the window's raw rows for the evidence map.
func TestClientScopedQuotaReadEqualsFullFuzz(t *testing.T) {
	iterations := integrityFuzzIterations(25)
	engaged, succeeded := 0, 0
	for seed := 0; seed < iterations; seed++ {
		rng := rand.New(rand.NewSource(int64(seed)))
		f := buildClientScopeFuzzDB(t, rng, rng.Intn(5))
		for _, client := range []string{"alice", "bob", "carol"} {
			q := integrityQuery{client: client, from: integrityFuzzBase, to: integrityFuzzBase.Add(40 * time.Hour), windowKeys: true, exact: true}
			if !compareIntegrityModes(t, f, q) {
				t.Fatalf("seed %d: Fast and Full disagree for %s\n damage: %v", seed, q, f.log)
			}
			visits, _, err := readClientScopeUnits(f.store, client, q.from, q.to, true)
			if err != nil {
				continue
			}
			succeeded++
			if visits.scopedIndexRead() {
				engaged++
			}
		}
	}
	if succeeded == 0 {
		t.Fatalf("no fast read succeeded in %d seeds", iterations)
	}
	if engaged == 0 {
		t.Fatalf("the client-scoped enumeration never engaged in %d successful fast reads; the fast read still scans every raw row of the window", succeeded)
	}
	t.Logf("client-scoped read engaged in %d of %d successful fast reads", engaged, succeeded)
}

// TestClientScopedQuotaReadFallbacks pins the explicit fallbacks: a receipt
// inside the window and a missing index decline to the exhaustive path and
// count the same reason, while the verdict stays identical.
func TestClientScopedQuotaReadFallbacks(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	from := now.Add(-24 * time.Hour)

	t.Run("receipt in window", func(t *testing.T) {
		store, db := buildClientScopeStore(t, now, map[string]int{"alice": 10})
		migrateClientIndex(t, db)
		// Rows at now-1h survive; rows at now-2h become receipts inside the read window.
		if _, err := store.PruneEventsBefore(now.Add(-90*time.Minute), 1000); err != nil {
			t.Fatalf("prune: %v", err)
		}
		if err := RunClassificationOnce(context.Background(), db); err != nil {
			t.Fatalf("classify: %v", err)
		}
		before := UsageIntegrityStats(db)
		_, units, err := readClientScopeUnits(store, "alice", from, now, false)
		after := UsageIntegrityStats(db)
		if got := after.FallbackReads[clientScopeFallbackReason]; got != before.FallbackReads[clientScopeFallbackReason]+1 {
			t.Fatalf("client_scope fallback counted %d times, want exactly once (reason not recorded or read replayed)", got-before.FallbackReads[clientScopeFallbackReason])
		}
		want, wantErr := store.readCostUnitsInScope(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &now}, exactCostTable{replayCosts}, true, nil, costReadHooks{forceLegacy: true, windowKeys: true}, nil)
		if (err == nil) != (wantErr == nil) || (err != nil && err.Error() != wantErr.Error()) {
			t.Fatalf("fallback verdict differs from the exhaustive read:\n got err %v\nwant err %v", err, wantErr)
		}
		gotJSON, _ := jsonUnits(units)
		wantJSON, _ := jsonUnits(want.units)
		if gotJSON != wantJSON {
			t.Fatalf("fallback verdict differs from the exhaustive read:\n got %s\nwant %s", gotJSON, wantJSON)
		}
	})

	t.Run("missing index", func(t *testing.T) {
		store, db := buildClientScopeStore(t, now, map[string]int{"alice": 10})
		if err := RunClassificationOnce(context.Background(), db); err != nil {
			t.Fatalf("classify: %v", err)
		}
		before := UsageIntegrityStats(db)
		units, err := store.ReadQuotaCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &now}, exactCostTable{replayCosts}, true)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		after := UsageIntegrityStats(db)
		if got := after.FallbackReads[clientScopeFallbackReason]; got != before.FallbackReads[clientScopeFallbackReason]+1 {
			t.Fatalf("missing index: client_scope fallback counted %d times, want exactly once", got-before.FallbackReads[clientScopeFallbackReason])
		}
		if len(units) == 0 {
			t.Fatalf("missing index: no units")
		}
	})

	t.Run("v1 only index stays scoped", func(t *testing.T) {
		store, db := buildClientScopeStore(t, now, map[string]int{"alice": 10, "bob": 10})
		markClientIndexV1Done(t, db)
		if err := RunClassificationOnce(context.Background(), db); err != nil {
			t.Fatalf("classify: %v", err)
		}
		before := UsageIntegrityStats(db)
		visits, units, err := readClientScopeUnits(store, "alice", from, now, false)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		after := UsageIntegrityStats(db)
		if got, want := after.FallbackReads[clientScopeFallbackReason], before.FallbackReads[clientScopeFallbackReason]; got != want {
			t.Fatalf("v1-only index must not decline the scoped path: fallback %d -> %d", want, got)
		}
		if visits.evidence != 0 {
			t.Fatalf("v1-only index: evidence scan visited %d raw rows", visits.evidence)
		}
		if len(units) != 10 {
			t.Fatalf("v1-only index: got %d units, want 10", len(units))
		}
	})

	t.Run("v1 done with cursor stays exhaustive", func(t *testing.T) {
		store, db := buildClientScopeStore(t, now, map[string]int{"alice": 10, "bob": 10})
		markClientIndexV1Done(t, db)
		// The cursor is a valid raw key; only its coexistence with the done
		// marker makes the state unprovable. The exhaustive evidence scan
		// rejects it, so the probe must not attach a scope that returns units.
		var cursor []byte
		if err := db.View(func(tx *bolt.Tx) error {
			k, _ := tx.Bucket([]byte(BucketUsageEvents)).Cursor().First()
			cursor = bytes.Clone(k)
			return nil
		}); err != nil {
			t.Fatalf("read raw cursor: %v", err)
		}
		if len(cursor) == 0 {
			t.Fatal("store has no raw rows")
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageClientIndexCursor), cursor)
		}); err != nil {
			t.Fatalf("inject migration cursor: %v", err)
		}
		if err := RunClassificationOnce(context.Background(), db); err != nil {
			t.Fatalf("classify: %v", err)
		}
		if !clientScopeProbeDeclines(t, db, "alice", from, now) {
			t.Fatalf("probe attached a scope while the done marker coexists with a migration cursor")
		}
		before := clientScopeFallbacks(db)
		_, units, err := readClientScopeUnits(store, "alice", from, now, false)
		if got := clientScopeFallbacks(db) - before; got != 1 {
			t.Fatalf("v1 done+cursor: client_scope fallback counted %d times, want exactly once", got)
		}
		clientScopeAssertSameVerdict(t, "v1 done+cursor", units, err, func() ([]CostUnit, error) {
			return clientScopeFullReference(t, db, usage.Filter{ClientName: "alice", From: &from, To: &now}, exactCostTable{replayCosts}, true)
		})
	})

	t.Run("anomalous overflow stays exhaustive", func(t *testing.T) {
		store, db := buildClientScopeStore(t, now, map[string]int{"alice": 10})
		migrateClientIndex(t, db)
		if err := db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
			for i := 0; i <= integrityAnomalyLimit; i++ {
				if err := b.Put([]byte(fmt.Sprintf("not-an-hour-%06d|x|y|z", i)), []byte(`{"requests":1}`)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("inject anomalies: %v", err)
		}
		if err := RunClassificationOnce(context.Background(), db); err != nil {
			t.Fatalf("classify: %v", err)
		}
		stats := UsageIntegrityStats(db)
		if stats.Complete {
			t.Fatalf("expected the classification to be unavailable after overflow")
		}
		before := clientScopeFallbacks(db)
		if _, err := store.ReadQuotaCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &now}, exactCostTable{replayCosts}, true); err != nil {
			t.Fatalf("overflow read: %v", err)
		}
		if got := clientScopeFallbacks(db) - before; got != 0 {
			t.Fatalf("overflow read counted %d client_scope fallbacks, want 0: an unavailable classification is already counted as incomplete", got)
		}
	})

	t.Run("scanning v2 progress", func(t *testing.T) {
		store, db := buildClientScopeStore(t, now, map[string]int{"alice": 10})
		migrateClientIndex(t, db)
		// Rewind the completed v2 anchor to its scanning state: the cursor must
		// equal Through and the v1 done marker must be absent. requireClientIndex
		// then fails, so the scoped enumeration cannot vouch for the index.
		if err := db.Update(func(tx *bolt.Tx) error {
			p, err := loadIndexProgressState(tx, false)
			if err != nil {
				return err
			}
			if p == nil {
				return fmt.Errorf("no v2 progress")
			}
			meta := tx.Bucket([]byte(BucketMeta))
			p.Phase = "scan"
			if err := saveIndexProgress(tx, p); err != nil {
				return err
			}
			if err := meta.Delete([]byte(usageClientIndexDone)); err != nil {
				return err
			}
			return meta.Put([]byte(usageClientIndexCursor), p.Through)
		}); err != nil {
			t.Fatalf("rewind progress: %v", err)
		}
		if err := RunClassificationOnce(context.Background(), db); err != nil {
			t.Fatalf("classify: %v", err)
		}
		before := clientScopeFallbacks(db)
		units, err := store.ReadQuotaCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &now}, exactCostTable{replayCosts}, true)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got := clientScopeFallbacks(db) - before; got != 1 {
			t.Fatalf("scanning progress: client_scope fallback counted %d times, want exactly once", got)
		}
		if len(units) == 0 {
			t.Fatalf("scanning progress: no units")
		}
		filter := usage.Filter{ClientName: "alice", From: &from, To: &now}
		clientScopeAssertSameVerdict(t, "scanning progress", units, err, func() ([]CostUnit, error) {
			return clientScopeFullReference(t, db, filter, exactCostTable{replayCosts}, true)
		})
	})
}

// errCallContext reports context.Canceled from the from-th Err call on, so a
// test can cancel a read at an exact check without racing a goroutine. Calls
// before that point see the parent's result, and Done/Value pass through.
// sites names the function of every Err call: the cancellation test asserts
// which check it canceled at, so an Err call added ahead of the probe turns
// into a clear failure instead of silently moving the cancellation earlier.
type errCallContext struct {
	context.Context
	from  int
	calls int
	sites []string
}

func (c *errCallContext) Err() error {
	c.calls++
	c.sites = append(c.sites, errCallSite())
	if c.from > 0 && c.calls >= c.from {
		return context.Canceled
	}
	return c.Context.Err()
}

// errCallSite names the innermost function that made the ctx.Err() call.
// Frames are expanded, so a call inlined into its caller still reports the
// logical function that contains it; runtime.FuncForPC would report only the
// enclosing physical function.
func errCallSite() string {
	var pcs [16]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		fn := frame.Function
		if fn != "" && !strings.Contains(fn, "errCallContext") && !strings.HasSuffix(fn, "errCallSite") {
			return fn
		}
		if !more {
			return ""
		}
	}
}

// TestClientScopedQuotaReadCancellationIsNotAFallback proves a canceled read
// is not counted as a client_scope fallback: cancellation is not a scope
// decline, and the read returns the cancellation without data, so counting it
// would inflate the metric with reads that never fell back.
//
// The probe is canceled deterministically mid-flight. The read's first Err
// call is the entry precheck (readCostUnitsInScope); the probe's first hourly
// check is the second (integrityFastFrom, integrityReadContext,
// missingUsageAggregateSources and loadIndexProgress do not call Err). The
// test pins that sequence via errCallContext.sites: the second call must come
// from probeClientScopedRead. With the wrapper canceling from the second call
// on, the probe declines, and the following exhaustive walks return
// context.Canceled. An Err call added ahead of the probe would make the test
// cancel before the probe and stop covering the counted path; the site
// assertion fails with that reason instead of letting the case pass.
func TestClientScopedQuotaReadCancellationIsNotAFallback(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	from := now.Add(-24 * time.Hour)
	store, db := buildClientScopeStore(t, now, map[string]int{"alice": 10})
	migrateClientIndex(t, db)
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatalf("classify: %v", err)
	}
	filter := usage.Filter{ClientName: "alice", From: &from, To: &now}
	resolver := exactCostTable{replayCosts}
	// The control is the same read on the same healthy, classified store
	// without cancellation: it must take the scoped enumeration (no legacy
	// scan) and succeed without counting a fallback.
	visits, units, err := readClientScopeUnits(store, "alice", from, now, false)
	if err != nil {
		t.Fatalf("control read: %v", err)
	}
	if len(units) != 10 {
		t.Fatalf("control read returned %d units, want 10", len(units))
	}
	if !visits.scopedIndexRead() {
		t.Fatalf("control read did not use the scoped index block: %+v", visits)
	}
	before := clientScopeFallbacks(db)
	cancelCtx := &errCallContext{Context: context.Background(), from: 2}
	if _, err := store.readCostUnitsInScope(cancelCtx, filter, resolver, true, nil, costReadHooks{windowKeys: true}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read returned %v, want context.Canceled", err)
	}
	// The second Err call must come from the probe; otherwise the cancellation
	// lands before the probe and this case no longer covers the count path
	// after the probe declines.
	if len(cancelCtx.sites) < 2 {
		t.Fatalf("canceled read made %d Err calls, want at least 2; the second Err call must come from probeClientScopedRead, otherwise the cancellation lands before the probe and this case cannot cover the counted path after the probe declines (sites: %v)", len(cancelCtx.sites), cancelCtx.sites)
	}
	if site := cancelCtx.sites[1]; !strings.HasSuffix(site, ".probeClientScopedRead") {
		t.Fatalf("second Err call came from %q, want probeClientScopedRead: the second Err call must come from the probe, otherwise the cancellation lands before the probe and this case cannot cover the counted path after the probe declines (sites: %v)", site, cancelCtx.sites)
	}
	if got := clientScopeFallbacks(db) - before; got != 0 {
		t.Fatalf("canceled read counted %d client_scope fallbacks, want 0: cancellation is not a scope decline", got)
	}
}

// TestClientScopedQuotaReadNotEligible pins the read shapes that never attach
// a client scope: an empty client, a model filter, stable pricing and an
// explicit legacy scan. None of them is a fallback, so no reason is counted,
// and the verdict still equals today's exhaustive read.
func TestClientScopedQuotaReadNotEligible(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	from := now.Add(-24 * time.Hour)
	cases := []struct {
		name     string
		filter   usage.Filter
		resolver usage.RateResolver
		precise  bool
		opts     clientScopeReadOptions
	}{
		{"empty client", usage.Filter{From: &from, To: &now}, exactCostTable{replayCosts}, true, clientScopeReadOptions{}},
		{"model filter", usage.Filter{ClientName: "alice", Model: "executor", From: &from, To: &now}, exactCostTable{replayCosts}, true, clientScopeReadOptions{}},
		{"stable pricing", usage.Filter{ClientName: "alice", From: &from, To: &now}, replayCosts, false, clientScopeReadOptions{}},
		{"force legacy", usage.Filter{ClientName: "alice", From: &from, To: &now}, exactCostTable{replayCosts}, true, clientScopeReadOptions{forceLegacy: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, db := buildClientScopeStore(t, now, map[string]int{"alice": 10, "bob": 10})
			migrateClientIndex(t, db)
			if err := RunClassificationOnce(context.Background(), db); err != nil {
				t.Fatalf("classify: %v", err)
			}
			before := clientScopeFallbacks(db)
			_, units, err := readClientScopeUnitsOpt(store, tc.filter, tc.resolver, tc.precise, tc.opts)
			if got := clientScopeFallbacks(db) - before; got != 0 {
				t.Fatalf("ineligible read counted %d client_scope fallbacks, want 0: the shape never attempts the scope", got)
			}
			filter := tc.filter
			clientScopeAssertSameVerdict(t, tc.name, units, err, func() ([]CostUnit, error) {
				return clientScopeFullReference(t, db, filter, tc.resolver, tc.precise)
			})
		})
	}
}

// TestClientScopedQuotaReadUnknownClient proves a filter that matches no
// client is answered entirely from the index: no raw row, receipt or quota
// cell of any other client is visited, and the verdict (no units) equals the
// exhaustive read.
func TestClientScopedQuotaReadUnknownClient(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	from := now.Add(-24 * time.Hour)
	store, db := buildClientScopeStore(t, now, map[string]int{"alice": 10, "bob": 10})
	migrateClientIndex(t, db)
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatalf("classify: %v", err)
	}
	before := clientScopeFallbacks(db)
	visits, units, err := readClientScopeUnits(store, "nobody", from, now, false)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := clientScopeFallbacks(db) - before; got != 0 {
		t.Fatalf("unknown client counted %d client_scope fallbacks, want 0", got)
	}
	if len(units) != 0 {
		t.Fatalf("unknown client returned %d units", len(units))
	}
	if !visits.scopedIndexRead() {
		t.Fatalf("unknown client read did not use the scoped index block: %+v", visits)
	}
	if visits.raw != 0 || visits.receipt != 0 || visits.quota != 0 {
		t.Fatalf("unknown client read visited another client's objects: %+v", visits)
	}
	filter := usage.Filter{ClientName: "nobody", From: &from, To: &now}
	clientScopeAssertSameVerdict(t, "unknown client", units, err, func() ([]CostUnit, error) {
		return clientScopeFullReference(t, db, filter, exactCostTable{replayCosts}, true)
	})
}

type clientScopeShape struct {
	name       string
	clients    []string
	wantDelta  int
	wantUnits  int // -1: not asserted
	wantScoped bool
	// requireProbeDecline asserts the probe itself declines, not merely that
	// the counter moved: a scoped attempt failing after attachment also counts
	// a fallback through its replay (see clientScopeProbeDeclines). Shapes
	// using it must stay scopeEligible (client set, empty model, windowKeys,
	// nil allow) on a classified store; otherwise the helper fails instead of
	// passing vacuously.
	requireProbeDecline bool
	// v1Only builds the client index as a completed v1-only migration (done
	// marker, no v2 anchors) instead of running the v2 migration.
	v1Only bool
	damage func(t *testing.T, db *bolt.DB, now time.Time)
}

// TestClientScopedQuotaReadEqualsFullShapes pins the damaged shapes named by
// the issue: cross-hour proofs, stale foreign proofs and orphaned entries,
// legacy rows without record_id, numeric ids, owner mismatches, malformed
// rows, quarantine and unresolved evidence. Every read must equal today's
// exhaustive verdict, and the fallback counter must record exactly the reads
// where the scope declined or had to replay.
func TestClientScopedQuotaReadEqualsFullShapes(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	from := now.Add(-24 * time.Hour)
	shapes := []clientScopeShape{
		{name: "healthy", clients: []string{"alice"}, wantDelta: 0, wantUnits: 6, wantScoped: true},
		{
			name: "cross-hour index proof same client", clients: []string{"alice"}, wantDelta: 1, wantUnits: -1,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				key := clientScopePickKey(t, db, "alice")
				e := clientScopeEvent(t, db, key)
				clientScopeRewriteProof(t, db, key, clientScopeIndexKey(e.ClientName, e.Timestamp.Add(2*time.Hour), key))
			},
		},
		{
			name: "foreign raw proof", clients: []string{"alice", "bob"}, wantDelta: 1, wantUnits: -1,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				key := clientScopePickKey(t, db, "alice")
				e := clientScopeEvent(t, db, key)
				clientScopeRewriteProof(t, db, key, clientScopeIndexKey("bob", e.Timestamp, key))
			},
		},
		{
			name: "orphan entry proof names the filter client", clients: []string{"alice", "carol"}, wantDelta: 1, wantUnits: -1,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				key := clientScopePickKey(t, db, "carol")
				e := clientScopeEvent(t, db, key)
				clientScopeDeleteRaw(t, db, key)
				clientScopeRewriteProof(t, db, key, clientScopeIndexKey("alice", e.Timestamp, key))
			},
		},
		{
			name: "legacy row without record_id", clients: []string{"alice"}, wantDelta: 1, wantUnits: -1,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				e := clientScopeSyntheticEvent(now.Add(-3*time.Hour), "alice", "")
				clientScopePutRaw(t, db, []byte(usageKey(e.Timestamp, "legacy-7")), e)
			},
		},
		{
			name: "legacy cross-hour row", clients: []string{"alice"}, wantDelta: 1, wantUnits: -1,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				e := clientScopeSyntheticEvent(now.Add(-5*time.Hour), "alice", "")
				clientScopePutRaw(t, db, []byte(usageKey(now.Add(-3*time.Hour), "legacy-moved-7")), e)
			},
		},
		{
			// The raw key hour is outside the read window and the event hour is
			// inside: the exhaustive evidence scan compares the sibling's hour
			// state against one observed row while the state holds two, falls
			// back to the legacy scan and fails its raw/rollup parity. A scoped
			// read must not return the two-row hour as a billable snapshot.
			name: "cross-hour raw key outside window v1-only", clients: []string{"alice"}, v1Only: true, wantDelta: 1, wantUnits: -1,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				clientScopeMoveRowIntoWindow(t, db, now, false)
			},
		},
		{
			name: "cross-hour raw key outside window v2 proof", clients: []string{"alice"}, wantDelta: 1, wantUnits: -1,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				clientScopeMoveRowIntoWindow(t, db, now, true)
			},
		},
		{
			// The probe enumerates []string{client, ""}: empty-client rows are
			// indexed under the empty prefix and poison every client-filtered
			// read inside the window. A moved empty-client row must decline the
			// scope exactly like a moved filter-client row; checking only the
			// filter client's rows would attach a scope the empty-client walk
			// cannot vouch for.
			name: "empty client cross-hour raw key outside window v2 proof", clients: []string{"alice"}, wantDelta: 1, wantUnits: -1, requireProbeDecline: true,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				clientScopeMoveEmptyClientRowIntoWindow(t, db, now, true, true)
			},
		},
		{
			name: "empty client cross-hour raw key outside window without sibling v2 proof", clients: []string{"alice"}, wantDelta: 1, wantUnits: -1, requireProbeDecline: true,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				clientScopeMoveEmptyClientRowIntoWindow(t, db, now, true, false)
			},
		},
		{
			name: "empty client cross-hour raw key outside window v1-only", clients: []string{"alice"}, v1Only: true, wantDelta: 1, wantUnits: -1, requireProbeDecline: true,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				clientScopeMoveEmptyClientRowIntoWindow(t, db, now, false, true)
			},
		},
		{
			name: "numeric record id", clients: []string{"alice"}, wantDelta: 1, wantUnits: -1,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				e := clientScopeSyntheticEvent(now.Add(-3*time.Hour), "alice", "123")
				clientScopePutRaw(t, db, []byte(usageKey(e.Timestamp, e.RecordID)), e)
			},
		},
		{
			name: "owner disagrees with body", clients: []string{"alice"}, wantDelta: 1, wantUnits: -1,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				key := clientScopePickKey(t, db, "alice")
				e := clientScopeEvent(t, db, key)
				e.ClientName = "mallory"
				clientScopePutRaw(t, db, key, e)
			},
		},
		{
			name: "malformed raw row", clients: []string{"alice"}, wantDelta: 1, wantUnits: -1,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				key := clientScopePickKey(t, db, "alice")
				if err := db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(BucketUsageEvents)).Put(key, []byte(`{"not":"an event"`))
				}); err != nil {
					t.Fatalf("garble raw row: %v", err)
				}
			},
		},
		{
			name: "quarantined row", clients: []string{"alice"}, wantDelta: 1, wantUnits: -1,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				clientScopeQuarantine(t, db, clientScopePickKey(t, db, "alice"))
			},
		},
		{
			name: "unresolved evidence", clients: []string{"alice"}, wantDelta: 1, wantUnits: -1,
			damage: func(t *testing.T, db *bolt.DB, now time.Time) {
				e := clientScopeSyntheticEvent(now.Add(-3*time.Hour), "alice", "feedface-0000-4000-8000-000000000001")
				original, err := json.Marshal(e)
				if err != nil {
					t.Fatalf("marshal unresolved event: %v", err)
				}
				if err := db.Update(func(tx *bolt.Tx) error {
					return putUnresolvedEvidence(tx, unresolvedUsage{Event: e, Original: original})
				}); err != nil {
					t.Fatalf("add unresolved evidence: %v", err)
				}
			},
		},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			store, db := buildClientScopeStore(t, now, map[string]int{"alice": 6, "bob": 6, "carol": 6})
			if shape.v1Only {
				markClientIndexV1Done(t, db)
			} else {
				migrateClientIndex(t, db)
			}
			if shape.damage != nil {
				shape.damage(t, db, now)
			}
			if err := RunClassificationOnce(context.Background(), db); err != nil {
				t.Fatalf("classify: %v", err)
			}
			type outcome struct {
				visits clientScopeVisits
				units  []CostUnit
				err    error
				delta  int
			}
			outcomes := make(map[string]outcome, len(shape.clients))
			for _, client := range shape.clients {
				before := clientScopeFallbacks(db)
				visits, units, err := readClientScopeUnits(store, client, from, now, false)
				outcomes[client] = outcome{visits: visits, units: units, err: err, delta: clientScopeFallbacks(db) - before}
				if shape.requireProbeDecline && !clientScopeProbeDeclines(t, db, client, from, now) {
					t.Fatalf("read of %s attached a client scope; this shape must be declined by the probe", client)
				}
			}
			for _, client := range shape.clients {
				got := outcomes[client]
				if got.delta != shape.wantDelta {
					t.Fatalf("read of %s counted %d client_scope fallbacks, want %d (verdict: %v)", client, got.delta, shape.wantDelta, got.err)
				}
				if shape.wantScoped && !got.visits.scopedIndexRead() {
					t.Fatalf("read of %s did not use the scoped index block: %+v", client, got.visits)
				}
				if shape.wantUnits >= 0 {
					if got.err != nil {
						t.Fatalf("read of %s failed: %v", client, got.err)
					}
					if len(got.units) != shape.wantUnits {
						t.Fatalf("read of %s returned %d units, want %d", client, len(got.units), shape.wantUnits)
					}
				}
				filter := usage.Filter{ClientName: client, From: &from, To: &now}
				clientScopeAssertSameVerdict(t, shape.name+" / "+client, got.units, got.err, func() ([]CostUnit, error) {
					return clientScopeFullReference(t, db, filter, exactCostTable{replayCosts}, true)
				})
			}
		})
	}
}

// TestClientScopedQuotaReadForeignProofUnknownScope pins the Issue #125 P2
// divergence a readable index proof introduces when its client field does not
// name a single client. A duplicate entry under bob's prefix points at a row
// whose event and proof name "unknown" (or the empty client): checkOrphans
// evaluates the proof's original scope with unresolvedScopeInRange, which
// treats an empty or "unknown" owner as relevant to every client, so the
// exhaustive read fails closed for alice. A byte comparison of the proof's
// client prefix against alice's prefix never matches, so the probe used to
// attach a scope and the scoped read reported the row as absent. The probe
// must decline, and the scoped read must reproduce the exhaustive verdict.
func TestClientScopedQuotaReadForeignProofUnknownScope(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	from := now.Add(-24 * time.Hour)
	for _, name := range []string{"unknown", ""} {
		name := name
		t.Run(fmt.Sprintf("proof client %q", name), func(t *testing.T) {
			store, db := buildClientScopeStore(t, now, map[string]int{"alice": 6, "bob": 6, "carol": 6})
			migrateClientIndex(t, db)
			at := now.Add(-2 * time.Hour).Truncate(time.Hour).Add(15 * time.Minute)
			rawKey := []byte(usageKey(at, "foreign-scope-1"))
			clientScopeWriteIndexedRow(t, db, rawKey, clientScopeSyntheticEvent(at, name, "foreign-scope-1"), true)
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketUsageEventsByClient)).Put(clientScopeIndexKey("bob", at, rawKey), rawKey)
			}); err != nil {
				t.Fatalf("duplicate foreign index entry: %v", err)
			}
			if err := RunClassificationOnce(context.Background(), db); err != nil {
				t.Fatalf("classify: %v", err)
			}
			if !clientScopeProbeDeclines(t, db, "alice", from, now) {
				t.Fatalf("probe attached a scope for alice although the foreign proof names client %q, which checkOrphans treats as relevant to every client", name)
			}
			before := clientScopeFallbacks(db)
			_, units, err := readClientScopeUnits(store, "alice", from, now, false)
			if !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("scoped read of alice returned %v (%d units), want ErrUsageHistoryIncomplete as the exhaustive read does", err, len(units))
			}
			if got := clientScopeFallbacks(db) - before; got != 1 {
				t.Fatalf("client_scope fallback counted %d times, want exactly once", got)
			}
			filter := usage.Filter{ClientName: "alice", From: &from, To: &now}
			clientScopeAssertSameVerdict(t, "alice", units, err, func() ([]CostUnit, error) {
				return clientScopeFullReference(t, db, filter, exactCostTable{replayCosts}, true)
			})
		})
	}
}

// TestForeignProofsBoundedDedup pins the bounded collection behind the probe:
// repeated proofs cost one map entry, and the set turns into an unconditional
// decline once distinct proofs exceed the limit instead of growing with the
// damage.
func TestForeignProofsBoundedDedup(t *testing.T) {
	var f foreignProofSet
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	scope := unresolvedScope{ClientName: "bob", Timestamp: now}
	for i := 0; i < 4*integrityForeignProofLimit; i++ {
		f.addScope(scope)
		f.addPrefix([]byte(clientIndexPrefix("bob")))
	}
	if f.closed {
		t.Fatalf("duplicate proofs closed the set")
	}
	if len(f.scopes) != 1 || len(f.prefixes) != 1 {
		t.Fatalf("duplicate proofs stored %d scopes and %d prefixes, want 1 and 1", len(f.scopes), len(f.prefixes))
	}
	if f.relevant("alice", now.Add(-time.Hour), now.Add(time.Hour)) {
		t.Fatalf("a bob-only scope is relevant to alice")
	}
	for i := 0; i < integrityForeignProofLimit-2; i++ {
		f.addScope(unresolvedScope{ClientName: fmt.Sprintf("client-%03d", i), Timestamp: now})
	}
	if f.closed {
		t.Fatalf("exactly %d distinct proofs closed the set", integrityForeignProofLimit)
	}
	f.addScope(unresolvedScope{ClientName: "one-more", Timestamp: now})
	if !f.closed {
		t.Fatalf("%d distinct proofs did not close the set", integrityForeignProofLimit+1)
	}
	if len(f.scopes) != 0 || len(f.prefixes) != 0 {
		t.Fatalf("closed set kept %d scopes and %d prefixes, want an empty set", len(f.scopes), len(f.prefixes))
	}
	if !f.relevant("alice", now.Add(-time.Hour), now.Add(time.Hour)) {
		t.Fatalf("a closed set must decline every client")
	}
}

// TestClientScopedQuotaReadForeignProofOverflow proves the overflow is visible
// to the probe: more distinct foreign proofs than the limit must decline every
// client and fall back to the exhaustive verdict.
func TestClientScopedQuotaReadForeignProofOverflow(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	from := now.Add(-24 * time.Hour)
	store, db := buildClientScopeStore(t, now, map[string]int{"alice": 6, "bob": 6, "carol": 6})
	migrateClientIndex(t, db)
	base := now.Add(-2 * time.Hour).Truncate(time.Hour)
	rawKeys := make([][]byte, 0, integrityForeignProofLimit+1)
	for i := 0; i <= integrityForeignProofLimit; i++ {
		at := base.Add(time.Duration(i+1) * time.Minute)
		client := fmt.Sprintf("overflow-%03d", i)
		rawKey := []byte(usageKey(at, fmt.Sprintf("overflow-%03d", i)))
		clientScopeWriteIndexedRow(t, db, rawKey, clientScopeSyntheticEvent(at, client, fmt.Sprintf("overflow-%03d", i)), true)
		rawKeys = append(rawKeys, rawKey)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		index := tx.Bucket([]byte(BucketUsageEventsByClient))
		for i, rawKey := range rawKeys {
			at := base.Add(time.Duration(i+1) * time.Minute)
			if err := index.Put(clientScopeIndexKey("bob", at, rawKey), rawKey); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("duplicate foreign index entries: %v", err)
	}
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if !clientScopeProbeDeclines(t, db, "alice", from, now) {
		t.Fatalf("probe attached a scope for alice although %d distinct foreign proofs exceeded the limit", integrityForeignProofLimit+1)
	}
	before := clientScopeFallbacks(db)
	_, units, err := readClientScopeUnits(store, "alice", from, now, false)
	if got := clientScopeFallbacks(db) - before; got != 1 {
		t.Fatalf("client_scope fallback counted %d times, want exactly once", got)
	}
	filter := usage.Filter{ClientName: "alice", From: &from, To: &now}
	clientScopeAssertSameVerdict(t, "alice", units, err, func() ([]CostUnit, error) {
		return clientScopeFullReference(t, db, filter, exactCostTable{replayCosts}, true)
	})
}

// TestClassifyIndexEntryDecodesOncePerEntry pins the P2 reuse contract: the
// foreign-scope check consumes the proof the entry classifier already read, so
// a healthy snapshot decodes one proof per window-exempt entry instead of two.
func TestClassifyIndexEntryDecodesOncePerEntry(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	_, db := buildClientScopeStore(t, now, map[string]int{"alice": 6, "bob": 6, "carol": 6})
	migrateClientIndex(t, db)
	entries := 0
	if err := db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEventsByClient)).ForEach(func(_, _ []byte) error {
			entries++
			return nil
		})
	}); err != nil {
		t.Fatalf("count index entries: %v", err)
	}
	before := integrityIndexProofDecodes.Load()
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if delta := integrityIndexProofDecodes.Load() - before; delta != int64(entries) {
		t.Fatalf("classification decoded %d index proofs for %d window-exempt entries; the foreign-scope check must reuse the entry classifier's proof", delta, entries)
	}
}

// TestClientScopedQuotaReadProbeHourCostScales records the probe's window-hour
// enumeration, which the object-visit counters cannot see. The probe reads one
// hour state and seeks one index range per client prefix (the filter client and
// the empty client) per window hour, so its cost grows with the window hours.
func TestClientScopedQuotaReadProbeHourCostScales(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for _, hours := range []int{24, 30 * 24} {
		from := now.Add(-time.Duration(hours) * time.Hour)
		store, db := buildClientScopeStore(t, now, map[string]int{"alice": 6, "bob": 6})
		migrateClientIndex(t, db)
		if err := RunClassificationOnce(context.Background(), db); err != nil {
			t.Fatalf("hours=%d classify: %v", hours, err)
		}
		visits, units, err := readClientScopeUnits(store, "alice", from, now, false)
		if err != nil {
			t.Fatalf("hours=%d read: %v", hours, err)
		}
		if len(units) == 0 || !visits.scopedIndexRead() {
			t.Fatalf("hours=%d: read did not use the scoped index block: %+v", hours, visits)
		}
		// The read window includes both hour bounds, so an N-hour span covers
		// N+1 window hours.
		if want := 2 * (hours + 1); visits.hourStates != want || visits.seeks != want {
			t.Fatalf("hours=%d: probe read %d hour states and %d ranges, want %d each (two client prefixes per window hour)", hours, visits.hourStates, visits.seeks, want)
		}
		if visits.emptyDecoded != 0 {
			t.Fatalf("hours=%d: probe decoded %d empty-client rows, want 0", hours, visits.emptyDecoded)
		}
	}
}

// TestClientScopedQuotaReadProbeCountsEmptyClientRows pins the empty-client
// decode counter: the probe enumerates the empty prefix too, so its rows are
// decoded even though the filter client is alice. The empty-owner quota cell
// inside the window fails every client-filtered read, which is the verdict the
// scoped attempt must hand back through its replay.
func TestClientScopedQuotaReadProbeCountsEmptyClientRows(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	from := now.Add(-2 * time.Hour)
	store, db := buildClientScopeStore(t, now, map[string]int{"alice": 6, "bob": 6})
	migrateClientIndex(t, db)
	at := now.Add(-time.Hour)
	clientScopeWriteIndexedRow(t, db, []byte(usageKey(at, "empty-probe-1")), clientScopeSyntheticEvent(at, "", "empty-probe-1"), true)
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatalf("classify: %v", err)
	}
	visits, _, err := readClientScopeUnits(store, "alice", from, now, false)
	if !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("read returned %v, want ErrUsageHistoryIncomplete for the empty-owner row in the window", err)
	}
	if visits.emptyDecoded != 1 {
		t.Fatalf("probe decoded %d empty-client rows, want 1", visits.emptyDecoded)
	}
	if want := 3 * 2; visits.hourStates != want || visits.seeks != want {
		t.Fatalf("probe read %d hour states and %d ranges, want %d each for three window hours", visits.hourStates, visits.seeks, want)
	}
}

// TestClientScopedQuotaReadCalibrationScales models the hourly calibration:
// calibrateAll reads the month window once per client through
// ReadQuotaCostUnits. The total visited raw, receipt and quota objects must
// grow with the window's total rows, not with clients times rows.
func TestClientScopedQuotaReadCalibrationScales(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	from := now.AddDate(0, -1, 0)
	var totals []int
	for _, clients := range []int{2, 8} {
		rows := make(map[string]int, clients)
		names := make([]string, 0, clients)
		for c := 0; c < clients; c++ {
			name := fmt.Sprintf("client-%02d", c)
			rows[name] = 30
			names = append(names, name)
		}
		store, db := buildClientScopeStore(t, now, rows)
		migrateClientIndex(t, db)
		if err := RunClassificationOnce(context.Background(), db); err != nil {
			t.Fatalf("classify: %v", err)
		}
		total := 0
		for _, name := range names {
			visits, _, err := readClientScopeUnits(store, name, from, now, false)
			if err != nil {
				t.Fatalf("clients=%d %s: %v", clients, name, err)
			}
			total += visits.raw + visits.receipt + visits.quota + visits.evidence
		}
		totalRows := clients * 30
		if total > 5*totalRows {
			t.Fatalf("clients=%d: the whole calibration visited %d objects for %d rows; the work must stay proportional to the window's rows, not clients times rows", clients, total, totalRows)
		}
		totals = append(totals, total)
		t.Logf("clients=%d rows=%d visited=%d", clients, totalRows, total)
	}
	if totals[1] > 6*max(totals[0], 1) {
		t.Fatalf("calibration work grew from %d to %d for four times the rows: the per-client read is still scanning the window", totals[0], totals[1])
	}
}

// TestClientScopedProbeRequiresClassifiedIndex pins the probe's fast-usability
// guard. checkOrphans abandons the fast index walk when the classification did
// not classify the index or was built under a different v2 progress than the
// read's. The probe trusts rawUnindexed and foreignProofs, which only that
// classification fills, so it must decline in the same situations and let the
// exhaustive read decide instead of attaching a scope it cannot prove.
func TestClientScopedProbeRequiresClassifiedIndex(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	from := now.Add(-24 * time.Hour)
	_, db := buildClientScopeStore(t, now, map[string]int{"alice": 6, "bob": 6})
	migrateClientIndex(t, db)
	start, end := from.Truncate(time.Hour), now.Truncate(time.Hour).Add(time.Hour)
	probe := func(t *testing.T, class func(progress *indexProgress) *integrityClassification) *clientScopedRead {
		t.Helper()
		var scoped *clientScopedRead
		if err := db.View(func(tx *bolt.Tx) error {
			progress, err := loadIndexProgress(tx)
			if err != nil {
				return err
			}
			if progress == nil || progress.Phase != "complete" {
				return fmt.Errorf("migrated index has progress %+v", progress)
			}
			fast := newIntegrityFast(class(progress), start, end)
			if fast.empty {
				return fmt.Errorf("constructed fast window is empty")
			}
			scoped = probeClientScopedRead(context.Background(), tx, "alice", start, end, progress, fast)
			return nil
		}); err != nil {
			t.Fatalf("probe: %v", err)
		}
		return scoped
	}
	t.Run("index not classified", func(t *testing.T) {
		scoped := probe(t, func(*indexProgress) *integrityClassification {
			return &integrityClassification{}
		})
		if scoped != nil {
			t.Fatalf("probe attached a scope although the classification did not classify the index; checkOrphans abandons the fast walk in that case")
		}
	})
	t.Run("stale progress", func(t *testing.T) {
		scoped := probe(t, func(progress *indexProgress) *integrityClassification {
			stale := *progress
			stale.Generation += "-stale"
			return &integrityClassification{indexClassified: true, indexProgress: &stale}
		})
		if scoped != nil {
			t.Fatalf("probe attached a scope although the classification was built under a different v2 progress than the read's")
		}
	})
	t.Run("matching progress", func(t *testing.T) {
		scoped := probe(t, func(progress *indexProgress) *integrityClassification {
			return &integrityClassification{indexClassified: true, indexProgress: progress}
		})
		if scoped == nil {
			t.Fatalf("probe declined a classification built under the read's own v2 progress")
		}
	})
}
