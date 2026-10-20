package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStreamUsageTargetManagement(t *testing.T) {
	h, _, stores, service := newTargetUpdateTestHandlerWithService(t, t.TempDir(), []string{"fixture-key"})
	t.Cleanup(func() { _ = service.Close() })
	if rec := wsCreateTarget(t, h, map[string]any{"name": "stream-ui", "endpoint_type": "openai", "usage_adapter": "standard", "endpoint": "https://fixture.invalid", "api_key": "fixture-key", "request_stream_usage_if_missing": true}); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	for _, step := range []struct {
		body map[string]any
		want bool
	}{{map[string]any{}, true}, {map[string]any{"request_stream_usage_if_missing": false}, false}, {map[string]any{"request_stream_usage_if_missing": true}, true}} {
		if rec := putTarget(t, h, "stream-ui", step.body); rec.Code != http.StatusOK {
			t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
		}
		if got := mustGetTarget(t, stores.targetStore, "stream-ui").RequestStreamUsageIfMissing; got != step.want {
			t.Fatalf("stored %v want %v", got, step.want)
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
			if tg["name"] == "stream-ui" {
				found = true
				if tg["request_stream_usage_if_missing"] != step.want {
					t.Fatalf("list: %v", tg)
				}
			}
		}
		if !found {
			t.Fatal("list missing target")
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/data/schema", nil))
	var schema map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &schema); err != nil {
		t.Fatal(err)
	}
	if schema["defaults"].(map[string]any)["request_stream_usage_if_missing"] != false {
		t.Fatal("schema default missing/true")
	}
	if schema["stream_usage_request_schema"] != "openai-chat" {
		t.Fatal("schema applicability missing")
	}
}
