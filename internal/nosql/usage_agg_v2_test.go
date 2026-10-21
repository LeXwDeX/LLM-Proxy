package nosql

import (
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// manualLegacyAggKey reproduces the old unescaped key shape without relying on
// the implementation under test. Fixed test dimensions are already normalized.
func manualLegacyAggKey(hour string, endpointType, client, model, target string) string {
	return hour + "|" + endpointType + "|" + client + "|" + model + "|" + target
}

// manualV2AggKey reproduces the versioned key contract independently.
func manualV2AggKey(t time.Time, endpointType, client, model, target string) string {
	hour := t.UTC().Truncate(time.Hour).Format(hourLayout)
	encode := func(value string) string { return hex.EncodeToString([]byte(value)) }
	return "v2|" + hour + "|" + encode(endpointType) + "|" + encode(client) + "|" + encode(model) + "|" + encode(target)
}

func putAggRaw(t *testing.T, db *bolt.DB, key string, value []byte) {
	t.Helper()
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(BucketUsageAggHourly))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), value)
	}); err != nil {
		t.Fatalf("put raw aggregate %q: %v", key, err)
	}
}

func TestAggKeyV2EncodesDimensionsWithoutCollision(t *testing.T) {
	hour := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	key := string(aggKey(hour, " OpenAI ", "team|api", " Model%7C ", " Target:One "))
	parts := strings.Split(key, "|")
	if len(parts) != 6 || parts[0] != "v2" {
		t.Fatalf("v2 key %q is not six segments with a version prefix", key)
	}
	decoded := make([]string, 4)
	for i, part := range parts[2:] {
		raw, err := hex.DecodeString(part)
		if err != nil {
			t.Fatalf("segment %d is not hex: %v", i, err)
		}
		decoded[i] = string(raw)
	}
	want := []string{"openai", "team|api", "model%7c", "target:one"}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("decoded dimensions=%q, want %q", decoded, want)
	}

	// The old format collapsed these two dimension sets into one key. The v2
	// format must keep them apart.
	oldCollisionA := manualLegacyAggKey(hour.Format(hourLayout), "openai", "team|api", "m", "t")
	oldCollisionB := manualLegacyAggKey(hour.Format(hourLayout), "openai", "team", "api|m", "t")
	if oldCollisionA != oldCollisionB {
		t.Fatalf("test premise broken: legacy keys differ: %q vs %q", oldCollisionA, oldCollisionB)
	}
	if string(aggKey(hour, "openai", "team|api", "m", "t")) == string(aggKey(hour, "openai", "team", "api|m", "t")) {
		t.Fatal("v2 keys still collide for delimiter-bearing client and model")
	}
	if string(aggKey(hour, "openai", "team|api", "m", "t")) == string(aggKey(hour, "openai", "team%7Capi", "m", "t")) {
		t.Fatal("v2 keys collapse a pipe and a literal percent-encoded pipe")
	}
	if string(aggKey(hour, "openai", "客户|代理", "模型:v1", "目标%7C名称")) ==
		string(aggKey(hour, "openai", "客户", "代理|模型:v1", "目标%7C名称")) {
		t.Fatal("v2 keys collide for Unicode dimensions containing separators")
	}

	empty := string(aggKey(hour, "", "", "", ""))
	emptyParts := strings.Split(empty, "|")
	if len(emptyParts) != 6 {
		t.Fatalf("empty dimensions produced %d segments: %q", len(emptyParts), empty)
	}
	for i, want := range []string{"", "unknown", "unknown", "unknown"} {
		raw, err := hex.DecodeString(emptyParts[2+i])
		if err != nil || string(raw) != want {
			t.Fatalf("empty dimension %d decoded=%q err=%v, want %q", i, raw, err, want)
		}
	}
}

func TestAggregateAndCountReadMixedFormats(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	hour := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	hourKey := hour.Format(hourLayout)

	// A five-segment legacy cell remains decodable, and no raw rows exist:
	// this is the pruned-history shape.
	putAgg(t, db, manualLegacyAggKey(hourKey, "openai", "alice", "gpt-4", "upstream"),
		AggCell{RecordCount: 2, Requests: 2, Success: 1, InputTokens: 10})
	putAgg(t, db, manualV2AggKey(hour, "openai", "alice", "gpt-4", "upstream"),
		AggCell{RecordCount: 3, Requests: 3, Success: 2, InputTokens: 20})
	putAgg(t, db, manualV2AggKey(hour, "claude", "alice", "sonnet", "upstream"),
		AggCell{RecordCount: 1, Requests: 1, Success: 1, InputTokens: 5})

	total, success, err := store.Count(hour, hour)
	if err != nil {
		t.Fatalf("count mixed formats: %v", err)
	}
	if total != 6 || success != 4 {
		t.Fatalf("count mixed formats=(%d,%d), want (6,4)", total, success)
	}

	agg, err := store.Aggregate(usage.Filter{From: &hour, To: &hour}, "hour", nil)
	if err != nil {
		t.Fatalf("aggregate mixed formats: %v", err)
	}
	if agg.Totals.Requests != 6 || agg.Totals.InputTokens != 35 {
		t.Fatalf("aggregate mixed totals=%+v", agg.Totals)
	}
	if len(agg.Buckets) != 1 || !agg.Buckets[0].BucketStart.Equal(hour) {
		t.Fatalf("aggregate buckets=%+v, want one at %v", agg.Buckets, hour)
	}

	full := store.Count
	if _, _, err := full(hour.Add(2*time.Hour), hour.Add(2*time.Hour)); err != nil {
		t.Fatalf("count empty window: %v", err)
	}
	if total, success, _ := full(hour.Add(2*time.Hour), hour.Add(2*time.Hour)); total != 0 || success != 0 {
		t.Fatalf("empty window counted (%d,%d)", total, success)
	}
	if total, _, _ := full(hour.Add(time.Hour), hour.Add(time.Hour)); total != 0 {
		t.Fatalf("next hour counted %d legacy/v2 requests", total)
	}

	t.Run("supplementary rows count once after prune", func(t *testing.T) {
		db := testDB(t)
		store := NewUsageStore(db)
		event := fixedIdentityBundle()
		if err := store.Record(event); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PruneEventsBefore(hour.Add(time.Hour), 10); err != nil {
			t.Fatalf("prune: %v", err)
		}
		total, success, err := store.Count(hour, hour)
		if err != nil || total != 1 || success != 1 {
			t.Fatalf("count after prune=(%d,%d,%v), want (1,1,nil)", total, success, err)
		}
		agg, err := store.Aggregate(usage.Filter{From: &hour, To: &hour}, "hour", nil)
		if err != nil || agg.Totals.Requests != 1 {
			t.Fatalf("aggregate after prune requests=%d err=%v", agg.Totals.Requests, err)
		}
	})
}

func TestDimensionReadersRejectAmbiguousLegacyKey(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	hour := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	putAgg(t, db, manualLegacyAggKey(hour.Format(hourLayout), "openai", "team|api", "model", "upstream"),
		AggCell{RecordCount: 2, Requests: 2, Success: 1, InputTokens: 10})

	readers := []struct {
		name string
		run  func() error
	}{
		{"SumByClientRange", func() error {
			_, err := store.SumByClientRange("team|api", hour, hour)
			return err
		}},
		{"SumByClientHourlyTargets", func() error {
			_, err := store.SumByClientHourlyTargets("team|api", hour, hour)
			return err
		}},
		{"SumByClientTargetRange", func() error {
			_, err := store.SumByClientTargetRange("team|api", hour, hour)
			return err
		}},
		{"Aggregate", func() error {
			result, err := store.Aggregate(usage.Filter{From: &hour, To: &hour}, "hour", nil)
			if err == nil && result.Totals.Requests != 0 {
				t.Fatalf("aggregate returned partial totals on an ambiguous legacy key")
			}
			return err
		}},
	}
	for _, reader := range readers {
		t.Run(reader.name, func(t *testing.T) {
			if err := reader.run(); err == nil {
				t.Fatal("ambiguous legacy key was silently skipped")
			}
		})
	}

	total, success, err := store.Count(hour, hour)
	if err != nil {
		t.Fatalf("count must stay available for ambiguous legacy cells: %v", err)
	}
	if total != 2 || success != 1 {
		t.Fatalf("count ambiguous legacy=(%d,%d), want (2,1)", total, success)
	}
}

// TestMalformedAggregateCellsDimensionReadersFailClosed keeps dimension
// readers (quota fallback) fail-closed while the display-only Count skips the
// cell with a warning, matching v1.0.1 read tolerance (#35).
func TestMalformedAggregateCellsDimensionReadersFailClosed(t *testing.T) {
	hour := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	hourKey := hour.Format(hourLayout)
	for _, tc := range []struct {
		name  string
		key   string
		value []byte
	}{
		{"legacy cell JSON", manualLegacyAggKey(hourKey, "openai", "alice", "gpt-4", "upstream"), []byte("notjson")},
		{"v2 cell JSON", manualV2AggKey(hour, "openai", "alice", "gpt-4", "upstream"), []byte("notjson")},
		{"v2 non-hex", "v2|" + hourKey + "|zz|616c696365|6d|7570", []byte(`{"record_count":1}`)},
		{"v2 uppercase hex", "v2|" + hourKey + "|4F70656E4149|616c696365|6d|7570", []byte(`{"record_count":1}`)},
		{"v2 wrong segment count", "v2|" + hourKey + "|6f70656e6169|616c696365", []byte(`{"record_count":1}`)},
		{"v2 invalid hour", "v2|2026-10-21T09x|6f70656e6169|616c696365|6d|7570", []byte(`{"record_count":1}`)},
		{"legacy invalid hour", "2026-10-21T09x|openai|alice|gpt-4|upstream", []byte(`{"record_count":1}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			store := NewUsageStore(db)
			putAggRaw(t, db, tc.key, tc.value)
			if _, err := store.SumByClientRange("alice", hour, hour); err == nil {
				t.Fatal("dimension reader accepted a malformed cell")
			}
			total, success, err := store.Count(hour, hour)
			if err != nil || total != 0 || success != 0 {
				t.Fatalf("count malformed cell=(%d,%d,%v), want (0,0,nil) with the cell skipped", total, success, err)
			}
		})
	}
}

// TestReceiptWatermarkMarksNonCanonicalHour covers keys whose physical ordering
// places them outside any range scan. The one-time watermark scan sees the
// whole bucket; it cannot count such a cell, so the hour the key claims becomes
// untrusted instead of blocking every explicit write (#35).
func TestReceiptWatermarkMarksNonCanonicalHour(t *testing.T) {
	db := testDB(t)
	putAgg(t, db, "2026-10-21T9|openai|alice|gpt-4|upstream", AggCell{RecordCount: 1})
	if err := NewUsageStore(db).EnsureUsageReceipts(); err != nil {
		t.Fatalf("non-canonical hour blocked the migration: %v", err)
	}
	if got, want := receiptWatermark(t, db), time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("watermark=%v, want %v", got, want)
	}
}

func TestQuotaFallbackPropagatesAmbiguousDimensions(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	hour := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	putAgg(t, db, manualLegacyAggKey(hour.Format(hourLayout), "openai", "team|api", "model", "upstream"),
		AggCell{RecordCount: 1, Requests: 1, InputTokens: 10})
	if _, err := store.SumQuotaByClientRange("team|api", hour, hour); err == nil {
		t.Fatal("quota legacy fallback hid an ambiguous usage dimension")
	}
}

func TestBackfillWritesV2AndPreservesCompletedLedger(t *testing.T) {
	db := testDB(t)
	hour := time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
	writeRawEvents(t, db, []usage.Event{{
		Timestamp: hour, ClientName: "alice", Model: "gpt-4", EndpointType: "openai",
		Target: "upstream", InputTokens: 10, StatusCode: 200,
	}})
	if err := BackfillUsageAgg(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	built := indexBucketSnapshot(t, db, BucketUsageAggHourly)
	if len(built) == 0 {
		t.Fatal("backfill wrote no aggregate cells")
	}
	for key := range built {
		if !strings.HasPrefix(key, "v2|") {
			t.Fatalf("first-time backfill wrote a legacy key %q", key)
		}
	}

	// An existing completion marker must leave the ledger untouched, including
	// any legacy cell that is already present.
	putAgg(t, db, manualLegacyAggKey(hour.Format(hourLayout), "openai", "alice", "gpt-4", "upstream"),
		AggCell{RecordCount: 1, Requests: 1})
	before := indexBucketSnapshot(t, db, BucketUsageAggHourly)
	if err := BackfillUsageAgg(db); err != nil {
		t.Fatalf("repeated backfill: %v", err)
	}
	after := indexBucketSnapshot(t, db, BucketUsageAggHourly)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("completed backfill rewrote existing legacy cells")
	}
}

func TestRecordRejectsCorruptAggregateCellWithoutRewriting(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := fixedPlainEvent()
	putAggRaw(t, db, string(aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)), []byte("notjson"))
	before := indexBucketSnapshot(t, db, BucketUsageAggHourly)
	if err := store.Record(event); err == nil {
		t.Fatal("corrupt permanent cell was reset and overwritten")
	}
	raw := 0
	if err := db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket([]byte(BucketUsageEvents)).Cursor()
		for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
			raw++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if raw != 0 {
		t.Fatalf("failed record left %d raw rows", raw)
	}
	if !reflect.DeepEqual(before, indexBucketSnapshot(t, db, BucketUsageAggHourly)) {
		t.Fatal("failed record changed the corrupt permanent cell")
	}
}

func TestLegacyRawRetryDoesNotWriteV2Cell(t *testing.T) {
	db := testDB(t)
	store := NewUsageStore(db)
	event := fixedPlainEvent()
	event.ClientName = "team|api"
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(event.Timestamp, event.RecordID)), data); err != nil {
			return err
		}
		b := tx.Bucket([]byte(BucketUsageAggHourly))
		cell, err := json.Marshal(AggCell{RecordCount: 1, Requests: 1})
		if err != nil {
			return err
		}
		return b.Put([]byte(manualLegacyAggKey(event.Timestamp.UTC().Truncate(time.Hour).Format(hourLayout), event.EndpointType, event.ClientName, event.Model, event.Target)), cell)
	}); err != nil {
		t.Fatal(err)
	}

	added, err := store.RecordOnce(event)
	if err != nil || added {
		t.Fatalf("legacy raw retry: added=%v err=%v", added, err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket([]byte(BucketUsageAggHourly)).Cursor()
		for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
			if strings.HasPrefix(string(k), "v2|") {
				t.Fatalf("legacy raw retry created a v2 cell %q", k)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReceiptWatermarkMixedCellFormats covers the one-time migration when both
// physical key formats coexist and raw detail may already be pruned.
func TestReceiptWatermarkMixedCellFormats(t *testing.T) {
	const (
		endpoint = "claude"
		client   = "team|api"
		model    = "m"
		target   = "anthropic"
	)
	hour := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	hourKey := hour.Format(hourLayout)

	putRaw := func(t *testing.T, db *bolt.DB, event usage.Event) {
		t.Helper()
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte(BucketUsageEvents)).Put([]byte(usageKey(event.Timestamp, event.RecordID)), data)
		}); err != nil {
			t.Fatal(err)
		}
	}
	rawEvent := func(id string) usage.Event {
		return usage.Event{
			RecordID: id, Timestamp: hour.Add(30 * time.Minute), ClientName: client,
			Model: model, EndpointType: endpoint, Target: target, InputTokens: 10, StatusCode: 200,
		}
	}
	putQuota := func(t *testing.T, db *bolt.DB, count int64) {
		t.Helper()
		data, err := json.Marshal(AggCell{RecordCount: count, Requests: count})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte(BucketQuotaUsageAggHourly)).Put(
				quotaAggKey(hour, client, target, model), data)
		}); err != nil {
			t.Fatal(err)
		}
	}
	cellJSON := func(count int64) []byte {
		data, err := json.Marshal(AggCell{RecordCount: count, Requests: count})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	t.Run("legacy and v2 projections preserve two raw rows", func(t *testing.T) {
		db := testDB(t)
		putRaw(t, db, rawEvent(fixedRootRecordID))
		putRaw(t, db, rawEvent(fixedLaterRecordID))
		putAggRaw(t, db, manualLegacyAggKey(hourKey, endpoint, client, model, target), cellJSON(1))
		putAggRaw(t, db, manualV2AggKey(hour, endpoint, client, model, target), cellJSON(1))
		putQuota(t, db, 2)
		resetReceiptTracking(t, db)
		if err := NewUsageStore(db).EnsureUsageReceipts(); err != nil {
			t.Fatalf("mixed migration: %v", err)
		}
		if got := receiptWatermark(t, db); !got.IsZero() {
			t.Fatalf("two raw rows and matching legacy/v2 cells produced watermark %v", got)
		}
	})

	t.Run("missing raw row marks the hour", func(t *testing.T) {
		db := testDB(t)
		putRaw(t, db, rawEvent(fixedRootRecordID))
		putRaw(t, db, rawEvent(fixedLaterRecordID))
		putAggRaw(t, db, manualLegacyAggKey(hourKey, endpoint, client, model, target), cellJSON(1))
		putAggRaw(t, db, manualV2AggKey(hour, endpoint, client, model, target), cellJSON(1))
		putQuota(t, db, 2)
		deleteUsageRawAndIndex(t, db, rawEvent(fixedRootRecordID))
		resetReceiptTracking(t, db)
		if err := NewUsageStore(db).EnsureUsageReceipts(); err != nil {
			t.Fatalf("mixed migration: %v", err)
		}
		want := hour.Add(time.Hour)
		if got := receiptWatermark(t, db); !got.Equal(want) {
			t.Fatalf("watermark=%v, want %v", got, want)
		}
	})

	t.Run("missing rows do not cancel across hours", func(t *testing.T) {
		db := testDB(t)
		nextHour := hour.Add(time.Hour)
		putRaw(t, db, rawEvent(fixedRootRecordID))
		next := rawEvent(fixedLaterRecordID)
		next.Timestamp = nextHour.Add(30 * time.Minute)
		putRaw(t, db, next)
		putAggRaw(t, db, manualLegacyAggKey(hourKey, endpoint, client, model, target), cellJSON(1))
		putAggRaw(t, db, manualLegacyAggKey(nextHour.Format(hourLayout), endpoint, client, model, target), cellJSON(2))
		data, err := json.Marshal(AggCell{RecordCount: 1, Requests: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
			if err := b.Put(quotaAggKey(hour, client, target, model), data); err != nil {
				return err
			}
			more, err := json.Marshal(AggCell{RecordCount: 2, Requests: 2})
			if err != nil {
				return err
			}
			return b.Put(quotaAggKey(nextHour, client, target, model), more)
		}); err != nil {
			t.Fatal(err)
		}
		resetReceiptTracking(t, db)
		if err := NewUsageStore(db).EnsureUsageReceipts(); err != nil {
			t.Fatalf("mixed migration: %v", err)
		}
		want := nextHour.Add(time.Hour)
		if got := receiptWatermark(t, db); !got.Equal(want) {
			t.Fatalf("watermark=%v, want missing rows in %v to mark %v", got, nextHour, want)
		}
	})

	t.Run("missing raw for one v2 dimension is not offset", func(t *testing.T) {
		db := testDB(t)
		putRaw(t, db, rawEvent(fixedRootRecordID))
		putRaw(t, db, rawEvent(fixedLaterRecordID))
		// Two raw rows support D1, but the stored D1 cell claims only one. D2
		// has the same legacy projection and no raw at all. Legacy totals are
		// equal, so only the per-v2 lower bound can detect the missing row.
		putAggRaw(t, db, manualV2AggKey(hour, endpoint, "team|api", "m", target), cellJSON(1))
		if manualLegacyAggKey(hourKey, endpoint, "team|api", "m", target) !=
			manualLegacyAggKey(hourKey, endpoint, "team", "api|m", target) {
			t.Fatal("test premise broken: legacy projections must collide")
		}
		putAggRaw(t, db, manualV2AggKey(hour, endpoint, "team", "api|m", target), cellJSON(1))
		putQuota(t, db, 2)
		resetReceiptTracking(t, db)
		if err := NewUsageStore(db).EnsureUsageReceipts(); err != nil {
			t.Fatalf("mixed migration: %v", err)
		}
		want := hour.Add(time.Hour)
		if got := receiptWatermark(t, db); !got.Equal(want) {
			t.Fatalf("watermark=%v, want missing D2 raw to mark %v", got, want)
		}
	})
}
