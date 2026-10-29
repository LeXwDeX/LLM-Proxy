package nosql

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func quotaCellValue(db *bolt.DB, e usage.Event) string {
	var out string
	_ = db.View(func(tx *bolt.Tx) error {
		out = string(tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Get(quotaAggKey(e.Timestamp, e.ClientName, e.Target, e.Model)))
		return nil
	})
	return out
}

func clientIndexMarked(db *bolt.DB) bool {
	marked := false
	_ = db.View(func(tx *bolt.Tx) error { marked = clientIndexNonCanonicalMarked(tx); return nil })
	return marked
}

// 带外删除 raw 与 owner 后，索引项残留。规范形态由前缀探测发现；非规范形态
// （时间戳不同、后缀不同）探测看不到，必须靠启动预热写入标记，随后身份判定
// 退回全量扫描，拒绝重复入账。
func TestClientIndexResidueRejectsDuplicateAfterWarmup(t *testing.T) {
	for _, variant := range []string{"canonical", "other-stamp", "other-suffix"} {
		t.Run(variant, func(t *testing.T) {
			db := testDB(t)
			store := NewUsageStore(db)
			base := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
			e := scaleEvent(1, base)
			if err := store.RecordBatch([]usage.Event{e}); err != nil {
				t.Fatal(err)
			}
			before := quotaCellValue(db, e)
			key := []byte(usageKey(e.Timestamp, e.RecordID))
			if err := db.Update(func(tx *bolt.Tx) error {
				_ = tx.Bucket([]byte(BucketUsageEvents)).Delete(key)
				_ = tx.Bucket([]byte(rawOwnerBucket)).Delete(key)
				idx := tx.Bucket([]byte(BucketUsageEventsByClient))
				switch variant {
				case "other-stamp":
					_ = idx.Delete(clientEventIndexKey(e, key))
					other := e
					other.Timestamp = e.Timestamp.Add(time.Nanosecond)
					return idx.Put(clientEventIndexKey(other, key), key)
				case "other-suffix":
					_ = idx.Delete(clientEventIndexKey(e, key))
					return idx.Put(clientEventIndexKey(e, []byte(string(key)+"x")), key)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if clientIndexMarked(db) {
				t.Fatal("marker set before any startup scan")
			}
			if _, err := WarmUsageProofScanCache(db); err != nil {
				t.Fatal(err)
			}
			if _, err := WarmUsageProofScanCache(db); err != nil { // repeatable
				t.Fatal(err)
			}
			if want := variant != "canonical"; clientIndexMarked(db) != want {
				t.Fatalf("marker = %v, want %v", clientIndexMarked(db), want)
			}
			err := store.RecordBatch([]usage.Event{e})
			if !errors.Is(err, ErrUsageIdentityUnverified) {
				t.Fatalf("re-recording over index residue = %v, want ErrUsageIdentityUnverified", err)
			}
			if after := quotaCellValue(db, e); after != before {
				t.Fatalf("duplicate was charged: before=%s after=%s", before, after)
			}
		})
	}
}

// 没有标记时探测只认规范项：带外破坏在下次启动预热前不可见，预热之后才被拦住。
func TestClientIndexOutOfBandCorruptionIsFoundByNextWarmup(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	e := scaleEvent(7, base)
	if err := store.RecordBatch([]usage.Event{e}); err != nil {
		t.Fatal(err)
	}
	key := []byte(usageKey(e.Timestamp, e.RecordID))
	if err := db.Update(func(tx *bolt.Tx) error {
		idx := tx.Bucket([]byte(BucketUsageEventsByClient))
		_ = idx.Delete(clientEventIndexKey(e, key))
		other := e
		other.Timestamp = e.Timestamp.Add(time.Nanosecond)
		return idx.Put(clientEventIndexKey(other, key), key)
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.View(func(tx *bolt.Tx) error {
		idx := tx.Bucket([]byte(BucketUsageEventsByClient))
		if got, _ := clientIndexReferencesKey(idx, key); got {
			t.Fatal("probe unexpectedly saw the non-canonical entry")
		}
		if !clientIndexScanReferencesKey(idx, key) {
			t.Fatal("scan must see the entry")
		}
		return nil
	})
	if _, err := WarmUsageProofScanCache(db); err != nil {
		t.Fatal(err)
	}
	if !clientIndexMarked(db) {
		t.Fatal("warm-up did not mark the non-canonical index entry")
	}
}

func TestClientIndexEntryCanonicalShapes(t *testing.T) {
	at := time.Date(2026, 10, 29, 12, 0, 0, 5, time.UTC)
	e := usage.Event{ClientName: "alice", Timestamp: at}
	raw := []byte(usageKey(at, "id"))
	good := clientEventIndexKey(e, raw)
	for name, tc := range map[string]struct {
		key, value []byte
		want       bool
	}{
		"canonical":       {good, raw, true},
		"no separator":    {[]byte("zz1"), raw, false},
		"value no stamp":  {good, []byte("noseparator"), false},
		"value bad stamp": {good, []byte("bogus_id"), false},
		"offset stamp":    {good, []byte("2026-10-29T20:00:00.000000005+08:00_id"), false},
		"other stamp":     {clientEventIndexKey(usage.Event{ClientName: "alice", Timestamp: at.Add(1)}, raw), raw, false},
		"other suffix":    {clientEventIndexKey(e, append(append([]byte{}, raw...), 'x')), raw, false},
	} {
		if got := clientIndexEntryCanonical(tc.key, tc.value); got != tc.want {
			t.Errorf("%s: canonical=%v want %v", name, got, tc.want)
		}
	}
}

// 索引里存在没有客户端分隔符的项时，探测必须退回扫描并命中它。
func TestClientIndexProbeFallsBackOnEntryWithoutSeparator(t *testing.T) {
	db := testDB(t)
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	raw := []byte(usageKey(at, "id-1"))
	if err := db.Update(func(tx *bolt.Tx) error {
		idx := tx.Bucket([]byte(BucketUsageEventsByClient))
		if err := idx.Put(clientEventIndexKey(usage.Event{ClientName: "alice", Timestamp: at}, []byte(usageKey(at, "other"))), []byte(usageKey(at, "other"))); err != nil {
			return err
		}
		return idx.Put([]byte("zz-no-separator"), raw)
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.View(func(tx *bolt.Tx) error {
		idx := tx.Bucket([]byte(BucketUsageEventsByClient))
		got, err := clientIndexReferencesKey(idx, raw)
		if err != nil || !got {
			t.Fatalf("probe = %v, %v; want a hit through the scan fallback", got, err)
		}
		return nil
	})
}

// 摘要必须是单射：key 与 data 的拼接相同但切分不同，不能共用记忆项。
func TestProofRowDigestIsInjective(t *testing.T) {
	data1 := []byte(`{"a":1}`)
	key1 := []byte("k\x00y")
	key2 := []byte("k")
	data2 := append([]byte("y\x00"), data1...)
	if proofRowDigest(key1, data1) == proofRowDigest(key2, data2) {
		t.Fatal("digest collides for different (key, data) splits")
	}
}

// 构造 key\x00data 拼接碰撞：损坏行不得复用规范行的记忆项而被范围外跳过。
func TestProofScanDoesNotSkipCorruptRowCollidingWithCanonicalMemo(t *testing.T) {
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	db, _ := seedScaleStore(t, 50, base)
	far := base.Add(30 * time.Hour)
	e := scaleEvent(777, base)
	e.Timestamp = far
	e.RecordID = "x\x00y"
	data1, _ := json.Marshal(e)
	key1 := []byte(usageKey(e.Timestamp, e.RecordID))
	if !decodeProofEvent(data1, &usage.Event{}) {
		t.Fatal("first row is not canonical")
	}
	key2 := []byte(usageKey(e.Timestamp, "x"))
	data2 := append([]byte("y\x00"), data1...)
	if err := db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketUsageEvents))
		_ = raw.Put(key1, data1)
		_ = raw.Put(key2, data2)
		return tx.Bucket([]byte(rawOwnerBucket)).Put(key2, []byte("corrupt"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := warmCanonicalRawRows(db); err != nil {
		t.Fatal(err)
	}
	bundle, bkey := firstCanonicalRow(t, db, e.ClientName)
	eval := func(skip bool) string {
		proofScopeSkipEnabled = skip
		defer func() { proofScopeSkipEnabled = true }()
		var out string
		_ = db.View(func(tx *bolt.Tx) error {
			p := usageAccountingRawProven(tx, []usage.Event{bundle}, []string{bkey})
			n, x, ok := currentRollupIdentities(tx, BucketQuotaUsageAggHourly, quotaAggKey(bundle.Timestamp, bundle.ClientName, bundle.Target, bundle.Model), nil)
			out = fmt.Sprint(p, n, x, ok)
			return nil
		})
		return out
	}
	if a, b := eval(false), eval(true); a != b {
		t.Fatalf("scoped scan disagrees with exhaustive scan: exhaustive=%.60s scoped=%.60s", a, b)
	}
}

func firstCanonicalRow(t *testing.T, db *bolt.DB, client string) (usage.Event, string) {
	t.Helper()
	var bundle usage.Event
	var key string
	_ = db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var ev usage.Event
			if decodeProofEvent(v, &ev) && usageKey(ev.Timestamp, ev.RecordID) == string(k) && ev.ClientName == client {
				bundle, key = ev, string(k)
				return nil
			}
		}
		return nil
	})
	if key == "" {
		t.Fatal("no canonical row")
	}
	return bundle, key
}

// 记忆之后，范围外的规范行被带外改写：改写后的行不得再被跳过。
func TestProofScanMemoDoesNotHideOutOfBandRewrite(t *testing.T) {
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	for variant := 0; variant < 4; variant++ {
		db, _ := seedScaleStore(t, 600, base)
		if err := warmCanonicalRawRows(db); err != nil {
			t.Fatal(err)
		}
		var bundle, victim usage.Event
		var bkey string
		var victimKey []byte
		_ = db.View(func(tx *bolt.Tx) error {
			c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var ev usage.Event
				_ = json.Unmarshal(v, &ev)
				if bkey == "" {
					bundle, bkey = ev, string(k)
				} else if ev.ClientName == bundle.ClientName && ev.Timestamp.Truncate(time.Hour) != bundle.Timestamp.Truncate(time.Hour) {
					victimKey, victim = append([]byte(nil), k...), ev
				}
			}
			return nil
		})
		_ = db.View(func(tx *bolt.Tx) error {
			usageAccountingRawProven(tx, []usage.Event{bundle}, []string{bkey})
			return nil
		})
		if err := db.Update(func(tx *bolt.Tx) error {
			raw := tx.Bucket([]byte(BucketUsageEvents))
			v := victim
			switch variant {
			case 0: // 移入证明范围的小时，键不变
				v.Timestamp = bundle.Timestamp
			case 1: // 破坏规范形态
				d, _ := json.Marshal(v)
				return raw.Put(victimKey, append(d, ' '))
			case 2: // 垃圾内容加损坏的 owner
				_ = tx.Bucket([]byte(rawOwnerBucket)).Put(victimKey, []byte("bad"))
				return raw.Put(victimKey, []byte("nope"))
			case 3: // 改 RecordID
				v.RecordID = "other"
			}
			d, _ := json.Marshal(v)
			return raw.Put(victimKey, d)
		}); err != nil {
			t.Fatal(err)
		}
		eval := func(skip bool) string {
			proofScopeSkipEnabled = skip
			defer func() { proofScopeSkipEnabled = true }()
			var s string
			_ = db.View(func(tx *bolt.Tx) error {
				p := usageAccountingRawProven(tx, []usage.Event{bundle}, []string{bkey})
				n, x, ok := currentRollupIdentities(tx, BucketQuotaUsageAggHourly, quotaAggKey(bundle.Timestamp, bundle.ClientName, bundle.Target, bundle.Model), nil)
				s = fmt.Sprint(p, n, x, ok)
				return nil
			})
			return s
		}
		if a, b := eval(false), eval(true); a != b {
			t.Errorf("variant %d: scoped scan disagrees with exhaustive scan\nexhaustive=%.70s\nscoped=%.70s", variant, a, b)
		}
	}
}

// 清理删除的回执要计入单元的滚动身份：回执被忽略时计数会少。独立于新旧对照，
// 直接对照由造数得出的期望事件数。
func TestRollupIdentityCountsPrunedReceipts(t *testing.T) {
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	events := 700
	if raceEnabled {
		events = 120
	}
	db, store := seedScaleStore(t, events, base)
	expected := map[string]uint64{}
	for i := 0; i < events; i++ {
		e := scaleEvent(i, base)
		e.Timestamp = base.Add(time.Duration(i) * 7 * time.Second)
		expected[string(quotaAggKey(e.Timestamp, e.ClientName, e.Target, e.Model))]++
	}
	if n, err := store.PruneEventsBefore(base.Add(time.Duration(events/28+1)*time.Minute), 1000); err != nil || n == 0 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	checked := 0
	for _, skip := range []bool{false, true} {
		proofScopeSkipEnabled = skip
		_ = db.View(func(tx *bolt.Tx) error {
			for key, want := range expected {
				count, _, ok := currentRollupIdentities(tx, BucketQuotaUsageAggHourly, []byte(key), nil)
				if !ok || count != want {
					t.Errorf("skip=%v cell %q: identities=%d ok=%v, want %d", skip, key, count, ok, want)
					return nil
				}
				checked++
			}
			return nil
		})
	}
	proofScopeSkipEnabled = true
	if checked == 0 {
		t.Fatal("nothing checked")
	}
}

// 清理被预算拆成多个事务时，滚动证明的校验结果在事务之间复用：总的身份重算次数
// 与单事务相当；其他写入方推进了 raw 修订代次后，缓存作废并重新校验。
func TestPruneReusesRollupProofVerdictsAcrossTransactions(t *testing.T) {
	base := time.Now().UTC().Add(-90 * 24 * time.Hour).Truncate(time.Hour)
	// Few distinct client/model/target cells, so most pruned events share the
	// rollup cell of an earlier one in the same call.
	cutoff := base.Add(10 * time.Minute)
	retained := 600
	if raceEnabled {
		retained, cutoff = 250, base.Add(5*time.Minute)
	}
	run := func(budget time.Duration, between func(*UsageStore)) (int, int64, pruneSnapshot) {
		oldBudget, oldHook := pruneTxBudget, pruneBetweenTx
		pruneTxBudget = budget
		t.Cleanup(func() { pruneTxBudget, pruneBetweenTx = oldBudget, oldHook })
		db := testDB(t)
		store := NewUsageStore(db)
		batch := make([]usage.Event, retained)
		for i := range batch {
			batch[i] = scaleEvent(i, base)
			batch[i].ClientName = fmt.Sprintf("c%02d", i%4)
			batch[i].Model, batch[i].Target = "qwen-max", "t0"
			batch[i].Timestamp = base.Add(time.Duration(i) * 7 * time.Second)
		}
		if err := store.RecordBatch(batch); err != nil {
			t.Fatal(err)
		}
		if err := warmCanonicalRawRows(db); err != nil {
			t.Fatal(err)
		}
		if between != nil {
			calls := 0
			pruneBetweenTx = func() {
				if calls++; calls == 3 {
					between(store)
				}
			}
		}
		first := proofIdentityScans.Load()
		var tx0 int
		_ = db.View(func(tx *bolt.Tx) error { tx0 = tx.ID(); return nil })
		n, err := store.PruneEventsBefore(cutoff, 5000)
		if err != nil {
			t.Fatal(err)
		}
		var tx1 int
		_ = db.View(func(tx *bolt.Tx) error { tx1 = tx.ID(); return nil })
		if budget == time.Nanosecond && tx1-tx0 < 20 {
			t.Fatalf("budget did not split the prune: %d transactions", tx1-tx0)
		}
		return n, proofIdentityScans.Load() - first, snapshotPruneState(t, db)
	}
	nOne, scansOne, snapOne := run(time.Hour, nil)
	nMany, scansMany, snapMany := run(time.Nanosecond, nil)
	t.Logf("identity scans: one tx=%d, many tx=%d (deleted %d)", scansOne, scansMany, nOne)
	if nOne == 0 || nOne != nMany || scansOne == 0 {
		t.Fatalf("deleted one=%d many=%d scans one=%d", nOne, nMany, scansOne)
	}
	if scansMany*2 > scansOne*3 {
		t.Fatalf("splitting the prune recomputed proofs: one tx=%d, many tx=%d (limit 1.5x)", scansOne, scansMany)
	}
	if fmt.Sprint(snapOne.raw) != fmt.Sprint(snapMany.raw) || fmt.Sprint(snapOne.receipts) != fmt.Sprint(snapMany.receipts) || fmt.Sprint(sortedKV(snapOne.aggs)) != fmt.Sprint(sortedKV(snapMany.aggs)) {
		t.Fatal("split prune produced different state")
	}
	// 另一个写入方在两个清理事务之间提交：缓存必须作废。
	_, scansInterrupted, _ := run(time.Nanosecond, func(store *UsageStore) {
		e := scaleEvent(900001, base)
		e.Timestamp = base.Add(10 * 24 * time.Hour)
		if err := store.RecordBatch([]usage.Event{e}); err != nil {
			t.Error(err)
		}
	})
	if scansInterrupted <= scansMany {
		t.Fatalf("a foreign write between prune transactions did not invalidate the cache: %d <= %d", scansInterrupted, scansMany)
	}
}

// 规范行记忆按 raw key 保存：覆盖写入替换旧项，清理删除的行被移除。
func TestCanonicalRowMemoFollowsRetainedRows(t *testing.T) {
	size := func() int {
		proofScanCache.RLock()
		defer proofScanCache.RUnlock()
		return len(proofScanCache.canonical)
	}
	base := time.Now().UTC().Add(-90 * 24 * time.Hour).Truncate(time.Hour)
	db, store := seedScaleStore(t, 150, base)
	if err := warmCanonicalRawRows(db); err != nil {
		t.Fatal(err)
	}
	// 同一 key 的不同内容只占一项。
	key := []byte(usageKey(base, "memo-key"))
	e := scaleEvent(1, base)
	e.RecordID = "memo-key"
	e.Timestamp = base
	first, _ := json.Marshal(e)
	e.InputTokens++
	second, _ := json.Marshal(e)
	if !canonicalRawRowCached(key, first) {
		t.Fatal("first row not canonical")
	}
	afterFirst := size()
	if !canonicalRawRowCached(key, second) || size() != afterFirst {
		t.Fatalf("rewriting a key grew the memo: %d -> %d", afterFirst, size())
	}
	if canonicalRawRowCached(key, first) == false || size() != afterFirst {
		t.Fatal("memo entry for the older bytes should be replaced, not accumulated")
	}
	forgetCanonicalRows([][]byte{key})
	// 清理删除的行从记忆中移除。
	beforePrune := size()
	deleted, err := store.PruneEventsBefore(base.Add(5*time.Minute), 1000)
	if err != nil || deleted == 0 {
		t.Fatalf("prune = %d, %v", deleted, err)
	}
	if got := size(); got > beforePrune-deleted+1 {
		t.Fatalf("memo kept pruned rows: before=%d deleted=%d after=%d", beforePrune, deleted, got)
	}
}

// 升级库（未置 tracked 标记）的每个写事务也不得随保留事件总数线性增长。
func TestRecordBatchTimeDoesNotScaleWithRetainedEventsOnUpgradedLedger(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	small, large, rounds := 4000, 30000, 9
	if raceEnabled {
		// Race instrumentation makes wall-clock ratios meaningless; the decode
		// count below is deterministic and is checked at a smaller size.
		small, large, rounds = 400, 1600, 2
	}
	db := testDB(t)
	store := NewUsageStore(db)
	base := time.Now().UTC().Truncate(time.Hour)
	history := base.Add(-300 * time.Hour)
	next, live := 0, 0
	fill := func(target int) {
		for next < target {
			n := min(512, target-next)
			batch := make([]usage.Event, n)
			for k := range batch {
				batch[k] = scaleEvent(next, base)
				batch[k].Timestamp = history.Add(time.Duration(next) * 7 * time.Second)
				next++
			}
			if err := store.RecordBatch(batch); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 每个阶段写入各自独立的当前小时，使进入范围的行数在两阶段相同。
	measure := func(hourOffset time.Duration) (time.Duration, int64) {
		var d []time.Duration
		var parses int64
		for round := 0; round < rounds; round++ {
			batch := make([]usage.Event, 64)
			for k := range batch {
				batch[k] = scaleEvent(1_000_000+live, base)
				batch[k].Timestamp = base.Add(hourOffset).Add(time.Duration(live%1000) * 50 * time.Millisecond)
				live++
			}
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
			}); err != nil {
				t.Fatal(err)
			}
			before := proofRawParses.Load()
			start := time.Now()
			if err := store.RecordBatch(batch); err != nil {
				t.Fatal(err)
			}
			d = append(d, time.Since(start))
			parses = max(parses, proofRawParses.Load()-before)
		}
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[len(d)/2], parses
	}
	fill(small)
	smallTime, smallParses := measure(-5 * time.Hour)
	fill(large)
	largeTime, largeParses := measure(-2 * time.Hour)
	t.Logf("upgraded batch(64) p50: %d=%v %d=%v ratio=%.2f; max decodes per batch %d -> %d", small, smallTime, large, largeTime, float64(largeTime)/float64(smallTime), smallParses, largeParses)
	if largeParses > smallParses+smallParses/2+200 {
		t.Fatalf("upgraded-ledger batch decodes scale with retained events: %d -> %d", smallParses, largeParses)
	}
	if !raceEnabled && largeTime > 2*smallTime {
		t.Fatalf("upgraded-ledger batch time scales with retained events: %v -> %v", smallTime, largeTime)
	}
}

type upgradedVerdict struct {
	fresh, continuation, rawProven, retained bool
	identities                               string
}

// 升级库路径的新旧对照：穷举、仅按小时跳过、按事务摘要限定范围三种方式，对含带外
// 注入行（遗留行、偏移时区键、垃圾、异常回执）的同一库给出完全一致的判定。
func TestUpgradedProofScansMatchExhaustiveScans(t *testing.T) {
	rng := rand.New(rand.NewSource(2120))
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	rounds := 10
	if raceEnabled {
		rounds = 2
	}
	trueCount := 0
	for round := 0; round < rounds; round++ {
		seeded := 250 + rng.Intn(250)
		if raceEnabled {
			seeded = 130 + rng.Intn(80)
		}
		db, store := seedScaleStore(t, seeded, base)
		// Round 0 prunes without injections, which is what yields positive
		// verdicts; later rounds prune at random and inject odd rows.
		if round == 0 || rng.Intn(2) == 0 {
			if _, err := store.PruneEventsBefore(base.Add(time.Duration(5+rng.Intn(25))*time.Minute), 1000); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			if err := tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey)); err != nil {
				return err
			}
			raw := tx.Bucket([]byte(BucketUsageEvents))
			receipts := tx.Bucket([]byte(BucketUsageReceipts))
			injections := rng.Intn(5)
			if round == 0 {
				injections = 0
			}
			for i, n := 0, injections; i < n; i++ {
				at := base.Add(time.Duration(rng.Intn(24*3600)) * time.Second)
				switch rng.Intn(7) {
				case 0:
					_ = raw.Put([]byte(usageKey(at, fmt.Sprintf("legacy-%d", i))), []byte(fmt.Sprintf(`{"ClientName":"c%02d","Timestamp":%q,"Model":"qwen-max"}`, rng.Intn(40), at.Format(time.RFC3339Nano))))
				case 1:
					_ = raw.Put([]byte(fmt.Sprintf("weird-%d", i)), []byte(`{"client_name":"c01"}`))
				case 2:
					_ = raw.Put([]byte(usageKey(at, fmt.Sprintf("garbage-%d", i))), []byte("not json"))
				case 3:
					_ = receipts.Put([]byte(fmt.Sprintf("badreceipt-%d", i)), []byte(`{"version":1,"digest":"x"}`))
				case 4: // 偏移时区的键：数据规范，键不是
					e := scaleEvent(910000+i, base)
					e.Timestamp = at
					e.RecordID = fmt.Sprintf("00000000-0000-4000-8000-%012x", 910000+i)
					data, _ := json.Marshal(e)
					_ = raw.Put([]byte(at.In(time.FixedZone("x", 8*3600)).Format(time.RFC3339Nano)+"_"+e.RecordID), data)
				case 5: // 偏移时区的回执键
					_ = receipts.Put([]byte(at.In(time.FixedZone("x", 8*3600)).Format(time.RFC3339Nano)+"_offset"), []byte(`{"version":1,"digest":"x"}`))
				case 6: // 规范键、缺失所有索引的规范行
					e := scaleEvent(920000+i, base)
					e.Timestamp = at
					e.RecordID = fmt.Sprintf("00000000-0000-4000-8000-%012x", 920000+i)
					data, _ := json.Marshal(e)
					_ = raw.Put([]byte(usageKey(e.Timestamp, e.RecordID)), data)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		stride := 29
		if raceEnabled {
			stride = 7
		}
		type cellRef struct {
			source string
			key    []byte
		}
		var cells []cellRef
		var existing [][]usage.Event
		var existingKeys [][]string
		var candidates [][]usage.Event
		_ = db.View(func(tx *bolt.Tx) error {
			c := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
			i := 0
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var e usage.Event
				if json.Unmarshal(v, &e) != nil || usageKey(e.Timestamp, e.RecordID) != string(k) {
					continue
				}
				if i%stride == 0 {
					cells = append(cells, cellRef{BucketUsageAggHourly, aggKey(e.Timestamp, e.EndpointType, e.ClientName, e.Model, e.Target)}, cellRef{BucketQuotaUsageAggHourly, quotaAggKey(e.Timestamp, e.ClientName, e.Target, e.Model)})
					existing = append(existing, []usage.Event{e})
					existingKeys = append(existingKeys, []string{string(k)})
					fresh := e
					fresh.RecordID = fmt.Sprintf("00000000-0000-4000-8000-%012x", 930000+i)
					fresh.RequestID = fmt.Sprintf("req-new-%d", i)
					fresh.Timestamp = e.Timestamp.Add(time.Millisecond)
					candidates = append(candidates, []usage.Event{fresh})
				}
				i++
			}
			return nil
		})
		evaluate := func(skip, summary bool) ([]upgradedVerdict, int) {
			proofScopeSkipEnabled = skip
			defer func() { proofScopeSkipEnabled = true }()
			var out []upgradedVerdict
			trues := 0
			_ = db.View(func(tx *bolt.Tx) error {
				ctx := t.Context()
				if summary {
					ctx = withTxScopeSummary(ctx, tx)
				}
				for i := range existing {
					var v upgradedVerdict
					v.fresh = freshScopeProvenContext(ctx, tx, candidates[i])
					v.continuation = continuationScopeProvenMemoContext(ctx, tx, candidates[i], nil)
					v.rawProven, _ = usageAccountingRawProvenContext(ctx, tx, existing[i], existingKeys[i])
					v.retained, _ = usageRetainedRollupsProvenContext(ctx, tx, existing[i], existingKeys[i])
					var ids string
					for _, c := range cells[2*i : 2*i+2] {
						n, x, ok, _ := currentRollupIdentitiesContext(ctx, tx, c.source, c.key, nil)
						ids += fmt.Sprint(n, x, ok, ";")
					}
					v.identities = ids
					out = append(out, v)
					for _, b := range []bool{v.fresh, v.continuation, v.rawProven, v.retained} {
						if b {
							trues++
						}
					}
				}
				return nil
			})
			return out, trues
		}
		ref, refTrues := evaluate(false, false)
		skipOnly, _ := evaluate(true, false)
		scoped, _ := evaluate(true, true)
		trueCount += refTrues
		if fmt.Sprint(ref) != fmt.Sprint(skipOnly) || fmt.Sprint(ref) != fmt.Sprint(scoped) {
			t.Fatalf("round %d: scans disagree\nexhaustive=%v\nskip=%v\nscoped=%v", round, ref, skipOnly, scoped)
		}
	}
	t.Logf("true verdicts across rounds: %d", trueCount)
	if trueCount == 0 {
		t.Fatal("no verdict was ever true, the comparison proves nothing")
	}
}

// drain 过滤用追加时记下的内存元数据，不再在持锁期间解码日志行：行内容是垃圾也
// 不影响过滤结果。
func TestRelevantOwnedFilterUsesAppendTimeMetadata(t *testing.T) {
	journal, err := NewUsageJournal(NewUsageStore(testDB(t)), filepath.Join(t.TempDir(), "usage-journal.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	at := time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)
	window := &usageDrainWindow{from: at, to: at.Add(time.Hour)}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.owned["not json at all"] = ownedLine{client: "alice", event: usage.Event{ClientName: "alice", Timestamp: at}, consistent: true}
	journal.owned["\x00\x01 garbage"] = ownedLine{client: "bob", event: usage.Event{ClientName: "bob", Timestamp: at}, consistent: true}
	journal.owned["old"] = ownedLine{client: "alice", event: usage.Event{ClientName: "alice", Timestamp: at.Add(-3 * time.Hour)}, consistent: true}
	journal.owned["odd"] = ownedLine{client: "alice", event: usage.Event{ClientName: "alice ", Timestamp: at}, consistent: false}
	if got := journal.relevantOwnedLocked("alice", window); got != 2 {
		t.Fatalf("relevant owned = %d, want 2 (the current-window line and the inconsistent one)", got)
	}
	if got := journal.relevantOwnedLocked("carol", window); got != 1 {
		t.Fatalf("relevant owned for another client = %d, want only the inconsistent line", got)
	}
}

// 异步写入器运行且没有外来行时，窗口 drain 的收尾检查不读日志文件，也就不会在
// 持锁期间读文件并解码。
func TestWindowDrainFinalCheckDoesNotReadJournalFile(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	journal, err := NewUsageJournal(store, filepath.Join(t.TempDir(), "usage-journal.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if err := journal.EnableAsyncCommit(UsageJournalAsyncOptions{}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Hour).Add(-30 * time.Minute)
	for i := 0; i < 5; i++ {
		e := windowFixture(fmt.Sprintf("bbbbbbbb-bbbb-4bbb-8bbb-%012x", i), at.Add(time.Duration(i)*time.Second), "alice")
		if err := journal.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	journal.Flush()
	reads := journal.fileReads.Load()
	if err := journal.DrainClientRange("alice", at.Add(-time.Hour), at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := journal.fileReads.Load(); got != reads {
		t.Fatalf("window drain read the journal file %d times under the lock", got-reads)
	}
}

// 单条写入路径（没有事务摘要）在升级库上同样只解码证明范围内的行：解码量不随
// 保留的历史增长。
func TestSingleRecordProofDecodesOnlyRelevantRangeOnUpgradedLedger(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Hour)
	history := base.Add(-300 * time.Hour)
	decodes := func(retained int) int64 {
		db := testDB(t)
		store := NewUsageStore(db)
		for next := 0; next < retained; {
			n := min(512, retained-next)
			batch := make([]usage.Event, n)
			for k := range batch {
				batch[k] = scaleEvent(next, base)
				batch[k].Timestamp = history.Add(time.Duration(next) * 7 * time.Second)
				next++
			}
			if err := store.RecordBatch(batch); err != nil {
				t.Fatal(err)
			}
		}
		untrack := func() {
			if err := db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(BucketMeta)).Delete([]byte(usageAccountingProofTrackedKey))
			}); err != nil {
				t.Fatal(err)
			}
		}
		live := func(i int) usage.Event {
			e := scaleEvent(2_000_000+i, base)
			e.ClientName = "live"
			e.Timestamp = base.Add(-time.Hour).Add(time.Duration(i) * time.Second)
			return e
		}
		untrack()
		if err := store.Record(live(0)); err != nil { // first row of the client/hour
			t.Fatal(err)
		}
		if err := warmCanonicalRawRows(db); err != nil {
			t.Fatal(err)
		}
		untrack()
		before := proofRawParses.Load()
		if err := store.Record(live(1)); err != nil { // continues the attested hour
			t.Fatal(err)
		}
		return proofRawParses.Load() - before
	}
	smallN, largeN := 3000, 12000
	if raceEnabled {
		smallN, largeN = 700, 2800
	}
	small, large := decodes(smallN), decodes(largeN)
	t.Logf("single Record proof decodes: 3k retained=%d, 12k retained=%d", small, large)
	if large > small+small/2+200 {
		t.Fatalf("single-record proof decodes scale with retained history: %d -> %d", small, large)
	}
}
