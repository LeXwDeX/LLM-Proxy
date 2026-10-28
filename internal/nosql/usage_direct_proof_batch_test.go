package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestDirectProofScansDoNotMultiplyByCell(t *testing.T) {
	for _, n := range []int{1, 4} {
		t.Run(fmt.Sprintf("cells=%d", n), func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			base := fixedPlainEvent()
			for i := 0; i < n; i++ {
				event := base
				event.RecordID = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", i+1)
				event.Model = fmt.Sprintf("model-%d", i)
				if err := store.Record(event); err != nil {
					t.Fatal(err)
				}
			}
			if count, err := store.PruneEventsBefore(base.Timestamp.Add(time.Hour), 100); err != nil || count != n {
				t.Fatalf("prune count=%d err=%v", count, err)
			}
			fresh := base
			fresh.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-999999999999"
			fresh.Timestamp = base.Timestamp.Add(time.Second)
			if err := store.Record(fresh); err != nil {
				t.Fatal(err)
			}
			from := base.Timestamp.Truncate(time.Hour)
			to := from.Add(time.Hour - time.Nanosecond)
			raw, receipts := 0, 0
			err := db.View(func(tx *bolt.Tx) error {
				for i := 0; i < n; i++ {
					key := quotaAggKey(base.Timestamp, base.ClientName, base.Target, fmt.Sprintf("model-%d", i))
					if _, ok := readRollupProofValue(tx, BucketQuotaUsageAggHourly, key); !ok {
						t.Fatalf("fixture: missing or invalid proof for cell %d", i)
					}
				}
				return checkDirectQuotaEvidence(tx, usage.Filter{ClientName: base.ClientName}, from, to, nil, &directIntegrityHooks{
					raw: func() { raw++ }, receipt: func() { receipts++ },
				})
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("cells=%d raw visits=%d receipt visits=%d", n, raw, receipts)
			if raw > 2 || receipts > 2*n {
				t.Fatalf("proof scans multiplied by cell count: raw=%d receipt=%d", raw, receipts)
			}
		})
	}
}

func TestDirectProofBatchNarrowScopeState(t *testing.T) {
	for _, n := range []int{1, 96} {
		t.Run(fmt.Sprintf("proofs=%d", n), func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			base := fixedPlainEvent()
			for i := 0; i < n; i++ {
				e := base
				e.RecordID = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", i+1)
				e.Model = fmt.Sprintf("model-%02d", i)
				if i > 0 {
					e.ClientName = "bob"
					e.Timestamp = e.Timestamp.Add(2 * time.Hour)
				}
				if err := store.Record(e); err != nil {
					t.Fatal(err)
				}
			}
			if count, err := store.PruneEventsBefore(base.Timestamp.Add(4*time.Hour), 100); err != nil || count != n {
				t.Fatalf("prune=%d err=%v", count, err)
			}
			from := base.Timestamp.Truncate(time.Hour)
			for _, dimension := range []string{"client", "hour", "model", "group"} {
				to := from.Add(4*time.Hour - time.Nanosecond)
				filter := usage.Filter{}
				var allow costPairFilter
				switch dimension {
				case "client":
					filter.ClientName = base.ClientName
				case "hour":
					to = from.Add(time.Hour - time.Nanosecond)
				case "model":
					filter.Model = "model-00"
				case "group":
					allow = costPairFilter(func(_, model string) bool { return model == "model-00" })
				}
				cells, identities, raw, receipts := 0, 0, 0, 0
				hooks := &directIntegrityHooks{proofCell: func() { cells++ }, proofIdentity: func() { identities++ }, raw: func() { raw++ }, receipt: func() { receipts++ }}
				err := db.View(func(tx *bolt.Tx) error {
					if err := checkDirectQuotaEvidenceContext(context.Background(), tx, filter, from, to, allow, hooks); err != nil {
						return err
					}
					batch, err := quotaProofBatchInScopeContext(context.Background(), tx, filter, from, to, allow, nil)
					if err != nil {
						return err
					}
					key := quotaAggKey(base.Timestamp, base.ClientName, base.Target, "model-00")
					oracle, err := validRollupProofContext(context.Background(), tx, BucketQuotaUsageAggHourly, key, nil)
					if err != nil {
						return err
					}
					cell := batch[string(key)]
					if len(batch) != 1 || cell == nil || len(cell.seen) != 1 || !cell.valid() || !oracle {
						t.Errorf("%s scoped/oracle state: cells=%d cell=%+v oracle=%v", dimension, len(batch), cell, oracle)
					}
					return nil
				})
				if err != nil {
					t.Fatalf("%s: %v", dimension, err)
				}
				t.Logf("%s proofs=%d cells=%d identities=%d raw=%d receipts=%d", dimension, n, cells, identities, raw, receipts)
				if cells != 1 || identities != 1 || raw != 0 || receipts != 2*n {
					t.Fatalf("%s: cells=%d identities=%d raw=%d receipts=%d", dimension, cells, identities, raw, receipts)
				}
			}
		})
	}
}

func TestQuotaProofBatchMatchesSingleCellOracle(t *testing.T) {
	scenarios := []string{"clean", "mixed_duplicate", "mixed_conflict", "missing_index", "noncanonical_raw", "unknown_raw", "unknown_hour", "other_model", "duplicate_json", "owned_bad_model", "owner_payload_conflict", "bad_raw_owner", "bad_receipt", "bad_receipt_owner", "bad_cell_binding", "unknown_receipt", "missing_receipt", "bad_proof", "noncanonical_proof", "tombstone", "bad_marker", "cell_tamper"}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			base := fixedPlainEvent()
			var events []usage.Event
			for i := 0; i < 9; i++ {
				e := base
				e.RecordID = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", i+1)
				e.ClientName = []string{"alice", "bob"}[(i/4)%2]
				e.Timestamp = base.Timestamp.Add(time.Duration((i/2)%2) * time.Hour)
				e.Model = []string{"model-a", "model-b"}[i%2]
				if i == 8 {
					e.ClientName, e.Timestamp, e.Model = "alice", base.Timestamp, "model-a"
				}
				if err := store.Record(e); err != nil {
					t.Fatal(err)
				}
				events = append(events, e)
			}
			if n, err := store.PruneEventsBefore(base.Timestamp.Add(3*time.Hour), 100); err != nil || n != len(events) {
				t.Fatalf("prune=%d %v", n, err)
			}
			key := []byte(usageKey(events[0].Timestamp, events[0].RecordID))
			qkey := quotaAggKey(events[0].Timestamp, events[0].ClientName, events[0].Target, events[0].Model)
			if err := db.Update(func(tx *bolt.Tx) error {
				raw := tx.Bucket([]byte(BucketUsageEvents))
				switch scenario {
				case "mixed_duplicate", "mixed_conflict", "missing_index", "noncanonical_raw", "owned_bad_model", "owner_payload_conflict", "bad_raw_owner":
					e := events[0]
					if scenario == "mixed_conflict" {
						e.InputTokens++
					}
					data, err := json.Marshal(e)
					if err != nil {
						return err
					}
					if scenario == "noncanonical_raw" {
						data = append([]byte(" "), data...)
					}
					if scenario == "owned_bad_model" {
						data = []byte(`{"client_name":"alice","model":"model-b","input_tokens":"bad"}`)
					}
					if scenario == "bad_raw_owner" {
						data = []byte(`{"client_name":"alice","input_tokens":"bad"}`)
					}
					if scenario == "owner_payload_conflict" {
						data = []byte(`{"client_name":"bob","model":"model-b","input_tokens":"bad"}`)
					}
					if err := raw.Put(key, data); err != nil {
						return err
					}
					if scenario != "missing_index" {
						if err := tx.Bucket([]byte(BucketUsageEventsByClient)).Put(clientEventIndexKey(e, key), key); err != nil {
							return err
						}
					}
					if scenario == "bad_raw_owner" {
						return tx.Bucket([]byte(rawOwnerBucket)).Put(key, []byte(`{}`))
					}
				case "unknown_raw":
					return raw.Put([]byte("bad-key"), []byte(`{`))
				case "unknown_hour":
					return raw.Put([]byte("bad-key"), []byte(`{"client_name":"alice","model":"model-a","input_tokens":"bad"}`))
				case "other_model":
					return raw.Put([]byte(usageKey(base.Timestamp, "bad")), []byte(`{"client_name":"alice","model":"other","input_tokens":"bad"}`))
				case "duplicate_json":
					return raw.Put([]byte(usageKey(base.Timestamp, "bad")), []byte(`{"client_name":"alice","client_name":"bob"}`))
				case "bad_receipt":
					return tx.Bucket([]byte(BucketUsageReceipts)).Put(key, []byte(`{}`))
				case "bad_receipt_owner":
					return tx.Bucket([]byte(receiptOwnersBucket)).Put(key, []byte(`{}`))
				case "bad_cell_binding":
					b := tx.Bucket([]byte(receiptOwnersBucket))
					entry, ok := decodeReceiptOwner(b.Get(key))
					if !ok {
						return errors.New("invalid fixture owner")
					}
					entry.CellBinding = "damaged"
					data, err := json.Marshal(entry)
					if err != nil {
						return err
					}
					return b.Put(key, data)
				case "unknown_receipt":
					return tx.Bucket([]byte(BucketUsageReceipts)).Put([]byte("bad-key"), []byte(`{}`))
				case "missing_receipt":
					return tx.Bucket([]byte(BucketUsageReceipts)).Delete(key)
				case "bad_proof":
					return tx.Bucket([]byte(rollupProofBucket)).Put(rollupProofKey(BucketQuotaUsageAggHourly, qkey), []byte(`{}`))
				case "noncanonical_proof":
					b := tx.Bucket([]byte(rollupProofBucket))
					k := rollupProofKey(BucketQuotaUsageAggHourly, qkey)
					return b.Put(k, append([]byte(" "), b.Get(k)...))
				case "tombstone":
					return invalidateRollupProof(tx, BucketQuotaUsageAggHourly, qkey)
				case "bad_marker":
					return tx.Bucket([]byte(BucketMeta)).Put([]byte(rollupProofMarker), []byte("bad"))
				case "cell_tamper":
					return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put(qkey, []byte(`{}`))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				batch, err := quotaProofBatchContext(context.Background(), tx, nil)
				if err != nil {
					return err
				}
				seen := make(map[string]bool)
				labels := []string{"alice-0-a", "alice-0-b", "alice-1-a", "alice-1-b", "bob-0-a", "bob-0-b", "bob-1-a", "bob-1-b", "alice-0-a"}
				all := "alice-0-a alice-0-b alice-1-a alice-1-b bob-0-a bob-0-b bob-1-a bob-1-b"
				badCells := map[string]string{
					"clean": "", "mixed_duplicate": "", "mixed_conflict": "alice-0-a", "missing_index": "alice-0-a",
					"noncanonical_raw": "alice-0-a alice-0-b", "unknown_raw": all, "unknown_hour": "alice-0-a alice-1-a", "other_model": "",
					"duplicate_json": all, "owned_bad_model": "alice-0-a alice-0-b", "owner_payload_conflict": all, "bad_raw_owner": all,
					"bad_receipt": "alice-0-a alice-0-b bob-0-a bob-0-b", "bad_receipt_owner": "alice-0-a alice-0-b bob-0-a bob-0-b",
					"bad_cell_binding": "alice-0-a alice-0-b", "unknown_receipt": all,
					"missing_receipt": "alice-0-a", "bad_proof": "alice-0-a", "noncanonical_proof": "alice-0-a", "tombstone": "alice-0-a", "bad_marker": all, "cell_tamper": "alice-0-a",
				}
				expected := make(map[string]bool)
				for _, label := range strings.Fields(badCells[scenario]) {
					expected[label] = true
				}
				for i, e := range events {
					k := quotaAggKey(e.Timestamp, e.ClientName, e.Target, e.Model)
					if seen[string(k)] {
						continue
					}
					seen[string(k)] = true
					want, err := validRollupProofContext(context.Background(), tx, BucketQuotaUsageAggHourly, k, nil)
					if err != nil {
						return err
					}
					got := batch[string(k)].valid()
					if got == expected[labels[i]] {
						t.Errorf("cell %s: valid=%v; expected invalid=%v", labels[i], got, expected[labels[i]])
					}
					if got != want {
						t.Errorf("cell %q batch=%v oracle=%v", k, got, want)
					}
					state, ok := readRollupProofValue(tx, BucketQuotaUsageAggHourly, k)
					if ok {
						count, xor, identitiesOK, err := currentRollupIdentitiesContext(context.Background(), tx, BucketQuotaUsageAggHourly, k, nil)
						if err != nil {
							return err
						}
						cell := batch[string(k)]
						if cell == nil || identitiesOK != !cell.invalid {
							t.Errorf("identity status mismatch cell=%q oracle=%v batch=%+v", k, identitiesOK, cell)
							continue
						}
						if identitiesOK && (cell == nil || cell.invalid || cell.count != count || cell.xor != xor || got != (count == state.IdentityCount && xor == state.IdentityXor)) {
							t.Errorf("identity mismatch cell=%q", k)
						}
						if !identitiesOK && cell.xor != xor {
							t.Errorf("partial identity XOR mismatch cell=%q", k)
						}
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestQuotaProofBatchCancellation(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	e := fixedPlainEvent()
	if err := store.Record(e); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventsBefore(e.Timestamp.Add(time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	fresh := e
	fresh.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-999999999999"
	if err := store.Record(fresh); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		for _, at := range []string{"raw", "receipt"} {
			ctx, cancel := context.WithCancel(context.Background())
			visits := 0
			stop := func() { visits++; cancel() }
			hooks := &directIntegrityHooks{}
			if at == "raw" {
				hooks.raw = stop
			} else {
				hooks.receipt = stop
			}
			_, err := quotaProofBatchContext(ctx, tx, hooks)
			if !errors.Is(err, context.Canceled) || visits != 1 {
				t.Fatalf("cancel %s: err=%v visits=%d", at, err, visits)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDirectNarrowProofDamageBoundaries(t *testing.T) {
	for _, scenario := range []string{"invalid_proof", "unrelated_bad_raw", "unknown_bad_raw", "invalid_quota_key"} {
		t.Run(scenario, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			e := fixedPlainEvent()
			if err := store.Record(e); err != nil {
				t.Fatal(err)
			}
			if n, err := store.PruneEventsBefore(e.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
				t.Fatalf("prune=%d %v", n, err)
			}
			key := quotaAggKey(e.Timestamp, e.ClientName, e.Target, e.Model)
			if err := db.Update(func(tx *bolt.Tx) error {
				switch scenario {
				case "invalid_proof":
					return invalidateRollupProof(tx, BucketQuotaUsageAggHourly, key)
				case "unrelated_bad_raw":
					return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(e.Timestamp, "aaaaaaaa-aaaa-4aaa-8aaa-999999999999")), []byte(`{"client_name":"bob","model":"different","input_tokens":"bad"}`))
				case "unknown_bad_raw":
					return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte("unknown"), []byte(`{`))
				case "invalid_quota_key":
					quota := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
					badKey := []byte("invalid-hour|alice|anthropic|executor")
					if err := quota.Put(badKey, quota.Get(key)); err != nil {
						return err
					}
					return putRollupProof(tx, BucketQuotaUsageAggHourly, badKey, rollupProofState{IdentityCount: 1})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			from, to := e.Timestamp.Truncate(time.Hour), e.Timestamp.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
			filter := usage.Filter{ClientName: e.ClientName, Model: e.Model}
			allow := costPairFilter(func(_, model string) bool { return model == e.Model })
			cells, identities, raw, receipts := 0, 0, 0, 0
			hooks := &directIntegrityHooks{proofCell: func() { cells++ }, proofIdentity: func() { identities++ }, raw: func() { raw++ }, receipt: func() { receipts++ }}
			err := db.View(func(tx *bolt.Tx) error {
				if scenario == "unrelated_bad_raw" || scenario == "unknown_bad_raw" {
					batch, err := quotaProofBatchInScopeContext(context.Background(), tx, filter, from, to, allow, nil)
					if err != nil {
						return err
					}
					valid := batch[string(key)].valid()
					if valid != (scenario == "unrelated_bad_raw") {
						t.Errorf("scoped proof for %s: valid=%v", scenario, valid)
					}
					oracle, err := validRollupProofContext(context.Background(), tx, BucketQuotaUsageAggHourly, key, nil)
					if err != nil {
						return err
					}
					if valid != oracle {
						t.Errorf("scoped proof differs from oracle: %v %v", valid, oracle)
					}
				}
				if scenario == "invalid_quota_key" {
					batch, err := quotaProofBatchInScopeContext(context.Background(), tx, filter, from, to, allow, nil)
					if err != nil {
						return err
					}
					if cell := batch["invalid-hour|alice|anthropic|executor"]; cell == nil || !cell.invalid {
						t.Errorf("malformed proof key omitted: %+v", cell)
					}
				}
				return checkDirectQuotaEvidenceContext(context.Background(), tx, filter, from, to, allow, hooks)
			})
			if scenario == "unrelated_bad_raw" {
				if err != nil || cells != 1 || identities != 1 {
					t.Fatalf("unrelated damage: err=%v cells=%d identities=%d", err, cells, identities)
				}
			} else if !errors.Is(err, ErrUsageHistoryIncomplete) {
				t.Fatalf("%s: err=%v", scenario, err)
			}
			if scenario == "invalid_proof" && (cells != 0 || identities != 0 || raw != 0 || receipts != 1) {
				t.Fatalf("invalid proof did not fail early: cells=%d identities=%d raw=%d receipts=%d", cells, identities, raw, receipts)
			}
		})
	}
}

func TestDirectScopedProofDoesNotSkipSecondInvalidCell(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	e := fixedPlainEvent()
	e.Model = "model-a"
	other := e
	other.Model = "model-b"
	other.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-999999999999"
	for _, event := range []usage.Event{e, other} {
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := store.PruneEventsBefore(e.Timestamp.Add(time.Hour), 100); err != nil || n != 2 {
		t.Fatalf("prune=%d %v", n, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return invalidateRollupProof(tx, BucketQuotaUsageAggHourly, quotaAggKey(other.Timestamp, other.ClientName, other.Target, other.Model))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := e.Timestamp.Truncate(time.Hour), e.Timestamp.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
	for _, tc := range []struct {
		name      string
		allow     costPairFilter
		wantError bool
	}{
		{"both", costPairFilter(func(_, _ string) bool { return true }), true},
		{"only-a", costPairFilter(func(_, model string) bool { return model == "model-a" }), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := db.View(func(tx *bolt.Tx) error {
				return checkDirectQuotaEvidenceContext(context.Background(), tx, usage.Filter{ClientName: e.ClientName}, from, to, tc.allow, nil)
			})
			if tc.wantError != errors.Is(err, ErrUsageHistoryIncomplete) || (!tc.wantError && err != nil) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestDirectProofBatchPartialPruneAndModelGroup(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-999999999999"
	second.Timestamp = first.Timestamp.Add(10 * time.Second)
	for _, e := range []usage.Event{first, second} {
		if err := store.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := store.PruneEventsBefore(first.Timestamp.Add(5*time.Second), 1); err != nil || n != 1 {
		t.Fatalf("partial prune=%d %v", n, err)
	}
	from := first.Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour - time.Nanosecond)
	filter := usage.Filter{ClientName: first.ClientName}
	allowed := costPairFilter(func(_, model string) bool { return model == first.Model })
	excluded := costPairFilter(func(_, _ string) bool { return false })
	check := func(allow costPairFilter) error {
		return db.View(func(tx *bolt.Tx) error {
			return checkDirectQuotaEvidenceContext(context.Background(), tx, filter, from, to, allow, nil)
		})
	}
	if err := check(allowed); err != nil {
		t.Fatalf("partial prune: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return invalidateRollupProof(tx, BucketQuotaUsageAggHourly, quotaAggKey(first.Timestamp, first.ClientName, first.Target, first.Model))
	}); err != nil {
		t.Fatal(err)
	}
	if err := check(allowed); !errors.Is(err, ErrUsageHistoryIncomplete) {
		t.Fatalf("included damaged proof: %v", err)
	}
	if err := check(excluded); err != nil {
		t.Fatalf("excluded damaged proof: %v", err)
	}
}

func BenchmarkQuotaProofBatch(b *testing.B) {
	for _, n := range []int{1, 8} {
		for _, unknown := range []bool{false, true} {
			b.Run(fmt.Sprintf("cells=%d/unknown=%t", n, unknown), func(b *testing.B) {
				db := testDB(b)
				store := clockedStore(db)
				base := fixedPlainEvent()
				var keys [][]byte
				for i := 0; i < n; i++ {
					e := base
					e.RecordID = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", i+1)
					e.Model = fmt.Sprintf("model-%d", i)
					if err := store.Record(e); err != nil {
						b.Fatal(err)
					}
					keys = append(keys, quotaAggKey(e.Timestamp, e.ClientName, e.Target, e.Model))
				}
				if count, err := store.PruneEventsBefore(base.Timestamp.Add(time.Hour), 100); err != nil || count != n {
					b.Fatalf("prune=%d %v", count, err)
				}
				if unknown {
					if err := db.Update(func(tx *bolt.Tx) error {
						return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte("bad-key"), []byte(`{`))
					}); err != nil {
						b.Fatal(err)
					}
				}
				for _, mode := range []string{"oracle", "batch"} {
					b.Run(mode, func(b *testing.B) {
						b.ReportAllocs()
						for i := 0; i < b.N; i++ {
							err := db.View(func(tx *bolt.Tx) error {
								if mode == "batch" {
									_, err := quotaProofBatchContext(context.Background(), tx, nil)
									return err
								}
								for _, key := range keys {
									if _, err := validRollupProofContext(context.Background(), tx, BucketQuotaUsageAggHourly, key, nil); err != nil {
										return err
									}
								}
								return nil
							})
							if err != nil {
								b.Fatal(err)
							}
						}
					})
				}
			})
		}
	}
}

func BenchmarkQuotaProofBatchNarrowScope(b *testing.B) {
	for _, n := range []int{1, 96} {
		b.Run(fmt.Sprintf("proofs=%d", n), func(b *testing.B) {
			db := testDB(b)
			store := clockedStore(db)
			base := fixedPlainEvent()
			for i := 0; i < n; i++ {
				e := base
				e.RecordID = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", i+1)
				e.Model = fmt.Sprintf("model-%02d", i)
				if err := store.Record(e); err != nil {
					b.Fatal(err)
				}
			}
			if count, err := store.PruneEventsBefore(base.Timestamp.Add(time.Hour), 100); err != nil || count != n {
				b.Fatalf("prune=%d err=%v", count, err)
			}
			from := base.Timestamp.Truncate(time.Hour)
			to := from.Add(time.Hour - time.Nanosecond)
			filter := usage.Filter{ClientName: base.ClientName, Model: "model-00"}
			allow := costPairFilter(func(_, model string) bool { return model == "model-00" })
			for _, mode := range []string{"all", "narrow"} {
				b.Run(mode, func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						if err := db.View(func(tx *bolt.Tx) error {
							if mode == "all" {
								_, err := quotaProofBatchContext(context.Background(), tx, nil)
								return err
							}
							_, err := quotaProofBatchInScopeContext(context.Background(), tx, filter, from, to, allow, nil)
							return err
						}); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
