package nosql

import (
	"errors"
	"reflect"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func issue72ReadTransactionID(t *testing.T, db *bolt.DB) int {
	t.Helper()
	var id int
	if err := db.View(func(tx *bolt.Tx) error {
		id = tx.ID()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// An identity lookup may inspect legacy coverage, but must not migrate it or
// publish evidence: the caller may decide not to write the event at all.
func TestIssue72RecordExistsDoesNotWrite(t *testing.T) {
	for _, tc := range []struct {
		name      string
		corrupt   bool
		committed bool
		ready     bool
	}{
		{name: "new_identity"},
		{name: "unverified_identity", corrupt: true},
		{name: "known_identity", committed: true},
		{name: "already_migrated", ready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := boundEvent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaab71", boundScanTime)
			if tc.committed {
				if err := store.Record(event); err != nil {
					t.Fatal(err)
				}
			}
			if tc.corrupt {
				future := boundScanTime.Add(48 * time.Hour).Truncate(time.Hour)
				putAggRaw(t, db, future.Format(hourLayout)+"|claude|alice|executor|anthropic", []byte(corruptCell))
			}
			if tc.ready {
				if err := store.EnsureUsageReceipts(); err != nil {
					t.Fatal(err)
				}
			} else {
				oldWatermarkDone(t, db, time.Time{})
			}
			store = clockedStore(db)
			beforeID := issue72ReadTransactionID(t, db)
			beforeMeta := indexBucketSnapshot(t, db, BucketMeta)
			exists, err := store.RecordExists(event)
			if tc.corrupt {
				if exists || !errors.Is(err, ErrUsageIdentityUnverified) {
					t.Errorf("unverified lookup: exists=%v err=%v", exists, err)
				}
			} else if exists != tc.committed || err != nil {
				t.Errorf("lookup: exists=%v err=%v", exists, err)
			}
			if after := issue72ReadTransactionID(t, db); after != beforeID {
				t.Errorf("lookup committed a write transaction: before=%d after=%d", beforeID, after)
			}
			if after := indexBucketSnapshot(t, db, BucketMeta); !reflect.DeepEqual(beforeMeta, after) {
				t.Errorf("lookup changed migration metadata: before=%v after=%v", beforeMeta, after)
			}
		})
	}
}
