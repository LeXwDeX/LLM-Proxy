package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelPrefixTargetManagement(t *testing.T) {
	h, _, stores, service := newTargetUpdateTestHandlerWithService(t, t.TempDir(), []string{"fixture-key"})
	defer service.Close()
	body := map[string]any{"name": "prefix-ui", "endpoint_type": "openai", "usage_adapter": "standard", "endpoint": "https://fixture.invalid", "api_key": "fixture-key", "model_prefix": "Vendor/"}
	if rec := wsCreateTarget(t, h, body); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	for _, step := range []struct {
		body map[string]any
		want string
	}{{map[string]any{}, "Vendor/"}, {map[string]any{"model_prefix": "Team:Vendor/"}, "Team:Vendor/"}, {map[string]any{"model_prefix": ""}, ""}} {
		if rec := putTarget(t, h, "prefix-ui", step.body); rec.Code != http.StatusOK {
			t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
		}
		if got := mustGetTarget(t, stores.targetStore, "prefix-ui").ModelPrefix; got != step.want {
			t.Fatalf("stored=%q want=%q", got, step.want)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/data/targets", nil))
		var list struct {
			Targets []map[string]any `json:"targets"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, tg := range list.Targets {
			if tg["name"] == "prefix-ui" {
				found = true
				if tg["model_prefix"] != step.want {
					t.Fatalf("list=%v", tg)
				}
			}
		}
		if !found {
			t.Fatal("missing target")
		}
	}
	for _, prefix := range []any{"bad\n", 42} {
		if rec := putTarget(t, h, "prefix-ui", map[string]any{"model_prefix": prefix}); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid update: %d", rec.Code)
		}
	}
	body["name"] = "invalid-prefix"
	body["model_prefix"] = "bad\r"
	if rec := wsCreateTarget(t, h, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid create: %d", rec.Code)
	}
}
