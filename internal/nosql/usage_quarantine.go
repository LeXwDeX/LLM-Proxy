package nosql

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	BucketUsageEventsQuarantine = "usage_events_quarantine_v1"
	usageQuarantineCountKey     = "usage_events_quarantine_count_v1"
)

// The original key remains the bucket key. RawValue is encoded losslessly as
// base64 by encoding/json, including torn or non-UTF8 data. Never log its bytes.
type usageQuarantineRecord struct {
	RawValue      []byte    `json:"raw_value"`
	QuarantinedAt time.Time `json:"quarantined_at"`
	Reason        string    `json:"reason"`
}

// Quarantine bytes may be corrupt, so only bound owner and raw key can
// establish that a quarantined identity is outside a proof's scope.
func quarantineOutsideProof(tx *bolt.Tx, owners map[string]bool, hours map[time.Time]bool) bool {
	b := tx.Bucket([]byte(BucketUsageEventsQuarantine))
	meta := tx.Bucket([]byte(BucketMeta))
	if b == nil || meta == nil {
		return false
	}
	var count uint64
	err := b.ForEach(func(key, data []byte) error {
		count++
		var record usageQuarantineRecord
		if json.Unmarshal(data, &record) != nil {
			return errors.New("invalid quarantine")
		}
		owner, known, err := readRawOwner(tx, key)
		if err != nil || !known {
			return errors.New("unknown quarantine owner")
		}
		scope, ambiguous := backfillRawScope(record.RawValue)
		if ambiguous && json.Valid(record.RawValue) || (!ambiguous && scope.ClientName != "" && receiptClientHash(scope.ClientName) != owner.Client) {
			return errors.New("conflicting quarantine owner")
		}
		if owners[owner.Client] {
			stamp, _, found := strings.Cut(string(key), "_")
			at, err := time.Parse(time.RFC3339Nano, stamp)
			if !found || err != nil || hours[at.UTC().Truncate(time.Hour)] {
				return errors.New("intersecting quarantine")
			}
		}
		return nil
	})
	if err != nil {
		return false
	}
	rawCount := meta.Get([]byte(usageQuarantineCountKey))
	return rawCount == nil && count == 0 || len(rawCount) == 8 && binary.BigEndian.Uint64(rawCount) == count
}

func hasUsageQuarantine(tx *bolt.Tx) bool {
	if b := tx.Bucket([]byte(BucketUsageEventsQuarantine)); b != nil {
		key, _ := b.Cursor().First()
		if key != nil {
			return true
		}
	}
	// Retain the explicit unresolved fact even if the evidence bucket is lost.
	return tx.Bucket([]byte(BucketMeta)).Get([]byte(usageQuarantineCountKey)) != nil
}

// Called in the same transaction as the raw deletion and raw revision bump.
// An unknown owner invalidates indexed reads globally. This conservative rule
// avoids guessing the event's client or trusting its potentially corrupt time.
func quarantineUsageEvent(tx *bolt.Tx, key, data []byte) error {
	b := tx.Bucket([]byte(BucketUsageEventsQuarantine))
	if b == nil {
		return fmt.Errorf("%w: quarantine bucket missing", ErrUsageClientIndexIncomplete)
	}
	if b.Get(key) != nil {
		return fmt.Errorf("%w: quarantine key already exists; refusing to overwrite evidence", ErrUsageClientIndexIncomplete)
	}
	encoded, err := json.Marshal(usageQuarantineRecord{RawValue: data, QuarantinedAt: time.Now().UTC(), Reason: "raw_event_decode_failed"})
	if err != nil {
		return err
	}
	if err := b.Put(key, encoded); err != nil {
		return err
	}
	meta := tx.Bucket([]byte(BucketMeta))
	var count uint64
	if old := meta.Get([]byte(usageQuarantineCountKey)); len(old) == 8 {
		count = binary.BigEndian.Uint64(old)
	}
	var countBytes [8]byte
	binary.BigEndian.PutUint64(countBytes[:], count+1)
	if err := meta.Put([]byte(usageQuarantineCountKey), countBytes[:]); err != nil {
		return err
	}
	return meta.Delete([]byte(usageClientIndexDone))
}
