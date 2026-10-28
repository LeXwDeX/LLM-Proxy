package admin

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/proxy"
)

func TestIssue75InvalidEndpointErrorsDoNotLogURL(t *testing.T) {
	const invalid = "https://fixture-user:fixture-password@example.invalid/%zz?token=fixture-query"
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	raw, _, _, service := newTargetUpdateTestHandlerWithService(t, t.TempDir(), []string{"fixture-key"})
	t.Cleanup(func() { _ = service.Close() })
	h := raw.(*Handler)
	h.logger = logger
	for _, tc := range []struct{ method, path, name string }{
		{http.MethodPost, "/data/targets", "new-target"},
		{http.MethodPut, "/data/targets/target-1", "target-1"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			logs.Reset()
			body, _ := json.Marshal(map[string]any{"name": tc.name, "endpoint_type": "openai", "usage_adapter": "standard", "endpoint": invalid, "api_key": "fixture-key"})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, bytes.NewReader(body)))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			for _, secret := range []string{"fixture-user", "fixture-password", "fixture-query"} {
				if strings.Contains(logs.String()+rec.Body.String(), secret) {
					t.Errorf("secret %q leaked in logs or response", secret)
				}
			}
			if !strings.Contains(logs.String(), tc.name) || !strings.Contains(logs.String(), "invalid endpoint") {
				t.Errorf("missing controlled diagnostic: %s", logs.String())
			}
		})
	}
	cfg, err := h.currentConfig()
	if err != nil || len(cfg.Targets) != 1 || cfg.Targets[0].Endpoint == invalid {
		t.Fatalf("failed edit changed configuration: %+v, %v", cfg, err)
	}

	dir := t.TempDir()
	loadCfg := testConfig(dir, 1, nil)
	loadCfg.Targets[0].Endpoint = invalid
	path := filepath.Join(dir, "config.json")
	writeConfigFile(t, path, loadCfg)
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	_, err = proxy.NewService(loaded, logger)
	if err == nil {
		t.Fatal("invalid endpoint accepted at startup")
	}
	logs.Reset()
	logger.Error("failed to initialize proxy", "error", err)
	for _, secret := range []string{"fixture-user", "fixture-password", "fixture-query"} {
		if strings.Contains(err.Error()+logs.String(), secret) {
			t.Errorf("startup error or log contains %q", secret)
		}
	}
	if !strings.Contains(err.Error(), "target-1") || !strings.Contains(err.Error(), "invalid endpoint") {
		t.Errorf("missing startup diagnostic: %v", err)
	}
}
