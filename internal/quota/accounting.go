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
	event.ClientName = strings.TrimSpace(event.ClientName)
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	gate := m.accountingGate(event.ClientName)
	gate.RLock()
	defer gate.RUnlock()
	resolver, _ := m.getResolver()
	usage.CheckEventPrices(&event, resolver)
	if err := recorder.Record(event); err != nil {
		return err
	}
	m.IncrementEvent(event)
	return nil
}
