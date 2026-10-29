package nosql

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func etagTarget(name string) config.Target {
	return config.Target{Name: name, Endpoint: "https://" + name + ".example.com", ResourcePathPrefix: "/", APIKey: "k-" + name, UsageAdapter: config.UsageAdapterStandard}
}

func mustETag(t *testing.T, s *TargetStore, name string) string {
	t.Helper()
	etag, err := s.ETag(name)
	if err != nil {
		t.Fatalf("ETag(%q): %v", name, err)
	}
	return etag
}

// putRawTarget 模拟旧二进制或外部工具直接改写 targets 字节。
func putRawTarget(t *testing.T, db *bolt.DB, key string, data []byte) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketTargets)).Put([]byte(key), data); err != nil {
			return err
		}
		return updateTargetOrder(tx, func(order []string) []string { return appendMissingTargetOrder(order, key) })
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTargetETagIsContentHashOfStoredBytes(t *testing.T) {
	s := NewTargetStore(testDB(t))
	if got := mustETag(t, s, "missing"); got != "" {
		t.Fatalf("missing target ETag = %q, want empty", got)
	}
	if err := s.Create(etagTarget("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(etagTarget("b")); err != nil {
		t.Fatal(err)
	}
	ea, eb := mustETag(t, s, "a"), mustETag(t, s, "B")
	if !strings.HasPrefix(ea, "t1-") || len(ea) != len("t1-")+22 || ea == eb {
		t.Fatalf("etags a=%q b=%q", ea, eb)
	}
	if mustETag(t, s, "A") != ea {
		t.Fatal("name lookup must be case-insensitive and stable")
	}
	next := etagTarget("a")
	next.Endpoint = "https://changed.example.com"
	newETag, err := s.UpdateIfMatch("a", next, ea)
	if err != nil || newETag == ea || newETag != mustETag(t, s, "a") {
		t.Fatalf("UpdateIfMatch: %q err=%v stored=%q", newETag, err, mustETag(t, s, "a"))
	}
	if got := mustETag(t, s, "b"); got != eb {
		t.Fatalf("updating a changed b: %q -> %q", eb, got)
	}
	all, err := s.ETags()
	if err != nil || len(all) != 2 || all["a"] != newETag || all["b"] != eb {
		t.Fatalf("ETags() = %v err=%v", all, err)
	}
}

func TestTargetIfMatchConflictLeavesStoreUntouched(t *testing.T) {
	s := NewTargetStore(testDB(t))
	if err := s.Create(etagTarget("a")); err != nil {
		t.Fatal(err)
	}
	etag := mustETag(t, s, "a")
	winner := etagTarget("a")
	winner.Endpoint = "https://winner.example.com"
	newETag, err := s.UpdateIfMatch("a", winner, etag)
	if err != nil {
		t.Fatal(err)
	}
	loser := etagTarget("a")
	loser.Endpoint = "https://loser.example.com"
	if _, err := s.UpdateIfMatch("a", loser, etag); !errors.Is(err, ErrTargetETagConflict) {
		t.Fatalf("stale CAS error = %v", err)
	}
	if err := s.DeleteIfMatch("a", etag); !errors.Is(err, ErrTargetETagConflict) {
		t.Fatalf("stale delete error = %v", err)
	}
	targets, err := s.List()
	if err != nil || len(targets) != 1 || targets[0].Endpoint != winner.Endpoint {
		t.Fatalf("conflict changed the store: %+v err=%v", targets, err)
	}
	if err := s.DeleteIfMatch("a", newETag); err != nil {
		t.Fatalf("matching delete: %v", err)
	}
	if _, err := s.UpdateIfMatch("a", winner, newETag); !errors.Is(err, ErrTargetETagConflict) {
		t.Fatalf("CAS on deleted target error = %v", err)
	}
	if err := s.DeleteIfMatch("a", newETag); !errors.Is(err, ErrTargetETagConflict) {
		t.Fatalf("delete of deleted target error = %v", err)
	}
}

// 存量库：目标从未经本版本代码写入，也必须有并发保护（两个编辑者只有一个成功）。
func TestTargetETagProtectsLegacyRowsFromFirstWrite(t *testing.T) {
	db := testDB(t)
	s := NewTargetStore(db)
	legacy := etagTarget("old")
	legacy.EndpointType = "azure_openai"
	data, _ := json.Marshal(legacy)
	putRawTarget(t, db, "old", data)

	etag := mustETag(t, s, "old")
	if etag == "" {
		t.Fatal("legacy row has no ETag")
	}
	a, b := legacy, legacy
	a.Endpoint = "https://editor-a.example.com"
	b.Endpoint = "https://editor-b.example.com"
	if _, err := s.UpdateIfMatch("old", a, etag); err != nil {
		t.Fatalf("first editor: %v", err)
	}
	if _, err := s.UpdateIfMatch("old", b, etag); !errors.Is(err, ErrTargetETagConflict) {
		t.Fatalf("second editor must conflict, got %v", err)
	}
}

// 旧二进制回滚后直接改写 targets 字节：旧 ETag 必须失效。
func TestTargetETagDetectsOutOfBandRewrite(t *testing.T) {
	db := testDB(t)
	s := NewTargetStore(db)
	if err := s.Create(etagTarget("a")); err != nil {
		t.Fatal(err)
	}
	etag := mustETag(t, s, "a")
	rewritten := etagTarget("a")
	rewritten.Endpoint = "https://rolled-back.example.com"
	data, _ := json.Marshal(rewritten)
	putRawTarget(t, db, "a", data)
	if mustETag(t, s, "a") == etag {
		t.Fatal("rewrite did not change the ETag")
	}
	if _, err := s.UpdateIfMatch("a", etagTarget("a"), etag); !errors.Is(err, ErrTargetETagConflict) {
		t.Fatalf("stale ETag after out-of-band rewrite = %v", err)
	}
}

// List 把旧格式规范化回写后内容确实变了，旧 ETag 被拒是正确行为。
func TestListWritebackChangesETag(t *testing.T) {
	db := testDB(t)
	s := NewTargetStore(db)
	putRawTarget(t, db, "old", []byte(`{"name":"old","endpoint":"https://old.example.com","api_key":"  padded  ","usage_adapter":"standard"}`))
	before := mustETag(t, s, "old")
	targets, err := s.List()
	if err != nil || len(targets) != 1 || targets[0].APIKey != "padded" {
		t.Fatalf("List: %+v err=%v", targets, err)
	}
	after := mustETag(t, s, "old")
	if after == before {
		t.Fatal("normalising writeback must change the ETag")
	}
	if _, err := s.UpdateIfMatch("old", targets[0], before); !errors.Is(err, ErrTargetETagConflict) {
		t.Fatalf("pre-writeback ETag must be rejected: %v", err)
	}
	if _, err := s.UpdateIfMatch("old", targets[0], after); err != nil {
		t.Fatalf("post-writeback ETag must match: %v", err)
	}
}

func TestOpenDBDoesNotCreateRevisionBucket(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "nb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("target_revisions")) != nil {
			t.Fatal("target_revisions bucket must not be created")
		}
		return nil
	})
}

func TestKeyIDSecretPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewTargetStore(db).KeyIDSecret()
	if err != nil || len(first) != 32 || bytes.Equal(first, make([]byte, 32)) {
		t.Fatalf("secret = %x err=%v", first, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	second, err := NewTargetStore(db).KeyIDSecret()
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("secret changed across reopen: %x -> %x err=%v", first, second, err)
	}
}
