package nosql

// Issue #211: stable/hourly-priced (needsExact=false) client-filtered quota
// reads must narrow their integrity walks to the filter client, exactly like
// the exact-priced path, while producing the same result and keeping every
// fail-closed boundary.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

var stableReplayCosts = usage.CostTable{
	"executor": {InputPer1MTokens: 2},
	"advisor":  {InputPer1MTokens: 5},
}

func readStableScopeUnits(t *testing.T, db *bolt.DB, filter usage.Filter, disableScope bool) (clientScopeVisits, []CostUnit, error) {
	t.Helper()
	var visits clientScopeVisits
	savedHour, savedSeek, savedDecoded := clientScopeProbeHourState, clientScopeProbeRangeSeek, clientScopeProbeDecoded
	clientScopeProbeHourState = func() { visits.hourStates++ }
	clientScopeProbeRangeSeek = func() { visits.seeks++ }
	clientScopeProbeDecoded = func() { visits.emptyDecoded++ }
	defer func() {
		clientScopeProbeHourState, clientScopeProbeRangeSeek, clientScopeProbeDecoded = savedHour, savedSeek, savedDecoded
	}()
	ctx := context.Background()
	if disableScope {
		ctx = context.WithValue(ctx, noClientScopeKey{}, true)
	}
	result, err := NewUsageStore(db).readCostUnitsInScope(ctx, filter, stableReplayCosts, true, nil, costReadHooks{
		windowKeys: true,
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

func windowRawCount(t *testing.T, db *bolt.DB, client string, from, to time.Time) int {
	t.Helper()
	count := 0
	if err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		if b == nil {
			return nil
		}
		lo := from.UTC().Truncate(time.Hour).Format(hourLayout)
		hi := to.UTC().Truncate(time.Hour).Format(hourLayout) + "}"
		c := b.Cursor()
		for k, v := c.Seek([]byte(lo)); k != nil && string(k) <= hi; k, v = c.Next() {
			var event usage.Event
			if json.Unmarshal(v, &event) != nil {
				continue
			}
			if event.ClientName == client {
				count++
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("count window raw rows: %v", err)
	}
	return count
}

// countWindowRawRows is defined above.

func TestStablePriceClientScopedReadNarrowsEvidence(t *testing.T) {
	productionIntegrityMode(t)
	if testing.Short() {
		t.Skip("integrity scan measurement")
	}
	now := time.Now().UTC()
	store, db := buildClientScopeStore(t, now, map[string]int{"alice": 30, "bob": 30, "carol": 30})
	_ = store
	migrateClientIndex(t, db)
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatalf("classify: %v", err)
	}
	from := now.Add(-24 * time.Hour)
	filter := usage.Filter{ClientName: "alice", From: &from, To: &now}

	wantClientRows := windowRawCount(t, db, "alice", from, now)
	before := clientScopeFallbacks(db)
	scopedVisits, scopedUnits, err := readStableScopeUnits(t, db, filter, false)
	if err != nil {
		t.Fatalf("scoped stable read: %v", err)
	}
	if scopedVisits.raw != wantClientRows {
		t.Fatalf("scoped stable read visited %d raw rows, want exactly alice's %d (visits %+v)", scopedVisits.raw, wantClientRows, scopedVisits)
	}
	if got := clientScopeFallbacks(db) - before; got != 0 {
		t.Fatalf("scoped read recorded %d client_scope fallbacks, want 0", got)
	}

	fullVisits, fullUnits, err := readStableScopeUnits(t, db, filter, true)
	if err != nil {
		t.Fatalf("exhaustive stable read: %v", err)
	}
	if fullVisits.raw <= scopedVisits.raw {
		t.Fatalf("exhaustive read visited %d raw rows, scoped %d: the fixture does not prove narrowing", fullVisits.raw, scopedVisits.raw)
	}
	scopedJSON, err := jsonUnits(scopedUnits)
	if err != nil {
		t.Fatal(err)
	}
	fullJSON, err := jsonUnits(fullUnits)
	if err != nil {
		t.Fatal(err)
	}
	if scopedJSON != fullJSON {
		t.Fatalf("scoped stable units differ from exhaustive:\n got %s\nwant %s", scopedJSON, fullJSON)
	}

	// The cold/exhaustive reference (no classification at all) must agree too.
	ForgetUsageIntegrityClassification(db)
	_, coldUnits, err := readStableScopeUnits(t, db, filter, false)
	if err != nil {
		t.Fatalf("cold stable read: %v", err)
	}
	coldJSON, err := jsonUnits(coldUnits)
	if err != nil {
		t.Fatal(err)
	}
	if coldJSON != fullJSON {
		t.Fatalf("cold stable units differ from exhaustive:\n got %s\nwant %s", coldJSON, fullJSON)
	}
}

func TestStablePriceClientScopedReadFallsBackAndKeepsVerdict(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Now().UTC()
	_, db := buildClientScopeStore(t, now, map[string]int{"alice": 12, "bob": 12})
	migrateClientIndex(t, db)
	// A decodable anomalous raw row inside the window makes the probe decline:
	// the exhaustive evidence pass can judge it, the scoped index cannot.
	odd := clientScopeSyntheticEvent(now.Add(-3*time.Hour), "alice", "not-a-uuid")
	clientScopePutRaw(t, db, []byte(usageKey(odd.Timestamp, odd.RecordID)), odd)
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatalf("classify: %v", err)
	}
	from := now.Add(-24 * time.Hour)
	filter := usage.Filter{ClientName: "alice", From: &from, To: &now}
	before := clientScopeFallbacks(db)
	scopedVisits, scopedUnits, scopedErr := readStableScopeUnits(t, db, filter, false)
	if got := clientScopeFallbacks(db) - before; got != 1 {
		t.Fatalf("declined probe counted %d client_scope fallbacks, want 1 (visits %+v)", got, scopedVisits)
	}
	fullVisits, fullUnits, fullErr := readStableScopeUnits(t, db, filter, true)
	clientScopeAssertSameVerdict(t, "stable declined probe", scopedUnits, scopedErr, func() ([]CostUnit, error) {
		return fullUnits, fullErr
	})
	wantAll := windowRawCount(t, db, "alice", from, now) + windowRawCount(t, db, "bob", from, now)
	if scopedVisits.raw != wantAll || fullVisits.raw != wantAll {
		t.Fatalf("declined probe must restore the full walk: scoped=%d full=%d want=%d", scopedVisits.raw, fullVisits.raw, wantAll)
	}
}

func TestStablePriceMalformedForeignQuotaKeyStillFailsClosed(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Now().UTC()
	_, db := buildClientScopeStore(t, now, map[string]int{"alice": 6, "bob": 6})
	migrateClientIndex(t, db)
	// A malformed owner cannot be attributed to another client, so both the
	// scoped and exhaustive walks must reject it.
	hour := now.Add(-2 * time.Hour).UTC().Truncate(time.Hour).Format(hourLayout)
	badKey := []byte(hour + "|bob%zz|anthropic|executor")
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put(badKey, []byte(`{"requests":1}`))
	}); err != nil {
		t.Fatalf("put malformed quota key: %v", err)
	}
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatalf("classify: %v", err)
	}
	from := now.Add(-24 * time.Hour)
	filter := usage.Filter{ClientName: "alice", From: &from, To: &now}
	_, scopedUnits, scopedErr := readStableScopeUnits(t, db, filter, false)
	_, fullUnits, fullErr := readStableScopeUnits(t, db, filter, true)
	clientScopeAssertSameVerdict(t, "stable malformed foreign quota key", scopedUnits, scopedErr, func() ([]CostUnit, error) {
		return fullUnits, fullErr
	})
	if scopedErr == nil {
		t.Fatal("malformed foreign-owner quota key did not fail closed")
	}
}

func TestStablePriceIncompleteClassificationKeepsFullWalk(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Now().UTC()
	_, db := buildClientScopeStore(t, now, map[string]int{"alice": 10, "bob": 10})
	migrateClientIndex(t, db)
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatalf("classify: %v", err)
	}
	from := now.Add(-24 * time.Hour)
	filter := usage.Filter{ClientName: "alice", From: &from, To: &now}
	_, wantUnits, wantErr := readStableScopeUnits(t, db, filter, true)

	ForgetUsageIntegrityClassification(db)
	before := UsageIntegrityStats(db)
	visits, units, err := readStableScopeUnits(t, db, filter, false)
	after := UsageIntegrityStats(db)
	if got := after.FallbackReads[integrityFallbackIncomplete] - before.FallbackReads[integrityFallbackIncomplete]; got == 0 {
		t.Fatalf("read without classification did not record an incomplete fallback (visits %+v)", visits)
	}
	if want := windowRawCount(t, db, "alice", from, now) + windowRawCount(t, db, "bob", from, now); visits.raw < want {
		t.Fatalf("cold read visited %d raw rows, want at least every client's %d", visits.raw, want)
	}
	clientScopeAssertSameVerdict(t, "stable without classification", units, err, func() ([]CostUnit, error) {
		return wantUnits, wantErr
	})
}

func TestStablePriceWindowBoundaryEventsStayInVerdict(t *testing.T) {
	productionIntegrityMode(t)
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
	_, db := buildClientScopeStore(t, now, map[string]int{"alice": 8, "bob": 8})
	migrateClientIndex(t, db)
	from := now.Add(-6 * time.Hour)
	boundary := []usage.Event{
		clientScopeSyntheticEvent(from, "alice", "aaaaaaaa-aaaa-4aaa-8aaa-000000000101"),
		clientScopeSyntheticEvent(from.Add(-time.Nanosecond), "alice", "aaaaaaaa-aaaa-4aaa-8aaa-000000000102"),
		clientScopeSyntheticEvent(now, "alice", "aaaaaaaa-aaaa-4aaa-8aaa-000000000103"),
		clientScopeSyntheticEvent(now.Add(time.Nanosecond), "alice", "aaaaaaaa-aaaa-4aaa-8aaa-000000000104"),
		clientScopeSyntheticEvent(from, "", "aaaaaaaa-aaaa-4aaa-8aaa-000000000105"),
	}
	for _, event := range boundary {
		clientScopePutRaw(t, db, []byte(usageKey(event.Timestamp, event.RecordID)), event)
	}
	if err := RunClassificationOnce(context.Background(), db); err != nil {
		t.Fatalf("classify: %v", err)
	}
	for _, client := range []string{"alice", "bob"} {
		filter := usage.Filter{ClientName: client, From: &from, To: &now}
		scopedUnits, scopedErr := func() ([]CostUnit, error) { _, u, e := readStableScopeUnits(t, db, filter, false); return u, e }()
		fullUnits, fullErr := func() ([]CostUnit, error) { _, u, e := readStableScopeUnits(t, db, filter, true); return u, e }()
		clientScopeAssertSameVerdict(t, fmt.Sprintf("stable boundary %s", client), scopedUnits, scopedErr, func() ([]CostUnit, error) {
			return fullUnits, fullErr
		})
	}
}