package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

type cancelOnCheck struct {
	context.Context
	cancel context.CancelFunc
	left   int
}

func (c *cancelOnCheck) Err() error {
	c.left--
	if c.left == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestCostCancellationStopsNestedProofAndReturnsContextError(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	first := fixedPlainEvent()
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(first.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("prune=%d err=%v", n, err)
	}
	second := first
	second.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8"
	second.Timestamp = first.Timestamp.Add(time.Second)
	if err := store.Record(second); err != nil {
		t.Fatal(err)
	}
	from := first.Timestamp.Truncate(time.Hour)
	to := from.Add(time.Hour - time.Nanosecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rawVisits := 0
	got, err := store.readCostUnits(ctx, usage.Filter{ClientName: first.ClientName, From: &from, To: &to}, replayCosts, false, nil, costReadHooks{direct: &directIntegrityHooks{raw: func() {
		rawVisits++
		if rawVisits == 2 {
			cancel()
		}
	}}})
	if !errors.Is(err, context.Canceled) || len(got.units) != 0 || rawVisits != 2 {
		t.Fatalf("result=%+v err=%v raw visits=%d", got, err, rawVisits)
	}
}

func TestCostCancellationDuringEvidenceReleasesTransaction(t *testing.T) {
	for _, stage := range []string{"receipt", "quota"} {
		t.Run(stage, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if stage == "receipt" {
				if n, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 100); err != nil || n != 1 {
					t.Fatalf("prune=%d err=%v", n, err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			visits := 0
			stop := func() { visits++; cancel() }
			hooks := &directIntegrityHooks{}
			if stage == "receipt" {
				hooks.receipt = stop
			} else {
				hooks.quota = stop
			}
			from := event.Timestamp.Truncate(time.Hour)
			to := from.Add(time.Hour - time.Nanosecond)
			got, err := store.readCostUnits(ctx, usage.Filter{ClientName: event.ClientName, From: &from, To: &to}, replayCosts, false, nil, costReadHooks{direct: hooks})
			if !errors.Is(err, context.Canceled) || len(got.units) != 0 || visits != 1 {
				t.Fatalf("result=%+v err=%v visits=%d", got, err, visits)
			}
			if err := db.Update(func(*bolt.Tx) error { return nil }); err != nil {
				t.Fatalf("write after canceled read: %v", err)
			}
		})
	}
}

func TestCostCancellationTakesPriorityOverCorruptRaw(t *testing.T) {
	db := testDB(t)
	at := time.Date(2026, 9, 20, 0, 10, 0, 0, time.UTC)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(at, "bad")), []byte("{broken"))
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	from, to := at.Truncate(time.Hour), at.Truncate(time.Hour).Add(time.Hour-time.Nanosecond)
	got, err := NewUsageStore(db).readCostUnits(ctx, usage.Filter{ClientName: "alice", From: &from, To: &to}, replayCosts, false, nil, costReadHooks{direct: &directIntegrityHooks{raw: cancel}})
	if !errors.Is(err, context.Canceled) || len(got.units) != 0 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestCostCancellationStopsOwnerEvidenceScan(t *testing.T) {
	db := testDB(t)
	at := time.Date(2026, 9, 20, 0, 10, 0, 0, time.UTC)
	if err := db.Update(func(tx *bolt.Tx) error {
		for i := 0; i < 100; i++ {
			event := usage.Event{Timestamp: at.Add(time.Duration(i) * time.Second), RecordID: fmt.Sprintf("record-%03d", i), ClientName: "alice", Model: "qwen-max"}
			key := []byte(usageKey(event.Timestamp, event.RecordID))
			value, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if err := tx.Bucket([]byte(BucketUsageEvents)).Put(key, value); err != nil {
				return err
			}
			if err := writeRawOwner(tx, key, event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelOnCheck{Context: base, cancel: cancel, left: 4}
	from := at.Truncate(time.Hour)
	err := db.View(func(tx *bolt.Tx) error {
		return checkDirectQuotaEvidenceContext(ctx, tx, usage.Filter{ClientName: "alice"}, from, from, nil, nil)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("owner loop did not stop: %v", err)
	}
}

func TestCostCancellationStopsFreshAndUnknownEvidenceLoops(t *testing.T) {
	for _, kind := range []string{"fresh", "unresolved"} {
		t.Run(kind, func(t *testing.T) {
			db := testDB(t)
			if err := db.Update(func(tx *bolt.Tx) error {
				name := freshAttestationBucket
				if kind == "unresolved" {
					name = unresolvedBucket
					if err := tx.Bucket([]byte(BucketMeta)).Put([]byte(unresolvedInitialized), []byte("1")); err != nil {
						return err
					}
				}
				b, err := tx.CreateBucketIfNotExists([]byte(name))
				if err != nil {
					return err
				}
				value, err := json.Marshal(unresolvedUsage{Event: usage.Event{ClientName: "bob", Timestamp: time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)}})
				if err != nil {
					return err
				}
				for i := 0; i < 100; i++ {
					if err := b.Put([]byte(fmt.Sprintf("key-%03d", i)), value); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &cancelOnCheck{Context: base, cancel: cancel, left: 4}
			from := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			err := db.View(func(tx *bolt.Tx) error {
				if kind == "fresh" {
					return checkMissingFreshRootsContext(ctx, tx, usage.Filter{ClientName: "alice"}, from, from)
				}
				return checkUnresolvedUsageInScopeContext(ctx, tx, "alice", from, from, "", nil)
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("context cancellation not propagated: %v", err)
			}
		})
	}
}

func TestCostCancellationTakesPriorityOverUnknownUnresolvedEvidence(t *testing.T) {
	db := testDB(t)
	at := time.Date(2026, 9, 20, 0, 10, 0, 0, time.UTC)
	if err := db.Update(func(tx *bolt.Tx) error {
		return putUnresolvedEvidence(tx, unresolvedUsage{Event: usage.Event{Timestamp: at, Model: "qwen-max", Target: "upstream"}, Kind: "unknown"})
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	from := at.Truncate(time.Hour)
	err := db.View(func(tx *bolt.Tx) error {
		return checkUnresolvedUsageInScopeContext(ctx, tx, "alice", from, from, "", func(_, _ string) bool {
			cancel()
			return true
		})
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("unknown evidence hid cancellation: %v", err)
	}
}
