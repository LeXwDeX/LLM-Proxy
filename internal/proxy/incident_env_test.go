package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// The production incident in miniature: one client uses two models. The
// "bailian" target serves both (qwen-max -> qwen4-max-preview, which the
// subscription limits with 429; deepseek -> deepseek-v4.1-flash, which is
// fine). The "official" target serves deepseek only and rejects it with a
// 400 the client cannot recover from. Shared by the model-cooldown,
// in-proxy-wait and sticky-retry tests.
const (
	incidentQwenUpstream     = "qwen4-max-preview"
	incidentDeepseekUpstream = "deepseek-v4.1-flash"
	incidentOfficialUpstream = "deepseek-flash"
)

type incidentUpstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits map[string]int
	// respond decides one request by its (upstream) model. A nil return means 200.
	respond func(model string) (status int, header http.Header, body string)
}

func (u *incidentUpstream) setRespond(f func(model string) (int, http.Header, string)) {
	u.mu.Lock()
	u.respond = f
	u.mu.Unlock()
}

func (u *incidentUpstream) hitsFor(model string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[model]
}

func (u *incidentUpstream) total() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, c := range u.hits {
		n += c
	}
	return n
}

func newIncidentUpstream(t *testing.T, respond func(model string) (int, http.Header, string)) *incidentUpstream {
	t.Helper()
	u := &incidentUpstream{hits: map[string]int{}, respond: respond}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.hits[body.Model]++
		respond := u.respond
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if respond != nil {
			if status, header, text := respond(body.Model); status != 0 && status != http.StatusOK {
				for k, v := range header {
					w.Header()[k] = v
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(text))
				return
			}
		}
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

type incidentEnv struct {
	t         *testing.T
	svc       *Service
	principal *auth.Principal
	bailian   *incidentUpstream
	official  *incidentUpstream
}

// newIncidentEnv builds the two targets; qwenRetryAfter is the Retry-After the
// bailian fake sends with its qwen 429 and officialStatus the official channel's rejection.
func newIncidentEnv(t *testing.T, qwenRetryAfter string, officialStatus int, mutate ...func(cfg *config.Config)) *incidentEnv {
	t.Helper()
	bailian := newIncidentUpstream(t, func(model string) (int, http.Header, string) {
		if model == incidentQwenUpstream {
			h := http.Header{}
			if qwenRetryAfter != "" {
				h.Set("Retry-After", qwenRetryAfter)
			}
			return http.StatusTooManyRequests, h, `{"error":{"message":"subscription rate limit","code":"rate_limit"}}`
		}
		return 0, nil, ""
	})
	official := newIncidentUpstream(t, func(model string) (int, http.Header, string) {
		return officialStatus, nil, `{"error":{"message":"thinking mode requires reasoning content","type":"invalid_request_error"}}`
	})
	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 10},
		Targets: []config.Target{
			{Name: "bailian", EndpointType: "openai", Endpoint: bailian.srv.URL, APIKey: "k-b", ModelMappings: []config.ModelMapping{
				{Upstream: incidentQwenUpstream, Fallback: "qwen-max"},
				{Upstream: incidentDeepseekUpstream, Fallback: "deepseek"},
			}},
			{Name: "official", EndpointType: "openai", Endpoint: official.srv.URL, APIKey: "k-o", ModelMappings: []config.ModelMapping{
				{Upstream: incidentOfficialUpstream, Fallback: "deepseek"},
			}},
		},
	}
	for _, m := range mutate {
		m(cfg)
	}
	svc, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	svc.quietPeriod = time.Minute
	svc.inProxyWaitJitterOverride = -1 // deterministic wakeups; jitter has its own test
	t.Cleanup(func() { _ = svc.Close() })
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatal(err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("no principal")
	}
	return &incidentEnv{t: t, svc: svc, principal: principal, bailian: bailian, official: official}
}

// do sends one chat request for the client alias, optionally in a session.
func (e *incidentEnv) do(model, session string) *httptest.ResponseRecorder {
	return e.doCtx(context.Background(), model, session, nil)
}

func (e *incidentEnv) doCtx(ctx context.Context, model, session string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)))
	r.RemoteAddr = "192.0.2.7:4321"
	if session != "" {
		r.Header.Set("X-Session-Id", session)
	}
	if mutate != nil {
		mutate(r)
	}
	r = r.WithContext(auth.WithPrincipal(ctx, e.principal))
	w := httptest.NewRecorder()
	e.svc.ServeHTTP(w, r)
	return w
}

func (e *incidentEnv) state(name string) *targetState { return capState(e.t, e.svc, name) }

// limitModel starts a model-level 429 cooldown of the given length.
func (e *incidentEnv) limitModel(target, upstreamModel string, d time.Duration) {
	e.t.Helper()
	if !e.state(target).markModelLimited(time.Now(), upstreamModel, d, true, http.StatusTooManyRequests, "upstream_429") {
		e.t.Fatal("model limit table full")
	}
}

// bind establishes the deepseek session/IP binding on bailian through a
// real request, so the stickiness is the production one.
func (e *incidentEnv) bind(session string) {
	e.t.Helper()
	rr := e.do("deepseek", session)
	if rr.Code != http.StatusOK || rr.Header().Get("X-Proxy-Target") != "bailian" {
		e.t.Fatalf("bind: status=%d target=%q body=%s", rr.Code, rr.Header().Get("X-Proxy-Target"), rr.Body.String())
	}
}

// usageEvents is a goroutine-safe usage recorder.
type usageEvents struct {
	mu     sync.Mutex
	events []usage.Event
}

func (u *usageEvents) Record(e usage.Event) error {
	u.mu.Lock()
	u.events = append(u.events, e)
	u.mu.Unlock()
	return nil
}

func (u *usageEvents) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.events)
}

// timedRecorder remembers when the first byte or header was written.
type timedRecorder struct {
	*httptest.ResponseRecorder
	mu    sync.Mutex
	first time.Time
}

func newTimedRecorder() *timedRecorder {
	return &timedRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (t *timedRecorder) mark() {
	t.mu.Lock()
	if t.first.IsZero() {
		t.first = time.Now()
	}
	t.mu.Unlock()
}

func (t *timedRecorder) WriteHeader(code int)        { t.mark(); t.ResponseRecorder.WriteHeader(code) }
func (t *timedRecorder) Write(p []byte) (int, error) { t.mark(); return t.ResponseRecorder.Write(p) }
func (t *timedRecorder) Flush()                      { t.mark(); t.ResponseRecorder.Flush() }

func (t *timedRecorder) firstWrite() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.first
}

func withPrincipalCtx(e *incidentEnv) context.Context {
	return auth.WithPrincipal(context.Background(), e.principal)
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}
