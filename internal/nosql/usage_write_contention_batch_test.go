package nosql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestBatchPrewriteProofScanReuse(t *testing.T) {
	type counts struct{ raw, receipt int }
	var baseline counts
	for _, n := range []int{1, 4, 16} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			db := testDB(t)
			contentionHistory(t, db, 48, true)
			store := clockedStore(db)
			if err := store.EnsureUsageReceipts(); err != nil {
				t.Fatal(err)
			}
			if count, err := store.PruneEventsBefore(fixedPlainEvent().Timestamp.Add(-23*time.Hour), 1); err != nil || count != 2 {
				t.Fatalf("prune fixture: %d, %v", count, err)
			}
			first := fixedPlainEvent()
			first.ClientName = "new-client"
			if err := store.RecordBatch([]usage.Event{first}); err != nil {
				t.Fatal(err)
			}
			batch := make([]usage.Event, n)
			for i := range batch {
				batch[i] = first
				batch[i].RecordID = fmt.Sprintf("20000000-0000-4000-8000-%012x", i+1)
				batch[i].Timestamp = first.Timestamp.Add(time.Duration(i+1) * time.Millisecond)
				batch[i].InputTokens += int64(i)
			}
			var got counts
			ctx := context.WithValue(context.Background(), proofScanHooksKey{}, &directIntegrityHooks{
				raw: func() { got.raw++ }, receipt: func() { got.receipt++ },
			})
			if err := store.recordBatchContext(ctx, batch); err != nil {
				t.Fatal(err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				for _, event := range batch {
					var stored usage.Event
					data := tx.Bucket([]byte(BucketUsageEvents)).Get([]byte(usageKey(event.Timestamp, event.RecordID)))
					if err := json.Unmarshal(data, &stored); err != nil || stored.InputTokens != event.InputTokens {
						return fmt.Errorf("identity %s lost or changed: %v", event.RecordID, err)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if n == 1 {
				baseline = got
				if got.raw == 0 || got.receipt == 0 {
					t.Fatalf("proof instrumentation missed scans: %+v", got)
				}
			} else if got != baseline {
				t.Fatalf("prewrite proof scans for %d bundles=%+v, want one bundle=%+v", n, got, baseline)
			}
		})
	}
}

func TestBatchProofMemoScopesAndAdmission(t *testing.T) {
	db := testDB(t)
	contentionHistory(t, db, 4, true)
	store := clockedStore(db)
	if err := store.EnsureUsageReceipts(); err != nil {
		t.Fatal(err)
	}
	base := fixedPlainEvent()
	if err := store.RecordBatch([]usage.Event{base}); err != nil {
		t.Fatal(err)
	}
	type scenario struct {
		name  string
		event usage.Event
		want  bool
	}
	var cases []scenario
	add := func(name string, change func(*usage.Event), want bool) {
		e := base
		change(&e)
		cases = append(cases, scenario{name, e, want})
	}
	add("continuation", func(*usage.Event) {}, true)
	add("model", func(e *usage.Event) { e.Model = "other" }, true)
	add("target", func(e *usage.Event) { e.Target = "other" }, true)
	add("endpoint", func(e *usage.Event) { e.EndpointType = "other" }, false)
	add("components", func(e *usage.Event) { e.Components = fixedIdentityBundle().Components }, true)
	add("different-component", func(e *usage.Event) {
		e.Components = fixedIdentityBundle().Components
		e.Components[0].Model = "other-advisor"
	}, true)
	add("new-client", func(e *usage.Event) { e.ClientName = "bob" }, true)
	add("case", func(e *usage.Event) { e.ClientName = "Alice" }, true)
	add("invalid-utf8-a", func(e *usage.Event) { e.ClientName = "x\xff" }, true)
	add("invalid-utf8-b", func(e *usage.Event) { e.ClientName = "x\xfe" }, true)
	add("hour", func(e *usage.Event) { e.Timestamp = e.Timestamp.Add(time.Hour) }, true)
	add("unknown", func(e *usage.Event) { e.ClientName = "unknown" }, false)
	if err := db.View(func(tx *bolt.Tx) error {
		memo := make(freshScopeMemo)
		for _, c := range cases {
			rows, _, err := normalizeUsageBundle(c.event)
			if err != nil {
				return err
			}
			want := freshScopeProven(tx, rows) || continuationScopeProven(tx, rows)
			before := len(memo)
			if got := memo.proven(context.Background(), tx, rows); got != want || got != c.want {
				return fmt.Errorf("%s: memo=%t oracle=%t want=%t", c.name, got, want, c.want)
			}
			if c.name != "unknown" && len(memo) != before+1 {
				return fmt.Errorf("%s scope aliased", c.name)
			}
			if got := memo.proven(context.Background(), tx, rows); got != want || len(memo) != before+boolInt(c.name != "unknown") {
				return fmt.Errorf("%s repeat result=%t", c.name, got)
			}
		}
		rows, _, err := normalizeUsageBundle(fixedIdentityBundle())
		if err != nil {
			return err
		}
		before := len(memo)
		reordered := []usage.Event{rows[1], rows[0], rows[1]}
		want := freshScopeProven(tx, reordered) || continuationScopeProven(tx, reordered)
		if got := memo.proven(context.Background(), tx, reordered); got != want || len(memo) != before {
			return fmt.Errorf("reordered/deduplicated rows: memo=%t oracle=%t entries=%d want=%d", got, want, len(memo), before)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	boundary := base.Timestamp.Add(30 * time.Second)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(usageReceiptsWatermarkKey), []byte(boundary.Format(time.RFC3339Nano)))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		memo := make(freshScopeMemo)
		for _, tc := range []struct {
			offset time.Duration
			want   bool
		}{{-time.Second, false}, {time.Second, true}, {-time.Second, false}} {
			e := base
			e.ClientName = "boundary-client"
			e.Timestamp = boundary.Add(tc.offset)
			rows, _, err := normalizeUsageBundle(e)
			if err != nil {
				return err
			}
			oracle := freshScopeProven(tx, rows) || continuationScopeProven(tx, rows)
			if got := memo.proven(context.Background(), tx, rows); got != tc.want || got != oracle {
				return fmt.Errorf("boundary %v: memo=%t oracle=%t want=%t", tc.offset, got, oracle, tc.want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestBatchProofMemoDamagedEvidenceAndNewTransaction(t *testing.T) {
	for _, damage := range []string{"raw", "raw-owner", "receipt", "quarantine", "snapshot", "zero-cell"} {
		t.Run(damage, func(t *testing.T) {
			db := testDB(t)
			contentionHistory(t, db, 1, true)
			store := clockedStore(db)
			if err := store.EnsureUsageReceipts(); err != nil {
				t.Fatal(err)
			}
			e := fixedPlainEvent()
			rows, _, err := normalizeUsageBundle(e)
			if err != nil {
				t.Fatal(err)
			}
			check := func(want bool) {
				t.Helper()
				if err := db.View(func(tx *bolt.Tx) error {
					memo := make(freshScopeMemo)
					oracle := freshScopeProven(tx, rows) || continuationScopeProven(tx, rows)
					for i := 0; i < 2; i++ {
						if got := memo.proven(context.Background(), tx, rows); got != want || got != oracle {
							return fmt.Errorf("iteration %d: memo=%t oracle=%t want=%t", i, got, oracle, want)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			check(true)
			if err := db.Update(func(tx *bolt.Tx) error {
				key := []byte(usageKey(e.Timestamp, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
				switch damage {
				case "raw", "raw-owner":
					if err := tx.Bucket([]byte(BucketUsageEvents)).Put(key, []byte("{broken")); err != nil {
						return err
					}
					if damage == "raw-owner" {
						b, err := tx.CreateBucketIfNotExists([]byte(rawOwnerBucket))
						if err != nil {
							return err
						}
						return b.Put(key, []byte("{broken"))
					}
				case "receipt":
					return tx.Bucket([]byte(BucketUsageReceipts)).Put(key, []byte("{broken"))
				case "quarantine":
					return tx.Bucket([]byte(BucketUsageEventsQuarantine)).Put(key, []byte("{broken"))
				case "snapshot":
					root, err := tx.CreateBucketIfNotExists([]byte(usageBackfillSnapshotBucket))
					if err != nil {
						return err
					}
					source, err := root.CreateBucketIfNotExists([]byte(BucketUsageAggHourly))
					if err != nil {
						return err
					}
					generation, err := source.CreateBucketIfNotExists([]byte("fixture"))
					if err != nil {
						return err
					}
					return generation.Put([]byte("unknown"), []byte("broken"))
				case "zero-cell":
					value, err := json.Marshal(AggCell{})
					if err != nil {
						return err
					}
					return tx.Bucket([]byte(BucketUsageAggHourly)).Put(aggKey(e.Timestamp, e.EndpointType, e.ClientName, e.Model, e.Target), value)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			check(false)
		})
	}
}
