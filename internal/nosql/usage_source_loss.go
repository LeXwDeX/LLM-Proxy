package nosql

import (
	"fmt"

	bolt "go.etcd.io/bbolt"
)

const usageAggregateSourceMissingPrefix = "usage_aggregate_source_missing_v1:"

// MissingUsageAggregateSources reports initialized aggregate buckets that were
// missing when the database opened. Their ownership and time range are unknown;
// this is an activation blocker, not proof that every client owes consumption.
func (s *UsageStore) MissingUsageAggregateSources() ([]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("usage source status: store is not initialized")
	}
	var missing []string
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		missing, err = missingUsageAggregateSources(tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return missing, nil
}

func missingUsageAggregateSources(tx *bolt.Tx) ([]string, error) {
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil {
		return nil, fmt.Errorf("usage source status: meta bucket missing")
	}
	var missing []string
	for _, source := range []string{BucketUsageAggHourly, BucketQuotaUsageAggHourly} {
		value := meta.Get([]byte(usageAggregateSourceMissingPrefix + source))
		if value == nil {
			continue
		}
		if string(value) != "1" {
			return nil, fmt.Errorf("usage source status: invalid source marker")
		}
		missing = append(missing, source)
	}
	return missing, nil
}
