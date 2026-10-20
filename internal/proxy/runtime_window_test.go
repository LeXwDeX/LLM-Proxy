package proxy

import (
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"sync"
	"testing"
	"time"
)

func TestRuntimeWindowExpiryAndLongRequest(t *testing.T) {
	now := time.Unix(1800000000, 0)
	w := &targetRuntimeWindow{}
	w.record(now, 2, 1, 1)
	w.record(now.Add(time.Second), -1, 0, 0)
	if got := w.snapshot(now.Add(899 * time.Second)); got != (RuntimeWindow{InFlightPeak: 2, AffinityHits: 1, BreakerTrips: 1}) {
		t.Fatalf("before expiry: %+v", got)
	}
	if got := w.snapshot(now.Add(900 * time.Second)); got != (RuntimeWindow{InFlightPeak: 2}) {
		t.Fatalf("event expiry: %+v", got)
	}
	if got := w.snapshot(now.Add(901 * time.Second)); got != (RuntimeWindow{InFlightPeak: 1}) {
		t.Fatalf("peak expiry with long request: %+v", got)
	}
	w.record(now.Add(902*time.Second), -1, 0, 0)
	if got := w.snapshot(now.Add(1802 * time.Second)); got != (RuntimeWindow{}) {
		t.Fatalf("idle window: %+v", got)
	}
}

func TestRuntimeWindowConcurrentEvents(t *testing.T) {
	w := &targetRuntimeWindow{}
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.record(now, 0, 1, 1) }()
	}
	wg.Wait()
	if got := w.snapshot(now); got.AffinityHits != 100 || got.BreakerTrips != 100 {
		t.Fatalf("lost events: %+v", got)
	}
}

func TestRuntimeWindowSurvivesTargetReload(t *testing.T) {
	svc := newCapacityTestService(t)
	old := capState(t, svc, "cap-a")
	if !old.acquireSlot() {
		t.Fatal("acquire")
	}
	old.MarkFailure(time.Now(), time.Second)
	// UpdateTargets shares the window, including requests still owned by old states.
	if err := svc.UpdateTargets([]config.Target{{Name: "cap-a", Endpoint: "https://example.com", APIKey: "test", EndpointType: "openai"}}); err != nil {
		t.Fatal(err)
	}
	next := capState(t, svc, "cap-a")
	if next.runtimeWindow != old.runtimeWindow {
		t.Fatal("window replaced on reload")
	}
	old.releaseSlot()
	got := next.runtimeWindow.snapshot(time.Now())
	if got.InFlightPeak != 1 || got.BreakerTrips != 1 {
		t.Fatalf("reload snapshot: %+v", got)
	}
	if next.inFlightCount() != 0 {
		t.Fatal("old request did not release real capacity")
	}
}

func TestRuntimeWindowNewEventAfterIdle(t *testing.T) {
	now := time.Unix(1800000000, 0)
	w := &targetRuntimeWindow{}
	w.record(now, 0, 3, 2)
	w.record(now.Add(time.Hour), 0, 1, 0)
	if got := w.snapshot(now.Add(time.Hour)); got != (RuntimeWindow{AffinityHits: 1}) {
		t.Fatalf("stale events after idle: %+v", got)
	}
}
