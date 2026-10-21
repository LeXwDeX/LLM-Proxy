package quota

import (
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func (m *Manager) accountingGate(clientName string) *sync.RWMutex {
	var hash uint32 = 2166136261
	for _, b := range []byte(strings.TrimSpace(clientName)) {
		hash = (hash ^ uint32(b)) * 16777619
	}
	return &m.accounting[hash%uint32(len(m.accounting))]
}

// RecordUsage is the request-completion entrypoint. Concurrent records retain
// bbolt batching; Evaluate waits until every in-progress record has updated the
// counters before taking its authoritative snapshot. A failed write cannot
// produce a charge that exists only in memory.
// 额度体系 v2：池分层限额已退役，不再接收 pool 参数。
func (m *Manager) RecordUsage(recorder usage.Recorder, event usage.Event) error {
	event = usage.CloneEvent(event)
	event.ClientName = strings.TrimSpace(event.ClientName)
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	gate := m.accountingGate(event.ClientName)
	gate.RLock()
	reconcile := false
	defer func() {
		gate.RUnlock()
		if reconcile {
			// Share admission's single-flight/backoff; the retry takes this gate
			// exclusively before replacing counters from durable usage.
			m.retryCalibrationIfDue(event.ClientName)
		}
	}()
	resolver, _ := m.getResolver()
	usage.CheckBundlePrices(&event, resolver)
	added := true
	var err error
	if idempotent, ok := recorder.(usage.IdempotentRecorder); ok {
		added, err = idempotent.RecordOnce(event)
	} else {
		err = recorder.Record(event)
	}
	if err != nil {
		// A reported failure may follow a successful primary commit. Until an
		// authoritative read settles that outcome, do not admit more requests
		// against counters that may omit a durable bill.
		m.calibrationFailed(event.ClientName, err)
		return err
	}
	if added {
		for _, row := range usage.ExpandEvent(event) {
			m.IncrementEvent(row)
		}
	}
	// An idempotent retry can be the first acknowledged observation of an
	// earlier uncertain commit. Retry calibration after releasing the shared gate;
	// blindly incrementing would double-charge already-accounted retries.
	m.mu.RLock()
	reconcile = m.calibrationErrors[event.ClientName] != ""
	m.mu.RUnlock()
	return nil
}
