package nosql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func TestDirectIntegrityScansOncePerCostQuery(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	pruned := first
	pruned.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa7"
	pruned.Timestamp = first.Timestamp.Add(-2 * time.Hour)
	if err := store.Record(pruned); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(pruned.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("prune fixture: n=%d err=%v", n, err)
	}
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8"
	second.Timestamp = first.Timestamp.Add(time.Second)
	for _, event := range []usage.Event{first, second} {
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	migrateClientIndex(t, db)
	from := first.Timestamp.Truncate(time.Hour)
	baselineRaw := 0
	for _, hours := range []int{1, 24, 721} {
		t.Run(fmt.Sprintf("%dh", hours), func(t *testing.T) {
			to := from.Add(time.Duration(hours)*time.Hour - time.Nanosecond)
			calls, raw, receipts, quota, indexed, fallback := 0, 0, 0, 0, 0, 0
			var checkedFrom, checkedTo time.Time
			hooks := costReadHooks{direct: &directIntegrityHooks{
				call: func(from, to time.Time) { calls++; checkedFrom, checkedTo = from, to }, raw: func() { raw++ },
				receipt: func() { receipts++ }, quota: func() { quota++ },
			}, scan: func(index bool) {
				if index {
					indexed++
				} else {
					fallback++
				}
			}}
			result, err := store.readCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, From: &from, To: &to}, exactCostTable{replayCosts}, true, nil, hooks)
			if err != nil || len(result.units) != 2 || costUnitTotal(result.units) != 4 || indexed != 1 || fallback != 0 {
				t.Fatalf("fixture/result: units=%+v indexed=%d fallback=%d err=%v", result.units, indexed, fallback, err)
			}
			t.Logf("direct calls=%d raw scan visits=%d receipt scan visits=%d quota scan visits=%d", calls, raw, receipts, quota)
			if hours == 1 {
				baselineRaw = raw
				if raw == 0 || receipts == 0 || quota == 0 {
					t.Errorf("instrumentation missed evidence: raw=%d receipt=%d quota=%d", raw, receipts, quota)
				}
			}
			if calls != 1 {
				t.Errorf("direct integrity calls=%d, want one per query", calls)
			} else if !checkedFrom.Equal(from) || !checkedTo.Truncate(time.Hour).Equal(to.Truncate(time.Hour)) {
				t.Errorf("direct integrity range=%v..%v, want whole query %v..%v", checkedFrom, checkedTo, from, to)
			}
			if raw > baselineRaw {
				t.Errorf("raw scan visits=%d, one-hour baseline=%d", raw, baselineRaw)
			}
		})
	}
}

func TestDirectIntegrityHooksCountNestedProofScans(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(first.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("prune fixture: n=%d err=%v", n, err)
	}
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8"
	second.Timestamp = first.Timestamp.Add(time.Second)
	if err := store.Record(second); err != nil {
		t.Fatal(err)
	}
	from, to := first.Timestamp.Truncate(time.Hour), first.Timestamp.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
	calls, raw, receipts, quota := 0, 0, 0, 0
	hooks := costReadHooks{direct: &directIntegrityHooks{
		call: func(time.Time, time.Time) { calls++ }, raw: func() { raw++ },
		receipt: func() { receipts++ }, quota: func() { quota++ },
	}}
	result, err := store.readCostUnits(context.Background(), usage.Filter{ClientName: first.ClientName, From: &from, To: &to}, replayCosts, false, nil, hooks)
	if err != nil || costUnitTotal(result.units) != 4 {
		t.Fatalf("fixture/result: units=%+v err=%v", result.units, err)
	}
	if calls != 1 || raw < 2 || receipts < 2 || quota != 1 {
		t.Fatalf("missing nested scans: calls=%d raw=%d receipt=%d quota=%d", calls, raw, receipts, quota)
	}
}
