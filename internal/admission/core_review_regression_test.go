package admission

import (
	"testing"
	"time"
)

func TestCoreReviewActiveKeysScaleSustainedRefill(t *testing.T) {
	m := NewManager(nil, Options{RInit: 50, RMax: 50})
	m.InitTarget("target", 10)
	m.UpdateActiveKeys("target", 1, 10)
	b := m.buckets["target"]
	b.tokens.Store(0)
	b.lastRefill.Store(time.Now().Add(-100 * time.Millisecond).UnixNano())
	if m.TryAcquire("target") {
		t.Fatal("1/10 active keys should refill at 5/sec; 100ms must not supply a token")
	}
	b.lastRefill.Store(time.Now().Add(-250 * time.Millisecond).UnixNano())
	if !m.TryAcquire("target") {
		t.Fatal("effective 5/sec should supply a token after 250ms")
	}
	m.UpdateActiveKeys("target", 10, 10)
	b.tokens.Store(0)
	b.lastRefill.Store(time.Now().Add(-30 * time.Millisecond).UnixNano())
	if !m.TryAcquire("target") {
		t.Fatal("recovered keys must restore 50/sec refill")
	}
	if state := m.Snapshot("target"); state.Rate != 50 {
		t.Fatal("active-key changes must preserve AIMD base rate")
	}
}

func TestCoreReviewFractionalEffectiveRateIsNotRoundedUp(t *testing.T) {
	m := NewManager(nil, Options{RInit: 5, RMax: 5})
	m.InitTarget("target", 10)
	m.UpdateActiveKeys("target", 5, 10)
	b := m.buckets["target"]
	b.tokens.Store(0)
	b.lastRefill.Store(time.Now().Add(-350 * time.Millisecond).UnixNano())
	if m.TryAcquire("target") {
		t.Fatal("effective 2.5/sec must not be rounded up to burst capacity 3/sec")
	}
	b.lastRefill.Store(time.Now().Add(-450 * time.Millisecond).UnixNano())
	if !m.TryAcquire("target") {
		t.Fatal("fractional effective rate failed to refill")
	}
}
