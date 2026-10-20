package nosql

import (
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestDictStoreCRUD(t *testing.T) {
	db := testDB(t)
	dict := NewDictStore(db)

	if _, err := dict.List(MetaKeyJobLevels); err != nil {
		t.Fatalf("list empty dict: %v", err)
	}

	if err := dict.Create(MetaKeyJobLevels, "L7"); err != nil {
		t.Fatalf("create L7: %v", err)
	}
	if err := dict.Create(MetaKeyJobLevels, "L6"); err != nil {
		t.Fatalf("create L6: %v", err)
	}
	if err := dict.Create(MetaKeyJobLevels, "l6"); err == nil {
		t.Fatal("expected case-insensitive duplicate to be rejected")
	}
	if err := dict.Create(MetaKeyJobLevels, ""); err == nil {
		t.Fatal("expected empty name to be rejected")
	}

	entries, err := dict.List(MetaKeyJobLevels)
	if err != nil {
		t.Fatalf("list dict: %v", err)
	}
	if len(entries) != 2 || entries[0] != "L6" || entries[1] != "L7" {
		t.Fatalf("unexpected entries: %v", entries)
	}

	if err := dict.Update(MetaKeyJobLevels, "L6", "L5"); err != nil {
		t.Fatalf("rename L6 -> L5: %v", err)
	}
	if err := dict.Update(MetaKeyJobLevels, "missing", "L4"); err == nil {
		t.Fatal("expected rename of missing entry to fail")
	}
	if err := dict.Update(MetaKeyJobLevels, "L5", "L7"); err == nil {
		t.Fatal("expected rename onto existing entry to fail")
	}

	if err := dict.Delete(MetaKeyJobLevels, "L5"); err != nil {
		t.Fatalf("delete L5: %v", err)
	}
	if err := dict.Delete(MetaKeyJobLevels, "L5"); err == nil {
		t.Fatal("expected double delete to fail")
	}

	entries, err = dict.List(MetaKeyJobLevels)
	if err != nil {
		t.Fatalf("list dict after delete: %v", err)
	}
	if len(entries) != 1 || entries[0] != "L7" {
		t.Fatalf("unexpected entries after delete: %v", entries)
	}
}

func TestDictStoreDictionariesAreIndependent(t *testing.T) {
	db := testDB(t)
	dict := NewDictStore(db)

	if err := dict.Create(MetaKeyJobLevels, "L6"); err != nil {
		t.Fatalf("create job level: %v", err)
	}
	if err := dict.Create(MetaKeyProjectGroups, "platform"); err != nil {
		t.Fatalf("create project group: %v", err)
	}

	levels, err := dict.List(MetaKeyJobLevels)
	if err != nil {
		t.Fatalf("list job levels: %v", err)
	}
	groups, err := dict.List(MetaKeyProjectGroups)
	if err != nil {
		t.Fatalf("list project groups: %v", err)
	}
	if len(levels) != 1 || levels[0] != "L6" || len(groups) != 1 || groups[0] != "platform" {
		t.Fatalf("dictionaries bleed into each other: levels=%v groups=%v", levels, groups)
	}
}

func TestDictStoreCorruptDataReturnsError(t *testing.T) {
	db := testDB(t)
	dict := NewDictStore(db)

	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketMeta)).Put([]byte(MetaKeyJobLevels), []byte("not-json"))
	}); err != nil {
		t.Fatalf("seed corrupt dictionary: %v", err)
	}

	if _, err := dict.List(MetaKeyJobLevels); err == nil || !strings.Contains(err.Error(), "decode dictionary") {
		t.Fatalf("List should surface the corrupt dictionary error, got %v", err)
	}
	if err := dict.Create(MetaKeyJobLevels, "L6"); err == nil || !strings.Contains(err.Error(), "decode dictionary") {
		t.Fatalf("Create should not overwrite corrupt dictionary, got %v", err)
	}
	if err := dict.Update(MetaKeyJobLevels, "L6", "L5"); err == nil || !strings.Contains(err.Error(), "decode dictionary") {
		t.Fatalf("Update should not overwrite corrupt dictionary, got %v", err)
	}
	if err := dict.Delete(MetaKeyJobLevels, "L6"); err == nil || !strings.Contains(err.Error(), "decode dictionary") {
		t.Fatalf("Delete should not overwrite corrupt dictionary, got %v", err)
	}

	// The corrupted raw value must remain untouched after the rejected writes.
	if err := db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketMeta)).Get([]byte(MetaKeyJobLevels))
		if string(raw) != "not-json" {
			t.Fatalf("corrupt dictionary value was modified: %q", raw)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify raw value: %v", err)
	}
}
