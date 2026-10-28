package nosql

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func TestUnresolvedProofCancellationStopsDeepScans(t *testing.T) {
	for _, scenario := range []string{"raw", "receipt", "retained_proof", "committed_scope"} {
		t.Run(scenario, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			if err := store.Record(event); err != nil {
				t.Fatal(err)
			}
			if scenario == "receipt" {
				other := event
				other.ClientName = "bob"
				other.Timestamp = other.Timestamp.Add(-48 * time.Hour)
				other.RecordID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1"
				if err := store.Record(other); err != nil {
					t.Fatal(err)
				}
				if n, err := store.PruneEventsBefore(other.Timestamp.Add(time.Hour), 100); n != 1 || err != nil {
					t.Fatalf("prune: %d %v", n, err)
				}
			}
			if scenario == "committed_scope" {
				conflict := event
				conflict.Model = "other"
				data, err := json.Marshal(conflict)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.RegisterUnresolvedUsage(conflict, data); err != nil {
					t.Fatal(err)
				}
			} else {
				data, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Update(func(tx *bolt.Tx) error {
					return putUnresolvedEvidence(tx, unresolvedUsage{Event: event, Original: data, Resolved: true})
				}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "retained_proof" {
				if n, err := store.PruneEventsBefore(event.Timestamp.Add(time.Hour), 100); n != 1 || err != nil {
					t.Fatalf("prune: %d %v", n, err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			visits := 0
			hooks := &directIntegrityHooks{}
			step := func() { visits++; cancel() }
			if scenario == "raw" || scenario == "committed_scope" {
				hooks.raw = step
			} else {
				hooks.receipt = step
			}
			from, to := event.Timestamp.Truncate(time.Hour), event.Timestamp.Add(time.Hour)
			ctx = context.WithValue(ctx, proofScanHooksKey{}, hooks)
			filter := usage.Filter{ClientName: event.ClientName, From: &from, To: &to}
			if scenario == "committed_scope" {
				filter.Model = event.Model
			}
			units, err := store.ReadCostUnits(ctx, filter, replayCosts, false)
			if len(units) != 0 || !errors.Is(err, context.Canceled) || visits != 1 {
				t.Fatalf("cancellation: visits=%d units=%v err=%v", visits, units, err)
			}
			if err := db.Update(func(*bolt.Tx) error { return nil }); err != nil {
				t.Fatalf("view not released: %v", err)
			}
		})
	}
}
