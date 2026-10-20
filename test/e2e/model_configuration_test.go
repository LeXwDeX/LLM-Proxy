//go:build e2e

package e2e

import (
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/fakellm"
	"github.com/LeXwDeX/LLM-Proxy/internal/pricing"
	"github.com/LeXwDeX/LLM-Proxy/internal/usage"
)

// #173 crosses the admin API, persisted configuration, live forwarding, usage
// storage and pricing resolver. All requests terminate at in-process fakellm.
func TestModelConfigurationPrefixSharedPricingAndPoolDisplay(t *testing.T) {
	display := uniq("OpenAI-PLAN")
	poolName := strings.ToLower(display)
	if code, body := w.adminDo("POST", "/admin/data/pools", map[string]any{"name": display}); code != http.StatusCreated {
		t.Fatalf("create display pool: %d %s", code, body)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/pools/"+poolName, nil) })
	models := []string{"fable5", "fable5-1"}
	type upstream struct {
		target string
		fake   *fakellm.Server
	}
	var upstreams []upstream
	for i, endpoint := range []string{w.urlA, w.urlB} {
		name := target(t, map[string]any{
			"endpoint_type": "openai", "endpoint": endpoint + "/openai/v1", "api_key": uniq("sk-model-config"),
			"model_prefix": "Vendor/", "pool": poolName,
			"model_mappings": []any{map[string]any{"upstream": models[0]}, map[string]any{"upstream": models[1]}},
		})
		fake := w.fakeA
		if i == 1 {
			fake = w.fakeB
		}
		upstreams = append(upstreams, upstream{name, fake})
	}
	rates := usage.CostRates{InputPer1MTokens: 2, OutputPer1MTokens: 4, CacheReadPer1MToken: 0.5}
	code, body := w.adminDo("POST", "/admin/data/pricing-templates", map[string]any{
		"targets": []string{upstreams[0].target, upstreams[1].target}, "models": models,
		"mode": "flat", "base_rates": rates,
	})
	if code != http.StatusCreated {
		t.Fatalf("create shared prices: %d %s", code, body)
	}
	var created struct {
		Template pricing.PricingTemplate `json:"template"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Template.ID == "" || len(created.Template.Targets) != 2 || len(created.Template.Models) != 2 {
		t.Fatalf("shared rule not returned: %s", body)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/pricing-templates/"+created.Template.ID, nil) })
	request := func(model string) map[string]any {
		return map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "[[fake usage.in=100 usage.out=30 usage.cached=40]]"}}}
	}
	for _, up := range upstreams {
		for _, model := range models {
			t.Run(up.target+"/"+model, func(t *testing.T) {
				clientName, key := client(t, []string{up.target}, nil)
				before := up.fake.LastSeq()
				resp, raw := call(t, key, "POST", "/v1/chat/completions", request(model), nil)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("forward: %d %s", resp.StatusCode, raw)
				}
				records := up.fake.RequestsSince(before)
				if len(records) != 1 || records[0].Model != "Vendor/"+model {
					t.Fatalf("prefixed upstream model: %+v", records)
				}
				ev := usageFor(t, clientName, 1)[0]
				if ev.Target != up.target || ev.Model != model || ev.InputTokens != 60 || ev.OutputTokens != 30 || ev.CachedTokens != 40 || ev.UsageStatus != "complete" || ev.CostIncomplete {
					t.Fatalf("billing identity or usage changed by prefix: %+v", ev)
				}
				code, data := w.adminDo("GET", "/admin/data/usage/aggregate?group_by=hour&client_name="+url.QueryEscape(clientName), nil)
				if code != http.StatusOK {
					t.Fatalf("aggregate: %d %s", code, data)
				}
				got, ok := dig(decode(t, data), "totals/estimated_cost").(float64)
				want := rates.CostFor(60, 30, 40, 0)
				if !ok || math.Abs(got-want) > 1e-12 {
					t.Fatalf("shared bill %.12f want %.12f: %s", got, want, data)
				}
				// Each Cartesian-product binding resolves to the same persisted rule.
				query := url.Values{"target": {up.target}, "model": {model}}
				code, data = w.adminDo("GET", "/admin/data/pricing-templates?"+query.Encode(), nil)
				if code != http.StatusOK || !strings.Contains(string(data), created.Template.ID) {
					t.Fatalf("shared binding absent: %d %s", code, data)
				}
			})
		}
	}
	// Changing display casing must preserve pool-only authorization and pause
	// propagation, including clients already bound to the canonical pool name.
	_, poolKey := client(t, nil, []string{poolName})
	updatedDisplay := strings.ToUpper(display)
	if code, body := w.adminDo("PUT", "/admin/data/pools/"+poolName, map[string]any{"display_name": updatedDisplay}); code != http.StatusOK {
		t.Fatalf("edit display casing: %d %s", code, body)
	}
	code, body = w.adminDo("GET", "/admin/data/pools", nil)
	var pools struct {
		Pools []config.DeploymentPool `json:"pools"`
	}
	if code != http.StatusOK {
		t.Fatalf("read display: %d %s", code, body)
	}
	if err := json.Unmarshal(body, &pools); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range pools.Pools {
		if p.Name == poolName {
			found = true
			if p.DisplayName != updatedDisplay {
				t.Fatalf("display roundtrip: %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("display edit changed pool identity")
	}
	resp, raw := call(t, poolKey, "POST", "/v1/chat/completions", request(models[0]), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pool-only authorization after display edit: %d %s", resp.StatusCode, raw)
	}
	if code, body := w.adminDo("PUT", "/admin/data/pools/"+poolName+"/pause", nil); code != http.StatusOK {
		t.Fatalf("pause: %d %s", code, body)
	}
	beforeA, beforeB := w.fakeA.LastSeq(), w.fakeB.LastSeq()
	resp, raw = call(t, poolKey, "POST", "/v1/chat/completions", request(models[0]), nil)
	if resp.StatusCode < 400 || w.fakeA.LastSeq() != beforeA || w.fakeB.LastSeq() != beforeB {
		t.Fatalf("paused pool still forwarded: %d %s", resp.StatusCode, raw)
	}
	if code, body := w.adminDo("PUT", "/admin/data/pools/"+poolName+"/resume", nil); code != http.StatusOK {
		t.Fatalf("resume: %d %s", code, body)
	}
	resp, raw = call(t, poolKey, "POST", "/v1/chat/completions", request(models[1]), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resumed display pool: %d %s", resp.StatusCode, raw)
	}
}
