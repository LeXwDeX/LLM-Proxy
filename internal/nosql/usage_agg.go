package nosql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// 预聚合层（hourly bucket）
//
// 设计：
//   - key  = "<2006-01-02T15>|<endpoint_type>|<client>|<model>"
//     timestamp 用截断到小时的字典序友好格式（UTC）；分隔符 '|' 不会出现在小时格式串中。
//   - val  = JSON{requests, success, input_tokens, output_tokens, cached_tokens}
//   - 写：Record() 在同一 bbolt 写事务内 read-modify-write 累加。
//   - 读：Summary/Aggregate/Count 仅扫此 bucket，避免百万级明细扫描 + Unmarshal。
//
// 维度选择不含 status_code：只 healthz 关心 success/total 比例，单独存 success 字段足矣，
// 避免 status 维度炸开 cardinality。
//
// 不做 daily bucket：日聚合可由小时桶累加得出，避免写放大。

const (
	// BucketUsageAggHourly stores hourly pre-aggregated usage rollups.
	BucketUsageAggHourly = "usage_agg_hourly"
	// BucketQuotaUsageAggHourly includes actual upstream target for group quotas.
	BucketQuotaUsageAggHourly = "quota_usage_agg_hourly"
	// usageAggBuiltMetaKey marks one-shot backfill completion (v1 schema).
	usageAggBuiltMetaKey = "usage_agg_built_at_v1"
	// hourLayout is the bucket-truncated hour key in UTC.
	hourLayout = "2006-01-02T15"
)

const quotaUsageAggBuiltMetaKey = "quota_usage_agg_built_at_v1"

// AggCell is the persisted value of one hourly aggregation cell.
type AggCell struct {
	RecordCount             int64              `json:"record_count,omitempty"`
	NonBillable             *NonBillableTokens `json:"non_billable,omitempty"`
	IncompleteUsageCount    int64              `json:"incomplete_usage_count,omitempty"`
	IncompleteCostCount     int64              `json:"incomplete_cost_count,omitempty"`
	Requests                int64              `json:"requests"`
	Success                 int64              `json:"success"`
	InputTokens             int64              `json:"input_tokens"`
	OutputTokens            int64              `json:"output_tokens"`
	CachedTokens            int64              `json:"cached_tokens"`
	CacheCreationTokens     int64              `json:"cache_creation_tokens"`
	ExplicitCacheReadTokens int64              `json:"explicit_cache_read_tokens,omitempty"`
	ReasoningTokens         int64              `json:"reasoning_tokens,omitempty"`
	CacheCreation5mTokens   int64              `json:"cache_creation_5m_tokens,omitempty"`
	CacheCreation1hTokens   int64              `json:"cache_creation_1h_tokens,omitempty"`
	ToolUsePromptTokens     int64              `json:"tool_use_prompt_tokens,omitempty"`
	ImageInputTokens        int64              `json:"image_input_tokens,omitempty"`
	CachedImageInputTokens  int64              `json:"cached_image_input_tokens,omitempty"`
	ImageOutputTokens       int64              `json:"image_output_tokens,omitempty"`
}

// NonBillableTokens preserves observed usage excluded from the bill. A missing
// subset on legacy cells means every observed token remains billable.
type NonBillableTokens struct {
	InputTokens             int64 `json:"input_tokens,omitempty"`
	OutputTokens            int64 `json:"output_tokens,omitempty"`
	CachedTokens            int64 `json:"cached_tokens,omitempty"`
	CacheCreationTokens     int64 `json:"cache_creation_tokens,omitempty"`
	ExplicitCacheReadTokens int64 `json:"explicit_cache_read_tokens,omitempty"`
	CacheCreation5mTokens   int64 `json:"cache_creation_5m_tokens,omitempty"`
	CacheCreation1hTokens   int64 `json:"cache_creation_1h_tokens,omitempty"`
	ImageInputTokens        int64 `json:"image_input_tokens,omitempty"`
	CachedImageInputTokens  int64 `json:"cached_image_input_tokens,omitempty"`
	ImageOutputTokens       int64 `json:"image_output_tokens,omitempty"`
}

func (cell AggCell) recordCount() int64 {
	if cell.RecordCount > 0 {
		return cell.RecordCount
	}
	return cell.Requests
}

func (cell *AggCell) addNonBillable(evt usage.Event) {
	if !evt.NonBillable {
		return
	}
	if cell.NonBillable == nil {
		cell.NonBillable = &NonBillableTokens{}
	}
	cell.NonBillable.InputTokens += evt.InputTokens
	cell.NonBillable.OutputTokens += evt.OutputTokens
	cell.NonBillable.CachedTokens += evt.CachedTokens
	cell.NonBillable.CacheCreationTokens += evt.CacheCreationTokens
	cell.NonBillable.ExplicitCacheReadTokens += evt.ExplicitCacheReadTokens
	cell.NonBillable.CacheCreation5mTokens += evt.CacheCreation5mTokens
	cell.NonBillable.CacheCreation1hTokens += evt.CacheCreation1hTokens
	cell.NonBillable.ImageInputTokens += evt.ImageInputTokens
	cell.NonBillable.CachedImageInputTokens += evt.CachedImageInputTokens
	cell.NonBillable.ImageOutputTokens += evt.ImageOutputTokens
}

// aggKey builds the composite key for an aggregation cell.
//
// Key format: hour|endpoint_type|client|model|target (5 segments).
// Legacy keys (pre-B1) carry only 4 segments without target; parseAggKey
// accepts both so historical data keeps serving reads. Legacy cells resolve
// to target="" which lands in the "ungrouped" bucket for group statistics.
func aggKey(t time.Time, endpointType, client, model, target string) []byte {
	hour := t.UTC().Truncate(time.Hour).Format(hourLayout)
	et := strings.ToLower(strings.TrimSpace(endpointType))
	c := strings.TrimSpace(client)
	m := strings.ToLower(strings.TrimSpace(model))
	tg := strings.ToLower(strings.TrimSpace(target))
	if c == "" {
		c = "unknown"
	}
	if m == "" {
		m = "unknown"
	}
	if tg == "" {
		tg = "unknown"
	}
	// Note: et may be empty (legacy events without endpoint_type). Keep as empty segment.
	return []byte(hour + "|" + et + "|" + c + "|" + m + "|" + tg)
}

// parseAggKey decomposes a key back into (hour, endpoint_type, client, model, target).
// Returns ok=false if the key is malformed. 4-segment legacy keys (no target)
// yield target="" — the honest value for pre-B1 history, grouped as ungrouped.
func parseAggKey(key []byte) (hour time.Time, endpointType, client, model, target string, ok bool) {
	parts := strings.Split(string(key), "|")
	switch len(parts) {
	case 4:
		// legacy pre-B1 key: hour|et|client|model
	case 5:
		// current key: hour|et|client|model|target
	default:
		return time.Time{}, "", "", "", "", false
	}
	t, err := time.ParseInLocation(hourLayout, parts[0], time.UTC)
	if err != nil {
		return time.Time{}, "", "", "", "", false
	}
	if len(parts) == 5 {
		target = parts[4]
	}
	return t, parts[1], parts[2], parts[3], target, true
}

// hourPrefix returns the hour-bucket-truncated key prefix for time t (used as Seek bound).
func hourPrefix(t time.Time) []byte {
	return []byte(t.UTC().Truncate(time.Hour).Format(hourLayout))
}

// isSuccessStatus mirrors the existing Count() definition: 0 < code < 500.
// StatusCode == 0 (legacy/missing) is treated as NOT success (conservative).
func isSuccessStatus(code int) bool {
	return code > 0 && code < 500
}

// bumpAgg performs read-modify-write accumulation for one event in the agg bucket.
// MUST be called inside a write transaction.
func bumpAgg(tx *bolt.Tx, evt usage.Event) error {
	b := tx.Bucket([]byte(BucketUsageAggHourly))
	if b == nil {
		return fmt.Errorf("agg bucket missing")
	}
	key := aggKey(evt.Timestamp, evt.EndpointType, evt.ClientName, evt.Model, evt.Target)

	var cell AggCell
	if existing := b.Get(key); existing != nil {
		if err := json.Unmarshal(existing, &cell); err != nil {
			// Corrupt cell: reset rather than abort (defensive).
			cell = AggCell{}
		}
	}
	cell.RecordCount = cell.recordCount() + 1
	cell.Requests += usage.RequestWeight(evt)
	cell.addNonBillable(evt)
	if isSuccessStatus(evt.StatusCode) {
		cell.Success += usage.RequestWeight(evt)
	}
	cell.InputTokens += evt.InputTokens
	cell.OutputTokens += evt.OutputTokens
	cell.CachedTokens += evt.CachedTokens
	if evt.UsageStatus == usage.UsageStatusIncomplete || evt.UsageStatus == usage.UsageStatusConflict {
		cell.IncompleteUsageCount++
	}
	if evt.CostIncomplete {
		cell.IncompleteCostCount++
	}
	cell.CacheCreationTokens += evt.CacheCreationTokens
	cell.ExplicitCacheReadTokens += evt.ExplicitCacheReadTokens
	cell.ReasoningTokens += evt.ReasoningTokens
	cell.CacheCreation5mTokens += evt.CacheCreation5mTokens
	cell.CacheCreation1hTokens += evt.CacheCreation1hTokens
	cell.ToolUsePromptTokens += evt.ToolUsePromptTokens
	cell.ImageInputTokens += evt.ImageInputTokens
	cell.CachedImageInputTokens += evt.CachedImageInputTokens
	cell.ImageOutputTokens += evt.ImageOutputTokens

	data, err := json.Marshal(cell)
	if err != nil {
		return fmt.Errorf("marshal agg cell: %w", err)
	}
	return b.Put(key, data)
}

// quotaKeyEncode percent-encodes the reserved separators ('|' and ':') and the
// escape char ('%') so quota aggregation composite keys can be split
// unambiguously. Percent-encoding (rather than backslash escaping) ensures no
// raw '|' or ':' remains in the encoded value, so strings.SplitN on the raw
// separator cannot split an escaped occurrence.
func quotaKeyEncode(s string) string {
	if !strings.ContainsAny(s, "|:%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '|':
			b.WriteString("%7C")
		case ':':
			b.WriteString("%3A")
		case '%':
			b.WriteString("%25")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// quotaKeyDecode reverses quotaKeyEncode.
func quotaKeyDecode(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			switch s[i+1 : i+3] {
			case "7C":
				b.WriteByte('|')
				i += 2
			case "3A":
				b.WriteByte(':')
				i += 2
			case "25":
				b.WriteByte('%')
				i += 2
			default:
				b.WriteByte(s[i])
			}
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// SplitQuotaGroupKey splits a composite target/model key (as produced by
// SumQuotaByClientRange) back into the raw target and model values.
func SplitQuotaGroupKey(key string) (string, string) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return "", key
	}
	return quotaKeyDecode(parts[0]), quotaKeyDecode(parts[1])
}

// quotaAggKey keeps the actual selected target, which is required to recover
// model-group quota usage after a restart or failover.
func quotaAggKey(t time.Time, client, target, model string) []byte {
	hour := t.UTC().Truncate(time.Hour).Format(hourLayout)
	return []byte(hour + "|" + quotaKeyEncode(strings.TrimSpace(client)) + "|" + quotaKeyEncode(strings.ToLower(strings.TrimSpace(target))) + "|" + quotaKeyEncode(strings.ToLower(strings.TrimSpace(model))))
}

func bumpQuotaAgg(tx *bolt.Tx, evt usage.Event) error {
	b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
	if b == nil {
		return fmt.Errorf("quota agg bucket missing")
	}
	key := quotaAggKey(evt.Timestamp, evt.ClientName, evt.Target, evt.Model)
	var cell AggCell
	if v := b.Get(key); v != nil {
		if err := json.Unmarshal(v, &cell); err != nil {
			return fmt.Errorf("decode quota agg cell %q: %w", key, err)
		}
	}
	cell.RecordCount = cell.recordCount() + 1
	cell.Requests += usage.RequestWeight(evt)
	cell.addNonBillable(evt)
	if isSuccessStatus(evt.StatusCode) {
		cell.Success += usage.RequestWeight(evt)
	}
	cell.InputTokens += evt.InputTokens
	cell.OutputTokens += evt.OutputTokens
	cell.CachedTokens += evt.CachedTokens
	if evt.UsageStatus == usage.UsageStatusIncomplete || evt.UsageStatus == usage.UsageStatusConflict {
		cell.IncompleteUsageCount++
	}
	if evt.CostIncomplete {
		cell.IncompleteCostCount++
	}
	cell.CacheCreationTokens += evt.CacheCreationTokens
	cell.ExplicitCacheReadTokens += evt.ExplicitCacheReadTokens
	cell.ReasoningTokens += evt.ReasoningTokens
	cell.CacheCreation5mTokens += evt.CacheCreation5mTokens
	cell.CacheCreation1hTokens += evt.CacheCreation1hTokens
	cell.ToolUsePromptTokens += evt.ToolUsePromptTokens
	cell.ImageInputTokens += evt.ImageInputTokens
	cell.CachedImageInputTokens += evt.CachedImageInputTokens
	cell.ImageOutputTokens += evt.ImageOutputTokens
	data, err := json.Marshal(cell)
	if err != nil {
		return err
	}
	return b.Put(key, data)
}

// SumQuotaByClientRange returns usage grouped by actual target and upstream model.
func (s *UsageStore) SumQuotaByClientRange(client string, from, to time.Time) (map[string]UsageTotals, error) {
	result := make(map[string]UsageTotals)
	fromHour, toHour := from.UTC().Truncate(time.Hour).Format(hourLayout), to.UTC().Truncate(time.Hour).Format(hourLayout)
	if toHour < fromHour {
		return result, nil
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		end := toHour + "}"
		for k, v := c.Seek([]byte(fromHour)); k != nil && string(k) <= end; k, v = c.Next() {
			p := strings.SplitN(string(k), "|", 4)
			if len(p) != 4 || p[1] != quotaKeyEncode(strings.TrimSpace(client)) {
				continue
			}
			var cell AggCell
			if json.Unmarshal(v, &cell) != nil {
				continue
			}
			t := result[p[2]+":"+p[3]]
			t.InputTokens += cell.InputTokens
			t.OutputTokens += cell.OutputTokens
			t.CachedTokens += cell.CachedTokens
			t.CacheCreationTokens += cell.CacheCreationTokens
			t.ExplicitCacheReadTokens += cell.ExplicitCacheReadTokens
			t.ReasoningTokens += cell.ReasoningTokens
			t.CacheCreation5mTokens += cell.CacheCreation5mTokens
			t.CacheCreation1hTokens += cell.CacheCreation1hTokens
			t.ToolUsePromptTokens += cell.ToolUsePromptTokens
			t.ImageInputTokens += cell.ImageInputTokens
			t.CachedImageInputTokens += cell.CachedImageInputTokens
			t.ImageOutputTokens += cell.ImageOutputTokens
			result[p[2]+":"+p[3]] = t
		}
		return nil
	})
	if err != nil || len(result) != 0 {
		return result, err
	}

	// Databases created before the target-aware aggregation was introduced may
	// have an empty quota bucket until startup backfill runs. Preserve their
	// personal-quota accounting by treating legacy cells as an unknown target.
	legacy, err := s.SumByClientRange(client, from, to)
	if err != nil {
		return result, err
	}
	for key, totals := range legacy {
		_, model := splitLegacyQuotaKey(key)
		groupKey := ":" + quotaKeyEncode(model)
		t := result[groupKey]
		t.InputTokens += totals.InputTokens
		t.OutputTokens += totals.OutputTokens
		t.CachedTokens += totals.CachedTokens
		t.CacheCreationTokens += totals.CacheCreationTokens
		t.ExplicitCacheReadTokens += totals.ExplicitCacheReadTokens
		t.ReasoningTokens += totals.ReasoningTokens
		t.CacheCreation5mTokens += totals.CacheCreation5mTokens
		t.CacheCreation1hTokens += totals.CacheCreation1hTokens
		t.ToolUsePromptTokens += totals.ToolUsePromptTokens
		t.ImageInputTokens += totals.ImageInputTokens
		t.CachedImageInputTokens += totals.CachedImageInputTokens
		t.ImageOutputTokens += totals.ImageOutputTokens
		result[groupKey] = t
	}
	return result, err
}

func splitLegacyQuotaKey(key string) (string, string) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return "", key
	}
	return parts[0], parts[1]
}

// aggIter visits all cells whose hour bucket falls within [from, to] inclusive.
// fn receives the parsed dimensions (target="" for legacy 4-segment keys) and
// the cell value. Returning an error stops iteration.
func aggIter(tx *bolt.Tx, from, to time.Time, fn func(hour time.Time, endpointType, client, model, target string, cell AggCell) error) error {
	b := tx.Bucket([]byte(BucketUsageAggHourly))
	if b == nil {
		return nil
	}
	c := b.Cursor()

	startKey := hourPrefix(from)
	// endPrefix is the inclusive last hour; we stop when key > endPrefix + sentinel.
	// "|" (0x7C) sorts before "}" (0x7D), so endPrefix+"}" is a clean upper bound for
	// any key starting with endPrefix + "|...".
	endStop := append(hourPrefix(to), '}')

	for k, v := c.Seek(startKey); k != nil; k, v = c.Next() {
		if string(k) > string(endStop) {
			break
		}
		hour, et, client, model, target, ok := parseAggKey(k)
		if !ok {
			continue
		}
		var cell AggCell
		if err := json.Unmarshal(v, &cell); err != nil {
			continue
		}
		if err := fn(hour, et, client, model, target, cell); err != nil {
			return err
		}
	}
	return nil
}

const usageBackfillBatchSize = 5000

// scanUsageEventBatches reads usage_events in bounded View transactions. The
// callback runs after each read transaction has closed, so callers can write
// the batch in a separate bounded Update transaction.
func scanUsageEventBatches(db *bolt.DB, fn func([]usage.Event) error) error {
	var lastKey []byte
	for {
		batch := make([]usage.Event, 0, usageBackfillBatchSize)
		exhausted := false
		err := db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(BucketUsageEvents))
			if b == nil {
				exhausted = true
				return nil
			}
			c := b.Cursor()
			var k, v []byte
			if lastKey == nil {
				k, v = c.First()
			} else {
				k, v = c.Seek(lastKey)
				if k != nil && string(k) == string(lastKey) {
					k, v = c.Next()
				}
			}
			for ; k != nil && len(batch) < usageBackfillBatchSize; k, v = c.Next() {
				lastKey = append(lastKey[:0], k...)
				var evt usage.Event
				if err := json.Unmarshal(v, &evt); err != nil {
					continue
				}
				batch = append(batch, evt)
			}
			exhausted = k == nil
			return nil
		})
		if err != nil {
			return err
		}
		if len(batch) > 0 {
			if err := fn(batch); err != nil {
				return err
			}
		}
		if exhausted {
			return nil
		}
	}
}

// BackfillUsageAgg scans all existing usage_events and rebuilds the hourly agg bucket.
// Idempotent: skips if meta marker exists. Safe to run on startup.
//
// Concurrency: bbolt serializes write txs, so concurrent Record() during backfill
// is safe — both paths converge on the same key via read-modify-write accumulation.
// However, to avoid double-counting (an event written AFTER backfill marks the cell
// but BEFORE we commit), backfill happens BEFORE the proxy starts accepting traffic.
func BackfillUsageAgg(db *bolt.DB) error {
	// Already done?
	var done bool
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketMeta))
		if b == nil {
			return nil
		}
		if b.Get([]byte(usageAggBuiltMetaKey)) != nil {
			done = true
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("check agg meta: %w", err)
	}
	if done {
		return nil
	}

	// P11 幂等保障：完成标记与数据写入非原子（分批多事务 flush 后才写 meta）。
	// 若上次进程在 flush 完成后、标记落盘前崩溃，重启会全量重跑，而 bumpAgg
	// 是累加语义 → 历史小时格全部翻倍。因此重建前先清空 agg bucket：重跑即
	// 从零重建，天然幂等。backfill 在代理接流前执行，清空无并发风险。
	err = db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketUsageAggHourly)) != nil {
			if err := tx.DeleteBucket([]byte(BucketUsageAggHourly)); err != nil {
				return err
			}
		}
		_, err := tx.CreateBucketIfNotExists([]byte(BucketUsageAggHourly))
		return err
	})
	if err != nil {
		return fmt.Errorf("reset agg bucket before backfill: %w", err)
	}

	// Stream events and rebuild in batches to keep each write tx bounded.
	// Pattern: read a chunk in a View tx, commit it via an Update tx, advance lastKey.
	var processed int64
	err = scanUsageEventBatches(db, func(batch []usage.Event) error {
		err := db.Update(func(tx *bolt.Tx) error {
			for _, evt := range batch {
				if err := bumpAgg(tx, evt); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		processed += int64(len(batch))
		return nil
	})
	if err != nil {
		return fmt.Errorf("backfill agg batches: %w", err)
	}

	// Mark done.
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketMeta))
		marker, _ := json.Marshal(map[string]any{
			"built_at":  time.Now().UTC().Format(time.RFC3339),
			"processed": processed,
		})
		return b.Put([]byte(usageAggBuiltMetaKey), marker)
	})
	if err != nil {
		return fmt.Errorf("write agg meta: %w", err)
	}

	return nil
}

// BackfillQuotaUsageAgg rebuilds the target-aware quota aggregation once.
func BackfillQuotaUsageAgg(db *bolt.DB) error {
	var done bool
	err := db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(BucketMeta)); b != nil {
			done = b.Get([]byte(quotaUsageAggBuiltMetaKey)) != nil
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("check quota agg meta: %w", err)
	}
	if done {
		return nil
	}

	// A retry after a partial run must rebuild from zero because batches and
	// the completion marker are committed in separate transactions.
	if err := db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(BucketQuotaUsageAggHourly)) != nil {
			if err := tx.DeleteBucket([]byte(BucketQuotaUsageAggHourly)); err != nil {
				return err
			}
		}
		_, err := tx.CreateBucketIfNotExists([]byte(BucketQuotaUsageAggHourly))
		return err
	}); err != nil {
		return fmt.Errorf("reset quota agg bucket before backfill: %w", err)
	}

	var processed int64
	err = scanUsageEventBatches(db, func(batch []usage.Event) error {
		if err := db.Update(func(tx *bolt.Tx) error {
			for _, evt := range batch {
				if err := bumpQuotaAgg(tx, evt); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		processed += int64(len(batch))
		return nil
	})
	if err != nil {
		return fmt.Errorf("backfill quota agg batches: %w", err)
	}

	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketMeta))
		if b == nil {
			return fmt.Errorf("meta bucket missing")
		}
		marker, err := json.Marshal(map[string]any{
			"built_at":  time.Now().UTC().Format(time.RFC3339),
			"processed": processed,
		})
		if err != nil {
			return err
		}
		return b.Put([]byte(quotaUsageAggBuiltMetaKey), marker)
	})
}

// UsageTotals 单模型 token 累计。
type UsageTotals struct {
	InputTokens             int64
	OutputTokens            int64
	CachedTokens            int64
	CacheCreationTokens     int64
	ExplicitCacheReadTokens int64 `json:"explicit_cache_read_tokens,omitempty"`
	ReasoningTokens         int64 `json:"reasoning_tokens,omitempty"`
	CacheCreation5mTokens   int64 `json:"cache_creation_5m_tokens,omitempty"`
	CacheCreation1hTokens   int64 `json:"cache_creation_1h_tokens,omitempty"`
	ToolUsePromptTokens     int64 `json:"tool_use_prompt_tokens,omitempty"`
	ImageInputTokens        int64 `json:"image_input_tokens,omitempty"`
	CachedImageInputTokens  int64 `json:"cached_image_input_tokens,omitempty"`
	ImageOutputTokens       int64 `json:"image_output_tokens,omitempty"`
}

// Built-in model-group buckets for Target-aware consumption statistics.
const (
	// CompanyGeneralUsageModelGroup is the fallback for a named Target whose
	// global model-group catalog has no whole-target binding.
	CompanyGeneralUsageModelGroup = "company_general"
	// UnknownTargetUsageModelGroup is the fallback for usage events that lack a
	// Target entirely (legacy/empty historical events).
	UnknownTargetUsageModelGroup = "unknown_target"
)

// UsageTargetResolution describes how a historical target is classified for
// consumption-statistics reporting.
type UsageTargetResolution int

const (
	UsageTargetUnknown UsageTargetResolution = iota
	UsageTargetUnconfigured
	UsageTargetConfigured
	UsageTargetDeleted
)

type UsageTargetOwnership struct {
	Resolution    UsageTargetResolution
	ModelGroup    string
	TargetDeleted bool
}

func normalizeUsageTarget(target string) string { return strings.ToLower(strings.TrimSpace(target)) }

func normalizeUsageModelGroup(group string) string { return strings.TrimSpace(group) }

func modelGroupAllowed(group string, selected []string) bool {
	if len(selected) == 0 {
		return true
	}
	want := normalizeUsageModelGroup(group)
	for _, item := range selected {
		if strings.EqualFold(want, normalizeUsageModelGroup(item)) {
			return true
		}
	}
	return false
}

// usageOwnershipSnapshot is loaded once per report. History validation, totals
// and detail attribution must use the same ownership even if the catalog changes.
type usageOwnershipSnapshot struct {
	existing   map[string]struct{}
	groupIndex *GroupOwnershipIndex
}

func loadUsageOwnership(targets *TargetStore, globalGroups ...*ModelGroupStore) (usageOwnershipSnapshot, error) {
	snapshot := usageOwnershipSnapshot{existing: make(map[string]struct{})}
	if targets != nil {
		items, err := targets.List()
		if err != nil {
			return snapshot, err
		}
		for _, target := range items {
			snapshot.existing[normalizeUsageTarget(target.Name)] = struct{}{}
		}
	}
	var err error
	snapshot.groupIndex, err = targetGroupIndex(globalGroups...)
	return snapshot, err
}

func (snapshot usageOwnershipSnapshot) resolve(target, model string) UsageTargetOwnership {
	return resolveUsageOwnership(target, model, snapshot.groupIndex, snapshot.existing)
}

func (snapshot usageOwnershipSnapshot) pairFilter(selected []string) costPairFilter {
	if len(selected) == 0 {
		return nil
	}
	return func(target, model string) bool {
		return modelGroupAllowed(snapshot.resolve(target, model).ModelGroup, selected)
	}
}

type TargetModelCell struct {
	Hour   time.Time
	Target string
	Client string
	Model  string
	AggCell
}

// SumTargetModelRange returns target/model cells from the target-aware hourly
// bucket. It is a neutral reporting query and does not apply quota matching.
func (s *UsageStore) SumTargetModelRange(filter usage.Filter) ([]TargetModelCell, error) {
	from := time.Time{}
	if filter.From != nil {
		from = filter.From.UTC()
	}
	to := time.Now().UTC().Add(time.Hour)
	if filter.To != nil {
		to = filter.To.UTC()
	}
	fromHour := from.Truncate(time.Hour).Format(hourLayout)
	toHour := to.UTC().Truncate(time.Hour).Format(hourLayout)
	client := quotaKeyEncode(strings.TrimSpace(filter.ClientName))
	model := strings.ToLower(strings.TrimSpace(filter.Model))
	result := make([]TargetModelCell, 0)
	if toHour < fromHour {
		return result, nil
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketQuotaUsageAggHourly))
		if b == nil {
			return nil
		}
		end := append([]byte(toHour), '}')
		cursor := b.Cursor()
		for k, v := cursor.Seek([]byte(fromHour)); k != nil && string(k) <= string(end); k, v = cursor.Next() {
			parts := strings.SplitN(string(k), "|", 4)
			if len(parts) != 4 || (client != "" && parts[1] != client) {
				continue
			}
			currentModel := quotaKeyDecode(parts[3])
			if model != "" && currentModel != model {
				continue
			}
			var cell AggCell
			if err := json.Unmarshal(v, &cell); err != nil {
				continue
			}
			hour, _ := time.Parse(hourLayout, parts[0])
			result = append(result, TargetModelCell{Hour: hour, Target: quotaKeyDecode(parts[2]), Client: quotaKeyDecode(parts[1]), Model: currentModel, AggCell: cell})
		}
		return nil
	})
	return result, err
}

// AggregateModelGroups resolves target ownership at query time and aggregates
// target-aware hourly cells. The global model-group catalog is the source of
// truth; ownership is resolved per (target, model) with exact bindings winning
// over whole-target bindings, mirroring quota-side group matching. The variadic
// form preserves compatibility for callers that do not supply a catalog store.
func (s *UsageStore) AggregateModelGroups(filter usage.Filter, costs usage.RateResolver, targets *TargetStore, globalGroups ...*ModelGroupStore) (usage.ModelGroupUsageResult, error) {
	ownership, err := loadUsageOwnership(targets, globalGroups...)
	if err != nil {
		return usage.ModelGroupUsageResult{}, err
	}
	data, err := s.readCostUnitsInScope(context.Background(), filter, costs, false, nil, costReadHooks{}, ownership.pairFilter(filter.ModelGroups))
	if err != nil {
		return usage.ModelGroupUsageResult{}, err
	}
	return s.aggregateModelGroupCostUnits(filter, data.units, ownership)
}

func (s *UsageStore) aggregateModelGroupCostUnits(filter usage.Filter, cells []CostUnit, snapshot usageOwnershipSnapshot) (usage.ModelGroupUsageResult, error) {
	result := usage.ModelGroupUsageResult{}
	if filter.From != nil {
		result.From = filter.From.UTC()
	}
	if filter.To != nil {
		result.To = filter.To.UTC()
	}

	type aggregate struct {
		group   string
		target  string
		model   string
		deleted bool
		totals  usage.Totals
	}
	groupTotals := make(map[string]*usage.Totals)
	groupModels := make(map[string]*usage.ModelGroupModelTotals)
	modelTargets := make(map[string]map[string]struct{})
	details := make(map[string]*aggregate)
	groupTargets := make(map[string]map[string]struct{})
	for _, cell := range cells {
		ownership := snapshot.resolve(cell.Target, cell.Model)
		group := ownership.ModelGroup
		if !modelGroupAllowed(group, filter.ModelGroups) {
			continue
		}
		deleted := ownership.TargetDeleted
		cost := cell.CostUSD
		groupTotal := groupTotals[group]
		if groupTotal == nil {
			groupTotal = &usage.Totals{}
			groupTotals[group] = groupTotal
		}
		addSuccessfulCellToTotals(groupTotal, cell.AggCell, cost)

		modelKey := group + "\x00" + cell.Model
		modelRow := groupModels[modelKey]
		if modelRow == nil {
			modelRow = &usage.ModelGroupModelTotals{ModelGroup: group, Model: cell.Model}
			groupModels[modelKey] = modelRow
		}
		addSuccessfulCellToTotals(&modelRow.Totals, cell.AggCell, cost)
		if modelTargets[modelKey] == nil {
			modelTargets[modelKey] = make(map[string]struct{})
		}
		modelTargets[modelKey][normalizeUsageTarget(cell.Target)] = struct{}{}
		if groupTargets[group] == nil {
			groupTargets[group] = make(map[string]struct{})
		}
		groupTargets[group][normalizeUsageTarget(cell.Target)] = struct{}{}

		detailKey := group + "\x00" + normalizeUsageTarget(cell.Target) + "\x00" + cell.Model
		detail := details[detailKey]
		if detail == nil {
			detail = &aggregate{group: group, target: cell.Target, model: cell.Model, deleted: deleted}
			details[detailKey] = detail
		}
		addSuccessfulCellToTotals(&detail.totals, cell.AggCell, cost)
	}

	groupNames := make([]string, 0, len(groupTargets))
	for name := range groupTargets {
		groupNames = append(groupNames, name)
	}
	sort.Strings(groupNames)
	for _, name := range groupNames {
		modelCount := 0
		for key := range groupModels {
			if strings.HasPrefix(key, name+"\x00") {
				modelCount++
			}
		}
		group := usage.ModelGroupUsageGroup{Name: name, TargetCount: len(groupTargets[name]), ModelCount: modelCount}
		if total := groupTotals[name]; total != nil {
			group.Totals = *total
		}
		for key, item := range details {
			if item.group != name || strings.Count(key, "\x00") != 2 {
				continue
			}
			group.ByTargetModel = append(group.ByTargetModel, usage.ModelGroupTargetTotals{Target: item.target, Model: item.model, TargetDeleted: item.deleted, Totals: item.totals})
		}
		sort.Slice(group.ByTargetModel, func(i, j int) bool {
			if group.ByTargetModel[i].Target != group.ByTargetModel[j].Target {
				return group.ByTargetModel[i].Target < group.ByTargetModel[j].Target
			}
			return group.ByTargetModel[i].Model < group.ByTargetModel[j].Model
		})
		result.Groups = append(result.Groups, group)
	}
	modelKeys := make([]string, 0, len(groupModels))
	for key := range groupModels {
		modelKeys = append(modelKeys, key)
	}
	sort.Strings(modelKeys)
	for _, key := range modelKeys {
		row := *groupModels[key]
		row.TargetCount = len(modelTargets[key])
		result.ByModelGroupModel = append(result.ByModelGroupModel, row)
	}
	for _, group := range result.Groups {
		for _, detail := range group.ByTargetModel {
			result.ByTargetModel = append(result.ByTargetModel, usage.TargetModelTotals{
				ModelGroup:    group.Name,
				Target:        detail.Target,
				Model:         detail.Model,
				TargetDeleted: detail.TargetDeleted,
				Totals:        detail.Totals,
			})
		}
	}
	return result, nil
}

// AggregateByModelGroups builds the complete usage aggregate from target-aware
// cells, counting the `requests` field as success events. It is used when a
// model-group filter is active because legacy hourly cells do not retain target
// ownership.
func (s *UsageStore) AggregateByModelGroups(filter usage.Filter, groupBy string, costs usage.RateResolver, targets *TargetStore, globalGroups ...*ModelGroupStore) (usage.AggregateResult, error) {
	return s.aggregateTargetModelCells(filter, groupBy, costs, targets, true, globalGroups...)
}

// AggregateByModelGroupsAllRequests is like AggregateByModelGroups but counts the
// legacy `requests` field as total requests (not success events), preserving the
// no-filter consumption page's historical request-count semantics while still
// pricing by (target, model, hour).
func (s *UsageStore) AggregateByModelGroupsAllRequests(filter usage.Filter, groupBy string, costs usage.RateResolver, targets *TargetStore, globalGroups ...*ModelGroupStore) (usage.AggregateResult, error) {
	return s.aggregateTargetModelCells(filter, groupBy, costs, targets, false, globalGroups...)
}

func (s *UsageStore) aggregateTargetModelCells(filter usage.Filter, groupBy string, costs usage.RateResolver, targets *TargetStore, successRequests bool, globalGroups ...*ModelGroupStore) (usage.AggregateResult, error) {
	return s.aggregateTargetModelCellsRead(filter, groupBy, costs, targets, successRequests, costReadHooks{}, globalGroups...)
}

func (s *UsageStore) aggregateTargetModelCellsRead(filter usage.Filter, groupBy string, costs usage.RateResolver, targets *TargetStore, successRequests bool, hooks costReadHooks, globalGroups ...*ModelGroupStore) (usage.AggregateResult, error) {
	groupBy = usage.NormalizeGroupBy(groupBy)
	result := usage.AggregateResult{GroupBy: groupBy}
	if filter.From != nil {
		result.From = filter.From.UTC()
	}
	if filter.To != nil {
		result.To = filter.To.UTC()
	}
	snapshot, err := loadUsageOwnership(targets, globalGroups...)
	if err != nil {
		return result, err
	}
	data, err := s.readCostUnitsInScope(context.Background(), filter, costs, false, nil, hooks, snapshot.pairFilter(filter.ModelGroups))
	if err != nil {
		return result, err
	}
	cells := data.units
	buckets := make(map[time.Time]*usage.Bucket)
	byClient := make(map[string]*usage.DimensionTotals)
	byModel := make(map[string]*usage.DimensionTotals)
	for _, cell := range cells {
		ownership := snapshot.resolve(cell.Target, cell.Model)
		if !modelGroupAllowed(ownership.ModelGroup, filter.ModelGroups) {
			continue
		}
		cost := cell.CostUSD
		if successRequests {
			addSuccessfulCellToTotals(&result.Totals, cell.AggCell, cost)
		} else {
			addCellToTotals(&result.Totals, cell.AggCell, cost)
		}
		start := usage.BucketStartFor(cell.Hour, groupBy)
		bucket := buckets[start]
		if bucket == nil {
			bucket = &usage.Bucket{BucketStart: start, BucketEnd: start.Add(usage.StepDuration(groupBy))}
			buckets[start] = bucket
		}
		if successRequests {
			addSuccessfulCellToTotals(&bucket.Totals, cell.AggCell, cost)
		} else {
			addCellToTotals(&bucket.Totals, cell.AggCell, cost)
		}
		client := cell.Client
		if client == "" {
			client = "unknown"
		}
		if byClient[client] == nil {
			byClient[client] = &usage.DimensionTotals{Key: client}
		}
		if successRequests {
			addSuccessfulCellToTotals(&byClient[client].Totals, cell.AggCell, cost)
		} else {
			addCellToTotals(&byClient[client].Totals, cell.AggCell, cost)
		}
		model := cell.Model
		if model == "" {
			model = "unknown"
		}
		if byModel[model] == nil {
			byModel[model] = &usage.DimensionTotals{Key: model}
		}
		if successRequests {
			addSuccessfulCellToTotals(&byModel[model].Totals, cell.AggCell, cost)
		} else {
			addCellToTotals(&byModel[model].Totals, cell.AggCell, cost)
		}
	}
	keys := make([]time.Time, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })
	for _, key := range keys {
		result.Buckets = append(result.Buckets, *buckets[key])
	}
	result.ByClient = usage.SortedDimensions(byClient)
	result.ByModel = usage.SortedDimensions(byModel)
	modelGroups, err := s.aggregateModelGroupCostUnits(filter, cells, snapshot)
	if err != nil {
		return result, err
	}
	result.ByModelGroupModel = modelGroups.ByModelGroupModel
	result.ByTargetModel = modelGroups.ByTargetModel
	return result, nil
}

// targetGroupIndex returns the (target, model) ownership index built from the
// global catalog: exact bindings win over whole-target bindings, mirroring
// quota matching. A nil/absent catalog yields a nil index (Group reports no
// ownership, so every named target falls back to company_general).
func targetGroupIndex(globalGroups ...*ModelGroupStore) (*GroupOwnershipIndex, error) {
	if len(globalGroups) == 0 || globalGroups[0] == nil {
		return nil, nil
	}
	return globalGroups[0].OwnershipIndex()
}

// resolveUsageOwnership classifies a historical target into one of the four
// resolution states. Ownership comes from the catalog index (exact (target,
// model) binding first, then whole-target binding), matching quota-side group
// resolution so statistics and quota attribute the same traffic identically.
func resolveUsageOwnership(target, model string, groupIndex *GroupOwnershipIndex, existing map[string]struct{}) UsageTargetOwnership {
	key := normalizeUsageTarget(target)
	if key == "" {
		return UsageTargetOwnership{Resolution: UsageTargetUnknown, ModelGroup: UnknownTargetUsageModelGroup}
	}
	_, configured := existing[key]
	if group, ok := groupIndex.Group(target, model); ok {
		if configured {
			return UsageTargetOwnership{Resolution: UsageTargetConfigured, ModelGroup: group}
		}
		return UsageTargetOwnership{Resolution: UsageTargetDeleted, ModelGroup: group, TargetDeleted: true}
	}
	if !configured {
		return UsageTargetOwnership{Resolution: UsageTargetDeleted, ModelGroup: CompanyGeneralUsageModelGroup, TargetDeleted: true}
	}
	return UsageTargetOwnership{Resolution: UsageTargetUnconfigured, ModelGroup: CompanyGeneralUsageModelGroup}
}

// SumByClientRange 按 endpoint_type:model 分组聚合指定 client + 时间范围的小时格。
// 用 Cursor Seek 做范围扫描（禁止 ForEach 全量扫后过滤，避免 O(N) 遍历整 bucket）。
// 返回的 map key = "<endpoint_type>:<model>"，legacy 空 endpoint_type 表现为 ":<model>"。
//
// 分组域口径见 internal/nosql/model_groups.go。
func (s *UsageStore) SumByClientRange(client string, from, to time.Time) (map[string]UsageTotals, error) {
	client = strings.TrimSpace(client)
	result := make(map[string]UsageTotals)
	from = from.UTC().Truncate(time.Hour)
	to = to.UTC().Truncate(time.Hour)
	if to.Before(from) {
		return result, nil
	}

	fromHour := from.Format(hourLayout)
	toHour := to.Format(hourLayout)

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageAggHourly))
		if b == nil {
			return nil
		}
		curs := b.Cursor()
		// Upper-bound: same sentinel pattern as aggIter — "}" (ASCII 125) sits strictly
		// after "|" (ASCII 124), so endPrefix+"}" is greater than every key "endPrefix|...".
		endStop := append([]byte(toHour), '}')

		for k, v := curs.Seek([]byte(fromHour)); k != nil; k, v = curs.Next() {
			if string(k) > string(endStop) {
				break
			}
			hour, et, kClient, model, _, ok := parseAggKey(k)
			if !ok {
				continue
			}
			kHour := hour.Format(hourLayout)
			if kHour < fromHour || kHour > toHour {
				if kHour > toHour {
					break
				}
				continue
			}
			if kClient != client {
				continue
			}
			var cell AggCell
			if err := json.Unmarshal(v, &cell); err != nil {
				continue
			}
			groupKey := et + ":" + model
			totals := result[groupKey]
			totals.InputTokens += cell.InputTokens
			totals.OutputTokens += cell.OutputTokens
			totals.CachedTokens += cell.CachedTokens
			totals.CacheCreationTokens += cell.CacheCreationTokens
			totals.ExplicitCacheReadTokens += cell.ExplicitCacheReadTokens
			totals.ReasoningTokens += cell.ReasoningTokens
			totals.CacheCreation5mTokens += cell.CacheCreation5mTokens
			totals.CacheCreation1hTokens += cell.CacheCreation1hTokens
			totals.ToolUsePromptTokens += cell.ToolUsePromptTokens
			totals.ImageInputTokens += cell.ImageInputTokens
			totals.CachedImageInputTokens += cell.CachedImageInputTokens
			totals.ImageOutputTokens += cell.ImageOutputTokens
			result[groupKey] = totals
		}
		return nil
	})
	return result, err
}

// SumByClientHourlyTargets 按 (小时桶, target) 双层分组聚合指定 client 的小时格，
// 内层保留 et:model 差价维度（同 SumByClientTargetRange）。B2 分时计价的
// 计量基础：quota 评估用小时桶作定价时刻，保证跨时段窗口内逐桶按各自生效
// 价率求和（消除"加总 tokens 乘单一价率"的分时误差）。
func (s *UsageStore) SumByClientHourlyTargets(client string, from, to time.Time) (map[string]map[string]map[string]UsageTotals, error) {
	client = strings.TrimSpace(client)
	result := make(map[string]map[string]map[string]UsageTotals)
	from = from.UTC().Truncate(time.Hour)
	to = to.UTC().Truncate(time.Hour)
	if to.Before(from) {
		return result, nil
	}

	fromHour := from.Format(hourLayout)
	toHour := to.Format(hourLayout)

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageAggHourly))
		if b == nil {
			return nil
		}
		curs := b.Cursor()
		endStop := append([]byte(toHour), '}')

		for k, v := curs.Seek([]byte(fromHour)); k != nil; k, v = curs.Next() {
			if string(k) > string(endStop) {
				break
			}
			hour, et, kClient, model, target, ok := parseAggKey(k)
			if !ok {
				continue
			}
			if kClient != client {
				continue
			}
			var cell AggCell
			if err := json.Unmarshal(v, &cell); err != nil {
				continue
			}
			hk := hour.Format(hourLayout)
			byTarget := result[hk]
			if byTarget == nil {
				byTarget = make(map[string]map[string]UsageTotals)
				result[hk] = byTarget
			}
			inner := byTarget[target]
			if inner == nil {
				inner = make(map[string]UsageTotals)
				byTarget[target] = inner
			}
			groupKey := et + ":" + model
			totals := inner[groupKey]
			totals.InputTokens += cell.InputTokens
			totals.OutputTokens += cell.OutputTokens
			totals.CachedTokens += cell.CachedTokens
			totals.CacheCreationTokens += cell.CacheCreationTokens
			totals.ExplicitCacheReadTokens += cell.ExplicitCacheReadTokens
			totals.ReasoningTokens += cell.ReasoningTokens
			totals.CacheCreation5mTokens += cell.CacheCreation5mTokens
			totals.CacheCreation1hTokens += cell.CacheCreation1hTokens
			totals.ToolUsePromptTokens += cell.ToolUsePromptTokens
			totals.ImageInputTokens += cell.ImageInputTokens
			totals.CachedImageInputTokens += cell.CachedImageInputTokens
			totals.ImageOutputTokens += cell.ImageOutputTokens
			inner[groupKey] = totals
		}
		return nil
	})
	return result, err
}
func (s *UsageStore) SumByClientTargetRange(client string, from, to time.Time) (map[string]map[string]UsageTotals, error) {
	client = strings.TrimSpace(client)
	result := make(map[string]map[string]UsageTotals)
	from = from.UTC().Truncate(time.Hour)
	to = to.UTC().Truncate(time.Hour)
	if to.Before(from) {
		return result, nil
	}

	fromHour := from.Format(hourLayout)
	toHour := to.Format(hourLayout)

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageAggHourly))
		if b == nil {
			return nil
		}
		curs := b.Cursor()
		endStop := append([]byte(toHour), '}')

		for k, v := curs.Seek([]byte(fromHour)); k != nil; k, v = curs.Next() {
			if string(k) > string(endStop) {
				break
			}
			hour, et, kClient, model, target, ok := parseAggKey(k)
			_ = hour
			if !ok {
				continue
			}
			kHour := hour.Format(hourLayout)
			if kHour < fromHour || kHour > toHour {
				if kHour > toHour {
					break
				}
				continue
			}
			if kClient != client {
				continue
			}
			var cell AggCell
			if err := json.Unmarshal(v, &cell); err != nil {
				continue
			}
			inner := result[target]
			if inner == nil {
				inner = make(map[string]UsageTotals)
				result[target] = inner
			}
			groupKey := et + ":" + model
			totals := inner[groupKey]
			totals.InputTokens += cell.InputTokens
			totals.OutputTokens += cell.OutputTokens
			totals.CachedTokens += cell.CachedTokens
			totals.CacheCreationTokens += cell.CacheCreationTokens
			totals.ExplicitCacheReadTokens += cell.ExplicitCacheReadTokens
			totals.ReasoningTokens += cell.ReasoningTokens
			totals.CacheCreation5mTokens += cell.CacheCreation5mTokens
			totals.CacheCreation1hTokens += cell.CacheCreation1hTokens
			totals.ToolUsePromptTokens += cell.ToolUsePromptTokens
			totals.ImageInputTokens += cell.ImageInputTokens
			totals.CachedImageInputTokens += cell.CachedImageInputTokens
			totals.ImageOutputTokens += cell.ImageOutputTokens
			inner[groupKey] = totals
		}
		return nil
	})
	return result, err
}
