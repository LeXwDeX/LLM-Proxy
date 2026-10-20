package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/admission"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

func reloadTarget(name string, keys ...string) config.Target {
	return config.Target{Name: name, Endpoint: "http://fixture.invalid", EndpointType: "openai", APIKeys: keys}
}

func reloadService(t *testing.T, targets ...config.Target) *Service {
	t.Helper()
	s, err := NewService(&config.Config{Targets: targets}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func reloadWith(t *testing.T, s *Service, apply bool, targets ...config.Target) {
	t.Helper()
	var err error
	if apply {
		err = s.ApplyConfig(&config.Config{Targets: targets})
	} else {
		err = s.UpdateTargets(targets)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// Copy values, not entries: entries will contain shared mutable runtime pointers.
type reloadKeySnapshot struct {
	status                 KeyStatus
	fails, limits, samples int
	tokens, inFlight       int64
}

func reloadKey(p *keyPool, i int) reloadKeySnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := &p.entries[i]
	return reloadKeySnapshot{
		status: KeyStatus{Exhausted: e.exhausted, Blocked: e.blocked, ExhaustReason: e.exhaustReason,
			ExhaustedAt: e.exhaustedAt, CooldownEnd: e.cooldownEnd, LastError: e.lastError, LastErrorAt: e.lastErrorAt},
		fails: e.consecutiveFails, limits: e.consecutiveRateLimit, samples: len(e.tokenSamples),
		tokens: p.tokenSumInWindow(i, time.Now()), inFlight: p.inFlight[i],
	}
}

func awaitReload(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reload/feedback barrier timed out (possible deadlock)")
	}
}

func TestKeyPoolReloadLateFeedback(t *testing.T) {
	end := time.Now().Add(time.Hour)
	for _, apply := range []bool{false, true} {
		for _, tc := range []struct {
			name, reason, body string
			feedback           func(*keyPool)
		}{
			{"429", "rate_limited", "", func(p *keyPool) { p.markRateLimited(0) }},
			{"429-body", glmRateLimitQPS, `{"error":{"code":"1302","message":"busy"}}`, func(p *keyPool) { p.markRateLimitedWithBody(0, []byte(`{"error":{"code":"1302","message":"busy"}}`)) }},
			{"quota", "quota_exceeded_api", "quota diagnostic", func(p *keyPool) { p.markExhaustedWithError(0, "quota_exceeded", "quota diagnostic") }},
			{"glm-sse", glmExhaustReason, "SSE diagnostic", func(p *keyPool) { p.markExhaustedWithCooldown(0, glmExhaustReason, end, "SSE diagnostic") }},
		} {
			t.Run(tc.name+map[bool]string{false: "/update", true: "/apply"}[apply], func(t *testing.T) {
				s := reloadService(t, reloadTarget("a", "keep", "drop", "other"))
				old := s.targetsByName["a"].keyPool
				ready, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
				go func() { close(ready); <-release; tc.feedback(old); close(done) }()
				awaitReload(t, ready)
				reloadWith(t, s, apply, reloadTarget("a", "other", "new", "keep"))
				current := s.targetsByName["a"].keyPool
				close(release)
				awaitReload(t, done)
				got := reloadKey(current, 2)
				if !got.status.Exhausted || got.status.ExhaustReason != tc.reason || got.status.LastError != tc.body || got.status.ExhaustedAt.IsZero() || !got.status.CooldownEnd.After(time.Now()) {
					t.Fatalf("late feedback lost on retained key at new index: %+v", got)
				}
				if tc.body != "" && got.status.LastErrorAt.IsZero() {
					t.Fatal("diagnostic timestamp lost")
				}
				if tc.name == "glm-sse" && got.status.CooldownEnd != end {
					t.Fatal("precise SSE cooldown changed")
				}
				if reloadKey(current, 0).status.Exhausted || reloadKey(current, 1).status.Exhausted {
					t.Fatal("old index contaminated other keys")
				}
				if !reflect.DeepEqual(got, reloadKey(old, 0)) {
					t.Fatal("generations disagree on shared feedback")
				}
			})
		}
	}
}

func TestKeyPoolReloadCountersRecoveryAndTokens(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "update", true: "apply"}[apply], func(t *testing.T) {
			s := reloadService(t, reloadTarget("a", "keep", "other"))
			a := s.targetsByName["a"].keyPool
			a.recordTokens(0, 11)
			if a.incrementFails(0) != 1 {
				t.Fatal("first failure")
			}
			a.markRateLimited(0)
			a.blockKey(0)
			reloadWith(t, s, apply, reloadTarget("a", "other", "keep", "new"))
			b := s.targetsByName["a"].keyPool
			if b.incrementFails(1) != 2 {
				t.Fatal("reload incorrectly grants another first retry")
			}
			before := reloadKey(a, 0)
			// A third generation must share directly, without forwarding/double writes.
			reloadWith(t, s, apply, reloadTarget("a", "new", "other", "extra", "keep"))
			c := s.targetsByName["a"].keyPool
			if !reflect.DeepEqual(before, reloadKey(c, 3)) {
				t.Fatal("reload changed existing runtime")
			}
			if a.incrementFails(0) != 3 || c.incrementFails(3) != 4 {
				t.Fatal("failure counter is not continuous across A/B/C")
			}
			a.recordTokens(0, 17)
			c.recordTokens(3, 19)
			got := reloadKey(b, 1)
			if got.samples != 3 || got.tokens != 47 {
				t.Fatalf("lost or duplicated token samples: %+v", got)
			}
			// Real success is two calls. markKeySuccess must not reset failures or availability.
			a.markKeySuccess(0)
			got = reloadKey(c, 3)
			if got.limits != 0 || got.fails != 4 || !got.status.Exhausted || !got.status.Blocked {
				t.Fatalf("markKeySuccess changed wrong state: %+v", got)
			}
			a.resetFails(0)
			if reloadKey(c, 3).fails != 0 {
				t.Fatal("late success did not reset shared failures")
			}
			if !b.markRecovered(1) || a.markRecovered(0) || c.markRecovered(3) {
				t.Fatal("recovery must return true exactly once")
			}
			if !reloadKey(c, 3).status.Blocked {
				t.Fatal("probe recovery cleared manual block")
			}
			c.unblockKey(3)
			if reloadKey(a, 0).status.Blocked {
				t.Fatal("new generation unblock invisible to old")
			}
			c.markRateLimited(3)
			a.unmarkExhaustedForRetry(0)
			got = reloadKey(c, 3)
			if got.status.Exhausted || !got.status.CooldownEnd.IsZero() || got.status.ExhaustReason != "rate_limited" || got.limits != 1 {
				t.Fatalf("retry unmark semantics changed: %+v", got)
			}
			// Cooldown recovery through the old pool is also visible in the new pool.
			a.markExhaustedWithCooldown(0, glmRateLimitQPS, time.Now().Add(-time.Second), "expired")
			a.blockKey(1) // force selectKey's cooldown recovery path
			if _, i := a.selectKey(); i != 0 || reloadKey(c, 3).status.Exhausted {
				t.Fatal("cooldown recovery not shared")
			}
		})
	}
}

func TestKeyPoolReloadRetryUnmarkNotification(t *testing.T) {
	s := reloadService(t, reloadTarget("a", "keep", "other"))
	old := s.targetsByName["a"].keyPool
	// Seed directly so no earlier asynchronous notification can satisfy this test.
	old.mu.Lock()
	old.entries[0].exhausted = true
	old.entries[0].exhaustReason = "rate_limited"
	old.entries[0].consecutiveFails = 2
	old.entries[0].consecutiveRateLimit = 3
	old.entries[0].cooldownEnd = time.Now().Add(time.Hour)
	old.mu.Unlock()
	m := admission.NewManager(newTestLogger(), admission.Options{RInit: 12, RMax: 12})
	s.SetAdmissionManager(m)
	reloadWith(t, s, true, reloadTarget("a", "other", "keep", "new"))
	changed := make(chan struct{}, 1)
	old.mu.Lock()
	callback := old.onKeyChange
	old.mu.Unlock()
	old.setOnKeyChange(func(active, total int) { callback(active, total); changed <- struct{}{} })
	old.unmarkExhaustedForRetry(0)
	awaitReload(t, changed)
	got := reloadKey(s.targetsByName["a"].keyPool, 1)
	if got.status.Exhausted || !got.status.CooldownEnd.IsZero() || got.fails != 2 || got.limits != 3 || got.status.ExhaustReason != "rate_limited" {
		t.Fatalf("retry unmark lost state: %+v", got)
	}
	m.Restore("a", &admission.AIMDState{Rate: 12, Tokens: 12})
	if m.Snapshot("a").Tokens != 12 {
		t.Fatal("retry unmark notification did not restore current admission capacity")
	}
}

func TestKeyPoolReloadIdentityBoundaries(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "update", true: "apply"}[apply], func(t *testing.T) {
			s := reloadService(t, reloadTarget("a", "keep", "drop"), reloadTarget("b", "keep", "other"))
			old := s.targetsByName["a"].keyPool
			reloadWith(t, s, apply, reloadTarget("A", "other", "keep"), reloadTarget("b", "keep", "other"))
			old.blockKey(0)
			if !reloadKey(s.targetsByName["a"].keyPool, 1).status.Blocked || reloadKey(s.targetsByName["b"].keyPool, 0).status.Blocked {
				t.Fatal("normalized target identity or target isolation broken")
			}
			// Removed index now holds another key; old result still updates its detached runtime.
			reloadWith(t, s, apply, reloadTarget("a", "replacement", "other"))
			old.markRateLimited(0)
			old.markRateLimited(1)
			if old.incrementFails(0) != 1 || old.incrementFails(1) != 1 {
				t.Fatal("detached runtime lost return semantics")
			}
			if s.targetsByName["a"].keyPool.activeKeyCount() != 2 {
				t.Fatal("removed keys contaminated new indexes")
			}
			reloadWith(t, s, apply, reloadTarget("a", "keep", "drop"))
			old.blockKey(1)
			if s.targetsByName["a"].keyPool.activeKeyCount() != 2 || reloadKey(s.targetsByName["a"].keyPool, 0).fails != 0 {
				t.Fatal("remove/re-add resurrected obsolete runtime")
			}
			// Renaming, deletion, and the single-key boundary end the key-pool lifecycle.
			for _, middle := range []config.Target{reloadTarget("renamed", "keep", "drop"), reloadTarget("a", "keep")} {
				previous := s.targetsByName["a"].keyPool
				reloadWith(t, s, apply, middle)
				reloadWith(t, s, apply, reloadTarget("a", "keep", "drop"))
				previous.markRateLimited(0)
				if s.targetsByName["a"].keyPool.activeKeyCount() != 2 {
					t.Fatal("ended lifecycle contaminated rebuilt pool")
				}
			}
			previous := s.targetsByName["a"].keyPool
			reloadWith(t, s, apply)
			reloadWith(t, s, apply, reloadTarget("a", "keep", "drop"))
			previous.blockKey(0)
			if s.targetsByName["a"].keyPool.activeKeyCount() != 2 {
				t.Fatal("same-name recreation shared obsolete runtime")
			}
			// Key identity is exact, not case-insensitive or based on a masked value.
			previous = s.targetsByName["a"].keyPool
			reloadWith(t, s, apply, reloadTarget("a", "KEEP", "different"))
			previous.blockKey(0)
			if s.targetsByName["a"].keyPool.activeKeyCount() != 2 {
				t.Fatal("key matching is not exact")
			}
		})
	}
}

func TestKeyPoolReloadAdmissionCurrentCounts(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "update", true: "apply"}[apply], func(t *testing.T) {
			s := reloadService(t, reloadTarget("a", "keep", "drop"))
			m := admission.NewManager(newTestLogger(), admission.Options{RInit: 12, RMax: 12})
			s.SetAdmissionManager(m)
			old := s.targetsByName["a"].keyPool
			old.mu.Lock()
			callback := old.onKeyChange
			old.mu.Unlock()
			reloadWith(t, s, apply, reloadTarget("a", "new", "keep", "other"))
			current := s.targetsByName["a"].keyPool
			notified := make(chan struct{}, 16)
			old.setOnKeyChange(func(active, total int) { callback(active, total); notified <- struct{}{} })
			old.markRateLimited(0)
			awaitReload(t, notified) // The actual old-pool notification updates current admission.
			callback(2, 200)         // Ignore stale payload; use current active=2, total=3.
			m.Restore("a", &admission.AIMDState{Rate: 12, Tokens: 12})
			if got := m.Snapshot("a").Tokens; got != 8 {
				t.Fatalf("old feedback did not submit current 2/3 capacity: %d", got)
			}
			current.blockKey(2)
			callback(2, 2) // An older notification cannot overwrite active=1, total=3.
			m.Restore("a", &admission.AIMDState{Rate: 12, Tokens: 12})
			if got := m.Snapshot("a").Tokens; got != 4 {
				t.Fatalf("stale callback overwrote current capacity: %d", got)
			}
			reloadWith(t, s, apply, reloadTarget("a", "fresh-a", "fresh-b"))
			old.blockKey(1)
			callback(0, 2)
			m.Restore("a", &admission.AIMDState{Rate: 12, Tokens: 12})
			if got := m.Snapshot("a").Tokens; got != 12 {
				t.Fatalf("detached old callback reduced fresh capacity: %d", got)
			}
			m.OnLimit("a")
			before := *m.Snapshot("a")
			reloadWith(t, s, apply, reloadTarget("a", "fresh-b", "fresh-a", "third"))
			after := *m.Snapshot("a")
			if after.Rate != before.Rate || after.LastFail != before.LastFail || after.Tokens != before.Tokens {
				t.Fatalf("reload altered AIMD: before=%+v after=%+v", before, after)
			}
			reloadWith(t, s, apply, reloadTarget("b", "fresh-a", "fresh-b"))
			if m.Snapshot("a") != nil || m.Snapshot("b") == nil {
				t.Fatal("admission target set not synchronized")
			}
		})
	}
}

func TestKeyPoolReloadPublicationBarrier(t *testing.T) {
	s := reloadService(t, reloadTarget("a", "keep", "other"))
	s.SetAdmissionManager(admission.NewManager(newTestLogger()))
	old := s.targetsByName["a"].keyPool
	old.incrementFails(0) // before publication
	parsed, order, err := buildTargetStates([]config.Target{reloadTarget("a", "other", "keep", "new")}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	old.mu.Lock()
	entered, published, feedback := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		s.mu.Lock()
		close(entered)
		s.replaceTargetsLocked(parsed, order)
		s.mu.Unlock()
		close(published)
	}()
	awaitReload(t, entered) // publication owns Service.mu, is waiting on the pool domain
	go func() { old.incrementFails(0); old.recordTokens(0, 7); close(feedback) }()
	old.mu.Unlock()
	awaitReload(t, published)
	awaitReload(t, feedback)
	current := s.targetsByName["a"].keyPool
	if current.incrementFails(1) != 3 || reloadKey(current, 1).tokens != 7 {
		t.Fatal("feedback during publication lost or duplicated")
	}
	old.markRateLimited(0) // after publication; callback must not deadlock on Service.mu
	old.asyncNotifyKeyChange()
}

func TestKeyPoolReloadResetTimeDoesNotRecalculateCooldown(t *testing.T) {
	s := reloadService(t, reloadTarget("a", "keep", "other"))
	old := s.targetsByName["a"].keyPool
	old.markExhaustedWithError(0, "quota_exceeded", "diagnostic")
	before := reloadKey(old, 0)
	target := reloadTarget("a", "keep", "other")
	target.KeyResetTime = "monthly:1"
	reloadWith(t, s, true, target)
	current := s.targetsByName["a"].keyPool
	if current == old || !reflect.DeepEqual(before, reloadKey(current, 0)) {
		t.Fatal("reset-time change recalculated existing runtime")
	}
}

func TestKeyPoolReloadGenerationOwnership(t *testing.T) {
	s := reloadService(t, reloadTarget("a", "keep", "other"))
	old := s.targetsByName["a"].keyPool
	old.mu.Lock()
	old.bindLocked("client", 0, time.Now())
	old.rrCounter = 5
	old.wakeUpCooldown = time.Now().Add(time.Minute)
	old.mu.Unlock()
	old.wakeUpInProgress.Store(true)
	reloadWith(t, s, false, reloadTarget("a", "other", "keep", "new"))
	current := s.targetsByName["a"].keyPool
	if current == old || current.keyPoolDomain != old.keyPoolDomain {
		t.Fatal("generations must retain separate pools with one lock domain")
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.entries[1].keyRuntime != old.entries[0].keyRuntime || current.entries[2].keyRuntime == old.entries[0].keyRuntime {
		t.Fatal("runtime ownership does not follow exact retained key")
	}
	if len(current.clientAffinity) != 0 || len(current.clientAffinityAt) != 0 || current.rrCounter != 0 || !current.wakeUpCooldown.IsZero() || current.wakeUpInProgress.Load() {
		t.Fatal("generation inherited request/scheduling resources")
	}
	if old.entries[0].key != "keep" || old.clientAffinity["client"] != 0 || old.rrCounter != 5 || !old.wakeUpInProgress.Load() {
		t.Fatal("old generation was rewritten")
	}
}

func TestKeyPoolReloadUsesActualPublishedPredecessor(t *testing.T) {
	s := reloadService(t, reloadTarget("a", "keep", "other"))
	old := s.targetsByName["a"].keyPool
	// Build a candidate while A is current, but publish it only after B removes K.
	parsed, order, err := buildTargetStates([]config.Target{reloadTarget("a", "new", "keep")}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	reloadWith(t, s, true, reloadTarget("a", "other", "replacement"))
	b := s.targetsByName["a"].keyPool
	s.mu.Lock()
	s.replaceTargetsLocked(parsed, order)
	s.mu.Unlock()
	current := s.targetsByName["a"].keyPool
	old.markRateLimited(0)
	if current.activeKeyCount() != 2 || current.keyPoolDomain != b.keyPoolDomain {
		t.Fatal("publication used build-time predecessor or reconnected removed runtime")
	}
}

// The request keeps its selected old pool/index. Only key runtime is shared;
// response-body ownership and key inFlight must remain with that original pool.
func TestKeyPoolReloadDuringForward(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType, reason string
		status                          int
	}{
		{"429", `{"error":{"type":"rate_limit_error"}}`, "application/json", "rate_limited", 429},
		{"quota", `{"error":{"code":"credit_balance_exhausted","message":"quota"}}`, "application/json", "quota_exceeded_api", 429},
		{"glm-sse", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"code\":\"1308\",\"message\":\"quota\"}}\n\n", "text/event-stream", glmExhaustReason, 200},
		{"success", `{"usage":{"prompt_tokens":2,"completion_tokens":3}}`, "application/json", "rate_limited", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := reloadTarget("a", "keep", "drop", "other")
			if tc.name == "glm-sse" {
				target.UsageAdapter = "glm"
			}
			s := reloadService(t, target)
			old := s.targetsByName["a"]
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			s.httpClient = &http.Client{Transport: statsRoundTrip(func(req *http.Request) (*http.Response, error) {
				close(entered)
				<-release
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: req}, nil
			})}
			var response *http.Response
			var forwardErr error
			go func() {
				defer close(done)
				var cancel func()
				request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
				body := []byte(`{"model":"m"}`)
				response, cancel, _, forwardErr = s.forwardRequest(request, old, body, "", nil)
				// Ordinary 429 feedback belongs to Service's retry loop, not forwardRequest.
				if tc.name == "429" && response != nil {
					old.keyPool.markRateLimited(0)
				}
				if cancel != nil {
					defer cancel()
				}
				if response != nil {
					if tc.name == "success" {
						s.writeResponse(httptest.NewRecorder(), request, old, response, cancel, 1, "m", body, time.Now(), 0, false, nil)
					}
					_ = response.Body.Close()
					_ = response.Body.Close()
				}
			}()
			awaitReload(t, entered)
			if reloadKey(old.keyPool, 0).inFlight != 1 {
				t.Fatal("request did not acquire original key")
			}
			target.APIKeys = []string{"other", "new", "keep"}
			reloadWith(t, s, true, target)
			current := s.targetsByName["a"].keyPool
			if reloadKey(current, 2).inFlight != 0 {
				t.Fatal("new pool inherited old key occupancy")
			}
			current.acquireInFlight(2)
			if tc.name == "success" {
				// Another request marks the retained key while this old request is in flight.
				current.markRateLimited(2)
				current.blockKey(2)
				current.incrementFails(2)
				current.incrementFails(2)
			}
			unblock()
			awaitReload(t, done)
			if tc.name == "glm-sse" && forwardErr == nil {
				t.Fatal("GLM SSE not detected")
			}
			if tc.name != "glm-sse" && forwardErr != nil {
				t.Fatal(forwardErr)
			}
			got := reloadKey(current, 2)
			if !got.status.Exhausted || got.status.ExhaustReason != tc.reason {
				t.Fatalf("forward late terminal lost: %+v", got)
			}
			if tc.name == "success" && (!got.status.Blocked || got.limits != 0 || got.fails != 0) {
				t.Fatalf("late HTTP success must clear both shared counters without restoring availability: %+v", got)
			}
			if reloadKey(old.keyPool, 0).inFlight != 0 || got.inFlight != 1 {
				t.Fatal("old response released new pool occupancy")
			}
			current.releaseInFlight(2)
		})
	}
}
