package nosql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestIndexEvidenceLossAcrossReopenCannotAuthorizeChangedRaw(t *testing.T) {
	db, _, resolver, day := cursorCostFixture(t)
	migrateClientIndex(t, db)
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		c := b.Cursor()
		for key, data := c.First(); key != nil; key, data = c.Next() {
			var event usage.Event
			if err := json.Unmarshal(data, &event); err != nil {
				return err
			}
			event.InputTokens = 3_000_000 - event.InputTokens
			changed, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if err := b.Put(append([]byte(nil), key...), changed); err != nil {
				return err
			}
		}
		if err := tx.DeleteBucket([]byte(BucketUsageEventsByClient)); err != nil {
			return err
		}
		return tx.DeleteBucket([]byte(BucketUsageClientHours))
	}); err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	units, err := NewUsageStore(reopened).ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, resolver, true)
	if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
		t.Fatalf("lost sources accepted changed raw: cost=%v units=%d err=%v", costUnitTotal(units), len(units), err)
	}
}

func TestIndexMigrationResumesAfterCursorRawPruned(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	putLegacyClientEvents(t, db, []usage.Event{
		{Timestamp: day.Add(9*time.Hour + 10*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 1_000_000},
		{Timestamp: day.Add(10*time.Hour + 45*time.Minute), ClientName: "alice", Target: "upstream", Model: "qwen-max", InputTokens: 2_000_000},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := migrateUsageClientIndex(ctx, db, 1, cancel); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(day.Add(10*time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("prune=%d err=%v", n, err)
	}
	from, to := day.Add(10*time.Hour), day.Add(11*time.Hour-time.Nanosecond)
	r := pricing.NewResolver([]pricing.PricingTemplate{{Model: "qwen-max", Start: day.Add(time.Minute), Mode: pricing.ModeFlat, BaseRates: usage.CostRates{InputPer1MTokens: 2}}}, nil)
	units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: "alice", From: &from, To: &to}, r, true)
	if err != nil || costUnitTotal(units) != 4 {
		t.Fatalf("pruned cursor blocked unrelated retained hour: units=%+v err=%v", units, err)
	}
	if err := MigrateUsageClientIndex(context.Background(), db, 1); err != nil {
		t.Fatalf("resume after prune: %v", err)
	}
}

func TestIndexV2CostRejectsDamagedEvidence(t *testing.T) {
	for _, damage := range []string{"progress-missing", "progress-corrupt", "required-missing", "required-corrupt", "proof-bucket-missing", "proof-missing", "proof-corrupt", "cursor-back", "cursor-forward", "cursor-missing"} {
		t.Run(damage, func(t *testing.T) {
			db, store, resolver, day := cursorCostFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := migrateUsageClientIndex(ctx, db, 2, cancel); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				meta := tx.Bucket([]byte(BucketMeta))
				c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
				first, _ := c.First()
				first = append([]byte(nil), first...)
				switch damage {
				case "progress-missing":
					return meta.Delete([]byte(indexProgressV2))
				case "progress-corrupt":
					return meta.Put([]byte(indexProgressV2), []byte("{}"))
				case "required-missing":
					return meta.Delete([]byte(indexRequiredV2))
				case "required-corrupt":
					return meta.Put([]byte(indexRequiredV2), []byte("1"))
				case "proof-bucket-missing":
					return tx.DeleteBucket([]byte(indexProofsV2))
				case "proof-missing":
					return tx.Bucket([]byte(indexProofsV2)).Delete(first)
				case "proof-corrupt":
					return tx.Bucket([]byte(indexProofsV2)).Put(first, []byte("{}"))
				case "cursor-back":
					return meta.Put([]byte(usageClientIndexCursor), first)
				case "cursor-forward":
					return meta.Put([]byte(usageClientIndexCursor), []byte(usageKey(day.Add(9*time.Hour+50*time.Minute), "forward")))
				default:
					return meta.Delete([]byte(usageClientIndexCursor))
				}
			}); err != nil {
				t.Fatal(err)
			}
			from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
			for _, client := range []string{"alice", ""} {
				units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, resolver, true)
				if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
					t.Errorf("client=%q accepted %s: units=%+v err=%v", client, damage, units, err)
				}
			}
		})
	}
}

func TestIndexV2AtomicProofForCost(t *testing.T) {
	for _, damage := range []string{"none", "missing", "corrupt", "wrong-origin"} {
		t.Run(damage, func(t *testing.T) {
			db, store, resolver, day := cursorCostFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := migrateUsageClientIndex(ctx, db, 1, cancel); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
				c.First()
				key, value := c.Next()
				if err := ReindexUsageEvent(tx, key, value, value); err != nil {
					return err
				}
				proofs := tx.Bucket([]byte(indexProofsV2))
				switch damage {
				case "missing":
					return proofs.Delete(key)
				case "corrupt":
					return proofs.Put(key, []byte("{}"))
				case "wrong-origin":
					var proof indexProof
					if err := json.Unmarshal(proofs.Get(key), &proof); err != nil {
						return err
					}
					proof.Origin = "scan"
					proof.Binding = proof.binding(key)
					data, err := json.Marshal(proof)
					if err != nil {
						return err
					}
					return proofs.Put(key, data)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
			for _, client := range []string{"alice", ""} {
				units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, resolver, true)
				if damage == "none" {
					if err != nil || costUnitTotal(units) != 21 {
						t.Errorf("client=%q cost=%v err=%v", client, costUnitTotal(units), err)
					}
				} else if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
					t.Errorf("client=%q accepted %s: units=%+v err=%v", client, damage, units, err)
				}
			}
		})
	}
}

func TestIndexV2MissingEarlierProofCannotCompleteMigration(t *testing.T) {
	db, _, _, _ := cursorCostFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := migrateUsageClientIndex(ctx, db, 2, cancel); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		first, _ := tx.Bucket([]byte(BucketUsageEvents)).Cursor().First()
		return tx.Bucket([]byte(indexProofsV2)).Delete(first)
	}); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUsageClientIndex(context.Background(), db, 1); !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("missing earlier proof allowed completion: %v", err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketMeta)).Get([]byte(usageClientIndexDone)) != nil {
			t.Error("completed after proof loss")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIndexV2MissingAllAnchorsDoesNotCertifyPartialV1(t *testing.T) {
	db, store, resolver, day := cursorCostFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := migrateUsageClientIndex(ctx, db, 2, cancel); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		if err := meta.Delete([]byte(indexProgressV2)); err != nil {
			return err
		}
		if err := meta.Delete([]byte(indexRequiredV2)); err != nil {
			return err
		}
		return tx.DeleteBucket([]byte(indexProofsV2))
	}); err != nil {
		t.Fatal(err)
	}
	from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
	for _, client := range []string{"alice", ""} {
		units, err := store.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, resolver, true)
		if !errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 {
			t.Errorf("client=%q certified ambiguous v1: units=%+v err=%v", client, units, err)
		}
	}
}

func TestIndexV2ReindexDoesNotRepairMissingScannedProof(t *testing.T) {
	db, _, _, _ := cursorCostFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := migrateUsageClientIndex(ctx, db, 2, cancel); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var key, value []byte
	if err := db.Update(func(tx *bolt.Tx) error {
		k, v := tx.Bucket([]byte(BucketUsageEvents)).Cursor().First()
		key, value = append([]byte(nil), k...), append([]byte(nil), v...)
		return tx.Bucket([]byte(indexProofsV2)).Delete(key)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return ReindexUsageEvent(tx, key, value, value) }); !errors.Is(err, ErrUsageClientIndexIncomplete) {
		t.Fatalf("reindex repaired missing proof: %v", err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(indexProofsV2)).Get(key) != nil {
			t.Error("reindex created replacement proof")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIndexV2ReindexPreservesProofAndAccountingBoundary(t *testing.T) {
	for _, origin := range []string{"scan", "atomic"} {
		for _, rewrite := range []string{"accounting", "formatting"} {
			t.Run(origin+"/"+rewrite, func(t *testing.T) {
				db, store, resolver, day := cursorCostFixture(t)
				var key, before, proofBytes, ownerBytes []byte
				if err := db.Update(func(tx *bolt.Tx) error {
					raw := tx.Bucket([]byte(BucketUsageEvents))
					c := raw.Cursor()
					k, v := c.First()
					if origin == "atomic" {
						k, v = c.Next()
					}
					key = bytes.Clone(k)
					before = append(bytes.Clone(bytes.TrimSuffix(v, []byte("}"))), []byte(`,"unknown_field":{"preserved":true}}`)...)
					return raw.Put(key, before)
				}); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if err := migrateUsageClientIndex(ctx, db, 1, cancel); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if origin == "atomic" {
					if err := db.Update(func(tx *bolt.Tx) error { return ReindexUsageEvent(tx, key, before, before) }); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.View(func(tx *bolt.Tx) error {
					proofBytes = bytes.Clone(tx.Bucket([]byte(indexProofsV2)).Get(key))
					var proof indexProof
					if json.Unmarshal(proofBytes, &proof) != nil || proof.Origin != origin {
						t.Fatalf("proof origin=%q, want %q", proof.Origin, origin)
					}
					if owners := tx.Bucket([]byte(rawOwnerBucket)); owners != nil {
						ownerBytes = bytes.Clone(owners.Get(key))
					}
					if ownerBytes != nil {
						t.Fatal("legacy fixture unexpectedly has an owner")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := db.Update(func(tx *bolt.Tx) error { return ReindexUsageEvent(tx, key, before, before) }); err != nil {
					t.Fatal(err)
				}
				from, to := day.Add(9*time.Hour), day.Add(10*time.Hour-time.Nanosecond)
				checkCost := func(s *UsageStore, wantError bool) {
					t.Helper()
					for _, client := range []string{"alice", ""} {
						units, err := s.ReadCostUnits(context.Background(), usage.Filter{ClientName: client, From: &from, To: &to}, resolver, true)
						if wantError && (!errors.Is(err, ErrUsageHistoryIncomplete) || len(units) != 0 || !strings.Contains(err.Error(), "invalid client index proof")) {
							t.Errorf("client=%q accepted changed proof: units=%+v err=%v", client, units, err)
						}
						if !wantError && (err != nil || costUnitTotal(units) != 21) {
							t.Errorf("client=%q no-op cost=%v err=%v", client, costUnitTotal(units), err)
						}
					}
				}
				checkCost(store, false)
				var original usage.Event
				if err := json.Unmarshal(before, &original); err != nil {
					t.Fatal(err)
				}
				after := append([]byte(" \n"), before...)
				if rewrite == "accounting" {
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(before, &fields); err != nil {
						t.Fatal(err)
					}
					fields["input_tokens"] = json.RawMessage("4000000")
					var err error
					after, err = json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
				}
				var changed usage.Event
				if err := json.Unmarshal(after, &changed); err != nil {
					t.Fatal(err)
				}
				oldDigest, err := usage.RetryEquivalentDigest(original)
				if err != nil {
					t.Fatal(err)
				}
				newDigest, err := usage.RetryEquivalentDigest(changed)
				if err != nil || (oldDigest == newDigest) != (rewrite == "formatting") {
					t.Fatalf("retry identity: %v", err)
				}
				if !bytes.Contains(after, []byte(`"unknown_field"`)) {
					t.Fatal("unknown field missing from after bytes")
				}
				var afterFields map[string]json.RawMessage
				if err := json.Unmarshal(after, &afterFields); err != nil || string(afterFields["unknown_field"]) != `{"preserved":true}` {
					t.Fatalf("unknown field changed: %s err=%v", afterFields["unknown_field"], err)
				}
				if err := db.Update(func(tx *bolt.Tx) error {
					if err := ReindexUsageEvent(tx, key, before, after); err != nil {
						return err
					}
					if err := tx.Bucket([]byte(BucketUsageEvents)).Put(key, after); err != nil {
						return err
					}
					for _, cell := range []struct {
						name string
						key  []byte
					}{
						{BucketUsageAggHourly, aggKey(original.Timestamp, original.EndpointType, original.ClientName, original.Model, original.Target)},
						{BucketQuotaUsageAggHourly, quotaAggKey(original.Timestamp, original.ClientName, original.Target, original.Model)},
					} {
						b := tx.Bucket([]byte(cell.name))
						var value AggCell
						if err := json.Unmarshal(b.Get(cell.key), &value); err != nil {
							return err
						}
						value.InputTokens += changed.InputTokens - original.InputTokens
						data, err := json.Marshal(value)
						if err != nil {
							return err
						}
						if err := b.Put(cell.key, data); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				verify := func(db *bolt.DB) {
					t.Helper()
					if err := db.View(func(tx *bolt.Tx) error {
						if !bytes.Equal(proofBytes, tx.Bucket([]byte(indexProofsV2)).Get(key)) || !bytes.Equal(after, tx.Bucket([]byte(BucketUsageEvents)).Get(key)) {
							t.Error("proof or raw bytes changed")
						}
						if owners := tx.Bucket([]byte(rawOwnerBucket)); owners != nil && !bytes.Equal(ownerBytes, owners.Get(key)) {
							t.Error("owner evidence changed")
						}
						if tx.Bucket([]byte(BucketUsageEvents)).Stats().KeyN != 2 {
							t.Error("unexpected raw row count")
						}
						if !bytes.Equal(key, tx.Bucket([]byte(BucketUsageEventsByClient)).Get(clientEventIndexKey(changed, key))) {
							t.Error("derived index missing after event")
						}
						events, err := readClientHourInTx(context.Background(), tx, "alice", original.Timestamp.UTC().Truncate(time.Hour), nil, nil)
						wantCount := 1
						if origin == "atomic" {
							wantCount = 2
						}
						if err != nil || len(events) != wantCount {
							t.Errorf("derived hour events=%d err=%v", len(events), err)
						}
						matched := false
						for _, event := range events {
							if event.Timestamp.Equal(changed.Timestamp) && event.InputTokens == changed.InputTokens {
								matched = true
							}
						}
						if !matched {
							t.Error("derived hour lacks rewritten event")
						}
						for _, cell := range []struct {
							name string
							key  []byte
						}{
							{BucketUsageAggHourly, aggKey(original.Timestamp, original.EndpointType, original.ClientName, original.Model, original.Target)},
							{BucketQuotaUsageAggHourly, quotaAggKey(original.Timestamp, original.ClientName, original.Target, original.Model)},
						} {
							var value AggCell
							if err := json.Unmarshal(tx.Bucket([]byte(cell.name)).Get(cell.key), &value); err != nil || value.InputTokens != 3_000_000+changed.InputTokens-original.InputTokens || value.recordCount() != 2 {
								t.Errorf("%s incorrect rollup: %+v err=%v", cell.name, value, err)
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				verify(db)
				checkCost(store, true)
				path := db.Path()
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := OpenDB(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { reopened.Close() })
				verify(reopened)
				checkCost(NewUsageStore(reopened), true)
			})
		}
	}
}
