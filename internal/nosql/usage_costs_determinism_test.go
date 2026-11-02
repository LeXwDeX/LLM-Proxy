package nosql

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Issue #125 P2: validateCostRawHourEvidence must pick its first error by
// sorted hour key. The evidence map is filled in raw-key order but iterated
// randomly, so a map walk made the verdict depend on map iteration order: the
// same unchanged database returned "invalid client index proof" or
// "client/hour evidence disagrees with raw history" at random, and the Fast vs
// Full differential fuzz caught the two reads disagreeing on seed 1021.
func TestValidateCostRawHourEvidenceDeterministicFirstError(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	const client = "bob"
	hourA := integrityFuzzBase.Add(5 * time.Hour)
	hourB := integrityFuzzBase.Add(6 * time.Hour)
	keyA := string(clientHourKey(client, hourA))
	keyB := string(clientHourKey(client, hourB))
	if keyA >= keyB {
		t.Fatalf("test hours do not sort as intended: %q >= %q", keyA, keyB)
	}
	// Hour A carries an invalid proof and hour B a disagreeing hour state, so
	// the walk reaches a fatal error on either key; only the sorted order makes
	// the choice deterministic. keyB is inserted first so a walk that followed
	// the map's insertion order instead of sorting would report keyB's error and
	// fail; Go's walk starts at a random slot, so a two-entry pair usually comes
	// out in insertion order.
	evidence := map[string]*costRawHourEvidence{
		keyB: {client: client, hour: hourB, relevant: true, count: 2},
		keyA: {client: client, hour: hourA, relevant: true, proofInvalid: true},
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		// A completed index keeps the walk on the state comparison instead of
		// the missing-completion branch.
		if err := tx.Bucket([]byte(BucketMeta)).Put([]byte(usageClientIndexDone), []byte("1")); err != nil {
			return err
		}
		state, err := json.Marshal(usageClientHour{Count: 1})
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(BucketUsageClientHours)).Put([]byte(keyB), state)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := "usage history is incomplete for exact pricing: invalid client index proof"
	for i := 0; i < 256; i++ {
		var got string
		if err := db.View(func(tx *bolt.Tx) error {
			if _, err := validateCostRawHourEvidence(ctx, tx, nil, evidence); err != nil {
				got = err.Error()
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("iteration %d: first error %q, want the sorted-first hour's %q", i, got, want)
		}
	}
}
