package nosql

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

// AuditEvent records a backend admin operation.
type AuditEvent struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Object    string    `json:"object,omitempty"`
	Result    string    `json:"result"`
	Detail    string    `json:"detail,omitempty"`
}

// AuditStore manages audit events backed by bbolt.
type AuditStore struct {
	db *bolt.DB
}

// NewAuditStore creates a new bbolt-backed audit store.
func NewAuditStore(db *bolt.DB) *AuditStore {
	return &AuditStore{db: db}
}

// Record appends one audit event.
func (s *AuditStore) Record(event AuditEvent) error {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	event.ID = strings.TrimSpace(event.ID)
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	event.Actor = strings.TrimSpace(event.Actor)
	event.Action = strings.TrimSpace(event.Action)
	event.Object = strings.TrimSpace(event.Object)
	event.Result = strings.TrimSpace(event.Result)
	event.Detail = strings.TrimSpace(event.Detail)

	key := auditKey(event.Timestamp, event.ID)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAdminAudit))
		data, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("marshal audit event: %w", err)
		}
		return b.Put([]byte(key), data)
	})
}

// List returns the latest audit events (reverse chronological order).
func (s *AuditStore) List(limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	var events []AuditEvent
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAdminAudit))
		if b == nil {
			return nil
		}
		events = scanRecentEvents(b, limit, nil, nil,
			func(evt AuditEvent) time.Time { return evt.Timestamp }, nil)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// auditKey preserves the historical RFC3339Nano_uuid format. List corrects
// fractional-second ordering without migrating existing records.
func auditKey(t time.Time, id string) string {
	return t.UTC().Format(time.RFC3339Nano) + "_" + id
}
