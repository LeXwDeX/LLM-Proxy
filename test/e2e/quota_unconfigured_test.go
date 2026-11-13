//go:build e2e

package e2e

import (
	"encoding/json"
	"testing"
)

// Quota is only granted through the client's plan groups: a route outside every
// plan group is rejected with 403 quota_not_configured before reaching the
// upstream; there is no unlimited fallback group.
func TestQuotaUnconfiguredRouteRejected(t *testing.T) {
	tg := target(t, map[string]any{"endpoint_type": "openai", "endpoint": w.urlA, "api_key": uniq("sk-unconf"),
		"model_mappings": []any{map[string]any{"upstream": "e2e-granted"}, map[string]any{"upstream": "e2e-ungranted"}}})
	name, key := client(t, []string{tg}, nil)
	body := func(model string) map[string]any {
		return map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	}
	if resp, raw := call(t, key, "POST", "/v1/chat/completions", body("e2e-ungranted"), nil); resp.StatusCode != 200 {
		t.Fatalf("whole-target grant: HTTP %d %s", resp.StatusCode, raw)
	}

	group := uniq("e2e-exact")
	if code, raw := w.adminDo("POST", "/admin/data/model-groups", map[string]any{"name": group, "cost_coefficient": 1,
		"bindings": []any{map[string]any{"target": tg, "model": "e2e-granted"}}}); code != 201 {
		t.Fatalf("group: %d %s", code, raw)
	}
	t.Cleanup(func() { w.adminDo("DELETE", "/admin/data/model-groups/"+group, nil) })
	if code, raw := w.adminDo("PUT", "/admin/data/quota/"+name, map[string]any{"job_level": "e2e-level", "quota_groups": []any{map[string]any{"name": group, "coefficient": 1}}}); code != 200 {
		t.Fatalf("plan: %d %s", code, raw)
	}
	if resp, raw := call(t, key, "POST", "/v1/chat/completions", body("e2e-granted"), nil); resp.StatusCode != 200 {
		t.Fatalf("granted model: HTTP %d %s", resp.StatusCode, raw)
	}
	seq := w.fakeA.LastSeq()
	resp, raw := call(t, key, "POST", "/v1/chat/completions", body("e2e-ungranted"), nil)
	if resp.StatusCode != 403 {
		t.Fatalf("ungranted model: HTTP %d %s, want 403", resp.StatusCode, raw)
	}
	var got struct {
		Error struct {
			Code  string         `json:"code"`
			Quota map[string]any `json:"quota"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.Error.Code != "quota_not_configured" || got.Error.Quota["scope"] != "unconfigured" {
		t.Fatalf("body = %s (err %v), want quota_not_configured / unconfigured", raw, err)
	}
	if w.fakeA.LastSeq() != seq {
		t.Fatal("rejected request reached the upstream")
	}
}
