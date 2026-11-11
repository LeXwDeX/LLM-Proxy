//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
)

// dualTargetE2E creates a dual_protocol target and returns its generated name
// and upstream API key. The key is fixed per target so the test can assert
// that the selected prefix keeps the target's bearer authentication.
func dualTargetE2E(t *testing.T, endpoint, openaiPrefix, anthropicPrefix, poolName string, supportsResponses bool, mappings []any, extra map[string]any) (string, string) {
	t.Helper()
	apiKey := uniq("sk-dual")
	spec := map[string]any{
		"endpoint_type":      "dual_protocol",
		"endpoint":           endpoint,
		"api_key":            apiKey,
		"usage_adapter":      "standard",
		"openai_prefix":      openaiPrefix,
		"anthropic_prefix":   anthropicPrefix,
		"supports_responses": supportsResponses,
		"model_mappings":     mappings,
	}
	if poolName != "" {
		spec["pool"] = poolName
	}
	for k, v := range extra {
		spec[k] = v
	}
	return target(t, spec), apiKey
}

func dualMapping(upstream, alias string) map[string]any {
	return map[string]any{"upstream": upstream, "fallback": alias}
}

// TestDualProtocolPrefixMatrixE2E covers both prefixes, every prefix shape
// seen in practice, and the two client protocols on one target.
func TestDualProtocolPrefixMatrixE2E(t *testing.T) {
	cases := []struct {
		name          string
		endpoint      string
		openaiPrefix  string
		anthropicPfx  string
		supportsResp  bool
		wantChatPath  string
		wantMsgPath   string
		wantCountPath string
		wantRespPath  string
	}{
		{
			name:     "qianfan-versioned-openai-plain-anthropic",
			endpoint: "{A}/qianfan", openaiPrefix: "/v2", anthropicPfx: "/anthropic",
			wantChatPath:  "/qianfan/v2/chat/completions",
			wantMsgPath:   "/qianfan/anthropic/v1/messages",
			wantCountPath: "/qianfan/anthropic/v1/messages/count_tokens",
		},
		{
			name:     "plain-prefixes-keep-client-version",
			endpoint: "{A}/compat", openaiPrefix: "/compatible-mode", anthropicPfx: "/apps/anthropic",
			wantChatPath:  "/compat/compatible-mode/v1/chat/completions",
			wantMsgPath:   "/compat/apps/anthropic/v1/messages",
			wantCountPath: "/compat/apps/anthropic/v1/messages/count_tokens",
		},
		{
			name:     "both-prefixes-versioned",
			endpoint: "{A}/both", openaiPrefix: "/v2", anthropicPfx: "/v3",
			wantChatPath:  "/both/v2/chat/completions",
			wantMsgPath:   "/both/v3/messages",
			wantCountPath: "/both/v3/messages/count_tokens",
		},
		{
			name:     "empty-prefixes-versioned-endpoint",
			endpoint: "{A}/base/v2", openaiPrefix: "", anthropicPfx: "",
			wantChatPath:  "/base/v2/chat/completions",
			wantMsgPath:   "/base/v2/messages",
			wantCountPath: "/base/v2/messages/count_tokens",
		},
		{
			name:     "empty-prefixes-plain-endpoint",
			endpoint: "{A}/base", openaiPrefix: "", anthropicPfx: "",
			wantChatPath:  "/base/v1/chat/completions",
			wantMsgPath:   "/base/v1/messages",
			wantCountPath: "/base/v1/messages/count_tokens",
		},
		{
			name:     "trailing-slash-prefixes",
			endpoint: "{A}/slash", openaiPrefix: "/v2/", anthropicPfx: "/anthropic/",
			wantChatPath:  "/slash/v2/chat/completions",
			wantMsgPath:   "/slash/anthropic/v1/messages",
			wantCountPath: "/slash/anthropic/v1/messages/count_tokens",
		},
		{
			name:     "deeper-versioned-prefixes",
			endpoint: "{A}/deep", openaiPrefix: "/compatible-mode/v1", anthropicPfx: "/apps/anthropic/v2beta",
			wantChatPath:  "/deep/compatible-mode/v1/chat/completions",
			wantMsgPath:   "/deep/apps/anthropic/v2beta/messages",
			wantCountPath: "/deep/apps/anthropic/v2beta/messages/count_tokens",
		},
		{
			name:     "responses-neutral-path",
			endpoint: "{A}/resp", openaiPrefix: "/v2", anthropicPfx: "/anthropic", supportsResp: true,
			wantChatPath:  "/resp/v2/chat/completions",
			wantMsgPath:   "/resp/anthropic/v1/messages",
			wantCountPath: "/resp/anthropic/v1/messages/count_tokens",
			wantRespPath:  "/resp/v2/responses",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chatAlias, msgAlias, respAlias := uniq("e2e-dchat"), uniq("e2e-dmsg"), uniq("e2e-dresp")
			chatUp, msgUp, respUp := uniq("up-chat"), uniq("up-msg"), uniq("up-resp")
			mappings := []any{dualMapping(chatUp, chatAlias), dualMapping(msgUp, msgAlias)}
			if tc.supportsResp {
				mappings = append(mappings, dualMapping(respUp, respAlias))
			}
			tg, apiKey := dualTargetE2E(t, expand(tc.endpoint, "").(string), tc.openaiPrefix, tc.anthropicPfx, "", tc.supportsResp, mappings, nil)
			clientName, key := client(t, []string{tg}, nil)
			wantAuth := "bearer"
			wantKey := fakellm.KeyFingerprint(apiKey)

			resp, raw := call(t, key, "POST", "/v1/chat/completions",
				expand(chatBody("[[fake usage.in=100 usage.out=20]]", false), chatAlias), nil)
			if resp.StatusCode != 200 {
				t.Fatalf("chat HTTP %d: %s", resp.StatusCode, raw)
			}
			rec := lastRecord(t, w.fakeA, tc.wantChatPath)
			if rec.Path != tc.wantChatPath {
				t.Errorf("chat upstream path = %q, want %q", rec.Path, tc.wantChatPath)
			}
			if rec.Auth != wantAuth || rec.Key != wantKey {
				t.Errorf("chat upstream auth = %q key-ok=%v, want %q with the target key", rec.Auth, rec.Key == wantKey, wantAuth)
			}
			if rec.Model != chatUp {
				t.Errorf("chat upstream model = %q, want %q", rec.Model, chatUp)
			}

			resp, raw = call(t, key, "POST", "/v1/messages",
				expand(messagesBody("[[fake usage.in=100 usage.out=20]]", false), msgAlias), nil)
			if resp.StatusCode != 200 {
				t.Fatalf("messages HTTP %d: %s", resp.StatusCode, raw)
			}
			rec = lastRecord(t, w.fakeA, tc.wantMsgPath)
			if rec.Path != tc.wantMsgPath {
				t.Errorf("messages upstream path = %q, want %q", rec.Path, tc.wantMsgPath)
			}
			if rec.Auth != wantAuth || rec.Key != wantKey {
				t.Errorf("messages upstream auth = %q key-ok=%v, want %q with the target key", rec.Auth, rec.Key == wantKey, wantAuth)
			}
			if rec.Model != msgUp {
				t.Errorf("messages upstream model = %q, want %q", rec.Model, msgUp)
			}

			resp, raw = call(t, key, "POST", "/v1/messages/count_tokens",
				expand(messagesBody("[[fake usage.in=100 usage.out=20]]", false), msgAlias), nil)
			if resp.StatusCode != 200 {
				t.Fatalf("count_tokens HTTP %d: %s", resp.StatusCode, raw)
			}
			rec = lastRecord(t, w.fakeA, tc.wantCountPath)
			if rec.Path != tc.wantCountPath {
				t.Errorf("count_tokens upstream path = %q, want %q", rec.Path, tc.wantCountPath)
			}

			if tc.supportsResp {
				resp, raw = call(t, key, "POST", "/v1/responses",
					expand(responsesBody("[[fake usage.in=100 usage.out=20]]", false), respAlias), nil)
				if resp.StatusCode != 200 {
					t.Fatalf("responses HTTP %d: %s", resp.StatusCode, raw)
				}
				rec = lastRecord(t, w.fakeA, tc.wantRespPath)
				if rec.Path != tc.wantRespPath {
					t.Errorf("responses upstream path = %q, want %q", rec.Path, tc.wantRespPath)
				}
				if rec.Auth != wantAuth || rec.Key != wantKey {
					t.Errorf("responses upstream auth = %q key-ok=%v, want %q with the target key", rec.Auth, rec.Key == wantKey, wantAuth)
				}
			}

			// Wait for every expected path, not a bare event count: the
			// count_tokens call may record its own event, which would satisfy
			// a count before the /v1/responses event is written.
			wants := []usageWant{{Path: "/v1/chat/completions"}, {Path: "/v1/messages"}}
			if tc.supportsResp {
				wants = append(wants, usageWant{Path: "/v1/responses"})
			}
			byPath := map[string]usageEvent{}
			for _, ev := range usageForWant(t, clientName, wants...) {
				byPath[ev.Path] = ev
			}
			for _, expected := range []struct {
				path, model string
			}{
				{"/v1/chat/completions", chatUp},
				{"/v1/messages", msgUp},
			} {
				ev, ok := byPath[expected.path]
				if !ok {
					t.Errorf("missing usage event for %s; events=%+v", expected.path, byPath)
					continue
				}
				checkBilled(t, ev, billed{in: 100, out: 20})
				if ev.Model != expected.model || ev.Target != tg || ev.StatusCode != 200 {
					t.Errorf("%s usage = model %q target %q status %d, want %q %q 200", expected.path, ev.Model, ev.Target, ev.StatusCode, expected.model, tg)
				}
			}
			if tc.supportsResp {
				ev, ok := byPath["/v1/responses"]
				if !ok {
					t.Errorf("missing usage event for /v1/responses")
				} else {
					checkBilled(t, ev, billed{in: 100, out: 20})
					if ev.Model != respUp || ev.Target != tg || ev.StatusCode != 200 {
						t.Errorf("responses usage = model %q target %q status %d, want %q %q 200", ev.Model, ev.Target, ev.StatusCode, respUp, tg)
					}
				}
			}
		})
	}
}

// TestDualProtocolBillingE2E prices both protocol models and checks the
// usage event cost, not just the token counters.
func TestDualProtocolBillingE2E(t *testing.T) {
	chatAlias, msgAlias := uniq("e2e-bchat"), uniq("e2e-bmsg")
	chatUp, msgUp := uniq("bill-chat"), uniq("bill-msg")
	tg, _ := dualTargetE2E(t, expand("{A}/bill", "").(string), "/v2", "/anthropic", "", false,
		[]any{dualMapping(chatUp, chatAlias), dualMapping(msgUp, msgAlias)}, nil)
	clientName, key := client(t, []string{tg}, nil)

	priceTemplate(t, flatPrice(tg, chatUp, 3, 7))
	priceTemplate(t, flatPrice(tg, msgUp, 5, 11))

	if resp, raw := call(t, key, "POST", "/v1/chat/completions",
		expand(chatBody("[[fake usage.in=100 usage.out=20]]", false), chatAlias), nil); resp.StatusCode != 200 {
		t.Fatalf("chat HTTP %d: %s", resp.StatusCode, raw)
	}
	if resp, raw := call(t, key, "POST", "/v1/messages",
		expand(messagesBody("[[fake usage.in=100 usage.out=20]]", false), msgAlias), nil); resp.StatusCode != 200 {
		t.Fatalf("messages HTTP %d: %s", resp.StatusCode, raw)
	}

	events := usageFor(t, clientName, 2)
	for _, ev := range events {
		checkBilled(t, ev, billed{in: 100, out: 20})
		if ev.PricingRuleID == "" || ev.CostIncomplete {
			t.Errorf("pricing rule id = %q cost_incomplete=%v", ev.PricingRuleID, ev.CostIncomplete)
		}
	}
	want := (100*3 + 20*7 + 100*5 + 20*11) / 1e6
	if got := clientCost(t, clientName); math.Abs(got-want) > 1e-9 {
		t.Fatalf("billed cost = %.12f, want %.12f", got, want)
	}
}

// TestDualProtocolLoadDistributionE2E uses the default load strategy with
// different max_concurrency members and a slow upstream: concurrent requests
// must spill onto both dual targets, and each request must still be billed
// exactly once.
func TestDualProtocolLoadDistributionE2E(t *testing.T) {
	poolName := pool(t)
	chatAlias, msgAlias := uniq("e2e-lchat"), uniq("e2e-lmsg")
	tgA, keyA := dualTargetE2E(t, expand("{A}/loadA", "").(string), "/v2", "/anthropic", poolName, false,
		[]any{dualMapping(uniq("load-a-chat"), chatAlias), dualMapping(uniq("load-a-msg"), msgAlias)}, map[string]any{"max_concurrency": 2})
	tgB, keyB := dualTargetE2E(t, expand("{A}/loadB", "").(string), "/v2", "/anthropic", poolName, false,
		[]any{dualMapping(uniq("load-b-chat"), chatAlias), dualMapping(uniq("load-b-msg"), msgAlias)}, map[string]any{"max_concurrency": 8})
	w.fakeA.SetKeyScenario(keyA, &fakellm.Scenario{DelayMS: 200})
	defer w.fakeA.SetKeyScenario(keyA, nil)
	w.fakeA.SetKeyScenario(keyB, &fakellm.Scenario{DelayMS: 200})
	defer w.fakeA.SetKeyScenario(keyB, nil)

	const clients = 8
	names := make([]string, clients)
	keys := make([]string, clients)
	for i := range names {
		names[i], keys[i] = client(t, nil, []string{poolName})
	}

	var wg sync.WaitGroup
	errs := make(chan error, clients)
	from := w.fakeA.LastSeq()
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := "/v1/chat/completions"
			body := expand(chatBody("[[fake usage.in=100 usage.out=20]]", false), chatAlias).(map[string]any)
			if i%2 == 1 {
				path = "/v1/messages"
				body = expand(messagesBody("[[fake usage.in=100 usage.out=20]]", false), msgAlias).(map[string]any)
			}
			if err := concurrentDualCall(w.base, keys[i], path, body); err != nil {
				errs <- fmt.Errorf("request %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	targets := map[string]int{}
	for _, name := range names {
		for _, ev := range usageFor(t, name, 1) {
			checkBilled(t, ev, billed{in: 100, out: 20})
			if ev.Target != tgA && ev.Target != tgB {
				t.Errorf("usage target = %q, want %q or %q", ev.Target, tgA, tgB)
			}
			targets[ev.Target]++
		}
	}

	paths := make([]string, 0, clients)
	for _, rec := range w.fakeA.RequestsSince(from) {
		if strings.HasPrefix(rec.Path, "/loadA/") || strings.HasPrefix(rec.Path, "/loadB/") {
			paths = append(paths, rec.Path)
		}
	}
	var a, b int
	for _, p := range paths {
		if strings.HasPrefix(p, "/loadA/") {
			a++
		} else {
			b++
		}
	}
	if a == 0 || b == 0 {
		t.Fatalf("load strategy did not use both dual targets: A=%d B=%d paths=%v", a, b, paths)
	}
	if a+b != clients {
		t.Fatalf("upstream requests = %d, want %d: %v", a+b, clients, paths)
	}
	if targets[tgA] == 0 || targets[tgB] == 0 {
		t.Fatalf("usage targets did not cover both members: %v", targets)
	}
}

// TestDualProtocolCostRoutingE2E covers the cost ("money load") strategy:
// traffic follows the cheaper member for both protocol paths, billing keeps
// the serving member's price, and a price swap never interrupts the sticky
// client but moves a client with no affinity.
func TestDualProtocolCostRoutingE2E(t *testing.T) {
	poolName := routingPool(t, "cost")
	chatAlias, msgAlias := uniq("e2e-cchat"), uniq("e2e-cmsg")
	expensiveChat, expensiveMsg := uniq("exp-chat"), uniq("exp-msg")
	cheapChat, cheapMsg := uniq("cheap-chat"), uniq("cheap-msg")
	expensive, _ := dualTargetE2E(t, expand("{A}/cost-exp", "").(string), "/v2", "/anthropic", poolName, false,
		[]any{dualMapping(expensiveChat, chatAlias), dualMapping(expensiveMsg, msgAlias)}, nil)
	cheap, _ := dualTargetE2E(t, expand("{B}/cost-cheap", "").(string), "/v2", "/anthropic", poolName, false,
		[]any{dualMapping(cheapChat, chatAlias), dualMapping(cheapMsg, msgAlias)}, nil)
	clientName, key := client(t, nil, []string{poolName})

	setExpensive := priceTemplate(t, flatPrice(expensive, expensiveChat, 10, 10))
	priceTemplate(t, flatPrice(expensive, expensiveMsg, 10, 10))
	setCheap := priceTemplate(t, flatPrice(cheap, cheapChat, 1, 1))
	priceTemplate(t, flatPrice(cheap, cheapMsg, 1, 1))

	chatBody100 := expand(chatBody("[[fake usage.in=100 usage.out=20]]", false), chatAlias)
	msgBody100 := expand(messagesBody("[[fake usage.in=100 usage.out=20]]", false), msgAlias)

	for i := 0; i < 3; i++ {
		dualCallOK(t, key, "/v1/chat/completions", chatBody100)
		dualCallOK(t, key, "/v1/messages", msgBody100)
	}
	for _, ev := range usageFor(t, clientName, 6) {
		if ev.Target != cheap {
			t.Fatalf("usage target = %q, want cheap target %q", ev.Target, cheap)
		}
		if ev.Model != cheapChat && ev.Model != cheapMsg {
			t.Fatalf("usage model = %q, want a cheap upstream model", ev.Model)
		}
		checkBilled(t, ev, billed{in: 100, out: 20})
		if ev.CostIncomplete {
			t.Fatalf("incomplete cost on cheap member: %+v", ev)
		}
	}

	// Billing uses the cheap member's price while it serves. The aggregate is
	// evaluated against the templates current at query time, so this must be
	// checked before the price swap below.
	wantCheap := 6 * (100*1 + 20*1) / 1e6
	if got := clientCost(t, clientName); math.Abs(got-wantCheap) > 1e-9 {
		t.Fatalf("billed cost before swap = %.12f, want %.12f", got, wantCheap)
	}

	// Swap prices: the sticky client stays on its healthy member (no active
	// interruption); a client with no affinity follows the new cheapest member.
	setExpensive(flatPrice(expensive, expensiveChat, 1, 1))
	setCheap(flatPrice(cheap, cheapChat, 10, 10))
	dualCallOK(t, key, "/v1/chat/completions", chatBody100)
	for _, ev := range usageFor(t, clientName, 7) {
		if ev.Target != cheap {
			t.Fatalf("sticky client moved to %q after the price swap, want to stay on %q", ev.Target, cheap)
		}
	}
	freshName, freshKey := client(t, nil, []string{poolName})
	dualCallOK(t, freshKey, "/v1/chat/completions", chatBody100)
	fresh := usageFor(t, freshName, 1)[0]
	if fresh.Target != expensive || fresh.Model != expensiveChat || fresh.Path != "/v1/chat/completions" {
		t.Fatalf("new client served by target=%q model=%q path=%q, want %q %q /v1/chat/completions", fresh.Target, fresh.Model, fresh.Path, expensive, expensiveChat)
	}
}

func dualCallOK(t *testing.T, key, path string, body any) {
	t.Helper()
	resp, raw := call(t, key, "POST", path, body, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%s HTTP %d: %s", path, resp.StatusCode, raw)
	}
}

func concurrentDualCall(base, key, path string, body map[string]any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
	}
	return nil
}
