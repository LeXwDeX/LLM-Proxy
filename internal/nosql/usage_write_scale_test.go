package nosql

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

// legacyIndexReferencesKey is the pre-#120 reference: scan every value of the
// client index. The optimized lookup must answer exactly the same.
func legacyIndexReferencesKey(index *bolt.Bucket, key []byte) bool {
	hit := false
	_ = index.ForEach(func(_, value []byte) error {
		if bytes.Equal(value, key) {
			hit = true
		}
		return nil
	})
	return hit
}

func scaleEvent(i int, base time.Time) usage.Event {
	return usage.Event{
		Timestamp: base.Add(time.Duration(i) * 50 * time.Millisecond), ClientName: fmt.Sprintf("c%02d", i%40),
		EndpointType: "openai", Model: []string{"qwen-max", "glm-max", "deepseek-chat", "gpt-x"}[i%4], Target: fmt.Sprintf("t%d", i%6),
		InputTokens: int64(1000 + i%5000), OutputTokens: int64(100 + i%700), StatusCode: 200,
		RequestID: fmt.Sprintf("req-%d", i), RecordID: fmt.Sprintf("00000000-0000-4000-8000-%012x", i), Path: "/v1/chat/completions",
	}
}

// 新事件身份判定不得随保留事件总数线性增长：4k 与 30k 保留事件下，
// 单批 64 条的写事务耗时比值不超过 2 倍。
func TestRecordBatchTimeDoesNotScaleWithRetainedEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	db := testDB(t)
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour)
	next := 0
	fill := func(target int) {
		for next < target {
			n := min(512, target-next)
			batch := make([]usage.Event, n)
			for k := range batch {
				batch[k] = scaleEvent(next, base)
				next++
			}
			if err := store.RecordBatch(batch); err != nil {
				t.Fatal(err)
			}
		}
	}
	measure := func() time.Duration {
		var d []time.Duration
		for round := 0; round < 21; round++ {
			batch := make([]usage.Event, 64)
			for k := range batch {
				batch[k] = scaleEvent(next, base)
				next++
			}
			start := time.Now()
			if err := store.RecordBatch(batch); err != nil {
				t.Fatal(err)
			}
			d = append(d, time.Since(start))
		}
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[len(d)/2]
	}
	fill(4000)
	small := measure()
	fill(30000)
	large := measure()
	t.Logf("batch(64) p50: 4k=%v 30k=%v ratio=%.2f", small, large, float64(large)/float64(small))
	if large > 2*small {
		t.Fatalf("batch write time scales with retained events: 4k=%v 30k=%v", small, large)
	}
}

func TestClientIndexReferencesKeyMatchesLegacyScan(t *testing.T) {
	rng := rand.New(rand.NewSource(120))
	// "a" and "ab" hex-encode to prefixes of each other; empty and unicode
	// clients exercise separator and ordering corner cases.
	clients := []string{"a", "ab", "abc", "alice", "bob", "中文", " spaced ", "c01", "c0"}
	base := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for round := 0; round < 60; round++ {
		db := testDB(t)
		var keys [][]byte
		err := db.Update(func(tx *bolt.Tx) error {
			index := tx.Bucket([]byte(BucketUsageEventsByClient))
			for i, n := 0, rng.Intn(40); i < n; i++ {
				ts := base.Add(time.Duration(rng.Intn(50)) * 250 * time.Millisecond)
				e := usage.Event{ClientName: clients[rng.Intn(len(clients))], Timestamp: ts}
				rawKey := []byte(usageKey(ts, fmt.Sprintf("id-%d", rng.Intn(20))))
				keys = append(keys, rawKey)
				switch rng.Intn(10) {
				case 0: // malformed entry without client separator
					if err := index.Put([]byte(fmt.Sprintf("zz%d", rng.Intn(5))), rawKey); err != nil {
						return err
					}
				case 1: // non-canonical timestamp part
					if err := index.Put([]byte(clientIndexPrefix(e.ClientName)+"bogus|"+string(rawKey)), rawKey); err != nil {
						return err
					}
				default:
					if err := index.Put(clientEventIndexKey(e, rawKey), rawKey); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		probes := append([][]byte{}, keys...)
		probes = append(probes, []byte("garbage"), []byte(""), []byte("2026-10-29T12:00:00Z_id-1"), []byte(usageKey(base, "id-1")), []byte(usageKey(base.Add(time.Hour), "never")))
		_ = db.View(func(tx *bolt.Tx) error {
			index := tx.Bucket([]byte(BucketUsageEventsByClient))
			for _, probe := range probes {
				want := legacyIndexReferencesKey(index, probe)
				got, err := clientIndexReferencesKey(index, probe)
				if err != nil {
					t.Fatal(err)
				}
				// Canonical entries must always agree. Entries with a
				// deliberately non-canonical timestamp part are the only
				// shape the keyed probe cannot see; the scan reference must
				// not report anything the probe misses for canonical data.
				if got != want && !hasNonCanonicalEntry(index, probe) {
					t.Fatalf("round %d key %q: probe=%v legacy=%v", round, probe, got, want)
				}
			}
			return nil
		})
	}
}

func hasNonCanonicalEntry(index *bolt.Bucket, key []byte) bool {
	found := false
	_ = index.ForEach(func(k, v []byte) error {
		if bytes.Equal(v, key) && !bytes.HasSuffix(k, []byte("Z|"+string(key))) {
			found = true
		}
		return nil
	})
	return found
}

func seedScaleStore(t testing.TB, n int, base time.Time) (*bolt.DB, *UsageStore) {
	t.Helper()
	db := testDB(t)
	store := NewUsageStore(db)
	for next := 0; next < n; {
		m := min(512, n-next)
		batch := make([]usage.Event, m)
		for k := range batch {
			batch[k] = scaleEvent(next, base)
			batch[k].Timestamp = base.Add(time.Duration(next) * 7 * time.Second)
			next++
		}
		if err := store.RecordBatch(batch); err != nil {
			t.Fatal(err)
		}
	}
	return db, store
}

type pruneSnapshot struct {
	raw, receipts, owners []string
	aggs                  map[string]string
}

func snapshotPruneState(t testing.TB, db *bolt.DB) pruneSnapshot {
	t.Helper()
	snap := pruneSnapshot{aggs: map[string]string{}}
	if err := db.View(func(tx *bolt.Tx) error {
		collect := func(name string, dst *[]string) {
			_ = tx.Bucket([]byte(name)).ForEach(func(k, v []byte) error {
				*dst = append(*dst, string(k)+"="+string(v))
				return nil
			})
		}
		collect(BucketUsageEvents, &snap.raw)
		collect(BucketUsageReceipts, &snap.receipts)
		collect(BucketUsageEventsByClient, &snap.owners)
		for _, name := range []string{BucketUsageAggHourly, BucketQuotaUsageAggHourly, rollupProofBucket, BucketUsageClientHours} {
			if b := tx.Bucket([]byte(name)); b != nil {
				_ = b.ForEach(func(k, v []byte) error {
					snap.aggs[name+"|"+string(k)] = string(v)
					return nil
				})
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return snap
}

// 清理少量旧事件时，证明捕获只解码相关小时的原始行，解码量不随保留事件总数增长。
func TestPruneProofDecodesOnlyRelevantRange(t *testing.T) {
	base := time.Now().UTC().Add(-90 * 24 * time.Hour).Truncate(time.Hour)
	decodes := func(retained int) int64 {
		db, store := seedScaleStore(t, retained, base)
		if err := warmCanonicalRawRows(db); err != nil { // cold classification is a one-off, outside the writer
			t.Fatal(err)
		}
		before := proofRawParses.Load()
		deleted, err := store.PruneEventsBefore(base.Add(2*time.Minute), 100)
		if err != nil {
			t.Fatal(err)
		}
		if deleted == 0 || deleted > 100 {
			t.Fatalf("unexpected deleted count %d", deleted)
		}
		return proofRawParses.Load() - before
	}
	small, large := decodes(4000), decodes(16000)
	t.Logf("proof decodes while pruning: 4k retained=%d, 16k retained=%d", small, large)
	if large > small+small/2+500 {
		t.Fatalf("prune proof decodes scale with retained history: 4k=%d 16k=%d", small, large)
	}
}

// 按耗时限额让出写锁：每个事务只处理预算内的事件，多次事务合计结果与单事务一致。
func TestPruneYieldsWriteLockWithSameResult(t *testing.T) {
	base := time.Now().UTC().Add(-90 * 24 * time.Hour).Truncate(time.Hour)
	cutoff := base.Add(20 * time.Minute)
	run := func(budget time.Duration) (int, pruneSnapshot, int) {
		oldBudget := pruneTxBudget
		pruneTxBudget = budget
		t.Cleanup(func() { pruneTxBudget = oldBudget })
		db, store := seedScaleStore(t, 1000, base)
		txs := 0
		n, err := store.PruneEventsBefore(cutoff, 5000)
		if err != nil {
			t.Fatal(err)
		}
		_ = db.View(func(tx *bolt.Tx) error { txs = int(tx.ID()); return nil })
		return n, snapshotPruneState(t, db), txs
	}
	nOne, snapOne, txOne := run(time.Hour)
	nMany, snapMany, txMany := run(time.Nanosecond)
	if nOne == 0 || nOne != nMany {
		t.Fatalf("deleted one-tx=%d many-tx=%d", nOne, nMany)
	}
	if txMany <= txOne+5 {
		t.Fatalf("budget did not split the work: tx ids one=%d many=%d", txOne, txMany)
	}
	if fmt.Sprint(snapOne.raw) != fmt.Sprint(snapMany.raw) || fmt.Sprint(snapOne.receipts) != fmt.Sprint(snapMany.receipts) || fmt.Sprint(snapOne.owners) != fmt.Sprint(snapMany.owners) || fmt.Sprint(sortedKV(snapOne.aggs)) != fmt.Sprint(sortedKV(snapMany.aggs)) {
		t.Fatal("pruning in many budget-limited transactions produced different state")
	}
}

func sortedKV(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

type proofVerdict struct {
	count uint64
	xor   [32]byte
	ok    bool
}

// 新旧实现对照：同一数据库状态下，按小时范围跳过与全量扫描必须给出完全一致的
// 判定（含带外注入的非规范行、异常键回执）。
func TestScopedProofScansMatchExhaustiveScans(t *testing.T) {
	rng := rand.New(rand.NewSource(1120))
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	for round := 0; round < 12; round++ {
		db, store := seedScaleStore(t, 300+rng.Intn(300), base)
		// 清理最早的若干行，产生回执。
		if rng.Intn(2) == 0 {
			if _, err := store.PruneEventsBefore(base.Add(time.Duration(5+rng.Intn(20))*time.Minute), 1000); err != nil {
				t.Fatal(err)
			}
		}
		err := db.Update(func(tx *bolt.Tx) error {
			raw := tx.Bucket([]byte(BucketUsageEvents))
			receipts := tx.Bucket([]byte(BucketUsageReceipts))
			for i, n := 0, rng.Intn(4); i < n; i++ {
				at := base.Add(time.Duration(rng.Intn(24*3600)) * time.Second)
				switch rng.Intn(5) {
				case 0: // legacy-looking row with a valid key
					_ = raw.Put([]byte(usageKey(at, fmt.Sprintf("legacy-%d", i))), []byte(fmt.Sprintf(`{"ClientName":"c%02d","Timestamp":%q,"Model":"qwen-max"}`, rng.Intn(40), at.Format(time.RFC3339Nano))))
				case 1: // unparseable key
					_ = raw.Put([]byte(fmt.Sprintf("weird-%d", i)), []byte(`{"client_name":"c01"}`))
				case 2: // garbage bytes
					_ = raw.Put([]byte(usageKey(at, fmt.Sprintf("garbage-%d", i))), []byte("not json"))
				case 3: // receipt with unparseable key
					_ = receipts.Put([]byte(fmt.Sprintf("badreceipt-%d", i)), []byte(`{"version":1,"digest":"x"}`))
				case 4: // canonical-looking row missing from every index
					e := scaleEvent(900000+i, base)
					e.Timestamp = at
					e.RecordID = fmt.Sprintf("00000000-0000-4000-8000-%012x", 900000+i)
					data, _ := json.Marshal(e)
					_ = raw.Put([]byte(usageKey(e.Timestamp, e.RecordID)), data)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		type cellRef struct {
			source string
			key    []byte
		}
		var cells []cellRef
		var bundles [][]usage.Event
		bundleKeys := [][]string{}
		_ = db.View(func(tx *bolt.Tx) error {
			c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
			i := 0
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var e usage.Event
				if json.Unmarshal(v, &e) != nil || usageKey(e.Timestamp, e.RecordID) != string(k) {
					continue
				}
				if i%37 == 0 {
					cells = append(cells, cellRef{BucketUsageAggHourly, aggKey(e.Timestamp, e.EndpointType, e.ClientName, e.Model, e.Target)}, cellRef{BucketQuotaUsageAggHourly, quotaAggKey(e.Timestamp, e.ClientName, e.Target, e.Model)})
					bundles = append(bundles, []usage.Event{e})
					bundleKeys = append(bundleKeys, []string{string(k)})
				}
				i++
			}
			return nil
		})
		evaluate := func(skip bool) ([]proofVerdict, []bool, []bool) {
			proofScopeSkipEnabled = skip
			defer func() { proofScopeSkipEnabled = true }()
			var ids []proofVerdict
			var proven, fresh []bool
			_ = db.View(func(tx *bolt.Tx) error {
				for _, c := range cells {
					n, x, ok := currentRollupIdentities(tx, c.source, c.key, nil)
					ids = append(ids, proofVerdict{n, x, ok})
				}
				for i := range bundles {
					proven = append(proven, usageAccountingRawProven(tx, bundles[i], bundleKeys[i]))
					fresh = append(fresh, freshScopeProven(tx, bundles[i]))
				}
				return nil
			})
			return ids, proven, fresh
		}
		idsRef, provenRef, freshRef := evaluate(false)
		ids, proven, fresh := evaluate(true)
		okIDs, okProven := 0, 0
		for _, v := range idsRef {
			if v.ok {
				okIDs++
			}
		}
		for _, v := range provenRef {
			if v {
				okProven++
			}
		}
		t.Logf("round %d: cells=%d identityOK=%d bundles=%d proven=%d", round, len(idsRef), okIDs, len(provenRef), okProven)
		if fmt.Sprint(ids) != fmt.Sprint(idsRef) || fmt.Sprint(proven) != fmt.Sprint(provenRef) || fmt.Sprint(fresh) != fmt.Sprint(freshRef) {
			t.Fatalf("round %d: scoped scans disagree with exhaustive scans\nidentities scoped=%v exhaustive=%v\nproven scoped=%v exhaustive=%v\nfresh scoped=%v exhaustive=%v", round, ids, idsRef, proven, provenRef, fresh, freshRef)
		}
	}
}
