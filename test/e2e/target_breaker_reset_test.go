//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

// The real proxy binary must make corrected credentials or an explicit resume
// usable immediately, while an unrelated PUT preserves the existing cooldown.
func TestBreakerResetsOnTargetIdentityChange(t *testing.T) {
	for _, reset := range []string{"key", "target_resume", "pool_resume"} {
		t.Run(reset, func(t *testing.T) {
			alias, oldKey, newKey := uniq("e2e-breaker"), uniq("sk-breaker-old"), uniq("sk-breaker-new")
			extra := map[string]any{}
			poolName := ""
			if reset == "pool_resume" {
				poolName = pool(t)
				extra["pool"] = poolName
			}
			tg := keyedTarget(t, w.urlA, "/openai/v1", alias, []string{oldKey}, extra)
			_, key := client(t, []string{tg}, nil)
			w.fakeA.SetKeyScenario(oldKey, &fakellm.Scenario{Status: http.StatusServiceUnavailable})
			t.Cleanup(func() { w.fakeA.SetKeyScenario(oldKey, nil) })
			from := w.fakeA.LastSeq()
			if resp, body := chat(t, key, alias, "trip breaker"); resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("first upstream failure: HTTP %d %s", resp.StatusCode, body)
			}
			if n := hits(w.fakeA, oldKey, from); n != 1 {
				t.Fatalf("first failure made %d upstream calls, want 1", n)
			}
			previous := breakerRuntimeStatus(t, tg)
			if previous.Phase != "muted" || !previous.Muted || previous.ConsecutiveFailures == 0 {
				t.Fatalf("expected muted breaker after upstream failure: %+v", previous)
			}
			w.fakeA.SetKeyScenario(oldKey, nil)
			assertMuted := func() {
				t.Helper()
				cursor := w.fakeA.LastSeq()
				if resp, body := chat(t, key, alias, "still muted"); resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "all targets muted") {
					t.Fatalf("expected local muted rejection: HTTP %d %s", resp.StatusCode, body)
				}
				if n := hits(w.fakeA, oldKey, cursor) + hits(w.fakeA, newKey, cursor); n != 0 {
					t.Fatalf("local rejection made %d upstream calls", n)
				}
			}
			assertMuted()
			if code, body := w.adminDo("PUT", "/admin/data/targets/"+tg, map[string]any{"max_concurrency": 3}); code != http.StatusOK {
				t.Fatalf("update max_concurrency: HTTP %d %s", code, body)
			}
			unchanged := breakerRuntimeStatus(t, tg)
			if !unchanged.Muted || unchanged.Phase != "muted" || !unchanged.MutedUntil.Equal(previous.MutedUntil) || unchanged.ConsecutiveFailures != previous.ConsecutiveFailures {
				t.Fatalf("max_concurrency edit changed cooldown: before=%+v after=%+v", previous, unchanged)
			}
			assertMuted()
			expectedKey := oldKey
			if reset == "key" {
				expectedKey = newKey
				if code, body := w.adminDo("PUT", "/admin/data/targets/"+tg, map[string]any{"api_key": newKey}); code != http.StatusOK {
					t.Fatalf("update credentials: HTTP %d %s", code, body)
				}
			} else {
				path := "/admin/data/targets/" + tg
				if reset == "pool_resume" {
					path = "/admin/data/pools/" + poolName
				}
				for _, action := range []string{"pause", "resume"} {
					if code, body := w.adminDo("PUT", path+"/"+action, nil); code != http.StatusOK {
						t.Fatalf("%s: HTTP %d %s", action, code, body)
					}
				}
			}
			ready := breakerRuntimeStatus(t, tg)
			if ready.Paused || ready.Muted || ready.Phase != "ready" || !ready.MutedUntil.IsZero() || ready.ConsecutiveFailures != 0 {
				t.Fatalf("reset did not make target immediately ready: %+v", ready)
			}
			cursor := w.fakeA.LastSeq()
			if resp, body := chat(t, key, alias, "recovered"); resp.StatusCode != http.StatusOK || dig(decode(t, body), "usage/prompt_tokens") == nil {
				t.Fatalf("request immediately after reset: HTTP %d %s", resp.StatusCode, body)
			}
			if n := hits(w.fakeA, expectedKey, cursor); n != 1 {
				t.Fatalf("recovered request made %d upstream calls with the expected key, want 1", n)
			}
			if reset == "key" && hits(w.fakeA, oldKey, cursor) != 0 {
				t.Fatal("recovered request used replaced credentials")
			}
		})
	}
}

type breakerRuntime struct {
	Name                string
	Phase               string
	Paused              bool
	Muted               bool
	MutedUntil          time.Time
	ConsecutiveFailures int
}

func breakerRuntimeStatus(t *testing.T, name string) breakerRuntime {
	t.Helper()
	code, body := w.adminDo("GET", "/admin/healthz", nil)
	if code != http.StatusOK {
		t.Fatalf("runtime health: HTTP %d %s", code, body)
	}
	var page struct {
		Targets []breakerRuntime `json:"targets"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode runtime health: %v", err)
	}
	for _, target := range page.Targets {
		if target.Name == name {
			return target
		}
	}
	t.Fatalf("target %s missing from runtime health", name)
	return breakerRuntime{}
}
