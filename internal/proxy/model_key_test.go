package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// How model cooldowns are keyed: by the upstream model of the target, in the
// spelling the target forwards.

func TestModelKeyTwoAliasesOfOneUpstreamShareTheCooldown(t *testing.T) {
	svc := &Service{quietPeriod: time.Minute}
	target := &Target{Name: "t", allowedModelIdx: buildModelIndex([]config.ModelMapping{{Upstream: "Up-X", Fallback: "alias-1, alias-2"}})}
	st := newTargetState(target, newTestLogger())
	now := time.Now()
	upstream, _ := resolveUpstreamModel(target, "alias-1")
	svc.markTargetFailure(st, now, 429, nil, false, upstream)
	for _, alias := range []string{"alias-1", "alias-2", "up-x"} {
		if _, limited := st.requestModelLimit(alias, now); !limited {
			t.Fatalf("alias %q of the limited upstream must share its cooldown", alias)
		}
	}
	if cds := st.modelCooldowns(now); len(cds) != 1 || cds[0].Model != "Up-X" {
		t.Fatalf("cooldowns = %+v, want one entry keyed by the configured spelling Up-X", cds)
	}
}

func TestModelKeyTargetWithoutMappingsUsesTheLowercaseRequestName(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow"}}`))
	}))
	t.Cleanup(up.Close)
	svc, err := NewService(&config.Config{Server: config.ServerConfig{RequestTimeoutSeconds: 5},
		Targets: []config.Target{{Name: "plain", EndpointType: "openai", Endpoint: up.URL, APIKey: "k"}}}, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatal(err)
	}
	principal, _ := store.Authenticate("token")
	send := func(model string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		rr := httptest.NewRecorder()
		svc.ServeHTTP(rr, req.WithContext(auth.WithPrincipal(req.Context(), principal)))
		return rr
	}
	if rr := send("GPT-4o-Mini"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", rr.Code)
	}
	state, _ := svc.targetByName("plain")
	if cds := state.modelCooldowns(time.Now()); len(cds) != 1 || cds[0].Model != "gpt-4o-mini" {
		t.Fatalf("cooldowns = %+v, want the lowercase request name gpt-4o-mini (no model_mappings)", cds)
	}
	// The same model in any spelling is cooling; another model is not.
	if rr := send("gpt-4O-MINI"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("same model, other spelling: status = %d, want 503 while cooling", rr.Code)
	}
}

func TestModelKeyTargetsWithTheSameUpstreamNameAreIsolated(t *testing.T) {
	svc := &Service{quietPeriod: time.Minute}
	mapping := []config.ModelMapping{{Upstream: "same-up", Fallback: "alias"}}
	a := newTargetState(&Target{Name: "a", allowedModelIdx: buildModelIndex(mapping)}, newTestLogger())
	b := newTargetState(&Target{Name: "b", allowedModelIdx: buildModelIndex(mapping)}, newTestLogger())
	svc.markTargetFailure(a, time.Now(), 429, nil, false, "same-up")
	if _, limited := b.requestModelLimit("alias", time.Now()); limited {
		t.Fatal("a cooldown of target a leaked to target b")
	}
	if adm, _, p := b.admitModel("alias", time.Now()); adm != modelOpen || p != nil {
		t.Fatalf("target b admission = %v probe=%v", adm, p)
	}
}
