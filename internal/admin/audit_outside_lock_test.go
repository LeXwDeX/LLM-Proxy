package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
)

// Issue #151: key block/unblock and config reload used to write their audit
// (a bbolt Update + fsync) while holding configMu.
func TestKeyBlockAndReloadAuditOutsideConfigLock(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	var seen []string
	var held bool
	e.h.auditWriteHook = func(ev nosql.AuditEvent) {
		seen = append(seen, ev.Action)
		if !e.h.configMu.TryLock() {
			held = true
		} else {
			e.h.configMu.Unlock()
		}
	}
	var list struct {
		KeyIDs  map[string][]string `json:"key_ids"`
		Targets []struct {
			Name   string   `json:"name"`
			KeyIDs []string `json:"key_ids"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(e.do(http.MethodGet, "targets", "", nil).Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	keyID := ""
	for _, tg := range list.Targets {
		if tg.Name == "target-a" && len(tg.KeyIDs) > 0 {
			keyID = tg.KeyIDs[0]
		}
	}
	if keyID == "" {
		t.Fatal("no key id for target-a")
	}
	for _, step := range []struct{ method, path, action string }{
		{http.MethodPut, "targets/target-a/keys/id/KEYID/block", "block_target_key"},
		{http.MethodPut, "targets/target-a/keys/id/KEYID/unblock", "unblock_target_key"},
		{http.MethodPost, "RELOAD", "config_reload"},
	} {
		rec := e.do(step.method, strings.ReplaceAll(step.path, "KEYID", keyID), "", nil)
		if step.path == "RELOAD" {
			rec = httptest.NewRecorder()
			e.h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/config/reload", nil))
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", step.path, rec.Code, rec.Body.String())
		}
		if seen[len(seen)-1] != step.action {
			t.Fatalf("%s: audit actions %v", step.path, seen)
		}
	}
	if held {
		t.Fatal("audit written while holding configMu")
	}
}
