// Package requeststats defines non-billing request completion statistics.
package requeststats

import "time"

type Outcome string

const (
	Success     Outcome = "success"
	Failure     Outcome = "failure"
	Canceled    Outcome = "canceled"
	Window              = 72 * time.Hour
	Retention           = 7 * 24 * time.Hour
	SampleBasis         = "service_completions_and_valid_websocket_create_turns"
	Exclusions          = "pre-service authentication/IP rejects; health/admin/ping; websocket session controls, invalid or revalidation-rejected messages"
)

type Event struct {
	Timestamp time.Time
	Outcome   Outcome
}

type Recorder interface{ Record(Event) error }
type Reader interface {
	Snapshot(time.Time) (Snapshot, error)
}

type Counters struct {
	Total    uint64 `json:"total"`
	Success  uint64 `json:"success"`
	Failure  uint64 `json:"failure"`
	Canceled uint64 `json:"canceled"`
}

type Snapshot struct {
	Counters
	Available      bool      `json:"available"`
	SuccessRate    *float64  `json:"success_rate"`
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
	Since          time.Time `json:"since"`
	RecordFailures int64     `json:"record_failures"`
	SampleBasis    string    `json:"sample_basis"`
	Exclusions     string    `json:"exclusions"`
	Error          string    `json:"error,omitempty"`
}

func EmptySnapshot(now time.Time) Snapshot {
	to := now.UTC().Truncate(time.Minute)
	return Snapshot{From: to.Add(-Window), To: to, SampleBasis: SampleBasis, Exclusions: Exclusions}
}
