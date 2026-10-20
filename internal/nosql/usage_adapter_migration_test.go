package nosql

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	bolt "go.etcd.io/bbolt"
)

func seedAdapterRaw(t *testing.T, db *bolt.DB, key, raw string) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketTargets)).Put([]byte(key), []byte(raw))
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyConfigImportRecordsAndLogs(t *testing.T) {
	db := testDB(t)
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{"server":{"bind":"127.0.0.1:0","request_timeout_seconds":5},"data_store":{"db_path":"test.db"},"admin_session":{"cookie_name":"test-session","secret":"test-secret","ttl_seconds":3600},"logging":{"level":"info","access_log":"access.log","error_log":"error.log"},"targets":[
		{"name":"Missing","endpoint":"https://user:secret@dashscope.aliyuncs.com/v1?key=secret","api_key":"secret"},
		{"name":"Empty","usage_adapter":"","endpoint":"https://api.z.ai","api_key":"secret"},
		{"name":"Whitespace","usage_adapter":" \t ","endpoint":"https://gateway.example.com","api_key":"secret"},
		{"name":"Auto","usage_adapter":" AUTO ","endpoint":"https://api.deepseek.com","api_key":"secret"},
		{"name":"Explicit","usage_adapter":"standard","endpoint":"https://dashscope.aliyuncs.com","api_key":"secret"}
	]}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	writer := adapterImportLogWriter{buffer: &logs, check: func() {
		// A log callback must observe committed targets and records, not the
		// old database snapshot from inside the write transaction.
		if len(adapterBucketSnapshot(t, db, BucketTargets)) != 5 || len(adapterBucketSnapshot(t, db, BucketUsageAdapterMigration)) != 4 {
			t.Error("logged before import committed")
		}
	}}
	logger := slog.New(slog.NewTextHandler(writer, nil))
	store := NewTargetStore(db, logger)
	if imported, err := store.MigrateFromConfig(cfg.Targets); err != nil || !imported {
		t.Fatalf("import: %v %v", imported, err)
	}
	records := adapterBucketSnapshot(t, db, BucketUsageAdapterMigration)
	if len(records) != 4 {
		t.Fatalf("missing import records: %+v", records)
	}
	for key, want := range map[string]struct{ state, old, next string }{
		"missing": {"missing", "", "qwen"}, "empty": {"empty", "", "glm"}, "auto": {"auto", " AUTO ", "deepseek"}, "whitespace": {"empty", " \t ", "standard"},
	} {
		var record usageAdapterMigrationRecord
		if err := json.Unmarshal([]byte(records[key]), &record); err != nil || record.OldState != want.state || record.OldValue != want.old || record.NewValue != want.next || record.MigratedAt.IsZero() || record.Version != usageAdapterMigrationVersion {
			t.Fatalf("record %s: %+v %v", key, record, err)
		}
		if !strings.Contains(logs.String(), "target="+record.Target) || !strings.Contains(logs.String(), "new="+want.next) {
			t.Fatalf("missing per-target import log: %s", logs.String())
		}
	}
	if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "https://") {
		t.Fatal("import log leaked URL/credentials")
	}
	beforeTargets := adapterBucketSnapshot(t, db, BucketTargets)
	beforeLogs := logs.String()
	if err := MigrateUsageAdapterExplicit(db, logger); err != nil {
		t.Fatal(err)
	}
	if imported, err := store.MigrateFromConfig(cfg.Targets); err != nil || imported {
		t.Fatalf("second import: %v %v", imported, err)
	}
	if logs.String() != beforeLogs {
		t.Fatal("no-op migration/import emitted logs")
	}
	for key, value := range records {
		if adapterBucketSnapshot(t, db, BucketUsageAdapterMigration)[key] != value {
			t.Fatal("no-op changed record")
		}
	}
	for key, value := range beforeTargets {
		if adapterBucketSnapshot(t, db, BucketTargets)[key] != value {
			t.Fatal("no-op changed target")
		}
	}
}

type adapterImportLogWriter struct {
	buffer *bytes.Buffer
	check  func()
}

func (w adapterImportLogWriter) Write(data []byte) (int, error) {
	w.check()
	return w.buffer.Write(data)
}

func TestLegacyConfigImportPreservesExistingRecord(t *testing.T) {
	db := testDB(t)
	original := `{"original":true}`
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(BucketUsageAdapterMigration))
		if err != nil {
			return err
		}
		return b.Put([]byte("legacy"), []byte(original))
	}); err != nil {
		t.Fatal(err)
	}
	legacy := []config.Target{{Name: "legacy", Endpoint: "https://api.z.ai", APIKey: "test", UsageAdapter: "auto"}}
	if imported, err := MigrateTargetsFromConfig(db, legacy); err != nil || !imported {
		t.Fatalf("import: %v %v", imported, err)
	}
	if adapterBucketSnapshot(t, db, BucketUsageAdapterMigration)["legacy"] != original {
		t.Fatal("import overwrote existing migration record")
	}
}

func TestLegacyConfigImportFailureDoesNotWriteRecordsOrLogs(t *testing.T) {
	for _, failure := range []string{"bad-endpoint", "duplicate-target", "record-write"} {
		t.Run(failure, func(t *testing.T) {
			db := testDB(t)
			legacy := []config.Target{{Name: "first", Endpoint: "https://api.z.ai", APIKey: "test", UsageAdapter: "auto"}, {Name: "second", Endpoint: "https://example.com", APIKey: "test"}}
			switch failure {
			case "bad-endpoint":
				legacy[1].Endpoint = "not-a-url"
			case "duplicate-target":
				legacy[1].Name = "first"
			case "record-write":
				if err := db.Update(func(tx *bolt.Tx) error {
					b, err := tx.CreateBucketIfNotExists([]byte(BucketUsageAdapterMigration))
					if err != nil {
						return err
					}
					_, err = b.CreateBucket([]byte("second"))
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			beforeRecords := adapterBucketSnapshot(t, db, BucketUsageAdapterMigration)
			var logs bytes.Buffer
			store := NewTargetStore(db, slog.New(slog.NewTextHandler(&logs, nil)))
			if imported, err := store.MigrateFromConfig(legacy); err == nil || imported {
				t.Fatalf("failed import: %v %v", imported, err)
			}
			if len(adapterBucketSnapshot(t, db, BucketTargets)) != 0 || logs.Len() != 0 {
				t.Fatalf("failed import wrote targets/logs: %s", logs.String())
			}
			afterRecords := adapterBucketSnapshot(t, db, BucketUsageAdapterMigration)
			if len(afterRecords) != len(beforeRecords) {
				t.Fatalf("failed import wrote records: %+v", afterRecords)
			}
			for key, value := range beforeRecords {
				if afterRecords[key] != value {
					t.Fatal("failed import changed old records")
				}
			}
		})
	}
}

func adapterBucketSnapshot(t *testing.T, db *bolt.DB, bucket string) map[string]string {
	t.Helper()
	values := map[string]string{}
	if err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error { values[string(k)] = string(v); return nil })
	}); err != nil {
		t.Fatal(err)
	}
	return values
}

func TestMigrateUsageAdapterExplicitPreservesRawAndRecords(t *testing.T) {
	db := testDB(t)
	raw := ` { "name": "Legacy", "future": { "x": [1, 2] }, "endpoint_type":"bailian", "endpoint":"https://user:secret@dashscope.aliyuncs.com/v1?key=secret", "usage_adapter" : " AUTO ", "allowed_models":["old"], "api_key":"secret" } `
	seedAdapterRaw(t, db, "legacy", raw)
	seedAdapterRaw(t, db, "missing", `{"endpoint":"https://api.deepseek.com","name":"Missing","future":42}`)
	seedAdapterRaw(t, db, "empty", `{"usage_adapter":"  ","endpoint":"https://open.bigmodel.cn","name":"Empty"}`)
	seedAdapterRaw(t, db, "custom", `{"usage_adapter":"auto","endpoint":"https://gateway.example.com","name":"Custom"}`)
	seedAdapterRaw(t, db, "explicit", `{"usage_adapter":"standard","endpoint":"not-a-url","future":true}`)
	before := adapterBucketSnapshot(t, db, BucketTargets)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	if err := MigrateUsageAdapterExplicit(db, logger); err != nil {
		t.Fatal(err)
	}
	after := adapterBucketSnapshot(t, db, BucketTargets)
	if want := strings.Replace(raw, `" AUTO "`, `"qwen"`, 1); after["legacy"] != want {
		t.Fatalf("changed fields/order outside adapter: %s", after["legacy"])
	}
	if after["explicit"] != before["explicit"] {
		t.Fatal("explicit value changed")
	}
	if after["missing"] != `{"endpoint":"https://api.deepseek.com","name":"Missing","future":42,"usage_adapter":"deepseek"}` {
		t.Fatalf("missing-field migration changed existing bytes: %s", after["missing"])
	}
	for key, want := range map[string]string{"legacy": "qwen", "missing": "deepseek", "empty": "glm", "custom": "standard"} {
		var target config.Target
		if err := json.Unmarshal([]byte(after[key]), &target); err != nil || target.UsageAdapter != want {
			t.Fatalf("%s: %+v %v", key, target, err)
		}
	}
	records := adapterBucketSnapshot(t, db, BucketUsageAdapterMigration)
	if len(records) != 4 {
		t.Fatalf("records: %+v", records)
	}
	for key, state := range map[string]string{"legacy": "auto", "missing": "missing", "empty": "empty"} {
		var record usageAdapterMigrationRecord
		if err := json.Unmarshal([]byte(records[key]), &record); err != nil || record.OldState != state || record.NewValue == "" || record.Version == "" || record.MigratedAt.IsZero() {
			t.Fatalf("record %s: %+v %v", key, record, err)
		}
	}
	if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "https://") {
		t.Fatalf("migration log leaked endpoint/key: %s", logs.String())
	}
	logSize := logs.Len()
	if err := MigrateUsageAdapterExplicit(db, logger); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != logSize {
		t.Fatal("no-op migration emitted logs")
	}
	for key, value := range after {
		if adapterBucketSnapshot(t, db, BucketTargets)[key] != value {
			t.Fatal("second run changed data")
		}
	}
	for key, value := range records {
		if adapterBucketSnapshot(t, db, BucketUsageAdapterMigration)[key] != value {
			t.Fatal("second run changed record")
		}
	}
	// A later legacy write still migrates, but must not replace its original record.
	seedAdapterRaw(t, db, "legacy", raw)
	if err := MigrateUsageAdapterExplicit(db, logger); err != nil {
		t.Fatal(err)
	}
	if adapterBucketSnapshot(t, db, BucketUsageAdapterMigration)["legacy"] != records["legacy"] {
		t.Fatal("record overwritten")
	}
}

func TestMigrateUsageAdapterExplicitRollsBackWholeBatch(t *testing.T) {
	for _, bad := range []string{
		`{"usage_adapter":"auto","endpoint":"not-a-url"}`,
		`{"usage_adapter":"auto","endpoint":123}`,
		`{"usage_adapter":`,
		`{"usage_adapter":123,"endpoint":"https://example.com"}`,
		`{"usage_adapter":null,"endpoint":"https://example.com"}`,
		`{"usage_adapter":"auto","usage_adapter":"standard","endpoint":"https://example.com"}`,
		`[]`,
	} {
		t.Run(bad, func(t *testing.T) {
			db := testDB(t)
			seedAdapterRaw(t, db, "a-good", `{"usage_adapter":"auto","endpoint":"https://api.z.ai"}`)
			seedAdapterRaw(t, db, "z-bad", bad)
			before := adapterBucketSnapshot(t, db, BucketTargets)
			var logs bytes.Buffer
			if err := MigrateUsageAdapterExplicit(db, slog.New(slog.NewTextHandler(&logs, nil))); err == nil {
				t.Fatal("invalid batch accepted")
			}
			for key, value := range before {
				if adapterBucketSnapshot(t, db, BucketTargets)[key] != value {
					t.Fatal("partially committed")
				}
			}
			if len(adapterBucketSnapshot(t, db, BucketUsageAdapterMigration)) != 0 || logs.Len() != 0 {
				t.Fatal("failed transaction published records/logs")
			}
			// Correcting the bad endpoint permits a retry without resetting the DB.
			seedAdapterRaw(t, db, "z-bad", `{"usage_adapter":"auto","endpoint":"https://example.com"}`)
			if err := MigrateUsageAdapterExplicit(db, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMigrateUsageAdapterExplicitWriteFailureRollsBack(t *testing.T) {
	db := testDB(t)
	seedAdapterRaw(t, db, "a-good", `{"usage_adapter":"auto","endpoint":"https://api.z.ai"}`)
	seedAdapterRaw(t, db, "z-collision", `{"usage_adapter":"auto","endpoint":"https://example.com"}`)
	// A bucket at a record key makes bbolt Put fail. Both target updates and
	// the earlier record write must roll back, while the old bucket survives.
	if err := db.Update(func(tx *bolt.Tx) error {
		records, err := tx.CreateBucketIfNotExists([]byte(BucketUsageAdapterMigration))
		if err != nil {
			return err
		}
		_, err = records.CreateBucket([]byte("z-collision"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := adapterBucketSnapshot(t, db, BucketTargets)
	var logs bytes.Buffer
	if err := MigrateUsageAdapterExplicit(db, slog.New(slog.NewTextHandler(&logs, nil))); err == nil {
		t.Fatal("write failure ignored")
	}
	for key, value := range before {
		if adapterBucketSnapshot(t, db, BucketTargets)[key] != value {
			t.Fatal("targets partially committed")
		}
	}
	records := adapterBucketSnapshot(t, db, BucketUsageAdapterMigration)
	if len(records) != 1 || logs.Len() != 0 {
		t.Fatalf("records/logs partially committed: %+v %s", records, logs.String())
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketUsageAdapterMigration)).Bucket([]byte("z-collision")) == nil {
			t.Fatal("old bucket lost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyConfigImportFailureRollsBack(t *testing.T) {
	db := testDB(t)
	legacy := []config.Target{
		{Name: "good", Endpoint: "https://dashscope.aliyuncs.com", APIKey: "test"},
		{Name: "bad", Endpoint: "not-a-url", APIKey: "test", UsageAdapter: "auto"},
	}
	if imported, err := MigrateTargetsFromConfig(db, legacy); err == nil || imported {
		t.Fatalf("invalid import: %v %v", imported, err)
	}
	if len(adapterBucketSnapshot(t, db, BucketTargets)) != 0 {
		t.Fatal("partial legacy import")
	}
	legacy[1].Endpoint = "https://api.deepseek.com"
	if imported, err := MigrateTargetsFromConfig(db, legacy); err != nil || !imported {
		t.Fatalf("retry: %v %v", imported, err)
	}
}

func TestLegacyConfigImportExplicitAdaptersAndSkipStaleConfig(t *testing.T) {
	db := testDB(t)
	legacy := []config.Target{
		{Name: "empty", Endpoint: "https://dashscope.aliyuncs.com", APIKey: "test"},
		{Name: "auto", Endpoint: "https://api.z.ai", APIKey: "test", UsageAdapter: "auto"},
	}
	if imported, err := MigrateTargetsFromConfig(db, legacy); err != nil || !imported {
		t.Fatalf("import: %v %v", imported, err)
	}
	targets, err := NewTargetStore(db).List()
	if err != nil || len(targets) != 2 || targets[0].UsageAdapter != "qwen" || targets[1].UsageAdapter != "glm" {
		t.Fatalf("targets: %+v %v", targets, err)
	}
	legacy[0].Endpoint = "bad"
	legacy[0].UsageAdapter = "unknown"
	if imported, err := MigrateTargetsFromConfig(db, legacy); err != nil || imported {
		t.Fatalf("stale config rejected: %v %v", imported, err)
	}
}

func TestTargetStoreRejectsNonExplicitWritesAndSkipsRuntimeResiduals(t *testing.T) {
	db := testDB(t)
	var logs bytes.Buffer
	store := NewTargetStore(db, slog.New(slog.NewTextHandler(&logs, nil)))
	valid := config.Target{Name: "valid", Endpoint: "https://example.com", APIKey: "test", UsageAdapter: "standard"}
	if err := store.Create(valid); err != nil {
		t.Fatal(err)
	}
	for _, adapter := range []string{"", " ", "auto", "unknown"} {
		bad := valid
		bad.Name = "bad"
		bad.UsageAdapter = adapter
		if err := store.Create(bad); err == nil {
			t.Errorf("create accepted %q", adapter)
		}
		bad.Name = valid.Name
		if err := store.Update(valid.Name, bad); err == nil {
			t.Errorf("update accepted %q", adapter)
		}
	}
	for name, adapter := range map[string]string{"empty": "", "auto": "auto", "unknown": "unknown"} {
		raw := `{"name":"` + name + `","endpoint":"https://dashscope.aliyuncs.com","usage_adapter":"` + adapter + `","api_key":"secret"}`
		seedAdapterRaw(t, db, name, raw)
	}
	seedAdapterRaw(t, db, "missing", `{"name":"missing","endpoint":"https://dashscope.aliyuncs.com","api_key":"secret"}`)
	before := adapterBucketSnapshot(t, db, BucketTargets)
	targets, err := store.List()
	if err != nil || len(targets) != 1 || targets[0].Name != "valid" {
		t.Fatalf("runtime residuals: %+v %v", targets, err)
	}
	for _, name := range []string{"empty", "auto", "unknown", "missing"} {
		if adapterBucketSnapshot(t, db, BucketTargets)[name] != before[name] {
			t.Fatal("skipped target was rewritten")
		}
		if !strings.Contains(logs.String(), `target=`+name) {
			t.Errorf("missing target error log: %s", logs.String())
		}
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatal("missing Error log")
	}
}
