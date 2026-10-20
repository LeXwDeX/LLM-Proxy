package nosql

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/requeststats"
	bolt "go.etcd.io/bbolt"
)

const BucketRequestStats = "request_stats_minutes"

var requestStatsSinceKey = []byte("request_stats_since")

type RequestStatsStore struct{ db *bolt.DB }

func NewRequestStatsStore(db *bolt.DB) *RequestStatsStore { return &RequestStatsStore{db: db} }

func initRequestStats(tx *bolt.Tx, now time.Time) error {
	if tx.Bucket([]byte(BucketRequestStats)) != nil {
		return nil
	}
	if _, err := tx.CreateBucket([]byte(BucketRequestStats)); err != nil {
		return err
	}
	return tx.Bucket([]byte(BucketMeta)).Put(requestStatsSinceKey, []byte(now.UTC().Format(time.RFC3339Nano)))
}

func requestMinuteKey(at time.Time) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, uint64(at.UTC().Unix()/60))
	return key
}

func decodeRequestCounters(raw []byte) (requeststats.Counters, error) {
	var c requeststats.Counters
	if raw != nil {
		// Missing or null fields are corruption, not an empty sample.
		var cell struct {
			Total    *uint64 `json:"total"`
			Success  *uint64 `json:"success"`
			Failure  *uint64 `json:"failure"`
			Canceled *uint64 `json:"canceled"`
		}
		if err := json.Unmarshal(raw, &cell); err != nil {
			return c, fmt.Errorf("decode request stats: %w", err)
		}
		if cell.Total == nil || cell.Success == nil || cell.Failure == nil || cell.Canceled == nil {
			return c, fmt.Errorf("missing request stats counters")
		}
		c = requeststats.Counters{Total: *cell.Total, Success: *cell.Success, Failure: *cell.Failure, Canceled: *cell.Canceled}
		if c.Success > c.Total || c.Failure > c.Total-c.Success || c.Canceled != c.Total-c.Success-c.Failure {
			return c, fmt.Errorf("invalid request stats counters")
		}
	}
	return c, nil
}

func (s *RequestStatsStore) Record(event requeststats.Event) error {
	if err := validateRequestStatsEvent(event); err != nil {
		return err
	}
	key := requestMinuteKey(event.Timestamp)
	// Batch may repeat the callback. All state is rebuilt inside the transaction.
	return s.db.Batch(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketRequestStats))
		if b == nil {
			return fmt.Errorf("request stats bucket unavailable")
		}
		c, err := decodeRequestCounters(b.Get(key))
		if err != nil {
			return err
		}
		if c.Total == math.MaxUint64 {
			return fmt.Errorf("request stats overflow")
		}
		c.Total++
		switch event.Outcome {
		case requeststats.Success:
			c.Success++
		case requeststats.Failure:
			c.Failure++
		case requeststats.Canceled:
			c.Canceled++
		}
		raw, err := json.Marshal(c)
		if err != nil {
			return err
		}
		return b.Put(key, raw)
	})
}

func (s *RequestStatsStore) Snapshot(now time.Time) (requeststats.Snapshot, error) {
	result := requeststats.EmptySnapshot(now)
	if now.IsZero() || result.From.Unix() < 0 {
		return result, fmt.Errorf("invalid request stats window")
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		b, meta := tx.Bucket([]byte(BucketRequestStats)), tx.Bucket([]byte(BucketMeta))
		if b == nil || meta == nil {
			return fmt.Errorf("request stats unavailable")
		}
		var err error
		result.Since, err = time.Parse(time.RFC3339Nano, string(meta.Get(requestStatsSinceKey)))
		if err != nil {
			return fmt.Errorf("invalid request stats collection start: %w", err)
		}
		if result.Since.IsZero() || result.Since.Unix() < 0 {
			return fmt.Errorf("invalid request stats collection start")
		}
		// Exactly 4320 point reads: no raw event or monthly history scan.
		for at := result.From; at.Before(result.To); at = at.Add(time.Minute) {
			c, err := decodeRequestCounters(b.Get(requestMinuteKey(at)))
			if err != nil {
				return err
			}
			if c.Total > math.MaxUint64-result.Total {
				return fmt.Errorf("request stats sum overflow")
			}
			result.Total += c.Total
			result.Success += c.Success
			result.Failure += c.Failure
			result.Canceled += c.Canceled
		}
		return nil
	})
	if err != nil {
		return requeststats.EmptySnapshot(now), err
	}
	result.Available = true
	if result.Total > 0 {
		rate := float64(result.Success) / float64(result.Total) * 100
		result.SuccessRate = &rate
	}
	return result, nil
}

// PruneBefore removes at most limit closed minute cells, preserving collection start.
func (s *RequestStatsStore) PruneBefore(cutoff time.Time, limit int) (int, error) {
	if cutoff.IsZero() || cutoff.Unix() < 0 || limit <= 0 {
		return 0, fmt.Errorf("invalid request stats retention bounds")
	}
	count := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketRequestStats))
		if b == nil {
			return fmt.Errorf("request stats unavailable")
		}
		cursor := b.Cursor()
		boundary := requestMinuteKey(cutoff.Truncate(time.Minute))
		for k, _ := cursor.First(); k != nil && count < limit; k, _ = cursor.Next() {
			if len(k) != 8 {
				return fmt.Errorf("invalid request stats minute key")
			}
			if binary.BigEndian.Uint64(k) >= binary.BigEndian.Uint64(boundary) {
				break
			}
			if err := cursor.Delete(); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
