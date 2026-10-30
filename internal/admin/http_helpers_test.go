package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func requestWithRawPathParam(rawPath, name, value string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/data/x", nil)
	if rawPath != "" {
		req.URL.RawPath = rawPath
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(name, value)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// pathParamLower 用于大小写不敏感的资源名归一化：先按 chi 规则解码一次，
// 再 trim + 小写。模型名可能含 %2F，不能被二次解码破坏。
func TestPathParamLowerDecodesTrimsAndLowercases(t *testing.T) {
	req := requestWithRawPathParam("/data/models/MiniMax%2FMiniMax-M3", "name", "MiniMax%2FMiniMax-M3")
	if got := pathParam(req, "name"); got != "MiniMax/MiniMax-M3" {
		t.Fatalf("pathParam = %q, want MiniMax/MiniMax-M3", got)
	}
	if got := pathParamLower(req, "name"); got != "minimax/minimax-m3" {
		t.Fatalf("pathParamLower = %q, want minimax/minimax-m3", got)
	}

	req = requestWithRawPathParam("", "name", "  GLM-Max  ")
	if got := pathParamLower(req, "name"); got != "glm-max" {
		t.Fatalf("pathParamLower untrimmed = %q, want glm-max", got)
	}
}

// RawPath 存在但参数不是合法转义时，pathParam 必须回退到 chi 的原始值，
// 而不是返回空串或吞掉请求。
func TestPathParamFallsBackWhenUnescapeFails(t *testing.T) {
	req := requestWithRawPathParam("/data/x/%zz", "name", "%zz")
	if got := pathParam(req, "name"); got != "%zz" {
		t.Fatalf("pathParam fallback = %q, want %%zz", got)
	}
	if got := pathParamLower(req, "name"); got != "%zz" {
		t.Fatalf("pathParamLower fallback = %q, want %%zz", got)
	}
}
