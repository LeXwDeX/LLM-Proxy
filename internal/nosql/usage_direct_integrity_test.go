package nosql

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// directIntegrityVerdict runs the direct quota evidence check in one mode and
// returns the reported failure text, or "ok".
func directIntegrityVerdict(t *testing.T, db *bolt.DB, filter usage.Filter, from, to time.Time, fast bool) string {
	t.Helper()
	saved := integrityForceFast.Load()
	integrityForceFast.Store(fast)
	defer integrityForceFast.Store(saved)
	var got error
	if err := db.View(func(tx *bolt.Tx) error {
		got = checkDirectQuotaEvidenceContext(context.Background(), tx, filter, from, to, nil, &directIntegrityHooks{windowKeys: true})
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
	if got == nil {
		return "ok"
	}
	return got.Error()
}

// TestDirectQuotaFirstAggregationErrorIsDeterministic pins the first reported
// usage-aggregation failure to the lexicographically smallest aggregation key.
// Two cells in one scope fail in different ways; walking the Go map in its
// random order picks either message, so the exhaustive and windowed reads (and
// repeated reads of the same database) can disagree although every failure is
// real.
func TestDirectQuotaFirstAggregationErrorIsDeterministic(t *testing.T) {
	const want = "usage history is incomplete for exact pricing: invalid usage aggregation"
	base := fixedPlainEvent()
	base.Components = nil
	base.InputTokens = 1_000_000
	advisor := base
	advisor.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa01"
	advisor.Model = "advisor"
	executor := base
	executor.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa02"
	executor.Model = "executor"
	advisorKey := aggKey(advisor.Timestamp, advisor.EndpointType, advisor.ClientName, advisor.Model, advisor.Target)
	executorKey := aggKey(executor.Timestamp, executor.EndpointType, executor.ClientName, executor.Model, executor.Target)
	if bytes.Compare(advisorKey, executorKey) >= 0 {
		t.Fatalf("test keys not ordered: %q >= %q", advisorKey, executorKey)
	}
	from := advisor.Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour - time.Nanosecond)
	filter := usage.Filter{ClientName: advisor.ClientName, From: &from, To: &to}
	for i := 0; i < 50; i++ {
		db := testDB(t)
		store := clockedStore(db)
		events := []usage.Event{advisor, executor}
		if i%2 == 1 {
			events[0], events[1] = events[1], events[0]
		}
		for _, event := range events {
			if err := store.Record(event); err != nil {
				t.Fatalf("iteration %d record: %v", i, err)
			}
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			agg := tx.Bucket([]byte(BucketUsageAggHourly))
			if err := agg.Put(advisorKey, []byte(`{"input_tokens":"bad"}`)); err != nil {
				return err
			}
			return agg.Put(executorKey, []byte(`{"requests":0}`))
		}); err != nil {
			t.Fatalf("iteration %d damage: %v", i, err)
		}
		if got := directIntegrityVerdict(t, db, filter, from, to, false); got != want {
			t.Fatalf("iteration %d: exhaustive first error = %q, want %q", i, got, want)
		}
		if got := directIntegrityVerdict(t, db, filter, from, to, true); got != want {
			t.Fatalf("iteration %d: windowed first error = %q, want %q", i, got, want)
		}
	}
}

func TestDirectQuotaReceiptEvidenceAfterPrune(t *testing.T) {
	for _, damage := range []string{"missing", "null"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			event.Target = ""
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if n, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
				t.Fatalf("prune: %d %v", n, err)
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
			if _, err := store.ReadCostUnits(context.Background(), filter, usage.CostTable{}, false); err != nil {
				t.Fatalf("zero-price retained usage: %v", err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
				key := quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
				if damage == "missing" {
					return b.Delete(key)
				}
				return b.Put(key, []byte("null"))
			}); err != nil {
				t.Fatal(err)
			}
			if units, err := store.ReadCostUnits(context.Background(), filter, usage.CostTable{}, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("lost retained cell: %v %v", units, err)
			}
			filter.ClientName = "unrelated"
			if _, err := store.ReadCostUnits(context.Background(), filter, usage.CostTable{}, false); err != nil {
				t.Fatalf("unrelated client: %v", err)
			}
		})
	}
}

func TestDirectQuotaUnrelatedCorruptRawDoesNotBlockClient(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	alice := fixedPlainEvent()
	if err := store.Record(alice); err != nil {
		t.Fatal(err)
	}
	bob := alice
	bob.ClientName = "bob"
	bob.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa9"
	if err := store.Record(bob); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(bob.Timestamp, bob.RecordID)), []byte(`{"client_name":"bob","timestamp":"bad"`))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := alice.Timestamp.Truncate(time.Hour), alice.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: "alice", From: &from, To: &to}
	units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
	if err != nil || len(units) != 1 || units[0].CostUSD != 2 {
		t.Fatalf("unrelated damaged row affected alice: %+v %v", units, err)
	}
}

func TestDirectQuotaRejectsAlteredProvenCell(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		key := quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)
		return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put(key, []byte(`{"record_count":1}`))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	if units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false); len(units) != 0 || !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("altered proven cell: %v %v", units, err)
	}
}

func TestDirectQuotaProofIgnoresAttributableCorruptOtherClient(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	bob := event
	bob.ClientName = "bob"
	bob.RecordID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if err := store.Record(bob); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		value := fmt.Sprintf(`{"client_name":"bob","timestamp":%q,"input_tokens":"bad"}`, event.Timestamp.Format(time.RFC3339Nano))
		return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(bob.Timestamp, bob.RecordID)), []byte(value))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
	filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
	units, err := store.ReadCostUnits(context.Background(), filter, replayCosts, false)
	if err != nil || len(units) != 1 || units[0].CostUSD != 2 {
		t.Fatalf("other client's corrupt row affected receipt proof: %+v %v", units, err)
	}
}
