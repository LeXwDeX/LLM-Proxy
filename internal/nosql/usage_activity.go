package nosql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	bolt "go.etcd.io/bbolt"
)

type ClientActivityUsage struct {
	Name   string
	Totals usage.Totals
	Series []usage.Totals
}

type RecentActivityUsage struct {
	Totals  usage.Totals
	Series  []usage.Totals
	Clients map[string]*ClientActivityUsage
}

// RecentActivity aggregates every raw event in [from,to) in one View, with no
// List limit or price resolution. Complete seconds are scanned because legacy
// RFC3339Nano keys do not sort chronologically within a second.
func (s *UsageStore) RecentActivity(ctx context.Context, from, to time.Time, step time.Duration) (RecentActivityUsage, error) {
	if err := ctx.Err(); err != nil {
		return RecentActivityUsage{}, err
	}
	from, to = from.UTC(), to.UTC()
	if step <= 0 || !from.Before(to) {
		return RecentActivityUsage{}, fmt.Errorf("invalid activity range")
	}
	start := from.Truncate(step)
	count := int((to.Sub(start) + step - 1) / step)
	result := RecentActivityUsage{Series: make([]usage.Totals, count), Clients: make(map[string]*ClientActivityUsage)}
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := tx.Bucket([]byte(BucketUsageEvents))
		if b == nil {
			return fmt.Errorf("usage events bucket missing")
		}
		lower := []byte(from.Truncate(time.Second).Format(eventSecondLayout))
		upper := to.Truncate(time.Second).Add(time.Second).Format(eventSecondLayout)
		cursor := b.Cursor()
		scanned := 0
		for k, v := cursor.Seek(lower); k != nil && string(k) < upper; k, v = cursor.Next() {
			if scanned%128 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			scanned++
			var evt usage.Event
			if err := json.Unmarshal(v, &evt); err != nil {
				return fmt.Errorf("decode activity usage: %w", err)
			}
			if evt.Timestamp.IsZero() {
				return fmt.Errorf("activity event timestamp missing")
			}
			if evt.Timestamp.Before(from) || !evt.Timestamp.Before(to) {
				continue
			}
			index := int(evt.Timestamp.Sub(start) / step)
			name := strings.TrimSpace(evt.ClientName)
			key := strings.ToLower(name)
			client := result.Clients[key]
			if client == nil {
				client = &ClientActivityUsage{Name: name, Series: make([]usage.Totals, count)}
				result.Clients[key] = client
			}
			addEventToTotals(&result.Totals, evt, 0)
			addEventToTotals(&result.Series[index], evt, 0)
			addEventToTotals(&client.Totals, evt, 0)
			addEventToTotals(&client.Series[index], evt, 0)
		}
		return ctx.Err()
	})
	if err != nil {
		return RecentActivityUsage{}, err
	}
	return result, nil
}
