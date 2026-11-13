package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/quota"
)

// 未被 plan 任一模型组授予额度的路由：403 + quota_not_configured，
// 不是 429（客户端不应按限流退避重试），也不带重置时间。
func TestWriteQuotaExceededResponse_UnconfiguredIs403(t *testing.T) {
	rec := httptest.NewRecorder()
	writeQuotaExceededResponse(rec, quota.ExceededInfo{Scope: quota.ScopeUnconfigured, Dimension: quota.UnconfiguredDimension})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var body struct {
		Error struct {
			Message string         `json:"message"`
			Type    string         `json:"type"`
			Code    string         `json:"code"`
			Quota   map[string]any `json:"quota"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "quota_not_configured" || body.Error.Type != "quota_not_configured" {
		t.Fatalf("error = %+v, want code/type quota_not_configured", body.Error)
	}
	if body.Error.Quota["scope"] != quota.ScopeUnconfigured {
		t.Fatalf("quota.scope = %v, want %q", body.Error.Quota["scope"], quota.ScopeUnconfigured)
	}
	if _, ok := body.Error.Quota["resets_at"]; ok {
		t.Fatal("unconfigured rejection must not claim a reset time")
	}
	if body.Error.Message == "" {
		t.Fatal("message must explain the route has no quota configured")
	}
}
