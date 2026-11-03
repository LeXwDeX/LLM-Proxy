//go:build e2e

package e2e

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
	"github.com/gorilla/websocket"
)

// Issue #153: WebSocket and HTTP share one retry/failover/key-cooldown rule.
// These cases drive the real proxy binary against fakellm (and, where a
// transport fault cannot be expressed by fakellm, a local listener).

func wsDial(t *testing.T, key string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(w.base, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer " + key}})
	if err != nil {
		t.Fatalf("dial: %v %v", err, resp)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func wsCreate(alias, text string) map[string]any {
	return map[string]any{"type": "response.create", "model": alias, "input": wsInput(text)}
}

func wsLast(evs []map[string]any) map[string]any { return evs[len(evs)-1] }

// wsTurns counts upstream WebSocket generation records (not 101 upgrades) for keys.
func wsTurns(fake *fakellm.Server, from int64, keys ...string) int {
	fps := map[string]bool{}
	for _, k := range keys {
		fps[fakellm.KeyFingerprint(k)] = true
	}
	n := 0
	for _, r := range fake.RequestsSince(from) {
		if r.WebSocket && r.Status != 101 && fps[r.Key] {
			n++
		}
	}
	return n
}

func posts(fake *fakellm.Server, from int64) int {
	n := 0
	for _, r := range fake.RequestsSince(from) {
		if r.Method == http.MethodPost && !r.WebSocket {
			n++
		}
	}
	return n
}

func proxyLogHas(t *testing.T, parts ...string) bool {
	t.Helper()
	out, err := os.ReadFile(w.logs)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		all := true
		for _, p := range parts {
			if !strings.Contains(line, p) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func wsResponsesTarget(t *testing.T, fakeURL, alias string, keys []string, extra map[string]any) string {
	spec := map[string]any{"supports_responses": true}
	for k, v := range extra {
		spec[k] = v
	}
	return keyedTarget(t, fakeURL, "/openai/v1", alias, keys, spec)
}

// Native handshake 401 on one key: the key is disabled and the turn completes
// on the next key before any response.create is sent; usage bills once.
func TestWSErrNativeHandshake401KeyFailover(t *testing.T) {
	bad, good, alias := uniq("sk-ws401-bad"), uniq("sk-ws401-good"), uniq("ws401")
	tg := wsResponsesTarget(t, w.urlA, alias, []string{bad, good}, nil)
	name, key := client(t, []string{tg}, nil)
	w.fakeA.SetKeyScenario(bad, &fakellm.Scenario{Preset: "invalid_key"})
	defer w.fakeA.SetKeyScenario(bad, nil)
	from := w.fakeA.LastSeq()
	conn := wsDial(t, key)
	if last := wsLast(wsTurn(t, conn, wsCreate(alias, "[[fake usage.in=20 usage.out=5]]"))); last["type"] != "response.completed" {
		t.Fatalf("turn: %v", last)
	}
	if hits(w.fakeA, bad, from) != 1 || wsTurns(w.fakeA, from, bad) != 0 || wsTurns(w.fakeA, from, good) != 1 {
		t.Fatalf("bad=%d bad turns=%d good turns=%d", hits(w.fakeA, bad, from), wsTurns(w.fakeA, from, bad), wsTurns(w.fakeA, from, good))
	}
	if st := keyStatus(t, tg, 0); !st.Exhausted || st.ExhaustReason != "invalid_token" {
		t.Fatalf("key feedback: %+v", st)
	}
	checkBilled(t, usageFor(t, name, 1)[0], billed{in: 20, out: 5})
}

// Native handshake 429: limited same-key retry, then the next key.
func TestWSErrNativeHandshake429KeyFailover(t *testing.T) {
	bad, good, alias := uniq("sk-ws429-bad"), uniq("sk-ws429-good"), uniq("ws429")
	tg := wsResponsesTarget(t, w.urlA, alias, []string{bad, good}, nil)
	name, key := client(t, []string{tg}, nil)
	w.fakeA.SetKeyScenario(bad, &fakellm.Scenario{Preset: "rate_limit", RetryAfter: "1"})
	defer w.fakeA.SetKeyScenario(bad, nil)
	from := w.fakeA.LastSeq()
	conn := wsDial(t, key)
	if last := wsLast(wsTurn(t, conn, wsCreate(alias, "[[fake usage.in=12 usage.out=3]]"))); last["type"] != "response.completed" {
		t.Fatalf("turn: %v", last)
	}
	if n := hits(w.fakeA, bad, from); n < 1 || n > 2 || wsTurns(w.fakeA, from, bad) != 0 || wsTurns(w.fakeA, from, good) != 1 {
		t.Fatalf("bad handshakes=%d (want 1..2) good turns=%d", n, wsTurns(w.fakeA, from, good))
	}
	checkBilled(t, usageFor(t, name, 1)[0], billed{in: 12, out: 3})
}

// Native handshake 429 on the only key ends the turn with an error event that
// keeps the upstream error object; nothing is billed and the session survives.
func TestWSErrNativeHandshake429ErrorEvent(t *testing.T) {
	k, alias := uniq("sk-ws429-only"), uniq("ws429only")
	tg := wsResponsesTarget(t, w.urlA, alias, []string{k}, nil)
	_, key := client(t, []string{tg}, nil)
	w.fakeA.SetKeyScenario(k, &fakellm.Scenario{Preset: "rate_limit", RetryAfter: "1", Times: 2})
	defer w.fakeA.SetKeyScenario(k, nil)
	from := w.fakeA.LastSeq()
	conn := wsDial(t, key)
	last := wsLast(wsTurn(t, conn, wsCreate(alias, "hi")))
	if last["type"] != "error" || last["status"] != float64(429) || dig(last, "error/code") == nil {
		t.Fatalf("expected a 429 error event with the error object: %v", last)
	}
	if wsTurns(w.fakeA, from, k) != 0 {
		t.Fatal("a rejected handshake sent a generation")
	}
}

// Standard counterexample: an ordinary 403 handshake is not a key failure. It
// is passed through as an error event; the other key is not tried and the key
// is not disabled.
func TestWSErrNativeHandshake403StandardPassThrough(t *testing.T) {
	bad, good, alias := uniq("sk-ws403-bad"), uniq("sk-ws403-good"), uniq("ws403")
	tg := wsResponsesTarget(t, w.urlA, alias, []string{bad, good}, nil)
	_, key := client(t, []string{tg}, nil)
	w.fakeA.SetKeyScenario(bad, &fakellm.Scenario{Status: 403})
	w.fakeA.SetKeyScenario(good, &fakellm.Scenario{Status: 403})
	defer w.fakeA.SetKeyScenario(bad, nil)
	defer w.fakeA.SetKeyScenario(good, nil)
	from := w.fakeA.LastSeq()
	conn := wsDial(t, key)
	last := wsLast(wsTurn(t, conn, wsCreate(alias, "hi")))
	if last["type"] != "error" || last["status"] != float64(403) {
		t.Fatalf("expected a 403 error event: %v", last)
	}
	if h := hits(w.fakeA, bad, from) + hits(w.fakeA, good, from); h != 1 {
		t.Fatalf("ordinary 403 retried another key: handshakes=%d", h)
	}
	for i := 0; i < 2; i++ {
		if st := keyStatus(t, tg, i); st.Exhausted {
			t.Fatalf("ordinary 403 disabled key %d: %+v", i, st)
		}
	}
}

// A proxy-local quota rejection passes its error object through unchanged
// (error.code quota_exceeded, error.quota.resets_at) and reaches no upstream.
func TestWSErrNativeQuotaExceededPassThrough(t *testing.T) {
	k, alias := uniq("sk-wsquota"), uniq("wsquota")
	tg := wsResponsesTarget(t, w.urlA, alias, []string{k}, nil)
	iterationPrice(t, tg, "up-"+alias, usage.CostRates{InputPer1MTokens: 10, OutputPer1MTokens: 10})
	level := uniq("wsquota-level")
	if code, raw := w.adminDo("POST", "/admin/data/job-levels", map[string]any{"name": level, "coefficient_daily": 1}); code != 201 {
		t.Fatalf("level HTTP %d: %s", code, raw)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/job-levels/"+level, nil) })
	group := uniq("wsquota-group")
	if code, raw := w.adminDo("POST", "/admin/data/model-groups", map[string]any{"name": group, "daily_usd": 1, "cost_coefficient": 1,
		"bindings": []any{map[string]any{"target": tg, "model": "up-" + alias}}}); code != 201 {
		t.Fatalf("group HTTP %d: %s", code, raw)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/model-groups/"+group, nil) })
	name, key := client(t, []string{tg}, nil)
	if code, raw := w.adminDo("PUT", "/admin/data/quota/"+name, map[string]any{"job_level": level,
		"quota_groups": []any{map[string]any{"name": group}}}); code != 200 {
		t.Fatalf("plan HTTP %d: %s", code, raw)
	}
	conn := wsDial(t, key)
	if last := wsLast(wsTurn(t, conn, wsCreate(alias, "[[fake usage.in=1000000 usage.out=0]]"))); last["type"] != "response.completed" {
		t.Fatalf("seed turn: %v", last)
	}
	usageFor(t, name, 1)
	from := w.fakeA.LastSeq()
	last := wsLast(wsTurn(t, conn, wsCreate(alias, "hi")))
	if last["type"] != "error" || last["status"] != float64(429) || dig(last, "error/code") != "quota_exceeded" ||
		dig(last, "error/type") != "quota_exceeded" || dig(last, "error/quota/resets_at") == nil || dig(last, "error/quota/group_name") != group {
		t.Fatalf("quota error object not passed through: %v", last)
	}
	if len(w.fakeA.RequestsSince(from)) != 0 {
		t.Fatal("quota-rejected turn reached the upstream")
	}
}

// An in-stream upstream failure is forwarded as its own event and billed with
// the usage it reported; it is never replayed.
func TestWSErrNativeInStreamFailureForwarded(t *testing.T) {
	k, alias := uniq("sk-wsfail"), uniq("wsfail")
	tg := wsResponsesTarget(t, w.urlA, alias, []string{k}, nil)
	name, key := client(t, []string{tg}, nil)
	from := w.fakeA.LastSeq()
	conn := wsDial(t, key)
	evs := wsTurn(t, conn, wsCreate(alias, "[[fake stream_error=1 usage.in=9 usage.out=2]]"))
	if last := wsLast(evs); last["type"] != "response.failed" || dig(last, "response/error/code") == nil {
		t.Fatalf("in-stream failure not forwarded: %v", last)
	}
	if wsTurns(w.fakeA, from, k) != 1 || posts(w.fakeA, from) != 0 {
		t.Fatal("failed generation replayed")
	}
	ev := usageFor(t, name, 1)[0]
	if ev.ResponseStatus != "failed" || ev.InputTokens != 9 || ev.OutputTokens != 2 || ev.CostIncomplete {
		t.Fatalf("failed turn usage: %+v", ev)
	}
}

// An upstream close frame mid-generation ends the native turn with 502
// upstream_disconnected; the generation is not replayed on the other target,
// the close code reaches the diagnostic log, and usage settles as incomplete.
func TestWSErrNativeUpstreamCloseNotReplayed(t *testing.T) {
	p := pool(t)
	alias := uniq("wsclose")
	kA, kB := uniq("sk-wsclose-a"), uniq("sk-wsclose-b")
	wsResponsesTarget(t, w.urlA, alias, []string{kA}, map[string]any{"pool": p})
	wsResponsesTarget(t, w.urlA, alias, []string{kB}, map[string]any{"pool": p})
	name, key := client(t, nil, []string{p})
	from, retries := w.fakeA.LastSeq(), proxyRetries(t)
	conn := wsDial(t, key)
	last := wsLast(wsTurn(t, conn, wsCreate(alias, "[[fake ws_close_after=2 ws_close_code=1011]]")))
	if last["type"] != "error" || last["status"] != float64(502) || dig(last, "error/code") != "upstream_disconnected" {
		t.Fatalf("expected 502 upstream_disconnected: %v", last)
	}
	if n := wsTurns(w.fakeA, from, kA, kB); n != 1 || posts(w.fakeA, from) != 0 || proxyRetries(t) != retries {
		t.Fatalf("generation replayed: turns=%d posts=%d retries=%d", n, posts(w.fakeA, from), proxyRetries(t)-retries)
	}
	ev := usageFor(t, name, 1)[0]
	if ev.InputTokens != 0 || ev.OutputTokens != 0 || !ev.CostIncomplete || !strings.Contains(ev.UsageDiagnostic, "upstream_interrupted_no_usage") {
		t.Fatalf("interrupted turn usage: %+v", ev)
	}
	if !proxyLogHas(t, "ended without a terminal event", "upstream_close_code", "1011") {
		t.Fatal("upstream close code missing from the terminal diagnostic")
	}
}

// Pooled HTTP→WS: an upstream close frame before the terminal event answers
// 502, is not replayed over HTTP, and logs the close code.
func TestWSErrPooledUpstreamCloseNotReplayed(t *testing.T) {
	k, alias := uniq("sk-wspclose"), uniq("wspclose")
	tg := wsResponsesTarget(t, w.urlA, alias, []string{k}, map[string]any{"prefer_websocket": true})
	_, key := client(t, []string{tg}, nil)
	from := w.fakeA.LastSeq()
	resp, body := call(t, key, "POST", "/v1/responses", map[string]any{"model": alias,
		"input": wsInput("[[fake ws_close_after=2 ws_close_code=1011]]")}, nil)
	if resp.StatusCode != 502 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if wsTurns(w.fakeA, from, k) != 1 || posts(w.fakeA, from) != 0 {
		t.Fatal("pooled generation replayed")
	}
	if !proxyLogHas(t, "websocket generation transport failure", "upstream_close_code", "1011") {
		t.Fatal("pooled close code missing from the transport-failure diagnostic")
	}
}

// A connection-refused HTTP fallback POST (the target is remembered as
// WebSocket-unsupported) provably sent nothing and fails over to the second
// target within the same turn.
func TestWSErrNativeFallbackConnectionRefusedFailsOver(t *testing.T) {
	fakeC := fakellm.New(fakellm.Options{Name: "fake-c"})
	// No keep-alive: the failing turn must dial a fresh (refused) connection,
	// not reuse a pooled one that srvC.Close() turns into a post-write EOF.
	srvC := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Connection", "close")
		fakeC.ServeHTTP(rw, r)
	}))
	defer srvC.Close()
	p := pool(t)
	alias := uniq("wsrefused")
	kA, kB := uniq("sk-wsrefused-a"), uniq("sk-wsrefused-b")
	wsResponsesTarget(t, srvC.URL, alias, []string{kA}, map[string]any{"pool": p,
		"custom_headers": map[string]any{"X-Fake-Scenario": "ws_status=426"}})
	tgB := wsResponsesTarget(t, w.urlA, alias, []string{kB}, map[string]any{"pool": p})
	var conn *websocket.Conn
	var name string
	for attempt := 0; attempt < 16 && conn == nil; attempt++ {
		n, key := client(t, nil, []string{p})
		c := wsDial(t, key)
		fromC := fakeC.LastSeq()
		if last := wsLast(wsTurn(t, c, wsCreate(alias, "hi"))); last["type"] != "response.completed" {
			t.Fatalf("first turn: %v", last)
		}
		if posts(fakeC, fromC) == 1 {
			conn, name = c, n
		} else {
			c.Close()
		}
	}
	if conn == nil {
		t.Fatal("no fresh client was routed to the fallback target first in 16 attempts")
	}
	srvC.Close() // connection refused from now on
	from, retries := w.fakeA.LastSeq(), proxyRetries(t)
	if last := wsLast(wsTurn(t, conn, wsCreate(alias, "[[fake usage.in=7 usage.out=2]]"))); last["type"] != "response.completed" {
		t.Fatalf("pre-send fallback failure did not fail over: %v", last)
	}
	if wsTurns(w.fakeA, from, kB) != 1 || proxyRetries(t)-retries != 1 {
		t.Fatalf("second target turns=%d retries=%d", wsTurns(w.fakeA, from, kB), proxyRetries(t)-retries)
	}
	found := false
	for _, ev := range usageFor(t, name, 2) {
		if ev.Target == tgB && ev.InputTokens == 7 {
			checkBilled(t, ev, billed{in: 7, out: 2})
			found = true
		}
	}
	if !found {
		t.Fatal("failed-over turn was not billed on the second target")
	}
}

// An upstream upgrade that never answers times out before any response.create
// was sent: the turn fails over once to the other target.
func TestWSErrNativeHandshakeTimeoutFailsOver(t *testing.T) {
	stop := make(chan struct{})
	var hung atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		hung.Add(1)
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer srv.Close()
	defer close(stop)
	p := pool(t)
	alias := uniq("wstimeout")
	kA, kB := uniq("sk-wstimeout-a"), uniq("sk-wstimeout-b")
	wsResponsesTarget(t, srv.URL, alias, []string{kA}, map[string]any{"pool": p})
	wsResponsesTarget(t, w.urlA, alias, []string{kB}, map[string]any{"pool": p})
	for attempt := 0; attempt < 8; attempt++ {
		name, key := client(t, nil, []string{p})
		before, from, retries := hung.Load(), w.fakeA.LastSeq(), proxyRetries(t)
		conn := wsDial(t, key)
		last := wsLast(wsTurn(t, conn, wsCreate(alias, "[[fake usage.in=6 usage.out=1]]")))
		conn.Close()
		if hung.Load() == before {
			continue // routed to the healthy target first
		}
		if last["type"] != "response.completed" {
			t.Fatalf("handshake timeout did not fail over: %v", last)
		}
		if hung.Load()-before != 1 || wsTurns(w.fakeA, from, kB) != 1 || proxyRetries(t)-retries != 1 {
			t.Fatalf("hung handshakes=%d second target turns=%d retries=%d", hung.Load()-before, wsTurns(w.fakeA, from, kB), proxyRetries(t)-retries)
		}
		checkBilled(t, usageFor(t, name, 1)[0], billed{in: 6, out: 1})
		return
	}
	t.Fatal("no fresh client was routed to the hanging target first in 8 attempts")
}
