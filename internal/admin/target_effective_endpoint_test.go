package admin

import (
	"encoding/json"
	"net/http"
	"testing"
)

// endpoint 填成完整接口地址时保存照常成功，原值落库，响应提示实际生效的 base（Issue #232）。
func TestTargetSaveReportsEffectiveEndpoint(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)

	rec := e.updateA(quoted(e.view("target-a").etag(t)), func(b map[string]any) {
		b["endpoint_type"] = "openai"
		b["usage_adapter"] = "standard"
		b["endpoint"] = "https://host.example/v2/chat/completions"
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["effective_endpoint"] != "https://host.example/v2" || resp["endpoint_operation"] != "/chat/completions" {
		t.Fatalf("missing effective endpoint hint: %s", rec.Body.String())
	}
	if _, hasWarning := resp["warning"]; hasWarning {
		t.Fatalf("hint must not reuse the auth warning: %s", rec.Body.String())
	}
	if got := mustGetTarget(t, e.stores.targetStore, "target-a"); got.Endpoint != "https://host.example/v2/chat/completions" {
		t.Fatalf("stored endpoint rewritten: %s", got.Endpoint)
	}

	// 反例：普通 base 不带提示字段。
	rec = e.updateA(quoted(e.view("target-a").etag(t)), func(b map[string]any) {
		b["endpoint_type"] = "openai"
		b["usage_adapter"] = "standard"
		b["endpoint"] = "https://host.example/v2"
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	resp = map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := resp["effective_endpoint"]; ok {
		t.Fatalf("plain base must not carry a hint: %s", rec.Body.String())
	}
	if _, ok := resp["endpoint_operation"]; ok {
		t.Fatalf("plain base must not carry a hint: %s", rec.Body.String())
	}
}

func TestTargetCreateReportsEffectiveEndpoint(t *testing.T) {
	e := newRevEnv(t, t.TempDir(), false)
	rec := e.do(http.MethodPost, "targets", "", map[string]any{
		"name": "full-url", "endpoint_type": "openai", "usage_adapter": "standard",
		"endpoint": "https://host.example/api/v3/chat/completions/", "api_key": "fixture-key",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["effective_endpoint"] != "https://host.example/api/v3" || resp["endpoint_operation"] != "/chat/completions" {
		t.Fatalf("missing effective endpoint hint: %s", rec.Body.String())
	}

	// 反例：Azure 的路径自带 deployment 结构，不识别接口后缀。
	rec = e.updateA(quoted(e.view("target-a").etag(t)), func(b map[string]any) {
		b["endpoint"] = "https://host.example/openai/deployments/d/chat/completions"
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	resp = map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := resp["effective_endpoint"]; ok {
		t.Fatalf("azure endpoint must not carry a hint: %s", rec.Body.String())
	}
}
