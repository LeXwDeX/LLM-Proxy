package nosql

import (
	"container/heap"
	"encoding/json"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

const eventSecondLayout = "2006-01-02T15:04:05"

type recentEvent[T any] struct {
	value T
	at    time.Time
	key   string
}

type recentEvents[T any] []recentEvent[T]

func (h recentEvents[T]) Len() int { return len(h) }
func (h recentEvents[T]) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].key < h[j].key
	}
	return h[i].at.Before(h[j].at)
}
func (h recentEvents[T]) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *recentEvents[T]) Push(v any)   { *h = append(*h, v.(recentEvent[T])) }
func (h *recentEvents[T]) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

// scanRecentEvents keeps at most limit results while scanning complete seconds.
// Legacy RFC3339Nano keys sort by second, but NOT within a second: both missing
// fractions and trimmed trailing zeroes invert their chronological order.
// No key migration is needed; exact timestamps decide bounds and result order.
func scanRecentEvents[T any](b *bolt.Bucket, limit int, from, to *time.Time, timestamp func(T) time.Time, matches func(T) bool) []T {
	if b == nil {
		return nil
	}
	c := b.Cursor()
	var k, v []byte
	if to != nil {
		nextSecond := to.UTC().Truncate(time.Second).Add(time.Second).Format(eventSecondLayout)
		k, v = c.Seek([]byte(nextSecond))
		if k != nil {
			k, v = c.Prev()
		} else {
			k, v = c.Last()
		}
	} else {
		k, v = c.Last()
	}
	lower := ""
	if from != nil {
		lower = from.UTC().Format(eventSecondLayout)
	}
	h := make(recentEvents[T], 0, limit)
	for ; k != nil; k, v = c.Prev() {
		if len(k) < len(eventSecondLayout) {
			continue
		}
		second := string(k[:len(eventSecondLayout)])
		if lower != "" && second < lower {
			break
		}
		if len(h) == limit && second < h[0].at.UTC().Format(eventSecondLayout) {
			break
		}
		var event T
		if json.Unmarshal(v, &event) != nil {
			continue
		}
		at := timestamp(event)
		if (from != nil && at.Before(*from)) || (to != nil && at.After(*to)) || (matches != nil && !matches(event)) {
			continue
		}
		candidate := recentEvent[T]{value: event, at: at, key: string(k)}
		if len(h) < limit {
			heap.Push(&h, candidate)
		} else if at.After(h[0].at) || (at.Equal(h[0].at) && candidate.key > h[0].key) {
			h[0] = candidate
			heap.Fix(&h, 0)
		}
	}
	sort.Slice(h, func(i, j int) bool { return h.Less(j, i) })
	result := make([]T, len(h))
	for i := range h {
		result[i] = h[i].value
	}
	return result
}
