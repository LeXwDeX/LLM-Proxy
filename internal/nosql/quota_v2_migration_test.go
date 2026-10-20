package nosql

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func TestJobLevelStoreCRUDAndCascade(t *testing.T) {
	db := testDB(t)
	levels := NewJobLevelStore(db)
	clients := NewClientStore(db)

	daily := 2.0
	if err := levels.Create(config.JobLevel{Name: "L5", CoefficientDaily: &daily}); err != nil {
		t.Fatalf("create L5: %v", err)
	}
	if err := levels.Create(config.JobLevel{Name: "l5"}); err == nil {
		t.Fatal("expected case-insensitive duplicate to be rejected")
	}
	if err := levels.Create(config.JobLevel{Name: ""}); err == nil {
		t.Fatal("expected empty name to be rejected")
	}
	neg := -1.0
	if err := levels.Create(config.JobLevel{Name: "bad", CoefficientDaily: &neg}); err == nil {
		t.Fatal("expected negative coefficient to be rejected")
	}

	got, err := levels.Get("L5")
	if err != nil || got == nil || got.CoefficientDaily == nil || *got.CoefficientDaily != 2 {
		t.Fatalf("get L5 = %+v (err %v)", got, err)
	}
	// 缺省系数语义：nil → 1；显式值透传（0 = 不限由评估层解释）。
	if got.CoefficientFor("daily") != 2 || got.CoefficientFor("weekly") != 1 || got.CoefficientFor("monthly") != 1 {
		t.Fatalf("coefficient resolution wrong: %+v", got)
	}

	// 重命名级联：绑定客户端的 JobLevel 同步改写（大小写不敏感匹配）。
	if err := clients.Create(config.Client{Name: "alice", AccessKey: "k1", JobLevel: "L5"}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if err := clients.Create(config.Client{Name: "bob", AccessKey: "k2", JobLevel: "l5"}); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	renamed, err := levels.Update("L5", config.JobLevel{Name: "L6", CoefficientDaily: &daily})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(renamed) != 2 {
		t.Fatalf("renamed clients = %v, want alice+bob", renamed)
	}
	for _, name := range []string{"alice", "bob"} {
		client, err := clients.Get(name)
		if err != nil || client == nil || client.JobLevel != "L6" {
			t.Fatalf("%s job level not cascaded: %+v (err %v)", name, client, err)
		}
	}
	if stale, err := levels.Get("L5"); err != nil || stale != nil {
		t.Fatalf("old name must be gone: %+v (err %v)", stale, err)
	}

	// 删除保护：仍被活跃客户端引用 → 拒绝并列出客户端名。
	err = levels.Delete("L6")
	if err == nil || !strings.Contains(err.Error(), "is referenced by clients") || !strings.Contains(err.Error(), "alice") || !strings.Contains(err.Error(), "bob") {
		t.Fatalf("expected referenced rejection naming clients, got %v", err)
	}
	// 改绑后删除成功。
	if err := clients.Update("alice", config.Client{Name: "alice", AccessKey: "k1"}); err != nil {
		t.Fatalf("unbind alice: %v", err)
	}
	if err := clients.Update("bob", config.Client{Name: "bob", AccessKey: "k2"}); err != nil {
		t.Fatalf("unbind bob: %v", err)
	}
	if err := levels.Delete("L6"); err != nil {
		t.Fatalf("delete unreferenced: %v", err)
	}
	if err := levels.Delete("L6"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found, got %v", err)
	}
}

// seedRawQuotaEntry 直接向 client_quota_groups 桶写入 v1 形态的原始 JSON
// （带限额覆盖字段），模拟迁移前的存量数据。
func seedRawQuotaEntry(t *testing.T, db *bolt.DB, name, rawJSON string) {
	t.Helper()
	err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketClientQuotaGroups)).Put([]byte(strings.ToLower(name)), []byte(rawJSON))
	})
	if err != nil {
		t.Fatalf("seed raw entry %q: %v", name, err)
	}
}

func rawQuotaEntryBytes(t *testing.T, db *bolt.DB, name string) string {
	t.Helper()
	var out string
	err := db.View(func(tx *bolt.Tx) error {
		out = string(tx.Bucket([]byte(BucketClientQuotaGroups)).Get([]byte(strings.ToLower(name))))
		return nil
	})
	if err != nil {
		t.Fatalf("read raw entry %q: %v", name, err)
	}
	return out
}

// TestMigrateQuotaV2 验证一次性迁移：快照落盘、职级字典升级、plan 限额作废、
// 归档条目不改写、废弃字段保留、meta 标记幂等。
func TestMigrateQuotaV2(t *testing.T) {
	db := testDB(t)
	dataDir := filepath.Join(t.TempDir(), "data")

	clients := NewClientStore(db)
	quotaStore := NewClientQuotaStore(db)
	dict := NewDictStore(db)

	// v1 现场：客户端个人额度 + 池限额；职级字典；带限额覆盖的 plan；归档条目。
	if err := clients.Create(config.Client{
		Name: "alice", AccessKey: "k1", JobLevel: "L5",
		QuotaDailyUSD: 10, QuotaWeeklyUSD: 50, QuotaMonthlyUSD: 200,
		PoolQuotas: []config.PoolQuota{{Pool: "p1", DailyUSD: 3}},
	}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if err := dict.Create(MetaKeyJobLevels, "L5"); err != nil {
		t.Fatalf("dict create L5: %v", err)
	}
	if err := dict.Create(MetaKeyJobLevels, "L7"); err != nil {
		t.Fatalf("dict create L7: %v", err)
	}
	seedRawQuotaEntry(t, db, "alice", `{"name":"alice","groups":[{"name":"g1","daily_usd":5,"weekly_usd":6,"monthly_usd":7}]}`)
	seedRawQuotaEntry(t, db, "gone", `{"name":"gone","groups":[{"name":"g1","daily_usd":1}],"deleted_at":"2026-01-01T00:00:00Z","ip_address":"192.0.2.9"}`)

	if err := MigrateQuotaV2(db, dataDir, nil); err != nil {
		t.Fatalf("MigrateQuotaV2: %v", err)
	}

	// 1. 快照文件落盘且完整保留 v1 数据。
	dirEntries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	var snapshotPath string
	for _, e := range dirEntries {
		if strings.HasPrefix(e.Name(), "quota-v1-snapshot-") {
			snapshotPath = filepath.Join(dataDir, e.Name())
		}
	}
	if snapshotPath == "" {
		t.Fatal("snapshot file not written")
	}
	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snapshot QuotaV2Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if len(snapshot.Clients) != 1 || snapshot.Clients[0].QuotaDailyUSD != 10 || len(snapshot.Clients[0].PoolQuotas) != 1 {
		t.Fatalf("snapshot clients wrong: %+v", snapshot.Clients)
	}
	if len(snapshot.JobLevelsDict) != 2 {
		t.Fatalf("snapshot dict wrong: %+v", snapshot.JobLevelsDict)
	}
	foundLegacy := false
	for _, entry := range snapshot.ClientQuotaGroups {
		if entry.Name == "alice" && len(entry.Groups) == 1 && entry.Groups[0].DailyUSD == 5 {
			foundLegacy = true
		}
	}
	if !foundLegacy {
		t.Fatalf("snapshot plan overrides missing: %+v", snapshot.ClientQuotaGroups)
	}

	// 2. 职级字典升级为结构化记录（中性系数 1）。
	levels := NewJobLevelStore(db)
	list, err := levels.List()
	if err != nil {
		t.Fatalf("list levels: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("job levels not upgraded: %+v", list)
	}
	for _, level := range list {
		if level.CoefficientFor("daily") != 1 || level.CoefficientFor("weekly") != 1 || level.CoefficientFor("monthly") != 1 {
			t.Fatalf("migrated level coefficients must default to 1: %+v", level)
		}
	}

	// 3. 活跃 plan 限额作废（只剩名字，系数 nil → 1）。
	plan, err := quotaStore.Get("alice")
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if len(plan) != 1 || plan[0].Name != "g1" || plan[0].Coefficient != nil {
		t.Fatalf("plan not rewritten: %+v", plan)
	}

	// 4. 归档条目字节不改写（历史快照保留旧限额字段）。
	rawGone := rawQuotaEntryBytes(t, db, "gone")
	if !strings.Contains(rawGone, "daily_usd") || !strings.Contains(rawGone, "deleted_at") {
		t.Fatalf("archived entry bytes were rewritten: %s", rawGone)
	}

	// 5. 客户端记录的废弃字段保持原样（供回滚对照）。
	alice, err := clients.Get("alice")
	if err != nil || alice == nil {
		t.Fatalf("get alice: %v", err)
	}
	if alice.QuotaDailyUSD != 10 || len(alice.PoolQuotas) != 1 {
		t.Fatalf("client record must not be rewritten: %+v", alice)
	}

	// 6. 幂等：再跑一次无副作用、不新增快照。
	before, _ := os.ReadDir(dataDir)
	if err := MigrateQuotaV2(db, dataDir, nil); err != nil {
		t.Fatalf("second MigrateQuotaV2: %v", err)
	}
	after, _ := os.ReadDir(dataDir)
	if len(after) != len(before) {
		t.Fatalf("migration not idempotent: %d → %d files", len(before), len(after))
	}
}

func TestMigrateQuotaV2_RetryPreservesEarlierSnapshot(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	clients := NewClientStore(db)
	if err := clients.Create(config.Client{Name: "alice", AccessKey: "k1", QuotaDailyUSD: 10}); err != nil {
		t.Fatal(err)
	}
	// Force the write transaction to fail after the first snapshot is written.
	if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(BucketJobLevels)) }); err != nil {
		t.Fatal(err)
	}
	if err := MigrateQuotaV2(db, dataDir, nil); err == nil {
		t.Fatal("migration should fail without job_levels bucket")
	}
	files, err := os.ReadDir(dataDir)
	if err != nil || len(files) != 1 {
		t.Fatalf("first snapshot: files=%v err=%v", files, err)
	}
	firstPath := filepath.Join(dataDir, files[0].Name())
	firstBytes, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { _, err := tx.CreateBucket([]byte(BucketJobLevels)); return err }); err != nil {
		t.Fatal(err)
	}
	if err := MigrateQuotaV2(db, dataDir, nil); err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	files, err = os.ReadDir(dataDir)
	if err != nil || len(files) != 2 {
		t.Fatalf("retry must create separate snapshot: files=%v err=%v", files, err)
	}
	retainedBytes, err := os.ReadFile(firstPath)
	if err != nil || string(retainedBytes) != string(firstBytes) {
		t.Fatalf("first snapshot changed after retry: err=%v", err)
	}
}

func TestMigrateQuotaV2_SnapshotFailureLeavesDatabaseUntouched(t *testing.T) {
	db := testDB(t)
	seedRawQuotaEntry(t, db, "alice", `{"name":"alice","groups":[{"name":"g1","daily_usd":5}]}`)
	before := rawQuotaEntryBytes(t, db, "alice")
	invalidSnapshotDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(invalidSnapshotDir, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateQuotaV2(db, invalidSnapshotDir, nil); err == nil {
		t.Fatal("snapshot directory failure must abort migration")
	}
	if after := rawQuotaEntryBytes(t, db, "alice"); after != before {
		t.Fatalf("plan changed after snapshot failure: %s", after)
	}
	var marked bool
	if err := db.View(func(tx *bolt.Tx) error {
		marked = tx.Bucket([]byte(BucketMeta)).Get([]byte(quotaV2MigratedMetaKey)) != nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if marked {
		t.Fatal("migration marker set after snapshot failure")
	}
}
