package admin

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
	"github.com/go-chi/chi/v5"
	bolt "go.etcd.io/bbolt"
)

// Listing targets and pools must stay read-only once the store is migrated.
// A write transaction per GET waits on bbolt's single writer (shared with
// usage persistence) and made the targets page hit the 15s UI timeout.
func TestListTargetsAndPoolsDoNotCommitWrites(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, 2, nil)
	path := filepath.Join(dir, "config.json")
	writeConfigFile(t, path, cfg)
	stores := setupTestStores(t, dir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	sessions := NewSessionManager(config.AdminSessionConfig{Secret: "fixture-session-secret", CookieName: "session", TTLSeconds: 3600}, logger)
	h := NewHandler(config.NewManager(path), auth.NewStore(), service, stores.auditStore, stores.userStore, stores.clientStore, stores.targetStore, stores.modelCostStore, stores.usageStore, nil, nil, nil, sessions, logger)
	router := chi.NewRouter()
	router.Route("/admin", func(r chi.Router) { r.Use(sessions.Middleware); r.Mount("/", h) })
	_, cookie, err := sessions.CreateSession(RoleAdmin, RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	get := func(url string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", url, rec.Code, rec.Body.String())
		}
	}
	txID := func() int {
		var id int
		if err := stores.db.View(func(tx *bolt.Tx) error { id = tx.ID(); return nil }); err != nil {
			t.Fatal(err)
		}
		return id
	}

	// The first read may import the file targets into the empty store.
	get("/admin/data/targets")
	before := txID()
	for i := 0; i < 3; i++ {
		get("/admin/data/targets")
		get("/admin/data/pools")
	}
	if after := txID(); after != before {
		t.Fatalf("list requests committed write transactions: txid %d -> %d", before, after)
	}
}
