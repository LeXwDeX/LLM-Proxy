package proxy

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// 429 cools one (target, upstream model), not the whole target: the Bailian
// subscription limit of qwen must not push deepseek sessions off the target.

func TestModelRateLimitDoesNotCoolTarget(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "bailian"}, newTestLogger())
	now := time.Now()
	s.markTargetFailure(st, now, http.StatusTooManyRequests, nil, false, "qwen4-max-preview")
	if st.IsMuted(now) {
		t.Fatal("a model-level 429 must not mute the whole target")
	}
	if got := st.Stats(); got.ConsecutiveFailure != 0 || got.MutedUntil.After(now) {
		t.Fatalf("target-level breaker state moved by a model 429: %+v", got)
	}
	if st.Phase(now) != phaseReady {
		t.Fatalf("phase = %s, want ready while only a model is limited", st.Phase(now))
	}
	until, ok := st.modelLimitedUntil("qwen4-max-preview", now)
	if !ok || until.Sub(now) != 30*time.Second {
		t.Fatalf("limited model until = %v ok=%v, want 30s (half the quiet period)", until.Sub(now), ok)
	}
	if _, ok := st.modelLimitedUntil("deepseek-v4.1-flash", now); ok {
		t.Fatal("another model of the same target must stay available")
	}
	if _, ok := st.modelLimitedUntil("qwen4-max-preview", until.Add(time.Millisecond)); ok {
		t.Fatal("the model limit must end at its deadline")
	}
	if st.Stats().TotalFailure != 1 {
		t.Fatalf("total failures = %d, want 1", st.Stats().TotalFailure)
	}
}

func TestModelRateLimitKeepsUpstreamSpelling(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	s.markTargetFailure(st, now, 429, nil, false, "Qwen-Max")
	if _, ok := st.modelLimitedUntil("Qwen-Max", now); !ok {
		t.Fatal("limit must be keyed by the configured upstream spelling")
	}
	if _, ok := st.modelLimitedUntil("qwen-max", now); ok {
		t.Fatal("keys are the original upstream spelling, not folded")
	}
}

func TestModelRateLimitRetryAfterExactAndPerModelDoubling(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	h := http.Header{}
	h.Set("Retry-After", "45")
	now := time.Now()
	for i := 0; i < 4; i++ {
		at := now.Add(time.Duration(i) * 10 * time.Minute)
		s.markTargetFailure(st, at, 429, h, false, "x")
		until, _ := st.modelLimitedUntil("x", at)
		if until.Sub(at) != 45*time.Second {
			t.Fatalf("failure %d: Retry-After cooldown %v, want exactly 45s (never doubled)", i, until.Sub(at))
		}
	}
	// Without Retry-After the base (30s) doubles per consecutive 429 of that
	// model, capped at 8x; another model counts separately.
	want := []time.Duration{30, 60, 120, 240, 240}
	st2 := newTargetState(&Target{Name: "t2"}, newTestLogger())
	at := now
	for i, w := range want {
		s.markTargetFailure(st2, at, 429, nil, false, "x")
		until, _ := st2.modelLimitedUntil("x", at)
		if until.Sub(at) != w*time.Second {
			t.Fatalf("429 #%d cooldown = %v, want %v", i+1, until.Sub(at), w*time.Second)
		}
		at = until.Add(time.Second) // next failure after the cooldown ended
	}
	s.markTargetFailure(st2, at, 429, nil, false, "y")
	if until, _ := st2.modelLimitedUntil("y", at); until.Sub(at) != 30*time.Second {
		t.Fatalf("a second model starts its own streak, got %v", until.Sub(at))
	}
}

func TestModelRateLimitLateFailureOnlyCounts(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	s.markTargetFailure(st, now, 429, nil, false, "x")
	first, _ := st.modelLimitedUntil("x", now)
	for i := 0; i < 5; i++ {
		s.markTargetFailure(st, now.Add(time.Second), 429, nil, false, "x")
	}
	again, _ := st.modelLimitedUntil("x", now)
	if !again.Equal(first) {
		t.Fatalf("late 429s of one incident moved the deadline: %v -> %v", first, again)
	}
	st.mu.RLock()
	streak := st.modelLimits["x"].streak
	st.mu.RUnlock()
	if streak != 1 {
		t.Fatalf("streak = %d, want 1 (late failures only count)", streak)
	}
	if st.Stats().TotalFailure != 6 {
		t.Fatalf("total failures = %d, want 6", st.Stats().TotalFailure)
	}
}

func TestModelLimitClearedOnlyByThatModelsSuccess(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	s.markTargetFailure(st, now, 429, nil, false, "qwen")
	// A success of another model (including the target-level MarkSuccess a
	// reachable settlement performs) never shortens it.
	st.MarkSuccess(now.Add(time.Second))
	st.clearModelLimit("deepseek")
	st.markReachable(now.Add(time.Second), false)
	if _, ok := st.modelLimitedUntil("qwen", now.Add(2*time.Second)); !ok {
		t.Fatal("another model's success cleared the qwen limit")
	}
	st.clearModelLimit("qwen")
	if _, ok := st.modelLimitedUntil("qwen", now.Add(2*time.Second)); ok {
		t.Fatal("the model's own success must clear its limit")
	}
	// A cleared model restarts at the base cooldown.
	s.markTargetFailure(st, now.Add(3*time.Second), 429, nil, false, "qwen")
	if until, _ := st.modelLimitedUntil("qwen", now.Add(3*time.Second)); until.Sub(now.Add(3*time.Second)) != 30*time.Second {
		t.Fatalf("streak survived a success: %v", until.Sub(now.Add(3*time.Second)))
	}
}

func TestClearMuteClearsModelLimits(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	s.markTargetFailure(st, now, 429, nil, false, "a")
	s.markTargetFailure(st, now, 429, nil, false, "b")
	st.ClearMute()
	for _, m := range []string{"a", "b"} {
		if _, ok := st.modelLimitedUntil(m, now); ok {
			t.Fatalf("ClearMute left the %s limit", m)
		}
	}
	if len(st.modelCooldowns(now)) != 0 {
		t.Fatal("snapshot not empty after ClearMute")
	}
}

func TestIdentityForkResetsModelLimits(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	s.markTargetFailure(st, now, 429, nil, false, "a")
	fresh := st.targetHealthState.forkForNewIdentity()
	if len(fresh.modelLimits) != 0 {
		t.Fatalf("forked identity inherited model limits: %v", fresh.modelLimits)
	}
}

func TestModelLimitMapOverflowFallsBackToTarget(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	for i := 0; i < maxModelLimits; i++ {
		s.markTargetFailure(st, now, 429, nil, false, "m"+strconv.Itoa(i))
	}
	if st.IsMuted(now) {
		t.Fatal("within the cap no target-level cooldown is expected")
	}
	s.markTargetFailure(st, now, 429, nil, false, "one-too-many")
	if !st.IsMuted(now) {
		t.Fatal("past the cap the 429 must fall back to a target-level cooldown")
	}
	if got := st.Stats(); got.MuteStatus != 429 || got.MuteReason != "upstream_429" {
		t.Fatalf("fallback cooldown = %+v", got)
	}
	if len(st.modelCooldowns(now)) != maxModelLimits {
		t.Fatalf("model entries = %d, want cap %d", len(st.modelCooldowns(now)), maxModelLimits)
	}
}

func TestNonRateLimitFailuresStayTargetLevel(t *testing.T) {
	for _, status := range []int{0, 403, 408, 500, 502, 503, 504, 529} {
		s := &Service{quietPeriod: time.Minute}
		st := newTargetState(&Target{Name: "t"}, newTestLogger())
		now := time.Now()
		s.markTargetFailure(st, now, status, nil, false, "some-model")
		if !st.IsMuted(now) {
			t.Fatalf("status %d must keep the target-level cooldown", status)
		}
		if len(st.modelCooldowns(now)) != 0 {
			t.Fatalf("status %d created a model-level cooldown", status)
		}
	}
	// A 429 without a known upstream model (no model in the request) is also
	// target-level: there is nothing to key a model cooldown on.
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	s.markTargetFailure(st, now, 429, nil, false, "")
	if !st.IsMuted(now) {
		t.Fatal("429 without a model must stay target-level")
	}
}

func TestModelCooldownsSnapshot(t *testing.T) {
	s := &Service{quietPeriod: time.Minute}
	st := newTargetState(&Target{Name: "t"}, newTestLogger())
	now := time.Now()
	h := http.Header{}
	h.Set("Retry-After", "20")
	s.markTargetFailure(st, now, 429, h, false, "zeta")
	s.markTargetFailure(st, now, 429, nil, false, "alpha")
	got := st.modelCooldowns(now.Add(5 * time.Second))
	if len(got) != 2 || got[0].Model != "alpha" || got[1].Model != "zeta" {
		t.Fatalf("snapshot not sorted by model: %+v", got)
	}
	if got[0].Status != 429 || got[0].Reason != "upstream_429" || !got[1].Until.Equal(now.Add(20*time.Second)) {
		t.Fatalf("snapshot fields: %+v", got)
	}
	if rest := st.modelCooldowns(now.Add(25 * time.Second)); len(rest) != 1 || rest[0].Model != "alpha" {
		t.Fatalf("expired limits must be left out: %+v", rest)
	}
}
