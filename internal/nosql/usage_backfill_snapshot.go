package nosql

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

const usageBackfillSnapshotBucket = "usage_aggregate_backfill_snapshots_v1"
const usageBackfillStagePrefix = "usage_aggregate_backfill_stage_v1:"
const usageBackfillStageOwnedPrefix = "usage_aggregate_backfill_stage_owned_v1:"

func backfillSourceDigest(tx *bolt.Tx, source string) ([sha256.Size]byte, error) {
	h := sha256.New()
	b := tx.Bucket([]byte(source))
	if b == nil {
		h.Write([]byte{0})
		var sum [sha256.Size]byte
		copy(sum[:], h.Sum(nil))
		return sum, nil
	}
	h.Write([]byte{1})
	err := b.ForEach(func(k, v []byte) error {
		if v == nil && b.Bucket(k) != nil {
			return fmt.Errorf("aggregate source %q contains a nested bucket", source)
		}
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(k)))
		h.Write(length[:])
		h.Write(k)
		binary.BigEndian.PutUint64(length[:], uint64(len(v)))
		h.Write(length[:])
		h.Write(v)
		return nil
	})
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum, err
}

func prepareUsageBackfill(tx *bolt.Tx, source string) (uint64, [sha256.Size]byte, string, error) {
	var zero [sha256.Size]byte
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil {
		return 0, zero, "", fmt.Errorf("backfill meta bucket missing")
	}
	stageKey := []byte(usageBackfillStagePrefix + source)
	ownedKey := []byte(usageBackfillStageOwnedPrefix + source)
	stage := tx.Bucket(stageKey)
	owned := meta.Get(ownedKey)
	_, ownerErr := uuid.Parse(string(owned))
	if (stage == nil && owned != nil) || (stage != nil && ownerErr != nil) {
		return 0, zero, "", fmt.Errorf("backfill staging ownership cannot be verified")
	}
	if err := preserveBackfillIdentityBoundary(tx); err != nil {
		return 0, zero, "", err
	}
	if err := snapshotUsageAggregation(tx, source); err != nil {
		return 0, zero, "", err
	}
	if err := preserveBackfillAccounting(tx, source); err != nil {
		return 0, zero, "", err
	}
	digest, err := backfillSourceDigest(tx, source)
	if err != nil {
		return 0, zero, "", err
	}
	if stage != nil {
		if err := tx.DeleteBucket(stageKey); err != nil {
			return 0, zero, "", err
		}
	}
	if _, err := tx.CreateBucket(stageKey); err != nil {
		return 0, zero, "", err
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return 0, zero, "", err
	}
	token := id.String()
	if err := meta.Put(ownedKey, []byte(token)); err != nil {
		return 0, zero, "", err
	}
	return rawRevision(tx), digest, token, nil
}

func backfillStage(tx *bolt.Tx, source, token string) (*bolt.Bucket, error) {
	meta := tx.Bucket([]byte(BucketMeta))
	stage := tx.Bucket([]byte(usageBackfillStagePrefix + source))
	if meta == nil || stage == nil || string(meta.Get([]byte(usageBackfillStageOwnedPrefix+source))) != token {
		return nil, fmt.Errorf("backfill staging ownership cannot be verified")
	}
	return stage, nil
}

func finishUsageBackfill(tx *bolt.Tx, source, marker, token string, revision uint64, original [sha256.Size]byte, processed int64) error {
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil || rawRevision(tx) != revision {
		return fmt.Errorf("backfill raw history changed during rebuild")
	}
	digest, err := backfillSourceDigest(tx, source)
	if err != nil {
		return err
	}
	if digest != original {
		return fmt.Errorf("backfill aggregate source changed during rebuild")
	}
	stageKey := []byte(usageBackfillStagePrefix + source)
	ownedKey := []byte(usageBackfillStageOwnedPrefix + source)
	stage, err := backfillStage(tx, source, token)
	if err != nil {
		return err
	}
	if err := invalidateSourceRollupProofs(tx, source); err != nil {
		return err
	}
	if tx.Bucket([]byte(source)) != nil {
		if err := tx.DeleteBucket([]byte(source)); err != nil {
			return err
		}
	}
	result, err := tx.CreateBucket([]byte(source))
	if err != nil {
		return err
	}
	if err := stage.ForEach(func(k, v []byte) error {
		if v == nil && stage.Bucket(k) != nil {
			return fmt.Errorf("backfill staging contains nested bucket")
		}
		return result.Put(k, v)
	}); err != nil {
		return err
	}
	data, err := json.Marshal(map[string]any{"built_at": time.Now().UTC().Format(time.RFC3339), "processed": processed})
	if err != nil {
		return err
	}
	if err := meta.Put([]byte(marker), data); err != nil {
		return err
	}
	if err := tx.DeleteBucket(stageKey); err != nil {
		return err
	}
	return meta.Delete(ownedKey)
}

type backfillEvidence struct {
	Source string `json:"source"`
	Key    []byte `json:"key"`
	Value  []byte `json:"value"`
}

func preserveBackfillAccounting(tx *bolt.Tx, sourceName string) error {
	expected := make(map[string]AggCell)
	register := func(event usage.Event, source string, key, value []byte) error {
		original, err := json.Marshal(backfillEvidence{Source: source, Key: key, Value: value})
		if err != nil {
			return err
		}
		return putUnresolvedEvidence(tx, unresolvedUsage{Event: event, Original: original, Kind: "aggregate_backfill"})
	}
	raw := tx.Bucket([]byte(BucketUsageEvents))
	if raw == nil {
		if err := register(usage.Event{}, BucketUsageEvents, nil, nil); err != nil {
			return err
		}
	} else if err := raw.ForEach(func(k, v []byte) error {
		var event usage.Event
		scope, ambiguous := backfillRawScope(v)
		decoder := json.NewDecoder(bytes.NewReader(v))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil || ambiguous || !json.Valid(v) || !utf8.Valid(v) || event.Timestamp.IsZero() || strings.TrimSpace(event.ClientName) == "" {
			if err := register(scope, BucketUsageEvents, k, v); err != nil {
				return err
			}
			if ambiguous {
				return nil
			}
			if json.Unmarshal(v, &event) != nil {
				return nil
			}
		}
		var keys [][]byte
		if sourceName == BucketUsageAggHourly {
			keys = [][]byte{aggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target), legacyAggKey(event.Timestamp, event.EndpointType, event.ClientName, event.Model, event.Target)}
		} else {
			keys = [][]byte{quotaAggKey(event.Timestamp, event.ClientName, event.Target, event.Model)}
		}
		for _, key := range keys {
			cell := expected[string(key)]
			accumulateAggCell(&cell, event)
			expected[string(key)] = cell
		}
		return nil
	}); err != nil {
		return err
	}

	source := tx.Bucket([]byte(sourceName))
	if source == nil {
		return register(usage.Event{}, sourceName, nil, nil)
	}
	conflicts := make(map[string]bool)
	if sourceName == BucketUsageAggHourly {
		if err := source.ForEach(func(k, _ []byte) error {
			if !strings.HasPrefix(string(k), "v2|") {
				return nil
			}
			entry, err := decodeAggKey(k)
			if err != nil {
				return nil
			}
			legacy := legacyAggKey(entry.hour, entry.endpointType, entry.client, entry.model, entry.target)
			if source.Get(legacy) != nil {
				conflicts[string(k)] = true
				conflicts[string(legacy)] = true
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return source.ForEach(func(k, v []byte) error {
		cell, ok := expected[string(k)]
		if ok && !conflicts[string(k)] {
			encoded, err := json.Marshal(cell)
			if err != nil {
				return err
			}
			if bytes.Equal(encoded, v) {
				return nil
			}
		}
		return register(backfillEvidenceScope(sourceName, k), sourceName, k, v)
	})
}

// Extract only independently valid dimensions from a complete JSON object.
func backfillRawScope(raw []byte) (usage.Event, bool) {
	proofRawParses.Add(1)
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return usage.Event{}, true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return usage.Event{}, true
	}
	var clientRaw, timeRaw, modelRaw, targetRaw, componentsRaw json.RawMessage
	var clientCount, timeCount, modelCount, targetCount, componentsCount int
	var seen []string
	ambiguous := false
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return usage.Event{}, true
		}
		for _, prior := range seen {
			if strings.EqualFold(prior, name) {
				ambiguous = true
				break
			}
		}
		seen = append(seen, name)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return usage.Event{}, true
		}
		if foldedJSONCollision(value) {
			ambiguous = true
		}
		if strings.EqualFold(name, "client_name") {
			clientRaw = value
			clientCount++
		}
		if strings.EqualFold(name, "timestamp") {
			timeRaw = value
			timeCount++
		}
		if strings.EqualFold(name, "model") {
			modelRaw = value
			modelCount++
		}
		if strings.EqualFold(name, "target") {
			targetRaw = value
			targetCount++
		}
		if strings.EqualFold(name, "components") {
			componentsRaw = value
			componentsCount++
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return usage.Event{}, true
	}
	var scope usage.Event
	var client string
	if clientCount == 1 && json.Unmarshal(clientRaw, &client) == nil {
		client = strings.TrimSpace(client)
		if !strings.EqualFold(client, "unknown") {
			scope.ClientName = client
		}
	}
	var at time.Time
	if timeCount == 1 && json.Unmarshal(timeRaw, &at) == nil && !at.IsZero() {
		scope.Timestamp = at.UTC()
	}
	var target string
	if targetCount == 1 && json.Unmarshal(targetRaw, &target) == nil {
		scope.Target = strings.ToLower(strings.TrimSpace(target))
	}
	noComponents := componentsCount == 0
	if componentsCount == 1 {
		var components []json.RawMessage
		noComponents = json.Unmarshal(componentsRaw, &components) == nil && len(components) == 0
	}
	var model string
	if noComponents && modelCount == 1 && json.Unmarshal(modelRaw, &model) == nil {
		scope.Model = strings.ToLower(strings.TrimSpace(model))
	}
	return scope, ambiguous
}

func foldedJSONCollision(raw json.RawMessage) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var visit func(int) (bool, error)
	visit = func(depth int) (bool, error) {
		if depth > 128 {
			return true, nil
		}
		token, err := decoder.Token()
		if err != nil {
			return true, err
		}
		delim, isDelim := token.(json.Delim)
		if !isDelim {
			return false, nil
		}
		collision := false
		switch delim {
		case '{':
			var seen []string
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return true, err
				}
				for _, prior := range seen {
					if strings.EqualFold(prior, name) {
						collision = true
						break
					}
				}
				seen = append(seen, name)
				child, err := visit(depth + 1)
				if err != nil {
					return true, err
				}
				collision = collision || child
			}
		case '[':
			for decoder.More() {
				child, err := visit(depth + 1)
				if err != nil {
					return true, err
				}
				collision = collision || child
			}
		default:
			return true, fmt.Errorf("invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return collision, err
	}
	collision, err := visit(0)
	return collision || err != nil
}

func backfillEvidenceScope(source string, key []byte) usage.Event {
	var event usage.Event
	parts := strings.Split(string(key), "|")
	if source == BucketUsageAggHourly {
		hourIndex := 0
		if len(parts) > 0 && parts[0] == "v2" {
			hourIndex = 1
		}
		if len(parts) > hourIndex {
			if hour, err := parseAggHour(parts[hourIndex]); err == nil {
				event.Timestamp = hour
			}
		}
		if hourIndex == 1 && len(parts) == 6 {
			decode := func(part string) string {
				raw, err := hex.DecodeString(part)
				if err != nil || hex.EncodeToString(raw) != part || !utf8.Valid(raw) {
					return ""
				}
				return string(raw)
			}
			client := decode(parts[3])
			if client != "" && client != "unknown" && client == strings.TrimSpace(client) {
				event.ClientName = client
			}
			model := decode(parts[4])
			if model != "unknown" && model == strings.ToLower(strings.TrimSpace(model)) {
				event.Model = model
			}
			target := decode(parts[5])
			if target != "unknown" && target == strings.ToLower(strings.TrimSpace(target)) {
				event.Target = target
			}
		}
		return event
	}
	if len(parts) > 0 {
		if hour, err := parseAggHour(parts[0]); err == nil {
			event.Timestamp = hour
		}
	}
	if len(parts) < 3 {
		return event
	}
	client := quotaKeyDecode(parts[1])
	if quotaKeyEncode(client) == parts[1] && client != "" && client == strings.TrimSpace(client) && utf8.ValidString(client) {
		event.ClientName = client
	}
	if len(parts) != 4 {
		return event
	}
	if model := quotaKeyDecode(parts[3]); utf8.ValidString(model) && quotaKeyEncode(model) == parts[3] && model == strings.ToLower(strings.TrimSpace(model)) {
		event.Model = model
	}
	if target := quotaKeyDecode(parts[2]); utf8.ValidString(target) && quotaKeyEncode(target) == parts[2] && target == strings.ToLower(strings.TrimSpace(target)) {
		event.Target = target
	}
	return event
}

// Preserve identity bounds before rebuilding destroys the source used to derive them.
func preserveBackfillIdentityBoundary(tx *bolt.Tx) error {
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil {
		return fmt.Errorf("usage receipts: meta bucket missing")
	}
	_, through, unknown, err := computeUsageReceiptWatermark(tx, time.Now())
	if err != nil {
		return err
	}
	previous, err := decodeUsageReceiptWatermark(meta.Get([]byte(usageReceiptsUntrustedThroughKey)))
	if err != nil {
		return err
	}
	if through.After(previous) {
		if err := meta.Put([]byte(usageReceiptsUntrustedThroughKey), []byte(through.UTC().Format(time.RFC3339Nano))); err != nil {
			return err
		}
	}
	if unknown {
		return meta.Put([]byte(usageReceiptsUnknownHourKey), []byte("1"))
	}
	return nil
}

// Snapshot and reset use the same transaction: a failed copy cannot erase the
// source, and later rebuilds never overwrite an earlier generation.
func snapshotUsageAggregation(tx *bolt.Tx, sourceName string) error {
	root, err := tx.CreateBucketIfNotExists([]byte(usageBackfillSnapshotBucket))
	if err != nil {
		return err
	}
	generations, err := root.CreateBucketIfNotExists([]byte(sourceName))
	if err != nil {
		return err
	}
	sequence, err := generations.NextSequence()
	if err != nil {
		return err
	}
	var key [8]byte
	binary.BigEndian.PutUint64(key[:], sequence)
	snapshot, err := generations.CreateBucket(key[:])
	if err != nil {
		return err
	}
	source := tx.Bucket([]byte(sourceName))
	if source == nil {
		_, err := snapshot.CreateBucket([]byte("\x00source_bucket_missing"))
		return err
	}
	return source.ForEach(func(k, v []byte) error {
		if v == nil && source.Bucket(k) != nil {
			return fmt.Errorf("aggregate source %q contains a nested bucket", sourceName)
		}
		return snapshot.Put(k, v)
	})
}
