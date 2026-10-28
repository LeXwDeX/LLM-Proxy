//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

func issue74Price(t *testing.T, target, alias string) usage.CostRates {
	t.Helper()
	rates := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4}
	code, body := w.adminDo("POST", "/admin/data/pricing-templates", map[string]any{"target": target, "model": "up-" + alias, "mode": "flat", "base_rates": rates})
	if code != http.StatusCreated {
		t.Fatalf("create synthetic price: %d %s", code, body)
	}
	var response struct {
		Template struct {
			ID string `json:"id"`
		} `json:"template"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Template.ID == "" {
		t.Fatalf("price response: %s %v", body, err)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/pricing-templates/"+response.Template.ID, nil) })
	return rates
}

func issue74CheckCost(t *testing.T, clientName string, rates usage.CostRates) {
	t.Helper()
	code, body := w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(clientName), nil)
	cost, ok := dig(decode(t, body), "totals/estimated_cost").(float64)
	want := rates.CostFor(20, 5, 0, 0)
	if code != http.StatusOK || !ok || math.Abs(cost-want) > 1e-12 {
		t.Fatalf("aggregate cost=%g want=%g status=%d", cost, want, code)
	}
}

func issue74Readonly(t *testing.T) *http.Client {
	t.Helper()
	name := uniq("issue74-viewer")
	const password = "synthetic-readonly-password"
	if code, body := w.adminDo("POST", "/admin/data/users", map[string]any{"username": name, "password": password, "role": "readonly"}); code != http.StatusCreated {
		t.Fatalf("create readonly user: %d %s", code, body)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/users/"+name, nil) })
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	viewer := &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := viewer.PostForm(w.base+"/login", url.Values{"username": {name}, "password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("readonly login status %d", resp.StatusCode)
	}
	return viewer
}

func issue74SafeDiagnostics(t *testing.T, viewer *http.Client, target string, secrets ...string) {
	t.Helper()
	resp, err := viewer.Get(w.base + "/admin/data/targets")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("readonly target GET: status=%d err=%v", resp.StatusCode, err)
	}
	var page struct {
		Targets []struct {
			Name        string `json:"name"`
			KeyStatuses []struct {
				Exhausted bool   `json:"exhausted"`
				LastError string `json:"last_error"`
			} `json:"key_statuses"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	if target != "" {
		found := false
		for _, item := range page.Targets {
			if item.Name == target && len(item.KeyStatuses) > 0 && item.KeyStatuses[0].Exhausted && item.KeyStatuses[0].LastError != "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("readonly target did not expose controlled exhaustion metadata: %s", data)
		}
	}
	output, err := os.ReadFile(w.logs)
	if err != nil {
		t.Fatal(err)
	}
	structured, err := os.ReadFile(filepath.Join(filepath.Dir(w.logs), "upstream-error.log"))
	if err != nil {
		t.Fatal(err)
	}
	combined := string(data) + string(output) + string(structured)
	for _, secret := range secrets {
		if strings.Contains(combined, secret) {
			t.Errorf("default diagnostics or readonly target GET exposed synthetic value %q", secret)
		}
	}
}

func TestIssue74FakeKeyFeedbackAndReadonly(t *testing.T) {
	viewer := issue74Readonly(t)
	for _, tc := range []struct {
		name, adapter, endpoint, path, reason string
		scenario                              func(string) fakellm.Scenario
		stream                                bool
	}{
		{"401", "standard", "/openai/v1", "/v1/chat/completions", "invalid_token", func(secret string) fakellm.Scenario {
			return fakellm.Scenario{Status: 401, Body: `{"error":{"type":"authentication_error","message":"` + secret + `"}}`}
		}, false},
		{"402", "standard", "/openai/v1", "/v1/chat/completions", "billing_error", func(secret string) fakellm.Scenario {
			return fakellm.Scenario{Status: 402, Body: `{"error":{"message":"` + secret + `"}}`}
		}, false},
		{"429", "standard", "/openai/v1", "/v1/chat/completions", "quota_exceeded_api", func(secret string) fakellm.Scenario {
			return fakellm.Scenario{Status: 429, Body: `{"error":{"code":"credit_balance_exhausted","message":"` + secret + `"}}`}
		}, false},
		{"glm-sse", "glm", "/glm/api/anthropic", "/v1/messages", "glm_rate_limit_quota", func(secret string) fakellm.Scenario {
			return fakellm.Scenario{Status: 429, Preset: "glm_1310", SSEError: true, ErrMessage: secret}
		}, true},
		{"minimax-json", "minimax", "/minimax/v1", "/v1/chat/completions", "billing_error", func(secret string) fakellm.Scenario {
			return fakellm.Scenario{Status: 200, Body: `{"base_resp":{"status_code":1008,"status_msg":"` + secret + `"}}`}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad, good, alias := uniq("issue74-bad"), uniq("issue74-good"), uniq("issue74-model")
			secret, querySecret := uniq("issue74-response"), uniq("issue74-query")
			extra := map[string]any{"usage_adapter": tc.adapter}
			if tc.path == "/v1/messages" {
				extra["endpoint_type"] = "claude"
			}
			tg := keyedTarget(t, w.urlA, tc.endpoint, alias, []string{bad, good}, extra)
			rates := issue74Price(t, tg, alias)
			name, key := client(t, []string{tg}, nil)
			scenario := tc.scenario(secret)
			w.fakeA.SetKeyScenario(bad, &scenario)
			defer w.fakeA.SetKeyScenario(bad, nil)
			from := w.fakeA.LastSeq()
			body := map[string]any{"model": alias, "stream": tc.stream, "max_tokens": 32,
				"messages": []any{map[string]any{"role": "user", "content": "[[fake usage.in=20 usage.out=5]]"}}}
			resp, wire := call(t, key, "POST", tc.path+"?arbitrary="+querySecret, body, nil)
			if resp.StatusCode != http.StatusOK || hits(w.fakeA, bad, from) != 1 || hits(w.fakeA, good, from) != 1 {
				t.Fatalf("retry behavior changed: status=%d hits=%d/%d wire=%s", resp.StatusCode, hits(w.fakeA, bad, from), hits(w.fakeA, good, from), wire)
			}
			if st := keyStatus(t, tg, 0); !st.Exhausted || st.ExhaustReason != tc.reason {
				t.Fatalf("key feedback changed: %+v", st)
			}
			rec := lastRecord(t, w.fakeA, strings.TrimPrefix(tc.path, "/v1"))
			if !strings.Contains(rec.Query, querySecret) {
				t.Fatalf("query not forwarded to fakellm: %s", rec.Query)
			}
			ev := usageFor(t, name, 1)[0]
			if ev.InputTokens != 20 || ev.OutputTokens != 5 || ev.UsageStatus != "complete" || ev.CostIncomplete || ev.UsageDiagnostic != "" {
				t.Fatalf("healthy retry usage changed: %+v", ev)
			}
			issue74CheckCost(t, name, rates)
			issue74SafeDiagnostics(t, viewer, tg, secret, querySecret)
		})
	}
}

func TestIssue74FakeGzipErrorPreservesWire(t *testing.T) {
	viewer := issue74Readonly(t)
	upstream, alias := uniq("issue74-gzip-key"), uniq("issue74-gzip-model")
	secret, querySecret := uniq("issue74-gzip-body"), uniq("issue74-gzip-query")
	tg := keyedTarget(t, w.urlA, "/openai/v1", alias, []string{upstream}, nil)
	_, key := client(t, []string{tg}, nil)
	wire := `{"error":{"message":"` + secret + `"}}`
	w.fakeA.SetKeyScenario(upstream, &fakellm.Scenario{Status: 402, Body: wire, ResponseEncoding: "gzip"})
	defer w.fakeA.SetKeyScenario(upstream, nil)
	resp, body := call(t, key, "POST", "/v1/chat/completions?unknown="+querySecret, map[string]any{"model": alias, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, map[string]string{"Accept-Encoding": "gzip"})
	if resp.StatusCode != 402 || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip error changed status/encoding: %d %v", resp.StatusCode, resp.Header)
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(decoded) != wire {
		t.Fatalf("gzip payload changed: %q %v", decoded, err)
	}
	var expected bytes.Buffer
	compressor := gzip.NewWriter(&expected)
	_, _ = compressor.Write([]byte(wire))
	if err := compressor.Close(); err != nil || !bytes.Equal(body, expected.Bytes()) {
		t.Fatalf("gzip bytes changed: %v", err)
	}
	issue74SafeDiagnostics(t, viewer, "", secret, querySecret)
}

func TestIssue74FakeWebSocketHandshake(t *testing.T) {
	viewer := issue74Readonly(t)
	bad, good, alias := uniq("issue74-ws-bad"), uniq("issue74-ws-good"), uniq("issue74-ws-model")
	secret, querySecret := uniq("issue74-ws-body"), uniq("issue74-ws-query")
	tg := keyedTarget(t, w.urlA, "/openai/v1", alias, []string{bad, good}, map[string]any{"supports_responses": true, "prefer_websocket": true})
	rates := issue74Price(t, tg, alias)
	name, key := client(t, []string{tg}, nil)
	w.fakeA.SetKeyScenario(bad, &fakellm.Scenario{Status: 401, Body: `{"error":{"type":"authentication_error","message":"` + secret + `"}}`})
	defer w.fakeA.SetKeyScenario(bad, nil)
	from := w.fakeA.LastSeq()
	resp, body := call(t, key, "POST", "/v1/responses?arbitrary="+querySecret,
		map[string]any{"model": alias, "stream": true, "input": wsInput("[[fake usage.in=20 usage.out=5]]")}, nil)
	if resp.StatusCode != 200 || hits(w.fakeA, bad, from) != 1 || hits(w.fakeA, good, from) < 1 || !strings.Contains(string(body), "fake reply") {
		t.Fatalf("WS handshake retry changed: HTTP %d bad=%d good=%d body=%s", resp.StatusCode, hits(w.fakeA, bad, from), hits(w.fakeA, good, from), body)
	}
	if st := keyStatus(t, tg, 0); !st.Exhausted || st.ExhaustReason != "invalid_token" {
		t.Fatalf("WS key feedback changed: %+v", st)
	}
	ev := usageFor(t, name, 1)[0]
	if ev.InputTokens != 20 || ev.OutputTokens != 5 || ev.UsageStatus != "complete" || ev.CostIncomplete || ev.UsageDiagnostic != "" {
		t.Fatalf("WS retry usage changed: %+v", ev)
	}
	issue74CheckCost(t, name, rates)
	issue74SafeDiagnostics(t, viewer, tg, secret, querySecret)
}

func TestIssue74FakePatchStandardCounterexamples(t *testing.T) {
	viewer := issue74Readonly(t)
	for _, vendor := range []string{"glm", "minimax"} {
		t.Run(vendor, func(t *testing.T) {
			bad, good, alias := uniq("issue74-standard-bad"), uniq("issue74-standard-good"), uniq("issue74-standard-model")
			secret := uniq("issue74-standard-body")
			prefix, path := "/minimax/v1", "/v1/chat/completions"
			extra := map[string]any{"usage_adapter": "standard"}
			scenario := fakellm.Scenario{Status: 200, Body: `{"base_resp":{"status_code":1008,"status_msg":"` + secret + `"}}`}
			stream := false
			if vendor == "glm" {
				prefix, path, stream = "/glm/api/anthropic", "/v1/messages", true
				extra["endpoint_type"] = "claude"
				scenario = fakellm.Scenario{Status: 429, Preset: "glm_1310", SSEError: true, ErrMessage: secret}
			}
			tg := keyedTarget(t, w.urlA, prefix, alias, []string{bad, good}, extra)
			_, key := client(t, []string{tg}, nil)
			w.fakeA.SetKeyScenario(bad, &scenario)
			defer w.fakeA.SetKeyScenario(bad, nil)
			from := w.fakeA.LastSeq()
			resp, wire := call(t, key, "POST", path, map[string]any{"model": alias, "stream": stream, "max_tokens": 32,
				"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
			if resp.StatusCode != 200 || !strings.Contains(string(wire), secret) || hits(w.fakeA, bad, from) != 1 || hits(w.fakeA, good, from) != 0 || keyStatus(t, tg, 0).Exhausted {
				t.Fatalf("standard behavior changed: status=%d body=%s", resp.StatusCode, wire)
			}
			issue74SafeDiagnostics(t, viewer, "", secret)
		})
	}
}
