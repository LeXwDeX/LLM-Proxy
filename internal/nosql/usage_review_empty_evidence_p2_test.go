package nosql

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestIssue72PruneEmptyRawPreservesBothEvidenceCopies(t *testing.T) {
	db := testDB(t)
	store := clockedStore(db)
	event := fixedPlainEvent()
	if err := store.Record(event); err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(event.Timestamp, event.RecordID))
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsageEvents)).Put(key, []byte{})
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneEventsBefore(event.Timestamp.Add(2*time.Second), 1); n != 1 || err != nil {
		t.Fatalf("prune: %d %v", n, err)
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.View(func(tx *bolt.Tx) error {
		var quarantine usageQuarantineRecord
		if err := json.Unmarshal(tx.Bucket([]byte(BucketUsageEventsQuarantine)).Get(key), &quarantine); err != nil {
			return err
		}
		if quarantine.RawValue == nil || len(quarantine.RawValue) != 0 {
			t.Errorf("quarantine is not a non-nil empty value: %v", quarantine.RawValue)
		}
		b := tx.Bucket([]byte(unresolvedBucket))
		if b == nil {
			t.Error("unresolved evidence missing")
			return nil
		}
		found := false
		if err := b.ForEach(func(_, raw []byte) error {
			var item unresolvedUsage
			if err := json.Unmarshal(raw, &item); err != nil {
				return err
			}
			if item.Kind == "prune_unverified" && bytes.Equal(item.OriginalKey, key) {
				found = true
				if item.Original == nil || !bytes.Equal(item.Original, quarantine.RawValue) || len(item.Original) != 0 {
					t.Errorf("unresolved bytes differ from quarantine: %q", item.Original)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if !found {
			t.Error("unresolved raw key missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIssue72EmptyEvidenceAndNilFallbackAreIdempotent(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(map[bool]string{true: "empty", false: "nil_fallback"}[empty], func(t *testing.T) {
			db := testDB(t)
			store := clockedStore(db)
			event := fixedPlainEvent()
			var original []byte
			if empty {
				original = []byte{}
			}
			for i := 0; i < 2; i++ {
				if err := store.RegisterUnresolvedUsage(event, original); err != nil {
					t.Fatal(err)
				}
			}
			path := db.Path()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := OpenDB(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := clockedStore(db).RegisterUnresolvedUsage(event, original); err != nil {
				t.Fatal(err)
			}
			if err := db.View(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(unresolvedBucket))
				count := 0
				if err := b.ForEach(func(_, data []byte) error {
					count++
					var item unresolvedUsage
					if err := json.Unmarshal(data, &item); err != nil {
						return err
					}
					if empty && (item.Original == nil || len(item.Original) != 0) {
						t.Errorf("explicit empty changed: %q", item.Original)
					}
					if !empty && (len(item.Original) == 0 || !json.Valid(item.Original)) {
						t.Errorf("nil fallback missing: %q", item.Original)
					}
					if count > 1 {
						t.Error("repeated registration duplicated evidence")
					}
					return nil
				}); err != nil {
					return err
				}
				if count != 1 {
					t.Errorf("evidence count: %d", count)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
