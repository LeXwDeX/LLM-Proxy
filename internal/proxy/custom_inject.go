package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// customHeaderBlacklist 禁止通过 custom_headers 覆盖的 HTTP 头。
// 包含 hop-by-hop、认证类、代理内部管理头和上游协议关键头。
var customHeaderBlacklist = map[string]struct{}{
	"host":                  {},
	"content-length":        {},
	"content-type":          {},
	"transfer-encoding":     {},
	"connection":            {},
	"authorization":         {},
	"api-key":               {},
	"x-api-key":             {},
	"x-goog-api-key":        {},
	"x-proxy-target":        {},
	"x-azure-target":        {},
	"x-azure-authorization": {},
	"x-request-id":          {},
	// 上游协议关键头 — InjectAuth 会设置这些，custom_headers 不得覆盖
	"anthropic-version":       {},
	"user-agent":              {},
}

// customBodyFieldBlacklist 禁止通过 custom_body 覆盖的 JSON 顶层字段。
// 包含路由和协议关键字段。
var customBodyFieldBlacklist = map[string]struct{}{
	"model":    {},
	"messages": {},
	"stream":   {},
	"n":        {},
	"input":    {},
}

// ValidateCustomHeaders 返回首个命中黑名单的 header name，空串表示全部通过。
// 导出供 admin handler 创建/更新 target 时使用。
func ValidateCustomHeaders(headers map[string]string) string {
	return validateCustomHeaders(headers)
}

// ValidateCustomBody 返回首个命中黑名单的 body field name，空串表示全部通过。
// 导出供 admin handler 创建/更新 target 时使用。
func ValidateCustomBody(body map[string]any) string {
	return validateCustomBody(body)
}

// validateCustomHeaders 返回首个命中黑名单的 header name，空串表示全部通过。
func validateCustomHeaders(headers map[string]string) string {
	for k := range headers {
		if _, blocked := customHeaderBlacklist[strings.ToLower(k)]; blocked {
			return k
		}
	}
	return ""
}

// validateCustomBody 返回首个命中黑名单的 body field name，空串表示全部通过。
// A case variant of a protected request member (e.g. "Service_Tier") is
// rejected too: the proxy prices the exact member, while a case-insensitive
// upstream parser could read the variant instead.
func validateCustomBody(body map[string]any) string {
	for k := range body {
		if _, blocked := customBodyFieldBlacklist[strings.ToLower(k)]; blocked || isProtectedMemberVariant(k) {
			return k
		}
	}
	return ""
}

// injectCustomHeaders 将 target 的 custom_headers 注入到上游请求 header 中。
// 黑名单内的 key 静默跳过（admin 创建时已验证，此处为二次防护）。
// 在 InjectAuth 之后调用，故不会覆盖认证头。
func injectCustomHeaders(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		lk := strings.ToLower(k)
		if _, blocked := customHeaderBlacklist[lk]; blocked {
			continue
		}
		req.Header.Set(k, v)
	}
}

// injectCustomBody 将 target 的 custom_body 合并到请求体 JSON 中。
// 黑名单内的 key 静默跳过。
// Azure target：custom_body 在 sanitize 之后注入，绕过白名单（配置者明示意图）。
// 如果 JSON 解析失败，返回原始 body。
func injectCustomBody(body []byte, fields map[string]any) []byte {
	if len(fields) == 0 || len(body) == 0 {
		return body
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	// JSON null unmarshals into a nil map. It is valid JSON, but it is not an
	// object that can receive custom fields; leave it unchanged instead of
	// panicking on assignment.
	if parsed == nil {
		return body
	}
	for k, v := range fields {
		lk := strings.ToLower(k)
		if _, blocked := customBodyFieldBlacklist[lk]; blocked || isProtectedMemberVariant(k) {
			// Protected variants would coexist with the exact member the
			// proxy prices; skip them like blacklisted fields.
			continue
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			return body
		}
		parsed[k] = encoded
	}
	result, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return result
}
