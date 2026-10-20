package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPoolDisplayNameCRUD(t *testing.T) {
	h, authStore, stores, service := newPoolTestHandler(t, t.TempDir())
	rec := postPool(t, h, map[string]any{"name": " OpenAI-PLAN "})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	assertDisplay := func(want string) {
		t.Helper()
		pool := mustGetPool(t, stores.targetStore.Pools(), "openai-plan")
		data, _ := json.Marshal(pool)
		var got map[string]any
		_ = json.Unmarshal(data, &got)
		if pool.Name != "openai-plan" || got["display_name"] != want {
			t.Fatalf("pool = %s, want display %q with canonical identity", data, want)
		}
	}
	assertDisplay("OpenAI-PLAN")
	if rec := putTarget(t, h, "target-1", poolTargetUpdateBody("openai-plan")); rec.Code != http.StatusOK {
		t.Fatalf("assign target: %d %s", rec.Code, rec.Body.String())
	}
	payload, _ := json.Marshal(map[string]any{"name": "pool-case-client", "access_key": "pool-case-key", "allowed_targets": []string{}, "allowed_pools": []string{"openai-plan"}})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/clients", bytes.NewReader(payload)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create client: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postPool(t, h, map[string]any{"name": "OPENAI-PLAN"}); rec.Code != http.StatusConflict {
		t.Fatalf("case duplicate: %d", rec.Code)
	}
	rec = putPool(t, h, "openai-plan", map[string]any{"display_name": "OPENAI-Plan"})
	if rec.Code != http.StatusOK {
		t.Fatalf("update casing: %d %s", rec.Code, rec.Body.String())
	}
	assertDisplay("OPENAI-Plan")
	if target := mustGetTarget(t, stores.targetStore, "target-1"); target.Pool != "openai-plan" {
		t.Fatalf("changed target pool identity: %q", target.Pool)
	}
	principal, ok := authStore.Authenticate("pool-case-key")
	if !ok || !principal.CanAccess("target-1") || principal.AllowAll() {
		t.Fatal("display edit changed pool-only authorization")
	}
	rec = putPool(t, h, "openai-plan", map[string]any{"paused": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: %d", rec.Code)
	}
	assertDisplay("OPENAI-Plan")
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 1 || !statuses[0].Paused {
		t.Fatalf("pool pause lost canonical member binding: %#v", statuses)
	}
	for _, invalid := range []string{"another-pool", " "} {
		rec = putPool(t, h, "openai-plan", map[string]any{"display_name": invalid})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid display %q: %d", invalid, rec.Code)
		}
		assertDisplay("OPENAI-Plan")
	}
	rec = postPool(t, h, map[string]any{"name": "second", "display_name": "other"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid create display: %d", rec.Code)
	}
}
