package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

func metricsTestHandler(t *testing.T, stores testStores, usageStore *nosql.UsageStore) *Handler {
	t.Helper()
	service, err := proxy.NewService(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("new proxy service: %v", err)
	}
	t.Cleanup(func() { service.Close() })
	return NewHandler(nil, nil, service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, usageStore, nil, nil, nil, nil, nil)
}

func readMetricsBody(t *testing.T, h *Handler) (map[string]json.RawMessage, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/admin/metrics status %d body %s", rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /admin/metrics: %v", err)
	}
	return body, rec.Body.String()
}

func awaitMetricsUsageRetention(t *testing.T, h *Handler) (map[string]json.RawMessage, string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, raw := readMetricsBody(t, h)
		encoded, ok := body["usage_retention"]
		if ok {
			var retention map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &retention); err != nil {
				t.Fatalf("decode usage_retention: %v", err)
			}
			var available bool
			if err := json.Unmarshal(retention["available"], &available); err == nil && available {
				return retention, raw
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("/admin/metrics never exposed an available usage_retention snapshot: %s", readMetricsRawForTest(t, h))
	return nil, ""
}

func readMetricsRawForTest(t *testing.T, h *Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/metrics", nil))
	return rec.Body.String()
}

func exactKeys(t *testing.T, label string, raw map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(raw) != len(want) {
		t.Fatalf("%s has keys %v, want %v", label, sortedKeys(raw), want)
	}
	for _, key := range want {
		if _, ok := raw[key]; !ok {
			t.Fatalf("%s missing %q: %v", label, key, sortedKeys(raw))
		}
	}
}

func sortedKeys(raw map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func TestMetricsExposeUsageRetentionSnapshot(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	now := time.Now().UTC()
	if err := stores.usageStore.Record(usage.Event{
		Timestamp:  now.Add(-time.Hour),
		RecordID:   "123e4567-e89b-12d3-a456-426614174000",
		ClientName: "client-secret-name",
		Model:      "model-secret-name",
		Target:     "target-secret-name",
		StatusCode: 200,
	}); err != nil {
		t.Fatalf("seed usage event: %v", err)
	}
	legacy := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := stores.db.Update(func(tx *bolt.Tx) error {
		receipts := tx.Bucket([]byte(nosql.BucketUsageReceipts))
		if receipts == nil {
			return fmt.Errorf("receipts bucket missing")
		}
		if err := receipts.Put([]byte("receipt-key-secret-name"), []byte("{}")); err != nil {
			return err
		}
		meta := tx.Bucket([]byte(nosql.BucketMeta))
		if meta == nil {
			return fmt.Errorf("meta bucket missing")
		}
		if err := meta.Put([]byte("usage_receipts_legacy_unknown_before_v1"), []byte(legacy.Format(time.RFC3339Nano))); err != nil {
			return err
		}
		if err := meta.Put([]byte("usage_receipts_untrusted_through_v2"), []byte("raw-metadata-secret")); err != nil {
			return err
		}
		return meta.Put([]byte("usage_receipts_watermark_done_v1"), []byte("1"))
	}); err != nil {
		t.Fatalf("seed retention data: %v", err)
	}
	h := metricsTestHandler(t, stores, stores.usageStore)

	retention, raw := awaitMetricsUsageRetention(t, h)
	exactKeys(t, "usage_retention", retention, "available", "reason", "captured_at", "buckets", "events", "metadata")

	var buckets map[string]json.RawMessage
	if err := json.Unmarshal(retention["buckets"], &buckets); err != nil {
		t.Fatalf("decode buckets: %v", err)
	}
	exactKeys(t, "usage_retention.buckets", buckets, "receipts", "receipt_owners", "raw_owners", "fresh_attestations")
	for _, name := range []string{"receipts", "receipt_owners", "raw_owners", "fresh_attestations"} {
		var bucket map[string]json.RawMessage
		if err := json.Unmarshal(buckets[name], &bucket); err != nil {
			t.Fatalf("decode bucket %s: %v", name, err)
		}
		exactKeys(t, "usage_retention.buckets."+name, bucket, "present", "entries", "pages", "in_use_bytes")
	}

	var events map[string]json.RawMessage
	if err := json.Unmarshal(retention["events"], &events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	exactKeys(t, "usage_retention.events", events, "earliest", "latest", "invalid_keys")
	if string(events["earliest"]) == "null" || string(events["latest"]) == "null" {
		t.Fatalf("event range did not report the seeded event: %s", raw)
	}

	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(retention["metadata"], &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	exactKeys(t, "usage_retention.metadata", metadata,
		"legacy_unknown_before", "legacy_unknown_before_invalid",
		"untrusted_through", "untrusted_through_invalid",
		"watermark_done_v1", "watermark_done_v2", "unknown_aggregate_hour")

	var receipts map[string]json.RawMessage
	if err := json.Unmarshal(buckets["receipts"], &receipts); err != nil {
		t.Fatalf("decode receipts bucket: %v", err)
	}
	var receiptEntries int
	if err := json.Unmarshal(receipts["entries"], &receiptEntries); err != nil || receiptEntries != 1 {
		t.Fatalf("receipts entries = %s (err %v), want 1: %s", receipts["entries"], err, raw)
	}
	if string(metadata["legacy_unknown_before"]) == "null" || string(metadata["untrusted_through"]) != "null" {
		t.Fatalf("metadata watermarks not decoded as expected: %s", raw)
	}
	var untrustedInvalid, doneV1 bool
	_ = json.Unmarshal(metadata["untrusted_through_invalid"], &untrustedInvalid)
	_ = json.Unmarshal(metadata["watermark_done_v1"], &doneV1)
	if !untrustedInvalid || !doneV1 {
		t.Fatalf("metadata flags = invalid %v done_v1 %v, want true/true: %s", untrustedInvalid, doneV1, raw)
	}

	topLevel := readTopLevelKeys(t, h)
	for _, key := range []string{"generated_at", "uptime_seconds", "active_requests", "requests", "targets", "targets_runtime", "responses_ws", "persistence", "observation_budget"} {
		if _, ok := topLevel[key]; !ok {
			t.Fatalf("/admin/metrics lost legacy field %q", key)
		}
	}
	// The retention section must never contain raw keys, client/model names or
	// stored bytes.
	for _, secret := range []string{
		"123e4567-e89b-12d3-a456-426614174000", "client-secret-name", "model-secret-name", "target-secret-name",
		"receipt-key-secret-name", "raw-metadata-secret",
		"usage_record_receipts_v1", "usage_receipt_owner_hashes_v1", "usage_raw_owners_v1", "usage_fresh_commit_attestations_v1",
	} {
		if strings.Contains(raw, secret) {
			t.Fatalf("/admin/metrics leaked %q: %s", secret, raw)
		}
	}
}

func readTopLevelKeys(t *testing.T, h *Handler) map[string]json.RawMessage {
	t.Helper()
	body, _ := readMetricsBody(t, h)
	return body
}

func TestMetricsOmitUsageRetentionWithoutStore(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	h := metricsTestHandler(t, stores, nil)

	body, raw := readMetricsBody(t, h)
	if _, ok := body["usage_retention"]; ok {
		t.Fatalf("/admin/metrics exposed usage_retention without a usage store: %s", raw)
	}
	for _, key := range []string{"generated_at", "uptime_seconds", "active_requests", "requests", "targets", "targets_runtime", "responses_ws", "persistence", "observation_budget"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("/admin/metrics lost legacy field %q: %s", key, raw)
		}
	}
}

func TestMetricsUsageRetentionNotReadyOnFirstRead(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	h := metricsTestHandler(t, stores, stores.usageStore)

	// The first read starts the asynchronous refresh and must report not_ready
	// without waiting for the database.
	body, raw := readMetricsBody(t, h)
	encoded, ok := body["usage_retention"]
	if !ok {
		t.Fatalf("/admin/metrics omitted usage_retention: %s", raw)
	}
	var retention map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &retention); err != nil {
		t.Fatalf("decode usage_retention: %v", err)
	}
	exactKeys(t, "usage_retention", retention, "available", "reason", "captured_at", "buckets", "events", "metadata")
	var available bool
	var reason string
	_ = json.Unmarshal(retention["available"], &available)
	_ = json.Unmarshal(retention["reason"], &reason)
	if available || reason != "not_ready" {
		t.Fatalf("first read = available %v reason %q, want %q: %s", available, reason, "not_ready", raw)
	}
	for _, key := range []string{"captured_at", "buckets", "events", "metadata"} {
		if string(retention[key]) != "null" {
			t.Fatalf("not_ready snapshot carried %s = %s: %s", key, retention[key], raw)
		}
	}
}

func TestMetricsUsageRetentionStoreUnavailable(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	// A non-nil usage store whose database handle is unavailable is reachable
	// through the constructor without stubbing.
	h := metricsTestHandler(t, stores, nosql.NewUsageStore(nil))

	body, raw := readMetricsBody(t, h)
	encoded, ok := body["usage_retention"]
	if !ok {
		t.Fatalf("/admin/metrics omitted usage_retention: %s", raw)
	}
	var retention map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &retention); err != nil {
		t.Fatalf("decode usage_retention: %v", err)
	}
	exactKeys(t, "usage_retention", retention, "available", "reason", "captured_at", "buckets", "events", "metadata")
	var available bool
	var reason string
	_ = json.Unmarshal(retention["available"], &available)
	_ = json.Unmarshal(retention["reason"], &reason)
	if available || reason != "store_unavailable" {
		t.Fatalf("nil database snapshot = available %v reason %q, want %q: %s", available, reason, "store_unavailable", raw)
	}
	for _, key := range []string{"captured_at", "buckets", "events", "metadata"} {
		if string(retention[key]) != "null" {
			t.Fatalf("store_unavailable snapshot carried %s = %s: %s", key, retention[key], raw)
		}
	}
}

func TestMetricsUsageRetentionDegradesWhenDatabaseFails(t *testing.T) {
	stores := setupTestStores(t, t.TempDir())
	h := metricsTestHandler(t, stores, stores.usageStore)
	if err := stores.db.Close(); err != nil {
		t.Fatalf("close test database: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, raw := readMetricsBody(t, h)
		if encoded, ok := body["usage_retention"]; ok {
			var retention map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &retention); err != nil {
				t.Fatalf("decode usage_retention: %v", err)
			}
			var available bool
			var reason string
			_ = json.Unmarshal(retention["available"], &available)
			_ = json.Unmarshal(retention["reason"], &reason)
			if !available && reason == "transaction_failed" {
				if string(retention["buckets"]) != "null" || string(retention["events"]) != "null" || string(retention["metadata"]) != "null" {
					t.Fatalf("failed snapshot carried detail: %s", raw)
				}
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("/admin/metrics never reported a transaction_failed usage_retention snapshot: %s", readMetricsRawForTest(t, h))
}
