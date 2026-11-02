package nosql

import (
	"testing"

	bolt "go.etcd.io/bbolt"
)

// walkScoped is documented to visit every key in key order. Its odd-key input
// is not sorted by its callers: the persisted index returns global entries in
// index order and rawOddKeys/receiptOddKeys append the per-hour lists by
// iterating the requested-hours map, so the hour blocks arrive in map order.
// The walk must restore key order itself. A walk that trusts the input order
// still visits every key once, but an order-sensitive callback (for example
// one that stops at the first failing row) would then make the verdict depend
// on map iteration.
func TestWalkScopedVisitsOddKeysInKeyOrder(t *testing.T) {
	db := testDB(t)
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte("walk"))
		if err != nil {
			return err
		}
		for _, k := range []string{"a", "b1", "b2", "c", "d"} {
			if err := b.Put([]byte(k), []byte("v")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	odd := [][]byte{[]byte("d"), []byte("a"), []byte("c")}
	want := []string{"a", "b1", "b2", "c", "d"}
	var got []string
	if err := db.View(func(tx *bolt.Tx) error {
		return walkScoped(tx.Bucket([]byte("walk")), []string{"b"}, odd, func(k, _ []byte) error {
			got = append(got, string(k))
			return nil
		})
	}); err != nil {
		t.Fatalf("walkScoped: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("visited %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("visited %v, want key order %v", got, want)
		}
	}
}
