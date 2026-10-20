package admin

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	bolt "go.etcd.io/bbolt"
)

func TestTargetUsageAdapterValidationAndPreservation(t *testing.T) {
	h, _, stores := newTargetUpdateTestHandler(t, t.TempDir(), []string{"test-key"})
	body := targetUpdateBody("__existing_key_index__:0", nil)
	body["usage_adapter"] = "qwen"
	if r := putTarget(t, h, "target-1", body); r.Code != http.StatusOK {
		t.Fatalf("set: %d %s", r.Code, r.Body.String())
	}
	if got := mustGetTarget(t, stores.targetStore, "target-1").UsageAdapter; got != "qwen" {
		t.Fatalf("adapter=%q", got)
	}
	delete(body, "usage_adapter")
	if r := putTarget(t, h, "target-1", body); r.Code != http.StatusOK {
		t.Fatalf("preserve: %d", r.Code)
	}
	if got := mustGetTarget(t, stores.targetStore, "target-1").UsageAdapter; got != "qwen" {
		t.Fatalf("omission cleared adapter: %q", got)
	}
	for _, adapter := range []any{nil, "", " ", "auto", "untrusted"} {
		body["usage_adapter"] = adapter
		if r := putTarget(t, h, "target-1", body); r.Code != http.StatusBadRequest {
			t.Fatalf("invalid adapter %v status=%d", adapter, r.Code)
		}
		if got := mustGetTarget(t, stores.targetStore, "target-1").UsageAdapter; got != "qwen" {
			t.Fatalf("invalid update changed adapter: %q", got)
		}
	}
}

func TestKimiTargetCreateUpdateAndStore(t *testing.T) {
	h, _, stores := newTargetUpdateTestHandler(t, t.TempDir(), []string{"test-key"})
	body := targetUpdateBody("test-key", nil)
	body["name"] = "kimi-target"
	body["usage_adapter"] = " KIMI "
	payload, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/data/targets", bytes.NewReader(payload)))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGetTarget(t, stores.targetStore, "kimi-target").UsageAdapter; got != "kimi" {
		t.Fatalf("created adapter = %q", got)
	}
	body["api_key"] = "__existing_key_index__:0"
	for _, omit := range []bool{false, true} {
		if omit {
			delete(body, "usage_adapter")
		}
		if r := putTarget(t, h, "kimi-target", body); r.Code != http.StatusOK {
			t.Fatalf("update omit=%v: %d %s", omit, r.Code, r.Body.String())
		}
		if got := mustGetTarget(t, stores.targetStore, "kimi-target").UsageAdapter; got != "kimi" {
			t.Fatalf("updated adapter = %q", got)
		}
	}
}

func TestReloadLegacyConfigImportWritesRecordsAndLogs(t *testing.T) {
	for _, state := range []string{"missing", "empty", "auto"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			h, manager, stores := newTargetUpdateTestHandler(t, dir, []string{"test-key"})
			cfg := testConfig(dir, 1, []string{"k1"})
			var raw map[string]json.RawMessage
			content, _ := json.Marshal(cfg)
			if err := json.Unmarshal(content, &raw); err != nil {
				t.Fatal(err)
			}
			field := ""
			if state == "empty" {
				field = `,"usage_adapter":""`
			}
			if state == "auto" {
				field = `,"usage_adapter":" AUTO "`
			}
			raw["targets"] = json.RawMessage(`[{"name":"legacy","endpoint_type":"openai","endpoint":"https://user:secret@dashscope.aliyuncs.com/v1?key=secret","api_key":"secret"` + field + `}]`)
			content, _ = json.Marshal(raw)
			if err := os.WriteFile(filepath.Join(dir, "config.json"), content, 0o600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			h.(*Handler).logger = slog.New(slog.NewTextHandler(&logs, nil))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/config/reload", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("reload: %d %s", rec.Code, rec.Body.String())
			}
			var originalRecord []byte
			if err := stores.clientStore.DB().View(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(nosql.BucketUsageAdapterMigration))
				if b == nil {
					t.Fatal("reload import did not create record bucket")
				}
				originalRecord = append([]byte(nil), b.Get([]byte("legacy"))...)
				var record struct {
					OldState string `json:"old_state"`
					OldValue string `json:"old_value"`
					NewValue string `json:"new_value"`
				}
				if err := json.Unmarshal(originalRecord, &record); err != nil {
					t.Fatal(err)
				}
				wantOld := ""
				if state == "auto" {
					wantOld = " AUTO "
				}
				if record.OldState != state || record.OldValue != wantOld || record.NewValue != "qwen" {
					t.Fatalf("reload record: %+v", record)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logs.String(), "migrated target usage_adapter") || !strings.Contains(logs.String(), "target=legacy") || strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "https://") {
				t.Fatalf("reload import log: %s", logs.String())
			}
			current, err := manager.Current()
			if err != nil || len(current.Targets) != 1 || current.Targets[0].UsageAdapter != "qwen" {
				t.Fatalf("published config: %+v %v", current, err)
			}
			logs.Reset()
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/config/reload", nil))
			if rec.Code != http.StatusOK || strings.Contains(logs.String(), "migrated target usage_adapter") {
				t.Fatalf("second reload: %d %s", rec.Code, logs.String())
			}
			if err := stores.clientStore.DB().View(func(tx *bolt.Tx) error {
				if !bytes.Equal(tx.Bucket([]byte(nosql.BucketUsageAdapterMigration)).Get([]byte("legacy")), originalRecord) {
					t.Fatal("reload overwrote original record")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReloadLegacyConfigImportFailureDoesNotWriteRecordsOrLogs(t *testing.T) {
	dir := t.TempDir()
	h, manager, stores, service := newTargetUpdateTestHandlerWithService(t, dir, []string{"test-key"})
	initial := testConfig(dir, 1, []string{"k1"})
	manager.Replace(initial)
	beforeStatuses := service.TargetStatuses(time.Now())
	cfg := testConfig(dir, 2, []string{"k1"})
	cfg.Targets[0].Endpoint = "https://api.z.ai"
	cfg.Targets[0].UsageAdapter = "auto"
	cfg.Targets[1].Endpoint = "not-a-url"
	cfg.Targets[1].UsageAdapter = "auto"
	writeConfigFile(t, filepath.Join(dir, "config.json"), cfg)
	var logs bytes.Buffer
	h.(*Handler).logger = slog.New(slog.NewTextHandler(&logs, nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/config/reload", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed reload: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(logs.String(), "migrated target usage_adapter") {
		t.Fatalf("failed import logged migration: %s", logs.String())
	}
	if err := stores.clientStore.DB().View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(nosql.BucketTargets)).Stats().KeyN != 0 {
			t.Fatal("failed reload left partial targets")
		}
		if b := tx.Bucket([]byte(nosql.BucketUsageAdapterMigration)); b != nil && b.Stats().KeyN != 0 {
			t.Fatal("failed reload left partial records")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, err := manager.Current()
	if err != nil || len(current.Targets) != 1 || current.Targets[0].Endpoint != initial.Targets[0].Endpoint {
		t.Fatalf("failed reload published config: %+v %v", current, err)
	}
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != len(beforeStatuses) || statuses[0].Name != beforeStatuses[0].Name {
		t.Fatalf("failed reload changed runtime: %+v", statuses)
	}
}

func TestReloadImportsLegacyAdaptersBeforePublishing(t *testing.T) {
	dir := t.TempDir()
	h, manager, stores := newTargetUpdateTestHandler(t, dir, []string{"test-key"})
	cfg := testConfig(dir, 1, []string{"k1"})
	cfg.Targets[0].Endpoint = "https://dashscope.aliyuncs.com/compatible-mode"
	cfg.Targets[0].UsageAdapter = "auto"
	writeConfigFile(t, filepath.Join(dir, "config.json"), cfg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/config/reload", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("reload: %d %s", rec.Code, rec.Body.String())
	}
	current, err := manager.Current()
	if err != nil || len(current.Targets) != 1 || current.Targets[0].UsageAdapter != "qwen" {
		t.Fatalf("runtime config not explicit: %+v %v", current, err)
	}
	if got := mustGetTarget(t, stores.targetStore, "target-1").UsageAdapter; got != "qwen" {
		t.Fatalf("stored adapter: %q", got)
	}
}

func TestReloadMigrationFailureDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	h, manager, stores, service := newTargetUpdateTestHandlerWithService(t, dir, []string{"test-key"})
	manager.Replace(testConfig(dir, 1, []string{"k1"}))
	beforeStatuses := service.TargetStatuses(time.Now())
	db := stores.clientStore.DB()
	good := []byte(`{"name":"good","endpoint":"https://api.z.ai","usage_adapter":"auto"}`)
	bad := []byte(`{"name":"bad","endpoint":"not-a-url","usage_adapter":"auto"}`)
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(nosql.BucketTargets))
		if err := b.Put([]byte("a-good"), good); err != nil {
			return err
		}
		return b.Put([]byte("z-bad"), bad)
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/config/reload", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("reload: %d %s", rec.Code, rec.Body.String())
	}
	current, err := manager.Current()
	if err != nil || len(current.Targets) != 1 || current.Targets[0].Name != "target-1" || current.Targets[0].UsageAdapter != config.UsageAdapterStandard {
		t.Fatalf("failed migration published config: %+v %v", current, err)
	}
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != len(beforeStatuses) || statuses[0].Name != beforeStatuses[0].Name {
		t.Fatalf("failed migration changed runtime: %+v", statuses)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		if !bytes.Equal(tx.Bucket([]byte(nosql.BucketTargets)).Get([]byte("a-good")), good) {
			t.Fatal("failed reload partially migrated")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentConfigDoesNotFallbackToFileAfterSkippingInvalidStoreTargets(t *testing.T) {
	dir := t.TempDir()
	h, _, stores := newTargetUpdateTestHandler(t, dir, []string{"test-key"})
	if err := stores.clientStore.DB().Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(nosql.BucketTargets)).Put([]byte("invalid"), []byte(`{"name":"invalid","endpoint":"https://dashscope.aliyuncs.com","usage_adapter":"unknown"}`))
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := h.(*Handler).currentConfig()
	if err != nil || len(cfg.Targets) != 0 {
		t.Fatalf("file fallback leaked targets: %+v %v", cfg, err)
	}
}

func TestCreateTargetRequiresExplicitUsageAdapter(t *testing.T) {
	h, _, stores := newTargetUpdateTestHandler(t, t.TempDir(), []string{"test-key"})
	// Import the legacy fixture before exercising create; compare the stored count.
	if rec := putTarget(t, h, "target-1", targetUpdateBody("__existing_key_index__:0", nil)); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	for _, adapter := range []any{nil, "", " ", "auto", "unknown", 123, false} {
		body := targetUpdateBody("test-key", nil)
		body["name"] = "new-target"
		if adapter != nil {
			body["usage_adapter"] = adapter
		}
		payload, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/data/targets", bytes.NewReader(payload)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("adapter %v: %d %s", adapter, rec.Code, rec.Body.String())
		}
		targets, err := stores.targetStore.List()
		if err != nil || len(targets) != 1 {
			t.Fatalf("invalid create wrote data: %+v %v", targets, err)
		}
	}
	// Explicit null is different from a missing field, but is also invalid.
	body := targetUpdateBody("test-key", nil)
	body["name"] = "null-target"
	body["usage_adapter"] = nil
	payload, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/data/targets", bytes.NewReader(payload)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("null accepted: %d", rec.Code)
	}
}
