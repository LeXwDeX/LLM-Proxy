package proxy

import (
	"testing"
	"time"
)

func newModelTestPool(t *testing.T, keys ...string) *keyPool {
	t.Helper()
	return newKeyPool("t", keys, "", newTestLogger())
}

func TestKeyPoolGenericRateLimitIsScopedToKeyAndModel(t *testing.T) {
	p := newModelTestPool(t, "k0", "k1")
	if scoped := p.markRateLimitedFor(0, "qwen", nil, false); !scoped {
		t.Fatal("a generic 429 must be scoped to (key, model)")
	}
	p.mu.Lock()
	exhausted := p.entries[0].exhausted
	p.mu.Unlock()
	if exhausted {
		t.Fatal("the key must stay active for its other models")
	}
	now := time.Now()
	p.mu.Lock()
	qwenBlocked := p.unusableLocked(0, "qwen", now)
	dsBlocked := p.unusableLocked(0, "deepseek", now)
	p.mu.Unlock()
	if !qwenBlocked || dsBlocked {
		t.Fatalf("key 0 unusable for qwen=%v deepseek=%v, want true/false", qwenBlocked, dsBlocked)
	}
	// Selection for qwen skips key 0; for deepseek key 0 stays eligible.
	for i := 0; i < 4; i++ {
		if _, idx := p.selectKeyForClientModel("", "qwen"); idx != 1 {
			t.Fatalf("qwen selected key %d, want 1", idx)
		}
		if _, idx := p.selectNextActiveKeyModel(1, "qwen"); idx != 1 {
			t.Fatalf("next key for qwen = %d, want 1 (key 0 is limited)", idx)
		}
	}
	seen := map[int]bool{}
	for i := 0; i < 6; i++ {
		_, idx := p.selectKeyModel("deepseek")
		seen[idx] = true
	}
	if !seen[0] {
		t.Fatal("deepseek never used key 0")
	}
	// Every key limited for the model: no key, and the earliest end is reported.
	p.markRateLimitedFor(1, "qwen", nil, false)
	if key, idx := p.selectKeyForClientModel("c", "qwen"); key != "" || idx != -1 {
		t.Fatalf("selected key %d for a model every key is limited for", idx)
	}
	if until, ok := p.allKeysModelLimited("qwen", now); !ok || !until.After(now) {
		t.Fatalf("allKeysModelLimited = %v %v", until, ok)
	}
	if _, ok := p.allKeysModelLimited("deepseek", now); ok {
		t.Fatal("deepseek is not limited anywhere")
	}
	// That model's success ends its limit on that key only.
	p.markKeySuccessModel(0, "qwen")
	if _, idx := p.selectKeyModel("qwen"); idx != 0 {
		t.Fatalf("after key 0's qwen success selected %d, want 0", idx)
	}
}

func TestKeyPoolAccountClassStaysKeyLevel(t *testing.T) {
	p := newModelTestPool(t, "k0", "k1")
	p.markExhaustedWithError(0, "billing_error", "")
	if scoped := p.markRateLimitedFor(0, "qwen", nil, false); scoped {
		t.Fatal("an already exhausted key keeps its key-level state")
	}
	// A GLM business code stays key level.
	if scoped := p.markRateLimitedFor(1, "glm-model", []byte(`{"error":{"code":"1302","message":"x"}}`), true); scoped {
		t.Fatal("a GLM business code must keep the key-level classification")
	}
	p.mu.Lock()
	reason := p.entries[1].exhaustReason
	p.mu.Unlock()
	if reason == "" {
		t.Fatal("GLM 1302 did not exhaust the key")
	}
	// No model known: key level as before.
	q := newModelTestPool(t, "k0", "k1")
	if scoped := q.markRateLimitedFor(0, "", nil, false); scoped {
		t.Fatal("without a model the 429 stays key-level")
	}
}

func TestKeyPoolModelLimitTableOverflowFallsBackToKeyLevel(t *testing.T) {
	p := newModelTestPool(t, "k0", "k1")
	for i := 0; i < maxKeyModelLimits; i++ {
		if !p.markRateLimitedFor(0, "m"+time.Duration(i).String(), nil, false) {
			t.Fatalf("limit %d was not scoped", i)
		}
	}
	if scoped := p.markRateLimitedFor(0, "one-too-many", nil, false); scoped {
		t.Fatal("past the table bound the 429 must fall back to the key-level limit")
	}
	p.mu.Lock()
	exhausted := p.entries[0].exhausted
	p.mu.Unlock()
	if !exhausted {
		t.Fatal("fallback must rate limit the key")
	}
}

func TestKeyPoolStatusListsModelLimits(t *testing.T) {
	p := newModelTestPool(t, "k0", "k1")
	p.markRateLimitedFor(0, "qwen", nil, false)
	p.markRateLimitedFor(0, "alpha", nil, false)
	st := p.status()
	if len(st[0].ModelLimits) != 2 || st[0].ModelLimits[0].Model != "alpha" || st[0].ModelLimits[1].Model != "qwen" {
		t.Fatalf("status model limits = %+v, want alpha, qwen", st[0].ModelLimits)
	}
	if st[0].Exhausted || len(st[1].ModelLimits) != 0 {
		t.Fatalf("status = %+v", st)
	}
}
