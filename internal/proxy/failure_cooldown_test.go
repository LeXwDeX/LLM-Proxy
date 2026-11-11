package proxy

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestFailureCooldownDefaults(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	cases := []struct {
		status int
		want   time.Duration
		reason string
	}{
		{401, time.Hour, "upstream_401"},
		{402, time.Hour, "upstream_402"},
		{403, time.Hour, "upstream_403"},
		{408, time.Minute, "upstream_408"},
		{429, 30 * time.Second, "upstream_429"},
		{500, 30 * time.Second, "upstream_500"},
		{502, time.Minute, "upstream_502"},
		{503, time.Minute, "upstream_503"},
		{504, time.Minute, "upstream_504"},
		{529, 30 * time.Second, "upstream_529"},
		{507, time.Minute, "upstream_5xx"},
		{0, time.Minute, "network"},
	}
	for _, tc := range cases {
		if got := s.failureCooldown(tc.status, nil); got != tc.want {
			t.Errorf("status %d cooldown = %v, want %v", tc.status, got, tc.want)
		}
		if got := failureMuteReason(failureClass(tc.status)); got != tc.reason {
			t.Errorf("status %d reason = %q, want %q", tc.status, got, tc.reason)
		}
	}
	// Non-account classes scale with server.quiet_period_seconds.
	short := &Service{quietPeriod: 10 * time.Second}
	if got := short.failureCooldown(500, nil); got != 5*time.Second {
		t.Errorf("500 with 10s quiet period = %v, want 5s", got)
	}
	if got := short.failureCooldown(503, nil); got != 10*time.Second {
		t.Errorf("503 with 10s quiet period = %v, want 10s", got)
	}
	if got := short.failureCooldown(403, nil); got != time.Hour {
		t.Errorf("403 must stay a fixed hour, got %v", got)
	}
	// Every reason the breaker can record is declared for the admin UI.
	declared := map[string]bool{}
	for _, o := range AllTargetMuteReasonOptions() {
		declared[o.Value] = true
	}
	for _, tc := range cases {
		if !declared[tc.reason] {
			t.Errorf("reason %q missing from AllTargetMuteReasonOptions", tc.reason)
		}
	}
}

func TestFailureCooldownOverridesAndRetryAfter(t *testing.T) {
	s := &Service{quietPeriod: time.Minute, failureCooldownOverrides: map[string]int{"429": 5, "5xx": 90, "403": 600, "network": 20}}
	if got := s.failureCooldown(429, nil); got != 5*time.Second {
		t.Fatalf("429 override = %v", got)
	}
	if got := s.failureCooldown(507, nil); got != 90*time.Second {
		t.Fatalf("5xx override = %v", got)
	}
	if got := s.failureCooldown(403, nil); got != 10*time.Minute {
		t.Fatalf("403 override = %v", got)
	}
	if got := s.failureCooldown(0, nil); got != 20*time.Second {
		t.Fatalf("network override = %v", got)
	}
	h := http.Header{}
	h.Set("Retry-After", "12")
	for _, status := range []int{429, 503, 529} {
		if got := s.failureCooldown(status, h); got != 12*time.Second {
			t.Fatalf("%d with Retry-After = %v, want 12s", status, got)
		}
	}
	if got := s.failureCooldown(500, h); got != 30*time.Second {
		t.Fatalf("500 must ignore Retry-After, got %v", got)
	}
	h.Set("Retry-After", "86400")
	if got := s.failureCooldown(429, h); got != retryAfterMax {
		t.Fatalf("Retry-After not capped: %v", got)
	}
	h.Set("Retry-After", "garbage")
	if got := s.failureCooldown(429, h); got != 5*time.Second {
		t.Fatalf("invalid Retry-After must fall back, got %v", got)
	}
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	h.Set("Retry-After", now.Add(90*time.Second).Format(http.TimeFormat))
	if d, ok := parseRetryAfter(h, now); !ok || d != 90*time.Second {
		t.Fatalf("HTTP-date Retry-After = %v %v", d, ok)
	}
}

func TestMarkTargetFailureClassifiedBackoff(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	s.markTargetFailure(st, now, 500, nil, false, "")
	if got := st.Stats(); got.MuteReason != "upstream_500" || got.MuteStatus != 500 || got.MutedUntil.Sub(now) != 30*time.Second {
		t.Fatalf("first 500: %+v", got)
	}
	// Late failures from the same incident (still cooling) only count; they
	// neither raise the streak nor extend the cooldown.
	for i := 0; i < 4; i++ {
		s.markTargetFailure(st, now.Add(time.Second), 502, nil, false, "")
	}
	if got := st.Stats(); got.ConsecutiveFailure != 1 || got.TotalFailure != 5 || got.MutedUntil.Sub(now) != 30*time.Second || got.MuteReason != "upstream_500" {
		t.Fatalf("concurrent failures inflated the backoff: %+v", got)
	}
	// After expiry (half-open probe failing) the class base doubles.
	later := now.Add(31 * time.Second)
	s.markTargetFailure(st, later, 500, nil, false, "")
	if got := st.Stats().MutedUntil.Sub(later); got != 60*time.Second {
		t.Fatalf("probe failure cooldown = %v, want 60s", got)
	}
}

func TestRetryAfterIsExactNotDoubled(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	h := http.Header{}
	h.Set("Retry-After", "600")
	now := time.Now()
	for i := 0; i < 5; i++ {
		at := now.Add(time.Duration(i) * 11 * time.Minute) // each after the previous expiry
		s.markTargetFailure(st, at, 429, h, false, "")
		if got := st.Stats().MutedUntil.Sub(at); got != 10*time.Minute {
			t.Fatalf("failure %d: Retry-After cooldown %v, want exactly 10m (never multiplied)", i, got)
		}
	}
}

func TestUnprovenAccountStatusGetsOrdinaryCooldown(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	s.markTargetFailure(st, now, 403, nil, false, "")
	if got := st.Stats(); got.MuteReason != "" || got.MuteStatus != 0 || got.MutedUntil.Sub(now) > time.Minute {
		t.Fatalf("unproven 403 took the account cooldown: %+v", got)
	}
	st.MarkSuccess(now)
	// A proven account rejection is settled where it is proven (the key-loop
	// convergence or the dequeue), not again by the generic path.
	s.markTargetFailure(st, now, 403, nil, true, "")
	if got := st.Stats(); got.MutedUntil.After(now) || got.TotalFailure != 1 {
		t.Fatalf("proven path settled twice: %+v", got)
	}
	s.applyAccountRejection(st, now, 403, "", true)
	if got := st.Stats(); got.MuteReason != muteReasonUpstream403 || got.MutedUntil.Sub(now) != time.Hour {
		t.Fatalf("proven 403: %+v", got)
	}
	// An ordinary failure during the hour neither shortens nor relabels it.
	s.markTargetFailure(st, now.Add(time.Second), 502, nil, false, "")
	if got := st.Stats(); got.MuteReason != muteReasonUpstream403 || got.MutedUntil.Sub(now) != time.Hour {
		t.Fatalf("402/403 cooldown overwritten: %+v", got)
	}
}

func TestMarkReachableKeepsAccountCooldown(t *testing.T) {
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	st.MarkFailureFor(now, time.Hour, http.StatusPaymentRequired, "")
	st.markReachable(now.Add(time.Second), true)
	if !st.IsMuted(now.Add(time.Minute)) {
		t.Fatal("a 4xx (even on a probe) cleared the account-rejection cooldown")
	}
	st.MarkSuccess(now.Add(2 * time.Second))
	if st.IsMuted(now.Add(time.Minute)) || st.Stats().MuteReason != "" {
		t.Fatal("a real success must clear the cooldown")
	}
	// A late 4xx from a request without the probe does not reopen a
	// rate-limited target; the probe's own reachable answer does.
	s := &Service{quietPeriod: time.Minute}
	s.markTargetFailure(st, now, http.StatusTooManyRequests, nil, false, "")
	st.markReachable(now.Add(time.Second), false)
	if !st.IsMuted(now.Add(2 * time.Second)) {
		t.Fatal("a late 4xx reopened a 429 cooldown")
	}
	st.markReachable(now.Add(time.Second), true)
	if st.IsMuted(now.Add(2 * time.Second)) {
		t.Fatal("the probe's reachable answer must end an ordinary cooldown")
	}
}

// H1: concurrent unconfirmed rejections of one incident take the short
// cooldown once; only a repeat after it ended escalates.
func TestConcurrentUnverifiedRejectionsDoNotEscalate(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.applyAccountRejection(st, now, http.StatusForbidden, "", false)
		}()
	}
	wg.Wait()
	got := st.Stats()
	if remaining := got.MutedUntil.Sub(now); remaining != defaultUnverifiedRejectionCooldown {
		t.Fatalf("concurrent first incident cooled for %v, want %v", remaining, defaultUnverifiedRejectionCooldown)
	}
	if got.ConsecutiveFailure != 1 || got.TotalFailure != 8 {
		t.Fatalf("late results must only count: %+v", got)
	}
	later := now.Add(defaultUnverifiedRejectionCooldown + time.Second)
	if d, _ := s.applyAccountRejection(st, later, http.StatusForbidden, "", false); d != time.Hour {
		t.Fatalf("repeat after the short cooldown = %v, want the hour", d)
	}
}

func TestClearMuteRecoversAccountKeys(t *testing.T) {
	st := newTargetState(&Target{Name: "t", APIKeys: []string{"k1", "k2", "k3"}}, newTestLogger())
	now := time.Now()
	st.keyPool.markExhaustedWithCooldown(0, keyReasonForbidden, now.Add(time.Hour), "")
	st.keyPool.markExhaustedWithError(1, "billing_error", "")
	st.keyPool.markExhaustedWithError(2, "rate_limited", "")
	st.MarkFailureFor(now, time.Hour, http.StatusForbidden, "")
	st.ClearMute()
	if got := st.keyPool.exhaustReasonAt(0) + "|" + st.keyPool.exhaustReasonAt(1); got != "|" {
		t.Fatalf("account keys still exhausted: %q", got)
	}
	if st.keyPool.exhaustReasonAt(2) != "rate_limited" {
		t.Fatal("rate-limited key must keep its own recovery")
	}
}
